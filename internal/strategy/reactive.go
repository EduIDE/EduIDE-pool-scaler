package strategy

import (
	"context"
	"fmt"
	"math"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

// Reactive keeps enough idle pods for the claims expected while a new pool pod
// starts: recent claim rate x ready time x (1 + safety factor).
// It is the base, the last strategy in the fallback chain and the lower bound
// for every other strategy.
type Reactive struct {
	cfg config.Reactive
}

// NewReactive builds the reactive strategy.
func NewReactive(cfg config.Reactive) *Reactive { return &Reactive{cfg: cfg} }

// Name implements Strategy.
func (r *Reactive) Name() string { return config.StrategyReactive }

// Recommend implements Strategy.
func (r *Reactive) Recommend(_ context.Context, s model.Snapshot) ([]model.Recommendation, error) {
	var out []model.Recommendation
	for _, a := range sortedApps(s) {
		if rec, ok := r.ForApp(a); ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

// ForApp decides for one AppDefinition. It returns false when the claim rate
// is unknown, so the caller falls back instead of reading that as zero demand.
func (r *Reactive) ForApp(a *model.App) (model.Recommendation, bool) {
	if a.ClaimRate < 0 {
		return model.Recommendation{}, false
	}
	ready := a.ReadySeconds
	if ready <= 0 {
		ready = r.cfg.DefaultReadySeconds
	}
	expected := a.ClaimRate * ready
	target := int(math.Ceil(expected * (1 + r.cfg.SafetyFactor)))
	return model.Recommendation{
		AppDefinition: a.Name,
		TargetIdle:    target,
		Forecast:      expected,
		Reason:        fmt.Sprintf("%.3f claims/s x %.0fs ready x %.2f safety", a.ClaimRate, ready, 1+r.cfg.SafetyFactor),
	}, true
}

// Static recommends a fixed value per AppDefinition. It is the baseline arm of
// the switchback experiment.
type Static struct {
	limits func(app string) (floor, ceiling, static int)
}

// NewStatic builds the static strategy from the per app limits.
func NewStatic(limits func(app string) (floor, ceiling, static int)) *Static {
	return &Static{limits: limits}
}

// Name implements Strategy.
func (s *Static) Name() string { return config.StrategyStatic }

// Recommend implements Strategy.
func (s *Static) Recommend(_ context.Context, snap model.Snapshot) ([]model.Recommendation, error) {
	var out []model.Recommendation
	for _, a := range sortedApps(snap) {
		_, _, v := s.limits(a.Name)
		out = append(out, model.Recommendation{AppDefinition: a.Name, TargetIdle: v, Reason: "static value"})
	}
	return out, nil
}
