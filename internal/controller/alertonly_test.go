package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/alertmgr"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// ctlWithAMs builds a controller wired to the given Alertmanager URLs, for the silence
// reconcile tests (which take no Proxmox or state actions).
func ctlWithAMs(cfg *config.Config, urls ...string) *Controller {
	ams := map[string]*alertmgr.Client{}
	for _, u := range urls {
		ams[u] = alertmgr.New(u, nil)
	}
	return New(cfg,
		prom.New("http://unused"),
		proxmox.New("http://unused", "u@pam!t", "s", nil),
		ams,
		state.NewFileStore(""),
		metrics.New(true),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func alertCfg(am string, silences ...config.Silence) *config.Config {
	return &config.Config{
		DryRun: config.DryRunAlert,
		Alertmanager: config.Alertmanager{
			URLs:           []string{am},
			Comment:        "p1 down",
			UnsilenceGrace: config.Duration{Duration: 30 * time.Minute},
			Silences:       silences,
		},
	}
}

func sil(name, value string) config.Silence {
	return config.Silence{Matchers: []config.Matcher{{Name: name, Value: value, IsRegex: true}}}
}

// TestReconcileAlertOnlyLifecycle covers DryRunAlert: a silence set is created when p1 goes
// down, left untouched while it's healthy (no churn), grown when the config gains a silence,
// and grace-expired (not dropped outright) when p1 comes back.
func TestReconcileAlertOnlyLifecycle(t *testing.T) {
	am := newFakeAM()
	defer am.Close()
	cfg := alertCfg(am.URL(), sil("node", ".*-p1"), sil("instance", "pve-1(\\..*)?"))
	c := ctlWithAMs(cfg, am.URL())
	ctx := context.Background()

	// p1 down: one silence per configured entry, nothing deleted.
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: false})
	if cr, up, del := am.counts(); cr != 2 || up != 0 || del != 0 {
		t.Fatalf("after shutdown: creates=%d updates=%d deletes=%d, want 2/0/0", cr, up, del)
	}
	if am.activeOurs() != 2 {
		t.Fatalf("active silences = %d, want 2", am.activeOurs())
	}

	// Still down and healthy: no writes at all (this is the fix for per-tick churn).
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: false})
	if cr, up, del := am.counts(); cr != 2 || up != 0 || del != 0 {
		t.Errorf("healthy silences should not churn: creates=%d updates=%d deletes=%d, want 2/0/0", cr, up, del)
	}

	// Config gains a silence while still down: only the new one is created.
	cfg.Alertmanager.Silences = append(cfg.Alertmanager.Silences, sil("alertname", "GarageDown"))
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: false})
	if cr, up, del := am.counts(); cr != 3 || up != 0 || del != 0 {
		t.Errorf("added silence: creates=%d updates=%d deletes=%d, want 3/0/0", cr, up, del)
	}
	if am.activeOurs() != 3 {
		t.Errorf("active silences = %d, want 3", am.activeOurs())
	}

	// p1 back up: coverage isn't dropped outright but each silence is shortened to the grace
	// window (an in-place update), so it stays active for now and lapses on its own later.
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: true})
	if cr, up, del := am.counts(); cr != 3 || up != 3 || del != 0 {
		t.Errorf("after wake: creates=%d updates=%d deletes=%d, want 3/3/0 (grace-expired, not deleted)", cr, up, del)
	}
	if am.activeOurs() != 3 {
		t.Errorf("active silences after wake = %d, want 3 (still within grace)", am.activeOurs())
	}
}

// TestReconcileUnsilenceGrace: when p1 comes back up, a silence is shortened to the grace
// window and left to lapse rather than deleted; it isn't pushed out again on later up-ticks
// (which would keep it alive forever); and if p1 drops again inside the window it's extended
// back out to restore coverage.
func TestReconcileUnsilenceGrace(t *testing.T) {
	am := newFakeAM()
	defer am.Close()
	c := ctlWithAMs(alertCfg(am.URL(), sil("node", ".*-p1")), am.URL())
	ctx := context.Background()

	// p1 down: create the silence (ends ~24h out).
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: false})
	if cr, _, _ := am.counts(); cr != 1 {
		t.Fatalf("creates=%d, want 1", cr)
	}

	// p1 back up: shorten to the grace window (an update), don't delete.
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: true})
	if cr, up, del := am.counts(); cr != 1 || up != 1 || del != 0 {
		t.Fatalf("first wake: creates=%d updates=%d deletes=%d, want 1/1/0", cr, up, del)
	}
	if am.activeOurs() != 1 {
		t.Fatalf("silence should stay active within grace, activeOurs=%d", am.activeOurs())
	}

	// Still up: the silence now ends inside the grace window, so it's left to lapse - no further
	// write (the bug would re-shorten it every tick and it would never expire).
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: true})
	if cr, up, del := am.counts(); cr != 1 || up != 1 || del != 0 {
		t.Errorf("second wake churned the grace window: creates=%d updates=%d deletes=%d, want 1/1/0", cr, up, del)
	}

	// p1 drops again inside the window: the same silence is extended back out (it ends < the 1h
	// refresh window), restoring full coverage.
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: false})
	if cr, up, del := am.counts(); cr != 1 || up != 2 || del != 0 {
		t.Errorf("re-silence during grace: creates=%d updates=%d deletes=%d, want 1/2/0", cr, up, del)
	}
	if am.activeOurs() != 1 {
		t.Errorf("activeOurs=%d, want 1", am.activeOurs())
	}
}

// TestReconcileExtendsNearExpiry: an existing silence drifting toward expiry is extended in
// place (same id) rather than recreated.
func TestReconcileExtendsNearExpiry(t *testing.T) {
	am := newFakeAM()
	defer am.Close()
	cfg := alertCfg(am.URL(), sil("node", ".*-p1"))

	// Seed a matching silence that expires in 30m (< the 1h refresh window).
	id := am.seed("energy-watchdog", "p1 down",
		[]fakeMatcher{{Name: "node", Value: ".*-p1", IsRegex: true, IsEqual: true}},
		time.Now().Add(30*time.Minute))

	ctlWithAMs(cfg, am.URL()).reconcileAlertOnly(context.Background(), Snapshot{NodeUp: false})

	if cr, up, del := am.counts(); cr != 0 || up != 1 || del != 0 {
		t.Errorf("near-expiry: creates=%d updates=%d deletes=%d, want 0/1/0", cr, up, del)
	}
	if am.state(id) != "active" {
		t.Errorf("seeded silence %s state = %q, want active (extended)", id, am.state(id))
	}
	if am.activeOurs() != 1 {
		t.Errorf("active silences = %d, want 1 (no duplicate created)", am.activeOurs())
	}
}

// TestReconcileGCsDuplicatesAndDrift: duplicate copies of a desired silence and silences
// left over from a changed config are deleted, while a foreign silence is left alone.
func TestReconcileGCsDuplicatesAndDrift(t *testing.T) {
	am := newFakeAM()
	defer am.Close()
	cfg := alertCfg(am.URL(), sil("node", ".*-p1"))
	now := time.Now()

	nodeMatch := []fakeMatcher{{Name: "node", Value: ".*-p1", IsRegex: true, IsEqual: true}}
	keep := am.seed("energy-watchdog", "p1 down", nodeMatch, now.Add(24*time.Hour))
	dup := am.seed("energy-watchdog", "p1 down", nodeMatch, now.Add(20*time.Hour))
	drift := am.seed("energy-watchdog", "p1 down",
		[]fakeMatcher{{Name: "instance", Value: "old", IsRegex: true, IsEqual: true}}, now.Add(24*time.Hour))
	foreign := am.seed("jhofer", "manual", nodeMatch, now.Add(24*time.Hour))

	ctlWithAMs(cfg, am.URL()).reconcileAlertOnly(context.Background(), Snapshot{NodeUp: false})

	if cr, up, del := am.counts(); cr != 0 || up != 0 || del != 2 {
		t.Errorf("gc: creates=%d updates=%d deletes=%d, want 0/0/2", cr, up, del)
	}
	if am.state(keep) != "active" {
		t.Errorf("healthy silence should be kept, state=%q", am.state(keep))
	}
	if am.state(dup) != "expired" || am.state(drift) != "expired" {
		t.Errorf("dup=%q drift=%q, both want expired", am.state(dup), am.state(drift))
	}
	if am.state(foreign) != "active" {
		t.Errorf("foreign silence must be left alone, state=%q", am.state(foreign))
	}
}

// TestReconcilePartialFailureNoChurn is the regression for the amplifier bug: when one
// Alertmanager is unreachable, the healthy one is still reconciled and, crucially, is not
// recreated on the next tick just because its neighbour failed.
func TestReconcilePartialFailureNoChurn(t *testing.T) {
	good := newFakeAM()
	defer good.Close()
	// bad: List succeeds (empty), every create 503s - like the real down Alertmanager.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()

	cfg := alertCfg(good.URL(), sil("node", ".*-p1"))
	cfg.Alertmanager.URLs = []string{good.URL(), bad.URL}
	c := ctlWithAMs(cfg, good.URL(), bad.URL)
	ctx := context.Background()

	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: false})
	if cr, _, _ := good.counts(); cr != 1 {
		t.Fatalf("healthy AM creates = %d, want 1", cr)
	}

	// Next tick: the healthy AM already has its silence, so nothing is recreated there even
	// though the bad AM keeps failing.
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: false})
	if cr, up, del := good.counts(); cr != 1 || up != 0 || del != 0 {
		t.Errorf("healthy AM churned because neighbour failed: creates=%d updates=%d deletes=%d, want 1/0/0", cr, up, del)
	}
}
