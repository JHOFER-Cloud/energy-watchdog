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
	"github.com/JHOFER-Cloud/energy-watchdog/internal/controller"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
	"gopkg.in/yaml.v3"
)

type fakeCluster struct {
	up     bool
	guests []proxmox.Guest
	err    error
	gpu    map[int]string // vmid -> passthrough GPU key
	power  []string       // "<vmid>:<action>", in call order
}

func (f *fakeCluster) NodeState(context.Context, string) (bool, time.Duration, error) {
	return f.up, time.Hour, f.err
}

func (f *fakeCluster) Guests(context.Context, string) ([]proxmox.Guest, error) {
	return f.guests, f.err
}

func (f *fakeCluster) GPUKey(_ context.Context, _ string, g proxmox.Guest) (string, error) {
	return f.gpu[g.VMID], nil
}

func (f *fakeCluster) Power(_ context.Context, _ string, g proxmox.Guest, a proxmox.GuestPower) (string, error) {
	f.power = append(f.power, fmt.Sprintf("%d:%s", g.VMID, a))
	return "UPID:power", f.err
}

type fakeStore struct {
	intent state.Intent
	st     state.State
	saves  int
}

func (f *fakeStore) Load(context.Context) (state.State, error) { return f.st, nil }

func (f *fakeStore) LoadIntent(context.Context) (state.Intent, error) { return f.intent, nil }

func (f *fakeStore) SaveIntent(_ context.Context, i state.Intent) error {
	f.intent = i
	f.saves++
	return nil
}

type fakeNudge struct {
	n        int
	activity string
	failed   map[int]controller.StartFailure
}

func (f *fakeNudge) Nudge() { f.n++ }

func (f *fakeNudge) Activity() string { return f.activity }

func (f *fakeNudge) StartFailures() map[int]controller.StartFailure { return f.failed }

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
		Guests: g,
		// Wake requests are filtered by this, so a zero here silently makes every request in
		// a fixture dead on arrival.
		GamingGrace: config.Duration{Duration: 10 * time.Minute},
		Proxmox:     config.Proxmox{Node: "pve-1"},
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

func TestHoldIsAdminOnly(t *testing.T) {
	store := &fakeStore{}
	user, _ := testServer(t, staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}, &fakeCluster{up: true}, store)
	if w := do(t, user, http.MethodPost, "/api/admin/hold", `{"hold":"shed"}`); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin hold = %d, want 403", w.Code)
	}
	if store.intent.Shed {
		t.Fatal("non-admin set the hold")
	}

	admin, nudge := testServer(t, staticAuth{User: "josef", Groups: []string{"jhc-admins"}}, &fakeCluster{up: true}, store)
	if w := do(t, admin, http.MethodPost, "/api/admin/hold", `{"hold":"shed"}`); w.Code != http.StatusOK {
		t.Fatalf("admin hold = %d", w.Code)
	}
	if !store.intent.Shed {
		t.Error("admin hold did not stick")
	}
	if nudge.n != 1 {
		t.Errorf("nudges = %d, want 1", nudge.n)
	}
}

// The three positions are mutually exclusive by construction: one endpoint writes both flags,
// so no sequence of clicks can leave p1 held off and on at once.
func TestHoldPositionsAreExclusive(t *testing.T) {
	store := &fakeStore{}
	admin, _ := testServer(t, staticAuth{User: "josef", Groups: []string{"jhc-admins"}}, &fakeCluster{up: true}, store)

	for _, tc := range []struct {
		hold             string
		wantShed, wantOn bool
	}{
		{"shed", true, false},
		{"on", false, true},
		{"solar", false, false},
	} {
		if w := do(t, admin, http.MethodPost, "/api/admin/hold", `{"hold":"`+tc.hold+`"}`); w.Code != http.StatusOK {
			t.Fatalf("hold %s = %d", tc.hold, w.Code)
		}
		if store.intent.Shed != tc.wantShed || store.intent.On != tc.wantOn {
			t.Errorf("hold %s: shed=%v on=%v, want shed=%v on=%v",
				tc.hold, store.intent.Shed, store.intent.On, tc.wantShed, tc.wantOn)
		}
	}
	if w := do(t, admin, http.MethodPost, "/api/admin/hold", `{"hold":"off"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown hold = %d, want 400", w.Code)
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
		{http.MethodPost, "/api/admin/hold", `{"hold":"shed"}`, http.StatusUnauthorized},
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
		failed    bool
		want      string
	}{
		{"nothing asked for", false, false, false, false, "off"},
		{"asked while p1 is down", false, false, true, false, "waking"},
		{"p1 up, VM still booting", true, false, true, false, "starting"},
		{"VM running", true, true, true, false, "ready"},
		{"running without a request", true, true, false, false, "ready"},
		{"gave up starting it", true, false, false, true, "failed"},
		// A VM that came up after the loop gave up reads as ready: the user started it by
		// hand, and "couldn't start" over a running VM is just wrong.
		{"failed but running anyway", true, true, false, true, "ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := phase(clusterView{nodeUp: tt.nodeUp}, "", tt.running, tt.requested, tt.failed)
			if got != tt.want {
				t.Errorf("phase = %q, want %q", got, tt.want)
			}
		})
	}
}

// The loop having given up has to reach the user. Leaving the request reading as outstanding
// would keep the card on "Starting…" with the button disabled until the TTL ran out, which is
// the state that left editing the ConfigMap as the only way out.
func TestStatusReportsAVMTheLoopGaveUpOn(t *testing.T) {
	auth := staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}
	at := time.Now().Unix()
	store := &fakeStore{intent: state.Intent{
		Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: at}},
	}}
	s, nudge := testServer(t, auth, &fakeCluster{up: true, guests: []proxmox.Guest{{VMID: 601}}}, store)
	nudge.failed = map[int]controller.StartFailure{
		601: {Err: "task UPID:x failed: timeout waiting on systemd", RequestedAt: at},
	}

	var got statusResponse
	if err := json.Unmarshal(do(t, s, http.MethodGet, "/api/status", "").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	vm := got.VMs[0]
	if vm.Phase != "failed" {
		t.Errorf("phase = %q, want failed", vm.Phase)
	}
	if vm.Requested {
		t.Error("a request the loop has retired still reads as outstanding")
	}
	if !strings.Contains(vm.Error, "timeout waiting on systemd") {
		t.Errorf("error = %q, want the reason in it", vm.Error)
	}
}

// A VM that came up anyway - started by hand after the loop gave up - is running, and must
// read that way rather than carrying the stale failure.
func TestARunningVMDropsItsRecordedFailure(t *testing.T) {
	auth := staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}
	at := time.Now().Unix()
	store := &fakeStore{intent: state.Intent{
		Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: at}},
	}}
	s, nudge := testServer(t, auth,
		&fakeCluster{up: true, guests: []proxmox.Guest{{VMID: 601, Running: true}}}, store)
	nudge.failed = map[int]controller.StartFailure{601: {Err: "timeout waiting on systemd", RequestedAt: at}}

	var got statusResponse
	if err := json.Unmarshal(do(t, s, http.MethodGet, "/api/status", "").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.VMs[0].Phase != "ready" || got.VMs[0].Error != "" {
		t.Errorf("vm = %+v, want ready with no error", got.VMs[0])
	}
}

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

func decode(t *testing.T, w *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
}

// bothVMs is a caller allowed both desktop VMs, which is what makes a GPU conflict reachable.
func bothVMs(t *testing.T, cluster *fakeCluster, store *fakeStore) (*Server, *fakeNudge) {
	t.Helper()
	s, nudge := testServer(t, staticAuth{User: "josef", Groups: []string{"deskvm-josef", "deskvm-other"}}, cluster, store)
	s.cfg.SelfService.VMs[1].Groups = []string{"deskvm-other", "deskvm-josef"}
	return s, nudge
}

// Proxmox maps a GPU into one guest at a time, so starting the second is a guaranteed failure.
// It has to be refused up front, not left to fail somewhere in the reconcile loop.
func TestStartBlockedByGPUInUse(t *testing.T) {
	cluster := &fakeCluster{up: true, gpu: map[int]string{601: "gpu0", 602: "gpu0"}, guests: []proxmox.Guest{
		{VMID: 601, Type: proxmox.TypeQEMU, Running: true},
		{VMID: 602, Type: proxmox.TypeQEMU},
	}}
	store := &fakeStore{}
	s, _ := bothVMs(t, cluster, store)

	if rec := do(t, s, "POST", "/api/vms/602/start", ""); rec.Code != http.StatusConflict {
		t.Errorf("start = %d, want 409 while 601 holds the GPU", rec.Code)
	}
	if store.saves != 0 {
		t.Error("a blocked start must not record a wake request")
	}

	// The status feed has to say so too, or the button is enabled and the click just 409s.
	var resp statusResponse
	decode(t, do(t, s, "GET", "/api/status", ""), &resp)
	for _, vm := range resp.VMs {
		want := ""
		if vm.VMID == 602 {
			want = "josef-desktop"
		}
		if vm.BlockedBy != want {
			t.Errorf("vm %d blockedBy = %q, want %q", vm.VMID, vm.BlockedBy, want)
		}
	}
}

// While p1 is off both VMs look equally stopped, so a request has to count as holding the GPU
// too - otherwise two clicks in a row both get through and the second VM fails to boot.
func TestStartBlockedByGPURequestedWhileNodeDown(t *testing.T) {
	cluster := &fakeCluster{up: true, gpu: map[int]string{601: "gpu0", 602: "gpu0"}, guests: []proxmox.Guest{
		{VMID: 601, Type: proxmox.TypeQEMU},
		{VMID: 602, Type: proxmox.TypeQEMU},
	}}
	store := &fakeStore{}
	s, _ := bothVMs(t, cluster, store)

	if rec := do(t, s, "POST", "/api/vms/601/start", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("first start = %d, want 202", rec.Code)
	}
	cluster.up = false // p1 goes down to be woken; 601's request is still outstanding
	s.expireView()
	if rec := do(t, s, "POST", "/api/vms/602/start", ""); rec.Code != http.StatusConflict {
		t.Errorf("second start = %d, want 409: 601 already claimed the GPU", rec.Code)
	}
}

// Different GPUs are not a conflict; the guard must not block the unrelated VM.
func TestStartNotBlockedByADifferentGPU(t *testing.T) {
	cluster := &fakeCluster{up: true, gpu: map[int]string{601: "gpu0", 602: "gpu1"}, guests: []proxmox.Guest{
		{VMID: 601, Type: proxmox.TypeQEMU, Running: true},
		{VMID: 602, Type: proxmox.TypeQEMU},
	}}
	s, _ := bothVMs(t, cluster, &fakeStore{})
	if rec := do(t, s, "POST", "/api/vms/602/start", ""); rec.Code != http.StatusAccepted {
		t.Errorf("start = %d, want 202: the VMs are on different GPUs", rec.Code)
	}
}

func TestPowerActions(t *testing.T) {
	for _, action := range []string{"shutdown", "reboot", "reset", "stop"} {
		t.Run(action, func(t *testing.T) {
			cluster := &fakeCluster{up: true, guests: []proxmox.Guest{{VMID: 601, Type: proxmox.TypeQEMU, Running: true}}}
			s, _ := bothVMs(t, cluster, &fakeStore{})
			if rec := do(t, s, "POST", "/api/vms/601/power/"+action, ""); rec.Code != http.StatusAccepted {
				t.Fatalf("%s = %d, want 202", action, rec.Code)
			}
			if len(cluster.power) != 1 || cluster.power[0] != "601:"+action {
				t.Errorf("proxmox calls = %v, want [601:%s]", cluster.power, action)
			}
		})
	}
}

// The flaw in reverse: shutting a VM down has to retire its wake request, or the loop sees a
// live request against a stopped VM and starts it straight back up.
func TestShutdownRetiresTheWakeRequest(t *testing.T) {
	cluster := &fakeCluster{up: true, guests: []proxmox.Guest{
		{VMID: 601, Type: proxmox.TypeQEMU, Running: true},
		{VMID: 602, Type: proxmox.TypeQEMU, Running: true},
	}}
	store := &fakeStore{intent: state.Intent{Wake: []state.WakeRequest{
		{VMID: 601, User: "josef", RequestedAt: time.Now().Unix()},
		{VMID: 602, User: "josef", RequestedAt: time.Now().Unix()},
	}}}
	s, _ := bothVMs(t, cluster, store)

	if rec := do(t, s, "POST", "/api/vms/601/power/shutdown", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("shutdown = %d, want 202", rec.Code)
	}
	if len(store.intent.Wake) != 1 || store.intent.Wake[0].VMID != 602 {
		t.Errorf("wake = %+v, want only 602's request left", store.intent.Wake)
	}
}

// Reboot keeps the VM up, so its request must survive - dropping it would end the session.
func TestRebootKeepsTheWakeRequest(t *testing.T) {
	cluster := &fakeCluster{up: true, guests: []proxmox.Guest{{VMID: 601, Type: proxmox.TypeQEMU, Running: true}}}
	store := &fakeStore{intent: state.Intent{Wake: []state.WakeRequest{
		{VMID: 601, User: "josef", RequestedAt: time.Now().Unix()},
	}}}
	s, _ := bothVMs(t, cluster, store)

	if rec := do(t, s, "POST", "/api/vms/601/power/reboot", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("reboot = %d, want 202", rec.Code)
	}
	if len(store.intent.Wake) != 1 {
		t.Errorf("wake = %+v, want 601's request kept across a reboot", store.intent.Wake)
	}
}

// A caller may only act on VMs their own groups allow, for power actions as much as for start.
func TestPowerRequiresAuthorisation(t *testing.T) {
	cluster := &fakeCluster{up: true, guests: []proxmox.Guest{{VMID: 602, Type: proxmox.TypeQEMU, Running: true}}}
	s, _ := testServer(t, staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}, cluster, &fakeStore{})
	if rec := do(t, s, "POST", "/api/vms/602/power/stop", ""); rec.Code != http.StatusForbidden {
		t.Errorf("stop on someone else's VM = %d, want 403", rec.Code)
	}
	if len(cluster.power) != 0 {
		t.Errorf("proxmox was called anyway: %v", cluster.power)
	}
}

// A request the loop has already seen through to a running VM is spent: nothing will start the
// VM again, so the UI must show it as off rather than sitting on "starting" until the TTL runs
// out. This is what the user sees after shutting the VM down from inside the guest.
func TestSpentRequestStopsShowingAsStarting(t *testing.T) {
	clicked := time.Now().Unix()
	cluster := &fakeCluster{up: true, guests: []proxmox.Guest{{VMID: 601, Type: proxmox.TypeQEMU}}}
	store := &fakeStore{
		intent: state.Intent{Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: clicked}}},
		st:     state.State{WakeDone: map[int]int64{601: clicked}},
	}
	s, _ := bothVMs(t, cluster, store)

	var resp statusResponse
	decode(t, do(t, s, "GET", "/api/status", ""), &resp)
	for _, vm := range resp.VMs {
		if vm.VMID != 601 {
			continue
		}
		if vm.Requested || vm.Phase != "off" {
			t.Errorf("601 = requested %v phase %q, want false/off: the request is spent",
				vm.Requested, vm.Phase)
		}
	}
}

// A request the loop gave up on is as dead as a spent one, and must release the GPU on the
// same grounds - all the more so when the GPU conflict is why the start failed.
func TestRetiredRequestReleasesTheGPU(t *testing.T) {
	clicked := time.Now().Unix()
	cluster := &fakeCluster{up: true, gpu: map[int]string{601: "gpu0", 602: "gpu0"}, guests: []proxmox.Guest{
		{VMID: 601, Type: proxmox.TypeQEMU},
		{VMID: 602, Type: proxmox.TypeQEMU},
	}}
	store := &fakeStore{
		intent: state.Intent{Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: clicked}}},
	}
	s, nudge := bothVMs(t, cluster, store)
	nudge.failed = map[int]controller.StartFailure{601: {Err: "no free GPU", RequestedAt: clicked}}

	if rec := do(t, s, "POST", "/api/vms/602/start", ""); rec.Code != http.StatusAccepted {
		t.Errorf("start = %d, want 202: 601's request was given up on and its VM is down", rec.Code)
	}
}

// Pressing Start again must read as a wake in progress, not as the failure it replaced. The
// loop retries on the new request, so reporting the old error over it says the click did
// nothing - and while p1 is off the next attempt is minutes away behind the wake.
func TestAFreshRequestIsNotPaintedWithTheOldFailure(t *testing.T) {
	old := time.Now().Add(-time.Minute).Unix()
	store := &fakeStore{intent: state.Intent{
		Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: old + 30}},
	}}
	s, nudge := testServer(t, staticAuth{User: "josef", Groups: []string{"deskvm-josef"}},
		&fakeCluster{up: false}, store)
	nudge.failed = map[int]controller.StartFailure{601: {Err: "timeout waiting on systemd", RequestedAt: old}}

	var got statusResponse
	decode(t, do(t, s, "GET", "/api/status", ""), &got)
	if got.VMs[0].Error != "" {
		t.Errorf("error = %q, want none: this failure belongs to a request already replaced", got.VMs[0].Error)
	}
	if !got.VMs[0].Requested || got.VMs[0].Phase != "waking" {
		t.Errorf("vm = %+v, want an outstanding request waking p1", got.VMs[0])
	}
}

// Once the failing request ages out there is nothing left to explain, so the card must not
// keep the amber "Couldn't start" for the rest of the process's life.
func TestAFailureDiesWithItsRequest(t *testing.T) {
	stale := time.Now().Add(-time.Hour).Unix()
	store := &fakeStore{intent: state.Intent{
		Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: stale}},
	}}
	s, nudge := testServer(t, staticAuth{User: "josef", Groups: []string{"deskvm-josef"}},
		&fakeCluster{up: true, guests: []proxmox.Guest{{VMID: 601}}}, store)
	nudge.failed = map[int]controller.StartFailure{601: {Err: "timeout waiting on systemd", RequestedAt: stale}}

	var got statusResponse
	decode(t, do(t, s, "GET", "/api/status", ""), &got)
	if got.VMs[0].Phase != "off" || got.VMs[0].Error != "" {
		t.Errorf("vm = %+v, want a plain off card once the request has aged out", got.VMs[0])
	}
}

// A spent request must not keep holding the GPU either, or the other VM stays blocked forever.
func TestSpentRequestReleasesTheGPU(t *testing.T) {
	clicked := time.Now().Unix()
	cluster := &fakeCluster{up: true, gpu: map[int]string{601: "gpu0", 602: "gpu0"}, guests: []proxmox.Guest{
		{VMID: 601, Type: proxmox.TypeQEMU},
		{VMID: 602, Type: proxmox.TypeQEMU},
	}}
	store := &fakeStore{
		intent: state.Intent{Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: clicked}}},
		st:     state.State{WakeDone: map[int]int64{601: clicked}},
	}
	s, _ := bothVMs(t, cluster, store)
	if rec := do(t, s, "POST", "/api/vms/602/start", ""); rec.Code != http.StatusAccepted {
		t.Errorf("start = %d, want 202: 601's request is spent and its VM is down", rec.Code)
	}
}

// Proxmox reports the node online for the whole shed, so without the loop's own activity the
// UI claims the VM is starting at a host on its way down.
func TestRequestDuringAShedSaysSo(t *testing.T) {
	cluster := &fakeCluster{up: true, guests: []proxmox.Guest{{VMID: 601, Type: proxmox.TypeQEMU}}}
	store := &fakeStore{intent: state.Intent{
		Shed: true,
		Wake: []state.WakeRequest{{VMID: 601, User: "josef", RequestedAt: time.Now().Unix()}},
	}}
	s, nudge := testServer(t, staticAuth{User: "josef", Groups: []string{"deskvm-josef"}}, cluster, store)

	nudge.activity = "shedding"
	var resp statusResponse
	decode(t, do(t, s, "GET", "/api/status", ""), &resp)
	if resp.VMs[0].Phase != "shedding" {
		t.Errorf("phase = %q, want shedding: p1 reports online but is mid-shutdown", resp.VMs[0].Phase)
	}

	// Idle loop, same node state: this really is a VM starting on a host that is staying up.
	nudge.activity = ""
	decode(t, do(t, s, "GET", "/api/status", ""), &resp)
	if resp.VMs[0].Phase != "starting" {
		t.Errorf("phase = %q, want starting when no shed is running", resp.VMs[0].Phase)
	}
}
