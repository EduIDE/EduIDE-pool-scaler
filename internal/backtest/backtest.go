// Package backtest replays recorded claims against a strategy and measures
// what the pool would have done: the warm-ready share of claims and the idle
// pod hours it cost.
package backtest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/guardrail"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/strategy"
)

// Input is the recorded history to replay.
type Input struct {
	// Claims per Step, per AppDefinition. A sample at T counts the claims in
	// (T-Step, T].
	Claims map[string][]strategy.Sample
	Events []model.Event
	From   time.Time
	To     time.Time
	Step   time.Duration
	// ReadySeconds is how long a new pool pod takes to become ready.
	ReadySeconds float64
}

// Result is one strategy's simulated outcome for one AppDefinition.
type Result struct {
	Strategy      string
	AppDefinition string
	Claims        int
	WarmReady     int
	WarmStarting  int
	Cold          int
	IdlePodHours  float64
	MeanTarget    float64
}

// WarmReadyRatio is the share of claims served by a ready pod.
func (r Result) WarmReadyRatio() float64 {
	if r.Claims == 0 {
		return 1
	}
	return float64(r.WarmReady) / float64(r.Claims)
}

// Run simulates every AppDefinition under one strategy with the same
// guardrails the scaler applies (no budget: history does not say what was
// free). reactive supplies the lower bound, as in the scaler; pass nil for the
// static baseline.
func Run(ctx context.Context, in Input, st strategy.Strategy, reactive *strategy.Reactive, cfg config.Config) ([]Result, error) {
	apps := make([]string, 0, len(in.Claims))
	for a := range in.Claims {
		apps = append(apps, a)
	}
	sort.Strings(apps)

	type pool struct {
		ready    int
		starting []time.Time
		carry    float64
		next     int
		claims   []strategy.Sample
		history  []float64
		target   int
		res      Result
		samples  int
		sumTgt   float64
	}
	pools := map[string]*pool{}
	for _, a := range apps {
		c := append([]strategy.Sample(nil), in.Claims[a]...)
		sort.Slice(c, func(i, j int) bool { return c[i].T.Before(c[j].T) })
		pools[a] = &pool{claims: c, res: Result{Strategy: st.Name(), AppDefinition: a}}
	}

	guard := guardrail.NewGuard(cfg.Guardrails)
	window := int(math.Max(1, math.Round(float64(cfg.Reactive.Window.Duration)/float64(in.Step))))
	readyAfter := time.Duration(in.ReadySeconds * float64(time.Second))
	var lastDecision time.Time

	for t := in.From; !t.After(in.To); t = t.Add(in.Step) {
		// Claims recorded for this step.
		stepClaims := map[string]int{}
		for _, a := range apps {
			p := pools[a]
			v := 0.0
			for p.next < len(p.claims) && !p.claims[p.next].T.After(t) {
				if p.claims[p.next].T.After(t.Add(-in.Step)) {
					v += p.claims[p.next].V
				}
				p.next++
			}
			// increase() extrapolates to fractions; carry them over.
			p.carry += v
			n := int(math.Floor(p.carry + 1e-9))
			p.carry -= float64(n)
			stepClaims[a] = n
			p.history = append(p.history, float64(n))
			// Pods that finished starting.
			kept := p.starting[:0]
			for _, r := range p.starting {
				if !r.After(t) {
					p.ready++
				} else {
					kept = append(kept, r)
				}
			}
			p.starting = kept
		}

		if lastDecision.IsZero() || t.Sub(lastDecision) >= cfg.Interval.Duration {
			lastDecision = t
			snap := model.Snapshot{Now: t, Apps: map[string]*model.App{}, Events: in.Events}
			for _, a := range apps {
				p := pools[a]
				h := p.history[max(0, len(p.history)-window):]
				sum := 0.0
				for _, v := range h {
					sum += v
				}
				snap.Apps[a] = &model.App{
					Name: a, IdleReady: p.ready, IdleStarting: len(p.starting),
					ClaimRate: sum / (float64(len(h)) * in.Step.Seconds()), ReadySeconds: in.ReadySeconds,
				}
			}
			recs, err := st.Recommend(ctx, snap)
			if err != nil {
				return nil, fmt.Errorf("%s at %s: %w", st.Name(), t, err)
			}
			wish := map[string]int{}
			for _, r := range recs {
				wish[r.AppDefinition] = r.TargetIdle
			}
			floors := map[string]int{}
			if reactive != nil {
				rr, _ := reactive.Recommend(ctx, snap)
				for _, r := range rr {
					floors[r.AppDefinition] = r.TargetIdle
				}
			}
			for _, a := range apps {
				floor, ceiling, _ := cfg.Limits(a)
				rf, ok := floors[a]
				if !ok {
					rf = -1
				}
				w, ok := wish[a]
				if !ok {
					w = rf
				}
				guard.Seed(a, 0)
				pools[a].target, _ = guard.Apply(a, t, w, rf, guardrail.Limits{Floor: floor, Ceiling: ceiling})
			}
		}

		for _, a := range apps {
			p := pools[a]
			n := stepClaims[a]
			warm := min(n, p.ready)
			p.ready -= warm
			starting := min(n-warm, len(p.starting))
			p.starting = p.starting[starting:]
			p.res.Claims += n
			p.res.WarmReady += warm
			p.res.WarmStarting += starting
			p.res.Cold += n - warm - starting

			// The operator refills to targetIdle, counting starting pods as idle,
			// and removes surplus idle pods, starting ones first.
			idle := p.ready + len(p.starting)
			for ; idle < p.target; idle++ {
				p.starting = append(p.starting, t.Add(readyAfter))
			}
			for ; idle > p.target; idle-- {
				if len(p.starting) > 0 {
					p.starting = p.starting[:len(p.starting)-1]
				} else {
					p.ready--
				}
			}
			p.res.IdlePodHours += float64(p.ready+len(p.starting)) * in.Step.Hours()
			p.samples++
			p.sumTgt += float64(p.target)
		}
	}

	out := make([]Result, 0, len(apps))
	for _, a := range apps {
		p := pools[a]
		if p.samples > 0 {
			p.res.MeanTarget = p.sumTgt / float64(p.samples)
		}
		out = append(out, p.res)
	}
	return out, nil
}
