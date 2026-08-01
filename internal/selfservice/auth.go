// Package selfservice serves the desktop-VM self-service UI and its API (JHC-548). It only
// ever records intent for the reconcile loop to act on, so the loop stays the sole owner of
// p1's power state.
package selfservice

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Identity is the signed-in user.
type Identity struct {
	User   string
	Groups []string
}

const (
	sessionCookie = "energy_watchdog_session"
	flowCookie    = "energy_watchdog_flow"
	// flowTTL bounds how long a half-finished login may sit around.
	flowTTL = 10 * time.Minute
)

// oidcAuth runs the authorization-code flow against authentik and keeps the result in a
// signed cookie. The app does its own login, like every other service in the fleet, so
// nothing in the request path has to be trusted to inject identity headers.
type oidcAuth struct {
	issuer       string
	clientID     string
	clientSecret string
	redirectURL  string
	key          []byte // HMAC key for the session and flow cookies
	ttl          time.Duration
	secure       bool
	log          *slog.Logger

	mu       sync.RWMutex
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// newOIDCAuth resolves the issuer in the background: authentik being down must never stop the
// watchdog shedding or waking p1, so the UI degrades to 503 while the reconcile loop runs on.
func newOIDCAuth(ctx context.Context, issuer, clientID, clientSecret, redirectURL string, key []byte, ttl time.Duration, log *slog.Logger) *oidcAuth {
	a := &oidcAuth{
		issuer: issuer, clientID: clientID, clientSecret: clientSecret,
		redirectURL: redirectURL, key: key, ttl: ttl,
		secure: strings.HasPrefix(redirectURL, "https://"),
		log:    log,
	}
	go a.resolve(ctx)
	return a
}

func (a *oidcAuth) resolve(ctx context.Context) {
	for attempt := 0; ; attempt++ {
		provider, err := oidc.NewProvider(ctx, a.issuer)
		if err == nil {
			a.mu.Lock()
			a.verifier = provider.Verifier(&oidc.Config{ClientID: a.clientID})
			a.oauth = &oauth2.Config{
				ClientID:     a.clientID,
				ClientSecret: a.clientSecret,
				Endpoint:     provider.Endpoint(),
				RedirectURL:  a.redirectURL,
				Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
			}
			a.mu.Unlock()
			a.log.Info("self-service: OIDC issuer resolved", "issuer", a.issuer)
			return
		}
		a.log.Error("self-service: OIDC discovery failed, retrying", "issuer", a.issuer, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff(attempt)):
		}
	}
}

func backoff(attempt int) time.Duration {
	return min(time.Duration(1<<min(attempt, 6))*time.Second, time.Minute)
}

func (a *oidcAuth) ready() (*oauth2.Config, *oidc.IDTokenVerifier, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.oauth, a.verifier, a.oauth != nil
}

// Ready reports whether OIDC discovery has completed.
func (a *oidcAuth) Ready() bool { _, _, ok := a.ready(); return ok }

// Authenticate returns the identity carried by the session cookie.
func (a *oidcAuth) Authenticate(r *http.Request) (Identity, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Identity{}, errNoSession
	}
	var s sessionData
	if err := a.decode(c.Value, &s); err != nil {
		return Identity{}, err
	}
	if time.Now().Unix() > s.Expires {
		return Identity{}, errNoSession
	}
	return Identity{User: s.User, Groups: s.Groups}, nil
}

type sessionData struct {
	User    string   `json:"user"`
	Groups  []string `json:"groups"`
	Expires int64    `json:"exp"`
}

// flowData is the in-flight login, kept in a short-lived cookie rather than server memory so
// a restart doesn't strand a login half-way through.
type flowData struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Return   string `json:"return"`
	Expires  int64  `json:"exp"`
}

// encode returns payload.signature, both base64url. The signature covers the payload, so a
// cookie can't be edited to change the user or their groups.
func (a *oidcAuth) encode(v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	b := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(b))
	return b + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (a *oidcAuth) decode(s string, v any) error {
	b, sig, ok := strings.Cut(s, ".")
	if !ok {
		return fmt.Errorf("malformed cookie")
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(b))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || subtle.ConstantTimeCompare(got, mac.Sum(nil)) != 1 {
		return fmt.Errorf("bad cookie signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}

func (a *oidcAuth) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/",
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
		MaxAge: int(ttl.Seconds()),
	})
}

func (a *oidcAuth) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/",
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// startLogin sends the browser to authentik. State and the PKCE verifier travel in a signed
// cookie, so the callback can prove the response belongs to a login this server began.
func (a *oidcAuth) startLogin(w http.ResponseWriter, r *http.Request, returnTo string) {
	cfg, _, ok := a.ready()
	if !ok {
		http.Error(w, "authentication is not ready yet", http.StatusServiceUnavailable)
		return
	}
	state, err := randomString()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	flow, err := a.encode(flowData{
		State: state, Verifier: verifier, Return: returnTo,
		Expires: time.Now().Add(flowTTL).Unix(),
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.setCookie(w, flowCookie, flow, flowTTL)
	http.Redirect(w, r, cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// completeLogin handles the redirect back from authentik.
func (a *oidcAuth) completeLogin(w http.ResponseWriter, r *http.Request) {
	cfg, verifier, ok := a.ready()
	if !ok {
		http.Error(w, "authentication is not ready yet", http.StatusServiceUnavailable)
		return
	}
	c, err := r.Cookie(flowCookie)
	if err != nil {
		http.Error(w, "no login in progress", http.StatusBadRequest)
		return
	}
	a.clearCookie(w, flowCookie)

	var flow flowData
	if err := a.decode(c.Value, &flow); err != nil {
		a.log.Warn("self-service: bad login flow cookie", "err", err)
		http.Error(w, "invalid login", http.StatusBadRequest)
		return
	}
	// Expiry bounds a stale flow; the state comparison is what stops a third party feeding us
	// an authorization code of their choosing (CSRF).
	if time.Now().Unix() > flow.Expires {
		http.Error(w, "login expired, try again", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(flow.State), []byte(r.URL.Query().Get("state"))) != 1 {
		a.log.Warn("self-service: login state mismatch", "remote", r.RemoteAddr)
		http.Error(w, "invalid login", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		a.log.Warn("self-service: authentik refused the login", "error", e)
		http.Error(w, "login refused: "+e, http.StatusForbidden)
		return
	}

	tok, err := cfg.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		a.log.Error("self-service: token exchange failed", "err", err)
		http.Error(w, "login failed", http.StatusBadGateway)
		return
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token in response", http.StatusBadGateway)
		return
	}
	idToken, err := verifier.Verify(r.Context(), rawID)
	if err != nil {
		a.log.Error("self-service: id_token verification failed", "err", err)
		http.Error(w, "login failed", http.StatusBadGateway)
		return
	}
	var claims struct {
		Username string   `json:"preferred_username"`
		Email    string   `json:"email"`
		Groups   []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "login failed", http.StatusBadGateway)
		return
	}
	user := firstNonEmpty(claims.Username, claims.Email, idToken.Subject)
	session, err := a.encode(sessionData{
		User: user, Groups: claims.Groups, Expires: time.Now().Add(a.ttl).Unix(),
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.setCookie(w, sessionCookie, session, a.ttl)
	a.log.Info("self-service: signed in", "user", user, "groups", claims.Groups)
	http.Redirect(w, r, safeReturn(flow.Return), http.StatusFound)
}

func (a *oidcAuth) logout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/", http.StatusFound)
}

// safeReturn keeps the post-login redirect on this site: an attacker-supplied absolute URL
// would turn the login into an open redirect.
func safeReturn(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "/"
	}
	return p
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

var (
	errNoSession = fmt.Errorf("not signed in")
	errNotReady  = fmt.Errorf("authentication is not ready yet")
)

// staticAuth is the local-development identity, only ever constructed behind the -dev-user
// flag in main, so a production binary has no path to it.
type staticAuth Identity

func (s staticAuth) Ready() bool { return true }

func (s staticAuth) Authenticate(*http.Request) (Identity, error) { return Identity(s), nil }

func (s staticAuth) startLogin(w http.ResponseWriter, r *http.Request, _ string) {
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s staticAuth) completeLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s staticAuth) logout(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusFound)
}

// authenticator is what the handlers need, so tests and -dev-user can supply their own.
type authenticator interface {
	Ready() bool
	Authenticate(r *http.Request) (Identity, error)
	startLogin(w http.ResponseWriter, r *http.Request, returnTo string)
	completeLogin(w http.ResponseWriter, r *http.Request)
	logout(w http.ResponseWriter, r *http.Request)
}
