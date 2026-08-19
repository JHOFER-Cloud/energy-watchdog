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

	// startFails tracks desktop VMs that will not start, by VMID. Read by the self-service
	// API from its own goroutines, hence the mutex. Not persisted: a restart inside the
	// request's TTL re-arms every retired request, so p1 is woken and the VM tried three
	// more times. That is bounded and self-limiting, and cheaper than a persisted record
	// that could strand a VM as unstartable across the fix for whatever broke it.
	startMu    sync.Mutex
	startFails map[int]startFailure

	nudge chan struct{} // out-of-band reconcile requests from the self-service API

	// wakePoll is how often the stop phase re-reads intent to notice a desktop VM being asked
	// for. A field so tests don't have to wait it out; see waitStop.
	wakePoll time.Duration

	// nodeDownStreak counts consecutive ticks that reported p1 offline with nothing agreeing
	// it lost power. Reset the moment p1 reads up; see confirmDown.
	nodeDownStreak int

	// pendingStop is a bulk stop task we stopped waiting for, kept so the restore can let it
	// finish first. Without it good-morning reads the guest list mid-task, sees a guest the task
	// has not reached yet as running, leaves it out of the restart - and the task stops it
	// moments later, leaving it down until the next shed and restore, a day away. Not persisted
	// and dropped the moment p1 is seen down: the task died with the host either way.
	pendingStop stopTask

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

// wakePoll is how often the stop phase re-reads intent while the guests go down. Cheap - one
// ConfigMap read against the local apiserver - and only while a shed is actually running.
const wakePoll = 5 * time.Second

// startFailLimit is how many tries a requested desktop VM gets before the request is
// retired, at most one per reconcile interval. Without it a VM that cannot boot is retried
// every tick for the rest of the request's TTL, which holds p1 up on a session that will
// never happen and leaves the user watching "Starting…" with the error only in the pod log.
const startFailLimit = 3

// stopTask is a bulk stop task and the budget stopAll gave it. The deadline travels with the
// UPID because it is not derivable from config: stopAll allows the task every guest's
// stopTimeout in turn, so a settle bounded at a single one would give up while the task still
// had guests to go - which is the mid-task guest list read the settle exists to prevent.
type stopTask struct {
	upid     string
	deadline time.Time
}

// startFailure is one desktop VM that would not start: how many tries it has had, the
// request those tries belong to, and what Proxmox last said. Keying on the request is what
// lets a fresh press of Start re-arm it - the same request never gets another run of tries.
type startFailure struct {
	tries int
	reqAt int64
	err   string
	last  time.Time // when a try was last counted; see recordStartFailure
}

// StartFailure is a retired wake request as the API surfaces it. RequestedAt is what makes
// it usable: the caller has to be able to tell whether the failure belongs to the request
// still outstanding, or to an older one the user has since pressed Start past.
type StartFailure struct {
	Err         string
	RequestedAt int64
}

// StartFailures is the last error for each desktop VM whose wake request has been retired.
// The self-service API turns it into something the user can read.
func (c *Controller) StartFailures() map[int]StartFailure {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	out := map[int]StartFailure{}
	for vmid, f := range c.startFails {
		if f.tries >= startFailLimit {
			out[vmid] = StartFailure{Err: f.err, RequestedAt: f.reqAt}
		}
	}
	return out
}

// recordStartFailure counts one failed attempt for vmid, and logs the try that retires it.
func (c *Controller) recordStartFailure(vmid int, reqAt int64, msg string) {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	f := c.startFails[vmid]
	if f.reqAt != reqAt {
		f = startFailure{reqAt: reqAt} // a different request: start counting again
	}
	f.err = msg
	// Tries are not evenly spaced: the API nudges a reconcile on every button press, by
	// anyone, so unrelated clicks would otherwise burn the whole budget seconds after the
	// first failure - well before a transient Proxmox condition has had a chance to clear.
	// One per interval keeps the limit meaning what its comment says.
	now := time.Now()
	if f.tries > 0 && now.Sub(f.last) < c.cfg.Interval.Duration {
		c.startFails[vmid] = f
		return
	}
	f.tries++
	f.last = now
	c.startFails[vmid] = f
	if f.tries == startFailLimit {
		c.log.Error("giving up on a requested desktop VM", "vmid", vmid, "tries", f.tries, "err", msg)
	}
}

func (c *Controller) clearStartFailure(vmid int) {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	delete(c.startFails, vmid)
}

// retired reports that w has had its tries and must not be acted on again. A newer request
// for the same VM is a new ask and gets its own tries.
func (c *Controller) retired(w state.WakeRequest) bool {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	f, ok := c.startFails[w.VMID]
	return ok && f.reqAt == w.RequestedAt && f.tries >= startFailLimit
}

// New builds a Controller. ams is keyed by Alertmanager base URL so a persisted
// silence can be deleted from the same Alertmanager it was created in.
func New(cfg *config.Config, p *prom.Client, px *proxmox.Client, ams map[string]*alertmgr.Client, store state.Store, m *metrics.Metrics, log *slog.Logger) *Controller {
	c := &Controller{cfg: cfg, prom: p, px: px, ams: ams, store: store, metrics: m, log: log,
		nudge: make(chan struct{}, 1), startFails: map[int]startFailure{}, wakePoll: wakePoll}
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
		// Whatever we were holding for, this is not it any more: blind is not the same as
		// deliberately holding a host we can see is alive, and the alert on that gauge tells
		// the operator the loop is healthy and offers them a manual power-off. Leaving it
		// latched here would say exactly that while we cannot observe at all.
		c.metrics.SetNodeUnconfirmed(false)
		// Blind, not silent: going quiet leaves nut-dog deciding p1 from a stale request,
		// which after a UPS recovery means waking it at whatever hour that lands, with no
		// solar reading behind it. This pins p1 where it is until we can decide again, except
		// for a shed of ours in flight, which keeps being asserted. A UPS shed still overrides.
		c.holdPower(ctx)
		return
	}
	c.observeFailures = 0
	if snap.NodeUp {
		c.nodeDownStreak = 0
	} else if !c.confirmDown(ctx, snap) {
		// p1 reads offline but nothing agrees it actually lost power. Do not latch a mode off
		// that: treat the tick like a failed observe, which pins p1 where it is. This can hold
		// indefinitely - a partition does not heal on our account - so it gets its own gauge
		// rather than hiding inside the stale-reconcile one.
		c.metrics.SetNodeUnconfirmed(true)
		c.metrics.MarkStale(now.Unix())
		c.holdPower(ctx)
		return
	}
	c.metrics.SetNodeUnconfirmed(false)
	if !snap.NodeUp {
		c.askedOff = false         // the shed landed; a later hand-start must not be shut back down
		c.pendingStop = stopTask{} // whatever it still had to stop went down with the host
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
		Surplus:      reading.Surplus,
		SurplusRaw:   reading.SurplusRaw,
		SoC:          reading.SoC,
		NodeUp:       nodeUp,
		NodeUptime:   uptime,
		Guests:       guests,
		Mode:         st.Mode,
		GraceSince:   st.GraceSince,
		ManualShed:   manualShed,
		ManualOn:     manualOn,
		ShedInFlight: c.askedOff,
		Wake:         c.requestedWake(intent, now),
		WakeDone:     st.WakeDone,
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
		// A request we have given up on is dropped here rather than in Decide: it then holds
		// no grace clock and plans no start, so p1 sheds on schedule instead of waiting out a
		// VM that isn't coming.
		if c.retired(w) {
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
		if _, err := c.stopAll(ctx, p.Stop, c.wakeInterrupt(ctx, p)); err != nil {
			if !errors.Is(err, errStopInterrupted) {
				return err
			}
			// Someone asked for their desktop VM while the guests were going down. The stop task
			// carries on - that load is shed either way - but the power-off this shed was headed
			// for is off the table, so there is nothing left worth waiting for. Latch the mode
			// and let the queued nudge adopt the session: the same landing state as the re-check
			// below, minutes earlier.
			c.log.Info("desktop VM requested while the guests were stopping: leaving p1 up",
				"node", c.cfg.Proxmox.Node)
			return c.saveMode(ctx, st, p)
		}
	}
	if p.Wake {
		if err := c.wake(ctx, p.Reason); err != nil {
			return err
		}
	}
	// Ahead of the restore: someone is waiting on this one, and nothing about it needs the
	// restore first - config refuses a stop class overlapping the gaming guard, so the guests
	// being restarted and the VM being started are always disjoint.
	if len(p.StartRequested) > 0 {
		// Best-effort: a desktop VM that won't boot is the user's problem, and failing the
		// reconcile here would roll back the mode the wake just established.
		if err := c.startRequested(ctx, p.StartRequested, snap.Wake); err != nil {
			c.log.Warn("start requested desktop VMs", "err", err)
		}
	}
	if p.RestoreStopped {
		if err := c.restoreStopped(ctx); err != nil {
			return err
		}
	}
	if p.Poweroff {
		// The last moment this is still a decision, and the backstop for the poll in the stop
		// phase: a request landing after that phase ends, or during a shed with no guests to
		// stop at all, is first read here - p1 still up, nothing yet told to shut it down.
		// Leave it up and let the queued tick adopt the gaming session, instead of powering off
		// and making someone wait out a shutdown and a boot for a host that never had to go down.
		if c.wakeRequestedNow(ctx, p.WakeDone) {
			c.log.Info("desktop VM requested while shedding: leaving p1 up", "node", c.cfg.Proxmox.Node)
			return c.saveMode(ctx, st, p)
		}
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
	return c.saveMode(ctx, st, p)
}

func (c *Controller) saveMode(ctx context.Context, st state.State, p Plan) error {
	st.Mode = p.NextMode
	st.GraceSince = p.GraceSince
	return c.store.Save(ctx, st)
}

// wakeRequestedNow reports whether a desktop VM is being asked for right now, re-read from
// the store rather than taken from the snapshot: the plan holding that snapshot was made
// before the stop phase, which runs for minutes. done is the plan's spent-request markers, so
// a request already seen through to a running VM doesn't count. A failed read reports none -
// the shed the plan asked for is what happens by default.
func (c *Controller) wakeRequestedNow(ctx context.Context, done map[int]int64) bool {
	intent, err := c.store.LoadIntent(ctx)
	if err != nil {
		c.log.Warn("re-read intent before power-off", "err", err)
		return false
	}
	for _, w := range c.requestedWake(intent, time.Now()) {
		if w.RequestedAt > done[w.VMID] {
			return true
		}
	}
	return false
}

// nodeDownConfirm is how many consecutive ticks must report p1 offline, with nothing else
// agreeing, before this loop acts on it. One tick is not evidence: the reading comes from the
// rest of the pve cluster, which loses sight of a node for reasons that have nothing to do
// with power.
const nodeDownConfirm = 2

// probeMaxAge is how old nut-dog's probe of p1 may be and still count as an opinion. It polls
// every 15s, so anything approaching this means its own loop is in trouble and the reading
// should not be allowed to settle an argument.
const probeMaxAge = 2 * time.Minute

// confirmDown decides whether to believe a Proxmox cluster reporting p1 offline.
//
// That reading is not a power reading. It is the *other* pve nodes' opinion, so a p1 merely
// partitioned from corosync is reported exactly like one that is switched off - and this loop
// acting on it is what powered a healthy host down under a running gaming session: mode
// latched to shed, and the restate then asserted off, whereupon p1's upsmon shut it down.
//
// Only a reading that contradicts us is worth confirming. A shed of ours needs none - down is
// precisely what we asked for - and neither does a mode that already has p1 down, where it is
// the expected steady state and confirming it every tick would only delay good-morning.
// Otherwise nut-dog's probe settles it, since that reaches p1 directly; and where it has no
// fresh opinion, the cluster has to say it twice.
func (c *Controller) confirmDown(ctx context.Context, snap Snapshot) bool {
	if c.askedOff || snap.Mode == state.ModeShed {
		return true
	}
	// Anything we cannot use reads as unknown, so the fallback below is reached by one path
	// rather than several: no nut-dog configured, a failed call, or a probe too old to mean
	// anything are all "nobody can corroborate this".
	actual, age := powerapi.ActualUnknown, time.Duration(0)
	if c.power != nil {
		switch a, ag, err := c.power.State(ctx); {
		case err != nil:
			c.log.Warn("read p1's power state from nut-dog", "err", err)
		case ag > probeMaxAge:
			c.log.Warn("nut-dog's probe of p1 is too old to settle this", "probeAge", ag)
		default:
			actual, age = a, ag
		}
	}
	switch actual {
	case powerapi.ActualUp:
		// The one case worth shouting about: p1 is running and we cannot see it. Powering it
		// off from here would take down whatever is running on it. The streak resets too - a
		// probe that reaches p1 is evidence *against* acting, and leaving it standing would let
		// two such ticks either side of an unreachable nut-dog add up to a reason to act.
		c.nodeDownStreak = 0
		c.log.Error("p1 reads offline in Proxmox but nut-dog probes it as up: holding its power",
			"node", c.cfg.Proxmox.Node, "probeAge", age)
		return false
	case powerapi.ActualDown:
		return true // an independent reading agrees; no need to wait for a second tick
	case powerapi.ActualUnknown, "":
		// nut-dog has nothing to say, or we never got to ask. Fall through to the streak.
	default:
		// A word we don't recognise means the two services no longer agree on the vocabulary,
		// which silently costs us the check above - the one that keeps a partitioned p1 alive -
		// and leaves only the streak, which would not have stopped the shed on 19 Aug. Safe to
		// carry on, but never quietly.
		c.log.Error("nut-dog reported a power state we do not recognise; the up/down check is not working",
			"actual", actual, "node", c.cfg.Proxmox.Node)
	}
	c.nodeDownStreak++
	if c.nodeDownStreak < nodeDownConfirm {
		c.log.Warn("p1 reads offline with nothing to confirm it: waiting a tick before acting",
			"node", c.cfg.Proxmox.Node, "probe", actual, "streak", c.nodeDownStreak)
		return false
	}
	return true
}

// wakeInterrupt is what cuts the stop phase short: a desktop VM asked for during a shed that
// is still headed for a power-off. nil for every other plan, and in particular for the one that
// already knows about the request - it stops the same guests and then starts the VM in the same
// run, so interrupting it would abandon that stop only to put the start off until the next
// tick. There is nothing to rescue there: p1 was never going down.
//
// Only the stop phase gets this. It is the last thing a shed does before the power-off, so
// everything else has already been dispatched to Proxmox by then; giving up the wait during
// the migrate phase instead would skip the stop entirely, and the plan that follows - ModeShed
// adopting the session - plans no stop of its own, so that load would stay up all session.
func (c *Controller) wakeInterrupt(ctx context.Context, p Plan) func() bool {
	if !p.Poweroff {
		return nil
	}
	return func() bool { return c.wakeRequestedNow(ctx, p.WakeDone) }
}

// errStopInterrupted reports that the wait for the bulk stop task was given up because a
// desktop VM was asked for. The task itself is untouched and still running; only our waiting
// on it ended.
var errStopInterrupted = errors.New("desktop VM requested while the guests were stopping")

// waitStop waits out the bulk stop task, polling interrupt alongside it. The guests are left to
// finish going down either way: they are shed load whichever way the session goes, and the
// point here is only that p1 is up and usable for the minutes that takes - which is exactly
// how long a user who pressed Start seconds into a shed used to sit watching "shedding".
func (c *Controller) waitStop(ctx context.Context, upid string, interrupt func() bool) error {
	poll := c.wakePoll
	if interrupt == nil || poll <= 0 {
		// A Controller built by hand rather than by New has no poll interval, and NewTicker
		// panics on one - inside the reconcile goroutine, mid-shed. Wait it out instead.
		return c.px.WaitTask(ctx, c.cfg.Proxmox.Node, upid)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the waiter on every path out, including the interrupted one
	done := make(chan error, 1)
	go func() { done <- c.px.WaitTask(wctx, c.cfg.Proxmox.Node, upid) }()
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-t.C:
			// The task first: a select with both cases ready picks uniformly, so checking the
			// interrupt straight away would now and then report a task that had just *failed*
			// as interrupted - and an interrupted stop is never retried, while a failed one is.
			select {
			case err := <-done:
				return err
			default:
			}
			if interrupt() {
				return errStopInterrupted
			}
		}
	}
}

// settlePendingStop lets a stop task we walked away from finish before the guest list is read
// for the restore, so a guest it had not reached yet is seen as stopped and started again
// rather than left down. Bounded, and never fatal: the restore is best-effort, and a guest the
// task never got to is still running, which is the harmless direction.
func (c *Controller) settlePendingStop(ctx context.Context) {
	task := c.pendingStop
	if task.upid == "" {
		return
	}
	c.pendingStop = stopTask{} // cleared before the wait: a wedged task is never re-entered
	wctx, cancel := context.WithDeadline(ctx, task.deadline)
	defer cancel()
	c.log.Info("waiting for the abandoned stop task before restoring guests", "upid", task.upid)
	if err := c.px.WaitTask(wctx, c.cfg.Proxmox.Node, task.upid); err != nil {
		c.log.Warn("abandoned stop task did not finish cleanly", "upid", task.upid, "err", err)
	}
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
// interrupt, when set, can give up the wait early; see waitStop.
func (c *Controller) stopAll(ctx context.Context, guests []proxmox.Guest, interrupt func() bool) ([]state.GuestRef, error) {
	// Proxmox gives each guest stopTimeout; bound the whole task at the worst case of every
	// guest in its own order group, so a task that never reports done can't wedge the loop.
	sctx, cancel := context.WithTimeout(ctx, time.Duration(len(guests))*c.cfg.Proxmox.StopTimeout.Duration)
	defer cancel()
	upid, err := c.px.StopAll(sctx, c.cfg.Proxmox.Node, guestIDs(guests), c.cfg.Proxmox.StopTimeout.Duration)
	if err == nil {
		err = c.waitStop(sctx, upid, interrupt)
		if errors.Is(err, errStopInterrupted) {
			// Still working through the guests; the restore waits for it, on the budget it was
			// given here rather than one invented at the far end.
			deadline, _ := sctx.Deadline()
			c.pendingStop = stopTask{upid: upid, deadline: deadline}
		}
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
	c.settlePendingStop(ctx)
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
//
// Every attempt counts against the request, whatever the start task reported, so a VM that
// will not come up is given up on rather than retried until the request ages out. Success is
// then established by reading the node back, not by trusting the task: a bulk start can
// report OK while one of its guests stayed down, and that guest is the whole point here.
func (c *Controller) startRequested(ctx context.Context, ids []int, wake []state.WakeRequest) error {
	reqAt := make(map[int]int64, len(wake))
	for _, w := range wake {
		reqAt[w.VMID] = w.RequestedAt
	}
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
			c.recordStartFailure(id, reqAt[id], "this VM is not on "+c.cfg.Proxmox.Node)
			continue
		}
		if g.Running {
			c.clearStartFailure(id)
			continue
		}
		want = append(want, id)
	}
	if len(want) == 0 {
		return nil
	}
	upid, err := c.px.StartAll(ctx, c.cfg.Proxmox.Node, want)
	if err == nil {
		err = c.px.WaitTask(ctx, c.cfg.Proxmox.Node, upid)
	}
	// The node decides what counts, not the task: one task covers the whole batch and can
	// come back OK over a guest that stayed down. If that read fails, fall back to the task's
	// word - counting a start that worked would retire a healthy VM, the worse of the two
	// mistakes.
	up := map[int]bool{}
	if after, rerr := c.px.Guests(ctx, c.cfg.Proxmox.Node); rerr == nil {
		for _, g := range after {
			up[g.VMID] = g.Running
		}
	} else {
		c.log.Warn("cannot confirm requested starts", "vmids", want, "err", rerr)
		for _, id := range want {
			up[id] = err == nil
		}
	}
	msg := "the VM did not come up"
	if err != nil {
		msg = err.Error() // one task covers the batch, so its error is all any of them gets
	}
	var started []int
	for _, id := range want {
		if up[id] {
			c.clearStartFailure(id)
			started = append(started, id)
			continue
		}
		c.recordStartFailure(id, reqAt[id], msg)
	}
	if len(started) > 0 {
		c.log.Info("started requested guests", "vmids", started)
	}
	return err
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

// powerWish is what to restate for a plan that isn't transitioning power itself.
//
// askedOff answers first because it is the one thing here that is not a wish but a fact: p1's
// upsmon already has the signal and cannot be told to stop, so anything else would only
// release it and leave the shed half-applied - which is how a shed came to stop every guest
// and leave p1 running. Without that flag the state is indistinguishable from a p1 started by
// hand during a shed, where asserting off would cut power under running guests.
//
// Nothing else ever asks for off. A p1 that merely *reads* as down used to restate off here,
// to keep the request level-triggered against a nut-dog that had restarted and forgotten - but
// nut-dog's own startup grace holds every power-on for minutes after a restart, which covers
// that with room to spare, and hold restated each tick keeps it just as still. What the off
// did instead was shed a host this loop had lost sight of for reasons that were never about
// power, killing a running session. Off is now only ever a decision, never an inference.
func powerWish(p Plan, snap Snapshot, askedOff bool) string {
	switch {
	case askedOff:
		return powerapi.Off
	case p.NextMode != state.ModeShed && snap.NodeUp:
		return powerapi.On
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

// holdPower is what this loop says when it cannot decide: leave p1 alone - unless a shed of
// ours is in flight, where it re-asserts off instead. Hold survives an asserted shed signal but
// not one nut-dog has yet to poll, since it would replace that off and emit no action.
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
