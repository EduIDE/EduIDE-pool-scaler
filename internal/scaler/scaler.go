// Package scaler runs one decision round: gather a snapshot from the
// providers, let every strategy recommend, walk the fallback chain, apply the
// guardrails, overrides and budget, and patch spec.targetIdle.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/budget"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/guardrail"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/metrics"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/strategy"
)

// Kube is what the scaler needs from the Kubernetes provider.
type Kube interface {
	Apps(ctx context.Context) (map[string]*model.App, error)
	Budget(ctx context.Context) (quota, nodes *budget.Resources, err error)
	PatchTargetIdle(ctx context.Context, app string, v int) error
}

// Prom is what the scaler needs from the Prometheus provider.
type Prom interface {
	ClaimRates(ctx context.Context, window time.Duration, at time.Time) (map[string]float64, error)
	ReadySeconds(ctx context.Context, at time.Time) (map[string]float64, error)
	Scalar(ctx context.Context, q string, at time.Time) (float64, error)
	ClaimHistory(ctx context.Context, from, to time.Time) (map[string][]strategy.Sample, error)
}

// EventSource is a calendar or Artemis.
type EventSource interface {
	Name() string
	Events(ctx context.Context, now time.Time) ([]model.Event, error)
}

// Scaler holds the providers, strategies and the state between rounds.
type Scaler struct {
	Cfg     config.Config
	Loc     *time.Location
	Kube    Kube
	Prom    Prom // nil without Prometheus
	Sources []EventSource
	Metrics *metrics.Metrics
	Log     *slog.Logger

	// Strategies by name. reactive and static are always present.
	Strategies map[string]strategy.Strategy
	Reactive   *strategy.Reactive
	Predictive *strategy.Predictive    // nil when not configured
	Decision   *strategy.DecisionModel // nil when not configured

	guard      *guardrail.Guard
	lastValid  map[string]int
	pastEvents map[string]model.Event
	lastActive string
}

// New wires a scaler. The caller fills the providers and optional strategies.
func New(cfg config.Config, loc *time.Location, kube Kube, m *metrics.Metrics, log *slog.Logger) *Scaler {
	r := strategy.NewReactive(cfg.Reactive)
	s := &Scaler{
		Cfg:        cfg,
		Loc:        loc,
		Kube:       kube,
		Metrics:    m,
		Log:        log,
		Reactive:   r,
		Strategies: map[string]strategy.Strategy{config.StrategyReactive: r, config.StrategyStatic: strategy.NewStatic(cfg.Limits)},
		guard:      guardrail.NewGuard(cfg.Guardrails),
		lastValid:  map[string]int{},
		pastEvents: map[string]model.Event{},
	}
	m.Mode.WithLabelValues(string(cfg.Mode)).Set(1)
	return s
}

// AddPredictive registers the predictive strategy.
func (s *Scaler) AddPredictive(p *strategy.Predictive) {
	s.Predictive = p
	s.Strategies[p.Name()] = p
}

// AddDecisionModel registers the decision-model strategy.
func (s *Scaler) AddDecisionModel(d *strategy.DecisionModel) {
	d.OnError = func(app string, err error) {
		s.Metrics.StrategyErrors.WithLabelValues(d.Name()).Inc()
		s.Log.Warn("decision model call failed", "appdef", app, "err", err)
	}
	s.Decision = d
	s.Strategies[d.Name()] = d
}

// Tick runs one decision round. Without a pool snapshot it decides nothing
// and leaves every pool as it is.
func (s *Scaler) Tick(ctx context.Context, now time.Time) error {
	apps, err := s.Kube.Apps(ctx)
	s.Metrics.Provider("kubernetes", err)
	if err != nil {
		return fmt.Errorf("no pool snapshot, leaving pools as they are: %w", err)
	}
	if len(s.Cfg.Apps) > 0 {
		keep := map[string]bool{}
		for _, a := range s.Cfg.Apps {
			keep[a] = true
		}
		for name := range apps {
			if !keep[name] {
				delete(apps, name)
			}
		}
	}

	s.fillPrometheus(ctx, now, apps)
	events := s.events(ctx, now)
	s.train(ctx, now)

	snap := model.Snapshot{Now: now, Apps: apps, Events: events}
	active := ActiveStrategy(s.Cfg, s.Loc, now, events)
	if active != s.lastActive {
		s.Metrics.ActiveStrategy.Reset()
		s.Metrics.ActiveStrategy.WithLabelValues(active).Set(1)
		s.Log.Info("active strategy", "strategy", active)
		s.lastActive = active
	}
	recs, errs := s.recommend(ctx, snap, active)

	free := s.budget(ctx, now, apps)
	chain := guardrail.Chain(active)

	var demands []budget.Demand
	wishes := map[string]string{}
	for name, a := range apps {
		paused := guardrail.Paused(a.Annotations)
		s.Metrics.Paused.WithLabelValues(name).Set(b2f(paused))
		if paused {
			s.Metrics.OverrideActive.WithLabelValues(name).Set(0)
			continue
		}
		floor, ceiling, _ := s.Cfg.Limits(name)
		seed := a.Idle()
		if a.TargetIdle != nil {
			seed = *a.TargetIdle
		}
		s.guard.Seed(name, seed)

		ov, ovErr := guardrail.ParseOverride(a.Annotations, now)
		if ovErr != nil {
			s.Log.Warn("ignoring override", "appdef", name, "err", ovErr)
		}
		s.Metrics.OverrideActive.WithLabelValues(name).Set(b2f(ov != nil))
		if ov != nil {
			// An override bypasses strategy and guardrails; only the budget and
			// maxInstances still apply.
			v := ov.Value
			if a.MaxInstances > 0 && v > a.MaxInstances {
				v = a.MaxInstances
			}
			demands = append(demands, budget.Demand{App: name, Floor: min(floor, v), Desired: v, Pod: budget.Resources{CPU: a.PodCPU, Memory: a.PodMemory}})
			wishes[name] = fmt.Sprintf("override %d until %s", ov.Value, ov.Until.Format(time.RFC3339))
			continue
		}

		var last *int
		if v, ok := s.lastValid[name]; ok {
			last = &v
		} else if a.TargetIdle != nil {
			last = a.TargetIdle
		}
		choice := guardrail.Choose(name, chain, recs, errs, last, floor)
		if choice.FellBackFrom != "" {
			s.Metrics.StrategyFallbacks.WithLabelValues(choice.FellBackFrom, choice.Reason, active).Inc()
		}
		if choice.Rec != nil {
			s.lastValid[name] = choice.Value
		}
		reactiveFloor := -1
		if r, ok := recs[config.StrategyReactive][name]; ok && active != config.StrategyStatic {
			reactiveFloor = r.TargetIdle
		}
		v, notes := s.guard.Apply(name, now, choice.Value, reactiveFloor, guardrail.Limits{Floor: floor, Ceiling: ceiling, MaxInstances: a.MaxInstances})
		demands = append(demands, budget.Demand{App: name, Floor: floor, Desired: v, Pod: budget.Resources{CPU: a.PodCPU, Memory: a.PodMemory}})
		why := fmt.Sprintf("%s=%d (%s)", choice.Source, choice.Value, choice.Reason)
		if len(notes) > 0 {
			why += "; " + strings.Join(notes, "; ")
		}
		wishes[name] = why
	}

	grants := budget.Allocate(free, demands)
	s.Metrics.TargetIdle.Reset()
	for _, d := range demands {
		g := grants[d.App]
		a := apps[d.App]
		why := wishes[d.App]
		if g.Clamped {
			s.Metrics.BudgetClamped.WithLabelValues(d.App).Inc()
			why += fmt.Sprintf("; budget cut %d to %d", d.Desired, g.Value)
		}
		// The guard continues from what is really set: after a budget cut or
		// once an override ends.
		s.guard.Set(d.App, now, g.Value)
		s.Metrics.TargetIdle.WithLabelValues(d.App, active).Set(float64(g.Value))
		s.Log.Info("decision", "appdef", d.App, "targetIdle", g.Value, "mode", s.Cfg.Mode, "active_strategy", active,
			"idle_ready", a.IdleReady, "idle_starting", a.IdleStarting, "claimed", a.Claimed, "why", why)
		s.patch(ctx, a, g.Value)
	}
	s.Metrics.LastDecision.Set(float64(now.Unix()))
	return nil
}

func (s *Scaler) patch(ctx context.Context, a *model.App, v int) {
	if s.Cfg.Mode != config.ModeActive || (a.TargetIdle != nil && *a.TargetIdle == v) {
		return
	}
	if err := s.Kube.PatchTargetIdle(ctx, a.Name, v); err != nil {
		s.Metrics.Patches.WithLabelValues(a.Name, "error").Inc()
		s.Log.Error("patch failed", "appdef", a.Name, "err", err)
		return
	}
	s.Metrics.Patches.WithLabelValues(a.Name, "success").Inc()
}

func (s *Scaler) fillPrometheus(ctx context.Context, now time.Time, apps map[string]*model.App) {
	for _, a := range apps {
		a.ClaimRate = -1 // unknown until Prometheus says otherwise
	}
	if s.Prom == nil {
		return
	}
	rates, err := s.Prom.ClaimRates(ctx, s.Cfg.Reactive.Window.Duration, now)
	if err == nil {
		for name, a := range apps {
			a.ClaimRate = rates[name] // no series means no claims yet
		}
		var ready map[string]float64
		if ready, err = s.Prom.ReadySeconds(ctx, now); err == nil {
			for name, a := range apps {
				a.ReadySeconds = ready[name]
			}
		}
	}
	s.Metrics.Provider("prometheus", err)
	if err != nil {
		s.Log.Warn("prometheus", "err", err)
	}
}

func (s *Scaler) events(ctx context.Context, now time.Time) []model.Event {
	var all []model.Event
	for _, src := range s.Sources {
		evs, err := src.Events(ctx, now)
		s.Metrics.Provider(src.Name(), err)
		if err != nil {
			s.Log.Warn("event source", "provider", src.Name(), "err", err)
		}
		all = append(all, evs...)
	}
	// Remembered so the predictive strategy can learn event factors after the
	// fact; Artemis only reports what is still ahead.
	horizon := now.Add(7 * 24 * time.Hour)
	upcoming := map[string]int{}
	for _, e := range all {
		s.pastEvents[fmt.Sprintf("%s/%s/%d", e.AppDefinition, e.Kind, e.Time.Unix())] = e
		if e.Time.After(now) && e.Time.Before(horizon) {
			upcoming[e.AppDefinition]++
		}
	}
	for k, e := range s.pastEvents {
		if now.Sub(e.Time) > time.Duration(s.Cfg.Predictive.TrainWeeks)*7*24*time.Hour {
			delete(s.pastEvents, k)
		}
	}
	s.Metrics.UpcomingEvents.Reset()
	for app, n := range upcoming {
		s.Metrics.UpcomingEvents.WithLabelValues(app).Set(float64(n))
	}
	return all
}

func (s *Scaler) train(ctx context.Context, now time.Time) {
	if s.Predictive == nil || s.Prom == nil || !s.Predictive.NeedsTraining(now) {
		return
	}
	from := now.Add(-time.Duration(s.Cfg.Predictive.TrainWeeks) * 7 * 24 * time.Hour)
	hist, err := s.Prom.ClaimHistory(ctx, from, now)
	s.Metrics.Provider("prometheus", err)
	if err != nil {
		s.Log.Warn("predictive training failed, keeping the previous profile", "err", err)
		return
	}
	past := make([]model.Event, 0, len(s.pastEvents))
	for _, e := range s.pastEvents {
		past = append(past, e)
	}
	s.Predictive.Train(now, hist, past)
	s.Log.Info("predictive trained", "apps", len(hist), "from", from)
}

func (s *Scaler) recommend(ctx context.Context, snap model.Snapshot, active string) (map[string]map[string]model.Recommendation, map[string]error) {
	names := map[string]bool{active: true}
	for _, n := range append(append([]string{}, s.Cfg.Strategies...), guardrail.Chain(active)...) {
		names[n] = true
	}
	recs := map[string]map[string]model.Recommendation{}
	errs := map[string]error{}
	s.Metrics.DesiredIdle.Reset()
	for name := range names {
		st, ok := s.Strategies[name]
		if !ok {
			errs[name] = fmt.Errorf("strategy %s is not configured", name)
			continue
		}
		out, err := st.Recommend(ctx, snap)
		if err != nil {
			errs[name] = err
			s.Metrics.StrategyErrors.WithLabelValues(name).Inc()
			s.Log.Warn("strategy failed", "strategy", name, "err", err)
			continue
		}
		recs[name] = map[string]model.Recommendation{}
		for _, r := range out {
			recs[name][r.AppDefinition] = r
			s.Metrics.DesiredIdle.WithLabelValues(r.AppDefinition, name, strconv.FormatBool(name == active), active).Set(float64(r.TargetIdle))
			s.Metrics.ForecastStarts.WithLabelValues(r.AppDefinition, name).Set(r.Forecast)
			for _, p := range r.Distribution {
				s.Metrics.ForecastDistribution.WithLabelValues(r.AppDefinition, name, strconv.Itoa(p.Level)).Set(p.Probability)
			}
		}
	}
	if s.Decision != nil {
		s.Metrics.DecisionModelBreaker.Set(b2f(s.Decision.BreakerOpen(snap.Now)))
	}
	return recs, errs
}

func (s *Scaler) budget(ctx context.Context, now time.Time, apps map[string]*model.App) budget.Resources {
	var src budget.Sources
	quota, nodes, err := s.Kube.Budget(ctx)
	s.Metrics.Provider("kubernetes", err)
	if err != nil {
		s.Log.Warn("budget", "err", err)
	}
	src.Quota, src.Nodes = quota, nodes
	if s.Prom != nil && s.Cfg.Budget.UsageQuery != "" {
		v, err := s.Prom.Scalar(ctx, s.Cfg.Budget.UsageQuery, now)
		s.Metrics.Provider("prometheus", err)
		if err != nil {
			s.Log.Warn("usage headroom", "err", err)
		} else {
			v *= s.Cfg.Budget.UsageHeadroomFraction
			src.UsageMemory = &v
		}
	}
	for label, r := range map[string]*budget.Resources{"quota": quota, "nodes": nodes} {
		if r != nil {
			s.Metrics.Budget.WithLabelValues("cpu", label).Set(r.CPU)
			s.Metrics.Budget.WithLabelValues("memory", label).Set(r.Memory)
		}
	}
	if src.UsageMemory != nil {
		s.Metrics.Budget.WithLabelValues("memory", "usage").Set(*src.UsageMemory)
	}
	// Idle pods of paused AppDefinitions stay as they are, so they are not
	// part of what is shared out.
	var held budget.Resources
	for _, a := range apps {
		if guardrail.Paused(a.Annotations) {
			continue
		}
		held.CPU += float64(a.Idle()) * a.PodCPU
		held.Memory += float64(a.Idle()) * a.PodMemory
	}
	return budget.Free(src, held)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
