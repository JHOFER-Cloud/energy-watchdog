package selfservice

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/controller"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// Cluster is the read-only Proxmox view the UI needs. Reads don't contend with the reconcile
// loop, so the API may query them directly; every write still goes through intent.
type Cluster interface {
	NodeState(ctx context.Context, node string) (bool, time.Duration, error)
	Guests(ctx context.Context, node string) ([]proxmox.Guest, error)
	GPUKey(ctx context.Context, node string, g proxmox.Guest) (string, error)
	Power(ctx context.Context, node string, g proxmox.Guest, action proxmox.GuestPower) (string, error)
}

// Store is the intent half of the state store, plus a read of the loop's own state: a request
// the loop has already seen through to a running VM is spent, and the UI must not keep
// reporting it as still starting.
type Store interface {
	Load(ctx context.Context) (state.State, error)
	LoadIntent(ctx context.Context) (state.Intent, error)
	SaveIntent(ctx context.Context, i state.Intent) error
}

// Nudger asks the reconcile loop to run now, and reports what it is currently doing.
type Nudger interface {
	Nudge()
	Activity() string
}

// Server serves the UI and its API.
type Server struct {
	cfg     *config.Config
	auth    authenticator
	cluster Cluster
	store   Store
	nudge   Nudger
	log     *slog.Logger

	mu     sync.Mutex
	cached clusterView
	// gpu maps a desktop VM to the GPU passed through to it, re-read on every refresh while p1
	// is up and kept after it goes down - which is exactly when the UI has to spot a conflict.
	gpu map[int]string
}

// New builds a Server that signs users in against authentik itself.
func New(ctx context.Context, cfg *config.Config, cluster Cluster, store Store, nudge Nudger, log *slog.Logger) *Server {
	ss := cfg.SelfService
	auth := newOIDCAuth(ctx, ss.IssuerURL, ss.ClientID, ss.ClientSecret, ss.RedirectURL(),
		[]byte(ss.SessionKey), ss.SessionTTL.Duration, log)
	return &Server{cfg: cfg, auth: auth, cluster: cluster, store: store, nudge: nudge, log: log}
}

// NewDev builds a Server that skips SSO and treats every caller as the given identity. For
// local runs against the fake Proxmox only; main gates it behind an explicit flag.
func NewDev(cfg *config.Config, cluster Cluster, store Store, nudge Nudger, log *slog.Logger, user string, groups []string) *Server {
	log.Warn("self-service: SSO disabled, every request is treated as this user", "user", user, "groups", groups)
	return &Server{
		cfg: cfg, auth: staticAuth{User: user, Groups: groups},
		cluster: cluster, store: store, nudge: nudge, log: log,
	}
}

// Handler returns the mux serving the page and the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("GET /favicon.svg", s.handleFavicon)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /auth/login", func(w http.ResponseWriter, r *http.Request) {
		s.auth.startLogin(w, r, r.URL.Query().Get("return"))
	})
	mux.HandleFunc("GET /auth/callback", s.auth.completeLogin)
	mux.HandleFunc("POST /auth/logout", s.auth.logout)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/vms/{vmid}/start", s.handleStart)
	mux.HandleFunc("POST /api/vms/{vmid}/power/{action}", s.handlePower)
	mux.HandleFunc("POST /api/admin/hold", s.handleHold)
	return mux
}

// clusterView is a short-lived cache of the Proxmox reads, so a room full of browsers polling
// every two seconds doesn't turn into the same rate of Proxmox API calls.
type clusterView struct {
	nodeUp  bool
	running map[int]bool
	present map[int]bool
	guests  []proxmox.Guest
	at      time.Time
	err     error
}

const viewTTL = 3 * time.Second

func (s *Server) view(ctx context.Context) clusterView {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.cached.at) < viewTTL {
		return s.cached
	}
	v := clusterView{at: time.Now(), running: map[int]bool{}, present: map[int]bool{}}
	node := s.cfg.Proxmox.Node
	up, _, err := s.cluster.NodeState(ctx, node)
	if err != nil {
		v.err = err
		s.cached = v
		return v
	}
	v.nodeUp = up
	if up {
		guests, err := s.cluster.Guests(ctx, node)
		if err != nil {
			v.err = err
			s.cached = v
			return v
		}
		v.guests = guests
		for _, g := range guests {
			v.present[g.VMID] = true
			v.running[g.VMID] = g.Running
		}
		s.refreshGPUs(ctx, guests)
	}
	s.cached = v
	return v
}

// refreshGPUs re-reads which GPU each desktop VM has, on every refresh while p1 is up - so
// remapping a passthrough device takes effect within one poll. While p1 is down the last
// reading stands, which is sound because a guest's config can't change while its node is off.
func (s *Server) refreshGPUs(ctx context.Context, guests []proxmox.Guest) {
	byID := make(map[int]proxmox.Guest, len(guests))
	for _, g := range guests {
		byID[g.VMID] = g
	}
	next := map[int]string{}
	for _, vm := range s.cfg.SelfService.VMs {
		g, ok := byID[vm.VMID]
		if !ok {
			continue
		}
		key, err := s.cluster.GPUKey(ctx, s.cfg.Proxmox.Node, g)
		if err != nil {
			// Keep the previous reading rather than publishing a half-built one.
			s.log.Warn("read GPU passthrough config", "vmid", vm.VMID, "err", err)
			return
		}
		if key != "" {
			next[vm.VMID] = key
		}
	}
	s.gpu = next
}

// liveRequests is the desktop VMs with a wake request still waiting to be acted on. A request
// whose VM has already been up is spent - it will never start anything again - so counting it
// would leave the UI stuck on "starting" until the TTL ran out.
func (s *Server) liveRequests(ctx context.Context, intent state.Intent) map[int]bool {
	var done map[int]int64
	if st, err := s.store.Load(ctx); err != nil {
		s.log.Warn("read state", "err", err)
	} else {
		done = st.WakeDone
	}
	out := map[int]bool{}
	for _, req := range intent.LiveWake(time.Now(), s.cfg.GamingGrace.Duration) {
		if req.RequestedAt > done[req.VMID] {
			out[req.VMID] = true
		}
	}
	return out
}

// gpuBlocker reports the VM currently holding vmid's GPU, if any. Proxmox can only map a GPU
// into one guest at a time, so starting the second is a guaranteed failure. A VM that's only
// been requested counts: while p1 is off both look equally stopped.
func (s *Server) gpuBlocker(vmid int, running, requested map[int]bool) (int, bool) {
	key, ok := s.gpu[vmid]
	if !ok {
		return 0, false
	}
	for other, otherKey := range s.gpu {
		if other != vmid && otherKey == key && (running[other] || requested[other]) {
			return other, true
		}
	}
	return 0, false
}

// vmStatus is one desktop VM as the UI sees it.
type vmStatus struct {
	VMID       int    `json:"vmid"`
	Name       string `json:"name"`
	Running    bool   `json:"running"`
	Requested  bool   `json:"requested"`
	StreamHost string `json:"streamHost,omitempty"`
	// Phase drives the UI copy: off, waking, starting, ready.
	Phase string `json:"phase"`
	// BlockedBy names the VM holding this one's GPU; empty when it is free to start.
	BlockedBy string `json:"blockedBy,omitempty"`
}

type statusResponse struct {
	User       string      `json:"user"`
	Admin      bool        `json:"admin"`
	NodeUp     bool        `json:"nodeUp"`
	Activity   string      `json:"activity,omitempty"`
	ManualShed bool        `json:"manualShed"`
	ManualOn   bool        `json:"manualOn"`
	VMs        []vmStatus  `json:"vms"`
	Impact     *shedImpact `json:"impact,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// shedImpact is what holding p1 off would actually do right now, so the admin confirms
// against real numbers instead of a vague warning. Only sent to admins.
type shedImpact struct {
	Migrate int `json:"migrate"`
	Stop    int `json:"stop"`
	// GamingActive means a desktop VM is running, so the gaming guard vetoes the power-off
	// and the host stays up with its load shed around it.
	GamingActive bool `json:"gamingActive"`
}

func (s *Server) impact(v clusterView) *shedImpact {
	if !v.nodeUp {
		return &shedImpact{}
	}
	var i shedImpact
	for _, g := range v.guests {
		if !g.Running {
			continue
		}
		switch {
		case s.cfg.Guests.Migrate.Contains(g.VMID):
			i.Migrate++
		case s.cfg.Guests.Stop.Contains(g.VMID):
			i.Stop++
		case s.cfg.Guests.GamingGuard.Contains(g.VMID):
			i.GamingActive = true
		}
	}
	return &i
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	intent, err := s.store.LoadIntent(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "read intent", err)
		return
	}
	v := s.view(r.Context())
	requested := s.liveRequests(r.Context(), intent)
	manualShed, manualOn := intent.Holds()

	resp := statusResponse{
		User:       id.User,
		Admin:      s.cfg.SelfService.IsAdmin(id.Groups),
		NodeUp:     v.nodeUp,
		Activity:   s.nudge.Activity(),
		ManualShed: manualShed,
		ManualOn:   manualOn,
	}
	if v.err != nil {
		resp.Error = "cannot reach Proxmox right now"
	}
	if resp.Admin && v.err == nil {
		resp.Impact = s.impact(v)
	}
	for _, vm := range s.cfg.SelfService.Allowed(id.Groups) {
		st := vmStatus{VMID: vm.VMID, Name: vm.Name, StreamHost: vm.StreamHost}
		st.Running = v.running[vm.VMID]
		st.Requested = requested[vm.VMID]
		st.Phase = phase(v, resp.Activity, st.Running, st.Requested)
		if other, blocked := s.gpuBlocker(vm.VMID, v.running, requested); blocked && !st.Running {
			st.BlockedBy = s.cfg.SelfService.NameOf(other)
		}
		resp.VMs = append(resp.VMs, st)
	}
	writeJSON(w, http.StatusOK, resp)
}

// phase is the copy the UI shows. "shedding" is a request made while the shed is still running:
// Proxmox reports the node online throughout, so only the loop's activity gives it away.
func phase(v clusterView, activity string, running, requested bool) string {
	switch {
	case running:
		return "ready"
	case requested && !v.nodeUp:
		return "waking"
	case requested && activity == controller.ActivityShedding:
		return "shedding"
	case requested:
		return "starting"
	default:
		return "off"
	}
}

// authorise resolves the path's vmid against the caller's own allowed set, never against the
// id alone, and writes the error response itself if the caller may not touch it.
func (s *Server) authorise(w http.ResponseWriter, r *http.Request, what string) (Identity, int, bool) {
	id, ok := s.identify(w, r)
	if !ok {
		return id, 0, false
	}
	vmid, err := strconv.Atoi(r.PathValue("vmid"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "bad vmid", err)
		return id, 0, false
	}
	for _, vm := range s.cfg.SelfService.Allowed(id.Groups) {
		if vm.VMID == vmid {
			return id, vmid, true
		}
	}
	s.log.Warn("self-service: denied VM action", "user", id.User, "vmid", vmid, "action", what)
	http.Error(w, "not allowed to control this VM", http.StatusForbidden)
	return id, 0, false
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	id, vmid, ok := s.authorise(w, r, "start")
	if !ok {
		return
	}
	// Proxmox maps a GPU into one guest at a time, so starting the second is a guaranteed
	// failure. Refuse it here with the reason instead of letting the loop fail on it later.
	intent, err := s.store.LoadIntent(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "read intent", err)
		return
	}
	v := s.view(r.Context())
	if other, blocked := s.gpuBlocker(vmid, v.running, s.liveRequests(r.Context(), intent)); blocked {
		http.Error(w, s.cfg.SelfService.NameOf(other)+" is using this GPU; shut it down first",
			http.StatusConflict)
		return
	}
	now := time.Now()
	// Keep only live requests for other VMs, then record this one afresh so a repeat click
	// extends the window rather than piling up entries.
	var kept []state.WakeRequest
	for _, req := range intent.LiveWake(now, s.cfg.GamingGrace.Duration) {
		if req.VMID != vmid {
			kept = append(kept, req)
		}
	}
	intent.Wake = append(kept, state.WakeRequest{VMID: vmid, User: id.User, RequestedAt: now.Unix()})
	if err := s.store.SaveIntent(r.Context(), intent); err != nil {
		s.fail(w, http.StatusInternalServerError, "save intent", err)
		return
	}
	s.log.Info("self-service: desktop VM requested", "user", id.User, "vmid", vmid)
	s.nudge.Nudge()
	writeJSON(w, http.StatusAccepted, map[string]any{"vmid": vmid, "requested": true})
}

// powerActions is what the UI may ask for. Start is not here: it goes through intent so the
// reconcile loop can wake p1 first, while these go straight to Proxmox.
var powerActions = map[string]proxmox.GuestPower{
	"shutdown": proxmox.PowerShutdown,
	"reboot":   proxmox.PowerReboot,
	"reset":    proxmox.PowerReset,
	"stop":     proxmox.PowerStop,
}

// handlePower runs a power action straight against Proxmox. That doesn't break p1's single
// owner: the loop owns the *node's* power, and a guest action can't turn p1 on or off.
// Whether the guest is in a state that accepts the action is Proxmox's call, not ours.
func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	action, ok := powerActions[r.PathValue("action")]
	if !ok {
		http.Error(w, "unknown power action", http.StatusNotFound)
		return
	}
	id, vmid, ok := s.authorise(w, r, string(action))
	if !ok {
		return
	}
	v := s.view(r.Context())
	if !v.nodeUp {
		http.Error(w, "p1 is off, so this VM is already down", http.StatusConflict)
		return
	}
	var guest proxmox.Guest
	var found bool
	for _, g := range v.guests {
		if g.VMID == vmid {
			guest, found = g, true
		}
	}
	if !found {
		http.Error(w, "VM is not on this node", http.StatusConflict)
		return
	}

	// Taking the VM down has to retire its wake request too, or the loop would see a live
	// request against a stopped VM and start it straight back up.
	if action == proxmox.PowerShutdown || action == proxmox.PowerStop {
		if err := s.dropWakeRequest(r.Context(), vmid); err != nil {
			s.fail(w, http.StatusInternalServerError, "clear wake request", err)
			return
		}
	}
	if _, err := s.cluster.Power(r.Context(), s.cfg.Proxmox.Node, guest, action); err != nil {
		s.fail(w, http.StatusBadGateway, "proxmox "+string(action), err)
		return
	}
	s.log.Info("self-service: guest power action", "user", id.User, "vmid", vmid, "action", action)
	s.expireView()
	writeJSON(w, http.StatusAccepted, map[string]any{"vmid": vmid, "action": string(action)})
}

// dropWakeRequest removes a VM's outstanding request, keeping every other VM's intact.
func (s *Server) dropWakeRequest(ctx context.Context, vmid int) error {
	intent, err := s.store.LoadIntent(ctx)
	if err != nil {
		return err
	}
	var kept []state.WakeRequest
	for _, req := range intent.LiveWake(time.Now(), s.cfg.GamingGrace.Duration) {
		if req.VMID != vmid {
			kept = append(kept, req)
		}
	}
	intent.Wake = kept
	return s.store.SaveIntent(ctx, intent)
}

// expireView drops the cached Proxmox read so the next poll shows the action taking effect
// rather than up to viewTTL of stale state.
func (s *Server) expireView() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached.at = time.Time{}
}

// holds are the three positions of the admin control, as the pair of intent flags each one
// sets. Going through one endpoint is what stops the UI ever setting both.
var holds = map[string]struct{ shed, on bool }{
	"shed":  {shed: true},
	"on":    {on: true},
	"solar": {},
}

func (s *Server) handleHold(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	if !s.cfg.SelfService.IsAdmin(id.Groups) {
		s.log.Warn("self-service: denied hold change", "user", id.User)
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var body struct {
		Hold string `json:"hold"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		http.Error(w, `want {"hold": "shed"|"on"|"solar"}`, http.StatusBadRequest)
		return
	}
	want, ok := holds[body.Hold]
	if !ok {
		http.Error(w, `want {"hold": "shed"|"on"|"solar"}`, http.StatusBadRequest)
		return
	}
	intent, err := s.store.LoadIntent(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "read intent", err)
		return
	}
	intent.Shed, intent.On = want.shed, want.on
	if err := s.store.SaveIntent(r.Context(), intent); err != nil {
		s.fail(w, http.StatusInternalServerError, "save intent", err)
		return
	}
	s.log.Warn("self-service: hold changed", "user", id.User, "hold", body.Hold)
	s.nudge.Nudge()
	writeJSON(w, http.StatusOK, map[string]any{"manualShed": intent.Shed, "manualOn": intent.On})
}

// identify authenticates the caller, writing the error response itself when it fails. A
// browser hitting the page gets sent to the login; an API call gets a 401 to handle, since
// redirecting fetch() to authentik would just fail CORS and look like a hang.
func (s *Server) identify(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	id, err := s.auth.Authenticate(r)
	if err == nil {
		return id, true
	}
	if errors.Is(err, errNotReady) || !s.auth.Ready() {
		http.Error(w, "authentication not ready", http.StatusServiceUnavailable)
		return Identity{}, false
	}
	if !errors.Is(err, errNoSession) {
		s.log.Warn("self-service: rejected request", "err", err, "remote", r.RemoteAddr)
	}
	if isPageRequest(r) {
		s.auth.startLogin(w, r, r.URL.RequestURI())
		return Identity{}, false
	}
	http.Error(w, "not signed in", http.StatusUnauthorized)
	return Identity{}, false
}

// isPageRequest reports whether this is a browser navigating, rather than the page's own
// fetch() calls.
func isPageRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/")
}

func (s *Server) fail(w http.ResponseWriter, code int, what string, err error) {
	s.log.Error("self-service: "+what, "err", err)
	http.Error(w, what, code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
