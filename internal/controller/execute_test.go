package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/alertmgr"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// TestExecuteShedCycle drives a full RUNNING->SHED execution through the real clients
// against fake Proxmox/Alertmanager servers, asserting the action order and that the
// stopped set + mode are persisted.
func TestExecuteShedCycle(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
	)
	record := func(s string) { mu.Lock(); calls = append(calls, s); mu.Unlock() }

	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/migrateall"):
			_ = r.ParseForm()
			record("migrate-" + r.Form.Get("vms"))
			_, _ = w.Write([]byte(`{"data":"UPID:migrateall"}`))
		case strings.HasSuffix(p, "/stopall"):
			_ = r.ParseForm()
			record("stop-" + r.Form.Get("vms"))
			_, _ = w.Write([]byte(`{"data":"UPID:stopall"}`))
		// Read back after each bulk task: 101 has left the node, 301 is no longer running.
		case strings.HasSuffix(p, "/nodes/pve-1/qemu"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":301,"status":"stopped"}]}`))
		case strings.HasSuffix(p, "/nodes/pve-1/lxc"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.Contains(p, "/tasks/"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		case p == "/api2/json/cluster/replication":
			_, _ = w.Write([]byte(`{"data":[{"id":"104-0","source":"pve-2","target":"pve-1"}]}`))
		case strings.HasPrefix(p, "/api2/json/cluster/replication/"):
			record("repl-disable-104-0")
			_, _ = w.Write([]byte(`{"data":null}`))
		case strings.Contains(p, "/nodes/pve-1/status"):
			record("poweroff")
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			t.Errorf("unexpected proxmox path %s", p)
		}
	}))
	defer px.Close()

	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // List during reconcile: no silences yet.
			_, _ = w.Write([]byte(`[]`))
			return
		}
		record("silence")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"silenceID":"sil-1"}`))
	}))
	defer am.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := &config.Config{
		DryRun: config.DryRunFull,
		Proxmox: config.Proxmox{
			Node:           "pve-1",
			TargetNodes:    []string{"pve-2"},
			MigrateTimeout: config.Duration{Duration: time.Minute},
			StopTimeout:    config.Duration{Duration: time.Minute},
		},
		Alertmanager: config.Alertmanager{
			URLs:     []string{am.URL},
			Silences: []config.Silence{{Matchers: []config.Matcher{{Name: "node", Value: ".*-p1", IsRegex: true}}}},
		},
	}
	c := New(cfg,
		prom.New("http://unused"),
		proxmox.New(px.URL, "u@pam!t", "s", nil),
		map[string]*alertmgr.Client{am.URL: alertmgr.New(am.URL, nil)},
		state.NewFileStore(statePath),
		metrics.New(false),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	plan := Plan{
		Migrate:  []proxmox.Guest{{VMID: 101, Type: proxmox.TypeQEMU, Running: true}},
		Stop:     []proxmox.Guest{{VMID: 301, Type: proxmox.TypeQEMU, Running: true}},
		Poweroff: true,
		NextMode: state.ModeShed,
	}
	// Through apply, not execute: silencing now happens there, so only apply can pin the order.
	snap := Snapshot{Mode: state.ModeRunning, NodeUp: true}
	if err := c.apply(context.Background(), plan, snap); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Order: silence first, because migrating and stopping the guests is what makes their
	// alerts fire and that runs for tens of minutes. Then replication is disabled before the
	// poweroff so no run can land inside the shutdown window (JHC-538).
	want := []string{"silence", "migrate-101", "stop-301", "repl-disable-104-0", "poweroff"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q (all: %v)", i, calls[i], want[i], calls)
		}
	}

	// State persisted: shed mode. The stopped guests are deliberately NOT recorded - the
	// good-morning restore derives them from config and the live guest list instead.
	st, err := state.NewFileStore(statePath).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeShed {
		t.Errorf("mode = %q, want shed", st.Mode)
	}
}

// TestApplyPersistsDryRunTransition: in a dry-run mode the state machine must still advance
// and persist (only the physical actions are skipped), so the mode latches tick to tick and
// the dashboard previews what live would decide.
func TestApplyPersistsDryRunTransition(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	c := dryRunController(t, config.DryRunLog, statePath)

	// RUNNING->SHED with the physical intents set; log mode persists the transition but must
	// touch no Proxmox/WoL (the clients point at unused URLs, so any call would error).
	plan := Plan{Poweroff: true, NextMode: state.ModeShed}
	if err := c.apply(context.Background(), plan, Snapshot{Mode: state.ModeRunning}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	st, err := state.NewFileStore(statePath).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeShed {
		t.Errorf("mode = %q, want shed (dry-run must advance + persist state)", st.Mode)
	}
}

// TestApplyNoopDoesNotPersist: when the plan changes nothing there must be no write, so the
// state ConfigMap doesn't churn every tick. A store left unwritten loads the default mode.
func TestApplyNoopDoesNotPersist(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	c := dryRunController(t, config.DryRunLog, statePath)

	if err := c.apply(context.Background(), Plan{NextMode: state.ModeShed}, Snapshot{Mode: state.ModeShed}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	st, err := state.NewFileStore(statePath).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeRunning { // default; proves nothing was written
		t.Errorf("mode = %q, want running (noop must not persist)", st.Mode)
	}
}

// TestMigrateAllRetriesOnNextTarget: a target that can't take a guest must not strand it on
// p1. The bulk call reports one aggregate result, so what's left is read back and retried.
func TestMigrateAllRetriesOnNextTarget(t *testing.T) {
	var (
		mu      sync.Mutex
		calls   []string
		onNode  = map[int]bool{101: true, 102: true}
		badTask = map[string]bool{}
	)
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/migrateall"):
			_ = r.ParseForm()
			target, vms := r.Form.Get("target"), r.Form.Get("vms")
			calls = append(calls, target+":"+vms)
			upid := "UPID:" + target
			if target == "pve-2" { // pve-2 refuses everything; the guests stay put
				badTask[upid] = true
			} else {
				for _, s := range strings.Split(vms, ",") {
					id, _ := strconv.Atoi(s)
					delete(onNode, id)
				}
			}
			_, _ = w.Write([]byte(`{"data":"` + upid + `"}`))
		case strings.HasSuffix(p, "/nodes/pve-1/qemu"):
			var items []string
			for id := range onNode {
				items = append(items, fmt.Sprintf(`{"vmid":%d,"status":"running"}`, id))
			}
			_, _ = w.Write([]byte(`{"data":[` + strings.Join(items, ",") + `]}`))
		case strings.HasSuffix(p, "/nodes/pve-1/lxc"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.Contains(p, "/tasks/"):
			status := "OK"
			for upid := range badTask {
				if strings.Contains(p, url.PathEscape(upid)) {
					status = "migration aborted"
				}
			}
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"` + status + `"}}`))
		default:
			t.Errorf("unexpected proxmox path %s", p)
		}
	}))
	defer px.Close()

	c := bulkController(t, px.URL, []string{"pve-2", "pve-3"})
	guests := []proxmox.Guest{
		{VMID: 101, Type: proxmox.TypeQEMU, Running: true},
		{VMID: 102, Type: proxmox.TypeQEMU, Running: true},
	}
	if err := c.migrateAll(context.Background(), guests); err != nil {
		t.Fatalf("migrateAll: %v", err)
	}
	if len(onNode) != 0 {
		t.Errorf("guests %v left on pve-1, want none", onNode)
	}
	// Round 0 splits them one each (concurrently, so in either order); 101 comes back from the
	// refusal and round 1 sends it to the target that works.
	want := map[string]bool{"pve-2:101": true, "pve-3:102": true, "pve-3:101": true}
	if len(calls) != len(want) {
		t.Fatalf("migrateall calls = %v, want %v", calls, want)
	}
	for _, c := range calls {
		if !want[c] {
			t.Errorf("unexpected migrateall call %q (all: %v)", c, calls)
		}
	}
}

// TestStopAllRecordsOnlyWhatStopped: with force-stop=0 a guest that won't shut down fails the
// task. The ones that did stop must still be recorded, or good-morning never starts them again.
func TestStopAllRecordsOnlyWhatStopped(t *testing.T) {
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/stopall"):
			_, _ = w.Write([]byte(`{"data":"UPID:stopall"}`))
		case strings.HasSuffix(p, "/nodes/pve-1/qemu"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":301,"status":"stopped"},{"vmid":302,"status":"running"}]}`))
		case strings.HasSuffix(p, "/nodes/pve-1/lxc"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.Contains(p, "/tasks/"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"shutdown timed out"}}`))
		default:
			t.Errorf("unexpected proxmox path %s", p)
		}
	}))
	defer px.Close()

	c := bulkController(t, px.URL, nil)
	stopped, err := c.stopAll(context.Background(), []proxmox.Guest{
		{VMID: 301, Type: proxmox.TypeQEMU, Running: true},
		{VMID: 302, Type: proxmox.TypeQEMU, Running: true},
	})
	if err == nil {
		t.Error("stopAll = nil error, want the failed task surfaced (302 never went down)")
	}
	if len(stopped) != 1 || stopped[0].VMID != 301 {
		t.Errorf("stopped = %+v, want [301] only", stopped)
	}
}

// TestStopAllBoundsAWedgedTask: WaitTask polls until its ctx is done, so a task Proxmox never
// reports as finished would hang the reconcile loop forever without a deadline on the wait.
func TestStopAllBoundsAWedgedTask(t *testing.T) {
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case strings.HasSuffix(p, "/stopall"):
			_, _ = w.Write([]byte(`{"data":"UPID:stopall"}`))
		case strings.HasSuffix(p, "/nodes/pve-1/qemu"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":301,"status":"stopped"}]}`))
		case strings.HasSuffix(p, "/nodes/pve-1/lxc"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.Contains(p, "/tasks/"):
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`)) // never finishes
		}
	}))
	defer px.Close()

	c := bulkController(t, px.URL, nil)
	c.cfg.Proxmox.StopTimeout = config.Duration{Duration: 50 * time.Millisecond}

	done := make(chan []state.GuestRef, 1)
	go func() {
		stopped, err := c.stopAll(context.Background(), []proxmox.Guest{{VMID: 301, Type: proxmox.TypeQEMU}})
		if err == nil {
			t.Error("stopAll = nil error, want the wait deadline surfaced")
		}
		done <- stopped
	}()
	select {
	case stopped := <-done:
		// The read-back runs on the caller's ctx, so what did go down is still recorded.
		if len(stopped) != 1 || stopped[0].VMID != 301 {
			t.Errorf("stopped = %+v, want [301] recorded despite the timeout", stopped)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stopAll did not return: the bulk wait is unbounded")
	}
}

func bulkController(t *testing.T, pxURL string, targets []string) *Controller {
	t.Helper()
	return New(
		&config.Config{Proxmox: config.Proxmox{
			Node:           "pve-1",
			TargetNodes:    targets,
			MigrateTimeout: config.Duration{Duration: time.Minute},
			StopTimeout:    config.Duration{Duration: time.Minute},
		}},
		prom.New("http://unused"),
		proxmox.New(pxURL, "u@pam!t", "s", nil),
		map[string]*alertmgr.Client{},
		state.NewFileStore(filepath.Join(t.TempDir(), "state.json")),
		metrics.New(true),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func dryRunController(t *testing.T, dry config.DryRunMode, statePath string) *Controller {
	t.Helper()
	return New(
		&config.Config{DryRun: dry},
		prom.New("http://unused"),
		proxmox.New("http://unused", "u@pam!t", "s", nil),
		map[string]*alertmgr.Client{},
		state.NewFileStore(statePath),
		metrics.New(true),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}
