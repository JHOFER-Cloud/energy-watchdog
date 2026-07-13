// Package controller runs the reconcile loop: observe solar + Proxmox, Decide a Plan,
// then (unless dry-run) execute it and persist state.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/alertmgr"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/metrics"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/prom"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/wol"
)

// Controller wires the clients and persisted state together.
type Controller struct {
	cfg     *config.Config
	prom    *prom.Client
	px      *proxmox.Client
	ams     map[string]*alertmgr.Client
	store   state.Store
	metrics *metrics.Metrics
	log     *slog.Logger
}

// New builds a Controller. ams is keyed by Alertmanager base URL so a persisted
// silence can be deleted from the same Alertmanager it was created in.
func New(cfg *config.Config, p *prom.Client, px *proxmox.Client, ams map[string]*alertmgr.Client, store state.Store, m *metrics.Metrics, log *slog.Logger) *Controller {
	return &Controller{cfg: cfg, prom: p, px: px, ams: ams, store: store, metrics: m, log: log}
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
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) {
	now := time.Now()
	snap, gaming, err := c.observe(ctx)
	if err != nil {
		c.log.Error("observe failed", "err", err)
		c.metrics.Update(metrics.Sample{Mode: string(snap.Mode), Tick: now.Unix(), OK: false})
		return
	}

	plan := Decide(snap, c.cfg, now)
	c.log.Info("decision",
		"mode", snap.Mode, "next", plan.NextMode,
		"surplus", snap.Surplus, "surplusRaw", snap.SurplusRaw, "soc", snap.SoC,
		"nodeUp", snap.NodeUp, "gaming", gaming, "reason", plan.Reason)
	sample := metrics.Sample{
		Surplus: snap.Surplus, SurplusRaw: snap.SurplusRaw, SoC: snap.SoC,
		NodeUp: snap.NodeUp, Gaming: gaming, Mode: string(plan.NextMode), Tick: now.Unix(), OK: true,
	}
	c.metrics.Update(sample)

	if c.cfg.DryRun == config.DryRunAlert {
		c.reconcileAlertOnly(ctx, snap)
		return
	}
	if isNoop(plan, snap) {
		return
	}
	if c.cfg.DryRun == config.DryRunLog {
		c.logPlan(plan)
		return
	}
	if err := c.execute(ctx, plan, snap); err != nil {
		c.log.Error("execute failed", "err", err)
		sample.Mode, sample.OK = string(snap.Mode), false
		c.metrics.Update(sample)
	}
}

// observe gathers the snapshot and reports whether a gaming guest is running.
func (c *Controller) observe(ctx context.Context) (Snapshot, bool, error) {
	st, err := c.store.Load(ctx)
	if err != nil {
		return Snapshot{}, false, err
	}
	reading, err := c.prom.Read(ctx, c.cfg.Prometheus)
	if err != nil {
		return Snapshot{Mode: st.Mode}, false, err
	}
	nodeUp, err := c.px.NodeUp(ctx, c.cfg.Proxmox.Node)
	if err != nil {
		return Snapshot{Mode: st.Mode}, false, err
	}
	var guests []proxmox.Guest
	if nodeUp {
		if guests, err = c.px.Guests(ctx, c.cfg.Proxmox.Node); err != nil {
			return Snapshot{Mode: st.Mode}, false, err
		}
	}
	snap := Snapshot{
		Surplus:    reading.Surplus,
		SurplusRaw: reading.SurplusRaw,
		SoC:        reading.SoC,
		NodeUp:     nodeUp,
		Guests:     guests,
		Mode:       st.Mode,
		StoppedSet: st.Stopped,
		GraceSince: st.GraceSince,
	}
	return snap, nodeUp && gamingActive(guests, c.cfg.Guests.GamingGuard), nil
}

func isNoop(p Plan, snap Snapshot) bool {
	return p.NextMode == snap.Mode && p.GraceSince == snap.GraceSince &&
		!p.Poweroff && !p.Wake && !p.Silence && !p.Unsilence &&
		len(p.Migrate) == 0 && len(p.Stop) == 0 && len(p.Start) == 0
}

func (c *Controller) logPlan(p Plan) {
	c.log.Info("[dry-run] would act",
		"migrate", guestIDs(p.Migrate), "stop", guestIDs(p.Stop), "start", refIDs(p.Start),
		"poweroff", p.Poweroff, "wake", p.Wake, "silence", p.Silence, "unsilence", p.Unsilence,
		"nextMode", p.NextMode)
}

// execute applies the plan in a fixed, safe order and persists the resulting state.
func (c *Controller) execute(ctx context.Context, p Plan, snap Snapshot) error {
	st := state.State{Mode: snap.Mode, Stopped: snap.StoppedSet, GraceSince: p.GraceSince}

	if len(p.Migrate) > 0 {
		if err := c.migrateAll(ctx, p.Migrate); err != nil {
			return err
		}
	}
	if len(p.Stop) > 0 {
		stopped, err := c.stopAll(ctx, p.Stop)
		st.Stopped = stopped
		if err != nil {
			_ = c.store.Save(ctx, st) // persist whatever we managed to stop
			return err
		}
	}
	if p.Silence {
		if err := c.reconcileSilences(ctx, true); err != nil {
			return err
		}
	}
	if p.Wake {
		if err := c.wake(ctx); err != nil {
			return err
		}
	}
	if len(p.Start) > 0 {
		if err := c.startAll(ctx, p.Start); err != nil {
			return err
		}
		st.Stopped = nil
	}
	if p.Poweroff {
		c.log.Warn("powering off node", "node", c.cfg.Proxmox.Node)
		if err := c.px.ShutdownNode(ctx, c.cfg.Proxmox.Node); err != nil {
			return err
		}
	}
	if p.Unsilence {
		if err := c.reconcileSilences(ctx, false); err != nil {
			return err
		}
	}

	st.Mode = p.NextMode
	st.GraceSince = p.GraceSince
	return c.store.Save(ctx, st)
}

// migrateAll spreads the guests across target nodes round-robin, falling back to the
// next target on failure (e.g. insufficient resources).
func (c *Controller) migrateAll(ctx context.Context, guests []proxmox.Guest) error {
	targets := c.cfg.Proxmox.TargetNodes
	for i, g := range guests {
		ordered := rotate(targets, i) // equal split: guest i prefers target i%len
		if err := c.migrateOne(ctx, g, ordered); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) migrateOne(ctx context.Context, g proxmox.Guest, targets []string) error {
	var lastErr error
	for _, target := range targets {
		mctx, cancel := context.WithTimeout(ctx, c.cfg.Proxmox.MigrateTimeout.Duration)
		upid, err := c.px.Migrate(mctx, c.cfg.Proxmox.Node, g, target)
		if err == nil {
			err = c.px.WaitTask(mctx, c.cfg.Proxmox.Node, upid)
		}
		cancel()
		if err == nil {
			c.log.Info("migrated guest", "vmid", g.VMID, "type", g.Type, "target", target)
			return nil
		}
		c.log.Warn("migration failed, trying next target", "vmid", g.VMID, "target", target, "err", err)
		lastErr = err
	}
	return lastErr
}

func (c *Controller) stopAll(ctx context.Context, guests []proxmox.Guest) ([]state.GuestRef, error) {
	var stopped []proxmox.Guest
	for _, g := range guests {
		sctx, cancel := context.WithTimeout(ctx, c.cfg.Proxmox.StopTimeout.Duration)
		upid, err := c.px.Stop(sctx, c.cfg.Proxmox.Node, g)
		if err == nil {
			err = c.px.WaitTask(sctx, c.cfg.Proxmox.Node, upid)
		}
		cancel()
		if err != nil {
			return refs(stopped), err
		}
		c.log.Info("stopped guest", "vmid", g.VMID, "type", g.Type)
		stopped = append(stopped, g)
	}
	return refs(stopped), nil
}

func (c *Controller) startAll(ctx context.Context, guests []state.GuestRef) error {
	for _, ref := range guests {
		g := proxmox.Guest{VMID: ref.VMID, Type: proxmox.GuestType(ref.Type)}
		upid, err := c.px.Start(ctx, c.cfg.Proxmox.Node, g)
		if err == nil {
			err = c.px.WaitTask(ctx, c.cfg.Proxmox.Node, upid)
		}
		if err != nil {
			c.log.Warn("failed to start guest", "vmid", ref.VMID, "err", err)
			continue // best-effort; one stuck guest shouldn't block the others
		}
		c.log.Info("started guest", "vmid", ref.VMID, "type", ref.Type)
	}
	return nil
}

func (c *Controller) wake(ctx context.Context) error {
	c.log.Info("sending Wake-on-LAN", "mac", c.cfg.Proxmox.MAC, "node", c.cfg.Proxmox.Node)
	if err := wol.Send(c.cfg.Proxmox.MAC, c.cfg.Proxmox.WoLBroadcastAddr); err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, c.cfg.Proxmox.WakeTimeout.Duration)
	defer cancel()
	return c.px.WaitNodeUp(wctx, c.cfg.Proxmox.Node)
}

// silenceTTL is how long each Alertmanager silence lasts before it self-expires. The
// controller extends a silence before this lapses, so an arbitrarily long shutdown stays
// covered; if the watchdog itself dies, its silences self-expire within one TTL instead of
// lingering. silenceRefresh is how close to expiry a silence may drift before it's extended.
const (
	silenceTTL     = 24 * time.Hour
	silenceRefresh = time.Hour
)

// reconcileAlertOnly drives Alertmanager silences purely from p1's real power state
// (DryRunAlert mode): silence when the node is down, drop them when it's back. It takes no
// Proxmox actions, so it's safe to run before Wake-on-LAN is ready.
func (c *Controller) reconcileAlertOnly(ctx context.Context, snap Snapshot) {
	if err := c.reconcileSilences(ctx, !snap.NodeUp); err != nil {
		c.log.Error("alert-only: reconcile silences", "err", err)
	}
}

// reconcileSilences makes the energy-watchdog silences in every configured Alertmanager
// match the desired set: the configured silences when silence is true (p1 down), or none
// when false (p1 up). It never persists silence ids - it recognises its own silences by
// createdBy on each Alertmanager - so a lost or stale ConfigMap can't orphan them, and any
// orphans from an earlier run are cleaned up here. Each Alertmanager is reconciled
// independently, so one being unreachable doesn't disturb the others.
func (c *Controller) reconcileSilences(ctx context.Context, silence bool) error {
	var desired []config.Silence
	if silence {
		desired = c.cfg.Alertmanager.Silences
	}
	var firstErr error
	for _, url := range c.cfg.Alertmanager.URLs {
		if err := c.reconcileSilencesAt(ctx, url, desired); err != nil {
			c.log.Error("reconcile silences", "url", url, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// reconcileSilencesAt converges one Alertmanager to `desired`: it creates any desired
// silence that's missing, extends one that's drifting toward expiry, and deletes every
// other silence of ours (duplicates left by the old create-every-tick behaviour, silences
// from a since-changed config, or all of them when p1 is back up). Coverage is never
// dropped mid-run: creates and extensions happen before any delete.
func (c *Controller) reconcileSilencesAt(ctx context.Context, url string, desired []config.Silence) error {
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
		if err := am.Delete(ctx, s.ID); err != nil {
			return err
		}
		c.log.Info("removed alertmanager silence", "url", url, "id", s.ID)
	}
	return nil
}

func rotate[T any](s []T, n int) []T {
	if len(s) == 0 {
		return s
	}
	n %= len(s)
	out := make([]T, 0, len(s))
	out = append(out, s[n:]...)
	out = append(out, s[:n]...)
	return out
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
