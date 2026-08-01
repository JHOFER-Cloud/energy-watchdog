package state

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewFileStore(path)
	ctx := context.Background()

	// Missing file -> fresh running state.
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != ModeRunning {
		t.Errorf("fresh mode = %q, want running", got.Mode)
	}

	want := State{
		Mode:    ModeShed,
		Stopped: []GuestRef{{VMID: 301, Type: "qemu"}, {VMID: 311, Type: "lxc"}},
	}
	if err := s.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != want.Mode || len(got.Stopped) != 2 {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
	if got.Stopped[0].VMID != 301 || got.Stopped[1].Type != "lxc" {
		t.Errorf("stopped = %+v", got.Stopped)
	}
}

func TestFileStoreIntentRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewFileStore(path)
	ctx := context.Background()

	got, err := s.LoadIntent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Shed || len(got.Wake) != 0 {
		t.Errorf("fresh intent = %+v, want zero", got)
	}

	want := Intent{Shed: true, Wake: []WakeRequest{{VMID: 601, User: "josef", RequestedAt: 42}}}
	if err := s.SaveIntent(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadIntent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Shed || len(got.Wake) != 1 || got.Wake[0].VMID != 601 || got.Wake[0].User != "josef" {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}

	// Intent lives beside state, so saving one must not disturb the other.
	if err := s.Save(ctx, State{Mode: ModeShed}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.LoadIntent(ctx); err != nil || !got.Shed {
		t.Errorf("state save clobbered intent: %+v (err %v)", got, err)
	}
}

// fakeAPI is a minimal ConfigMap endpoint: GET returns the map, PATCH merges into data,
// POST creates. Enough to exercise the merge-patch split for real.
func fakeAPI(t *testing.T, data map[string]string) (*ConfigMapStore, func() map[string]string) {
	t.Helper()
	exists := data != nil
	if data == nil {
		data = map[string]string{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(configMap{Data: data})
		case http.MethodPatch:
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
				t.Errorf("patch content-type = %q", ct)
			}
			body, _ := io.ReadAll(r.Body)
			var patch struct {
				Data map[string]string `json:"data"`
			}
			if err := json.Unmarshal(body, &patch); err != nil {
				t.Fatalf("patch body: %v", err)
			}
			for k, v := range patch.Data { // merge, don't replace
				data[k] = v
			}
			_ = json.NewEncoder(w).Encode(configMap{Data: data})
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var cm configMap
			if err := json.Unmarshal(body, &cm); err != nil {
				t.Fatalf("post body: %v", err)
			}
			for k, v := range cm.Data {
				data[k] = v
			}
			exists = true
			_ = json.NewEncoder(w).Encode(cm)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	t.Cleanup(srv.Close)
	return &ConfigMapStore{
		name: "energy-watchdog-state", namespace: "energy",
		base: srv.URL, http: srv.Client(),
	}, func() map[string]string { return data }
}

// The whole point of the spec/status split: a state save landing mid-apply must not wipe an
// intent set while that apply was in flight, and vice versa.
func TestConfigMapStoreKeysAreIndependent(t *testing.T) {
	ctx := context.Background()
	store, data := fakeAPI(t, map[string]string{})

	if err := store.SaveIntent(ctx, Intent{Shed: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, State{Mode: ModeShed, Stopped: []GuestRef{{VMID: 301, Type: "qemu"}}}); err != nil {
		t.Fatal(err)
	}

	intent, err := store.LoadIntent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !intent.Shed {
		t.Errorf("state save clobbered intent: %+v", intent)
	}
	st, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != ModeShed || len(st.Stopped) != 1 {
		t.Errorf("state = %+v", st)
	}

	// An operator-added key must survive the loop writing state too.
	d := data()
	d["note"] = "set by hand"
	if err := store.Save(ctx, State{Mode: ModeRunning}); err != nil {
		t.Fatal(err)
	}
	if data()["note"] != "set by hand" {
		t.Errorf("save wiped an unrelated key: %v", data())
	}
}

func TestConfigMapStoreCreatesOnMissing(t *testing.T) {
	ctx := context.Background()
	store, data := fakeAPI(t, nil) // ConfigMap does not exist yet

	if got, err := store.LoadIntent(ctx); err != nil || got.Shed {
		t.Errorf("missing configmap intent = %+v (err %v)", got, err)
	}
	if err := store.SaveIntent(ctx, Intent{Shed: true}); err != nil {
		t.Fatal(err)
	}
	if data()[intentKey] == "" {
		t.Errorf("create did not write %s: %v", intentKey, data())
	}
	got, err := store.LoadIntent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Shed {
		t.Errorf("intent after create = %+v", got)
	}
}

func TestWakeRequestExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const ttl = 10 * time.Minute // gamingGrace
	fresh := WakeRequest{VMID: 601, RequestedAt: now.Unix()}
	stale := WakeRequest{VMID: 602, RequestedAt: now.Add(-ttl - time.Minute).Unix()}

	if !fresh.Live(now, ttl) {
		t.Error("fresh request should be live")
	}
	if stale.Live(now, ttl) {
		t.Error("stale request should have aged out")
	}
	live := Intent{Wake: []WakeRequest{fresh, stale}}.LiveWake(now, ttl)
	if len(live) != 1 || live[0].VMID != 601 {
		t.Errorf("LiveWake = %+v, want just 601", live)
	}
}
