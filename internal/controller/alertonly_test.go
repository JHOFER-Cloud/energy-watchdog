package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// TestReconcileAlertOnly covers DryRunAlert: silences track p1's real power state,
// refresh before the TTL lapses, and clear on wake - with no Proxmox actions.
func TestReconcileAlertOnly(t *testing.T) {
	var (
		mu      sync.Mutex
		creates int
		deletes int
	)
	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			creates++
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"silenceID":"sil-1"}`))
		case http.MethodDelete:
			deletes++
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer am.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	store := state.NewFileStore(statePath)
	cfg := &config.Config{
		DryRun: config.DryRunAlert,
		Alertmanager: config.Alertmanager{
			URLs:     []string{am.URL},
			Silences: []config.Silence{{Matchers: []config.Matcher{{Name: "node", Value: ".*-p1", IsRegex: true}}}},
		},
	}
	c := New(cfg,
		prom.New("http://unused"),
		proxmox.New(am.URL, "u@pam!t", "s", nil),
		map[string]*alertmgr.Client{am.URL: alertmgr.New(am.URL, nil)},
		store,
		metrics.New(true),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	ctx := context.Background()
	count := func() (int, int) { mu.Lock(); defer mu.Unlock(); return creates, deletes }

	// p1 goes down: a silence is created and recorded.
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: false})
	if st, _ := store.Load(ctx); len(st.Silences) != 1 || st.SilencedAt == 0 {
		t.Fatalf("after shutdown: silences=%+v silencedAt=%d", st.Silences, st.SilencedAt)
	}
	if cr, del := count(); cr != 1 || del != 0 {
		t.Fatalf("after shutdown: creates=%d deletes=%d, want 1/0", cr, del)
	}

	// Still down and fresh: no churn.
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: false})
	if cr, del := count(); cr != 1 || del != 0 {
		t.Errorf("fresh silence should not refresh: creates=%d deletes=%d", cr, del)
	}

	// Force the silence to look near-expiry: it should refresh (new create, retire old).
	st, _ := store.Load(ctx)
	st.SilencedAt = time.Now().Add(-silenceTTL).Unix()
	if err := store.Save(ctx, st); err != nil {
		t.Fatal(err)
	}
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: false})
	if cr, del := count(); cr != 2 || del != 1 {
		t.Errorf("stale silence should refresh: creates=%d deletes=%d, want 2/1", cr, del)
	}

	// p1 comes back: silence removed, state cleared.
	c.reconcileAlertOnly(ctx, Snapshot{NodeUp: true})
	if st, _ := store.Load(ctx); len(st.Silences) != 0 || st.SilencedAt != 0 {
		t.Errorf("after wake: silences=%+v silencedAt=%d", st.Silences, st.SilencedAt)
	}
	if cr, del := count(); cr != 2 || del != 2 {
		t.Errorf("after wake: creates=%d deletes=%d, want 2/2", cr, del)
	}
}
