package controller

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// The production failure of 17:36 on 19 Aug. p1 dropped out of corosync at 17:35:08
// ("members: 1/4773") while running perfectly well and serving a gaming session. Proxmox
// answers node state from the *other* nodes, so it reported pve-1 offline; the loop latched
// gaming -> shed on that, and the next tick's restate turned mode-shed-and-down into "off".
// nut-dog asserted the shed signal, p1's upsmon logged "forced shutdown in progress", and the
// session died - to a host that was never off and never in any trouble.
//
// nut-dog knew all along: its probe is a TCP check against pve-1's own Proxmox port, which
// does not care what the cluster thinks. Asking it is what settles this.
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

// With nobody to confirm it, one tick is not evidence. The cluster has to say it twice before
// the loop acts - which is cheap insurance, since the only thing lost is a minute.
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

// A probe that agrees p1 is down settles it on the spot: waiting a second tick would only
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

// A probe is only evidence while it is fresh. nut-dog polls every 15s, so a reading approaching
// probeMaxAge means its own loop is in trouble - and a stale "up" must not be allowed to hold
// p1 in limbo forever, any more than a stale "down" should settle an argument.
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

// The streak counts *consecutive* unconfirmed readings. A probe that reaches p1 is evidence
// against acting, so it has to clear the count - otherwise two unreachable-nut-dog ticks either
// side of a "p1 is up" add up to a reason to shed a host we were just told is alive.
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

// powerWish directly, because two rows of TestRestateNeverOverridesTheLoop now exit through
// the confirmDown hold path and never reach it. Off is a decision, never an inference: only a
// shed this loop asked for produces one.
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

// The hold can persist for as long as the partition does, and it means something quite
// different from a failing reconcile: p1 is most likely up and running unmanaged. It needs its
// own signal, or the only thing an operator sees is a stale-reconcile alert telling them to go
// read the pod logs.
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
