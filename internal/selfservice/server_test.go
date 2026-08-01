package selfservice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
	"gopkg.in/yaml.v3"
)

type fakeCluster struct {
	up     bool
	guests []proxmox.Guest
	err    error
}

func (f *fakeCluster) NodeState(context.Context, string) (bool, time.Duration, error) {
	return f.up, time.Hour, f.err
}

func (f *fakeCluster) Guests(context.Context, string) ([]proxmox.Guest, error) {
	return f.guests, f.err
}

type fakeStore struct {
	intent state.Intent
	saves  int
}

func (f *fakeStore) LoadIntent(context.Context) (state.Intent, error) { return f.intent, nil }

func (f *fakeStore) SaveIntent(_ context.Context, i state.Intent) error {
	f.intent = i
	f.saves++
	return nil
}

type fakeNudge struct{ n int }

func (f *fakeNudge) Nudge() { f.n++ }

// denyAuth stands in for a caller with no valid session.
type denyAuth struct{}

func (denyAuth) Ready() bool { return true }

func (denyAuth) Authenticate(*http.Request) (Identity, error) {
	return Identity{}, errNoSession
}

// Page requests get bounced to the login; the tests assert on that separately.
func (denyAuth) startLogin(w http.ResponseWriter, r *http.Request, _ string) {
	http.Redirect(w, r, "https://auth.example/authorize", http.StatusFound)
}

func (denyAuth) completeLogin(w http.ResponseWriter, r *http.Request) {}

func (denyAuth) logout(w http.ResponseWriter, r *http.Request) {}

func testServer(t *testing.T, auth authenticator, cluster *fakeCluster, store *fakeStore) (*Server, *fakeNudge) {
	t.Helper()
	var g config.Guests
	if err := yaml.Unmarshal([]byte("gamingGuard: [\"600-699\"]\n"), &g); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Guests:  g,
		Proxmox: config.Proxmox{Node: "pve-1"},
		SelfService: config.SelfService{
			Addr:        ":8080",
			AdminGroups: []string{"jhc-admins"},
			VMs: []config.SelfServiceVM{
				{VMID: 601, Name: "josef-desktop", Groups: []string{"deskvm-josef"}, StreamHost: "p1.lan"},
				{VMID: 602, Name: "other-desktop", Groups: []string{"deskvm-other"}},
			},
		},
	}
	nudge := &fakeNudge{}
	s := &Server{
		cfg: cfg, auth: auth, cluster: cluster, store: store, nudge: nudge,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return s, nudge
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestStatusListsOnlyPermittedVMs(t *testing.T) {
	auth := staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}
	s, _ := testServer(t, auth, &fakeCluster{up: true, guests: []proxmox.Guest{{VMID: 601, Running: false}}}, &fakeStore{})

	w := do(t, s, http.MethodGet, "/api/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var got statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.VMs) != 1 || got.VMs[0].VMID != 601 {
		t.Errorf("vms = %+v, want only 601", got.VMs)
	}
	if got.Admin {
		t.Error("non-admin reported as admin")
	}
	if got.VMs[0].Phase != "off" {
		t.Errorf("phase = %q, want off", got.VMs[0].Phase)
	}
}

func TestStartRecordsIntentAndNudges(t *testing.T) {
	auth := staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}
	store := &fakeStore{}
	s, nudge := testServer(t, auth, &fakeCluster{up: false}, store)

	w := do(t, s, http.MethodPost, "/api/vms/601/start", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("start = %d: %s", w.Code, w.Body)
	}
	if len(store.intent.Wake) != 1 || store.intent.Wake[0].VMID != 601 || store.intent.Wake[0].User != "josef" {
		t.Errorf("intent = %+v", store.intent)
	}
	if nudge.n != 1 {
		t.Errorf("nudges = %d, want 1", nudge.n)
	}
	// The API must never touch p1 itself - recording intent is the whole job.
	if store.saves != 1 {
		t.Errorf("saves = %d, want 1", store.saves)
	}
}

func TestStartDeniedForOtherPeoplesVM(t *testing.T) {
	auth := staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}
	store := &fakeStore{}
	s, nudge := testServer(t, auth, &fakeCluster{up: true}, store)

	w := do(t, s, http.MethodPost, "/api/vms/602/start", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("start of someone else's VM = %d, want 403", w.Code)
	}
	if len(store.intent.Wake) != 0 || nudge.n != 0 {
		t.Errorf("denied request still had an effect: %+v, nudges %d", store.intent, nudge.n)
	}
}

func TestRepeatClickExtendsRatherThanPilesUp(t *testing.T) {
	auth := staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}
	store := &fakeStore{}
	s, _ := testServer(t, auth, &fakeCluster{up: false}, store)

	do(t, s, http.MethodPost, "/api/vms/601/start", "")
	do(t, s, http.MethodPost, "/api/vms/601/start", "")
	if len(store.intent.Wake) != 1 {
		t.Errorf("wake = %+v, want a single entry", store.intent.Wake)
	}
}

func TestShedToggleIsAdminOnly(t *testing.T) {
	store := &fakeStore{}
	user, _ := testServer(t, staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}, &fakeCluster{up: true}, store)
	if w := do(t, user, http.MethodPost, "/api/admin/shed", `{"shed":true}`); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin shed = %d, want 403", w.Code)
	}
	if store.intent.Shed {
		t.Fatal("non-admin toggled the shed")
	}

	admin, nudge := testServer(t, staticAuth{User: "josef", Groups: []string{"jhc-admins"}}, &fakeCluster{up: true}, store)
	if w := do(t, admin, http.MethodPost, "/api/admin/shed", `{"shed":true}`); w.Code != http.StatusOK {
		t.Fatalf("admin shed = %d", w.Code)
	}
	if !store.intent.Shed {
		t.Error("admin toggle did not stick")
	}
	if nudge.n != 1 {
		t.Errorf("nudges = %d, want 1", nudge.n)
	}
}

// No session means no handler runs. A browser navigating gets sent to the login; the page's
// own fetch() calls get a 401 they can surface, since redirecting those to authentik would
// fail CORS and just look like a hang.
func TestUnauthenticatedIsRejectedEverywhere(t *testing.T) {
	store := &fakeStore{}
	s, nudge := testServer(t, denyAuth{}, &fakeCluster{up: true}, store)

	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/", "", http.StatusFound},
		{http.MethodGet, "/api/status", "", http.StatusUnauthorized},
		{http.MethodPost, "/api/vms/601/start", "", http.StatusUnauthorized},
		{http.MethodPost, "/api/admin/shed", `{"shed":true}`, http.StatusUnauthorized},
	} {
		if w := do(t, s, tc.method, tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, w.Code, tc.want)
		}
	}
	if store.saves != 0 || nudge.n != 0 {
		t.Errorf("unauthenticated request had an effect: saves %d, nudges %d", store.saves, nudge.n)
	}
}

func TestPhaseCopy(t *testing.T) {
	tests := []struct {
		name      string
		nodeUp    bool
		running   bool
		requested bool
		want      string
	}{
		{"nothing asked for", false, false, false, "off"},
		{"asked while p1 is down", false, false, true, "waking"},
		{"p1 up, VM still booting", true, false, true, "starting"},
		{"VM running", true, true, true, "ready"},
		{"running without a request", true, true, false, "ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := phase(clusterView{nodeUp: tt.nodeUp}, tt.running, tt.requested)
			if got != tt.want {
				t.Errorf("phase = %q, want %q", got, tt.want)
			}
		})
	}
}

// Proxmox being unreachable must degrade to a message, not a 500: the page still has to
// render so an admin can toggle the shed.
func TestStatusSurvivesProxmoxOutage(t *testing.T) {
	auth := staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}
	s, _ := testServer(t, auth, &fakeCluster{err: fmt.Errorf("502 bad gateway")}, &fakeStore{})

	w := do(t, s, http.MethodGet, "/api/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an error field", w.Code)
	}
	var got statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error == "" {
		t.Error("outage not reported to the UI")
	}
}
