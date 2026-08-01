package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/alertmgr"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// TestReconcileFailedApplyIsVisible drives the shed->running wake against a node that never
// comes up. The failure must reach the metrics, or EnergyWatchdogReconcileFailing can't fire.
func TestReconcileFailedApplyIsVisible(t *testing.T) {
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"5000"]}]}}`))
	}))
	defer promSrv.Close()

	var woke atomic.Bool
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/nodes": // never comes up, so the wake times out
			_, _ = w.Write([]byte(`{"data":[{"node":"pve-1","status":"offline","uptime":0}]}`))
		case "/api2/json/cluster/replication":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/api2/json/nodes/pve-1/wakeonlan":
			woke.Store(true)
			_, _ = w.Write([]byte(`{"data":"cc:28:aa:0e:59:f3"}`))
		default:
			t.Errorf("unexpected proxmox path %s", r.URL.Path)
		}
	}))
	defer px.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	store := state.NewFileStore(statePath)
	if err := store.Save(context.Background(), state.State{Mode: state.ModeShed}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Prometheus: config.Prometheus{
			URL: promSrv.URL, Window: "30m", HeadroomWatts: 1000, MinBatteryPercent: 20,
			ProductionMetric: "prod", ConsumptionMetric: "cons", BatteryMetric: "soc",
		},
		Proxmox: config.Proxmox{
			Node:        "pve-1",
			WakeTimeout: config.Duration{Duration: 50 * time.Millisecond},
		},
	}
	m := metrics.New(false)
	c := New(cfg,
		prom.New(promSrv.URL),
		proxmox.New(px.URL, "u@pam!t", "s", nil),
		map[string]*alertmgr.Client{},
		store,
		m,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	c.reconcile(context.Background())

	if !woke.Load() {
		t.Error("wake did not call the proxmox wakeonlan endpoint")
	}

	rr := httptest.NewRecorder()
	m.Handler()(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()

	for _, want := range []string{
		"energy_watchdog_last_reconcile_success 0",
		`energy_watchdog_mode{mode="shed"} 1`,    // the mode actually in force
		`energy_watchdog_mode{mode="running"} 0`, // not the one the plan wanted
		"energy_watchdog_node_up 0",
		"energy_watchdog_surplus_watts 5000", // the observation still publishes
	} {
		if !strings.Contains(body, want) {
			t.Errorf("after a failed apply, metrics missing %q\n--- got ---\n%s", want, body)
		}
	}

	// The failed wake must not latch the mode forward, or the next tick stops retrying.
	st, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeShed {
		t.Errorf("persisted mode = %q, want shed", st.Mode)
	}
}
