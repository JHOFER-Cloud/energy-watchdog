package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
	"gopkg.in/yaml.v3"
)

// restoreEnv stands up a fake Proxmox whose guest list is fixed, and returns the controller
// plus a pointer to the vms= parameter the startall endpoint received ("" if never called).
func restoreEnv(t *testing.T, qemu string) (*Controller, *string) {
	t.Helper()
	started := new(string)
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/startall"):
			_ = r.ParseForm()
			*started = r.Form.Get("vms")
			_, _ = w.Write([]byte(`{"data":"UPID:startall"}`))
		case strings.HasSuffix(p, "/nodes/pve-1/qemu"):
			_, _ = w.Write([]byte(qemu))
		case strings.HasSuffix(p, "/nodes/pve-1/lxc"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.Contains(p, "/tasks/"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			t.Errorf("unexpected proxmox path %s", p)
		}
	}))
	t.Cleanup(px.Close)

	var g config.Guests
	if err := yaml.Unmarshal([]byte("stop: [\"200-299\", \"700\"]\ngamingGuard: [\"600-699\"]\n"), &g); err != nil {
		t.Fatalf("unmarshal guests: %v", err)
	}
	cfg := &config.Config{
		DryRun:  config.DryRunFull,
		Guests:  g,
		Proxmox: config.Proxmox{Node: "pve-1"},
	}
	c := New(cfg,
		prom.New("http://unused"),
		proxmox.New(px.URL, "u@pam!t", "s", nil),
		nil,
		state.NewFileStore(filepath.Join(t.TempDir(), "state.json")),
		metrics.New(false),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return c, started
}

// Good-morning starts the stop-class guests that are down, skipping ones already running,
// ones outside the stop ranges, and ones tagged no-autostart.
func TestRestoreStoppedSelectsGuests(t *testing.T) {
	c, started := restoreEnv(t, `{"data":[
		{"vmid":201,"status":"stopped"},
		{"vmid":202,"status":"stopped","tags":"en_no-autostart"},
		{"vmid":203,"status":"running"},
		{"vmid":601,"status":"stopped"},
		{"vmid":700,"status":"stopped","tags":"gpu;en_no-autostart"}
	]}`)

	if err := c.restoreStopped(context.Background()); err != nil {
		t.Fatalf("restoreStopped: %v", err)
	}
	if *started != "201" {
		t.Errorf("started %q, want \"201\" (202/700 tagged, 203 already up, 601 out of range)", *started)
	}
}

// The regression this design exists for: the guests were stopped by something the watchdog
// never drove - a UPS shed, a crash, pve-guests during a host shutdown - so nothing was ever
// recorded. The restore must still bring them back, which it does by deriving the set from
// config and the live guest list rather than from persisted state.
func TestRestoreStoppedNeedsNoRecordOfHavingStoppedThem(t *testing.T) {
	c, started := restoreEnv(t, `{"data":[{"vmid":201,"status":"stopped"},{"vmid":202,"status":"stopped"}]}`)

	st, err := c.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeRunning {
		t.Fatalf("fixture: expected a fresh store, got mode %q", st.Mode)
	}

	if err := c.restoreStopped(context.Background()); err != nil {
		t.Fatalf("restoreStopped: %v", err)
	}
	if *started != "201,202" {
		t.Errorf("started %q, want \"201,202\" from an empty state store", *started)
	}
}

// Every stop-class guest tagged: the restore must issue no call at all rather than a
// start with an empty vmid list, which Proxmox would reject.
func TestRestoreStoppedNoopWhenNothingToStart(t *testing.T) {
	c, started := restoreEnv(t, `{"data":[{"vmid":201,"status":"stopped","tags":"en_no-autostart"}]}`)

	if err := c.restoreStopped(context.Background()); err != nil {
		t.Fatalf("restoreStopped: %v", err)
	}
	if *started != "" {
		t.Errorf("startall called with %q, want no call", *started)
	}
}
