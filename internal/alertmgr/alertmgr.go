// Package alertmgr creates and removes Alertmanager v2 silences so a planned p1
// shutdown doesn't page.
package alertmgr

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
)

// CreatedBy is stamped as the createdBy of every silence this client creates, and is how
// the controller recognises its own silences when reconciling (without persisting ids).
const CreatedBy = "energy-watchdog"

// Client is a minimal Alertmanager v2 silences client.
type Client struct {
	base string
	http *http.Client
}

// New builds an Alertmanager client for the given base URL. tlsConf is applied for
// https bases (e.g. a cross-cluster Alertmanager fronted by an internal-CA ingress);
// pass nil for plain-http in-cluster endpoints.
func New(base string, tlsConf *tls.Config) *Client {
	c := &http.Client{Timeout: 15 * time.Second}
	if tlsConf != nil {
		c.Transport = &http.Transport{TLSClientConfig: tlsConf}
	}
	return &Client{base: base, http: c}
}

type silenceMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

// Silence is an existing silence as returned by Alertmanager, carrying just the fields
// the controller needs to recognise its own silences and reconcile them.
type Silence struct {
	ID        string           `json:"id"`
	CreatedBy string           `json:"createdBy"`
	Comment   string           `json:"comment"`
	Matchers  []silenceMatcher `json:"matchers"`
	EndsAt    time.Time        `json:"endsAt"`
	Status    struct {
		State string `json:"state"` // "active", "pending" or "expired"
	} `json:"status"`
}

// Key is a canonical, order-independent digest of this silence's comment and matchers.
// Two silences with the same Key are considered the same silence, so the controller can
// recognise the ones it created (matching a DesiredKey) regardless of their id.
func (s Silence) Key() string {
	parts := make([]string, len(s.Matchers))
	for i, m := range s.Matchers {
		parts[i] = fmt.Sprintf("%s\x1f%s\x1f%t\x1f%t", m.Name, m.Value, m.IsRegex, m.IsEqual)
	}
	sort.Strings(parts)
	return s.Comment + "\x1e" + strings.Join(parts, "\x1d")
}

// DesiredKey builds the same canonical digest as Silence.Key for a configured silence, so
// a desired silence and an existing one can be compared by string. Configured matchers are
// always equality matchers (isEqual=true), mirroring what Create posts.
func DesiredKey(comment string, matchers []config.Matcher) string {
	s := Silence{Comment: comment, Matchers: make([]silenceMatcher, len(matchers))}
	for i, m := range matchers {
		s.Matchers[i] = silenceMatcher{Name: m.Name, Value: m.Value, IsRegex: m.IsRegex, IsEqual: true}
	}
	return s.Key()
}

// List returns every silence Alertmanager currently holds (active, pending and expired).
func (c *Client) List(ctx context.Context) ([]Silence, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v2/silences", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("list silences: %s: %s", resp.Status, data)
	}
	var out []Silence
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Create posts a new silence active until now+ttl and returns its id.
func (c *Client) Create(ctx context.Context, matchers []config.Matcher, comment string, ttl time.Duration, now time.Time) (string, error) {
	return c.post(ctx, "", toMatchers(matchers), comment, ttl, now)
}

// Update extends an existing silence (by id) to now+ttl, reusing it instead of creating a
// fresh one. Alertmanager treats a POST that carries an id as an in-place update, so a
// long shutdown just pushes the same silence's endsAt out rather than churning ids.
func (c *Client) Update(ctx context.Context, id string, matchers []config.Matcher, comment string, ttl time.Duration, now time.Time) (string, error) {
	return c.post(ctx, id, toMatchers(matchers), comment, ttl, now)
}

// Reschedule reposts an existing silence unchanged except for its window, moving it to
// [now, now+ttl]. It's used to shorten a silence to a short grace window instead of deleting
// it outright, so coverage lapses gently rather than vanishing in one tick. It reuses the
// silence's own matchers and comment, so the caller needn't reconstruct them.
func (c *Client) Reschedule(ctx context.Context, s Silence, ttl time.Duration, now time.Time) (string, error) {
	return c.post(ctx, s.ID, s.Matchers, s.Comment, ttl, now)
}

func toMatchers(ms []config.Matcher) []silenceMatcher {
	out := make([]silenceMatcher, 0, len(ms))
	for _, m := range ms {
		out = append(out, silenceMatcher{Name: m.Name, Value: m.Value, IsRegex: m.IsRegex, IsEqual: true})
	}
	return out
}

// post creates (id == "") or updates (id != "") a silence and returns its id.
func (c *Client) post(ctx context.Context, id string, ms []silenceMatcher, comment string, ttl time.Duration, now time.Time) (string, error) {
	payload := map[string]any{
		"matchers":  ms,
		"startsAt":  now.UTC().Format(time.RFC3339),
		"endsAt":    now.Add(ttl).UTC().Format(time.RFC3339),
		"createdBy": CreatedBy,
		"comment":   comment,
	}
	if id != "" {
		payload["id"] = id
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v2/silences", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("post silence: %s: %s", resp.Status, data)
	}
	var out struct {
		SilenceID string `json:"silenceID"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.SilenceID, nil
}

// Delete removes a silence by id. A 404 is treated as already-gone.
func (c *Client) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/api/v2/silence/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete silence %s: %s: %s", id, resp.Status, data)
	}
	return nil
}
