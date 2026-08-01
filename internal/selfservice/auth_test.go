package selfservice

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

func testAuth() *oidcAuth {
	return &oidcAuth{
		key: []byte("test-signing-key"), ttl: time.Hour,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestSessionCookieRoundTrip(t *testing.T) {
	a := testAuth()
	want := sessionData{User: "josef", Groups: []string{"deskvms-admins"}, Expires: time.Now().Add(time.Hour).Unix()}
	enc, err := a.encode(want)
	if err != nil {
		t.Fatal(err)
	}
	var got sessionData
	if err := a.decode(enc, &got); err != nil {
		t.Fatal(err)
	}
	if got.User != want.User || len(got.Groups) != 1 || got.Groups[0] != "deskvms-admins" {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
}

// The whole point of signing: a user must not be able to promote themselves by editing the
// cookie, and the app must not accept a cookie signed with a different key.
func TestSessionCookieCannotBeForged(t *testing.T) {
	a := testAuth()
	enc, err := a.encode(sessionData{User: "josef", Groups: []string{"deskvms-josef"}, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	payload, sig, _ := strings.Cut(enc, ".")

	// Swap the payload for one claiming admin, keeping the original signature.
	forged, err := a.encode(sessionData{User: "josef", Groups: []string{"deskvms-admins"}, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	forgedPayload, _, _ := strings.Cut(forged, ".")

	for name, cookie := range map[string]string{
		"payload swapped":  forgedPayload + "." + sig,
		"signature junk":   payload + ".AAAA",
		"no separator":     payload,
		"empty":            "",
		"signed elsewhere": mustEncode(t, &oidcAuth{key: []byte("different-key")}, sessionData{User: "mallory", Groups: []string{"deskvms-admins"}, Expires: time.Now().Add(time.Hour).Unix()}),
	} {
		t.Run(name, func(t *testing.T) {
			var got sessionData
			if err := a.decode(cookie, &got); err == nil {
				t.Errorf("forged cookie accepted as %+v", got)
			}
		})
	}
}

func mustEncode(t *testing.T, a *oidcAuth, v any) string {
	t.Helper()
	s, err := a.encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestExpiredSessionIsRejected(t *testing.T) {
	a := testAuth()
	enc, err := a.encode(sessionData{User: "josef", Expires: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: enc})
	if _, err := a.Authenticate(r); err == nil {
		t.Error("expired session accepted")
	}
}

// A login response whose state doesn't match the one we issued is a CSRF attempt, or at best
// a stale tab. Either way it must not produce a session.
func TestCallbackRejectsStateMismatch(t *testing.T) {
	a := testAuth()
	// ready() needs both halves. Neither is actually used: the state check rejects first, so
	// a bare config and a verifier over a nil key set are enough to get past readiness.
	a.oauth = &oauth2.Config{}
	a.verifier = oidc.NewVerifier("https://auth.example", nil, &oidc.Config{ClientID: "x"})

	flow, err := a.encode(flowData{State: "expected", Verifier: "v", Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=attacker&code=abc", nil)
	r.AddCookie(&http.Cookie{Name: flowCookie, Value: flow})
	w := httptest.NewRecorder()
	a.completeLogin(w, r)

	// Must be rejected *at the state check* (400). Anything else - notably 502 from a failed
	// token exchange - means we got past it and the check isn't doing the work.
	if w.Code != http.StatusBadRequest {
		t.Errorf("state mismatch = %d, want 400 (rejected before the exchange)", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Error("state mismatch still set a session cookie")
		}
	}
}

// The post-login redirect must stay on this site.
func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "/",
		"/":                    "/",
		"/?x=1":                "/?x=1",
		"//evil.example":       "/",
		"https://evil.example": "/",
		"evil":                 "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

// A half-initialised authenticator must report not-ready rather than being used: completeLogin
// dereferences the verifier, so "ready" with a nil verifier would panic on a real login.
func TestReadyNeedsBothHalves(t *testing.T) {
	a := testAuth()
	if a.Ready() {
		t.Error("zero authenticator reported ready")
	}
	a.oauth = &oauth2.Config{}
	if a.Ready() {
		t.Error("ready with a nil verifier")
	}
	a.verifier = oidc.NewVerifier("https://auth.example", nil, &oidc.Config{ClientID: "x"})
	if !a.Ready() {
		t.Error("not ready with both halves set")
	}
}

// returnTo comes from the query string, so it is normalised before being signed into the
// flow cookie - not just at redirect time.
func TestStartLoginNormalisesReturn(t *testing.T) {
	a := testAuth()
	a.oauth = &oauth2.Config{ClientID: "x", Endpoint: oauth2.Endpoint{AuthURL: "https://auth.example/authorize"}}
	a.verifier = oidc.NewVerifier("https://auth.example", nil, &oidc.Config{ClientID: "x"})

	for _, in := range []string{"https://evil.example", "//evil.example", strings.Repeat("/a", maxReturn)} {
		w := httptest.NewRecorder()
		a.startLogin(w, httptest.NewRequest(http.MethodGet, "/auth/login", nil), in)

		var flow flowData
		var found bool
		for _, c := range w.Result().Cookies() {
			if c.Name != flowCookie {
				continue
			}
			found = true
			if err := a.decode(c.Value, &flow); err != nil {
				t.Fatalf("flow cookie for %q: %v", in, err)
			}
		}
		if !found {
			t.Fatalf("no flow cookie set for %q", in)
		}
		if flow.Return != "/" {
			t.Errorf("returnTo %q stored as %q, want /", in, flow.Return)
		}
	}
}
