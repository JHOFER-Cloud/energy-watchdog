package controller

import (
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// A planned power action carries its reason to nut-dog, which logs it as the record of why
// p1 moved. An empty one would leave a power cut unexplained, so no branch may plan Wake or
// Poweroff without setting Reason.
func TestPowerActionsAlwaysCarryAReason(t *testing.T) {
	cfg := testCfg(t)
	surplus := map[string]float64{"deficit": -800, "neutral": 500, "surplus": 4000}
	now := time.Now()
	var wakes, poweroffs int

	for _, mode := range []state.Mode{state.ModeRunning, state.ModeShed, state.ModeGaming} {
		for _, hold := range []string{"none", "hold off", "hold on"} {
			for _, sig := range []string{"deficit", "neutral", "surplus"} {
				for _, nodeUp := range []bool{true, false} {
					for _, guest := range []bool{true, false} {
						for _, req := range []bool{true, false} {
							if guest && !nodeUp {
								continue
							}
							s := Snapshot{
								Surplus: surplus[sig], SoC: 90, NodeUp: nodeUp, Mode: mode,
								NodeUptime: time.Hour,
								StoppedSet: []state.GuestRef{{VMID: 301, Type: "qemu"}},
								ManualShed: hold == "hold off",
								ManualOn:   hold == "hold on",
							}
							if nodeUp {
								s.Guests = []proxmox.Guest{qemu(101, true), qemu(301, true)}
								if guest {
									s.Guests = append(s.Guests, qemu(600, true))
								}
							}
							if req {
								s.Wake = []state.WakeRequest{{VMID: 600, RequestedAt: now.Unix()}}
							}
							// Grace elapsed, so the gaming power-off branch is reachable.
							if mode == state.ModeGaming {
								s.GraceSince = now.Add(-time.Hour).Unix()
							}

							p := Decide(s, cfg, now)
							if p.Wake {
								wakes++
							}
							if p.Poweroff {
								poweroffs++
							}
							if (p.Wake || p.Poweroff) && p.Reason == "" {
								t.Errorf("power action without a reason: mode=%s hold=%s sig=%s nodeUp=%v guest=%v req=%v -> wake=%v poweroff=%v",
									mode, hold, sig, nodeUp, guest, req, p.Wake, p.Poweroff)
							}
						}
					}
				}
			}
		}
	}
	// Without this the loop above could stop reaching either branch and still pass.
	if wakes == 0 || poweroffs == 0 {
		t.Fatalf("enumeration covered no power action: wakes=%d poweroffs=%d", wakes, poweroffs)
	}
	t.Logf("covered %d wakes, %d poweroffs", wakes, poweroffs)
}
