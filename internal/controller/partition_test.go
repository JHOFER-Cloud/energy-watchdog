package controller

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// Regression for the 19 Aug shed: p1 left corosync while running, Proxmox reported pve-1
// offline, the loop latched gaming -> shed, and the next tick's restate sent off. Two ticks,
// because the second is the one that used to send it.
func TestAPartitionedP1IsNeverShed(t *testing.T) {
	nut := &fakeNutDog{actual: "up"} // the probe reaches p1 directly: still running
	srv := nut.server(t)
	defer srv.Close()

	// Deficit, held off by hand, mid-session - and the cluster reporting p1 offline.
	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeGaming, false)
	ctx := context.Background()

	c.reconcile(ctx)
	c.reconcile(ctx) // the second tick is the one that used to send "off"

	for _, got := range nut.requests() {
		if got == "off" {
			t.Fatalf("power requests = %v: a host we merely cannot see must never be shed", nut.requests())
		}
	}
	st, err := c.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeGaming {
		t.Errorf("mode = %q, want gaming: the session is still running, we just lost sight of it", st.Mode)
	}
}

// With no usable probe the reading must repeat nodeDownConfirm times before the mode latches.
func TestAnUnconfirmedDownWaitsForASecondTick(t *testing.T) {
	nut := &fakeNutDog{} // no opinion
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeGaming, false)
	ctx := context.Background()

	c.reconcile(ctx)
	st, err := c.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeGaming {
		t.Fatalf("mode = %q after one unconfirmed reading, want gaming", st.Mode)
	}

	c.reconcile(ctx)
	if st, err = c.store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeShed {
		t.Errorf("mode = %q after the reading repeated, want shed: p1 really is gone", st.Mode)
	}
}

// A probe that corroborates the offline reading latches on the first tick; waiting would only
// delay the shed bookkeeping for a host that is genuinely off.
func TestAProbedDownIsBelievedAtOnce(t *testing.T) {
	nut := &fakeNutDog{actual: "down"}
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeGaming, false)
	ctx := context.Background()

	c.reconcile(ctx)

	st, err := c.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeShed {
		t.Errorf("mode = %q, want shed: nut-dog probed p1 as down too", st.Mode)
	}
	// Believed, but still never told off by inference - only a decision does that.
	for _, got := range nut.requests() {
		if got == "off" {
			t.Errorf("power requests = %v, want no off: this loop never asked for a shed", nut.requests())
		}
	}
}

// A probe older than probeMaxAge is not corroboration in either direction: a stale "up" must
// not hold p1 indefinitely, and a stale "down" must not shortcut the streak.
func TestAStaleProbeSettlesNothing(t *testing.T) {
	nut := &fakeNutDog{actual: "up", ageSec: 3600} // its loop stopped an hour ago
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeGaming, false)
	ctx := context.Background()

	c.reconcile(ctx)
	st, err := c.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeGaming {
		t.Fatalf("mode = %q after one tick, want gaming: a stale probe is no reason to act either", st.Mode)
	}

	// ...it falls through to the two-tick rule rather than deciding anything.
	c.reconcile(ctx)
	if st, err = c.store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeShed {
		t.Errorf("mode = %q, want shed: with no usable probe the repeated reading stands", st.Mode)
	}
}

// The streak counts consecutive readings only. A successful probe clears it, so uncorroborated
// ticks either side of a "p1 is up" must not accumulate into a reason to act.
func TestAProbeThatReachesP1ClearsTheStreak(t *testing.T) {
	nut := &fakeNutDog{} // starts with no opinion
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeGaming, false)
	ctx := context.Background()

	c.reconcile(ctx) // unconfirmed: streak 1

	nut.mu.Lock()
	nut.actual = "up" // nut-dog gets its answer through: p1 is alive
	nut.mu.Unlock()
	c.reconcile(ctx)

	nut.mu.Lock()
	nut.actual = "" // and goes quiet again
	nut.mu.Unlock()
	c.reconcile(ctx)

	st, err := c.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeGaming {
		t.Errorf("mode = %q, want gaming: the two unconfirmed ticks were not consecutive", st.Mode)
	}
}

// powerWish directly: two rows of TestRestateNeverOverridesTheLoop now exit through the
// confirmDown hold path and no longer reach it. Only askedOff yields off.
func TestPowerWishOnlyEverInfersHold(t *testing.T) {
	tests := []struct {
		name     string
		plan     Plan
		snap     Snapshot
		askedOff bool
		want     string
	}{
		{"our shed, p1 still going down", Plan{NextMode: state.ModeShed},
			Snapshot{Mode: state.ModeShed, NodeUp: true}, true, "off"},
		{"p1 down in shed: not ours to assert", Plan{NextMode: state.ModeShed},
			Snapshot{Mode: state.ModeShed, NodeUp: false}, false, "hold"},
		{"p1 down while running: nothing decided yet", Plan{NextMode: state.ModeShed},
			Snapshot{Mode: state.ModeRunning, NodeUp: false}, false, "hold"},
		{"p1 up and wanted", Plan{NextMode: state.ModeRunning},
			Snapshot{Mode: state.ModeRunning, NodeUp: true}, false, "on"},
		{"p1 up during a shed: started by hand", Plan{NextMode: state.ModeShed},
			Snapshot{Mode: state.ModeShed, NodeUp: true}, false, "hold"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := powerWish(tt.plan, tt.snap, tt.askedOff); got != tt.want {
				t.Errorf("powerWish = %q, want %q", got, tt.want)
			}
		})
	}
}

// The hold is distinct from a failing reconcile - p1 is likely up and unmanaged - so it needs
// its own gauge rather than only surfacing as a stale reconcile.
func TestTheUnconfirmedHoldIsVisible(t *testing.T) {
	nut := &fakeNutDog{actual: "up"}
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeGaming, false)
	m := metrics.New(false)
	c.metrics = m

	c.reconcile(context.Background())

	rr := httptest.NewRecorder()
	m.Handler()(rr, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rr.Body.String(), "energy_watchdog_node_unconfirmed 1") {
		t.Errorf("holding on an unconfirmed reading left no signal:\n%s", rr.Body.String())
	}
}

// The gauge must not outlive the state it describes. Its alert asserts the loop is otherwise
// healthy, so a value latched across blind ticks would misreport a failing reconcile.
func TestTheUnconfirmedSignalClearsWhenTheLoopGoesBlind(t *testing.T) {
	nut := &fakeNutDog{actual: "up"}
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeGaming, false)
	m := metrics.New(false)
	c.metrics = m
	ctx := context.Background()

	c.reconcile(ctx) // holds: p1 reads offline, nut-dog says it is up
	rr := httptest.NewRecorder()
	m.Handler()(rr, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rr.Body.String(), "energy_watchdog_node_unconfirmed 1") {
		t.Fatalf("expected the hold to be signalled:\n%s", rr.Body.String())
	}

	dead := httptest.NewServer(nil)
	dead.Close()
	c.prom = prom.New(dead.URL) // now we cannot observe at all
	c.reconcile(ctx)

	rr = httptest.NewRecorder()
	m.Handler()(rr, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rr.Body.String(), "energy_watchdog_node_unconfirmed 0") {
		t.Errorf("the hold signal survived a blind tick; it now claims a healthy loop:\n%s", rr.Body.String())
	}
}

// An unrecognised probe state must behave as no opinion: neither latching on the first tick
// like "down", nor holding indefinitely like "up".
func TestAnUnrecognisedProbeStateFallsBackRatherThanDeciding(t *testing.T) {
	nut := &fakeNutDog{actual: "powered-on"} // a word this side does not know
	srv := nut.server(t)
	defer srv.Close()

	c, _ := delegateFixture(t, "-800", srv.URL, state.ModeGaming, false)
	ctx := context.Background()

	// Not "down": one tick must not be enough to act on.
	c.reconcile(ctx)
	st, err := c.store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeGaming {
		t.Fatalf("mode = %q after one tick, want gaming: an unreadable answer is not a reading", st.Mode)
	}

	// ...and not "up", which would hold p1 for as long as the drift lasted.
	c.reconcile(ctx)
	if st, err = c.store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if st.Mode != state.ModeShed {
		t.Errorf("mode = %q, want shed: with no usable probe the repeated reading stands", st.Mode)
	}
}
