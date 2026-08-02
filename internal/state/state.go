// Package state persists the watchdog's mode, the guests it stopped, and the active
// silence id across restarts. In-cluster it uses a ConfigMap; locally a JSON file.
package state

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Mode is the watchdog's coarse posture.
type Mode string

const (
	// ModeRunning: p1 is up and serving normally.
	ModeRunning Mode = "running"
	// ModeShed: p1 is powered off; criticals migrated, the rest stopped.
	ModeShed Mode = "shed"
	// ModeGaming: p1 is up in shed posture (criticals off, rest stopped) for a gaming session.
	ModeGaming Mode = "gaming"
)

// GuestRef identifies a guest enough to start it again later.
type GuestRef struct {
	VMID int    `json:"vmid"`
	Type string `json:"type"`
}

// Intent is what someone outside the reconcile loop asked for - a human with kubectl, or the
// self-service API (JHC-548). Keeping it separate from State is what keeps p1 single-owned:
// everyone else records a wish here, and the loop is the only thing that touches the hardware.
type Intent struct {
	// Shed holds the node shed regardless of solar surplus (heatwave, maintenance). Gaming
	// guard, grace window and wake inhibit behave exactly as in a solar-triggered shed.
	Shed bool `json:"shed,omitempty"`
	// Wake are outstanding requests to start a desktop VM, waking the node first if it's down.
	// They age out after gamingGrace rather than being cleared, so the loop never writes to spec.
	Wake []WakeRequest `json:"wake,omitempty"`
}

// WakeRequest is one user asking for their desktop VM to be started.
type WakeRequest struct {
	VMID        int    `json:"vmid"`
	User        string `json:"user"`
	RequestedAt int64  `json:"requestedAt"`
}

// Live reports whether the request still counts at time now. ttl is gamingGrace: a request
// holds p1 up for exactly as long as a gaming session gets to produce a running VM, so the
// two can't drift apart into a window where a request outlives the grace that honours it.
func (w WakeRequest) Live(now time.Time, ttl time.Duration) bool {
	return now.Sub(time.Unix(w.RequestedAt, 0)) < ttl
}

// LiveWake returns the wake requests that have not yet aged out.
func (i Intent) LiveWake(now time.Time, ttl time.Duration) []WakeRequest {
	var out []WakeRequest
	for _, w := range i.Wake {
		if w.Live(now, ttl) {
			out = append(out, w)
		}
	}
	return out
}

// State is the persisted controller state. Silences are deliberately not tracked here:
// they're reconciled directly against Alertmanager (identified by their createdBy), so a
// lost or stale ConfigMap can't orphan them.
type State struct {
	Mode    Mode       `json:"mode"`
	Stopped []GuestRef `json:"stopped"`
	// GraceSince is the unix time the gaming-session grace clock started ticking: it is
	// set when p1 is up in ModeGaming with no gaming guest running, and reset to 0 the
	// moment a gaming guest is seen (or the session ends). p1 isn't powered off until the
	// clock exceeds the grace window, so a freshly-woken host with no VM yet, or a
	// GPU-passthrough/VM reboot mid-session, isn't cut short. 0 when the clock isn't running.
	GraceSince int64 `json:"graceSince,omitempty"`
	// WakeDone records, per desktop VM, the RequestedAt of the last wake request seen through
	// to a running VM. A spent request must not start the VM again, or shutting it down inside
	// the request's TTL would just bring it back on the next tick.
	WakeDone map[int]int64 `json:"wakeDone,omitempty"`
}

// Store loads and saves the watchdog's State and the externally-set Intent. The two are
// persisted independently so that a writer of one can never overwrite the other.
type Store interface {
	Load(ctx context.Context) (State, error)
	Save(ctx context.Context, s State) error
	LoadIntent(ctx context.Context) (Intent, error)
	SaveIntent(ctx context.Context, i Intent) error
}

// FileStore persists state to a local JSON file (for local runs / tests). Intent lives in a
// sibling file, mirroring the separate-key split the ConfigMap store uses.
type FileStore struct{ path string }

// NewFileStore returns a file-backed store.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

func (f *FileStore) intentPath() string { return f.path + ".intent" }

// LoadIntent reads the intent file, returning the zero intent if it does not exist.
func (f *FileStore) LoadIntent(context.Context) (Intent, error) {
	data, err := os.ReadFile(f.intentPath())
	if os.IsNotExist(err) {
		return Intent{}, nil
	}
	if err != nil {
		return Intent{}, err
	}
	var i Intent
	if err := json.Unmarshal(data, &i); err != nil {
		return Intent{}, err
	}
	return i, nil
}

// SaveIntent writes the intent file atomically.
func (f *FileStore) SaveIntent(_ context.Context, i Intent) error {
	data, err := json.Marshal(i)
	if err != nil {
		return err
	}
	tmp := f.intentPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.intentPath())
}

// Load reads the state file, returning a fresh state if it does not exist.
func (f *FileStore) Load(context.Context) (State, error) {
	data, err := os.ReadFile(f.path)
	if os.IsNotExist(err) {
		return State{Mode: ModeRunning}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, err
	}
	return s, nil
}

// Save writes the state file atomically.
func (f *FileStore) Save(_ context.Context, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

const saPath = "/var/run/secrets/kubernetes.io/serviceaccount"

// InCluster reports whether a service-account token is mounted.
func InCluster() bool {
	_, err := os.Stat(saPath + "/token")
	return err == nil
}

// ConfigMapStore persists state in a single ConfigMap key, talking to the in-cluster
// API server directly (no client-go dependency).
type ConfigMapStore struct {
	name      string
	namespace string
	token     string
	base      string // API server root; overridden in tests
	http      *http.Client
}

// NewConfigMapStore builds a store backed by the named ConfigMap in the pod's namespace.
func NewConfigMapStore(name string) (*ConfigMapStore, error) {
	token, err := os.ReadFile(saPath + "/token")
	if err != nil {
		return nil, fmt.Errorf("read service-account token: %w", err)
	}
	ns, err := os.ReadFile(saPath + "/namespace")
	if err != nil {
		return nil, fmt.Errorf("read namespace: %w", err)
	}
	caPEM, err := os.ReadFile(saPath + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse cluster CA")
	}
	return &ConfigMapStore{
		name:      name,
		namespace: strings.TrimSpace(string(ns)),
		token:     strings.TrimSpace(string(token)),
		base:      "https://kubernetes.default.svc",
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

// The two ConfigMap keys. They are written by separate merge patches, never by a whole-object
// PUT, so the reconcile loop saving state and the API saving intent cannot overwrite each
// other - and neither wipes a key an operator added by hand.
const (
	stateKey  = "state.json"
	intentKey = "intent.json"
)

func (c *ConfigMapStore) url() string {
	return fmt.Sprintf("%s/api/v1/namespaces/%s/configmaps/%s", c.base, c.namespace, c.name)
}

func (c *ConfigMapStore) listURL() string {
	return fmt.Sprintf("%s/api/v1/namespaces/%s/configmaps", c.base, c.namespace)
}

type configMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   map[string]any    `json:"metadata"`
	Data       map[string]string `json:"data"`
}

func (c *ConfigMapStore) request(ctx context.Context, method, url, contentType string, body []byte) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		r = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

// readKey returns one ConfigMap key, empty if the map or the key is missing.
func (c *ConfigMapStore) readKey(ctx context.Context, key string) (string, error) {
	data, code, err := c.request(ctx, http.MethodGet, c.url(), "", nil)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "", nil
	}
	if code >= http.StatusMultipleChoices {
		return "", fmt.Errorf("get configmap: %d: %s", code, data)
	}
	var cm configMap
	if err := json.Unmarshal(data, &cm); err != nil {
		return "", err
	}
	return cm.Data[key], nil
}

// writeKey merge-patches a single key so the ConfigMap's other keys survive untouched. That
// is what lets the loop write state.json while the API writes intent.json.
func (c *ConfigMapStore) writeKey(ctx context.Context, key, value string) error {
	patch, err := json.Marshal(map[string]any{"data": map[string]string{key: value}})
	if err != nil {
		return err
	}
	data, code, err := c.request(ctx, http.MethodPatch, c.url(), "application/merge-patch+json", patch)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		body, err := json.Marshal(configMap{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Metadata:   map[string]any{"name": c.name, "namespace": c.namespace},
			Data:       map[string]string{key: value},
		})
		if err != nil {
			return err
		}
		data, code, err = c.request(ctx, http.MethodPost, c.listURL(), "application/json", body)
		if err != nil {
			return err
		}
	}
	if code >= http.StatusMultipleChoices {
		return fmt.Errorf("write configmap key %s: %d: %s", key, code, data)
	}
	return nil
}

// Load fetches the ConfigMap, returning a fresh state if it has no data yet.
func (c *ConfigMapStore) Load(ctx context.Context) (State, error) {
	raw, err := c.readKey(ctx, stateKey)
	if err != nil {
		return State{}, err
	}
	if raw == "" {
		return State{Mode: ModeRunning}, nil
	}
	var s State
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return State{}, err
	}
	return s, nil
}

// LoadIntent reads the externally-set intent, zero if nobody has set one.
func (c *ConfigMapStore) LoadIntent(ctx context.Context) (Intent, error) {
	raw, err := c.readKey(ctx, intentKey)
	if err != nil {
		return Intent{}, err
	}
	if raw == "" {
		return Intent{}, nil
	}
	var i Intent
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		return Intent{}, err
	}
	return i, nil
}

// SaveIntent records what was asked for. Only the API calls this, never the reconcile loop.
func (c *ConfigMapStore) SaveIntent(ctx context.Context, i Intent) error {
	payload, err := json.Marshal(i)
	if err != nil {
		return err
	}
	return c.writeKey(ctx, intentKey, string(payload))
}

// Save writes state back, creating the ConfigMap if it doesn't exist.
func (c *ConfigMapStore) Save(ctx context.Context, s State) error {
	payload, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return c.writeKey(ctx, stateKey, string(payload))
}
