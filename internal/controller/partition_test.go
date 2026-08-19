package controller

import (
	"context"
	"testing"

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
