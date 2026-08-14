// Package controller runs the reconcile loop: observe solar + Proxmox, Decide a Plan,
// then apply it. Full mode executes the physical actions and persists the new state; the
// dry-run modes persist the decision (so the mode still latches) but take no physical action,
// which makes a dry run a faithful preview of what live would decide.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/alertmgr"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/powerapi"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// Controller wires the clients and persisted state together.
type Controller struct {
	cfg  *config.Config
	prom *prom.Client
	// power delegates p1's on/off to nut-dog. nil means this controller powers p1
	// itself over Proxmox.
	power   *powerapi.Client
	px      *proxmox.Client
	ams     map[string]*alertmgr.Client
	store   state.Store
	metrics *metrics.Metrics
	log     *slog.Logger

	// askedOff records that this loop requested the power-off and p1 is still on its way
	// down, so the restate keeps re-asserting it. Cleared once p1 is observed down or woken.
	// Not persisted: a restart inside that window reads false and re-opens the same race -
	// mode stays shed with p1 up, and ModeShed plans nothing more, so it idles powered-up.
	askedOff bool

	observeFailures int  // consecutive failed observes, for log-level escalation
	warnedNoUptime  bool // the missing-uptime warning is logged once, not every tick

	nudge chan struct{} // out-of-band reconcile requests from the self-service API

	// activity is what an in-flight apply is doing. Proxmox reports the node online for the
	// whole shed, so the UI can't tell "starting your VM" from "host on its way down" without it.
	activity atomic.Value
}

// Activity is what the loop is doing now, "" when idle.
func (c *Controller) Activity() string {
	s, _ := c.activity.Load().(string)
	return s
}

const (
	ActivityShedding = "shedding"
	ActivityWaking   = "waking"
)

func (c *Controller) setActivity(a string) { c.activity.Store(a) }

// Nudge asks for a reconcile now instead of at the next tick, coalescing with any request
// already pending. The API calls it after writing intent so a button press feels immediate
// without the API ever touching p1 itself.
func (c *Controller) Nudge() {
	select {
	case c.nudge <- struct{}{}:
	default: // one already queued; a second changes nothing
	}
}

// observeFailEscalate is how many back-to-back failed observes are tolerated at warn before
// the failure is logged at error: a transient blip stays quiet, a sustained outage gets loud.
const observeFailEscalate = 5

// New builds a Controller. ams is keyed by Alertmanager base URL so a persisted
// silence can be deleted from the same Alertmanager it was created in.
func New(cfg *config.Config, p *prom.Client, px *proxmox.Client, ams map[string]*alertmgr.Client, store state.Store, m *metrics.Metrics, log *slog.Logger) *Controller {
	c := &Controller{cfg: cfg, prom: p, px: px, ams: ams, store: store, metrics: m, log: log,
		nudge: make(chan struct{}, 1)}
	if cfg.PowerAPI != nil {
		c.power = powerapi.New(cfg.PowerAPI.URL, cfg.PowerAPI.Token, cfg.PowerAPI.Load)
	}
	return c
}

// Run reconciles immediately, then on every interval until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) {
	c.reconcile(ctx)
	t := time.NewTicker(c.cfg.Interval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.reconcile(ctx)
		case <-c.nudge:
			c.reconcile(ctx)
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) {
	now := time.Now()
	snap, gaming, err := c.observe(ctx, now)
	if err != nil {
		// A failed observe is tolerated: the tick is skipped and the last good reading is kept
		// (publishing a zeroed sample here made a single transient Proxmox 502 flap
		// surplus/mode to zero on the dashboard), with the failure still visible via
		// energy_watchdog_last_reconcile_success. Transient blips are expected, so log at warn
		// and only escalate to error once they're clearly sustained.
		c.observeFailures++
		if c.observeFailures >= observeFailEscalate {
			c.log.Error("observe failing repeatedly", "err", err, "consecutive", c.observeFailures)
		} else {
			c.log.Warn("observe failed", "err", err, "consecutive", c.observeFailures)
		}
		c.metrics.MarkStale(now.Unix())
		// Blind, not silent: going quiet leaves nut-dog deciding p1 from a stale request,
		// which after a UPS recovery means waking it at whatever hour that lands, with no
		// solar reading behind it. Hold pins p1 where it is until we can decide again. A
		// UPS shed still overrides - that direction is never gated.
		c.holdPower(ctx)
		return
	}
	c.observeFailures = 0
	if !snap.NodeUp {
		c.askedOff = false // the shed landed; a later hand-start must not be shut back down
	}

	plan := Decide(snap, c.cfg, now)
	c.log.Info("decision",
		"mode", snap.Mode, "next", plan.NextMode,
		"surplus", snap.Surplus, "surplusRaw", snap.SurplusRaw, "soc", snap.SoC,
		"nodeUp", snap.NodeUp, "gaming", gaming, "reason", plan.Reason)
	// Publish the observation now, the outcome once apply resolves: apply can run for
	// minutes, and reporting its mode and success up front hid every failing apply.
	c.metrics.Update(metrics.Sample{
		Surplus: snap.Surplus, SurplusRaw: snap.SurplusRaw, SoC: snap.SoC,
		NodeUp: snap.NodeUp, Gaming: gaming, ManualShed: snap.ManualShed, ManualOn: snap.ManualOn,
		Tick: now.Unix(),
	})

	if err := c.apply(ctx, plan, snap); err != nil {
		c.log.Error("apply failed", "err", err)
		c.metrics.SetOutcome(string(snap.Mode), false) // plan didn't land; mode is unchanged
		return
	}
	c.metrics.SetOutcome(string(plan.NextMode), true)
}

// apply carries out the plan at the configured dry-run level. The physical Proxmox/WoL
// actions run only in full mode; alert additionally reconciles Alertmanager silences. The
// state machine is advanced and persisted in *every* mode: skipping the physical actions must
// not skip the bookkeeping, or the mode never latches and a dry run can't preview what live
// would decide.
func (c *Controller) apply(ctx context.Context, p Plan, snap Snapshot) error {
	// Replication into the managed node tracks its real power state in every mode, for the
	// same reason the silences do: while p1 is off, p2/p3 must not keep trying to
	// replicate to it and mailing about every failed run (JHC-538). Driving this from the
	// observed node state rather than from the plan also covers the cases no mode transition
	// produces - a p1 powered on by hand, or one already off when the watchdog starts. A
	// failure is logged, not returned: worst case the mails come back, which beats blocking
	// the whole shed on a replication API problem.
	if err := c.reconcileReplication(ctx, !snap.NodeUp); err != nil {
		c.log.Error("reconcile replication", "err", err)
	}
	// Silences are reconciled every tick, before the noop gate, because a shed can outlast
	// silenceTTL: driving them off the transition alone let a multi-day manual shed lapse.
	// This also keeps them ahead of the migrate/stop that make the alerts fire.
	switch c.cfg.DryRun {
	case config.DryRunAlert:
		// Alert-only takes no Proxmox actions, so its mode is simulated and can't drive
		// coverage; p1's real power state does.
		if err := c.reconcileSilences(ctx, !snap.NodeUp); err != nil {
			c.log.Error("alert-only: reconcile silences", "err", err)
		}
	case config.DryRunFull:
		if err := c.reconcileSilences(ctx, p.NextMode != state.ModeRunning); err != nil {
			return err
		}
	}
	// Restate the power wish every tick, not just on transitions. Skipped when the plan
	// transitions power itself: execute sends that in order, after the guests are handled.
	if c.cfg.DryRun == config.DryRunFull && c.power != nil && !p.Wake && !p.Poweroff {
		if err := c.restatePower(ctx, p, snap); err != nil {
			c.log.Warn("restate power request", "err", err)
		}
	}
	if isNoop(p, snap) {
		return nil
	}
	if c.cfg.DryRun == config.DryRunFull {
		return c.execute(ctx, p, snap) // physical actions + persist
	}
	// log / alert: advance the state machine, but take no physical action.
	c.logPlan(p)
	return c.persist(ctx, p, snap)
}

// persist advances the stored state to the plan's mode and grace clock without taking any
// physical action - used by the dry-run modes so the state machine latches tick to tick just
// as live would. The stopped-guest set carries through unchanged (nothing was stopped).
func (c *Controller) persist(ctx context.Context, p Plan, snap Snapshot) error {
	return c.store.Save(ctx, state.State{
		Mode:       p.NextMode,
		GraceSince: p.GraceSince,
		WakeDone:   p.WakeDone,
	})
}

// observe gathers the snapshot and reports whether a gaming guest is running.
func (c *Controller) observe(ctx context.Context, now time.Time) (Snapshot, bool, error) {
	st, err := c.store.Load(ctx)
	if err != nil {
		return Snapshot{}, false, err
	}
	intent, err := c.store.LoadIntent(ctx)
	if err != nil {
		return Snapshot{Mode: st.Mode}, false, err
	}
	reading, err := c.prom.Read(ctx, c.cfg.Prometheus)
	if err != nil {
		return Snapshot{Mode: st.Mode}, false, err
	}
	nodeUp, uptime, err := c.px.NodeState(ctx, c.cfg.Proxmox.Node)
	if err != nil {
		return Snapshot{Mode: st.Mode}, false, err
	}
	// Proxmox drops uptime from /nodes when the token lacks Sys.Audit, and an uptime of 0 reads
	// as freshly booted, so the fresh-boot window silently stops filtering anything.
	if nodeUp && uptime == 0 && !c.warnedNoUptime {
		c.log.Warn("node online but reports no uptime: adopting any online node, most likely the token lacks Sys.Audit on /nodes",
			"node", c.cfg.Proxmox.Node)
		c.warnedNoUptime = true
	}
	var guests []proxmox.Guest
	if nodeUp {
		if guests, err = c.px.Guests(ctx, c.cfg.Proxmox.Node); err != nil {
			return Snapshot{Mode: st.Mode}, false, err
		}
	}
	manualShed, manualOn := intent.Holds()
	snap := Snapshot{
		Surplus:    reading.Surplus,
		SurplusRaw: reading.SurplusRaw,
		SoC:        reading.SoC,
		NodeUp:     nodeUp,
		NodeUptime: uptime,
		Guests:     guests,
		Mode:       st.Mode,
		GraceSince: st.GraceSince,
		ManualShed: manualShed,
		ManualOn:   manualOn,
		Wake:       c.requestedWake(intent, now),
		WakeDone:   st.WakeDone,
	}
	return snap, nodeUp && gamingActive(guests, c.cfg.Guests.GamingGuard), nil
}

// requestedWake is the live wake requests, restricted to the gaming-guard block. The API
// authorises callers already; this makes a hand-edited intent unable to start, say, a Talos
// node VM, and stops a request keeping p1 up for a guest the guard would never hold it for.
func (c *Controller) requestedWake(intent state.Intent, now time.Time) []state.WakeRequest {
	var out []state.WakeRequest
	seen := map[int]bool{}
	for _, w := range intent.LiveWake(now, c.cfg.GamingGrace.Duration) {
		if !c.cfg.Guests.GamingGuard.Contains(w.VMID) {
			c.log.Warn("ignoring wake request outside the gaming-guard range", "vmid", w.VMID, "user", w.User)
			continue
		}
		// The API replaces a VM's request rather than appending, but intent.json is meant to be
		// hand-editable, and a repeated entry there would otherwise mean starting it twice.
		if seen[w.VMID] {
			continue
		}
		seen[w.VMID] = true
		out = append(out, w)
	}
	return out
}

// isNoop reports that the plan changes nothing worth a write. WakeDone counts: the tick that
// marks a request satisfied is otherwise a noop, and dropping that write would let the VM be
// started again the moment the user shuts it down.
func isNoop(p Plan, snap Snapshot) bool {
	return p.NextMode == snap.Mode && p.GraceSince == snap.GraceSince &&
		maps.Equal(p.WakeDone, snap.WakeDone) &&
		!p.Poweroff && !p.Wake &&
		len(p.Migrate) == 0 && len(p.Stop) == 0 && !p.RestoreStopped && len(p.StartRequested) == 0
}

func (c *Controller) logPlan(p Plan) {
	c.log.Info("[dry-run] would act",
		"migrate", guestIDs(p.Migrate), "stop", guestIDs(p.Stop), "restoreStopped", p.RestoreStopped,
		"startRequested", p.StartRequested,
		"poweroff", p.Poweroff, "wake", p.Wake,
		"nextMode", p.NextMode)
}

// execute applies the plan in a fixed, safe order and persists the resulting state.
func (c *Controller) execute(ctx context.Context, p Plan, snap Snapshot) error {
	st := state.State{Mode: snap.Mode, GraceSince: p.GraceSince, WakeDone: p.WakeDone}

	// Set before the first step, not per step: it has to cover the whole shed.
	switch {
	case p.Poweroff || len(p.Migrate) > 0 || len(p.Stop) > 0:
		c.setActivity(ActivityShedding)
	case p.Wake:
		c.setActivity(ActivityWaking)
	}
	defer c.setActivity("")

	if len(p.Migrate) > 0 {
		if err := c.migrateAll(ctx, p.Migrate); err != nil {
			return err
		}
	}
	if len(p.Stop) > 0 {
		if _, err := c.stopAll(ctx, p.Stop); err != nil {
			return err
		}
	}
	if p.Wake {
		if err := c.wake(ctx, p.Reason); err != nil {
			return err
		}
	}
	if p.RestoreStopped {
		if err := c.restoreStopped(ctx); err != nil {
			return err
		}
	}
	if len(p.StartRequested) > 0 {
		// Best-effort: a desktop VM that won't boot is the user's problem, and failing the
		// reconcile here would roll back the mode the wake just established.
		if err := c.startRequested(ctx, p.StartRequested); err != nil {
			c.log.Warn("start requested desktop VMs", "err", err)
		}
	}
	if p.Poweroff {
		// Disable replication ahead of the power-off instead of waiting for the next tick to
		// observe the node as down: a run landing inside the shutdown window is exactly the
		// failed run we're trying to avoid. Re-enabling needs no such treatment - the first
		// tick after the node reports up handles it, and replication staying off for one more
		// interval is harmless.
		if err := c.reconcileReplication(ctx, true); err != nil {
			c.log.Error("disable replication before power-off", "err", err)
		}
		c.log.Warn("powering off node", "node", c.cfg.Proxmox.Node)
		if err := c.powerOff(ctx, p.Reason); err != nil {
			return err
		}
	}
	st.Mode = p.NextMode
	st.GraceSince = p.GraceSince
	return c.store.Save(ctx, st)
}

// migrateAll runs one bulk migration per target node, concurrently. Guests still on the node
// after a round are retried against the next target: the old per-guest fallback for a full one.
func (c *Controller) migrateAll(ctx context.Context, guests []proxmox.Guest) error {
	targets := c.cfg.Proxmox.TargetNodes
	remaining := guestIDs(guests)
	var lastErr error
	for round := range targets {
		byTarget := map[string][]int{}
		for i, vmid := range remaining {
			t := targets[(i+round)%len(targets)] // equal split, shifted each round
			byTarget[t] = append(byTarget[t], vmid)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		for target, vmids := range byTarget {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := c.migrateBatch(ctx, target, vmids); err != nil {
					c.log.Warn("bulk migration failed", "target", target, "vmids", vmids, "err", err)
					mu.Lock()
					lastErr = err
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		left, err := c.stillOnNode(ctx, remaining)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			return nil
		}
		remaining = left
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("guests %v are still on %s after migrating", remaining, c.cfg.Proxmox.Node)
	}
	return lastErr
}

func (c *Controller) migrateBatch(ctx context.Context, target string, vmids []int) error {
	// migrateall has no per-guest timeout, so migrateTimeout bounds the whole batch. Running in
	// parallel, the batch is about as long as its slowest guest, which is what it bounded before.
	mctx, cancel := context.WithTimeout(ctx, c.cfg.Proxmox.MigrateTimeout.Duration)
	defer cancel()
	upid, err := c.px.MigrateAll(mctx, c.cfg.Proxmox.Node, target, vmids)
	if err != nil {
		return err
	}
	c.log.Info("migrating guests", "vmids", vmids, "target", target)
	return c.px.WaitTask(mctx, c.cfg.Proxmox.Node, upid)
}

// stillOnNode is the subset of vmids the node still hosts.
func (c *Controller) stillOnNode(ctx context.Context, vmids []int) ([]int, error) {
	guests, err := c.px.Guests(ctx, c.cfg.Proxmox.Node)
	if err != nil {
		return nil, err
	}
	here := make(map[int]bool, len(guests))
	for _, g := range guests {
		here[g.VMID] = true
	}
	var out []int
	for _, id := range vmids {
		if here[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// stopAll shuts the guests down in one bulk task: reverse startup order, max_workers at a
// time, each guest given stopTimeout to go down cleanly before the task reports it failed.
func (c *Controller) stopAll(ctx context.Context, guests []proxmox.Guest) ([]state.GuestRef, error) {
	// Proxmox gives each guest stopTimeout; bound the whole task at the worst case of every
	// guest in its own order group, so a task that never reports done can't wedge the loop.
	sctx, cancel := context.WithTimeout(ctx, time.Duration(len(guests))*c.cfg.Proxmox.StopTimeout.Duration)
	defer cancel()
	upid, err := c.px.StopAll(sctx, c.cfg.Proxmox.Node, guestIDs(guests), c.cfg.Proxmox.StopTimeout.Duration)
	if err == nil {
		err = c.px.WaitTask(sctx, c.cfg.Proxmox.Node, upid)
	}
	// Read back what stopped on the caller's ctx, not sctx: a stop that timed out still has to
	// record what did go down, or good-morning never starts those guests again.
	stopped, listErr := c.stoppedOf(ctx, guests)
	if listErr != nil {
		return refs(guests), errors.Join(err, listErr)
	}
	c.log.Info("stopped guests", "vmids", refIDs(stopped))
	return stopped, err
}

// stoppedOf is the subset of want the node no longer reports as running.
func (c *Controller) stoppedOf(ctx context.Context, want []proxmox.Guest) ([]state.GuestRef, error) {
	guests, err := c.px.Guests(ctx, c.cfg.Proxmox.Node)
	if err != nil {
		return nil, err
	}
	running := make(map[int]bool, len(guests))
	for _, g := range guests {
		running[g.VMID] = g.Running
	}
	var out []proxmox.Guest
	for _, g := range want {
		if !running[g.VMID] {
			out = append(out, g)
		}
	}
	return refs(out), nil
}

// startAll boots the guests we stopped, in startup order with each group's up= delay honoured.
// Best-effort: one guest that won't boot must not fail the reconcile and roll the mode back.
// restoreStopped brings the stop-class guests back at good-morning. The set is derived from
// config and the node's live guest list, never from a persisted record: p1 can be taken down
// by a UPS shed, a crash or pve-guests, none of which this loop drives, and a recorded list is
// then empty exactly when it is needed. Guests tagged NoAutostartTag stay off - the watchdog
// cannot tell a hand-stopped guest from one a shutdown killed, so that intent is declared.
func (c *Controller) restoreStopped(ctx context.Context) error {
	guests, err := c.px.Guests(ctx, c.cfg.Proxmox.Node)
	if err != nil {
		return err
	}
	var want []state.GuestRef
	for _, g := range guests {
		if g.Running || !c.cfg.Guests.Stop.Contains(g.VMID) {
			continue
		}
		if g.HasTag(config.NoAutostartTag) {
			c.log.Info("leaving guest stopped", "vmid", g.VMID, "tag", config.NoAutostartTag)
			continue
		}
		want = append(want, state.GuestRef{VMID: g.VMID, Type: string(g.Type)})
	}
	if len(want) == 0 {
		return nil
	}
	return c.startAll(ctx, want)
}

func (c *Controller) startAll(ctx context.Context, guests []state.GuestRef) error {
	ids := refIDs(guests)
	upid, err := c.px.StartAll(ctx, c.cfg.Proxmox.Node, ids)
	if err == nil {
		err = c.px.WaitTask(ctx, c.cfg.Proxmox.Node, upid)
	}
	if err != nil {
		c.log.Warn("failed to start guests", "vmids", ids, "err", err)
		return nil
	}
	c.log.Info("started guests", "vmids", ids)
	return nil
}

// startRequested boots the desktop VMs a self-service request asked for. The ids come from
// Decide without a guest type, because the node may still have been down then; now that it is
// up its guest list resolves them.
func (c *Controller) startRequested(ctx context.Context, ids []int) error {
	guests, err := c.px.Guests(ctx, c.cfg.Proxmox.Node)
	if err != nil {
		return err
	}
	byID := make(map[int]proxmox.Guest, len(guests))
	for _, g := range guests {
		byID[g.VMID] = g
	}
	var want []int
	for _, id := range ids {
		g, ok := byID[id]
		if !ok {
			c.log.Warn("requested guest is not on the node", "vmid", id, "node", c.cfg.Proxmox.Node)
			continue
		}
		if !g.Running {
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return nil
	}
	upid, err := c.px.StartAll(ctx, c.cfg.Proxmox.Node, want)
	if err == nil {
		err = c.px.WaitTask(ctx, c.cfg.Proxmox.Node, upid)
	}
	if err != nil {
		return err
	}
	c.log.Info("started requested guests", "vmids", want)
	return nil
}

func (c *Controller) wake(ctx context.Context, reason string) error {
	c.askedOff = false
	if c.power != nil {
		// nut-dog owns the WoL: its packet doesn't need another Proxmox node to relay it,
		// which is exactly what a full shed leaves us without.
		if err := c.requestPower(ctx, powerapi.On, reason); err != nil {
			return err
		}
		c.log.Info("asked nut-dog to power on", "node", c.cfg.Proxmox.Node)
	} else {
		mac, err := c.px.WakeOnLAN(ctx, c.cfg.Proxmox.Node)
		if err != nil {
			return err
		}
		c.log.Info("sent Wake-on-LAN", "mac", mac, "node", c.cfg.Proxmox.Node)
	}
	wctx, cancel := context.WithTimeout(ctx, c.cfg.Proxmox.WakeTimeout.Duration)
	defer cancel()
	return c.px.WaitNodeUp(wctx, c.cfg.Proxmox.Node)
}

// powerOff takes p1 down: through nut-dog when it owns the power, else directly.
func (c *Controller) powerOff(ctx context.Context, reason string) error {
	if c.power != nil {
		c.askedOff = true
		return c.requestPower(ctx, powerapi.Off, reason)
	}
	return c.px.ShutdownNode(ctx, c.cfg.Proxmox.Node)
}

// powerWish is what to restate for a plan that isn't transitioning power itself. askedOff says
// we requested this shed and p1 hasn't gone down yet; without it that state is
// indistinguishable from a p1 started by hand during a shed, where asserting off would cut
// power under running guests. Restating hold there instead replaced an off nut-dog had not yet
// polled (15s) and silently dropped the shed - off is idempotent, hold is not a no-op.
func powerWish(p Plan, snap Snapshot, askedOff bool) string {
	switch {
	case p.NextMode != state.ModeShed && snap.NodeUp:
		return powerapi.On
	case snap.Mode == state.ModeShed && (!snap.NodeUp || askedOff):
		return powerapi.Off
	default:
		return powerapi.Hold
	}
}

// requestPower sends one power request and records whether it landed. Every path that moves
// p1 goes through here: all of its power runs over this call now, so a wrong token or load
// name has to show up on the metric rather than only in a log line.
func (c *Controller) requestPower(ctx context.Context, desired, reason string) error {
	err := c.power.Request(ctx, desired, reason)
	c.metrics.SetPowerRequestOK(err == nil)
	return err
}

// holdPower asks nut-dog to leave p1 alone, for when this loop cannot decide. Hold survives an
// asserted shed signal, but not one nut-dog has yet to poll: it would replace that off and emit
// no action, so a shed of ours in flight keeps being asserted instead.
func (c *Controller) holdPower(ctx context.Context) {
	if c.power == nil || c.cfg.DryRun != config.DryRunFull {
		return
	}
	desired, reason := powerapi.Hold, "watchdog cannot observe"
	if c.askedOff {
		desired, reason = powerapi.Off, "watchdog cannot observe; shed still in flight"
	}
	if err := c.requestPower(ctx, desired, reason); err != nil {
		c.log.Warn("blind power request", "desired", desired, "err", err)
	}
}

// restatePower re-sends the wish. nut-dog keeps no state across restarts, so the request has
// to be level-triggered rather than edge-triggered.
func (c *Controller) restatePower(ctx context.Context, p Plan, snap Snapshot) error {
	return c.requestPower(ctx, powerWish(p, snap, c.askedOff), "solar")
}

// replicationMarker prefixes the comment of every replication job the watchdog disables. It
// is how a job is recognised as ours when the node comes back: one that was disabled by hand
// carries no marker and is therefore never re-enabled by us. Recognising our own work from
// the remote object rather than from persisted ids is the same approach as the Alertmanager
// silences, and it means a lost or stale state ConfigMap can never strand replication in the
// disabled state. Whatever comment the job already had is kept after the marker and restored
// verbatim when the job is re-enabled.
const replicationMarker = "[energy-watchdog]"

// markComment is the comment stored on a job we disable: our marker, then the job's own
// comment. It is the inverse of unmarkComment.
func markComment(orig string) string {
	return strings.TrimSpace(replicationMarker + " " + orig)
}

// unmarkComment recovers the job's original comment, reporting whether the job carried our
// marker at all. Anything unmarked is not ours to touch.
func unmarkComment(comment string) (orig string, ours bool) {
	rest, ok := strings.CutPrefix(comment, replicationMarker)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// reconcileReplication makes the cluster replication jobs that target the managed node match
// its power state: disabled while it is down, enabled while it is up. Only the jobs that
// replicate *into* the node are touched; jobs sourced from it need nothing, because pvesr
// runs on the source node and so they simply don't run while it is off.
//
// A job is claimed only by disabling one that is currently enabled, and only a claimed job -
// identified by replicationMarker in its comment - is ever re-enabled. A job someone
// disabled by hand is therefore left alone in both directions. Writes happen only where a
// job isn't already in the wanted state, so a steady-state tick makes no write calls at all.
func (c *Controller) reconcileReplication(ctx context.Context, disable bool) error {
	if !c.cfg.Proxmox.ReplicationManaged() {
		return nil
	}
	jobs, err := c.px.ReplicationJobs(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, j := range jobs {
		if j.Target != c.cfg.Proxmox.Node {
			continue
		}
		orig, ours := unmarkComment(j.Comment)
		if !ours {
			if !disable || j.Disabled {
				continue // not ours, and not an enabled job we want off: leave it entirely alone
			}
			orig = j.Comment
		}
		comment := orig
		if disable {
			comment = markComment(orig)
		}
		if j.Disabled == disable && j.Comment == comment {
			continue
		}
		if c.cfg.DryRun == config.DryRunLog {
			c.log.Info("[dry-run] would set replication job",
				"id", j.ID, "source", j.Source, "target", j.Target, "disabled", disable)
			continue
		}
		if err := c.px.SetReplicationJob(ctx, j.ID, comment, disable); err != nil {
			c.log.Error("set replication job", "id", j.ID, "disabled", disable, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		c.log.Info("set replication job",
			"id", j.ID, "source", j.Source, "target", j.Target, "disabled", disable)
	}
	return firstErr
}

// silenceTTL is how long each Alertmanager silence lasts before it self-expires. The
// controller extends a silence before this lapses, so an arbitrarily long shutdown stays
// covered; if the watchdog itself dies, its silences self-expire within one TTL instead of
// lingering. silenceRefresh is how close to expiry a silence may drift before it's extended.
const (
	silenceTTL     = 24 * time.Hour
	silenceRefresh = time.Hour
)

// reconcileSilences makes the energy-watchdog silences in every configured Alertmanager
// match the desired set: the configured silences when silence is true, or none when false.
// What that boolean tracks is the caller's business - see apply. It never persists silence
// ids - it recognises its own silences by createdBy on each Alertmanager - so a lost or stale
// ConfigMap can't orphan them, and any orphans from an earlier run are cleaned up here. Each
// Alertmanager is reconciled independently, so one being unreachable doesn't disturb the others.
func (c *Controller) reconcileSilences(ctx context.Context, silence bool) error {
	var desired []config.Silence
	if silence {
		desired = c.cfg.Alertmanager.Silences
	}
	var firstErr error
	for _, url := range c.cfg.Alertmanager.URLs {
		// When silencing, anything of ours that isn't desired is a stale-config orphan and is
		// removed at once. When unsilencing, drop coverage via the grace window
		// instead, so guests still booting on the node aren't un-suppressed the same tick.
		if err := c.reconcileSilencesAt(ctx, url, desired, !silence); err != nil {
			c.log.Error("reconcile silences", "url", url, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// reconcileSilencesAt converges one Alertmanager to `desired`: it creates any desired
// silence that's missing, extends one that's drifting toward expiry, and retires every
// other silence of ours (duplicates left by the old create-every-tick behaviour, silences
// from a since-changed config, or all of them when p1 is back up). Coverage is never
// dropped mid-run: creates and extensions happen before any retire. When graceDrop is set
// (p1 back up), a no-longer-wanted silence isn't deleted outright but shortened to the grace
// window and left to lapse, so alerts from guests still booting on the node stay suppressed.
func (c *Controller) reconcileSilencesAt(ctx context.Context, url string, desired []config.Silence, graceDrop bool) error {
	am := c.ams[url]
	if am == nil {
		return fmt.Errorf("no client for alertmanager %s", url)
	}
	existing, err := am.List(ctx)
	if err != nil {
		return err
	}
	now := time.Now()

	// Index our live silences by canonical key, keeping the latest-expiring one per key and
	// marking any others (duplicates) for deletion.
	ours := map[string]alertmgr.Silence{}
	var surplus []alertmgr.Silence
	for _, s := range existing {
		if s.CreatedBy != alertmgr.CreatedBy || (s.Status.State != "active" && s.Status.State != "pending") {
			continue
		}
		if cur, ok := ours[s.Key()]; ok {
			if s.EndsAt.After(cur.EndsAt) {
				surplus = append(surplus, cur)
				ours[s.Key()] = s
			} else {
				surplus = append(surplus, s)
			}
		} else {
			ours[s.Key()] = s
		}
	}

	// Ensure each desired silence exists and isn't about to expire.
	wanted := map[string]bool{}
	for _, d := range desired {
		key := alertmgr.DesiredKey(c.cfg.Alertmanager.Comment, d.Matchers)
		wanted[key] = true
		switch s, ok := ours[key]; {
		case !ok:
			id, err := am.Create(ctx, d.Matchers, c.cfg.Alertmanager.Comment, silenceTTL, now)
			if err != nil {
				return err
			}
			c.log.Info("created alertmanager silence", "url", url, "id", id)
		case s.EndsAt.Sub(now) < silenceRefresh:
			if _, err := am.Update(ctx, s.ID, d.Matchers, c.cfg.Alertmanager.Comment, silenceTTL, now); err != nil {
				return err
			}
			c.log.Info("extended alertmanager silence", "url", url, "id", s.ID)
		}
	}

	// Retire duplicates and anything of ours that's no longer wanted.
	for _, s := range surplus {
		if err := am.Delete(ctx, s.ID); err != nil {
			return err
		}
		c.log.Info("removed duplicate alertmanager silence", "url", url, "id", s.ID)
	}
	for key, s := range ours {
		if wanted[key] {
			continue
		}
		if graceDrop {
			// Let the silence lapse after the grace window rather than dropping it now. Only
			// shorten one that still ends beyond the window, so once it's inside the window it's
			// left to expire on its own instead of being pushed out again every tick.
			grace := c.cfg.Alertmanager.UnsilenceGrace.Duration
			if s.EndsAt.After(now.Add(grace)) {
				if _, err := am.Reschedule(ctx, s, grace, now); err != nil {
					return err
				}
				c.log.Info("grace-expiring alertmanager silence", "url", url, "id", s.ID, "grace", grace)
			}
			continue
		}
		if err := am.Delete(ctx, s.ID); err != nil {
			return err
		}
		c.log.Info("removed alertmanager silence", "url", url, "id", s.ID)
	}
	return nil
}

func guestIDs(gs []proxmox.Guest) []int {
	out := make([]int, len(gs))
	for i, g := range gs {
		out[i] = g.VMID
	}
	return out
}

func refIDs(rs []state.GuestRef) []int {
	out := make([]int, len(rs))
	for i, r := range rs {
		out[i] = r.VMID
	}
	return out
}
