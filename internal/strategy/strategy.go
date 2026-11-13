// Package strategy holds the interchangeable deciders. A strategy only reads a
// snapshot and returns recommendations: it never patches anything and never
// talks to the Kubernetes API. Guardrails live outside, in package guardrail.
package strategy

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

// Strategy recommends a targetIdle per AppDefinition.
//
// An AppDefinition a strategy cannot decide for is left out of the result;
// the fallback chain then takes over for that one alone. An error means the
// strategy could decide nothing at all.
type Strategy interface {
	Name() string
	Recommend(ctx context.Context, s model.Snapshot) ([]model.Recommendation, error)
}

// sortedApps keeps results and logs in a stable order.
func sortedApps(s model.Snapshot) []*model.App {
	apps := make([]*model.App, 0, len(s.Apps))
	for _, a := range s.Apps {
		apps = append(apps, a)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	return apps
}

// PoissonCDF returns P(X <= level) for X ~ Poisson(lambda), at every level.
func PoissonCDF(lambda float64, levels []int) []model.LevelProbability {
	out := make([]model.LevelProbability, len(levels))
	if lambda <= 0 {
		for i, l := range levels {
			out[i] = model.LevelProbability{Level: l, Probability: 1}
		}
		return out
	}
	// Sum the pmf in log space, so large lambdas do not underflow exp(-lambda).
	logLambda := math.Log(lambda)
	cdf, k := 0.0, 0
	for i, l := range levels {
		for ; k <= l; k++ {
			lg, _ := math.Lgamma(float64(k + 1))
			cdf += math.Exp(float64(k)*logLambda - lambda - lg)
		}
		out[i] = model.LevelProbability{Level: l, Probability: math.Min(cdf, 1)}
	}
	return out
}

// QuantileLevel is the smallest level with P(demand <= level) >= q, or the
// highest level when none reaches q.
func QuantileLevel(dist []model.LevelProbability, q float64) int {
	for _, p := range dist {
		if p.Probability >= q {
			return p.Level
		}
	}
	return dist[len(dist)-1].Level
}

// ValidateDistribution checks a cumulative distribution: ascending levels,
// probabilities in [0, 1], non-decreasing.
func ValidateDistribution(dist []model.LevelProbability) error {
	if len(dist) == 0 {
		return fmt.Errorf("empty distribution")
	}
	prev := model.LevelProbability{Level: math.MinInt, Probability: 0}
	for _, p := range dist {
		if math.IsNaN(p.Probability) || p.Probability < 0 || p.Probability > 1+1e-9 {
			return fmt.Errorf("probability %v at level %d is outside [0, 1]", p.Probability, p.Level)
		}
		if p.Level <= prev.Level || p.Probability < prev.Probability-1e-9 {
			return fmt.Errorf("distribution is not cumulative at level %d", p.Level)
		}
		prev = p
	}
	return nil
}
