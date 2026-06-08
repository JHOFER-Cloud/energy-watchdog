// Package prom queries Prometheus for the averaged solar surplus and battery charge
// that drive the watchdog's shutdown/wake decision.
package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/JHOFER-Cloud/energy-watchdog/internal/config"
)

// Client is a minimal Prometheus instant-query client.
type Client struct {
	base string
	http *http.Client
}

// New builds a Prometheus client for the given base URL.
func New(base string) *Client {
	return &Client{base: base, http: &http.Client{Timeout: 30 * time.Second}}
}

// Reading is one evaluation of the solar state.
type Reading struct {
	// Surplus is avg(production) - avg(consumption) over the window. This drives the
	// decision.
	Surplus float64
	// SurplusRaw is the instantaneous production - consumption. Not used for decisions;
	// exposed only so the dashboard can show how much the window smooths spikes.
	SurplusRaw float64
	// SoC is the battery charge percent (0 if no battery metric is configured).
	SoC float64
}

// Read evaluates the surplus and battery queries built from cfg. Production and
// consumption are summed so multiple inverter/battery series collapse to a house total,
// then scaled to watts via cfg.PowerScale.
func (c *Client) Read(ctx context.Context, cfg config.Prometheus) (Reading, error) {
	scale := cfg.PowerScale
	if scale == 0 {
		scale = 1
	}
	avgSurplus := fmt.Sprintf("sum(avg_over_time(%s[%s])) - sum(avg_over_time(%s[%s]))",
		cfg.ProductionMetric, cfg.Window, cfg.ConsumptionMetric, cfg.Window)
	surplus, err := c.query(ctx, avgSurplus)
	if err != nil {
		return Reading{}, fmt.Errorf("surplus query: %w", err)
	}
	r := Reading{Surplus: surplus * scale}

	rawSurplus := fmt.Sprintf("sum(%s) - sum(%s)", cfg.ProductionMetric, cfg.ConsumptionMetric)
	if raw, err := c.query(ctx, rawSurplus); err == nil {
		r.SurplusRaw = raw * scale
	}

	if cfg.BatteryMetric != "" {
		soc, err := c.query(ctx, fmt.Sprintf("avg(avg_over_time(%s[%s]))", cfg.BatteryMetric, cfg.Window))
		if err != nil {
			return Reading{}, fmt.Errorf("battery query: %w", err)
		}
		r.SoC = soc
	}
	return r, nil
}

// query runs an instant query and returns the first scalar/vector sample value.
func (c *Client) query(ctx context.Context, q string) (float64, error) {
	u := c.base + "/api/v1/query?query=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prometheus %s: %s", resp.Status, body)
	}

	var out struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, err
	}
	if out.Status != "success" {
		return 0, fmt.Errorf("prometheus query failed: %s", out.Error)
	}
	return parseFirstValue(out.Data.ResultType, out.Data.Result)
}

// parseFirstValue extracts the sample value from a scalar or vector result.
func parseFirstValue(resultType string, raw json.RawMessage) (float64, error) {
	switch resultType {
	case "scalar":
		var s [2]any
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
		return toFloat(s[1])
	case "vector":
		var vec []struct {
			Value [2]any `json:"value"`
		}
		if err := json.Unmarshal(raw, &vec); err != nil {
			return 0, err
		}
		if len(vec) == 0 {
			return 0, fmt.Errorf("query returned no series")
		}
		return toFloat(vec[0].Value[1])
	default:
		return 0, fmt.Errorf("unexpected result type %q", resultType)
	}
}

func toFloat(v any) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("sample value %v is not a string", v)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse sample value %q: %w", s, err)
	}
	return f, nil
}
