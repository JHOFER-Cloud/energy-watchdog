package controller

import (
	"slices"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// TestMinRuntime covers the shed hold: a deficit arriving shortly after p1 came up is not
// acted on, so a day of intermittent sun can't run the wake/shed cycle twice.
func TestMinRuntime(t *testing.T) {
	guests := []proxmox.Guest{qemu(101, true), qemu(301, true)}
	gamingUp := append(append([]proxmox.Guest{}, guests...), qemu(601, true))

	tests := []struct {
		name      string
		min       time.Duration
		snap      Snapshot
		wantMode  state.Mode
		wantPower bool
		wantHeld  bool
		wantStart []int
	}{
		{
			name:     "deficit inside the window holds p1 up",
			min:      2 * time.Hour,
			snap:     Snapshot{Surplus: -300, SoC: 80, NodeUp: true, NodeUptime: 30 * time.Minute, Guests: guests, Mode: state.ModeRunning},
			wantMode: state.ModeRunning,
			wantHeld: true,
		},
		{
			name:      "deficit past the window sheds",
			min:       2 * time.Hour,
			snap:      Snapshot{Surplus: -300, SoC: 80, NodeUp: true, NodeUptime: 3 * time.Hour, Guests: guests, Mode: state.ModeRunning},
			wantMode:  state.ModeShed,
			wantPower: true,
		},
		{
			// Proxmox drops uptime without Sys.Audit; holding on that would strand p1 up.
			name:      "unknown uptime sheds",
			min:       2 * time.Hour,
			snap:      Snapshot{Surplus: -300, SoC: 80, NodeUp: true, Guests: guests, Mode: state.ModeRunning},
			wantMode:  state.ModeShed,
			wantPower: true,
		},
		{
			name:      "unset minRuntime sheds",
			snap:      Snapshot{Surplus: -300, SoC: 80, NodeUp: true, NodeUptime: 30 * time.Minute, Guests: guests, Mode: state.ModeRunning},
			wantMode:  state.ModeShed,
			wantPower: true,
		},
		{
			name:      "hold off outranks it",
			min:       2 * time.Hour,
			snap:      Snapshot{Surplus: 4000, SoC: 80, NodeUp: true, NodeUptime: 30 * time.Minute, Guests: guests, Mode: state.ModeRunning, ManualShed: true},
			wantMode:  state.ModeShed,
			wantPower: true,
		},
		{
			// A hold-on already keeps p1 up, and must not be reported as a minRuntime hold.
			name:     "hold on is not a minRuntime hold",
			min:      2 * time.Hour,
			snap:     Snapshot{Surplus: -300, SoC: 80, NodeUp: true, NodeUptime: 30 * time.Minute, Guests: guests, Mode: state.ModeRunning, ManualOn: true},
			wantMode: state.ModeRunning,
		},
		{
			name:     "surplus inside the window is not a hold",
			min:      2 * time.Hour,
			snap:     Snapshot{Surplus: 4000, SoC: 80, NodeUp: true, NodeUptime: 30 * time.Minute, Guests: guests, Mode: state.ModeRunning},
			wantMode: state.ModeRunning,
		},
		{
			// The whole deficit branch is held, so no gaming posture is entered either.
			name:     "deficit with a gaming guest stays running",
			min:      2 * time.Hour,
			snap:     Snapshot{Surplus: -300, SoC: 80, NodeUp: true, NodeUptime: 30 * time.Minute, Guests: gamingUp, Mode: state.ModeRunning},
			wantMode: state.ModeRunning,
			wantHeld: true,
		},
		{
			// A hold plans no power move, so the desktop VM is still started on the host that
			// is already up - the shed it would otherwise have raced never happens.
			name:      "a desktop VM asked for during a hold is still started",
			min:       2 * time.Hour,
			snap:      Snapshot{Surplus: -300, SoC: 80, NodeUp: true, NodeUptime: 30 * time.Minute, Guests: guests, Mode: state.ModeRunning, Wake: wants(601)},
			wantMode:  state.ModeRunning,
			wantHeld:  true,
			wantStart: []int{601},
		},
		{
			// The grace window runs on its own clock: minRuntime never reaches it.
			name:      "gaming grace still powers off",
			min:       2 * time.Hour,
			snap:      Snapshot{Surplus: -300, SoC: 80, NodeUp: true, NodeUptime: 30 * time.Minute, Guests: guests, Mode: state.ModeGaming, GraceSince: testNow.Add(-time.Hour).Unix()},
			wantMode:  state.ModeShed,
			wantPower: true,
		},
		{
			name:     "a hand-started p1 is still adopted",
			min:      2 * time.Hour,
			snap:     Snapshot{Surplus: -300, SoC: 80, NodeUp: true, NodeUptime: time.Minute, Guests: guests, Mode: state.ModeShed},
			wantMode: state.ModeGaming,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg(t)
			cfg.MinRuntime = config.Duration{Duration: tc.min}
			p := Decide(tc.snap, cfg, testNow)
			if p.NextMode != tc.wantMode {
				t.Errorf("mode = %s, want %s (reason: %s)", p.NextMode, tc.wantMode, p.Reason)
			}
			if p.Poweroff != tc.wantPower {
				t.Errorf("poweroff = %v, want %v", p.Poweroff, tc.wantPower)
			}
			if p.MinRuntimeHeld != tc.wantHeld {
				t.Errorf("minRuntimeHeld = %v, want %v", p.MinRuntimeHeld, tc.wantHeld)
			}
			if !slices.Equal(p.StartRequested, tc.wantStart) {
				t.Errorf("startRequested = %v, want %v", p.StartRequested, tc.wantStart)
			}
			// A held tick sheds nothing and moves no power: the whole point is that the node
			// carries on exactly as it was.
			if tc.wantHeld && (len(p.Migrate) > 0 || len(p.Stop) > 0 || p.Wake || p.RestoreStopped) {
				t.Errorf("held tick planned migrate=%v stop=%v wake=%v restore=%v",
					ids(p.Migrate), ids(p.Stop), p.Wake, p.RestoreStopped)
			}
			if tc.wantHeld && p.Reason == "" {
				t.Error("held tick left the reason empty")
			}
		})
	}
}
