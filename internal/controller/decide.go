package controller

import (
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// signal is the solar verdict for one tick, with a hysteresis band between shed and wake.
type signal int

const (
	sigNeutral signal = iota // inside the band: hold the current mode
	sigDeficit               // consumption outruns production: shed
	sigSurplus               // production clears the wake headroom: wake
)

// Snapshot is everything decide needs: fully observable, no side effects.
type Snapshot struct {
	Surplus    float64
	SurplusRaw float64 // instantaneous surplus, for metrics only; not a decision input
	SoC        float64
	NodeUp     bool
	Guests     []proxmox.Guest // guests currently on the managed node ("" if it's down)
	Mode       state.Mode
	StoppedSet []state.GuestRef
	GraceSince int64 // unix time the gaming grace clock started; 0 when not running
}

// Plan is the set of actions a single reconcile wants to take. Disjoint per mode:
// a shed plan never also wakes, and vice-versa, so execute can apply a fixed order.
type Plan struct {
	Migrate    []proxmox.Guest  // live-migrate off the node before power-off
	Stop       []proxmox.Guest  // graceful stop + record
	Start      []state.GuestRef // start the guests we previously stopped
	Poweroff   bool
	Wake       bool
	Silence    bool
	Unsilence  bool
	NextMode   state.Mode
	GraceSince int64 // the grace clock to persist; carried forward unless a transition changes it
	Reason     string
}

// classify turns the solar reading into a signal using the hysteresis band.
func classify(surplus, soc float64, p config.Prometheus) signal {
	if surplus >= p.HeadroomWatts && soc >= p.MinBatteryPercent {
		return sigSurplus
	}
	if surplus < p.ShedBelowWatts {
		return sigDeficit
	}
	return sigNeutral
}

func gamingActive(guests []proxmox.Guest, set config.IDSet) bool {
	for _, g := range guests {
		if g.Running && set.Contains(g.VMID) {
			return true
		}
	}
	return false
}

func matchRunning(guests []proxmox.Guest, set config.IDSet) []proxmox.Guest {
	var out []proxmox.Guest
	for _, g := range guests {
		if g.Running && set.Contains(g.VMID) {
			out = append(out, g)
		}
	}
	return out
}

func refs(guests []proxmox.Guest) []state.GuestRef {
	out := make([]state.GuestRef, 0, len(guests))
	for _, g := range guests {
		out = append(out, state.GuestRef{VMID: g.VMID, Type: string(g.Type)})
	}
	return out
}

// Decide is the pure state machine. Given a fully-observed Snapshot it returns the
// Plan and the mode to transition to. It performs no I/O, so the whole behaviour
// (the JHC-504 comment logic) is unit-testable without touching hardware.
func Decide(s Snapshot, cfg *config.Config, now time.Time) Plan {
	sig := classify(s.Surplus, s.SoC, cfg.Prometheus)
	gaming := s.NodeUp && gamingActive(s.Guests, cfg.Guests.GamingGuard)
	// graceStart is the clock to record when we enter a gaming session: stopped (0) if a
	// gaming guest is already running, else start counting from now.
	graceStart := func() int64 {
		if gaming {
			return 0
		}
		return now.Unix()
	}
	p := Plan{NextMode: s.Mode, GraceSince: s.GraceSince}

	switch s.Mode {
	case state.ModeRunning:
		if sig != sigDeficit {
			return p
		}
		// Shed posture: criticals move, the rest stop, silence the resulting alerts.
		p.Migrate = matchRunning(s.Guests, cfg.Guests.Migrate)
		p.Stop = matchRunning(s.Guests, cfg.Guests.Stop)
		p.Silence = true
		if gaming {
			// A gaming guest is running, so keep the host up and just shed load around it.
			p.NextMode = state.ModeGaming
			p.GraceSince = graceStart()
			p.Reason = "deficit with a gaming guest running: shed load, keep p1 up"
		} else {
			p.Poweroff = true
			p.NextMode = state.ModeShed
			p.Reason = "deficit and no gaming guest: shed load and power off p1"
		}

	case state.ModeShed:
		switch {
		case sig == sigSurplus:
			p.Wake = true
			p.Start = s.StoppedSet
			p.Unsilence = true
			p.NextMode = state.ModeRunning
			p.Reason = "surplus returned: wake p1 and restart the guests we stopped"
		case s.NodeUp:
			// p1 came up on its own: the user woke it to game. Don't fight it - adopt as a
			// gaming session and start the grace clock so they have time to launch a VM.
			p.NextMode = state.ModeGaming
			p.GraceSince = graceStart()
			p.Reason = "p1 powered on during deficit: adopt as a gaming session"
		}

	case state.ModeGaming:
		switch {
		case sig == sigSurplus:
			// Good morning. p1 is already up; restore what we stopped. Criticals stay
			// where they were migrated. Nothing migrates back automatically.
			p.Start = s.StoppedSet
			p.Unsilence = true
			p.NextMode = state.ModeRunning
			p.GraceSince = 0
			p.Reason = "surplus returned while p1 up: restart the guests we stopped"
		case gaming:
			// A gaming guest is running: keep the grace clock stopped so the session runs
			// freely. Resetting it here is what lets a mid-session VM/GPU reboot ride out
			// the grace window instead of triggering an immediate power-off.
			p.GraceSince = 0
		case s.GraceSince == 0:
			// Gaming just went idle (or we just adopted): start the grace clock rather than
			// powering off now, so a reboot or a not-yet-started VM gets a chance.
			p.GraceSince = now.Unix()
			p.Reason = "no gaming guest running: starting grace period before power-off"
		case now.Sub(time.Unix(s.GraceSince, 0)) >= cfg.GamingGrace.Duration:
			// Grace elapsed with no gaming guest and still no surplus: complete the shed.
			p.Poweroff = true
			p.NextMode = state.ModeShed
			p.GraceSince = 0
			p.Reason = "grace period elapsed with no gaming guest: power off p1"
		default:
			// Still within the grace window: hold p1 up and keep waiting for a VM.
			p.Reason = "within gaming grace period: holding p1 up"
		}
	}
	return p
}
