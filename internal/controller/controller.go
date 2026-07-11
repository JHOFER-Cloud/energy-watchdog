// Package controller runs the reconcile loop: observe solar + Proxmox, Decide a Plan,
// then (unless dry-run) execute it and persist state.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	plan := Decide(snap, c.cfg)
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
	if isNoop(plan, snap.Mode) {
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
	}
	return snap, nodeUp && gamingActive(guests, c.cfg.Guests.GamingGuard), nil
}

func isNoop(p Plan, current state.Mode) bool {
	return p.NextMode == current && !p.Poweroff && !p.Wake && !p.Silence && !p.Unsilence &&
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
	st := state.State{Mode: snap.Mode, Stopped: snap.StoppedSet}
	if refs, err := c.currentSilences(ctx); err == nil {
		st.Silences = refs
	}

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
	if p.Silence && len(st.Silences) == 0 {
		refs, err := c.silenceAll(ctx)
		st.Silences = refs
		if err != nil {
			_ = c.store.Save(ctx, st) // persist whatever we created so wake can clean it up
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
	if p.Unsilence && len(st.Silences) > 0 {
		if err := c.unsilenceAll(ctx, st.Silences); err != nil {
			return err
		}
		st.Silences = nil
	}

	st.Mode = p.NextMode
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

// currentSilences reloads the persisted silence refs so execute doesn't double-create.
func (c *Controller) currentSilences(ctx context.Context) ([]state.SilenceRef, error) {
	st, err := c.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	return st.Silences, nil
}

// silenceTTL is how long each Alertmanager silence lasts. reconcileAlertOnly refreshes
// them before this lapses, so a shutdown longer than the TTL stays covered; if the
// watchdog itself dies, the silences self-expire within one TTL instead of lingering.
const silenceTTL = 24 * time.Hour

// reconcileAlertOnly drives Alertmanager silences purely from p1's real power state
// (DryRunAlert mode): silence when the node is down, drop them when it is back, and
// refresh before the TTL lapses so an indefinitely-long shutdown stays covered. It takes
// no Proxmox actions, so it's safe to run before Wake-on-LAN is ready.
func (c *Controller) reconcileAlertOnly(ctx context.Context, snap Snapshot) {
	st, err := c.store.Load(ctx)
	if err != nil {
		c.log.Error("alert-only: load state", "err", err)
		return
	}
	if snap.NodeUp {
		if len(st.Silences) > 0 {
			if err := c.unsilenceAll(ctx, st.Silences); err != nil {
				c.log.Error("alert-only: unsilence", "err", err)
				return
			}
			st.Silences, st.SilencedAt = nil, 0
			if err := c.store.Save(ctx, st); err != nil {
				c.log.Error("alert-only: save", "err", err)
			}
		}
		return
	}
	// p1 is down: make sure a fresh silence set matching the current config exists.
	now := time.Now()
	want := silenceFingerprint(c.cfg.Alertmanager.URLs, c.cfg.Alertmanager.Silences)
	stale := len(st.Silences) > 0 && now.Unix()-st.SilencedAt > int64((silenceTTL-time.Hour).Seconds())
	drifted := st.SilencesFingerprint != want // silence config changed since we last applied it
	if len(st.Silences) > 0 && !stale && !drifted {
		return
	}
	old := st.Silences
	refs, err := c.silenceAll(ctx)
	st.Silences, st.SilencedAt = refs, now.Unix()
	if err != nil {
		st.SilencesFingerprint = "" // partial set; force a retry on the next tick
		c.log.Error("alert-only: silence", "err", err)
	} else {
		st.SilencesFingerprint = want
	}
	if err := c.store.Save(ctx, st); err != nil {
		c.log.Error("alert-only: save", "err", err)
	}
	if len(old) > 0 { // a fresh set was created above; retire the superseded one
		if err := c.unsilenceAll(ctx, old); err != nil {
			c.log.Error("alert-only: retire stale silences", "err", err)
		}
	}
}

// silenceFingerprint is a stable digest of the desired silence set (the Alertmanager
// URLs and every matcher). reconcileAlertOnly recreates silences when it changes, so a
// config edit takes effect on the next tick instead of lingering until the TTL refresh.
func silenceFingerprint(urls []string, silences []config.Silence) string {
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(struct {
		URLs     []string         `json:"urls"`
		Silences []config.Silence `json:"silences"`
	}{urls, silences})
	return hex.EncodeToString(h.Sum(nil))
}

// silenceAll creates every configured silence in every configured Alertmanager and
// returns a ref per (alertmanager, silence) so each can be deleted from the right one.
func (c *Controller) silenceAll(ctx context.Context) ([]state.SilenceRef, error) {
	var refs []state.SilenceRef
	for _, url := range c.cfg.Alertmanager.URLs {
		am := c.ams[url]
		for _, s := range c.cfg.Alertmanager.Silences {
			id, err := am.Create(ctx, s.Matchers, c.cfg.Alertmanager.Comment, silenceTTL, time.Now())
			if err != nil {
				return refs, err // return what we have so far so they can be cleaned up on wake
			}
			refs = append(refs, state.SilenceRef{URL: url, ID: id})
			c.log.Info("created alertmanager silence", "url", url, "id", id)
		}
	}
	return refs, nil
}

// unsilenceAll deletes each persisted silence from the Alertmanager it was created in.
func (c *Controller) unsilenceAll(ctx context.Context, refs []state.SilenceRef) error {
	for _, ref := range refs {
		am := c.ams[ref.URL]
		if am == nil {
			c.log.Warn("no client for silenced alertmanager, skipping delete", "url", ref.URL, "id", ref.ID)
			continue
		}
		if err := am.Delete(ctx, ref.ID); err != nil {
			return err
		}
		c.log.Info("removed alertmanager silence", "url", ref.URL, "id", ref.ID)
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
