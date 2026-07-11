// Package config loads and validates the energy-watchdog configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level energy-watchdog configuration.
type Config struct {
	// Interval is how often the reconcile loop runs.
	Interval Duration `yaml:"interval"`
	// DryRun logs the actions the watchdog would take without performing any of them.
	DryRun bool `yaml:"dryRun"`
	// MetricsAddr is the listen address for the /metrics and /healthz endpoints.
	MetricsAddr string `yaml:"metricsAddr"`

	Prometheus   Prometheus   `yaml:"prometheus"`
	Proxmox      Proxmox      `yaml:"proxmox"`
	Guests       Guests       `yaml:"guests"`
	Alertmanager Alertmanager `yaml:"alertmanager"`
	State        State        `yaml:"state"`
}

// Prometheus configures the solar-surplus decision queries.
type Prometheus struct {
	URL string `yaml:"url"`
	// Window is the PromQL range used for avg_over_time, e.g. "30m". Averaging over a
	// window is what keeps a kitchen burst from instantly triggering a shutdown.
	Window string `yaml:"window"`
	// HeadroomWatts is the surplus (production - consumption) the system must clear,
	// sustained over Window, before p1 is woken. ~1kW covers p1+p2+p3 spinning up.
	HeadroomWatts float64 `yaml:"headroomWatts"`
	// ShedBelowWatts is the surplus threshold below which p1 is shed. Default 0:
	// shed once consumption exceeds production. The gap to HeadroomWatts is the
	// hysteresis band that prevents flapping around the break-even point.
	ShedBelowWatts float64 `yaml:"shedBelowWatts"`
	// MinBatteryPercent gates waking: don't count surplus as "wake" unless the
	// battery is at least this charged, so we never wake into a battery about to deplete.
	MinBatteryPercent float64 `yaml:"minBatteryPercent"`
	// PowerScale multiplies the production/consumption metrics to convert them to watts,
	// so the *Watts thresholds mean what they say. The sonnenbatterie metrics are in
	// milliwatts, so use 0.001. Defaults to 1 (metric already in watts).
	PowerScale float64 `yaml:"powerScale"`

	ProductionMetric  string `yaml:"productionMetric"`
	ConsumptionMetric string `yaml:"consumptionMetric"`
	BatteryMetric     string `yaml:"batteryMetric"`
}

// Proxmox configures the cluster API client and the host under management.
type Proxmox struct {
	// Endpoint must stay reachable while the managed node is off. The proxy fronting
	// all nodes works (it routes elsewhere), as does any single online node. Don't
	// point it at the node being powered off: its own API goes away with it, but
	// another node still reports it offline and can manage its guests / shut it down.
	Endpoint string `yaml:"endpoint"`
	// TokenID / TokenSecret are usually injected via the PROXMOX_TOKEN_ID /
	// PROXMOX_TOKEN_SECRET env vars (from the 1Password-synced secret), which override
	// these. TokenID format: "user@realm!tokenname".
	TokenID     string `yaml:"tokenID"`
	TokenSecret string `yaml:"tokenSecret"`
	// CACertPath trusts an extra CA (the internal jhc-ca) on top of the system roots,
	// so TLS to the Proxmox proxy verifies instead of being skipped.
	CACertPath         string `yaml:"caCertPath"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify"`

	// Node is the managed host that gets powered down ("p1").
	Node string `yaml:"node"`
	// MAC is the NIC of Node, used to wake it via Wake-on-LAN.
	MAC string `yaml:"mac"`
	// WoLBroadcastAddr is where the magic packet is sent. Defaults to
	// "255.255.255.255:9"; a subnet-directed address ("10.1.1.255:9") is often more
	// reliable from a hostNetwork pod with several interfaces.
	WoLBroadcastAddr string `yaml:"wolBroadcastAddr"`
	// TargetNodes are the destinations the migrate guests are spread across.
	TargetNodes []string `yaml:"targetNodes"`

	MigrateTimeout Duration `yaml:"migrateTimeout"`
	StopTimeout    Duration `yaml:"stopTimeout"`
	WakeTimeout    Duration `yaml:"wakeTimeout"`
}

// Guests classifies the guests on the managed node. Each list entry is either an
// integer VMID/CTID (601) or an inclusive range string ("600-699"). A range is a
// membership test against the ids that actually exist on the node, not a demand
// that every id in it exists.
type Guests struct {
	// Migrate guests are live-migrated off the node before it is powered off, so the
	// clusters keep these nodes. They are never migrated back automatically.
	Migrate IDSet `yaml:"migrate"`
	// Stop guests are gracefully stopped, recorded, and restarted at "good morning".
	Stop IDSet `yaml:"stop"`
	// GamingGuard guests veto the host power-off while any of them is running.
	GamingGuard IDSet `yaml:"gamingGuard"`
}

// Alertmanager configures the silences created while the node is down. Physical p1
// hosts guests in more than one cluster, so URLs lists every Alertmanager that needs
// silencing. Each Silence is created separately (per label dimension) so p1 is silenced
// precisely instead of with one broad match.
type Alertmanager struct {
	URLs     []string  `yaml:"urls"`
	Comment  string    `yaml:"comment"`
	Silences []Silence `yaml:"silences"`
}

// Silence is one Alertmanager silence; its matchers are AND-ed together.
type Silence struct {
	Matchers []Matcher `yaml:"matchers"`
}

// Matcher is an Alertmanager v2 silence matcher.
type Matcher struct {
	Name    string `yaml:"name"`
	Value   string `yaml:"value"`
	IsRegex bool   `yaml:"isRegex"`
}

// State configures where the controller persists which guests it stopped and the
// current mode. In-cluster it uses a ConfigMap; locally it falls back to a file.
type State struct {
	ConfigMapName string `yaml:"configMapName"`
	FilePath      string `yaml:"filePath"`
}

// Load reads, env-overrides, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if id := os.Getenv("PROXMOX_TOKEN_ID"); id != "" {
		c.Proxmox.TokenID = id
	}
	if secret := os.Getenv("PROXMOX_TOKEN_SECRET"); secret != "" {
		c.Proxmox.TokenSecret = secret
	}

	c.defaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.Interval.Duration == 0 {
		c.Interval = Duration{time.Minute}
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":9333"
	}
	if c.Prometheus.Window == "" {
		c.Prometheus.Window = "30m"
	}
	if c.Proxmox.MigrateTimeout.Duration == 0 {
		c.Proxmox.MigrateTimeout = Duration{15 * time.Minute}
	}
	if c.Proxmox.StopTimeout.Duration == 0 {
		c.Proxmox.StopTimeout = Duration{5 * time.Minute}
	}
	if c.Proxmox.WakeTimeout.Duration == 0 {
		c.Proxmox.WakeTimeout = Duration{5 * time.Minute}
	}
	if c.Proxmox.WoLBroadcastAddr == "" {
		c.Proxmox.WoLBroadcastAddr = "255.255.255.255:9"
	}
	if c.State.ConfigMapName == "" {
		c.State.ConfigMapName = "energy-watchdog-state"
	}
	if c.State.FilePath == "" {
		c.State.FilePath = "/tmp/energy-watchdog-state.json"
	}
}

func (c *Config) validate() error {
	switch {
	case c.Prometheus.URL == "":
		return fmt.Errorf("prometheus.url is required")
	case c.Prometheus.ProductionMetric == "":
		return fmt.Errorf("prometheus.productionMetric is required")
	case c.Prometheus.ConsumptionMetric == "":
		return fmt.Errorf("prometheus.consumptionMetric is required")
	case c.Proxmox.Endpoint == "":
		return fmt.Errorf("proxmox.endpoint is required")
	case c.Proxmox.Node == "":
		return fmt.Errorf("proxmox.node is required")
	case c.Proxmox.MAC == "":
		return fmt.Errorf("proxmox.mac is required (needed for Wake-on-LAN)")
	case len(c.Proxmox.TargetNodes) == 0:
		return fmt.Errorf("proxmox.targetNodes must list at least one migration destination")
	case c.Proxmox.TokenID == "" || c.Proxmox.TokenSecret == "":
		return fmt.Errorf("proxmox token missing (set proxmox.tokenID/tokenSecret or PROXMOX_TOKEN_ID/PROXMOX_TOKEN_SECRET)")
	case len(c.Alertmanager.URLs) > 0 && len(c.Alertmanager.Silences) == 0:
		return fmt.Errorf("alertmanager.silences must be set when alertmanager.urls is configured")
	}
	if c.Prometheus.HeadroomWatts < c.Prometheus.ShedBelowWatts {
		return fmt.Errorf("prometheus.headroomWatts (%v) must be >= shedBelowWatts (%v) for stable hysteresis",
			c.Prometheus.HeadroomWatts, c.Prometheus.ShedBelowWatts)
	}
	for _, o := range []struct {
		a, b string
		x, y IDSet
	}{
		{"migrate", "stop", c.Guests.Migrate, c.Guests.Stop},
		{"migrate", "gamingGuard", c.Guests.Migrate, c.Guests.GamingGuard},
		{"stop", "gamingGuard", c.Guests.Stop, c.Guests.GamingGuard},
	} {
		if lo, hi, ok := o.x.Overlap(o.y); ok {
			return fmt.Errorf("guests.%s and guests.%s overlap on id range %d-%d", o.a, o.b, lo, hi)
		}
	}
	return nil
}

// Duration is a yaml-friendly time.Duration parsed from strings like "30m".
type Duration struct{ time.Duration }

// UnmarshalYAML parses a duration string such as "60s" or "30m".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// IDSet is a set of VMIDs/CTIDs expressed as individual ids and inclusive ranges.
type IDSet struct {
	ranges []idRange
}

type idRange struct{ lo, hi int }

// UnmarshalYAML accepts a list whose entries are ints (601) or range strings ("600-699").
func (s *IDSet) UnmarshalYAML(value *yaml.Node) error {
	var nodes []yaml.Node
	if err := value.Decode(&nodes); err != nil {
		return fmt.Errorf("guest id list must be a sequence: %w", err)
	}
	for _, n := range nodes {
		var i int
		if err := n.Decode(&i); err == nil {
			s.ranges = append(s.ranges, idRange{i, i})
			continue
		}
		var str string
		if err := n.Decode(&str); err != nil {
			return fmt.Errorf("guest id %q is neither an int nor a range string", n.Value)
		}
		lo, hi, err := parseRange(str)
		if err != nil {
			return err
		}
		s.ranges = append(s.ranges, idRange{lo, hi})
	}
	return nil
}

func parseRange(s string) (lo, hi int, err error) {
	s = strings.TrimSpace(s)
	// A bare number string ("700") is a single id, not a range.
	if !strings.Contains(s, "-") {
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid id %q: want a number or \"lo-hi\" range", s)
		}
		return n, n, nil
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid id range %q: want \"lo-hi\"", s)
	}
	lo, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid id range %q: %w", s, err)
	}
	hi, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid id range %q: %w", s, err)
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("invalid id range %q: lo > hi", s)
	}
	return lo, hi, nil
}

// Contains reports whether id falls in any configured range.
func (s IDSet) Contains(id int) bool {
	for _, r := range s.ranges {
		if id >= r.lo && id <= r.hi {
			return true
		}
	}
	return false
}

// Overlap reports the first overlapping id span between two sets, if any.
func (s IDSet) Overlap(other IDSet) (lo, hi int, ok bool) {
	for _, a := range s.ranges {
		for _, b := range other.ranges {
			l, h := max(a.lo, b.lo), min(a.hi, b.hi)
			if l <= h {
				return l, h, true
			}
		}
	}
	return 0, 0, false
}
