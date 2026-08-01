// Package selfservice serves the desktop-VM self-service UI and its API (JHC-548). It only
// ever records intent for the reconcile loop to act on, so the loop stays the sole owner of
// p1's power state.
package selfservice

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Identity is the caller, as proven by the authentik token.
type Identity struct {
	User   string
	Groups []string
}

// authHeader carries the signed assertion the authentik forward-auth outpost adds. The
// unsigned X-authentik-username/groups headers are deliberately ignored: anything that can
// reach this port could set them.
const authHeader = "X-authentik-Jwt"

// Authenticator verifies the forwarded token against authentik's published signing keys.
//
// The keys are fetched from the issuer's discovery document, never from the request. The
// outpost also forwards X-authentik-meta-jwks, but trusting that would be circular - a forged
// request would simply supply its own key alongside its own token.
type Authenticator struct {
	issuer   string
	clientID string
	log      *slog.Logger

	mu       sync.RWMutex
	verifier *oidc.IDTokenVerifier
}

// NewAuthenticator returns an authenticator that resolves the issuer in the background.
// Discovery is not done synchronously on purpose: authentik being down must never stop the
// watchdog from shedding or waking p1, so the API degrades to 503 while the core loop runs on.
func NewAuthenticator(ctx context.Context, issuer, clientID string, log *slog.Logger) *Authenticator {
	a := &Authenticator{issuer: issuer, clientID: clientID, log: log}
	go a.resolve(ctx)
	return a
}

func (a *Authenticator) resolve(ctx context.Context) {
	for attempt := 0; ; attempt++ {
		provider, err := oidc.NewProvider(ctx, a.issuer)
		if err == nil {
			a.mu.Lock()
			a.verifier = provider.Verifier(&oidc.Config{ClientID: a.clientID})
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
	d := time.Duration(1<<min(attempt, 6)) * time.Second
	return min(d, time.Minute)
}

// Ready reports whether discovery has completed.
func (a *Authenticator) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.verifier != nil
}

// Authenticate verifies the request's token and returns who it belongs to. go-oidc checks the
// signature, issuer, audience and expiry, and rejects the alg-confusion cases.
func (a *Authenticator) Authenticate(r *http.Request) (Identity, error) {
	a.mu.RLock()
	v := a.verifier
	a.mu.RUnlock()
	if v == nil {
		return Identity{}, errNotReady
	}
	raw := r.Header.Get(authHeader)
	if raw == "" {
		return Identity{}, fmt.Errorf("no %s header: is the request going through the authentik middleware?", authHeader)
	}
	tok, err := v.Verify(r.Context(), raw)
	if err != nil {
		return Identity{}, fmt.Errorf("verify token: %w", err)
	}
	var claims struct {
		Username string   `json:"preferred_username"`
		Email    string   `json:"email"`
		Groups   []string `json:"groups"`
	}
	if err := tok.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("decode claims: %w", err)
	}
	user := claims.Username
	if user == "" {
		user = claims.Email
	}
	if user == "" {
		user = tok.Subject
	}
	return Identity{User: user, Groups: claims.Groups}, nil
}

var errNotReady = fmt.Errorf("authentication is not ready yet")

// staticAuth is the local-development identity. It is only ever constructed behind the -dev
// flag in main, so a production binary has no path to it.
type staticAuth Identity

func (s staticAuth) Ready() bool { return true }

func (s staticAuth) Authenticate(*http.Request) (Identity, error) { return Identity(s), nil }

// authenticator is what the handlers need, so tests and -dev can supply their own.
type authenticator interface {
	Ready() bool
	Authenticate(r *http.Request) (Identity, error)
}
