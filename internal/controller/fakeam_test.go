package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// fakeAM is a minimal in-memory Alertmanager v2 silences server for controller tests. It
// behaves like the real thing on the three routes the client uses: POST /api/v2/silences
// creates (no id) or updates (with id), GET /api/v2/silences lists, and
// DELETE /api/v2/silence/{id} expires. It counts each operation so tests can assert there's
// no churn.
type fakeAM struct {
	srv *httptest.Server

	mu      sync.Mutex
	seq     int
	sils    map[string]*fakeSilence
	creates int
	updates int
	deletes int
}

type fakeSilence struct {
	ID        string          `json:"id"`
	CreatedBy string          `json:"createdBy"`
	Comment   string          `json:"comment"`
	Matchers  []fakeMatcher   `json:"matchers"`
	EndsAt    time.Time       `json:"endsAt"`
	Status    fakeSilenceStat `json:"status"`
}

type fakeSilenceStat struct {
	State string `json:"state"`
}

type fakeMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

func newFakeAM() *fakeAM {
	f := &fakeAM{sils: map[string]*fakeSilence{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeAM) URL() string { return f.srv.URL }
func (f *fakeAM) Close()      { f.srv.Close() }

func (f *fakeAM) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/silences":
		var in fakeSilence
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &in)
		if in.ID != "" && f.sils[in.ID] != nil {
			f.updates++
			f.sils[in.ID].EndsAt = in.EndsAt
			f.sils[in.ID].Status.State = "active"
			_, _ = w.Write([]byte(`{"silenceID":"` + in.ID + `"}`))
			return
		}
		f.seq++
		id := "sil-" + itoa(f.seq)
		in.ID = id
		in.Status.State = "active"
		f.sils[id] = &in
		f.creates++
		_, _ = w.Write([]byte(`{"silenceID":"` + id + `"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/silences":
		out := make([]*fakeSilence, 0, len(f.sils))
		for _, s := range f.sils {
			out = append(out, s)
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v2/silence/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/silence/")
		if s := f.sils[id]; s != nil && s.Status.State != "expired" {
			s.Status.State = "expired"
			f.deletes++
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// counts returns creates, updates and deletes seen so far.
func (f *fakeAM) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.updates, f.deletes
}

// activeOurs counts non-expired silences created by energy-watchdog.
func (f *fakeAM) activeOurs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sils {
		if s.CreatedBy == "energy-watchdog" && s.Status.State != "expired" {
			n++
		}
	}
	return n
}

// seed injects a silence directly (as if created by an earlier run), returning its id.
func (f *fakeAM) seed(createdBy, comment string, matchers []fakeMatcher, endsAt time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := "seed-" + itoa(f.seq)
	f.sils[id] = &fakeSilence{
		ID: id, CreatedBy: createdBy, Comment: comment, Matchers: matchers,
		EndsAt: endsAt, Status: fakeSilenceStat{State: "active"},
	}
	return id
}

// state reports a silence's current state ("" if unknown).
func (f *fakeAM) state(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sils[id]; s != nil {
		return s.Status.State
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
