// Package guardrail holds everything that bounds a strategy's wish before it
// reaches the cluster: floor and ceiling, the reactive lower bound, step
// limits, hysteresis and cooldown, the fallback chain, the circuit breaker
// and the manual overrides.
package guardrail

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

// Annotations read from an AppDefinition. Fixed by the shared contract.
const (
	AnnotationOverride      = "pool-scaler.eduide/override-target-idle"
	AnnotationOverrideUntil = "pool-scaler.eduide/override-until"
	AnnotationPaused        = "pool-scaler.eduide/paused"
)

// Limits bound targetIdle for one AppDefinition.
type Limits struct {
	Floor   int
	Ceiling int
	// MaxInstances caps targetIdle too; the operator would cap the pool anyway.
	MaxInstances int
}

func (l Limits) clamp(v int) int {
	ceiling := l.Ceiling
	if l.MaxInstances > 0 && l.MaxInstances < ceiling {
		ceiling = l.MaxInstances
	}
	if v > ceiling {
		v = ceiling
	}
	if v < l.Floor {
		v = l.Floor
	}
	return v
}

// Guard applies the movement guardrails. It remembers per AppDefinition the
// last decided value and the last increase, so it works the same in shadow
// mode, where nothing is patched and the live value never moves.
type Guard struct {
	cfg config.Guardrails

	mu    sync.Mutex
	state map[string]*appState
}

type appState struct {
	current      int
	lastIncrease time.Time
}

// NewGuard builds a guard.
func NewGuard(cfg config.Guardrails) *Guard {
	return &Guard{cfg: cfg, state: map[string]*appState{}}
}

// Seed sets the starting point for an AppDefinition the guard has not seen,
// normally the live spec.targetIdle. Later calls for the same one are ignored.
func (g *Guard) Seed(app string, current int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.state[app]; !ok {
		g.state[app] = &appState{current: current}
	}
}

// Current returns the last decided value and whether there is one.
func (g *Guard) Current(app string) (int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.state[app]
	if !ok {
		return 0, false
	}
	return st.current, true
}

// Apply bounds a wish and records the result as the new current value.
// reactiveFloor < 0 disables the reactive lower bound (the static baseline).
// The returned notes say which guardrails changed the value.
func (g *Guard) Apply(app string, now time.Time, wish, reactiveFloor int, l Limits) (int, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.state[app]
	if !ok {
		st = &appState{current: l.clamp(0)}
		g.state[app] = st
	}

	var notes []string
	v := wish
	if v < reactiveFloor {
		v = reactiveFloor
		notes = append(notes, fmt.Sprintf("raised to reactive %d", reactiveFloor))
	}
	if c := l.clamp(v); c != v {
		notes = append(notes, fmt.Sprintf("clamped to [%d, %d]", l.Floor, l.Ceiling))
		v = c
	}

	cur := st.current
	switch {
	case v > cur && g.cfg.MaxStepUp > 0 && v-cur > g.cfg.MaxStepUp:
		v = cur + g.cfg.MaxStepUp
		notes = append(notes, fmt.Sprintf("step up limited to %d", g.cfg.MaxStepUp))
	case v < cur && cur-v < g.cfg.Hysteresis:
		v = cur
		notes = append(notes, "within hysteresis")
	case v < cur && now.Sub(st.lastIncrease) < g.cfg.ScaleDownCooldown.Duration:
		v = cur
		notes = append(notes, "scale-down cooldown")
	case v < cur && g.cfg.MaxStepDown > 0 && cur-v > g.cfg.MaxStepDown:
		v = cur - g.cfg.MaxStepDown
		notes = append(notes, fmt.Sprintf("step down limited to %d", g.cfg.MaxStepDown))
	}
	// Floor and ceiling win over the step limits, e.g. right after the floor
	// was raised in the configuration.
	v = l.clamp(v)

	if v > cur {
		st.lastIncrease = now
	}
	st.current = v
	return v, notes
}

// Set records a value decided outside the guard (an override), so the guard
// continues from there once the override ends.
func (g *Guard) Set(app string, now time.Time, v int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.state[app]
	if !ok {
		st = &appState{}
		g.state[app] = st
	}
	if v > st.current {
		st.lastIncrease = now
	}
	st.current = v
}

// Choice is the outcome of the fallback chain for one AppDefinition.
type Choice struct {
	Value  int
	Source string
	// FellBackFrom is the first strategy in the chain, set when it was not used.
	FellBackFrom string
	Reason       string
	Rec          *model.Recommendation
}

// Choose walks the fallback chain: the strategies in order, then the last
// valid value, then the floor. A recommendation counts only when present and
// plausible (non-negative).
func Choose(app string, chain []string, recs map[string]map[string]model.Recommendation, errs map[string]error, lastValid *int, floor int) Choice {
	first := ""
	reason := ""
	for i, name := range chain {
		if i == 0 {
			first = name
		}
		rec, ok := recs[name][app]
		switch {
		case errs[name] != nil:
			if reason == "" {
				reason = "error"
			}
		case !ok:
			if reason == "" {
				reason = "missing"
			}
		case rec.TargetIdle < 0:
			if reason == "" {
				reason = "implausible"
			}
		default:
			c := Choice{Value: rec.TargetIdle, Source: name, Reason: rec.Reason, Rec: &rec}
			if i > 0 {
				c.FellBackFrom, c.Reason = first, reason
			}
			return c
		}
	}
	if lastValid != nil {
		return Choice{Value: *lastValid, Source: "last-valid", FellBackFrom: first, Reason: reason}
	}
	return Choice{Value: floor, Source: "floor", FellBackFrom: first, Reason: reason}
}

// Chain is the fallback chain for an active strategy: it, then predictive,
// then reactive, without duplicates.
func Chain(active string) []string {
	out := []string{active}
	for _, s := range []string{config.StrategyPredictive, config.StrategyReactive} {
		if s != active {
			out = append(out, s)
		}
	}
	return out
}

// Override is a manual override read from annotations.
type Override struct {
	Value int
	Until time.Time
}

// ParseOverride returns the active override, nil when there is none or it has
// expired, and an error when the annotations are present but unusable. An
// override without an end time is rejected: it would never expire.
func ParseOverride(ann map[string]string, now time.Time) (*Override, error) {
	raw, ok := ann[AnnotationOverride]
	if !ok {
		return nil, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return nil, fmt.Errorf("%s=%q is not a non-negative integer", AnnotationOverride, raw)
	}
	untilRaw, ok := ann[AnnotationOverrideUntil]
	if !ok {
		return nil, fmt.Errorf("%s is set without %s", AnnotationOverride, AnnotationOverrideUntil)
	}
	until, err := time.Parse(time.RFC3339, untilRaw)
	if err != nil {
		return nil, fmt.Errorf("%s=%q is not RFC3339", AnnotationOverrideUntil, untilRaw)
	}
	if !now.Before(until) {
		return nil, nil
	}
	return &Override{Value: v, Until: until}, nil
}

// Paused reports whether the scaler must leave the AppDefinition alone.
func Paused(ann map[string]string) bool { return ann[AnnotationPaused] == "true" }

// Breaker is a circuit breaker: after `threshold` failures in a row it stays
// open for `pause`, then lets one test call through.
type Breaker struct {
	threshold int
	pause     time.Duration

	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

// NewBreaker builds a closed breaker.
func NewBreaker(threshold int, pause time.Duration) *Breaker {
	return &Breaker{threshold: threshold, pause: pause}
}

// Allow reports whether a call may go out now.
func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures < b.threshold || !now.Before(b.openUntil)
}

// Open reports whether the breaker currently blocks calls.
func (b *Breaker) Open(now time.Time) bool { return !b.Allow(now) }

// Success closes the breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
}

// Failure counts a failed call and opens the breaker at the threshold. A
// failed test call after the pause opens it again right away.
func (b *Breaker) Failure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = now.Add(b.pause)
	}
}

// Plausible reports whether value is within factor k of reference. A
// reference of zero accepts values up to k.
func Plausible(value, reference int, k float64) bool {
	if reference <= 0 {
		return float64(value) <= k
	}
	r := float64(value) / float64(reference)
	return r <= k && r >= 1/k
}
