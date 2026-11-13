package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
)

func snap(now time.Time, apps ...*model.App) model.Snapshot {
	s := model.Snapshot{Now: now, Apps: map[string]*model.App{}}
	for _, a := range apps {
		s.Apps[a.Name] = a
	}
	return s
}

func TestReactive(t *testing.T) {
	r := NewReactive(config.Default().Reactive)
	cases := []struct {
		name  string
		app   model.App
		want  int
		found bool
	}{
		{"unknown rate falls back", model.App{Name: "a", ClaimRate: -1}, 0, false},
		{"no claims", model.App{Name: "a", ClaimRate: 0}, 0, true},
		// 0.1/s x 60s x 1.2 = 7.2
		{"default ready time", model.App{Name: "a", ClaimRate: 0.1}, 8, true},
		// 0.1/s x 30s x 1.2 = 3.6
		{"measured ready time", model.App{Name: "a", ClaimRate: 0.1, ReadySeconds: 30}, 4, true},
		{"a single recent claim keeps one pod", model.App{Name: "a", ClaimRate: 1.0 / 300}, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, ok := r.ForApp(&c.app)
			if ok != c.found || rec.TargetIdle != c.want {
				t.Fatalf("got %d/%v, want %d/%v", rec.TargetIdle, ok, c.want, c.found)
			}
		})
	}
}

func TestPoissonCDFAndQuantile(t *testing.T) {
	levels := []int{0, 1, 2, 5, 10, 20, 160}
	d := PoissonCDF(0, levels)
	if QuantileLevel(d, 0.9) != 0 {
		t.Fatalf("zero demand must give level 0, got %v", d)
	}
	d = PoissonCDF(4, levels)
	if err := ValidateDistribution(d); err != nil {
		t.Fatal(err)
	}
	// P(X<=5 | 4) = 0.785, P(X<=10 | 4) = 0.997
	if math.Abs(d[3].Probability-0.785) > 0.001 {
		t.Fatalf("P(<=5) = %v", d[3].Probability)
	}
	if got := QuantileLevel(d, 0.9); got != 10 {
		t.Fatalf("q=0.9 level = %d, want 10", got)
	}
	// Large lambda must not underflow to an all zero distribution.
	d = PoissonCDF(500, levels)
	if got := QuantileLevel(d, 0.9); got != 160 {
		t.Fatalf("lambda beyond the last level must pick it, got %d", got)
	}
}

func TestValidateDistribution(t *testing.T) {
	bad := [][]model.LevelProbability{
		nil,
		{{Level: 0, Probability: 0.5}, {Level: 1, Probability: 0.4}},
		{{Level: 1, Probability: 0.5}, {Level: 1, Probability: 0.6}},
		{{Level: 0, Probability: math.NaN()}},
		{{Level: 0, Probability: 1.5}},
	}
	for i, d := range bad {
		if ValidateDistribution(d) == nil {
			t.Errorf("case %d: accepted %v", i, d)
		}
	}
}

func weeklyHistory(app string, end time.Time, weeks int, at func(time.Time) float64) map[string][]Sample {
	var s []Sample
	for t := end.Add(-time.Duration(weeks) * 7 * 24 * time.Hour); !t.After(end); t = t.Add(SlotLength) {
		s = append(s, Sample{T: t, V: at(t.Add(-SlotLength))})
	}
	return map[string][]Sample{app: s}
}

func TestPredictiveProfile(t *testing.T) {
	cfg := config.Default().Predictive
	cfg.HorizonBuffer = config.Duration{}
	p := NewPredictive(cfg, time.UTC, 900) // horizon = exactly one slot
	monday9 := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

	if _, err := p.Recommend(context.Background(), snap(monday9, &model.App{Name: "java-17"})); !errors.Is(err, ErrNotTrained) {
		t.Fatalf("untrained predictive must fail, got %v", err)
	}
	// 20 claims every Monday 09:00-09:15, nothing else.
	hist := weeklyHistory("java-17", monday9.Add(-time.Hour), 4, func(t time.Time) float64 {
		if t.Weekday() == time.Monday && t.Hour() == 9 && t.Minute() == 0 {
			return 20
		}
		return 0
	})
	p.Train(monday9.Add(-time.Hour), hist, nil)
	if p.NeedsTraining(monday9) {
		t.Fatal("just trained")
	}

	recs, err := p.Recommend(context.Background(), snap(monday9, &model.App{Name: "java-17"}))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(recs[0].Forecast-20) > 1e-6 {
		t.Fatalf("forecast %v, want 20", recs[0].Forecast)
	}
	// Poisson(20): P(<=20)=0.56, P(<=30)=0.987 -> level 30 at q=0.9
	if recs[0].TargetIdle != 30 {
		t.Fatalf("targetIdle %d, want 30", recs[0].TargetIdle)
	}
	quiet, _ := p.Recommend(context.Background(), snap(monday9.Add(2*time.Hour), &model.App{Name: "java-17"}))
	if quiet[0].TargetIdle != 0 {
		t.Fatalf("quiet slot gives %d", quiet[0].TargetIdle)
	}
	if !p.NeedsTraining(monday9.Add(25 * time.Hour)) {
		t.Fatal("must retrain after a day")
	}
}

func TestPredictiveEventUplift(t *testing.T) {
	cfg := config.Default().Predictive
	cfg.HorizonBuffer = config.Duration{}
	p := NewPredictive(cfg, time.UTC, 600)
	now := time.Date(2026, 11, 20, 7, 50, 0, 0, time.UTC)
	p.Train(now, map[string][]Sample{}, nil)

	exam := model.Event{AppDefinition: "java-17", Kind: model.EventExam, Time: now.Add(10 * time.Minute), Participants: 300}
	s := snap(now, &model.App{Name: "java-17"}, &model.App{Name: "c"})
	s.Events = []model.Event{exam}
	recs, err := p.Recommend(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	// Exam window 30 min, factor 0.9: 270 starts, 10 of 30 minutes in the horizon.
	byApp := map[string]model.Recommendation{}
	for _, r := range recs {
		byApp[r.AppDefinition] = r
	}
	if math.Abs(byApp["java-17"].Forecast-90) > 1e-6 {
		t.Fatalf("forecast %v, want 90", byApp["java-17"].Forecast)
	}
	if byApp["c"].Forecast != 0 {
		t.Fatal("an event must only lift its own AppDefinition")
	}
}

func TestPredictiveEstimatesFactor(t *testing.T) {
	cfg := config.Default().Predictive
	cfg.MinEventsForFactor = 2
	p := NewPredictive(cfg, time.UTC, 60)
	end := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	examA := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC) // a Tuesday
	examB := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC) // a Friday
	inWindow := func(t time.Time, at time.Time) bool {
		return !t.Before(at.Add(-15*time.Minute)) && t.Before(at.Add(15*time.Minute))
	}
	hist := weeklyHistory("java-17", end, 2, func(t time.Time) float64 {
		if inWindow(t, examA) || inWindow(t, examB) {
			return 25 // 50 starts per exam
		}
		return 0
	})
	events := []model.Event{
		{AppDefinition: "java-17", Kind: model.EventExam, Time: examA, Participants: 100},
		{AppDefinition: "java-17", Kind: model.EventExam, Time: examB, Participants: 100},
	}
	p.Train(end, hist, events)
	// The weekly profile only holds one week for these slots, so it already
	// partly expects the exam: (15 + 35) / 2 / 100 = 0.25 instead of the true 0.5.
	if f := p.Factor(model.EventExam); math.Abs(f-0.25) > 1e-6 {
		t.Fatalf("estimated factor %v", f)
	}
	if p.Factor(model.EventDue) != cfg.Events["due"].Factor {
		t.Fatal("kinds without enough events keep the configured factor")
	}
}

func dmFixture(t *testing.T, client DecisionClient) (*DecisionModel, *Predictive, time.Time) {
	t.Helper()
	pc := config.Default().Predictive
	pc.Levels = []int{0, 2, 5, 10, 20}
	pc.HorizonBuffer = config.Duration{}
	pred := NewPredictive(pc, time.UTC, 900)
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	// About 4 starts per slot, so predictive picks 5 or 10.
	pred.Train(now, weeklyHistory("java-17", now, 2, func(time.Time) float64 { return 4 }), nil)
	dc := config.Default().DecisionModel
	dc.Levels = pc.Levels
	return NewDecisionModel(dc, client, pred, time.UTC), pred, now
}

func answer(p ...float64) func(Question) (Answer, error) {
	return func(Question) (Answer, error) { return Answer{Probabilities: p}, nil }
}

func TestDecisionModelQuantileAndCaching(t *testing.T) {
	calls := 0
	fake := &FakeDecisionClient{Fn: func(q Question) (Answer, error) {
		calls++
		if len(q.Options) != 5 || q.Options[4] != "20+" {
			t.Fatalf("options %v", q.Options)
		}
		return answer(0, 0.1, 0.5, 0.35, 0.05)(q)
	}}
	dm, _, now := dmFixture(t, fake)
	recs, err := dm.Recommend(context.Background(), snap(now, &model.App{Name: "java-17"}))
	if err != nil || len(recs) != 1 {
		t.Fatalf("%v %v", recs, err)
	}
	// cumulative 0, .1, .6, .95, 1 -> q=0.9 gives 10
	if recs[0].TargetIdle != 10 {
		t.Fatalf("targetIdle %d", recs[0].TargetIdle)
	}
	// Within the interval the answer is reused.
	if _, err := dm.Recommend(context.Background(), snap(now.Add(5*time.Minute), &model.App{Name: "java-17"})); err != nil || calls != 1 {
		t.Fatalf("calls %d, err %v", calls, err)
	}
}

func TestDecisionModelRejectsBadAnswers(t *testing.T) {
	cases := map[string]func(Question) (Answer, error){
		"error":          func(Question) (Answer, error) { return Answer{}, errors.New("boom") },
		"wrong length":   answer(0.5, 0.5),
		"not summing":    answer(0.1, 0.1, 0.1, 0.1, 0.1),
		"negative":       answer(-0.5, 0.5, 0.5, 0.5, 0),
		"implausible":    answer(0, 0, 0, 0, 1), // 20 against predictive 5/10 within k=3 is fine...
		"implausible lo": answer(1, 0, 0, 0, 0), // ...but 0 against a positive predictive is not
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			var failed []error
			dm, _, now := dmFixture(t, &FakeDecisionClient{Fn: fn})
			dm.OnError = func(_ string, err error) { failed = append(failed, err) }
			recs, err := dm.Recommend(context.Background(), snap(now, &model.App{Name: "java-17"}))
			if err != nil {
				t.Fatal(err)
			}
			if name == "implausible" {
				if len(recs) != 1 {
					t.Fatalf("20 is within factor 3 of predictive, must be accepted: %v", failed)
				}
				return
			}
			if len(recs) != 0 || len(failed) != 1 {
				t.Fatalf("recs %v, failures %v", recs, failed)
			}
		})
	}
}

func TestDecisionModelBreakerAndStaleness(t *testing.T) {
	fail := true
	calls := 0
	dm, _, now := dmFixture(t, &FakeDecisionClient{Fn: func(q Question) (Answer, error) {
		calls++
		if fail {
			return Answer{}, errors.New("down")
		}
		return answer(0, 0.1, 0.5, 0.35, 0.05)(q)
	}})
	app := &model.App{Name: "java-17"}
	step := dm.cfg.Interval.Duration
	for i := 0; i < 3; i++ {
		_, _ = dm.Recommend(context.Background(), snap(now.Add(time.Duration(i)*step), app))
	}
	if calls != 3 || !dm.BreakerOpen(now.Add(3*step)) {
		t.Fatalf("calls %d, breaker must be open after 3 failures", calls)
	}
	// Open for 15 min: the next due call is skipped.
	_, _ = dm.Recommend(context.Background(), snap(now.Add(3*step), app))
	if calls != 3 {
		t.Fatalf("breaker let a call through: %d", calls)
	}
	// After the pause one test call goes out and closes it.
	fail = false
	later := now.Add(2*step + dm.cfg.BreakerPause.Duration)
	recs, _ := dm.Recommend(context.Background(), snap(later, app))
	if calls != 4 || len(recs) != 1 || dm.BreakerOpen(later) {
		t.Fatalf("calls %d recs %v", calls, recs)
	}
	// An answer older than two intervals is not used.
	fail = true
	dm.breaker.Success()
	_, _ = dm.Recommend(context.Background(), snap(later.Add(step), app))
	recs, _ = dm.Recommend(context.Background(), snap(later.Add(2*step+time.Second), app))
	if len(recs) != 0 {
		t.Fatalf("stale answer used: %v", recs)
	}
}

func TestHTTPDecisionClient(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var q Question
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.Prompt == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(Answer{Probabilities: []float64{0.5, 0.5}})
	}))
	defer srv.Close()

	if _, err := NewHTTPDecisionClient("http://example.org", "x", nil); err == nil {
		t.Fatal("plain http must be rejected")
	}
	c, err := NewHTTPDecisionClient(srv.URL, "secret", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Decide(context.Background(), Question{Prompt: "p", Options: []string{"0", "1+"}})
	if err != nil || len(a.Probabilities) != 2 {
		t.Fatalf("%v %v", a, err)
	}
	c.Token = "wrong"
	if _, err := c.Decide(context.Background(), Question{Prompt: "p"}); err == nil {
		t.Fatal("401 must be an error")
	}
}

func TestStatic(t *testing.T) {
	cfg := config.Default()
	five := 5
	cfg.AppLimits = map[string]config.AppLimits{"java-17": {Static: &five}}
	recs, _ := NewStatic(cfg.Limits).Recommend(context.Background(), snap(time.Now(), &model.App{Name: "java-17"}, &model.App{Name: "c"}))
	if recs[0].AppDefinition != "c" || recs[0].TargetIdle != 0 || recs[1].TargetIdle != 5 {
		t.Fatalf("%v", recs)
	}
}
