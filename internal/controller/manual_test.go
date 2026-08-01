package controller

import (
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// TestDecideManualShed covers the operator-set shed: it must behave exactly like a
// solar-triggered one, including the gaming guard and the wake inhibit, but never wake on
// surplus. Every case here runs with a surplus that would normally wake or hold the node.
func TestDecideManualShed(t *testing.T) {
	cfg := testCfg(t)

	tests := []struct {
		name        string
		snap        Snapshot
		wantMode    state.Mode
		wantMigrate []int
		wantStop    []int
		wantPower   bool
		wantWake    bool
		wantSilence bool
		wantGrace   int64
	}{
		{
			// The heatwave case: plenty of sun, but shed anyway.
			name:        "running, big surplus, manual shed -> shed + poweroff",
			snap:        Snapshot{Surplus: 4000, SoC: 90, NodeUp: true, Guests: []proxmox.Guest{qemu(101, true), qemu(301, true)}, Mode: state.ModeRunning, ManualShed: true},
			wantMode:    state.ModeShed,
			wantMigrate: []int{101},
			wantStop:    []int{301},
			wantPower:   true,
			wantSilence: true,
		},
		{
			// Requirement: the gaming guard still vetoes the power-off.
			name:        "running, manual shed, gaming VM up -> shed load, keep host",
			snap:        Snapshot{Surplus: 4000, SoC: 90, NodeUp: true, Guests: []proxmox.Guest{qemu(101, true), qemu(301, true), qemu(601, true)}, Mode: state.ModeRunning, ManualShed: true},
			wantMode:    state.ModeGaming,
			wantMigrate: []int{101},
			wantStop:    []int{301},
			wantPower:   false,
			wantSilence: true,
		},
		{
			// Requirement: wake inhibit. Surplus is way past headroom and it still must not wake.
			name:     "shed, big surplus, manual shed -> stays shed, no wake",
			snap:     Snapshot{Surplus: 4000, SoC: 90, NodeUp: false, Mode: state.ModeShed, ManualShed: true, StoppedSet: []state.GuestRef{{VMID: 301, Type: "qemu"}}},
			wantMode: state.ModeShed,
			wantWake: false,
		},
		{
			// Powering p1 on by hand during a manual shed is still adopted, so a maintenance
			// window doesn't fight someone who deliberately walked over and pressed the button.
			name:      "shed, manual shed, powered on by hand -> adopt as gaming",
			snap:      Snapshot{Surplus: 4000, SoC: 90, NodeUp: true, NodeUptime: time.Minute, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeShed, ManualShed: true},
			wantMode:  state.ModeGaming,
			wantGrace: testNow.Unix(),
		},
		{
			name:      "gaming, manual shed, grace elapsed -> poweroff",
			snap:      Snapshot{Surplus: 4000, SoC: 90, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeGaming, ManualShed: true, GraceSince: testNow.Add(-11 * time.Minute).Unix()},
			wantMode:  state.ModeShed,
			wantPower: true,
		},
		{
			// Manual shed must not end a session that is actually in progress.
			name:     "gaming, manual shed, VM running -> hold",
			snap:     Snapshot{Surplus: 4000, SoC: 90, NodeUp: true, Guests: []proxmox.Guest{qemu(601, true)}, Mode: state.ModeGaming, ManualShed: true},
			wantMode: state.ModeGaming,
		},
		{
			// Clearing the flag hands control straight back to the sun.
			name:     "shed, surplus, manual shed cleared -> normal wake",
			snap:     Snapshot{Surplus: 4000, SoC: 90, NodeUp: false, Mode: state.ModeShed, ManualShed: false, StoppedSet: []state.GuestRef{{VMID: 301, Type: "qemu"}}},
			wantMode: state.ModeRunning,
			wantWake: true,
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
			if p.Poweroff != tt.wantPower {
				t.Errorf("poweroff = %v, want %v", p.Poweroff, tt.wantPower)
			}
			if p.Wake != tt.wantWake {
				t.Errorf("wake = %v, want %v (%s)", p.Wake, tt.wantWake, p.Reason)
			}
			if p.Silence != tt.wantSilence {
				t.Errorf("silence = %v, want %v", p.Silence, tt.wantSilence)
			}
		})
	}
}

// TestDecideWakeRequest covers the self-service path (JHC-548): a request is an intent the
// loop acts on, so the UI never touches p1 itself.
func TestDecideWakeRequest(t *testing.T) {
	cfg := testCfg(t)

	tests := []struct {
		name       string
		snap       Snapshot
		wantMode   state.Mode
		wantWake   bool
		wantPower  bool
		wantStartR []int
		wantGrace  int64
	}{
		{
			name:       "shed, request -> wake p1 and start the VM",
			snap:       Snapshot{Surplus: -300, SoC: 80, NodeUp: false, Mode: state.ModeShed, WakeVMIDs: []int{601}},
			wantMode:   state.ModeGaming,
			wantWake:   true,
			wantStartR: []int{601},
			wantGrace:  testNow.Unix(),
		},
		{
			// A manual shed must not block someone gaming, same as the guard doesn't.
			name:       "shed, manual shed, request -> still wakes",
			snap:       Snapshot{Surplus: 4000, SoC: 90, NodeUp: false, Mode: state.ModeShed, ManualShed: true, WakeVMIDs: []int{601}},
			wantMode:   state.ModeGaming,
			wantWake:   true,
			wantStartR: []int{601},
			wantGrace:  testNow.Unix(),
		},
		{
			// This is the shutting-down race: the loop already powered p1 off, and the request
			// is still live, so the next tick simply wakes it again. Nothing hangs.
			name:       "gaming, p1 went down, request live -> shed, then wake next tick",
			snap:       Snapshot{Surplus: -300, SoC: 80, NodeUp: false, Mode: state.ModeGaming, WakeVMIDs: []int{601}, GraceSince: testNow.Add(-time.Minute).Unix()},
			wantMode:   state.ModeShed,
			wantWake:   false,
			wantStartR: nil,
		},
		{
			// Already running: nothing to start, and the guard holds p1 from here on.
			name:       "gaming, requested VM already running -> no start",
			snap:       Snapshot{Surplus: -300, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, true)}, Mode: state.ModeGaming, WakeVMIDs: []int{601}},
			wantMode:   state.ModeGaming,
			wantStartR: nil,
		},
		{
			// A request arriving late in an already-running grace window must not be cut off.
			name:       "gaming, request late in grace window -> hold, restart clock",
			snap:       Snapshot{Surplus: -300, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeGaming, WakeVMIDs: []int{601}, GraceSince: testNow.Add(-9 * time.Minute).Unix()},
			wantMode:   state.ModeGaming,
			wantPower:  false,
			wantStartR: []int{601},
			wantGrace:  testNow.Unix(),
		},
		{
			// p1 is up and serving: just start the VM, no mode change.
			name:       "running, request -> start the VM, stay running",
			snap:       Snapshot{Surplus: 4000, SoC: 90, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeRunning, WakeVMIDs: []int{601}},
			wantMode:   state.ModeRunning,
			wantStartR: []int{601},
		},
		{
			// Deficit with a request but no VM up yet: hold the host for the session.
			name:       "running, deficit, request -> gaming, keep host up",
			snap:       Snapshot{Surplus: -300, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeRunning, WakeVMIDs: []int{601}},
			wantMode:   state.ModeGaming,
			wantPower:  false,
			wantStartR: []int{601},
			wantGrace:  testNow.Unix(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Decide(tt.snap, cfg, testNow)
			if p.NextMode != tt.wantMode {
				t.Errorf("mode = %q, want %q (%s)", p.NextMode, tt.wantMode, p.Reason)
			}
			if p.Wake != tt.wantWake {
				t.Errorf("wake = %v, want %v (%s)", p.Wake, tt.wantWake, p.Reason)
			}
			if p.Poweroff != tt.wantPower {
				t.Errorf("poweroff = %v, want %v (%s)", p.Poweroff, tt.wantPower, p.Reason)
			}
			if !equal(p.StartRequested, tt.wantStartR) {
				t.Errorf("startRequested = %v, want %v", p.StartRequested, tt.wantStartR)
			}
			if p.GraceSince != tt.wantGrace {
				t.Errorf("graceSince = %d, want %d (%s)", p.GraceSince, tt.wantGrace, p.Reason)
			}
		})
	}
}
