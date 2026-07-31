package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "user@pam!tok", "secret", nil)
	return c
}

func TestNodeUp(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=user@pam!tok=secret" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"node":"pve-1","status":"offline"},{"node":"pve-2","status":"online"}]}`))
	})
	up, err := c.NodeUp(context.Background(), "pve-1")
	if err != nil {
		t.Fatal(err)
	}
	if up {
		t.Error("pve-1 should be offline")
	}
	up, _ = c.NodeUp(context.Background(), "pve-2")
	if !up {
		t.Error("pve-2 should be online")
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

func TestMigrateAndWaitTask(t *testing.T) {
	const upid = "UPID:pve-1:00001:migrate"
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/migrate"):
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("target") != "pve-2" || r.Form.Get("online") != "1" {
				t.Errorf("migrate form = %v", r.Form)
			}
			_, _ = w.Write([]byte(`{"data":"` + upid + `"}`))
		case strings.Contains(r.URL.Path, "/tasks/"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	got, err := c.Migrate(context.Background(), "pve-1", Guest{VMID: 101, Type: TypeQEMU}, "pve-2")
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
	if _, err := c.NodeUp(context.Background(), "pve-1"); err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}
