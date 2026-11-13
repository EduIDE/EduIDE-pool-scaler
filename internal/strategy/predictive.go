package strategy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

// SlotLength is the resolution of the weekly profile.
const SlotLength = 15 * time.Minute

const slotsPerWeek = 7 * 24 * 4

// Sample is the number of claims in the SlotLength ending at T.
type Sample struct {
	T time.Time
	V float64
}

// ErrNotTrained is returned until the first successful training.
var ErrNotTrained = errors.New("predictive: not trained yet")

// Predictive forecasts demand from a weekly profile (EWMA per weekday and
// quarter hour) plus an uplift around known events, and turns the forecast
// into a distribution over demand levels.
type Predictive struct {
	cfg          config.Predictive
	loc          *time.Location
	defaultReady float64

	mu        sync.RWMutex
	profiles  map[string]*[slotsPerWeek]float64
	factors   map[model.EventKind]float64
	trainedAt time.Time
}

// NewPredictive builds an untrained predictive strategy.
func NewPredictive(cfg config.Predictive, loc *time.Location, defaultReadySeconds float64) *Predictive {
	return &Predictive{cfg: cfg, loc: loc, defaultReady: defaultReadySeconds}
}

// Name implements Strategy.
func (p *Predictive) Name() string { return config.StrategyPredictive }

// NeedsTraining reports whether the profile is missing or older than the
// retrain interval.
func (p *Predictive) NeedsTraining(now time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.profiles == nil || now.Sub(p.trainedAt) >= p.cfg.RetrainInterval.Duration
}

func (p *Predictive) slot(t time.Time) int {
	t = t.In(p.loc)
	return int(t.Weekday())*96 + (t.Hour()*60+t.Minute())/15
}

// Train rebuilds the weekly profile from claim history and re-estimates the
// event factors from past events.
func (p *Predictive) Train(now time.Time, history map[string][]Sample, pastEvents []model.Event) {
	profiles := make(map[string]*[slotsPerWeek]float64, len(history))
	for app, samples := range history {
		samples = append([]Sample(nil), samples...)
		sort.Slice(samples, func(i, j int) bool { return samples[i].T.Before(samples[j].T) })
		var prof [slotsPerWeek]float64
		var seen [slotsPerWeek]bool
		for _, s := range samples {
			i := p.slot(s.T.Add(-SlotLength))
			if !seen[i] {
				prof[i], seen[i] = s.V, true
				continue
			}
			prof[i] = p.cfg.Alpha*s.V + (1-p.cfg.Alpha)*prof[i]
		}
		profiles[app] = &prof
	}

	p.mu.Lock()
	p.profiles = profiles
	p.trainedAt = now
	p.mu.Unlock()

	factors := p.estimateFactors(now, history, pastEvents)
	p.mu.Lock()
	p.factors = factors
	p.mu.Unlock()
}

// estimateFactors measures starts per participant for past events: the
// claims inside the event window above what the profile expects anyway.
//
// shortcut: the profile is trained on the same weeks, so recurring events are
// partly in the baseline and the factor comes out low; fine while events are
// irregular, upgrade to an event-free baseline once exams repeat weekly.
func (p *Predictive) estimateFactors(now time.Time, history map[string][]Sample, events []model.Event) map[model.EventKind]float64 {
	ratios := map[model.EventKind][]float64{}
	for _, e := range events {
		from, to := p.window(e)
		if e.Participants <= 0 || to.After(now) {
			continue
		}
		samples, ok := history[e.AppDefinition]
		if !ok {
			continue
		}
		observed, covered := 0.0, false
		for _, s := range samples {
			start := s.T.Add(-SlotLength)
			if s.T.After(from) && start.Before(to) {
				observed += s.V
				covered = true
			}
		}
		if !covered {
			continue
		}
		excess := observed - p.profileClaims(e.AppDefinition, from, to)
		ratios[e.Kind] = append(ratios[e.Kind], math.Max(0, excess)/float64(e.Participants))
	}
	out := map[model.EventKind]float64{}
	for kind, rs := range ratios {
		if len(rs) < p.cfg.MinEventsForFactor {
			continue
		}
		sum := 0.0
		for _, r := range rs {
			sum += r
		}
		out[kind] = math.Min(sum/float64(len(rs)), 2)
	}
	return out
}

// Factor is the starts per participant used for one kind of event.
func (p *Predictive) Factor(kind model.EventKind) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if f, ok := p.factors[kind]; ok {
		return f
	}
	return p.cfg.Events[string(kind)].Factor
}

func (p *Predictive) window(e model.Event) (time.Time, time.Time) {
	prof := p.cfg.Events[string(e.Kind)]
	return e.Time.Add(-prof.Before.Duration), e.Time.Add(prof.After.Duration)
}

// profileClaims integrates the weekly profile over [from, to).
func (p *Predictive) profileClaims(app string, from, to time.Time) float64 {
	p.mu.RLock()
	prof := p.profiles[app]
	p.mu.RUnlock()
	if prof == nil {
		return 0
	}
	total := 0.0
	for t := from; t.Before(to); {
		next := t.Truncate(SlotLength).Add(SlotLength)
		if next.After(to) {
			next = to
		}
		total += prof[p.slot(t)] * float64(next.Sub(t)) / float64(SlotLength)
		t = next
	}
	return total
}

// eventClaims is the expected uplift from events in [from, to): participants
// x factor, spread evenly over each event's window.
func (p *Predictive) eventClaims(app string, events []model.Event, from, to time.Time) float64 {
	total := 0.0
	for _, e := range events {
		if e.AppDefinition != app || e.Participants <= 0 {
			continue
		}
		ws, we := p.window(e)
		length := we.Sub(ws)
		if length <= 0 {
			continue
		}
		overlap := minTime(we, to).Sub(maxTime(ws, from))
		if overlap <= 0 {
			continue
		}
		total += float64(e.Participants) * p.Factor(e.Kind) * float64(overlap) / float64(length)
	}
	return total
}

// Horizon is how far ahead the forecast looks: p95 ready time plus a buffer.
func (p *Predictive) Horizon(a *model.App) time.Duration {
	ready := a.ReadySeconds
	if ready <= 0 {
		ready = p.defaultReady
	}
	return time.Duration(ready*float64(time.Second)) + p.cfg.HorizonBuffer.Duration
}

// Forecast returns the expected starts within the horizon and their
// distribution over the configured levels.
func (p *Predictive) Forecast(s model.Snapshot, a *model.App) (float64, []model.LevelProbability, error) {
	p.mu.RLock()
	trained := p.profiles != nil
	p.mu.RUnlock()
	if !trained {
		return 0, nil, ErrNotTrained
	}
	to := s.Now.Add(p.Horizon(a))
	lambda := p.profileClaims(a.Name, s.Now, to) + p.eventClaims(a.Name, s.Events, s.Now, to)
	// shortcut: Poisson underestimates the spread of bursty demand; switch to a
	// negative binomial if the calibration panel shows the quantile is missed.
	return lambda, PoissonCDF(lambda, p.cfg.Levels), nil
}

// Recommend implements Strategy.
func (p *Predictive) Recommend(_ context.Context, s model.Snapshot) ([]model.Recommendation, error) {
	var out []model.Recommendation
	for _, a := range sortedApps(s) {
		lambda, dist, err := p.Forecast(s, a)
		if err != nil {
			return nil, err
		}
		out = append(out, model.Recommendation{
			AppDefinition: a.Name,
			TargetIdle:    QuantileLevel(dist, p.cfg.Quantile),
			Forecast:      lambda,
			Distribution:  dist,
			Reason:        fmt.Sprintf("%.1f starts expected in %s, q=%.2f", lambda, p.Horizon(a), p.cfg.Quantile),
		})
	}
	return out, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
