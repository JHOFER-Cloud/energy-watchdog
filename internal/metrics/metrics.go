// Package metrics exposes the watchdog's current view as Prometheus text so it can be
// scraped and dashboarded.
package metrics

import (
	"fmt"
	"net/http"
	"sync"
)

// Metrics holds the latest reconcile snapshot for /metrics.
type Metrics struct {
	mu         sync.Mutex
	surplus    float64
	surplusRaw float64
	soc        float64
	nodeUp     bool
	gaming     bool
	manualShed bool
	manualOn   bool
	dryRun     bool
	// powerReqOK is nil until a request has been attempted, so a deployment that
	// doesn't delegate p1's power exports nothing rather than a misleading 1.
	powerReqOK *bool
	mode       string
	lastTick   int64
	lastOK     bool

	// Configured thresholds, exported so the dashboard draws decision lines from the
	// live config instead of hard-coded numbers that drift when you tune.
	cfgHeadroom   float64
	cfgShedBelow  float64
	cfgMinBattery float64
	cfgSet        bool
}

// New returns an empty metrics holder.
func New(dryRun bool) *Metrics { return &Metrics{dryRun: dryRun, mode: "unknown"} }

// SetThresholds records the configured decision thresholds (call once at startup).
func (m *Metrics) SetThresholds(headroom, shedBelow, minBattery float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfgHeadroom, m.cfgShedBelow, m.cfgMinBattery = headroom, shedBelow, minBattery
	m.cfgSet = true
}

// Sample is what one reconcile observed, and when. Mode and success aren't known until
// apply has run - see SetOutcome.
type Sample struct {
	Surplus    float64
	SurplusRaw float64
	SoC        float64
	NodeUp     bool
	Gaming     bool
	ManualShed bool
	ManualOn   bool
	Tick       int64
}

// Update records the observation from the latest reconcile.
func (m *Metrics) Update(s Sample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.surplus, m.surplusRaw, m.soc = s.Surplus, s.SurplusRaw, s.SoC
	m.nodeUp, m.gaming = s.NodeUp, s.Gaming
	m.manualShed, m.manualOn = s.ManualShed, s.ManualOn
	m.lastTick = s.Tick
}

// SetPowerRequestOK records whether the last power request to nut-dog landed. p1's power
// runs entirely through that call now, so a rejected token or a wrong load name has to be
// visible - it fails silently otherwise, with every other gauge still green.
func (m *Metrics) SetPowerRequestOK(ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.powerReqOK = &ok
}

// SetOutcome records the mode in force after apply and whether apply succeeded. Separate
// from Update because apply can run for minutes, and the gauges must not wait on it.
func (m *Metrics) SetOutcome(mode string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode, m.lastOK = mode, ok
}

// MarkStale records that a reconcile tick failed, without overwriting the last good
// reading. The surplus/mode/node gauges keep their previous values so a transient observe
// error (e.g. a single Proxmox 502) doesn't flap them to zero on the dashboard; only the
// tick time and the success flag move, so failures are still observable via
// energy_watchdog_last_reconcile_success.
func (m *Metrics) MarkStale(tick int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastTick, m.lastOK = tick, false
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Handler renders the metrics in Prometheus exposition format.
func (m *Metrics) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP energy_watchdog_surplus_watts Averaged solar production minus consumption.\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_surplus_watts gauge\n")
		fmt.Fprintf(w, "energy_watchdog_surplus_watts %g\n", m.surplus)
		fmt.Fprintf(w, "# HELP energy_watchdog_surplus_raw_watts Instantaneous production minus consumption (not used for decisions).\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_surplus_raw_watts gauge\n")
		fmt.Fprintf(w, "energy_watchdog_surplus_raw_watts %g\n", m.surplusRaw)
		fmt.Fprintf(w, "# HELP energy_watchdog_battery_percent Averaged battery state of charge.\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_battery_percent gauge\n")
		fmt.Fprintf(w, "energy_watchdog_battery_percent %g\n", m.soc)
		fmt.Fprintf(w, "# HELP energy_watchdog_node_up Whether the managed node is online.\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_node_up gauge\n")
		fmt.Fprintf(w, "energy_watchdog_node_up %g\n", b2f(m.nodeUp))
		fmt.Fprintf(w, "# HELP energy_watchdog_gaming_active Whether a gaming-guard guest is running.\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_gaming_active gauge\n")
		fmt.Fprintf(w, "energy_watchdog_gaming_active %g\n", b2f(m.gaming))
		fmt.Fprintf(w, "# HELP energy_watchdog_manual_shed Whether the node is held shed by hand rather than by the solar signal.\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_manual_shed gauge\n")
		fmt.Fprintf(w, "energy_watchdog_manual_shed %g\n", b2f(m.manualShed))
		// A hold-on that nobody clears keeps p1 on grid power every night, so this gauge is
		// meant to be alerted on: min_over_time(energy_watchdog_manual_on[6h]) == 1.
		fmt.Fprintf(w, "# HELP energy_watchdog_manual_on Whether the node is held on by hand rather than by the solar signal.\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_manual_on gauge\n")
		fmt.Fprintf(w, "energy_watchdog_manual_on %g\n", b2f(m.manualOn))
		if m.powerReqOK != nil {
			fmt.Fprintf(w, "# HELP energy_watchdog_power_request_success Whether the last power request to nut-dog succeeded.\n")
			fmt.Fprintf(w, "# TYPE energy_watchdog_power_request_success gauge\n")
			fmt.Fprintf(w, "energy_watchdog_power_request_success %g\n", b2f(*m.powerReqOK))
		}
		fmt.Fprintf(w, "# HELP energy_watchdog_dry_run Whether the watchdog is in dry-run mode.\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_dry_run gauge\n")
		fmt.Fprintf(w, "energy_watchdog_dry_run %g\n", b2f(m.dryRun))
		// A manual shed deliberately does NOT get its own mode label: dashboards and alerts
		// key off these three values. Join energy_watchdog_manual_shed for the "why".
		fmt.Fprintf(w, "# HELP energy_watchdog_mode Current mode (1 for the active mode).\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_mode gauge\n")
		for _, mode := range []string{"running", "shed", "gaming"} {
			fmt.Fprintf(w, "energy_watchdog_mode{mode=%q} %g\n", mode, b2f(m.mode == mode))
		}
		fmt.Fprintf(w, "# HELP energy_watchdog_last_reconcile_timestamp_seconds Unix time of the last reconcile.\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_last_reconcile_timestamp_seconds gauge\n")
		fmt.Fprintf(w, "energy_watchdog_last_reconcile_timestamp_seconds %d\n", m.lastTick)
		fmt.Fprintf(w, "# HELP energy_watchdog_last_reconcile_success Whether the last reconcile succeeded.\n")
		fmt.Fprintf(w, "# TYPE energy_watchdog_last_reconcile_success gauge\n")
		fmt.Fprintf(w, "energy_watchdog_last_reconcile_success %g\n", b2f(m.lastOK))
		if m.cfgSet {
			fmt.Fprintf(w, "# HELP energy_watchdog_config_headroom_watts Surplus required to wake (wake threshold).\n")
			fmt.Fprintf(w, "# TYPE energy_watchdog_config_headroom_watts gauge\n")
			fmt.Fprintf(w, "energy_watchdog_config_headroom_watts %g\n", m.cfgHeadroom)
			fmt.Fprintf(w, "# HELP energy_watchdog_config_shed_below_watts Surplus below which p1 is shed (shed threshold).\n")
			fmt.Fprintf(w, "# TYPE energy_watchdog_config_shed_below_watts gauge\n")
			fmt.Fprintf(w, "energy_watchdog_config_shed_below_watts %g\n", m.cfgShedBelow)
			fmt.Fprintf(w, "# HELP energy_watchdog_config_min_battery_percent Minimum battery charge to allow a wake.\n")
			fmt.Fprintf(w, "# TYPE energy_watchdog_config_min_battery_percent gauge\n")
			fmt.Fprintf(w, "energy_watchdog_config_min_battery_percent %g\n", m.cfgMinBattery)
		}
	}
}
