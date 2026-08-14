package controller

import (
	"bytes"
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

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
	"gopkg.in/yaml.v3"
)

// startFake is the fake Proxmox's guest 601. exit is what the startall task reports, and
// comesUp is whether a start actually brings the guest up - the two are separate so a task
// that reports OK over a guest that stayed down can be exercised.
type startFake struct {
	mu      sync.Mutex
	exit    string
	comesUp bool
	running bool
}

func (f *startFake) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// startEnv stands up a fake Proxmox with guest 601 stopped and a task that succeeds.
func startEnv(t *testing.T) (c *Controller, f *startFake) {
	t.Helper()
	f = &startFake{exit: "OK", comesUp: true}
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(p, "/startall"):
			if f.comesUp {
				f.running = true
			}
			_, _ = w.Write([]byte(`{"data":"UPID:startall"}`))
		case strings.HasSuffix(p, "/nodes/pve-1/qemu"):
			status := "stopped"
			if f.running {
				status = "running"
			}
			_, _ = w.Write([]byte(`{"data":[{"vmid":601,"status":"` + status + `"}]}`))
		case strings.HasSuffix(p, "/nodes/pve-1/lxc"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.Contains(p, "/tasks/"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"` + f.exit + `"}}`))
		default:
			t.Errorf("unexpected proxmox path %s", p)
		}
	}))
	t.Cleanup(px.Close)

	var g config.Guests
	if err := yaml.Unmarshal([]byte("gamingGuard: [\"600-699\"]\n"), &g); err != nil {
		t.Fatalf("unmarshal guests: %v", err)
	}
	c = New(&config.Config{
		DryRun:      config.DryRunFull,
		Guests:      g,
		GamingGrace: config.Duration{Duration: 10 * time.Minute},
		Proxmox:     config.Proxmox{Node: "pve-1"},
	},
		prom.New("http://unused"),
		proxmox.New(px.URL, "u@pam!t", "s", nil),
		nil,
		state.NewFileStore(filepath.Join(t.TempDir(), "state.json")),
		metrics.New(false),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return c, f
}

// tryStart runs one reconcile's worth of the start path for a request made at reqAt.
func tryStart(c *Controller, reqAt int64) {
	_ = c.startRequested(context.Background(), []int{601}, []state.WakeRequest{{VMID: 601, RequestedAt: reqAt}})
}

// live reports whether the loop would still act on the request - what Decide sees.
func live(c *Controller, reqAt int64) bool {
	got := c.requestedWake(state.Intent{Wake: []state.WakeRequest{{VMID: 601, RequestedAt: reqAt}}}, time.Now())
	return len(got) > 0
}

// A VM that will not boot must be given up on. Retrying it every tick for the rest of the
// request's TTL holds p1 up for a session that is never going to happen, and the only trace
// of the failure is a line in the pod log.
func TestUnstartableVMIsRetired(t *testing.T) {
	c, f := startEnv(t)
	f.set(func() { f.exit = "timeout waiting on systemd"; f.comesUp = false })
	at := time.Now().Unix()

	for range startFailLimit {
		if !live(c, at) {
			t.Fatal("request retired before it had its tries")
		}
		tryStart(c, at)
	}

	if live(c, at) {
		t.Errorf("request still live after %d failed starts", startFailLimit)
	}
	got := c.StartFailures()[601]
	if !strings.Contains(got.Err, "timeout waiting on systemd") {
		t.Errorf("StartFailures[601].Err = %q, want the task error in it", got.Err)
	}
	// The failure has to name the request it belongs to, or a caller cannot tell it apart
	// from one the user has since pressed Start past.
	if got.RequestedAt != at {
		t.Errorf("StartFailures[601].RequestedAt = %d, want %d", got.RequestedAt, at)
	}
}

// Pressing Start again is a new ask, and gets its own tries. Without this the VM is
// unstartable until the pod restarts, however the underlying problem is fixed.
func TestANewRequestReArmsARetiredVM(t *testing.T) {
	c, f := startEnv(t)
	f.set(func() { f.exit = "timeout waiting on systemd"; f.comesUp = false })
	at := time.Now().Unix()
	for range startFailLimit {
		tryStart(c, at)
	}
	if live(c, at) {
		t.Fatal("fixture: request should be retired")
	}

	again := at + 1
	if !live(c, again) {
		t.Error("a fresh request inherited the old one's failures")
	}
	// The stale record survives until the new request has been tried, so what it is tagged
	// with is load-bearing: reported against the new request it would paint "Couldn't start"
	// over a wake that is actually in progress.
	if got := c.StartFailures()[601].RequestedAt; got != at {
		t.Errorf("failure RequestedAt = %d, want the old request %d", got, at)
	}
}

// Failures have to be consecutive: a start that works clears the count, so an occasional
// flake never accumulates into a retirement.
func TestASuccessfulStartClearsTheCount(t *testing.T) {
	c, f := startEnv(t)
	at := time.Now().Unix()

	f.set(func() { f.exit = "timeout waiting on systemd"; f.comesUp = false })
	tryStart(c, at)
	tryStart(c, at)
	f.set(func() { f.exit = "OK"; f.comesUp = true })
	tryStart(c, at) // this one takes: the node reports 601 running

	// The user shuts it down again and the next start fails, so the count starts from one.
	f.set(func() { f.running = false; f.exit = "timeout waiting on systemd"; f.comesUp = false })
	tryStart(c, at)
	if !live(c, at) {
		t.Error("request retired on the first failure after a success")
	}
	if got := c.StartFailures(); len(got) != 0 {
		t.Errorf("StartFailures = %v, want empty below the limit", got)
	}
}

// A bulk start reports one status for the whole batch, so it can come back OK over a guest
// that stayed down. Trusting the task there is what left a VM being started every tick: if
// three attempts have not produced a running guest, a fourth will not either.
func TestATaskReportingOKOverADownGuestStillCounts(t *testing.T) {
	c, f := startEnv(t)
	f.set(func() { f.comesUp = false }) // the task says OK; 601 never comes up
	at := time.Now().Unix()

	for range startFailLimit {
		tryStart(c, at)
	}

	if live(c, at) {
		t.Errorf("request still live after %d starts that reported OK and did nothing", startFailLimit)
	}
	if got := c.StartFailures()[601].Err; got != "the VM did not come up" {
		t.Errorf("StartFailures[601].Err = %q, want it to say the guest stayed down", got)
	}
}

// The flip side: a start whose task reports OK and does bring the guest up must not count,
// or a VM that works every time still retires after three sessions.
func TestAStartThatWorksIsNotCountedAgainstTheRequest(t *testing.T) {
	c, f := startEnv(t)
	at := time.Now().Unix()

	for range startFailLimit + 2 {
		tryStart(c, at)
		f.set(func() { f.running = false }) // the user shuts it down between sessions
	}

	if !live(c, at) {
		t.Error("request retired despite every start working")
	}
	if got := c.StartFailures(); len(got) != 0 {
		t.Errorf("StartFailures = %v, want empty", got)
	}
}

// The start that finally works must not be counted or announced as a give-up on its way
// through. Counting first and undoing after left an ERROR line contradicted by the INFO on
// the next line, and a window - a Proxmox round-trip wide - where the API reported the VM
// as retired while it was in fact starting.
func TestTheStartThatWorksIsNeverReportedAsAGiveUp(t *testing.T) {
	c, f := startEnv(t)
	var buf bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	at := time.Now().Unix()

	f.set(func() { f.exit = "timeout waiting on systemd"; f.comesUp = false })
	for range startFailLimit - 1 {
		tryStart(c, at)
	}
	f.set(func() { f.exit = "OK"; f.comesUp = true })
	tryStart(c, at)

	if buf.Len() != 0 {
		t.Errorf("a successful start logged an error: %s", buf.String())
	}
	if !live(c, at) {
		t.Error("request retired by the start that worked")
	}
	if got := c.StartFailures(); len(got) != 0 {
		t.Errorf("StartFailures = %v, want empty after a start that took", got)
	}
}

// The API nudges a reconcile on every button press, by anyone, so tries arrive far faster
// than the interval. Burning the budget on them retires a request seconds after its first
// failure - and a transient Proxmox condition that one real tick would have cleared instead
// takes p1 down.
func TestNudgesDoNotBurnTheTryBudget(t *testing.T) {
	c, f := startEnv(t)
	c.cfg.Interval = config.Duration{Duration: time.Minute}
	f.set(func() { f.exit = "timeout waiting on systemd"; f.comesUp = false })
	at := time.Now().Unix()

	for range startFailLimit * 3 {
		tryStart(c, at) // a burst of nudged reconciles, all inside one interval
	}

	if !live(c, at) {
		t.Errorf("request retired after %d tries inside a single interval", startFailLimit*3)
	}
	if got := c.StartFailures(); len(got) != 0 {
		t.Errorf("StartFailures = %v, want empty: only one try counts per interval", got)
	}
}

// A guest that isn't on the node at all is the same dead end and must retire too: the loop
// cannot start it, and nothing about retrying changes that.
func TestAMissingGuestIsRetired(t *testing.T) {
	c, _ := startEnv(t)
	at := time.Now().Unix()
	want := []state.WakeRequest{{VMID: 699, RequestedAt: at}}
	for range startFailLimit {
		_ = c.startRequested(context.Background(), []int{699}, want)
	}
	if got := c.requestedWake(state.Intent{Wake: want}, time.Now()); len(got) != 0 {
		t.Errorf("request still live after %d tries: %+v", startFailLimit, got)
	}
	if got := c.StartFailures()[699].Err; !strings.Contains(got, "not on pve-1") {
		t.Errorf("StartFailures[699].Err = %q, want it to name the node", got)
	}
}
