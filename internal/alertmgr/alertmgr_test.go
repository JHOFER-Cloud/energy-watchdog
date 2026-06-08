package alertmgr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
)

func TestCreateAndDelete(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &gotBody)
			_, _ = w.Write([]byte(`{"silenceID":"abc-123"}`))
		case http.MethodDelete:
			if r.URL.Path != "/api/v2/silences/abc-123" {
				t.Errorf("delete path = %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	now := time.Date(2026, 6, 8, 22, 0, 0, 0, time.UTC)
	id, err := c.Create(context.Background(),
		[]config.Matcher{{Name: "instance", Value: "pve-1.*", IsRegex: true}},
		"shutdown", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if id != "abc-123" {
		t.Errorf("id = %q, want abc-123", id)
	}
	if gotBody["createdBy"] != "energy-watchdog" {
		t.Errorf("createdBy = %v", gotBody["createdBy"])
	}
	if gotBody["startsAt"] != "2026-06-08T22:00:00Z" || gotBody["endsAt"] != "2026-06-08T23:00:00Z" {
		t.Errorf("times = %v / %v", gotBody["startsAt"], gotBody["endsAt"])
	}

	if err := c.Delete(context.Background(), "abc-123"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestDeleteNotFoundIsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if err := New(srv.URL).Delete(context.Background(), "gone"); err != nil {
		t.Errorf("Delete of missing silence should be nil, got %v", err)
	}
}
