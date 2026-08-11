// Package proxmox is a thin Proxmox VE API client covering only what the watchdog
// needs: listing guests, migrating, stopping, starting, shutting a node down, and
// enabling/disabling cluster replication jobs.
package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
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
	Tags    []string // Proxmox guest tags, already split
}

// HasTag reports whether the guest carries tag (case-insensitive, as Proxmox
// lowercases tags on write but older ones may not be).
func (g Guest) HasTag(tag string) bool {
	for _, t := range g.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// splitTags parses Proxmox's tag encoding: semicolon-separated, and empty when unset.
func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ";")
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

// NodeState reports whether node is online and how long it has been up, as seen from the
// cluster API. Uptime needs Sys.Audit on /nodes; without it the field is dropped and reads 0.
func (c *Client) NodeState(ctx context.Context, node string) (up bool, uptime time.Duration, err error) {
	data, err := c.do(ctx, http.MethodGet, "/nodes", nil)
	if err != nil {
		return false, 0, err
	}
	var nodes []struct {
		Node   string `json:"node"`
		Status string `json:"status"`
		Uptime int64  `json:"uptime"`
	}
	if err := json.Unmarshal(data, &nodes); err != nil {
		return false, 0, err
	}
	for _, n := range nodes {
		if n.Node == node {
			return n.Status == "online", time.Duration(n.Uptime) * time.Second, nil
		}
	}
	return false, 0, fmt.Errorf("node %q not found in cluster", node)
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
			Tags   string      `json:"tags"`
		}
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			id, err := strconv.Atoi(it.VMID.String())
			if err != nil {
				continue
			}
			out = append(out, Guest{VMID: id, Name: it.Name, Type: t, Running: it.Status == "running", Tags: splitTags(it.Tags)})
		}
	}
	return out, nil
}

// The bulk endpoints below are Proxmox's own mass actions: they group guests by `startup`
// order and run each group max_workers wide. max-workers is never sent, so datacenter.cfg wins.

// MigrateAll moves guests to target. Running QEMU guests go online (live), running LXC ones
// restart-migrate - the same split the per-guest endpoint needs.
func (c *Client) MigrateAll(ctx context.Context, node, target string, vmids []int) (string, error) {
	list, err := vmidList(vmids)
	if err != nil {
		return "", err
	}
	v := url.Values{}
	v.Set("target", target)
	v.Set("vms", list)
	return c.task(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/migrateall", node), v)
}

// StopAll gracefully shuts guests down, in reverse startup order. force-stop=0 leaves a guest
// that won't shut down within timeout an error rather than hard-killing it.
func (c *Client) StopAll(ctx context.Context, node string, vmids []int, timeout time.Duration) (string, error) {
	list, err := vmidList(vmids)
	if err != nil {
		return "", err
	}
	v := url.Values{}
	v.Set("vms", list)
	v.Set("force-stop", "0")
	v.Set("timeout", strconv.Itoa(int(timeout.Seconds())))
	return c.task(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/stopall", node), v)
}

// StartAll boots guests in startup order, waiting out each group's up= delay. force=1 because
// a guest we stopped is ours to start again whether or not it has onboot set.
func (c *Client) StartAll(ctx context.Context, node string, vmids []int) (string, error) {
	list, err := vmidList(vmids)
	if err != nil {
		return "", err
	}
	v := url.Values{}
	v.Set("vms", list)
	v.Set("force", "1")
	return c.task(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/startall", node), v)
}

// vmidList formats the `vms` filter. Absent, it means *every* guest on the node, so an empty
// list is refused rather than sent - that would sweep up the gaming VMs a shed must leave up.
func vmidList(vmids []int) (string, error) {
	if len(vmids) == 0 {
		return "", errors.New("refusing a bulk action with no vmids: it would act on every guest on the node")
	}
	out := make([]string, len(vmids))
	for i, id := range vmids {
		out[i] = strconv.Itoa(id)
	}
	return strings.Join(out, ","), nil
}

// GuestPower is a per-guest power action, matching the API path segment.
type GuestPower string

const (
	PowerShutdown GuestPower = "shutdown" // graceful ACPI shutdown
	PowerReboot   GuestPower = "reboot"   // graceful shutdown and start again
	PowerReset    GuestPower = "reset"    // like the reset button; QEMU only
	PowerStop     GuestPower = "stop"     // pulls the plug
)

// Power runs a power action on a single guest and returns the task UPID. LXC has no reset, so
// it is rejected here rather than becoming a 501 from Proxmox mid-click.
func (c *Client) Power(ctx context.Context, node string, g Guest, action GuestPower) (string, error) {
	if action == PowerReset && g.Type != TypeQEMU {
		return "", fmt.Errorf("reset is QEMU-only, guest %d is %s", g.VMID, g.Type)
	}
	return c.task(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/%s/%d/status/%s", node, g.Type, g.VMID, action), url.Values{})
}

// GPUKey identifies the physical GPU passed through to a guest, or "" if it has none. It is
// the resource-mapping name where one is used (hostpci0: mapping=gpu0,...) and the raw PCI
// address otherwise, so two guests sharing a GPU always land on the same key.
func (c *Client) GPUKey(ctx context.Context, node string, g Guest) (string, error) {
	data, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/%s/%d/config", node, g.Type, g.VMID), nil)
	if err != nil {
		return "", err
	}
	// The config mixes strings and numbers (cores, memory), so decode lazily and only read the
	// hostpci fields. Lowest-numbered one wins: a guest with several passthrough devices still
	// gets one stable key, and the GPU is conventionally hostpci0.
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", err
	}
	field, lowest := "", -1
	for k := range cfg {
		// Compare the index numerically: "hostpci10" sorts before "hostpci2" as a string.
		n, err := strconv.Atoi(strings.TrimPrefix(k, "hostpci"))
		if !strings.HasPrefix(k, "hostpci") || err != nil {
			continue
		}
		if lowest < 0 || n < lowest {
			field, lowest = k, n
		}
	}
	if field == "" {
		return "", nil
	}
	var v string
	if err := json.Unmarshal(cfg[field], &v); err != nil {
		return "", fmt.Errorf("decode %s of guest %d: %w", field, g.VMID, err)
	}
	return hostPCIKey(v), nil
}

// hostPCIKey reduces a hostpciN value to its device identity, dropping the pcie=/x-vga= flags.
func hostPCIKey(v string) string {
	for _, part := range strings.Split(v, ",") {
		if name, ok := strings.CutPrefix(part, "mapping="); ok {
			return name
		}
	}
	return strings.Split(v, ",")[0]
}

// ReplicationJob is one entry of the cluster's replication config.
type ReplicationJob struct {
	ID       string // "<guest>-<jobnum>", e.g. "104-0"
	Source   string // node replicated from
	Target   string // node replicated to
	Comment  string
	Disabled bool
}

// ReplicationJobs lists the cluster's replication jobs. Note that the API silently omits
// jobs whose guest the token has no VM.Audit permission on, so a short list means a token
// permission problem rather than an empty replication config.
func (c *Client) ReplicationJobs(ctx context.Context) ([]ReplicationJob, error) {
	data, err := c.do(ctx, http.MethodGet, "/cluster/replication", nil)
	if err != nil {
		return nil, err
	}
	var items []struct {
		ID      string `json:"id"`
		Source  string `json:"source"`
		Target  string `json:"target"`
		Comment string `json:"comment"`
		Disable any    `json:"disable"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	out := make([]ReplicationJob, 0, len(items))
	for _, it := range items {
		out = append(out, ReplicationJob{
			ID:       it.ID,
			Source:   it.Source,
			Target:   it.Target,
			Comment:  it.Comment,
			Disabled: pveBool(it.Disable),
		})
	}
	return out, nil
}

// SetReplicationJob enables or disables a replication job and sets its comment. This is
// Proxmox's own disable flag - what "pvesr enable/disable" and the GUI's Enabled checkbox
// set - so the job, its schedule and its accumulated replication state all stay in place
// and re-enabling resumes incrementally instead of re-syncing from scratch. An empty
// comment is deleted from the job rather than stored as an empty string.
func (c *Client) SetReplicationJob(ctx context.Context, id, comment string, disable bool) error {
	v := url.Values{}
	if disable {
		v.Set("disable", "1")
	} else {
		v.Set("disable", "0")
	}
	if comment == "" {
		v.Set("delete", "comment")
	} else {
		v.Set("comment", comment)
	}
	_, err := c.do(ctx, http.MethodPut, "/cluster/replication/"+url.PathEscape(id), v)
	return err
}

// pveBool reads a Proxmox boolean, which the replication config passes through as a raw
// number, string or JSON bool depending on how it was written.
func pveBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	}
	return false
}

// WakeOnLAN asks the cluster to send node a magic packet. The sending node is one that's
// already on node's segment, so the packet is a local broadcast and needs no ARP entry for
// the powered-off NIC. The MAC comes from node's `wakeonlan` property (pvenode config set).
func (c *Client) WakeOnLAN(ctx context.Context, node string) (mac string, err error) {
	data, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/wakeonlan", node), url.Values{})
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &mac); err != nil {
		return "", fmt.Errorf("decode wakeonlan mac: %w", err)
	}
	return mac, nil
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
		if up, _, err := c.NodeState(ctx, node); err == nil && up {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
