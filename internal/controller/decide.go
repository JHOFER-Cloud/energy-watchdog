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
	NodeUptime time.Duration   // 0 when the node is down, or when the token can't read uptime
	Guests     []proxmox.Guest // guests currently on the managed node ("" if it's down)
	Mode       state.Mode
	StoppedSet []state.GuestRef
	GraceSince int64 // unix time the gaming grace clock started; 0 when not running
	// ManualShed holds the node shed regardless of surplus (state.Intent.Shed).
	ManualShed bool
	// ManualOn holds the node up regardless of surplus (state.Intent.On).
	ManualOn bool
	// Wake are the live self-service requests, already filtered to the gaming guard and deduped.
	Wake []state.WakeRequest
	// WakeDone is the persisted spent-request marker; see state.State.WakeDone.
	WakeDone map[int]int64
}

// Plan is the set of actions a single reconcile wants to take. Disjoint per mode:
// a shed plan never also wakes, and vice-versa, so execute can apply a fixed order.
type Plan struct {
	Migrate []proxmox.Guest  // live-migrate off the node before power-off
	Stop    []proxmox.Guest  // graceful stop + record
	Start   []state.GuestRef // start the guests we previously stopped
	// StartRequested are desktop VMs to start for a self-service request. Ids only: when the
	// node is still down we can't know a guest's type yet, so execute resolves it after wake.
	StartRequested []int
	Poweroff       bool
	Wake           bool
	NextMode       state.Mode
	GraceSince     int64         // the grace clock to persist; carried forward unless a transition changes it
	WakeDone       map[int]int64 // the spent-request markers to persist; see state.State.WakeDone
	Reason         string
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

// resolveWake splits the live requests into the ones still to act on and the spent-request
// markers to persist. A request is spent once its VM has been seen running: without that, the
// user shutting the VM down inside the request's TTL would just start it again next tick. Only
// VMIDs with a live request are carried, so the markers age out with the requests themselves.
func resolveWake(want []state.WakeRequest, guests []proxmox.Guest, done map[int]int64) ([]int, map[int]int64) {
	running := make(map[int]bool, len(guests))
	for _, g := range guests {
		if g.Running {
			running[g.VMID] = true
		}
	}
	var pending []int
	next := map[int]int64{}
	for _, w := range want {
		switch {
		case w.RequestedAt <= done[w.VMID]:
			next[w.VMID] = done[w.VMID] // spent on an earlier tick; a newer request re-arms it
		case running[w.VMID]:
			next[w.VMID] = w.RequestedAt // the VM is up: this request is satisfied
		default:
			pending = append(pending, w.VMID)
		}
	}
	if len(next) == 0 {
		return pending, nil
	}
	return pending, next
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
	// The manual holds are a pinned signal, not a fourth mode: every existing rule - gaming
	// guard, grace window, wake requests - then applies unchanged. Shed first, so a
	// hand-edited intent setting both can't override a heatwave shed.
	switch {
	case s.ManualShed:
		sig = sigDeficit
	case s.ManualOn:
		sig = sigSurplus // bypasses minBatteryPercent: a manual hold outranks the battery
	}
	gaming := s.NodeUp && gamingActive(s.Guests, cfg.Guests.GamingGuard)
	pending, wakeDone := resolveWake(s.Wake, s.Guests, s.WakeDone)
	requested := len(pending) > 0
	// graceStart is the clock to record when we enter a gaming session: stopped (0) if a
	// gaming guest is already running, else start counting from now.
	graceStart := func() int64 {
		if gaming {
			return 0
		}
		return now.Unix()
	}
	p := Plan{NextMode: s.Mode, GraceSince: s.GraceSince, WakeDone: wakeDone}

	switch s.Mode {
	case state.ModeRunning:
		if !s.NodeUp {
			// Running means p1 is up; something else took it down (nut-dog on a UPS event, a
			// crash, a human). Correct the mode so ModeShed's rules own the wake - this plans
			// no power action itself. Without it a hold-on is stranded: it pins the signal to
			// surplus, which is exactly what closes off the deficit path that rescues the
			// other cases.
			p.NextMode = state.ModeShed
			p.Reason = "p1 went down while running: back to shed"
			break
		}
		if sig != sigDeficit {
			break
		}
		// Shed posture: criticals move, the rest stop. Alertmanager coverage isn't planned
		// here - it's derived from NextMode every tick, so a long shed keeps being extended.
		p.Migrate = matchRunning(s.Guests, cfg.Guests.Migrate)
		p.Stop = matchRunning(s.Guests, cfg.Guests.Stop)
		if gaming || requested {
			// A gaming guest is running, or someone just asked for one: keep the host up and
			// shed load around it. The grace clock covers the VM that hasn't booted yet.
			p.NextMode = state.ModeGaming
			p.GraceSince = graceStart()
			p.Reason = "deficit with a gaming session wanted: shed load, keep p1 up"
		} else {
			// Never plan a power-off for a node that's already off: the call fails, and a failed
			// apply never persists the mode, so the same impossible plan is retried every tick.
			p.Poweroff = s.NodeUp
			p.NextMode = state.ModeShed
			p.Reason = "deficit and no gaming guest: shed load and power off p1"
		}

	case state.ModeShed:
		switch {
		case sig == sigSurplus:
			p.Wake = true
			p.Start = s.StoppedSet
			p.NextMode = state.ModeRunning
			p.Reason = "surplus returned: wake p1 and restart the guests we stopped"
		case requested:
			// Someone asked for their desktop VM. Wake p1 and adopt it as a gaming session, so
			// the grace clock - not the surplus - is what decides whether it stays up.
			p.Wake = !s.NodeUp
			p.NextMode = state.ModeGaming
			p.GraceSince = graceStart()
			p.Reason = "desktop VM requested during shed: waking p1 for a gaming session"
		case s.NodeUp && s.NodeUptime < cfg.Proxmox.FreshBootWindow.Duration:
			// p1 came up on its own: the user woke it to game. Don't fight it - adopt as a
			// gaming session and start the grace clock so they have time to launch a VM.
			// Without Sys.Audit uptime reads 0, so this adopts every online node, as it did
			// before the window existed.
			p.NextMode = state.ModeGaming
			p.GraceSince = graceStart()
			p.Reason = "p1 powered on during deficit: adopt as a gaming session"
		}

	case state.ModeGaming:
		switch {
		case !s.NodeUp:
			// Gaming means p1 is up; if it went away mid-session there's nothing left to shed.
			// Correct the mode now so ModeShed handles the wake, instead of running the clock out.
			p.NextMode = state.ModeShed
			p.GraceSince = 0
			p.Reason = "p1 went down during a gaming session: back to shed"
		case sig == sigSurplus:
			// Good morning. p1 is already up; restore what we stopped. Criticals stay
			// where they were migrated. Nothing migrates back automatically.
			p.Start = s.StoppedSet
			p.NextMode = state.ModeRunning
			p.GraceSince = 0
			p.Reason = "surplus returned while p1 up: restart the guests we stopped"
		case gaming:
			// A gaming guest is running: keep the grace clock stopped so the session runs
			// freely. Resetting it here is what lets a mid-session VM/GPU reboot ride out
			// the grace window instead of triggering an immediate power-off.
			p.GraceSince = 0
		case requested:
			// A request is still outstanding: hold the clock at now so a VM asked for late in
			// an already-running grace window doesn't get cut off. Bounded by the request TTL.
			p.GraceSince = now.Unix()
			p.Reason = "desktop VM requested but not up yet: holding p1"
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
	// A pinned signal makes the reasons read as if solar decided ("surplus returned" at
	// midnight); name who actually did.
	switch {
	case s.ManualShed && p.Reason != "":
		p.Reason += " (held off by hand)"
	case s.ManualOn && p.Reason != "":
		p.Reason += " (held on by hand)"
	}
	// One rule for every mode: start what was asked for wherever the node is up, or about to
	// be. Never on the way down - a VM started into a power-off would just die with it.
	if !p.Poweroff && (s.NodeUp || p.Wake) {
		p.StartRequested = pending
	}
	return p
}
