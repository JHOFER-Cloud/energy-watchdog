package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		case strings.Contains(p, "/qemu/101/migrate"):
			record("migrate-101")
			_, _ = w.Write([]byte(`{"data":"UPID:migrate-101"}`))
		case strings.Contains(p, "/qemu/301/status/shutdown"):
			record("stop-301")
			_, _ = w.Write([]byte(`{"data":"UPID:stop-301"}`))
		case strings.Contains(p, "/tasks/"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
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
		Silence:  true,
		Poweroff: true,
		NextMode: state.ModeShed,
	}
	snap := Snapshot{Mode: state.ModeRunning}
	if err := c.execute(context.Background(), plan, snap); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Order: migrate before stop before silence before poweroff.
	want := []string{"migrate-101", "stop-301", "silence", "poweroff"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q (all: %v)", i, calls[i], want[i], calls)
		}
	}

	// State persisted: shed mode and 301 recorded as stopped. Silences are no longer
	// tracked in state - they're reconciled against Alertmanager by createdBy.
	st, err := state.NewFileStore(statePath).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeShed {
		t.Errorf("mode = %q, want shed", st.Mode)
	}
	if len(st.Stopped) != 1 || st.Stopped[0].VMID != 301 {
		t.Errorf("stopped = %+v, want [301]", st.Stopped)
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
