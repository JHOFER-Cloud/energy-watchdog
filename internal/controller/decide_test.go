package controller

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// testNow is the fixed clock the decision tests decide against.
var testNow = time.Unix(1_700_000_000, 0)

// testCfg builds a config with the guest classes used across the decision tests:
// migrate 100-199, stop 300-399, gamingGuard 600-699. Wake at +1000W (SoC>=20),
// shed below 0W, with a 10m gaming grace window.
func testCfg(t *testing.T) *config.Config {
	t.Helper()
	var g config.Guests
	if err := yaml.Unmarshal([]byte(`
migrate: ["100-199"]
stop: ["300-399"]
gamingGuard: ["600-699"]
`), &g); err != nil {
		t.Fatalf("unmarshal guests: %v", err)
	}
	return &config.Config{
		Prometheus:  config.Prometheus{HeadroomWatts: 1000, ShedBelowWatts: 0, MinBatteryPercent: 20},
		Guests:      g,
		GamingGrace: config.Duration{Duration: 10 * time.Minute},
	}
}

func qemu(id int, running bool) proxmox.Guest {
	return proxmox.Guest{VMID: id, Type: proxmox.TypeQEMU, Running: running}
}

func ids(gs []proxmox.Guest) []int {
	out := make([]int, len(gs))
	for i, g := range gs {
		out[i] = g.VMID
	}
	return out
}

func TestDecide(t *testing.T) {
	cfg := testCfg(t)
	guestsOnP1 := []proxmox.Guest{qemu(101, true), qemu(301, true), qemu(302, false), qemu(601, false)}

	tests := []struct {
		name        string
		snap        Snapshot
		wantMode    state.Mode
		wantMigrate []int
		wantStop    []int
		wantStart   []int
		wantPower   bool
		wantWake    bool
		wantSilence bool
		wantUnsil   bool
		wantGrace   int64
	}{
		{
			name:     "running, surplus, holds",
			snap:     Snapshot{Surplus: 2000, SoC: 80, NodeUp: true, Guests: guestsOnP1, Mode: state.ModeRunning},
			wantMode: state.ModeRunning,
		},
		{
			name:     "running, neutral band, holds",
			snap:     Snapshot{Surplus: 500, SoC: 80, NodeUp: true, Guests: guestsOnP1, Mode: state.ModeRunning},
			wantMode: state.ModeRunning,
		},
		{
			name:        "running, deficit, no gaming -> shed + poweroff",
			snap:        Snapshot{Surplus: -300, SoC: 80, NodeUp: true, Guests: guestsOnP1, Mode: state.ModeRunning},
			wantMode:    state.ModeShed,
			wantMigrate: []int{101},
			wantStop:    []int{301}, // 302 is not running, so not stopped
			wantPower:   true,
			wantSilence: true,
		},
		{
			name:        "running, deficit, gaming up -> shed but keep host",
			snap:        Snapshot{Surplus: -300, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(101, true), qemu(301, true), qemu(601, true)}, Mode: state.ModeRunning},
			wantMode:    state.ModeGaming,
			wantMigrate: []int{101},
			wantStop:    []int{301},
			wantPower:   false,
			wantSilence: true,
		},
		{
			name:      "shed, surplus -> wake + restart stopped",
			snap:      Snapshot{Surplus: 1500, SoC: 80, NodeUp: false, Mode: state.ModeShed, StoppedSet: []state.GuestRef{{VMID: 301, Type: "qemu"}}},
			wantMode:  state.ModeRunning,
			wantStart: []int{301},
			wantWake:  true,
			wantUnsil: true,
		},
		{
			name:     "shed, low battery blocks wake despite surplus",
			snap:     Snapshot{Surplus: 1500, SoC: 10, NodeUp: false, Mode: state.ModeShed},
			wantMode: state.ModeShed,
		},
		{
			name:     "shed, node manually powered on with VM up -> adopt, clock stopped",
			snap:     Snapshot{Surplus: -100, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, true)}, Mode: state.ModeShed},
			wantMode: state.ModeGaming,
		},
		{
			name:      "shed, node manually powered on, no VM yet -> adopt, start grace clock",
			snap:      Snapshot{Surplus: -100, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeShed},
			wantMode:  state.ModeGaming,
			wantGrace: testNow.Unix(),
		},
		{
			name:      "gaming, surplus -> restart stopped, no wake",
			snap:      Snapshot{Surplus: 1500, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, true)}, Mode: state.ModeGaming, StoppedSet: []state.GuestRef{{VMID: 301, Type: "qemu"}}},
			wantMode:  state.ModeRunning,
			wantStart: []int{301},
			wantUnsil: true,
		},
		{
			// The bug fix: a freshly-woken host with no VM yet must NOT be powered off; it
			// starts the grace clock instead.
			name:      "gaming, no VM yet, clock idle -> start grace, no poweroff",
			snap:      Snapshot{Surplus: -100, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeGaming},
			wantMode:  state.ModeGaming,
			wantGrace: testNow.Unix(),
		},
		{
			name:      "gaming, no VM, within grace window -> hold, no poweroff",
			snap:      Snapshot{Surplus: -100, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeGaming, GraceSince: testNow.Add(-5 * time.Minute).Unix()},
			wantMode:  state.ModeGaming,
			wantGrace: testNow.Add(-5 * time.Minute).Unix(),
		},
		{
			name:      "gaming, no VM, grace elapsed -> poweroff",
			snap:      Snapshot{Surplus: -100, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeGaming, GraceSince: testNow.Add(-11 * time.Minute).Unix()},
			wantMode:  state.ModeShed,
			wantPower: true,
		},
		{
			// Mid-session VM/GPU reboot: the guest reappears while the clock was ticking, so
			// the clock resets and the host is kept up.
			name:     "gaming, VM back after reboot -> reset clock, keep host",
			snap:     Snapshot{Surplus: -100, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, true)}, Mode: state.ModeGaming, GraceSince: testNow.Add(-3 * time.Minute).Unix()},
			wantMode: state.ModeGaming,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Decide(tt.snap, cfg, testNow)
			if p.NextMode != tt.wantMode {
				t.Errorf("mode = %q, want %q (%s)", p.NextMode, tt.wantMode, p.Reason)
			}
			if p.GraceSince != tt.wantGrace {
				t.Errorf("graceSince = %d, want %d (%s)", p.GraceSince, tt.wantGrace, p.Reason)
			}
			if !equal(ids(p.Migrate), tt.wantMigrate) {
				t.Errorf("migrate = %v, want %v", ids(p.Migrate), tt.wantMigrate)
			}
			if !equal(ids(p.Stop), tt.wantStop) {
				t.Errorf("stop = %v, want %v", ids(p.Stop), tt.wantStop)
			}
			if !equal(refIDs(p.Start), tt.wantStart) {
				t.Errorf("start = %v, want %v", refIDs(p.Start), tt.wantStart)
			}
			if p.Poweroff != tt.wantPower {
				t.Errorf("poweroff = %v, want %v", p.Poweroff, tt.wantPower)
			}
			if p.Wake != tt.wantWake {
				t.Errorf("wake = %v, want %v", p.Wake, tt.wantWake)
			}
			if p.Silence != tt.wantSilence {
				t.Errorf("silence = %v, want %v", p.Silence, tt.wantSilence)
			}
			if p.Unsilence != tt.wantUnsil {
				t.Errorf("unsilence = %v, want %v", p.Unsilence, tt.wantUnsil)
			}
		})
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRotate(t *testing.T) {
	targets := []string{"pve-2", "pve-3"}
	// Equal split: guest i prefers target i%len, so consecutive guests alternate.
	if got := rotate(targets, 0)[0]; got != "pve-2" {
		t.Errorf("guest 0 -> %q, want pve-2", got)
	}
	if got := rotate(targets, 1)[0]; got != "pve-3" {
		t.Errorf("guest 1 -> %q, want pve-3", got)
	}
	if got := rotate(targets, 2)[0]; got != "pve-2" {
		t.Errorf("guest 2 -> %q, want pve-2", got)
	}
}
