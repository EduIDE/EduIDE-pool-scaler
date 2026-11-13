package scaler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/budget"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/guardrail"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/metrics"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/strategy"
)

var now = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

type fakeKube struct {
	apps    map[string]*model.App
	err     error
	quota   *budget.Resources
	patches map[string]int
}

func (f *fakeKube) Apps(context.Context) (map[string]*model.App, error) {
	if f.err != nil {
		return nil, f.err
	}
	// Fresh copies: the scaler fills them in place.
	out := map[string]*model.App{}
	for k, v := range f.apps {
		c := *v
		out[k] = &c
	}
	return out, nil
}
func (f *fakeKube) Budget(context.Context) (*budget.Resources, *budget.Resources, error) {
	return f.quota, nil, nil
}
func (f *fakeKube) PatchTargetIdle(_ context.Context, app string, v int) error {
	f.patches[app] = v
	return nil
}

type fakeProm struct {
	rates map[string]float64
	err   error
}

func (f *fakeProm) ClaimRates(context.Context, time.Duration, time.Time) (map[string]float64, error) {
	return f.rates, f.err
}
func (f *fakeProm) ReadySeconds(context.Context, time.Time) (map[string]float64, error) {
	return map[string]float64{}, f.err
}
func (f *fakeProm) Scalar(context.Context, string, time.Time) (float64, error) { return 0, f.err }
func (f *fakeProm) ClaimHistory(context.Context, time.Time, time.Time) (map[string][]strategy.Sample, error) {
	return nil, errors.New("no history")
}

func app(name string) *model.App {
	return &model.App{Name: name, MaxInstances: 100, PodCPU: 0.6, PodMemory: 650e6}
}

func newScaler(t *testing.T, mode config.Mode, active string, k *fakeKube, p *fakeProm) *Scaler {
	t.Helper()
	cfg := config.Default()
	cfg.Namespace = "ns"
	cfg.Mode = mode
	cfg.ActiveStrategy = active
	cfg.Guardrails.ScaleDownCooldown = config.Duration{}
	s := New(cfg, time.UTC, k, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if p != nil {
		s.Prom = p
	}
	return s
}

func TestShadowPatchesNothing(t *testing.T) {
	k := &fakeKube{apps: map[string]*model.App{"java-17": app("java-17")}, patches: map[string]int{}}
	s := newScaler(t, config.ModeShadow, config.StrategyReactive, k, &fakeProm{rates: map[string]float64{"java-17": 0.1}})
	if err := s.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(k.patches) != 0 {
		t.Fatalf("shadow mode patched %v", k.patches)
	}
	if v := testutil.ToFloat64(s.Metrics.TargetIdle.WithLabelValues("java-17", "reactive")); v != 8 {
		t.Fatalf("target_idle metric %v, want 8", v)
	}
}

func TestActivePatchesOnlyChanges(t *testing.T) {
	a := app("java-17")
	eight := 8
	b := app("c")
	b.TargetIdle = &eight
	k := &fakeKube{apps: map[string]*model.App{"java-17": a, "c": b}, patches: map[string]int{}}
	s := newScaler(t, config.ModeActive, config.StrategyReactive, k, &fakeProm{rates: map[string]float64{"java-17": 0.1, "c": 0.1}})
	if err := s.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if k.patches["java-17"] != 8 {
		t.Fatalf("patches %v", k.patches)
	}
	if _, ok := k.patches["c"]; ok {
		t.Fatal("unchanged value must not be patched")
	}
	if testutil.ToFloat64(s.Metrics.Patches.WithLabelValues("java-17", "success")) != 1 {
		t.Fatal("patch not counted")
	}
}

func TestNoSnapshotIsFailStatic(t *testing.T) {
	k := &fakeKube{err: errors.New("apiserver down"), patches: map[string]int{}}
	s := newScaler(t, config.ModeActive, config.StrategyReactive, k, nil)
	if err := s.Tick(context.Background(), now); err == nil || len(k.patches) != 0 {
		t.Fatalf("err %v patches %v", err, k.patches)
	}
	if testutil.ToFloat64(s.Metrics.ProviderUp.WithLabelValues("kubernetes")) != 0 {
		t.Fatal("provider_up must drop")
	}
}

func TestPausedIsLeftAlone(t *testing.T) {
	a := app("java-17")
	a.Annotations = map[string]string{guardrail.AnnotationPaused: "true"}
	k := &fakeKube{apps: map[string]*model.App{"java-17": a}, patches: map[string]int{}}
	s := newScaler(t, config.ModeActive, config.StrategyReactive, k, &fakeProm{rates: map[string]float64{"java-17": 1}})
	_ = s.Tick(context.Background(), now)
	if len(k.patches) != 0 || testutil.ToFloat64(s.Metrics.Paused.WithLabelValues("java-17")) != 1 {
		t.Fatalf("patches %v", k.patches)
	}
}

func TestOverrideBypassesGuardrailsButNotBudget(t *testing.T) {
	a := app("java-17")
	a.Annotations = map[string]string{
		guardrail.AnnotationOverride:      "50",
		guardrail.AnnotationOverrideUntil: now.Add(time.Hour).Format(time.RFC3339),
	}
	k := &fakeKube{apps: map[string]*model.App{"java-17": a}, patches: map[string]int{}}
	s := newScaler(t, config.ModeActive, config.StrategyReactive, k, &fakeProm{rates: map[string]float64{"java-17": 0}})
	// Default maxStepUp is 20 and ceiling 100: the override jumps straight to 50.
	_ = s.Tick(context.Background(), now)
	if k.patches["java-17"] != 50 {
		t.Fatalf("patches %v", k.patches)
	}
	if testutil.ToFloat64(s.Metrics.OverrideActive.WithLabelValues("java-17")) != 1 {
		t.Fatal("override_active not set")
	}
	// The quota leaves room for 10 pods: the budget still applies.
	k.quota = &budget.Resources{CPU: 6, Memory: 1e12}
	_ = s.Tick(context.Background(), now.Add(time.Minute))
	if k.patches["java-17"] != 10 {
		t.Fatalf("budget ignored: %v", k.patches)
	}
}

func TestFallbackToReactiveAndLastValid(t *testing.T) {
	k := &fakeKube{apps: map[string]*model.App{"java-17": app("java-17")}, patches: map[string]int{}}
	p := &fakeProm{rates: map[string]float64{"java-17": 0.1}}
	// predictive is configured but untrained: the chain falls to reactive.
	s := newScaler(t, config.ModeActive, config.StrategyPredictive, k, p)
	s.AddPredictive(strategy.NewPredictive(s.Cfg.Predictive, time.UTC, 60))
	_ = s.Tick(context.Background(), now)
	if k.patches["java-17"] != 8 {
		t.Fatalf("patches %v", k.patches)
	}
	if testutil.ToFloat64(s.Metrics.StrategyFallbacks.WithLabelValues("predictive", "error", "predictive")) != 1 {
		t.Fatal("fallback not counted")
	}
	// Prometheus down: reactive has no rate either, the last valid value holds.
	p.err = errors.New("down")
	_ = s.Tick(context.Background(), now.Add(time.Minute))
	if k.patches["java-17"] != 8 {
		t.Fatalf("patches %v", k.patches)
	}
}

func TestStaticSkipsReactiveFloor(t *testing.T) {
	k := &fakeKube{apps: map[string]*model.App{"java-17": app("java-17")}, patches: map[string]int{}}
	s := newScaler(t, config.ModeActive, config.StrategyStatic, k, &fakeProm{rates: map[string]float64{"java-17": 1}})
	three := 3
	s.Cfg.Defaults.Static = &three
	s.Strategies[config.StrategyStatic] = strategy.NewStatic(s.Cfg.Limits)
	_ = s.Tick(context.Background(), now)
	if k.patches["java-17"] != 3 {
		t.Fatalf("static baseline must not be lifted by reactive: %v", k.patches)
	}
}

func TestEventsAreCountedAndRemembered(t *testing.T) {
	k := &fakeKube{apps: map[string]*model.App{"java-17": app("java-17")}, patches: map[string]int{}}
	s := newScaler(t, config.ModeShadow, config.StrategyReactive, k, nil)
	s.Sources = []EventSource{fakeSource{events: []model.Event{
		{AppDefinition: "java-17", Kind: model.EventExam, Time: now.Add(24 * time.Hour)},
		{AppDefinition: "java-17", Kind: model.EventDue, Time: now.Add(30 * 24 * time.Hour)},
	}}}
	_ = s.Tick(context.Background(), now)
	if testutil.ToFloat64(s.Metrics.UpcomingEvents.WithLabelValues("java-17")) != 1 {
		t.Fatal("only events within 7 days count")
	}
	if len(s.pastEvents) != 2 {
		t.Fatalf("remembered %d", len(s.pastEvents))
	}
}

type fakeSource struct{ events []model.Event }

func (f fakeSource) Name() string { return "calendar" }
func (f fakeSource) Events(context.Context, time.Time) ([]model.Event, error) {
	return f.events, nil
}

func TestSwitchback(t *testing.T) {
	cfg := config.Default()
	cfg.ActiveStrategy = config.StrategyReactive
	if got := ActiveStrategy(cfg, time.UTC, now, nil); got != config.StrategyReactive {
		t.Fatalf("switchback off: %s", got)
	}
	cfg.Switchback = config.Switchback{
		Enabled:         true,
		Seed:            42,
		Strategies:      []string{"static", "reactive", "predictive"},
		ExamDayStrategy: "predictive",
		Schedule:        map[string]string{"2026-11-17": "static"},
	}
	if got := ActiveStrategy(cfg, time.UTC, now.Add(24*time.Hour), nil); got != "static" {
		t.Fatalf("pinned date: %s", got)
	}
	exam := []model.Event{{Kind: model.EventExam, Time: now.Add(3 * time.Hour)}}
	if got := ActiveStrategy(cfg, time.UTC, now, exam); got != "predictive" {
		t.Fatalf("exam day: %s", got)
	}
	// Every weekday sees each strategy exactly once per block of three weeks.
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC) // a Monday, block aligned or not
	for wd := 0; wd < 7; wd++ {
		for block := 0; block < 4; block++ {
			seen := map[string]int{}
			// Align to the scheme's own blocks.
			base := daysSinceEpoch(start) / 7
			first := (base/3 + int64(block)) * 3
			for w := first; w < first+3; w++ {
				day := time.Unix((w*7+int64(wd))*86400, 0).UTC().Add(12 * time.Hour)
				seen[ActiveStrategy(cfg, time.UTC, day, nil)]++
			}
			if len(seen) != 3 {
				t.Fatalf("weekday %d block %d: %v", wd, block, seen)
			}
		}
	}
	// The same day always gives the same strategy.
	if ActiveStrategy(cfg, time.UTC, start, nil) != ActiveStrategy(cfg, time.UTC, start.Add(time.Hour), nil) {
		t.Fatal("not stable within a day")
	}
	// Local date decides: 23:30 UTC on Sunday is Monday in Berlin.
	berlin, _ := time.LoadLocation("Europe/Berlin")
	cfg.Switchback.Schedule = map[string]string{"2026-11-16": "reactive"}
	if got := ActiveStrategy(cfg, berlin, time.Date(2026, 11, 15, 23, 30, 0, 0, time.UTC), nil); got != "reactive" {
		t.Fatalf("timezone ignored: %s", got)
	}
}

func TestDecisionModelErrorsAreCounted(t *testing.T) {
	k := &fakeKube{apps: map[string]*model.App{"java-17": app("java-17")}, patches: map[string]int{}}
	s := newScaler(t, config.ModeShadow, config.StrategyDecisionModel, k, &fakeProm{rates: map[string]float64{"java-17": 0}})
	pred := strategy.NewPredictive(s.Cfg.Predictive, time.UTC, 60)
	pred.Train(now, map[string][]strategy.Sample{}, nil)
	s.AddPredictive(pred)
	s.AddDecisionModel(strategy.NewDecisionModel(s.Cfg.DecisionModel, &strategy.FakeDecisionClient{Fn: func(strategy.Question) (strategy.Answer, error) {
		return strategy.Answer{}, errors.New("timeout")
	}}, pred, time.UTC))
	_ = s.Tick(context.Background(), now)
	if testutil.ToFloat64(s.Metrics.StrategyErrors.WithLabelValues("decision-model")) != 1 {
		t.Fatal("decision model error not counted")
	}
	if testutil.ToFloat64(s.Metrics.StrategyFallbacks.WithLabelValues("decision-model", "missing", "decision-model")) != 1 {
		t.Fatal("fallback not counted")
	}
	// predictive and reactive run alongside and are exported; the failed
	// decision model has nothing to export.
	if n := testutil.CollectAndCount(s.Metrics.DesiredIdle); n != 2 {
		t.Fatalf("desired_idle series %d, want 2", n)
	}
}
