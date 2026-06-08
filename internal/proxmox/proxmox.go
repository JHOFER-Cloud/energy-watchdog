// Package proxmox is a thin Proxmox VE API client covering only what the watchdog
// needs: listing guests, migrating, stopping, starting, and shutting a node down.
package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// GuestType is the Proxmox guest kind, matching the API path segment.
type GuestType string

const (
	TypeQEMU GuestType = "qemu"
	TypeLXC  GuestType = "lxc"
)

// Guest is a VM or container on a node.
type Guest struct {
	VMID    int
	Name    string
	Type    GuestType
	Running bool
}

// Client talks to a single Proxmox cluster node's API. That node manages the whole
// cluster, so it can act on guests that live on other (including offline) nodes.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

// New builds a client. token is "user@realm!tokenname=secret". A nil tlsConf uses the
// system roots.
func New(endpoint, tokenID, tokenSecret string, tlsConf *tls.Config) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		token:    fmt.Sprintf("%s=%s", tokenID, tokenSecret),
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsConf},
		},
	}
}

// TLSConfig builds the TLS config for the Proxmox client. insecure skips verification
// entirely. Otherwise, if caCertPath is set, that CA (the internal jhc-ca) is trusted
// on top of the system roots; an empty path falls back to the system roots alone.
func TLSConfig(caCertPath string, insecure bool) (*tls.Config, error) {
	if insecure {
		return &tls.Config{InsecureSkipVerify: true}, nil //nolint:gosec // opt-in escape hatch
	}
	if caCertPath == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %s: %w", caCertPath, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certs parsed from %s", caCertPath)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body url.Values) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(body.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+"/api2/json"+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("proxmox %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode proxmox response: %w", err)
	}
	return env.Data, nil
}

// NodeUp reports whether node is online, as seen from the cluster API.
func (c *Client) NodeUp(ctx context.Context, node string) (bool, error) {
	data, err := c.do(ctx, http.MethodGet, "/nodes", nil)
	if err != nil {
		return false, err
	}
	var nodes []struct {
		Node   string `json:"node"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &nodes); err != nil {
		return false, err
	}
	for _, n := range nodes {
		if n.Node == node {
			return n.Status == "online", nil
		}
	}
	return false, fmt.Errorf("node %q not found in cluster", node)
}

// Guests lists the VMs and containers on a node.
func (c *Client) Guests(ctx context.Context, node string) ([]Guest, error) {
	var out []Guest
	for _, t := range []GuestType{TypeQEMU, TypeLXC} {
		data, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/%s", node, t), nil)
		if err != nil {
			return nil, err
		}
		var items []struct {
			VMID   json.Number `json:"vmid"`
			Name   string      `json:"name"`
			Status string      `json:"status"`
		}
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			id, err := strconv.Atoi(it.VMID.String())
			if err != nil {
				continue
			}
			out = append(out, Guest{VMID: id, Name: it.Name, Type: t, Running: it.Status == "running"})
		}
	}
	return out, nil
}

// Migrate moves a guest to target. QEMU goes online (live); LXC uses restart-migration.
// It returns the task UPID to wait on.
func (c *Client) Migrate(ctx context.Context, node string, g Guest, target string) (string, error) {
	v := url.Values{}
	v.Set("target", target)
	if g.Type == TypeQEMU {
		v.Set("online", "1")
	} else {
		v.Set("restart", "1")
	}
	return c.task(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/%s/%d/migrate", node, g.Type, g.VMID), v)
}

// Stop gracefully shuts a guest down. Returns the task UPID.
func (c *Client) Stop(ctx context.Context, node string, g Guest) (string, error) {
	return c.task(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/%s/%d/status/shutdown", node, g.Type, g.VMID), nil)
}

// Start boots a guest. Returns the task UPID.
func (c *Client) Start(ctx context.Context, node string, g Guest) (string, error) {
	return c.task(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/%s/%d/status/start", node, g.Type, g.VMID), nil)
}

// ShutdownNode powers off a whole node.
func (c *Client) ShutdownNode(ctx context.Context, node string) error {
	v := url.Values{}
	v.Set("command", "shutdown")
	_, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/status", node), v)
	return err
}

func (c *Client) task(ctx context.Context, method, path string, body url.Values) (string, error) {
	data, err := c.do(ctx, method, path, body)
	if err != nil {
		return "", err
	}
	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		return "", fmt.Errorf("decode task upid: %w", err)
	}
	return upid, nil
}

// WaitTask blocks until the task identified by upid finishes, erroring on a non-OK exit.
func (c *Client) WaitTask(ctx context.Context, node, upid string) error {
	for {
		data, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/tasks/%s/status", node, url.PathEscape(upid)), nil)
		if err != nil {
			return err
		}
		var s struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if s.Status == "stopped" {
			if s.ExitStatus != "OK" {
				return fmt.Errorf("task %s failed: %s", upid, s.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// WaitNodeUp blocks until node reports online or ctx expires.
func (c *Client) WaitNodeUp(ctx context.Context, node string) error {
	for {
		if up, err := c.NodeUp(ctx, node); err == nil && up {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
