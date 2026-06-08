package controller

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// testCfg builds a config with the guest classes used across the decision tests:
// migrate 100-199, stop 300-399, gamingGuard 600-699. Wake at +1000W (SoC>=20),
// shed below 0W.
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
		Prometheus: config.Prometheus{HeadroomWatts: 1000, ShedBelowWatts: 0, MinBatteryPercent: 20},
		Guests:     g,
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
			name:     "shed, node manually powered on -> adopt as gaming",
			snap:     Snapshot{Surplus: -100, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, true)}, Mode: state.ModeShed},
			wantMode: state.ModeGaming,
		},
		{
			name:      "gaming, surplus -> restart stopped, no wake",
			snap:      Snapshot{Surplus: 1500, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, true)}, Mode: state.ModeGaming, StoppedSet: []state.GuestRef{{VMID: 301, Type: "qemu"}}},
			wantMode:  state.ModeRunning,
			wantStart: []int{301},
			wantUnsil: true,
		},
		{
			name:      "gaming ends, still deficit -> poweroff",
			snap:      Snapshot{Surplus: -100, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeGaming},
			wantMode:  state.ModeShed,
			wantPower: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Decide(tt.snap, cfg)
			if p.NextMode != tt.wantMode {
				t.Errorf("mode = %q, want %q (%s)", p.NextMode, tt.wantMode, p.Reason)
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
