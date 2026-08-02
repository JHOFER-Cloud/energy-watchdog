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
	"github.com/JHOFER-Cloud/energy-watchdog/internal/proxmox"
	"github.com/JHOFER-Cloud/energy-watchdog/internal/state"
)

// Cluster is the read-only Proxmox view the UI needs. Reads don't contend with the reconcile
// loop, so the API may query them directly; every write still goes through intent.
type Cluster interface {
	NodeState(ctx context.Context, node string) (bool, time.Duration, error)
	Guests(ctx context.Context, node string) ([]proxmox.Guest, error)
}

// Store is the intent half of the state store.
type Store interface {
	LoadIntent(ctx context.Context) (state.Intent, error)
	SaveIntent(ctx context.Context, i state.Intent) error
}

// Nudger asks the reconcile loop to run now.
type Nudger interface{ Nudge() }

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
	mux.HandleFunc("POST /api/admin/shed", s.handleShed)
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
	}
	s.cached = v
	return v
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
}

type statusResponse struct {
	User       string      `json:"user"`
	Admin      bool        `json:"admin"`
	NodeUp     bool        `json:"nodeUp"`
	ManualShed bool        `json:"manualShed"`
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
	now := time.Now()
	requested := map[int]bool{}
	for _, req := range intent.LiveWake(now, s.cfg.GamingGrace.Duration) {
		requested[req.VMID] = true
	}

	resp := statusResponse{
		User:       id.User,
		Admin:      s.cfg.SelfService.IsAdmin(id.Groups),
		NodeUp:     v.nodeUp,
		ManualShed: intent.Shed,
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
		st.Phase = phase(v, st.Running, st.Requested)
		resp.VMs = append(resp.VMs, st)
	}
	writeJSON(w, http.StatusOK, resp)
}

// phase is the copy the UI shows. "waking" covers the case the user actually notices: they
// asked for a VM while p1 was on its way down, so the request is live but the host is gone
// until the shutdown finishes and the loop wakes it again.
func phase(v clusterView, running, requested bool) string {
	switch {
	case running:
		return "ready"
	case requested && !v.nodeUp:
		return "waking"
	case requested:
		return "starting"
	default:
		return "off"
	}
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	vmid, err := strconv.Atoi(r.PathValue("vmid"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "bad vmid", err)
		return
	}
	// Authorise against the caller's own allowed set, never against the id alone.
	var allowed bool
	for _, vm := range s.cfg.SelfService.Allowed(id.Groups) {
		if vm.VMID == vmid {
			allowed = true
			break
		}
	}
	if !allowed {
		s.log.Warn("self-service: denied VM start", "user", id.User, "vmid", vmid)
		http.Error(w, "not allowed to control this VM", http.StatusForbidden)
		return
	}

	intent, err := s.store.LoadIntent(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "read intent", err)
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

func (s *Server) handleShed(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	if !s.cfg.SelfService.IsAdmin(id.Groups) {
		s.log.Warn("self-service: denied shed toggle", "user", id.User)
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var body struct {
		Shed *bool `json:"shed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.Shed == nil {
		http.Error(w, `want {"shed": true|false}`, http.StatusBadRequest)
		return
	}
	intent, err := s.store.LoadIntent(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "read intent", err)
		return
	}
	intent.Shed = *body.Shed
	if err := s.store.SaveIntent(r.Context(), intent); err != nil {
		s.fail(w, http.StatusInternalServerError, "save intent", err)
		return
	}
	s.log.Warn("self-service: manual shed toggled", "user", id.User, "shed", intent.Shed)
	s.nudge.Nudge()
	writeJSON(w, http.StatusOK, map[string]any{"manualShed": intent.Shed})
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
