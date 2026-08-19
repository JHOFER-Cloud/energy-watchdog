package powerapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestSendsTheWishAndTheToken(t *testing.T) {
	var gotPath, gotAuth, gotDesired, gotReason string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		var body struct{ Desired, Reason string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotDesired, gotReason = body.Desired, body.Reason
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := New(srv.URL, "t0ken", "p1").Request(context.Background(), Off, "solar"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/loads/p1/power" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer t0ken" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotDesired != "off" || gotReason != "solar" {
		t.Errorf("body = {%q, %q}, want {off, solar}", gotDesired, gotReason)
	}
}

// A refused request must surface: p1's power runs entirely through this call, so a wrong token
// or load name would otherwise fail with every gauge still green.
func TestRequestSurfacesARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unknown load", http.StatusNotFound)
	}))
	defer srv.Close()

	if err := New(srv.URL, "t0ken", "p1").Request(context.Background(), Off, "solar"); err == nil {
		t.Error("a 404 came back as success")
	}
}

func TestStateReadsTheProbeAndItsAge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/loads/p1/state" || r.Method != http.MethodGet {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"actual":"up","ageSeconds":7}`))
	}))
	defer srv.Close()

	actual, age, err := New(srv.URL, "t0ken", "p1").State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if actual != ActualUp {
		t.Errorf("actual = %q, want up", actual)
	}
	if age != 7*time.Second {
		t.Errorf("age = %v, want 7s", age)
	}
}

// An unrecognised state is returned as received rather than flattened to "unknown": the two
// services declare this vocabulary independently, and the caller can only report a drift if it
// still sees the word.
func TestStateReportsAnUnknownWordAsItself(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"actual":"powered-on","ageSeconds":1}`))
	}))
	defer srv.Close()

	actual, _, err := New(srv.URL, "t0ken", "p1").State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if actual != "powered-on" {
		t.Errorf("actual = %q, want it passed through so the caller can report the drift", actual)
	}
}

// An older nut-dog has no such endpoint; a 404 must be an error rather than an empty success,
// which would be indistinguishable from a probe with no opinion.
func TestStateSurfacesAMissingEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, _, err := New(srv.URL, "t0ken", "p1").State(context.Background()); err == nil {
		t.Error("a 404 came back as a usable reading")
	}
}
