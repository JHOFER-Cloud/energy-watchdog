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

// State is the persisted controller state.
type State struct {
	Mode      Mode       `json:"mode"`
	Stopped   []GuestRef `json:"stopped"`
	SilenceID string     `json:"silenceID"`
}

// Store loads and saves State.
type Store interface {
	Load(ctx context.Context) (State, error)
	Save(ctx context.Context, s State) error
}

// FileStore persists state to a local JSON file (for local runs / tests).
type FileStore struct{ path string }

// NewFileStore returns a file-backed store.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

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
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

const stateKey = "state.json"

func (c *ConfigMapStore) url() string {
	return fmt.Sprintf("https://kubernetes.default.svc/api/v1/namespaces/%s/configmaps/%s", c.namespace, c.name)
}

type configMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   map[string]any    `json:"metadata"`
	Data       map[string]string `json:"data"`
}

func (c *ConfigMapStore) request(ctx context.Context, method, url string, body []byte) ([]byte, int, error) {
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
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

// Load fetches the ConfigMap, returning a fresh state if it has no data yet.
func (c *ConfigMapStore) Load(ctx context.Context) (State, error) {
	data, code, err := c.request(ctx, http.MethodGet, c.url(), nil)
	if err != nil {
		return State{}, err
	}
	if code == http.StatusNotFound {
		return State{Mode: ModeRunning}, nil
	}
	if code >= http.StatusMultipleChoices {
		return State{}, fmt.Errorf("get configmap: %d: %s", code, data)
	}
	var cm configMap
	if err := json.Unmarshal(data, &cm); err != nil {
		return State{}, err
	}
	raw, ok := cm.Data[stateKey]
	if !ok || raw == "" {
		return State{Mode: ModeRunning}, nil
	}
	var s State
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return State{}, err
	}
	return s, nil
}

// Save writes state back, creating the ConfigMap if it doesn't exist.
func (c *ConfigMapStore) Save(ctx context.Context, s State) error {
	payload, err := json.Marshal(s)
	if err != nil {
		return err
	}
	cm := configMap{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata:   map[string]any{"name": c.name, "namespace": c.namespace},
		Data:       map[string]string{stateKey: string(payload)},
	}
	body, err := json.Marshal(cm)
	if err != nil {
		return err
	}

	// Try update first; create on 404.
	data, code, err := c.request(ctx, http.MethodPut, c.url(), body)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		listURL := fmt.Sprintf("https://kubernetes.default.svc/api/v1/namespaces/%s/configmaps", c.namespace)
		data, code, err = c.request(ctx, http.MethodPost, listURL, body)
		if err != nil {
			return err
		}
	}
	if code >= http.StatusMultipleChoices {
		return fmt.Errorf("save configmap: %d: %s", code, data)
	}
	return nil
}
