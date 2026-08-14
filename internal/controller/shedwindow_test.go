package controller

import (
	"context"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/powerapi"
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
