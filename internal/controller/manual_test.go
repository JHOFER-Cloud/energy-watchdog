package controller

import (
	"io"
	"log/slog"
	"maps"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"

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
			snap:       Snapshot{Surplus: -300, SoC: 80, NodeUp: false, Mode: state.ModeShed, Wake: wants(601)},
			wantMode:   state.ModeGaming,
			wantWake:   true,
			wantStartR: []int{601},
			wantGrace:  testNow.Unix(),
		},
		{
			// A manual shed must not block someone gaming, same as the guard doesn't.
			name:       "shed, manual shed, request -> still wakes",
			snap:       Snapshot{Surplus: 4000, SoC: 90, NodeUp: false, Mode: state.ModeShed, ManualShed: true, Wake: wants(601)},
			wantMode:   state.ModeGaming,
			wantWake:   true,
			wantStartR: []int{601},
			wantGrace:  testNow.Unix(),
		},
		{
			// This is the shutting-down race: the loop already powered p1 off, and the request
			// is still live, so the next tick simply wakes it again. Nothing hangs.
			name:       "gaming, p1 went down, request live -> shed, then wake next tick",
			snap:       Snapshot{Surplus: -300, SoC: 80, NodeUp: false, Mode: state.ModeGaming, Wake: wants(601), GraceSince: testNow.Add(-time.Minute).Unix()},
			wantMode:   state.ModeShed,
			wantWake:   false,
			wantStartR: nil,
		},
		{
			// Already running: nothing to start, and the guard holds p1 from here on.
			name:       "gaming, requested VM already running -> no start",
			snap:       Snapshot{Surplus: -300, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, true)}, Mode: state.ModeGaming, Wake: wants(601)},
			wantMode:   state.ModeGaming,
			wantStartR: nil,
		},
		{
			// A request arriving late in an already-running grace window must not be cut off.
			name:       "gaming, request late in grace window -> hold, restart clock",
			snap:       Snapshot{Surplus: -300, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeGaming, Wake: wants(601), GraceSince: testNow.Add(-9 * time.Minute).Unix()},
			wantMode:   state.ModeGaming,
			wantPower:  false,
			wantStartR: []int{601},
			wantGrace:  testNow.Unix(),
		},
		{
			// p1 is up and serving: just start the VM, no mode change.
			name:       "running, request -> start the VM, stay running",
			snap:       Snapshot{Surplus: 4000, SoC: 90, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeRunning, Wake: wants(601)},
			wantMode:   state.ModeRunning,
			wantStartR: []int{601},
		},
		{
			// Deficit with a request but no VM up yet: hold the host for the session.
			name:       "running, deficit, request -> gaming, keep host up",
			snap:       Snapshot{Surplus: -300, SoC: 80, NodeUp: true, Guests: []proxmox.Guest{qemu(601, false)}, Mode: state.ModeRunning, Wake: wants(601)},
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

// intent.json is a documented break-glass path, so a hand-written one must not be able to
// make the loop start the same VM twice, or start something the gaming guard won't hold p1 for.
func TestRequestedWakeSanitisesIntent(t *testing.T) {
	cfg := testCfg(t)
	cfg.GamingGrace = config.Duration{Duration: 10 * time.Minute}
	c := &Controller{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	stale := testNow.Add(-11 * time.Minute).Unix()
	got := c.requestedWake(state.Intent{Wake: []state.WakeRequest{
		{VMID: 601, RequestedAt: testNow.Unix()},
		{VMID: 601, RequestedAt: testNow.Unix()}, // duplicate entry
		{VMID: 101, RequestedAt: testNow.Unix()}, // outside gamingGuard
		{VMID: 602, RequestedAt: stale},          // aged out
	}}, testNow)

	if len(got) != 1 || got[0].VMID != 601 {
		t.Errorf("requestedWake = %+v, want just 601", got)
	}
}

// wants builds live wake requests for the decision tests, dated at testNow.
func wants(vmids ...int) []state.WakeRequest {
	out := make([]state.WakeRequest, len(vmids))
	for i, id := range vmids {
		out[i] = state.WakeRequest{VMID: id, RequestedAt: testNow.Unix()}
	}
	return out
}

// TestWakeRequestIsSpentOnceTheVMIsUp walks the lifecycle of one Start click. The request must
// stop acting the moment the VM has been up, or shutting the VM down inside its TTL - the
// request lives for gamingGrace - just starts it again on the next tick.
func TestWakeRequestIsSpentOnceTheVMIsUp(t *testing.T) {
	cfg := testCfg(t)
	req := wants(601)
	clicked := req[0].RequestedAt

	tests := []struct {
		name     string
		guests   []proxmox.Guest
		done     map[int]int64
		wantStar []int
		wantDone map[int]int64
	}{
		{
			name:     "clicked, VM not up yet -> start it",
			guests:   []proxmox.Guest{qemu(601, false)},
			wantStar: []int{601},
		},
		{
			name:     "VM came up -> nothing to start, request marked spent",
			guests:   []proxmox.Guest{qemu(601, true)},
			wantDone: map[int]int64{601: clicked},
		},
		{
			// The flaw: same still-live request, VM now shut down by the user. It must stay down.
			name:     "user shut the VM down -> left alone",
			guests:   []proxmox.Guest{qemu(601, false)},
			done:     map[int]int64{601: clicked},
			wantDone: map[int]int64{601: clicked},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := Snapshot{
				Surplus: -300, SoC: 80, NodeUp: true, Mode: state.ModeGaming,
				Guests: tt.guests, Wake: req, WakeDone: tt.done,
			}
			p := Decide(snap, cfg, testNow)
			if !equal(p.StartRequested, tt.wantStar) {
				t.Errorf("startRequested = %v, want %v", p.StartRequested, tt.wantStar)
			}
			if !maps.Equal(p.WakeDone, tt.wantDone) {
				t.Errorf("wakeDone = %v, want %v", p.WakeDone, tt.wantDone)
			}
		})
	}

	// Clicking Start again re-arms it: the new request is newer than the spent marker.
	again := []state.WakeRequest{{VMID: 601, RequestedAt: clicked + 1}}
	p := Decide(Snapshot{
		Surplus: -300, SoC: 80, NodeUp: true, Mode: state.ModeGaming,
		Guests: []proxmox.Guest{qemu(601, false)}, Wake: again, WakeDone: map[int]int64{601: clicked},
	}, cfg, testNow)
	if !equal(p.StartRequested, []int{601}) {
		t.Errorf("a fresh click must re-arm: startRequested = %v, want [601]", p.StartRequested)
	}
}

// The tick that marks a request spent changes nothing else, so it must not be treated as a
// noop - skipping that write would lose the marker and let the VM be restarted.
func TestMarkingAWakeSpentIsNotANoop(t *testing.T) {
	snap := Snapshot{Mode: state.ModeGaming}
	p := Plan{NextMode: state.ModeGaming, WakeDone: map[int]int64{601: testNow.Unix()}}
	if isNoop(p, snap) {
		t.Error("isNoop = true for a new wakeDone marker, so it would never be persisted")
	}
}
