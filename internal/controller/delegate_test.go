package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"strings"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/alertmgr"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
	"gopkg.in/yaml.v3"
)

// fakeNutDog records the power requests it receives, in order.
type fakeNutDog struct {
	mu   sync.Mutex
	got  []string
	auth []string
}

func (f *fakeNutDog) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/loads/p1/power" || r.Method != http.MethodPut {
			t.Errorf("unexpected power call: %s %s", r.Method, r.URL.Path)
		}
		var body struct{ Desired string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.got = append(f.got, body.Desired)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (f *fakeNutDog) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func (f *fakeNutDog) authHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}

// delegateFixture wires a controller whose p1 power belongs to a fake nut-dog. surplus drives
// the solar signal; the Proxmox fake reports the node up with one stoppable guest.
func delegateFixture(t *testing.T, surplus, nutURL string, mode state.Mode, nodeUp bool) (*Controller, *[]string) {
	t.Helper()
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"` + surplus + `"]}]}}`))
	}))
	t.Cleanup(promSrv.Close)

	var calls []string
	var mu sync.Mutex
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api2/json/nodes":
			status := "offline"
			if nodeUp {
				status = "online"
			}
			_, _ = w.Write([]byte(`{"data":[{"node":"pve-1","status":"` + status + `","uptime":100000}]}`))
		case "/api2/json/nodes/pve-1/qemu":
			_, _ = w.Write([]byte(`{"data":[{"vmid":301,"status":"running","name":"g"}]}`))
		case "/api2/json/nodes/pve-1/lxc":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/api2/json/cluster/replication":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/api2/json/nodes/pve-1/tasks/UPID:done/status":
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			// stopall and the other task-returning calls the shed makes
			_, _ = w.Write([]byte(`{"data":"UPID:done"}`))
		}
	}))
	t.Cleanup(px.Close)

	store := state.NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Save(context.Background(), state.State{Mode: mode}); err != nil {
		t.Fatal(err)
	}
	var guests config.Guests
	if err := yaml.Unmarshal([]byte("stop: [\"300-399\"]\n"), &guests); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Prometheus: config.Prometheus{
			URL: promSrv.URL, Window: "30m", HeadroomWatts: 1000, MinBatteryPercent: 0,
			ProductionMetric: "prod", ConsumptionMetric: "cons",
		},
		Proxmox: config.Proxmox{
			Node: "pve-1", TargetNodes: []string{"pve-2"},
			StopTimeout: config.Duration{Duration: time.Second},
			WakeTimeout: config.Duration{Duration: 50 * time.Millisecond},
		},
		Guests:   guests,
		PowerAPI: &config.PowerAPI{URL: nutURL, Load: "p1", Token: "t0ken"},
	}
	c := New(cfg, prom.New(promSrv.URL), proxmox.New(px.URL, "u@pam!t", "s", nil),
		map[string]*alertmgr.Client{}, store, metrics.New(false),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c, &calls
}

// A shed must reach nut-dog, and must never go through Proxmox's node shutdown.
func TestShedDelegatesPowerOff(t *testing.T) {
	nut := &fakeNutDog{}
	srv := nut.server(t)
	defer srv.Close()

	c, pxCalls := delegateFixture(t, "-800", srv.URL, state.ModeRunning, true)
	c.reconcile(context.Background())

	if got := nut.requests(); len(got) != 1 || got[0] != "off" {
		t.Fatalf("power requests = %v, want [off]", got)
	}
	if got := nut.authHeaders()[0]; got != "Bearer t0ken" {
		t.Errorf("authorization = %q", got)
	}
	for _, path := range *pxCalls {
		if path == "/api2/json/nodes/pve-1/status" {
			t.Error("shed called Proxmox node shutdown; power belongs to nut-dog")
		}
	}
}

// A settled mode still restates its wish, so a nut-dog that restarted and forgot is told
// again within one tick - which is what its startup grace is sized for.
func TestSettledModeRestatesPower(t *testing.T) {
	nut := &fakeNutDog{}
	srv := nut.server(t)
	defer srv.Close()

	// Surplus with p1 already up and mode running: nothing to do at all.
	c, _ := delegateFixture(t, "5000", srv.URL, state.ModeRunning, true)
	c.reconcile(context.Background())
	c.reconcile(context.Background())

	got := nut.requests()
	if len(got) != 2 || got[0] != "on" || got[1] != "on" {
		t.Errorf("power requests = %v, want [on on] - one per tick", got)
	}
}

// The wish must come from what the loop established, not from the mode label. Both cases
// here are ones where Decide deliberately plans nothing, so nut-dog must be told nothing.
func TestRestateNeverOverridesTheLoop(t *testing.T) {
	tests := []struct {
		name    string
		surplus string
		mode    state.Mode
		nodeUp  bool
		want    string
	}{
		// p1 down while the mode still says running: the tick only corrects the mode, so
		// nothing is asserted about power until the next one decides.
		{"running, p1 down, neutral signal", "500", state.ModeRunning, false, "hold"},
		{"running, p1 down, surplus", "5000", state.ModeRunning, false, "hold"},
		// p1 started by hand during a shed, seen outside the fresh-boot window: no migrate
		// and no stop are planned, so asserting "off" would cut power under running guests.
		{"shed, p1 up, deficit", "-800", state.ModeShed, true, "hold"},
		{"shed, p1 down, deficit", "-800", state.ModeShed, false, "off"},
		{"running, p1 up, surplus", "5000", state.ModeRunning, true, "on"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nut := &fakeNutDog{}
			srv := nut.server(t)
			defer srv.Close()
			c, _ := delegateFixture(t, tt.surplus, srv.URL, tt.mode, tt.nodeUp)

			c.reconcile(context.Background())

			got := nut.requests()
			if len(got) != 1 || got[0] != tt.want {
				t.Errorf("power requests = %v, want [%s]", got, tt.want)
			}
		})
	}
}

// A blind watchdog must still speak. Going quiet hands p1 to nut-dog's stale request, which
// on a UPS recovery wakes it with no solar reading behind the decision.
func TestBlindWatchdogHoldsPower(t *testing.T) {
	nut := &fakeNutDog{}
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "5000", srv.URL, state.ModeRunning, true)
	dead := httptest.NewServer(nil)
	dead.Close() // Prometheus went down with the cluster
	c.prom = prom.New(dead.URL)

	c.reconcile(context.Background())
	c.reconcile(context.Background())

	got := nut.requests()
	if len(got) != 2 || got[0] != "hold" || got[1] != "hold" {
		t.Errorf("power requests = %v, want [hold hold] - one per blind tick", got)
	}
}

// A power request that nut-dog rejects - wrong token, wrong load name - must show up on the
// metric. It is the only signal that the thing actually moving p1 has stopped working: every
// other gauge stays green, because from the loop's point of view the plan was applied.
func TestFailedPowerRequestIsVisible(t *testing.T) {
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unknown load", http.StatusNotFound)
	}))
	defer reject.Close()

	c, _ := delegateFixture(t, "-800", reject.URL, state.ModeRunning, true)
	m := metrics.New(false)
	c.metrics = m

	c.reconcile(context.Background()) // a shed: powerOff goes through the API

	rr := httptest.NewRecorder()
	m.Handler()(rr, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rr.Body.String(), "energy_watchdog_power_request_success 0") {
		t.Errorf("a rejected power request left the metric green:\n%s", rr.Body.String())
	}
}
