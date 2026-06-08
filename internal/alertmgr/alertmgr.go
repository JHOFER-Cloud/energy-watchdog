// Package alertmgr creates and removes Alertmanager v2 silences so a planned p1
// shutdown doesn't page.
package alertmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
)

// Client is a minimal Alertmanager v2 silences client.
type Client struct {
	base string
	http *http.Client
}

// New builds an Alertmanager client for the given base URL.
func New(base string) *Client {
	return &Client{base: base, http: &http.Client{Timeout: 15 * time.Second}}
}

type silenceMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

// Create posts a silence active until now+ttl and returns its id.
func (c *Client) Create(ctx context.Context, matchers []config.Matcher, comment string, ttl time.Duration, now time.Time) (string, error) {
	ms := make([]silenceMatcher, 0, len(matchers))
	for _, m := range matchers {
		ms = append(ms, silenceMatcher{Name: m.Name, Value: m.Value, IsRegex: m.IsRegex, IsEqual: true})
	}
	payload := map[string]any{
		"matchers":  ms,
		"startsAt":  now.UTC().Format(time.RFC3339),
		"endsAt":    now.Add(ttl).UTC().Format(time.RFC3339),
		"createdBy": "energy-watchdog",
		"comment":   comment,
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
		return "", fmt.Errorf("create silence: %s: %s", resp.Status, data)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/api/v2/silences/"+id, nil)
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
