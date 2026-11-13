package strategy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/guardrail"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

// Question asks the decision model to pick among Options, given Prompt.
type Question struct {
	Prompt  string   `json:"question"`
	Options []string `json:"choices"`
}

// Answer is one probability per option, in the order of the question.
type Answer struct {
	Probabilities []float64 `json:"probabilities"`
}

// DecisionClient asks an externally hosted decision model.
type DecisionClient interface {
	Decide(ctx context.Context, q Question) (Answer, error)
}

// HTTPDecisionClient posts the question as JSON and reads the answer as JSON:
//
//	POST <url>  {"question": "...", "choices": ["0", "2", ..., "160+"]}
//	200         {"probabilities": [0.1, 0.3, ...]}
//
// shortcut: this request shape is our own; adapt Decide once the hosted
// Open-Jev style endpoint and its schema are fixed (open point 3 of the plan).
type HTTPDecisionClient struct {
	URL    string
	Token  string
	Client *http.Client
}

// NewHTTPDecisionClient checks the URL is https: the token is a bearer secret.
func NewHTTPDecisionClient(rawURL, token string, client *http.Client) (*HTTPDecisionClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("decision model URL must be https, got %q", u.Scheme)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPDecisionClient{URL: rawURL, Token: token, Client: client}, nil
}

// Decide implements DecisionClient.
func (c *HTTPDecisionClient) Decide(ctx context.Context, q Question) (Answer, error) {
	body, err := json.Marshal(q)
	if err != nil {
		return Answer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return Answer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Answer{}, fmt.Errorf("decision model answered %s", resp.Status)
	}
	var a Answer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&a); err != nil {
		return Answer{}, fmt.Errorf("decision model answer: %w", err)
	}
	return a, nil
}

// FakeDecisionClient answers from a function, for tests and backtesting
// without a hosted model.
type FakeDecisionClient struct {
	Fn func(q Question) (Answer, error)
}

// Decide implements DecisionClient.
func (f *FakeDecisionClient) Decide(_ context.Context, q Question) (Answer, error) { return f.Fn(q) }

// DecisionModel asks an external decision model for a distribution over
// demand levels and applies the same quantile rule as predictive. It calls the
// model once per interval and AppDefinition and reuses the answer in between.
// An answer counts as failed on timeout, error, an invalid distribution or one
// more than factor k off predictive; a stale answer (older than two intervals)
// is not used. Three failures in a row open a circuit breaker.
type DecisionModel struct {
	cfg        config.DecisionModel
	client     DecisionClient
	predictive *Predictive
	breaker    *guardrail.Breaker
	loc        *time.Location

	// OnError, when set, is called for every failed call.
	OnError func(app string, err error)

	mu       sync.Mutex
	lastCall map[string]time.Time
	answers  map[string]cachedAnswer
}

type cachedAnswer struct {
	at     time.Time
	dist   []model.LevelProbability
	target int
}

// NewDecisionModel builds the strategy. It needs predictive for the prompt and
// the plausibility check.
func NewDecisionModel(cfg config.DecisionModel, client DecisionClient, predictive *Predictive, loc *time.Location) *DecisionModel {
	if len(cfg.Levels) == 0 {
		cfg.Levels = predictive.cfg.Levels
	}
	return &DecisionModel{
		cfg:        cfg,
		client:     client,
		predictive: predictive,
		breaker:    guardrail.NewBreaker(cfg.BreakerFailures, cfg.BreakerPause.Duration),
		loc:        loc,
		lastCall:   map[string]time.Time{},
		answers:    map[string]cachedAnswer{},
	}
}

// Name implements Strategy.
func (d *DecisionModel) Name() string { return config.StrategyDecisionModel }

// BreakerOpen reports whether the circuit breaker currently blocks calls.
func (d *DecisionModel) BreakerOpen(now time.Time) bool { return d.breaker.Open(now) }

// Options are the choices offered to the model: the levels, the last one open
// ended.
func (d *DecisionModel) Options() []string {
	out := make([]string, len(d.cfg.Levels))
	for i, l := range d.cfg.Levels {
		out[i] = strconv.Itoa(l)
	}
	out[len(out)-1] += "+"
	return out
}

// Recommend implements Strategy.
func (d *DecisionModel) Recommend(ctx context.Context, s model.Snapshot) ([]model.Recommendation, error) {
	var out []model.Recommendation
	for _, a := range sortedApps(s) {
		lambda, predDist, err := d.predictive.Forecast(s, a)
		if err != nil {
			return nil, fmt.Errorf("decision-model needs predictive: %w", err)
		}
		predTarget := QuantileLevel(predDist, d.predictive.cfg.Quantile)

		d.mu.Lock()
		due := s.Now.Sub(d.lastCall[a.Name]) >= d.cfg.Interval.Duration
		d.mu.Unlock()
		if due && d.breaker.Allow(s.Now) {
			d.mu.Lock()
			d.lastCall[a.Name] = s.Now
			d.mu.Unlock()
			dist, target, err := d.ask(ctx, s, a, lambda, predDist, predTarget)
			if err != nil {
				d.breaker.Failure(s.Now)
				if d.OnError != nil {
					d.OnError(a.Name, err)
				}
			} else {
				d.breaker.Success()
				d.mu.Lock()
				d.answers[a.Name] = cachedAnswer{at: s.Now, dist: dist, target: target}
				d.mu.Unlock()
			}
		}

		d.mu.Lock()
		ans, ok := d.answers[a.Name]
		d.mu.Unlock()
		if !ok || s.Now.Sub(ans.at) > 2*d.cfg.Interval.Duration {
			continue
		}
		out = append(out, model.Recommendation{
			AppDefinition: a.Name,
			TargetIdle:    ans.target,
			Distribution:  ans.dist,
			Forecast:      lambda,
			Reason:        fmt.Sprintf("decision model answer from %s, q=%.2f", ans.at.In(d.loc).Format(time.TimeOnly), d.cfg.Quantile),
		})
	}
	return out, nil
}

func (d *DecisionModel) ask(ctx context.Context, s model.Snapshot, a *model.App, lambda float64, predDist []model.LevelProbability, predTarget int) ([]model.LevelProbability, int, error) {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout.Duration)
	defer cancel()
	q := Question{Prompt: d.prompt(s, a, lambda, predDist), Options: d.Options()}
	ans, err := d.client.Decide(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	dist, err := cumulative(d.cfg.Levels, ans.Probabilities)
	if err != nil {
		return nil, 0, err
	}
	target := QuantileLevel(dist, d.cfg.Quantile)
	if !guardrail.Plausible(target, predTarget, d.cfg.PlausibilityFactor) {
		return nil, 0, fmt.Errorf("implausible: %d against predictive %d (factor %.1f)", target, predTarget, d.cfg.PlausibilityFactor)
	}
	return dist, target, nil
}

// cumulative turns one probability per level into P(demand <= level),
// rejecting answers that are not a distribution.
func cumulative(levels []int, probs []float64) ([]model.LevelProbability, error) {
	if len(probs) != len(levels) {
		return nil, fmt.Errorf("got %d probabilities for %d options", len(probs), len(levels))
	}
	sum := 0.0
	for _, p := range probs {
		if math.IsNaN(p) || p < 0 {
			return nil, fmt.Errorf("invalid probability %v", p)
		}
		sum += p
	}
	if math.Abs(sum-1) > 0.05 {
		return nil, fmt.Errorf("probabilities sum to %.3f, not 1", sum)
	}
	out := make([]model.LevelProbability, len(levels))
	acc := 0.0
	for i, p := range probs {
		acc += p / sum
		out[i] = model.LevelProbability{Level: levels[i], Probability: math.Min(acc, 1)}
	}
	return out, ValidateDistribution(out)
}

// prompt describes the state with aggregated numbers only. Nothing about
// individual students leaves the cluster.
func (d *DecisionModel) prompt(s model.Snapshot, a *model.App, lambda float64, predDist []model.LevelProbability) string {
	var b strings.Builder
	h := d.predictive.Horizon(a)
	fmt.Fprintf(&b, "EduIDE prewarmed pool for the IDE image %q.\n", a.Name)
	fmt.Fprintf(&b, "Local time: %s.\n", s.Now.In(d.loc).Format("Monday 2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "Idle pods: %d ready, %d starting. Sessions running from the pool: %d.\n", a.IdleReady, a.IdleStarting, a.Claimed)
	if a.ClaimRate >= 0 {
		fmt.Fprintf(&b, "Recent session starts: %.2f per minute.\n", a.ClaimRate*60)
	}
	fmt.Fprintf(&b, "A new pool pod needs about %s to become ready.\n", h-d.predictive.cfg.HorizonBuffer.Duration)
	fmt.Fprintf(&b, "Statistical forecast: %.1f starts expected in the next %s.", lambda, h)
	for _, p := range predDist {
		fmt.Fprintf(&b, " P(<=%d)=%.2f", p.Level, p.Probability)
	}
	b.WriteString("\n")
	n := 0
	for _, e := range s.Events {
		if e.AppDefinition != a.Name || e.Time.Before(s.Now.Add(-6*time.Hour)) || e.Time.After(s.Now.Add(24*time.Hour)) {
			continue
		}
		if n == 0 {
			b.WriteString("Known events within 24 hours:\n")
		}
		fmt.Fprintf(&b, "- %s at %s, %d participants\n", e.Kind, e.Time.In(d.loc).Format("Mon 15:04"), e.Participants)
		n++
	}
	fmt.Fprintf(&b, "Question: how many sessions for %q will start in the next %s?", a.Name, h)
	return b.String()
}
