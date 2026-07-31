package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/alertmgr"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// fakePVE serves /cluster/replication and applies PUTs to an in-memory job set, so a test can
// assert both the resulting config and how many writes it took to get there.
type fakePVE struct {
	mu    sync.Mutex
	jobs  map[string]*proxmox.ReplicationJob
	order []string
	gets  int
	puts  int
	srv   *httptest.Server
}

func newFakePVE(t *testing.T, jobs ...proxmox.ReplicationJob) *fakePVE {
	t.Helper()
	f := &fakePVE{jobs: map[string]*proxmox.ReplicationJob{}}
	for _, j := range jobs {
		cp := j
		f.jobs[j.ID] = &cp
		f.order = append(f.order, j.ID)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

const replBase = "/api2/json/cluster/replication"

func (f *fakePVE) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == replBase:
		f.gets++
		// Mirror Proxmox: absent keys rather than zero values for an enabled, uncommented job.
		out := make([]map[string]any, 0, len(f.order))
		for _, id := range f.order {
			j := f.jobs[id]
			m := map[string]any{"id": j.ID, "source": j.Source, "target": j.Target}
			if j.Comment != "" {
				m["comment"] = j.Comment
			}
			if j.Disabled {
				m["disable"] = 1
			}
			out = append(out, m)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})

	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, replBase+"/"):
		j, ok := f.jobs[strings.TrimPrefix(r.URL.Path, replBase+"/")]
		if !ok {
			http.Error(w, "no such replication job", http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.puts++
		j.Disabled = r.Form.Get("disable") == "1"
		if r.Form.Get("delete") == "comment" {
			j.Comment = ""
		} else if c := r.Form.Get("comment"); c != "" {
			j.Comment = c
		}
		_, _ = w.Write([]byte(`{"data":null}`))

	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
	}
}

func (f *fakePVE) counts() (gets, puts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.puts
}

func (f *fakePVE) job(t *testing.T, id string) proxmox.ReplicationJob {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		t.Fatalf("no job %s", id)
	}
	return *j
}

func (f *fakePVE) assertJob(t *testing.T, id string, disabled bool, comment string) {
	t.Helper()
	j := f.job(t, id)
	if j.Disabled != disabled || j.Comment != comment {
		t.Errorf("job %s = {disabled:%v comment:%q}, want {disabled:%v comment:%q}",
			id, j.Disabled, j.Comment, disabled, comment)
	}
}

func replCtl(t *testing.T, f *fakePVE, cfg config.Proxmox) *Controller {
	t.Helper()
	return New(
		&config.Config{Proxmox: cfg},
		prom.New("http://unused"),
		proxmox.New(f.srv.URL, "u@pam!t", "s", nil),
		map[string]*alertmgr.Client{},
		state.NewFileStore(filepath.Join(t.TempDir(), "state.json")),
		metrics.New(true),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

// replFixture is the cluster the lifecycle tests run against: two live jobs replicating into
// p1, one already disabled by hand, and two that must never be touched (one sourced from p1,
// one between other nodes entirely).
func replFixture(t *testing.T) *fakePVE {
	t.Helper()
	return newFakePVE(t,
		proxmox.ReplicationJob{ID: "104-0", Source: "pve-2", Target: "pve-1", Comment: "nightly"},
		proxmox.ReplicationJob{ID: "107-0", Source: "pve-3", Target: "pve-1"},
		proxmox.ReplicationJob{ID: "108-0", Source: "pve-3", Target: "pve-1", Comment: "broken disk", Disabled: true},
		proxmox.ReplicationJob{ID: "201-0", Source: "pve-1", Target: "pve-2"},
		proxmox.ReplicationJob{ID: "202-0", Source: "pve-3", Target: "pve-2"},
	)
}

// TestReconcileReplicationLifecycle is the JHC-538 core: jobs replicating into p1 are
// disabled while it is down and restored when it is back, a job disabled by hand is left
// alone in both directions, jobs targeting other nodes are never touched, and a steady-state
// tick writes nothing.
func TestReconcileReplicationLifecycle(t *testing.T) {
	f := replFixture(t)
	c := replCtl(t, f, config.Proxmox{Node: "pve-1"})
	ctx := context.Background()

	// p1 down: claim and disable the two live jobs into p1, preserving their comments behind
	// the marker. The hand-disabled job is not claimed, so it keeps its own comment.
	if err := c.reconcileReplication(ctx, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, puts := f.counts(); puts != 2 {
		t.Fatalf("disable puts = %d, want 2", puts)
	}
	f.assertJob(t, "104-0", true, "[energy-watchdog] nightly")
	f.assertJob(t, "107-0", true, "[energy-watchdog]")
	f.assertJob(t, "108-0", true, "broken disk")
	f.assertJob(t, "201-0", false, "")
	f.assertJob(t, "202-0", false, "")

	// Still down: everything already where we want it, so no further writes.
	if err := c.reconcileReplication(ctx, true); err != nil {
		t.Fatalf("disable again: %v", err)
	}
	if _, puts := f.counts(); puts != 2 {
		t.Errorf("steady-state disable churned: puts = %d, want 2", puts)
	}

	// p1 back up: only our two marked jobs are re-enabled, with their original comments
	// restored verbatim. The hand-disabled job stays disabled - that is the whole point of
	// the marker.
	if err := c.reconcileReplication(ctx, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, puts := f.counts(); puts != 4 {
		t.Fatalf("enable puts = %d, want 4", puts)
	}
	f.assertJob(t, "104-0", false, "nightly")
	f.assertJob(t, "107-0", false, "")
	f.assertJob(t, "108-0", true, "broken disk")
	f.assertJob(t, "201-0", false, "")
	f.assertJob(t, "202-0", false, "")

	// Still up: nothing left to do.
	if err := c.reconcileReplication(ctx, false); err != nil {
		t.Fatalf("enable again: %v", err)
	}
	if _, puts := f.counts(); puts != 4 {
		t.Errorf("steady-state enable churned: puts = %d, want 4", puts)
	}
}

// TestReconcileReplicationReDisablesOurJob: a marked job re-enabled by hand while p1 is still
// down is put back, without double-marking its comment.
func TestReconcileReplicationReDisablesOurJob(t *testing.T) {
	f := newFakePVE(t,
		proxmox.ReplicationJob{ID: "104-0", Source: "pve-2", Target: "pve-1",
			Comment: "[energy-watchdog] nightly", Disabled: false},
	)
	c := replCtl(t, f, config.Proxmox{Node: "pve-1"})

	if err := c.reconcileReplication(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	f.assertJob(t, "104-0", true, "[energy-watchdog] nightly")
}

// TestReconcileReplicationDisabledByConfig: manageReplication: false takes the feature fully
// out of the loop - not even the list call is made.
func TestReconcileReplicationDisabledByConfig(t *testing.T) {
	f := replFixture(t)
	off := false
	c := replCtl(t, f, config.Proxmox{Node: "pve-1", ManageReplication: &off})

	if err := c.reconcileReplication(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if gets, puts := f.counts(); gets != 0 || puts != 0 {
		t.Errorf("gets=%d puts=%d, want 0/0 when manageReplication is false", gets, puts)
	}
}

// TestReconcileReplicationDryRunLog: log mode previews the change without writing it.
func TestReconcileReplicationDryRunLog(t *testing.T) {
	f := replFixture(t)
	c := replCtl(t, f, config.Proxmox{Node: "pve-1"})
	c.cfg.DryRun = config.DryRunLog

	if err := c.reconcileReplication(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if gets, puts := f.counts(); gets != 1 || puts != 0 {
		t.Errorf("gets=%d puts=%d, want 1/0 (log mode reads but must not write)", gets, puts)
	}
	f.assertJob(t, "104-0", false, "nightly")
}

func TestMarkUnmarkComment(t *testing.T) {
	for _, tc := range []struct {
		orig, marked string
	}{
		{"", "[energy-watchdog]"},
		{"nightly", "[energy-watchdog] nightly"},
		{"[not ours]", "[energy-watchdog] [not ours]"},
	} {
		if got := markComment(tc.orig); got != tc.marked {
			t.Errorf("markComment(%q) = %q, want %q", tc.orig, got, tc.marked)
		}
		got, ours := unmarkComment(tc.marked)
		if !ours || got != tc.orig {
			t.Errorf("unmarkComment(%q) = (%q,%v), want (%q,true)", tc.marked, got, ours, tc.orig)
		}
	}
	for _, unmarked := range []string{"", "broken disk", "energy-watchdog", "x [energy-watchdog]"} {
		if got, ours := unmarkComment(unmarked); ours {
			t.Errorf("unmarkComment(%q) = (%q,true), want not ours", unmarked, got)
		}
	}
}
