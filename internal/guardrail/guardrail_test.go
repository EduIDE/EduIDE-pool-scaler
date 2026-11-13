package guardrail

import (
	"errors"
	"testing"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func guard() *Guard {
	return NewGuard(config.Guardrails{MaxStepUp: 10, MaxStepDown: 3, Hysteresis: 2, ScaleDownCooldown: config.Duration{Duration: 10 * time.Minute}})
}

func TestGuardBounds(t *testing.T) {
	l := Limits{Floor: 2, Ceiling: 30, MaxInstances: 100}
	cases := []struct {
		name          string
		current, wish int
		reactive      int
		want          int
	}{
		{"floor", 2, 0, -1, 2},
		{"ceiling", 25, 50, -1, 30},
		{"never below reactive", 5, 3, 7, 7},
		{"static ignores reactive", 5, 5, -1, 5},
		{"step up limit", 2, 20, -1, 12},
		{"hysteresis holds", 10, 9, -1, 10},
		{"step down limit", 20, 5, -1, 17},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := guard()
			g.Seed("a", c.current)
			if got, _ := g.Apply("a", t0, c.wish, c.reactive, l); got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestGuardMaxInstancesCapsCeiling(t *testing.T) {
	g := guard()
	g.Seed("a", 0)
	if got, _ := g.Apply("a", t0, 8, -1, Limits{Ceiling: 30, MaxInstances: 4}); got != 4 {
		t.Fatalf("got %d", got)
	}
}

func TestGuardCooldown(t *testing.T) {
	g := guard()
	l := Limits{Ceiling: 50}
	g.Seed("a", 0)
	if v, _ := g.Apply("a", t0, 10, -1, l); v != 10 {
		t.Fatalf("up %d", v)
	}
	if v, notes := g.Apply("a", t0.Add(5*time.Minute), 0, -1, l); v != 10 || len(notes) == 0 {
		t.Fatalf("scaled down inside the cooldown: %d", v)
	}
	if v, _ := g.Apply("a", t0.Add(11*time.Minute), 0, -1, l); v != 7 {
		t.Fatalf("after cooldown want 10-3=7, got %d", v)
	}
	// Scaling up again restarts the cooldown.
	g.Apply("a", t0.Add(12*time.Minute), 9, -1, l)
	if v, _ := g.Apply("a", t0.Add(13*time.Minute), 0, -1, l); v != 9 {
		t.Fatalf("got %d", v)
	}
}

func TestGuardSetContinuesFromRealValue(t *testing.T) {
	g := guard()
	g.Seed("a", 0)
	g.Apply("a", t0, 10, -1, Limits{Ceiling: 50})
	g.Set("a", t0, 4) // budget cut it
	if v, _ := g.Current("a"); v != 4 {
		t.Fatalf("current %d", v)
	}
}

func TestChooseFallbackChain(t *testing.T) {
	chain := Chain(config.StrategyDecisionModel)
	if len(chain) != 3 || chain[1] != config.StrategyPredictive || chain[2] != config.StrategyReactive {
		t.Fatalf("chain %v", chain)
	}
	rec := func(v int) model.Recommendation { return model.Recommendation{TargetIdle: v} }
	five := 5
	cases := []struct {
		name   string
		recs   map[string]map[string]model.Recommendation
		errs   map[string]error
		last   *int
		value  int
		source string
		reason string
	}{
		{"active used", map[string]map[string]model.Recommendation{"decision-model": {"a": rec(9)}, "reactive": {"a": rec(1)}}, nil, nil, 9, "decision-model", ""},
		{"error falls to predictive", map[string]map[string]model.Recommendation{"predictive": {"a": rec(6)}}, map[string]error{"decision-model": errors.New("x")}, nil, 6, "predictive", "error"},
		{"missing falls to reactive", map[string]map[string]model.Recommendation{"decision-model": {}, "reactive": {"a": rec(2)}}, nil, nil, 2, "reactive", "missing"},
		{"implausible is skipped", map[string]map[string]model.Recommendation{"decision-model": {"a": rec(-1)}, "reactive": {"a": rec(2)}}, nil, nil, 2, "reactive", "implausible"},
		{"last valid", nil, nil, &five, 5, "last-valid", "missing"},
		{"floor", nil, nil, nil, 3, "floor", "missing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Choose("a", chain, c.recs, c.errs, c.last, 3)
			if got.Value != c.value || got.Source != c.source {
				t.Fatalf("got %+v", got)
			}
			if c.source != chain[0] && (got.FellBackFrom != chain[0] || got.Reason != c.reason) {
				t.Fatalf("fallback not reported: %+v", got)
			}
		})
	}
	if got := Chain(config.StrategyReactive); len(got) != 2 || got[0] != config.StrategyReactive {
		t.Fatalf("no duplicates: %v", got)
	}
}

func TestParseOverride(t *testing.T) {
	until := t0.Add(time.Hour).Format(time.RFC3339)
	cases := []struct {
		name    string
		ann     map[string]string
		want    int
		active  bool
		wantErr bool
	}{
		{"none", nil, 0, false, false},
		{"active", map[string]string{AnnotationOverride: "12", AnnotationOverrideUntil: until}, 12, true, false},
		{"expired", map[string]string{AnnotationOverride: "12", AnnotationOverrideUntil: t0.Add(-time.Second).Format(time.RFC3339)}, 0, false, false},
		{"end time is mandatory", map[string]string{AnnotationOverride: "12"}, 0, false, true},
		{"bad number", map[string]string{AnnotationOverride: "many", AnnotationOverrideUntil: until}, 0, false, true},
		{"negative", map[string]string{AnnotationOverride: "-1", AnnotationOverrideUntil: until}, 0, false, true},
		{"bad time", map[string]string{AnnotationOverride: "1", AnnotationOverrideUntil: "tomorrow"}, 0, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, err := ParseOverride(c.ann, t0)
			if (err != nil) != c.wantErr || (o != nil) != c.active || (o != nil && o.Value != c.want) {
				t.Fatalf("got %+v, %v", o, err)
			}
		})
	}
	if !Paused(map[string]string{AnnotationPaused: "true"}) || Paused(map[string]string{AnnotationPaused: "yes"}) || Paused(nil) {
		t.Fatal("paused only on exactly \"true\"")
	}
}

func TestBreaker(t *testing.T) {
	b := NewBreaker(3, 15*time.Minute)
	for i := 0; i < 2; i++ {
		b.Failure(t0)
	}
	if !b.Allow(t0) {
		t.Fatal("two failures must not open it")
	}
	b.Failure(t0)
	if b.Allow(t0.Add(14 * time.Minute)) {
		t.Fatal("open for 15 minutes")
	}
	if !b.Allow(t0.Add(15 * time.Minute)) {
		t.Fatal("one test call after the pause")
	}
	b.Failure(t0.Add(15 * time.Minute))
	if b.Allow(t0.Add(16 * time.Minute)) {
		t.Fatal("a failed test call reopens it")
	}
	b.Success()
	if !b.Allow(t0.Add(16 * time.Minute)) {
		t.Fatal("success closes it")
	}
}

func TestPlausible(t *testing.T) {
	for _, c := range []struct {
		v, ref int
		ok     bool
	}{{9, 3, true}, {10, 3, false}, {1, 3, true}, {0, 3, false}, {3, 0, true}, {4, 0, false}} {
		if Plausible(c.v, c.ref, 3) != c.ok {
			t.Errorf("Plausible(%d, %d) != %v", c.v, c.ref, c.ok)
		}
	}
}
