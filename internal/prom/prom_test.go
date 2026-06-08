package prom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
)

func TestRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "production"):
			// production avg 3000 - consumption avg 1200 => surplus 1800
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"1800"]}]}}`))
		case strings.Contains(q, "charge_level"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"73.5"]}]}}`))
		default:
			t.Errorf("unexpected query %q", q)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	r, err := c.Read(context.Background(), config.Prometheus{
		Window:            "30m",
		ProductionMetric:  "sonnenbatterie_production_mw",
		ConsumptionMetric: "sonnenbatterie_consumption_mw",
		BatteryMetric:     "sonnenbatterie_user_charge_level_percent",
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.Surplus != 1800 {
		t.Errorf("surplus = %v, want 1800", r.Surplus)
	}
	if r.SurplusRaw != 1800 {
		t.Errorf("surplusRaw = %v, want 1800", r.SurplusRaw)
	}
	if r.SoC != 73.5 {
		t.Errorf("soc = %v, want 73.5", r.SoC)
	}
}

func TestReadEmptyVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.Read(context.Background(), config.Prometheus{Window: "30m", ProductionMetric: "p", ConsumptionMetric: "c"})
	if err == nil {
		t.Fatal("expected error for empty series, got nil")
	}
}

func TestReadScalar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1700000000,"42"]}}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	r, err := c.Read(context.Background(), config.Prometheus{Window: "30m", ProductionMetric: "p", ConsumptionMetric: "c"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.Surplus != 42 {
		t.Errorf("surplus = %v, want 42", r.Surplus)
	}
}
