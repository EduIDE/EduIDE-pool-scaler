// Package prometheus reads claim rates, pool pod ready times, memory usage and
// the claim history for training from the operator metrics in Prometheus.
package prometheus

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/strategy"
)

// Provider queries one namespace's operator metrics.
type Provider struct {
	API       v1.API
	Namespace string
	Timeout   time.Duration
}

// New connects to a Prometheus HTTP API.
func New(url, namespace string, timeout time.Duration) (*Provider, error) {
	c, err := api.NewClient(api.Config{Address: url})
	if err != nil {
		return nil, err
	}
	return &Provider{API: v1.NewAPI(c), Namespace: namespace, Timeout: timeout}, nil
}

func (p *Provider) query(ctx context.Context, q string, at time.Time) (model.Vector, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	v, _, _, err := p.API.Query(ctx, q, at)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", q, err)
	}
	vec, ok := v.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("%s: expected a vector, got %s", q, v.Type())
	}
	return vec, nil
}

func byApp(vec model.Vector) map[string]float64 {
	out := map[string]float64{}
	for _, s := range vec {
		v := float64(s.Value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out[string(s.Metric["app_definition"])] = v
	}
	return out
}

// ClaimRates returns claims per second over window, per AppDefinition.
// Every claim counts, whatever its result: all of them are demand.
func (p *Provider) ClaimRates(ctx context.Context, window time.Duration, at time.Time) (map[string]float64, error) {
	vec, err := p.query(ctx, fmt.Sprintf(`sum by (app_definition) (rate(eduide_session_claims_total{namespace=%q}[%s]))`,
		p.Namespace, model.Duration(window)), at)
	return byApp(vec), err
}

// ReadySeconds returns the p95 time from pool deployment created to pod Ready.
func (p *Provider) ReadySeconds(ctx context.Context, at time.Time) (map[string]float64, error) {
	vec, err := p.query(ctx, fmt.Sprintf(`histogram_quantile(0.95, sum by (app_definition, le) (rate(eduide_pool_instance_ready_seconds_bucket{namespace=%q}[6h])))`,
		p.Namespace), at)
	return byApp(vec), err
}

// Scalar runs a query that returns one number.
func (p *Provider) Scalar(ctx context.Context, q string, at time.Time) (float64, error) {
	vec, err := p.query(ctx, q, at)
	if err != nil {
		return 0, err
	}
	if len(vec) != 1 {
		return 0, fmt.Errorf("%s: expected one series, got %d", q, len(vec))
	}
	return float64(vec[0].Value), nil
}

// ClaimHistory returns the claims per SlotLength per AppDefinition in
// [from, to], queried a week at a time to stay under the points limit.
func (p *Provider) ClaimHistory(ctx context.Context, from, to time.Time) (map[string][]strategy.Sample, error) {
	return p.History(ctx, from, to, strategy.SlotLength)
}

// History returns claims per step per AppDefinition in [from, to].
func (p *Provider) History(ctx context.Context, from, to time.Time, step time.Duration) (map[string][]strategy.Sample, error) {
	q := fmt.Sprintf(`sum by (app_definition) (increase(eduide_session_claims_total{namespace=%q}[%s]))`, p.Namespace, model.Duration(step))
	out := map[string][]strategy.Sample{}
	chunk := 10000 * step
	for start := from.Truncate(step); start.Before(to); start = start.Add(chunk) {
		end := start.Add(chunk - step)
		if end.After(to) {
			end = to
		}
		cctx, cancel := context.WithTimeout(ctx, p.Timeout)
		v, _, _, err := p.API.QueryRange(cctx, q, v1.Range{Start: start, End: end, Step: step})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("claim history: %w", err)
		}
		m, ok := v.(model.Matrix)
		if !ok {
			return nil, fmt.Errorf("claim history: expected a matrix, got %s", v.Type())
		}
		for _, series := range m {
			app := string(series.Metric["app_definition"])
			for _, pt := range series.Values {
				if f := float64(pt.Value); !math.IsNaN(f) {
					out[app] = append(out[app], strategy.Sample{T: pt.Timestamp.Time(), V: f})
				}
			}
		}
	}
	return out, nil
}
