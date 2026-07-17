package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	m := New(true)
	m.Update(Sample{
		Surplus: 1800, SurplusRaw: 2400, SoC: 73.5,
		NodeUp: false, Gaming: true, Mode: "shed", Tick: 1700000000, OK: true,
	})

	rr := httptest.NewRecorder()
	m.Handler()(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()

	for _, want := range []string{
		"energy_watchdog_surplus_watts 1800",
		"energy_watchdog_surplus_raw_watts 2400",
		"energy_watchdog_battery_percent 73.5",
		"energy_watchdog_node_up 0",
		"energy_watchdog_gaming_active 1",
		"energy_watchdog_dry_run 1",
		`energy_watchdog_mode{mode="shed"} 1`,
		`energy_watchdog_mode{mode="running"} 0`,
		"energy_watchdog_last_reconcile_success 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n--- got ---\n%s", want, body)
		}
	}
}

// TestMarkStaleKeepsLastGood: a failed tick must not zero the gauges. Only the success flag
// (and tick) move, so a transient observe error doesn't flap surplus/mode on the dashboard.
func TestMarkStaleKeepsLastGood(t *testing.T) {
	m := New(false)
	m.Update(Sample{Surplus: 1800, SoC: 73.5, NodeUp: true, Mode: "running", Tick: 1700000000, OK: true})
	m.MarkStale(1700000060)

	rr := httptest.NewRecorder()
	m.Handler()(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()

	for _, want := range []string{
		"energy_watchdog_surplus_watts 1800",       // preserved, not zeroed
		"energy_watchdog_battery_percent 73.5",     // preserved
		"energy_watchdog_node_up 1",                // preserved
		`energy_watchdog_mode{mode="running"} 1`,   // preserved
		"energy_watchdog_last_reconcile_success 0", // the failure is visible
		"energy_watchdog_last_reconcile_timestamp_seconds 1700000060",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("after MarkStale, metrics missing %q\n--- got ---\n%s", want, body)
		}
	}
}
