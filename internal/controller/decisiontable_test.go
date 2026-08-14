package controller

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// row is one input combination and what Decide made of it.
type row struct {
	Mode, Hold, Signal, Node, Guest, Request, Grace string
	NextMode, Wish                                  string
	Migrate, Stop, StartReq                         []int
	Poweroff, Wake, RestoreStopped                  bool
	GraceAction, Reason                             string
}

// TestGenerateDecisionTable dumps every reachable Decide outcome as JSON, for the decision
// reference page. Skipped unless OUT names a file, so it costs nothing in a normal run but
// still compiles against Decide - a signature change breaks the build instead of rotting.
// See DEVELOPMENT.md, "Regenerating the decision reference".
func TestGenerateDecisionTable(t *testing.T) {
	out := os.Getenv("OUT")
	if out == "" {
		t.Skip("set OUT=<path> to regenerate the decision table")
	}
	cfg := testCfg(t)
	surplus := map[string]float64{"deficit": -800, "neutral": 500, "surplus": 4000}
	var rows []row
	for _, mode := range []state.Mode{state.ModeRunning, state.ModeShed, state.ModeGaming} {
		for _, hold := range []string{"none", "hold off", "hold on"} {
			for _, sig := range []string{"deficit", "neutral", "surplus"} {
				for _, nodeUp := range []bool{true, false} {
					for _, guest := range []bool{true, false} {
						for _, req := range []bool{true, false} {
							for _, grace := range []string{"stopped", "running", "elapsed"} {
								if guest && !nodeUp {
									continue
								}
								if mode != state.ModeGaming && grace != "stopped" {
									continue
								}
								s := Snapshot{
									Surplus: surplus[sig], SoC: 90, NodeUp: nodeUp, Mode: mode,
									NodeUptime: time.Hour,
									ManualShed: hold == "hold off",
									ManualOn:   hold == "hold on",
								}
								if nodeUp {
									s.Guests = []proxmox.Guest{qemu(101, true), qemu(301, true)}
									if guest {
										s.Guests = append(s.Guests, qemu(601, true))
									}
								}
								if req {
									s.Wake = wants(601)
								}
								switch grace {
								case "running":
									s.GraceSince = testNow.Add(-time.Minute).Unix()
								case "elapsed":
									s.GraceSince = testNow.Add(-11 * time.Minute).Unix()
								}
								p := Decide(s, cfg, testNow)
								ga := "carried"
								switch {
								case p.GraceSince == 0 && s.GraceSince != 0:
									ga = "cleared"
								case p.GraceSince == testNow.Unix() && s.GraceSince != testNow.Unix():
									ga = "started"
								}
								rows = append(rows, row{
									Mode: string(mode), Hold: hold, Signal: sig,
									Node:    map[bool]string{true: "up", false: "down"}[nodeUp],
									Guest:   map[bool]string{true: "yes", false: "no"}[guest],
									Request: map[bool]string{true: "yes", false: "no"}[req],
									// askedOff is controller state, not a Decide input, so it stays out of the
									// enumeration: Wish is the steady-state answer. Mid-shed (we asked for the
									// power-off, p1 not down yet) it is off wherever this shows hold.
									Grace: grace, NextMode: string(p.NextMode), Wish: powerWish(p, s, false),
									Migrate: ids(p.Migrate), Stop: ids(p.Stop), RestoreStopped: p.RestoreStopped,
									StartReq: p.StartRequested, Poweroff: p.Poweroff, Wake: p.Wake,
									GraceAction: ga, Reason: p.Reason,
								})
							}
						}
					}
				}
			}
		}
	}
	b, _ := json.Marshal(rows)
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("rows: %d", len(rows))
}
