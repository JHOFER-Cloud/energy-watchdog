// Package config loads and validates the energy-watchdog configuration.
package config

import (
	"fmt"
	"net/url"
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
	// DryRun controls how much the watchdog actually does: full operation, log-only, or
	// alert-only (suppress the noise a down p1 causes - Alertmanager silences and its
	// replication jobs - from p1's real power state, without moving any guests).
	DryRun DryRunMode `yaml:"dryRun"`
	// MetricsAddr is the listen address for the /metrics and /healthz endpoints.
	MetricsAddr string `yaml:"metricsAddr"`
	// GamingGrace is how long p1 may stay up in a gaming session with no gaming guest
	// running before it is powered off. It covers waking the host to start a VM (which
	// won't autostart) and GPU-passthrough/VM reboots mid-session, so a short gap doesn't
	// cut the session short. Default 10m.
	GamingGrace Duration `yaml:"gamingGrace"`

	// PowerAPI delegates p1's power to nut-dog. Unset means the watchdog powers p1
	// itself over Proxmox (WoL relay + node shutdown), which is what local runs use.
	PowerAPI *PowerAPI `yaml:"powerAPI"`

	Prometheus   Prometheus   `yaml:"prometheus"`
	Proxmox      Proxmox      `yaml:"proxmox"`
	Guests       Guests       `yaml:"guests"`
	Alertmanager Alertmanager `yaml:"alertmanager"`
	State        State        `yaml:"state"`
	SelfService  SelfService  `yaml:"selfService"`
}

// SelfService configures the desktop-VM self-service UI (JHC-548). It is off unless addr is
// set. The UI only ever records intent; the reconcile loop remains the sole owner of p1.
type SelfService struct {
	// Addr is the listen address for the UI and its API. Empty disables the whole feature.
	Addr string `yaml:"addr"`
	// IssuerURL is the authentik OIDC issuer. The app runs the authorization-code flow
	// against it itself, the same way every other service in the fleet does.
	IssuerURL string `yaml:"issuerURL"`
	// ClientID / ClientSecret identify this app to authentik. The secret is usually injected
	// via SELFSERVICE_CLIENT_SECRET, which overrides whatever is in the file.
	ClientID     string `yaml:"clientID"`
	ClientSecret string `yaml:"clientSecret"`
	// SessionKey signs the session cookie. Injected via SELFSERVICE_SESSION_KEY. Changing it
	// invalidates everyone's session, which is the intended way to force a re-login.
	SessionKey string `yaml:"sessionKey"`
	// ExternalURL is how a browser reaches this UI. The OIDC redirect is built from it, so it
	// has to match the redirect URI registered on the authentik provider.
	ExternalURL string `yaml:"externalURL"`
	// SessionTTL is how long a login lasts. Default 12h.
	SessionTTL Duration `yaml:"sessionTTL"`
	// AdminGroups may toggle the manual shed. Membership comes from the id_token.
	AdminGroups []string `yaml:"adminGroups"`
	// VMs are the desktop VMs offered, each with the groups allowed to control it.
	VMs []SelfServiceVM `yaml:"vms"`
}

// SelfServiceVM is one desktop VM and who may start it.
type SelfServiceVM struct {
	VMID int    `yaml:"vmid"`
	Name string `yaml:"name"`
	// Groups are authentik groups whose members may start this VM.
	Groups []string `yaml:"groups"`
	// StreamHost is the host the UI hands to Moonlight once the VM is up. Optional.
	StreamHost string `yaml:"streamHost"`
}

// minSessionKeyLen is the shortest session key worth signing with; anything shorter is a
// placeholder someone forgot to replace.
const minSessionKeyLen = 16

// Enabled reports whether the self-service UI should be served.
func (s SelfService) Enabled() bool { return s.Addr != "" }

// RedirectURL is the OIDC callback, derived from ExternalURL so the two can't disagree.
func (s SelfService) RedirectURL() string {
	return strings.TrimRight(s.ExternalURL, "/") + "/auth/callback"
}

// Allowed reports the VMs these groups may control.
func (s SelfService) Allowed(groups []string) []SelfServiceVM {
	has := make(map[string]bool, len(groups))
	for _, g := range groups {
		has[g] = true
	}
	var out []SelfServiceVM
	for _, vm := range s.VMs {
		for _, g := range vm.Groups {
			if has[g] {
				out = append(out, vm)
				break
			}
		}
	}
	return out
}

// NameOf is a VM's configured name, falling back to its id so a message never reads "VM  is
// using this GPU" for something the config forgot to name.
func (s SelfService) NameOf(vmid int) string {
	for _, vm := range s.VMs {
		if vm.VMID == vmid && vm.Name != "" {
			return vm.Name
		}
	}
	return "VM " + strconv.Itoa(vmid)
}

// IsAdmin reports whether any of these groups may toggle the manual shed.
func (s SelfService) IsAdmin(groups []string) bool {
	for _, g := range groups {
		for _, a := range s.AdminGroups {
			if g == a {
				return true
			}
		}
	}
	return false
}

// DryRunMode is how much of the plan the watchdog carries out.
type DryRunMode int

const (
	// DryRunFull performs everything (from yaml: false).
	DryRunFull DryRunMode = iota
	// DryRunLog logs the plan and touches nothing (from yaml: true).
	DryRunLog
	// DryRunAlert suppresses the noise a down p1 makes, driven by p1's actual power state:
	// Alertmanager silences and (unless manageReplication is off) the replication jobs
	// targeting it. Still no migrate/stop/poweroff/wake - no guest is moved and the host is
	// never powered. For running before WoL is ready, so a manual p1 shutdown still gets
	// silenced and doesn't mail about failed replication (from yaml: alert).
	DryRunAlert
)

func (m DryRunMode) String() string {
	switch m {
	case DryRunLog:
		return "log"
	case DryRunAlert:
		return "alert"
	default:
		return "full"
	}
}

// UnmarshalYAML accepts a bool (false=full, true=log) or the string "alert".
func (m *DryRunMode) UnmarshalYAML(value *yaml.Node) error {
	var b bool
	if err := value.Decode(&b); err == nil {
		if b {
			*m = DryRunLog
		} else {
			*m = DryRunFull
		}
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("dryRun must be true, false, or \"alert\"")
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "alert":
		*m = DryRunAlert
	case "true":
		*m = DryRunLog
	case "false":
		*m = DryRunFull
	default:
		return fmt.Errorf("dryRun: unknown value %q (want true, false, or alert)", s)
	}
	return nil
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

// PowerAPI points at nut-dog's power endpoint. nut-dog owns p1's power because it
// must work when the cluster doesn't; the watchdog only states what it wants.
type PowerAPI struct {
	URL  string `yaml:"url"`  // base URL, e.g. http://nut-dog.energy.svc.cluster.local:9335
	Load string `yaml:"load"` // nut-dog load name for the managed node
	// Token authenticates the request. Usually injected via POWER_API_TOKEN.
	Token string `yaml:"token"`
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
	// TargetNodes are the destinations the migrate guests are spread across. Unrelated to a
	// replication job's own "target" field, which is matched against Node, not this list.
	TargetNodes []string `yaml:"targetNodes"`
	// ManageReplication disables the cluster replication jobs that replicate *into* Node
	// while it is powered down, and re-enables them when it is back, so the other nodes stop
	// attempting (and mailing about) replication runs to a host that is off on purpose.
	// Unset means enabled; set it to false if the API token lacks VM.Replicate on the
	// replicated guests, which would otherwise make every reconcile log an error.
	ManageReplication *bool `yaml:"manageReplication"`

	MigrateTimeout Duration `yaml:"migrateTimeout"`
	StopTimeout    Duration `yaml:"stopTimeout"`
	WakeTimeout    Duration `yaml:"wakeTimeout"`
	// FreshBootWindow is how recently Node must have booted to count as powered on by hand:
	// Proxmox reports a node that is shutting down as online, and only its uptime tells the
	// two apart. It has to be longer than the uptime Node reports when it first shows up
	// online (cluster join is well after kernel boot) and shorter than the uptime it has when
	// the watchdog sheds it. Default 5m.
	FreshBootWindow Duration `yaml:"freshBootWindow"`
}

// ReplicationManaged reports whether replication jobs targeting Node should be disabled
// while it is down. It is nil-safe so a zero Proxmox still reads as the default: on.
func (p Proxmox) ReplicationManaged() bool {
	return p.ManageReplication == nil || *p.ManageReplication
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
	URLs           []string  `yaml:"urls"`
	Comment        string    `yaml:"comment"`
	UnsilenceGrace Duration  `yaml:"unsilenceGrace"`
	Silences       []Silence `yaml:"silences"`
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

	if s := os.Getenv("SELFSERVICE_CLIENT_SECRET"); s != "" {
		c.SelfService.ClientSecret = s
	}
	if s := os.Getenv("SELFSERVICE_SESSION_KEY"); s != "" {
		c.SelfService.SessionKey = s
	}
	if id := os.Getenv("PROXMOX_TOKEN_ID"); id != "" {
		c.Proxmox.TokenID = id
	}
	if secret := os.Getenv("PROXMOX_TOKEN_SECRET"); secret != "" {
		c.Proxmox.TokenSecret = secret
	}
	if t := os.Getenv("POWER_API_TOKEN"); t != "" && c.PowerAPI != nil {
		c.PowerAPI.Token = t
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
	if c.SelfService.SessionTTL.Duration == 0 {
		c.SelfService.SessionTTL = Duration{12 * time.Hour}
	}
	if c.GamingGrace.Duration == 0 {
		c.GamingGrace = Duration{10 * time.Minute}
	}
	if c.Alertmanager.UnsilenceGrace.Duration == 0 {
		c.Alertmanager.UnsilenceGrace = Duration{15 * time.Minute}
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
	if c.Proxmox.FreshBootWindow.Duration == 0 {
		c.Proxmox.FreshBootWindow = Duration{5 * time.Minute}
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
	case len(c.Proxmox.TargetNodes) == 0:
		return fmt.Errorf("proxmox.targetNodes must list at least one migration destination")
	case c.Proxmox.TokenID == "" || c.Proxmox.TokenSecret == "":
		return fmt.Errorf("proxmox token missing (set proxmox.tokenID/tokenSecret or PROXMOX_TOKEN_ID/PROXMOX_TOKEN_SECRET)")
	case len(c.Alertmanager.URLs) > 0 && len(c.Alertmanager.Silences) == 0:
		return fmt.Errorf("alertmanager.silences must be set when alertmanager.urls is configured")
	case c.PowerAPI != nil && (c.PowerAPI.URL == "" || c.PowerAPI.Load == ""):
		return fmt.Errorf("powerAPI needs url and load")
	case c.PowerAPI != nil && c.PowerAPI.Token == "":
		return fmt.Errorf("powerAPI token missing (set powerAPI.token or POWER_API_TOKEN)")
	}
	if c.Prometheus.HeadroomWatts < c.Prometheus.ShedBelowWatts {
		return fmt.Errorf("prometheus.headroomWatts (%v) must be >= shedBelowWatts (%v) for stable hysteresis",
			c.Prometheus.HeadroomWatts, c.Prometheus.ShedBelowWatts)
	}
	if c.SelfService.Enabled() {
		switch {
		case c.SelfService.IssuerURL == "":
			return fmt.Errorf("selfService.issuerURL is required when selfService.addr is set")
		case c.SelfService.ClientID == "":
			return fmt.Errorf("selfService.clientID is required when selfService.addr is set")
		case c.SelfService.ClientSecret == "":
			return fmt.Errorf("selfService.clientSecret is required (or set SELFSERVICE_CLIENT_SECRET)")
		case c.SelfService.SessionKey == "":
			return fmt.Errorf("selfService.sessionKey is required (or set SELFSERVICE_SESSION_KEY)")
		case c.SelfService.ExternalURL == "":
			return fmt.Errorf("selfService.externalURL is required: the OIDC redirect URI is built from it")
		case len(c.SelfService.SessionKey) < minSessionKeyLen:
			return fmt.Errorf("selfService.sessionKey must be at least %d characters: it signs the session cookie", minSessionKeyLen)
		case c.SelfService.SessionTTL.Duration <= 0:
			return fmt.Errorf("selfService.sessionTTL must be positive, got %v", c.SelfService.SessionTTL.Duration)
		}
		// Catch a malformed externalURL at startup rather than as a confusing redirect_uri
		// mismatch from authentik on someone's first login.
		u, err := url.Parse(c.SelfService.ExternalURL)
		switch {
		case err != nil:
			return fmt.Errorf("selfService.externalURL is not a URL: %w", err)
		case u.Scheme != "http" && u.Scheme != "https":
			return fmt.Errorf("selfService.externalURL needs an http:// or https:// scheme, got %q", c.SelfService.ExternalURL)
		case u.Host == "":
			return fmt.Errorf("selfService.externalURL has no host: %q", c.SelfService.ExternalURL)
		case u.RawQuery != "" || u.Fragment != "":
			return fmt.Errorf("selfService.externalURL must be a bare origin, not %q", c.SelfService.ExternalURL)
		}
		// A self-service VM outside gamingGuard would be started and then powered off under
		// it, since nothing would hold p1 up for it.
		for _, vm := range c.SelfService.VMs {
			if !c.Guests.GamingGuard.Contains(vm.VMID) {
				return fmt.Errorf("selfService.vms: %d is not in guests.gamingGuard, so p1 would not stay up for it", vm.VMID)
			}
		}
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
