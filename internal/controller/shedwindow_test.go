package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/powerapi"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// lateIntent hides a wake request from the first intent read of a reconcile and reveals it to
// every later one. That is the production timeline: observe reads intent, Decide plans the
// power-off from it, and stopping the guests then runs for minutes - so the request a user
// files seconds into a shed is first seen by the re-check before the power-off.
type lateIntent struct {
	state.Store
	reads int
	req   state.WakeRequest
}

func (s *lateIntent) LoadIntent(ctx context.Context) (state.Intent, error) {
	i, err := s.Store.LoadIntent(ctx)
	if err != nil {
		return i, err
	}
	s.reads++
	if s.reads > 1 {
		i.Wake = append(i.Wake, s.req)
	}
	return i, nil
}

// hangingStop is delegateFixture's Proxmox fake with one change: the bulk stop task is never
// reported finished. That is the production shape - 211, 212, 202 and 201 took 5m51s to go
// down, and the loop sat in WaitTask for every second of it.
func hangingStop(t *testing.T) (*proxmox.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		switch p := r.URL.Path; {
		case p == "/api2/json/nodes":
			_, _ = w.Write([]byte(`{"data":[{"node":"pve-1","status":"online","uptime":100000}]}`))
		case p == "/api2/json/nodes/pve-1/qemu":
			_, _ = w.Write([]byte(`{"data":[{"vmid":301,"status":"running","name":"g"}]}`))
		case p == "/api2/json/nodes/pve-1/lxc", p == "/api2/json/cluster/replication":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.Contains(p, "/tasks/UPID:stopping/"):
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
		default: // stopall and the other task-returning calls
			_, _ = w.Write([]byte(`{"data":"UPID:stopping"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return proxmox.New(srv.URL, "u@pam!t", "s", nil), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

// The half of the production failure that survived the fix at the power-off. No power-off was
// ever sent - the re-check saw the request and left p1 up, exactly as designed - but it only
// got to run once the guests were down. p1 was up and usable for all 5m51s of that, and the
// user who pressed Start 4 seconds in watched "shedding" for the whole of it.
//
// The stop phase is the last thing between the plan and the power-off, so once a request lands
// there is nothing left worth waiting for: the guests carry on going down on Proxmox's side
// while the loop latches the mode and lets the queued nudge start the VM.
func TestARequestDuringTheStopPhaseCutsTheWaitShort(t *testing.T) {
	nut := &fakeNutDog{}
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeRunning, true)
	px, pxCalls := hangingStop(t)
	c.px = px
	c.wakePoll = 5 * time.Millisecond
	// Long enough that waiting the stop out cannot be mistaken for cutting it short.
	c.cfg.Proxmox.StopTimeout = config.Duration{Duration: 30 * time.Second}
	c.store = &lateIntent{Store: c.store,
		req: state.WakeRequest{VMID: 601, User: "josef", RequestedAt: time.Now().Unix()}}

	done := make(chan struct{})
	go func() {
		c.reconcile(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile is still waiting out the stop task; the request should have ended the wait")
	}

	if got := nut.requests(); len(got) != 0 {
		t.Errorf("power requests = %v, want none: p1 was still up and wanted", got)
	}
	if c.askedOff {
		t.Error("askedOff set: the power-off went out despite a live request")
	}
	// Cutting the wait short is not the same as skipping the shed: the load still has to have
	// been told to go, or a session during a deficit runs with everything else still up.
	if !slices.Contains(pxCalls(), "/api2/json/nodes/pve-1/stopall") {
		t.Error("no stopall was ever issued; the guests have to be shed, only the wait is skipped")
	}
	if c.pendingStop.upid == "" {
		t.Error("pendingStop is empty: the restore has no way to wait for the task we left running")
	}
	st, err := c.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeShed {
		t.Errorf("mode = %q, want shed so the next tick adopts the gaming session", st.Mode)
	}
}

// The budget has to travel with the task. stopAll allows it every guest's stopTimeout in turn,
// so a settle that invented a fresh single stopTimeout would give up while the task still had
// guests to go - and the restore would then read the very guest list mid-task that waiting was
// meant to avoid. Four guests at 10m, the production shape, is 40m of budget against 10m.
func TestTheAbandonedStopKeepsItsOwnBudget(t *testing.T) {
	nut := &fakeNutDog{}
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeRunning, true)
	c.px, _ = hangingStop(t)
	c.wakePoll = 5 * time.Millisecond
	c.cfg.Proxmox.StopTimeout = config.Duration{Duration: time.Minute}

	guests := []proxmox.Guest{
		{VMID: 301, Type: proxmox.TypeQEMU, Running: true},
		{VMID: 302, Type: proxmox.TypeQEMU, Running: true},
		{VMID: 303, Type: proxmox.TypeQEMU, Running: true},
	}
	if _, err := c.stopAll(context.Background(), guests, func() bool { return true }); !errors.Is(err, errStopInterrupted) {
		t.Fatalf("stopAll = %v, want the interrupt", err)
	}

	// Three guests at a minute each, minus the moment the call itself took.
	if got := time.Until(c.pendingStop.deadline); got < 2*time.Minute {
		t.Errorf("deadline is %s away, want ~3m: the task's own budget, not one stopTimeout", got)
	}
}

// Walking away from the stop task leaves it working through its guests, and good-morning can
// land while it still is. A guest it has not reached yet reads as running, a running guest is
// no part of the restore - and the task stops it moments later, so nothing starts it again
// until the next shed and restore, a day away. The restore has to let that task land first.
func TestTheRestoreWaitsForAnAbandonedStop(t *testing.T) {
	var mu sync.Mutex
	taskSettled, started := false, false
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch p := r.URL.Path; {
		case strings.Contains(p, "/tasks/UPID:stopping/"):
			taskSettled = true
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		case p == "/api2/json/nodes/pve-1/qemu":
			// 301 is the guest the abandoned task still had to reach: running until it lands.
			status := "running"
			if taskSettled {
				status = "stopped"
			}
			_, _ = w.Write([]byte(`{"data":[{"vmid":301,"status":"` + status + `","name":"g"}]}`))
		case p == "/api2/json/nodes/pve-1/lxc":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(p, "/startall"):
			started = true
			_, _ = w.Write([]byte(`{"data":"UPID:done"}`))
		case strings.Contains(p, "/tasks/"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":"UPID:done"}`))
		}
	}))
	defer px.Close()

	nut := &fakeNutDog{}
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "5000", srv.URL, state.ModeShed, true)
	c.px = proxmox.New(px.URL, "u@pam!t", "s", nil)
	c.pendingStop = stopTask{upid: "UPID:stopping", deadline: time.Now().Add(time.Minute)}

	if err := c.restoreStopped(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !started {
		t.Error("301 was left down: the guest list was read while the abandoned stop was still running")
	}
	if c.pendingStop.upid != "" {
		t.Errorf("pendingStop = %q, want cleared once it has been waited for", c.pendingStop.upid)
	}
}

// Good-morning starts the VM someone is waiting on before it restores the stop class. Behind
// the restore it waited out that bulk start task's up= delays, and any stop task we walked away
// from - minutes in which a request can pass its gamingGrace TTL, leaving a VM that then fails
// to start with no live request to retry it. The two sets are disjoint by config, so nothing
// about the start needs the restore to have happened first.
func TestTheRequestedVMStartsBeforeTheRestore(t *testing.T) {
	var mu sync.Mutex
	var order []string
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/api2/json/nodes":
			_, _ = w.Write([]byte(`{"data":[{"node":"pve-1","status":"online","uptime":100000}]}`))
		case p == "/api2/json/nodes/pve-1/qemu":
			// 301 is stop-class and down, so the restore wants it; 601 is the requested desktop VM.
			_, _ = w.Write([]byte(`{"data":[{"vmid":301,"status":"stopped"},{"vmid":601,"status":"stopped"}]}`))
		case p == "/api2/json/nodes/pve-1/lxc", p == "/api2/json/cluster/replication":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(p, "/startall"):
			mu.Lock()
			order = append(order, r.FormValue("vms"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":"UPID:done"}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		}
	}))
	defer px.Close()

	nut := &fakeNutDog{}
	srv := nut.server(t)
	defer srv.Close()

	// Surplus with a live request: good-morning restores the stop class and starts the VM.
	c, _ := delegateFixture(t, "5000", srv.URL, state.ModeGaming, true)
	c.px = proxmox.New(px.URL, "u@pam!t", "s", nil)
	if err := c.store.SaveIntent(context.Background(), state.Intent{
		Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: time.Now().Unix()}},
	}); err != nil {
		t.Fatal(err)
	}

	c.reconcile(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, []string{"601", "301"}) {
		t.Errorf("startall order = %v, want [601 301]: the VM someone is waiting on goes first", order)
	}
}

// The trap in interrupting the stop phase: a plan that already knows about the request stops
// the same guests and then starts the VM in the same run. Cutting that one short abandons its
// stop and returns before the start, putting the VM off until the next tick - slower, for a
// host that was never going down in the first place.
func TestAKnownRequestDoesNotAbandonItsOwnStop(t *testing.T) {
	nut := &fakeNutDog{}
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeRunning, true)
	c.px, _ = hangingStop(t)
	c.wakePoll = 5 * time.Millisecond
	c.cfg.Proxmox.StopTimeout = config.Duration{Duration: 30 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	// Live before the first read, so Decide plans the gaming shed itself: stop the load, keep
	// p1 up, start the VM. No power-off, and so nothing to interrupt.
	if err := c.store.SaveIntent(ctx, state.Intent{
		Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: time.Now().Unix()}},
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		c.reconcile(ctx)
		close(done)
	}()
	select {
	case <-done:
		cancel()
		t.Fatal("reconcile returned while the guests were still stopping: it abandoned its own stop")
	case <-time.After(200 * time.Millisecond): // still in the stop phase, as it should be
	}
	cancel()
	<-done
}

// The production failure of 14:40. A shed was planned, stopping the guests ran for 5m45s with
// the loop busy throughout, and the desktop VM asked for 3 seconds in was only read once the
// power-off had already been sent. It was then started on a host that was going down, died
// with it, and those few seconds of running marked the request satisfied - so the wake that
// followed had nothing to act on and p1 sat idle.
//
// p1 was up and usable for all of those 5m45s. Re-reading intent at the point of no return is
// what keeps that window usable: the shed is abandoned and the VM starts where it is.
func TestARequestDuringTheStopPhaseCancelsThePowerOff(t *testing.T) {
	nut := &fakeNutDog{}
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeRunning, true)
	c.store = &lateIntent{Store: c.store,
		req: state.WakeRequest{VMID: 601, User: "josef", RequestedAt: time.Now().Unix()}}

	c.reconcile(context.Background())

	if got := nut.requests(); len(got) != 0 {
		t.Errorf("power requests = %v, want none: p1 was still up and wanted", got)
	}
	if c.askedOff {
		t.Error("askedOff set: the power-off went out despite a live request")
	}
	st, err := c.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeShed {
		t.Errorf("mode = %q, want shed so the next tick adopts the gaming session", st.Mode)
	}
}

// A request that has already been seen through to a running VM is spent and must not keep
// rescuing p1 from every later shed - that would make the host unshedable for the rest of the
// request's TTL.
func TestASpentRequestDoesNotCancelThePowerOff(t *testing.T) {
	nut := &fakeNutDog{}
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeRunning, true)
	ctx := context.Background()
	at := time.Now().Unix()
	if err := c.store.SaveIntent(ctx, state.Intent{
		Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: at}},
	}); err != nil {
		t.Fatal(err)
	}
	st, err := c.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.WakeDone = map[int]int64{601: at}
	if err := c.store.Save(ctx, st); err != nil {
		t.Fatal(err)
	}

	c.reconcile(ctx)

	if got := nut.requests(); len(got) != 1 || got[0] != "off" {
		t.Errorf("power requests = %v, want [off]: the request was already spent", got)
	}
}

// Once the power-off has gone out, p1 is unusable however up it still looks - its upsmon has
// the signal and cannot be told to stop. Nothing may be planned on it until the shed lands:
// no gaming session to adopt, and above all no VM started, because a VM that runs for seconds
// and dies with the host marks the request satisfied and consumes it.
func TestNothingIsPlannedOnP1OnceTheShedIsSent(t *testing.T) {
	cfg := testCfg(t)
	snap := Snapshot{
		Surplus: -800, SoC: 90, NodeUp: true, Mode: state.ModeShed,
		NodeUptime: time.Hour, ManualShed: true, ShedInFlight: true,
		Wake: wants(601),
	}
	p := Decide(snap, cfg, testNow)

	if len(p.StartRequested) != 0 {
		t.Errorf("StartRequested = %v, want none on a host that is going down", p.StartRequested)
	}
	if p.Wake {
		t.Error("planned a wake for a host that has not finished going down")
	}
	if p.NextMode != state.ModeShed {
		t.Errorf("NextMode = %q, want shed: the session cannot start until p1 is back", p.NextMode)
	}
	// The restate has to keep saying off, or it releases a signal p1 has already acted on.
	if got := powerWish(p, snap, true); got != "off" {
		t.Errorf("powerWish = %q, want off while our own shed is in flight", got)
	}
}

// askedOff is answered before the plan because it is a fact about p1 rather than a wish: we
// have already told nut-dog to shut it down. The reachable case is a power-off whose request
// failed - askedOff is set but the mode was never advanced, so the next tick still sees
// running and can plan a session on a host we have committed to shedding.
func TestAskedOffOutranksThePlan(t *testing.T) {
	p := Plan{NextMode: state.ModeGaming}
	snap := Snapshot{Mode: state.ModeRunning, NodeUp: true}

	if got := powerWish(p, snap, true); got != powerapi.Off {
		t.Errorf("powerWish = %q, want off: the shed had already been asked for", got)
	}
	if got := powerWish(p, snap, false); got != powerapi.On {
		t.Errorf("powerWish = %q, want on with no shed in flight", got)
	}
}

// ...and the moment p1 is observed down the request is acted on: it was never started, so it
// was never marked spent, and the wake is the one that finally runs it.
func TestTheRequestSurvivesTheShedAndWakesP1(t *testing.T) {
	cfg := testCfg(t)
	p := Decide(Snapshot{
		Surplus: -800, SoC: 90, NodeUp: false, Mode: state.ModeShed,
		ManualShed: true, Wake: wants(601),
	}, cfg, testNow)

	if !p.Wake {
		t.Error("p1 is down with a live request and no wake was planned")
	}
	if len(p.StartRequested) != 1 || p.StartRequested[0] != 601 {
		t.Errorf("StartRequested = %v, want [601]", p.StartRequested)
	}
	if p.NextMode != state.ModeGaming {
		t.Errorf("NextMode = %q, want gaming", p.NextMode)
	}
}
