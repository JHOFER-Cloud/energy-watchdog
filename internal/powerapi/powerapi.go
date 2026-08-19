// Package powerapi asks nut-dog for p1's power state. nut-dog owns the physical
// power: it is the service that has to work during an outage, and its mechanisms
// (NUT shed signal, WoL) need neither Proxmox nor the cluster. Requests are
// advisory - a critical UPS outranks them - so a refusal is normal, not an error.
package powerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Desired is the wire vocabulary; "hold" withdraws a request.
const (
	On   = "on"
	Off  = "off"
	Hold = "hold"
)

// Probe states reported by nut-dog for a load.
const (
	ActualUp      = "up"
	ActualDown    = "down"
	ActualUnknown = "unknown"
)

// Client talks to one nut-dog load.
type Client struct {
	base  string
	token string
	load  string
	http  *http.Client
}

// New builds a client for the named load.
func New(base, token, load string) *Client {
	return &Client{base: strings.TrimSuffix(base, "/"), token: token, load: load,
		http: &http.Client{Timeout: 10 * time.Second}}
}

// Request states what this controller wants the load's power to be. It is
// level-triggered: restating the same wish is a no-op on nut-dog's side, and is
// what lets nut-dog stay stateless across restarts.
func (c *Client) Request(ctx context.Context, desired, reason string) error {
	body, err := json.Marshal(map[string]string{"desired": desired, "reason": reason})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/loads/%s/power", c.base, c.load)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("power request %s: %s: %s", desired, resp.Status, bytes.TrimSpace(msg))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// State returns nut-dog's last probe of the load and the age of that reading.
//
// This answers a different question from Proxmox node state, which is reported by the other
// pve nodes and so reads offline for a host that is merely partitioned from corosync. nut-dog
// probes the host directly.
func (c *Client) State(ctx context.Context) (actual string, age time.Duration, err error) {
	url := fmt.Sprintf("%s/api/loads/%s/state", c.base, c.load)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("power state: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var body struct {
		Actual     string `json:"actual"`
		AgeSeconds int    `json:"ageSeconds"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<10)).Decode(&body); err != nil {
		return "", 0, fmt.Errorf("decode power state: %w", err)
	}
	// Returned as received, unrecognised values included. Both services declare this
	// vocabulary independently, so normalising an unknown word here would make a drift between
	// them indistinguishable from nut-dog having no opinion - and a drift disables the
	// caller's up/down handling entirely. Callers treat anything they cannot read as unknown.
	return body.Actual, time.Duration(body.AgeSeconds) * time.Second, nil
}
