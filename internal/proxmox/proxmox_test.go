package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "user@pam!tok", "secret", nil)
	return c
}

func TestNodeState(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=user@pam!tok=secret" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"node":"pve-1","status":"offline"},{"node":"pve-2","status":"online","uptime":25903}]}`))
	})
	up, uptime, err := c.NodeState(context.Background(), "pve-1")
	if err != nil {
		t.Fatal(err)
	}
	if up || uptime != 0 {
		t.Errorf("pve-1 = up %v, uptime %v; want offline with no uptime", up, uptime)
	}
	up, uptime, err = c.NodeState(context.Background(), "pve-2")
	if err != nil {
		t.Fatal(err)
	}
	if !up {
		t.Error("pve-2 should be online")
	}
	if uptime != 25903*time.Second {
		t.Errorf("pve-2 uptime = %v, want %v", uptime, 25903*time.Second)
	}
}

func TestGuests(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/qemu"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":101,"name":"talos-1","status":"running"},{"vmid":601,"name":"desktop","status":"stopped"}]}`))
		case strings.HasSuffix(r.URL.Path, "/lxc"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":311,"name":"ct","status":"running"}]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	guests, err := c.Guests(context.Background(), "pve-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(guests) != 3 {
		t.Fatalf("got %d guests, want 3", len(guests))
	}
	want := map[int]struct {
		t       GuestType
		running bool
	}{
		101: {TypeQEMU, true},
		601: {TypeQEMU, false},
		311: {TypeLXC, true},
	}
	for _, g := range guests {
		w := want[g.VMID]
		if g.Type != w.t || g.Running != w.running {
			t.Errorf("guest %d = {%s,%v}, want {%s,%v}", g.VMID, g.Type, g.Running, w.t, w.running)
		}
	}
}

func TestMigrateAllAndWaitTask(t *testing.T) {
	const upid = "UPID:pve-1:00001:migrateall"
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/migrateall"):
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("target") != "pve-2" || r.Form.Get("vms") != "101,311" {
				t.Errorf("migrateall form = %v", r.Form)
			}
			// max-workers must stay unset so the datacenter's bulk-action setting applies.
			if _, ok := r.Form["max-workers"]; ok {
				t.Errorf("max-workers was sent: %v", r.Form)
			}
			_, _ = w.Write([]byte(`{"data":"` + upid + `"}`))
		case strings.Contains(r.URL.Path, "/tasks/"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	got, err := c.MigrateAll(context.Background(), "pve-1", "pve-2", []int{101, 311})
	if err != nil {
		t.Fatal(err)
	}
	if got != upid {
		t.Errorf("upid = %q, want %q", got, upid)
	}
	if err := c.WaitTask(context.Background(), "pve-1", got); err != nil {
		t.Errorf("WaitTask: %v", err)
	}
}

// TestStopAllForm pins the two parameters that decide what a shed does to a guest that won't
// go down: it gets stopTimeout, and force-stop=0 leaves it an error rather than a hard kill.
func TestStopAllForm(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("vms") != "301" || r.Form.Get("force-stop") != "0" || r.Form.Get("timeout") != "600" {
			t.Errorf("stopall form = %v", r.Form)
		}
		_, _ = w.Write([]byte(`{"data":"UPID:stopall"}`))
	})
	if _, err := c.StopAll(context.Background(), "pve-1", []int{301}, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
}

// TestStartAllForcesOnboot: startall skips guests without onboot=1 unless force is set, and
// a guest we stopped is ours to start again whether or not it boots on its own.
func TestStartAllForcesOnboot(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("vms") != "301,700" || r.Form.Get("force") != "1" {
			t.Errorf("startall form = %v", r.Form)
		}
		_, _ = w.Write([]byte(`{"data":"UPID:startall"}`))
	})
	if _, err := c.StartAll(context.Background(), "pve-1", []int{301, 700}); err != nil {
		t.Fatal(err)
	}
}

// TestBulkRefusesEmptyList: with vms absent the bulk endpoints act on every guest on the node,
// so an empty list must never reach them - a shed would sweep up the gaming VMs.
func TestBulkRefusesEmptyList(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("bulk call reached the server with an empty vmid list: %s", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":"UPID:x"}`))
	})
	ctx := context.Background()
	if _, err := c.StopAll(ctx, "pve-1", nil, time.Minute); err == nil {
		t.Error("StopAll(nil) = nil error, want a refusal")
	}
	if _, err := c.StartAll(ctx, "pve-1", nil); err == nil {
		t.Error("StartAll(nil) = nil error, want a refusal")
	}
	if _, err := c.MigrateAll(ctx, "pve-1", "pve-2", nil); err == nil {
		t.Error("MigrateAll(nil) = nil error, want a refusal")
	}
}

func TestWaitTaskFailure(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"migration aborted"}}`))
	})
	if err := c.WaitTask(context.Background(), "pve-1", "UPID:x"); err == nil {
		t.Fatal("expected task failure error, got nil")
	}
}

func TestShutdownNode(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("command") != "shutdown" {
			t.Errorf("command = %q", r.Form.Get("command"))
		}
		_, _ = w.Write([]byte(`{"data":null}`))
	})
	if err := c.ShutdownNode(context.Background(), "pve-1"); err != nil {
		t.Fatal(err)
	}
}

// TestReplicationJobs covers the shapes Proxmox passes the disable flag through as: a
// number, a string, a JSON bool, and absent entirely (an enabled job).
func TestReplicationJobs(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/replication" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"104-0","source":"pve-2","target":"pve-1","disable":1,"comment":"nightly"},
			{"id":"107-0","source":"pve-3","target":"pve-1"},
			{"id":"108-0","source":"pve-1","target":"pve-2","disable":"1"},
			{"id":"109-0","source":"pve-3","target":"pve-2","disable":true},
			{"id":"110-0","source":"pve-2","target":"pve-3","disable":0}
		]}`))
	})
	jobs, err := c.ReplicationJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 5 {
		t.Fatalf("got %d jobs, want 5", len(jobs))
	}
	want := map[string]bool{"104-0": true, "107-0": false, "108-0": true, "109-0": true, "110-0": false}
	for _, j := range jobs {
		if j.Disabled != want[j.ID] {
			t.Errorf("job %s disabled = %v, want %v", j.ID, j.Disabled, want[j.ID])
		}
	}
	if jobs[0].Source != "pve-2" || jobs[0].Target != "pve-1" || jobs[0].Comment != "nightly" {
		t.Errorf("job 104-0 = %+v", jobs[0])
	}
}

// TestSetReplicationJob asserts we send exactly what "pvesr disable/enable" sends, and that
// clearing a comment deletes the key instead of writing an empty string.
func TestSetReplicationJob(t *testing.T) {
	for _, tc := range []struct {
		name          string
		comment       string
		disable       bool
		wantDisable   string
		wantComment   string
		wantDelete    string
		wantNoComment bool
	}{
		{name: "disable with comment", comment: "[energy-watchdog] nightly", disable: true, wantDisable: "1", wantComment: "[energy-watchdog] nightly"},
		{name: "enable clearing comment", comment: "", disable: false, wantDisable: "0", wantDelete: "comment", wantNoComment: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut {
					t.Errorf("method = %s, want PUT", r.Method)
				}
				if r.URL.Path != "/api2/json/cluster/replication/104-0" {
					t.Errorf("path = %s", r.URL.Path)
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if got := r.Form.Get("disable"); got != tc.wantDisable {
					t.Errorf("disable = %q, want %q", got, tc.wantDisable)
				}
				if got := r.Form.Get("delete"); got != tc.wantDelete {
					t.Errorf("delete = %q, want %q", got, tc.wantDelete)
				}
				if tc.wantNoComment {
					if _, ok := r.Form["comment"]; ok {
						t.Errorf("comment should not be sent when clearing, form = %v", r.Form)
					}
				} else if got := r.Form.Get("comment"); got != tc.wantComment {
					t.Errorf("comment = %q, want %q", got, tc.wantComment)
				}
				_, _ = w.Write([]byte(`{"data":null}`))
			})
			if err := c.SetReplicationJob(context.Background(), "104-0", tc.comment, tc.disable); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestErrorStatus(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`permission denied`))
	})
	if _, _, err := c.NodeState(context.Background(), "pve-1"); err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

func TestGPUKey(t *testing.T) {
	tests := []struct {
		name, config, want string
	}{
		{"mapped device", `{"hostpci0":"mapping=gpu0,pcie=1,x-vga=1","cores":8}`, "gpu0"},
		{"raw pci address", `{"hostpci0":"0000:01:00,pcie=1","memory":16384}`, "0000:01:00"},
		// hostpci0 is the GPU by convention; a second device must not change the key.
		{"lowest hostpci wins", `{"hostpci1":"mapping=nic","hostpci0":"mapping=gpu0"}`, "gpu0"},
		// Numeric, not lexicographic: "hostpci10" sorts before "hostpci2" as a string.
		{"double-digit index", `{"hostpci10":"mapping=nic","hostpci2":"mapping=gpu0"}`, "gpu0"},
		{"no passthrough", `{"cores":4,"memory":8192}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":` + tt.config + `}`))
			})
			got, err := c.GPUKey(context.Background(), "pve-1", Guest{VMID: 601, Type: TypeQEMU})
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("GPUKey = %q, want %q", got, tt.want)
			}
		})
	}
}

// Proxmox has no reset for containers, so it must be refused before it becomes a mid-click 501.
func TestPowerRejectsResetOnLXC(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("reset on an LXC reached the server: %s", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":"UPID:x"}`))
	})
	if _, err := c.Power(context.Background(), "pve-1", Guest{VMID: 301, Type: TypeLXC}, PowerReset); err == nil {
		t.Error("Power(reset, lxc) = nil error, want a refusal")
	}
}

func TestPowerPathPerAction(t *testing.T) {
	for _, action := range []GuestPower{PowerShutdown, PowerReboot, PowerReset, PowerStop} {
		t.Run(string(action), func(t *testing.T) {
			c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if want := "/api2/json/nodes/pve-1/qemu/601/status/" + string(action); r.URL.Path != want {
					t.Errorf("path = %q, want %q", r.URL.Path, want)
				}
				_, _ = w.Write([]byte(`{"data":"UPID:` + string(action) + `"}`))
			})
			if _, err := c.Power(context.Background(), "pve-1", Guest{VMID: 601, Type: TypeQEMU}, action); err != nil {
				t.Fatal(err)
			}
		})
	}
}
