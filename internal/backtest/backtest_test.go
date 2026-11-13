package backtest

import (
	"context"
	"testing"
	"time"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/strategy"
)

func TestRun(t *testing.T) {
	cfg := config.Default()
	cfg.Interval = config.Duration{Duration: time.Minute}
	from := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	to := from.Add(2 * time.Hour)
	// One claim a minute, steady.
	var s []strategy.Sample
	for t := from; !t.After(to); t = t.Add(time.Minute) {
		s = append(s, strategy.Sample{T: t, V: 1})
	}
	in := Input{Claims: map[string][]strategy.Sample{"java-17": s}, From: from, To: to, Step: time.Minute, ReadySeconds: 120}

	reactive := strategy.NewReactive(cfg.Reactive)
	none, err := Run(context.Background(), in, strategy.NewStatic(cfg.Limits), nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if none[0].Claims != 121 || none[0].WarmReady != 0 || none[0].IdlePodHours != 0 {
		t.Fatalf("static 0 keeps no pool: %+v", none[0])
	}
	r, err := Run(context.Background(), in, reactive, reactive, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// After the first two minutes the pool keeps up: 1/60 x 120s x 1.2 -> 3 idle.
	if r[0].WarmReadyRatio() < 0.9 || r[0].IdlePodHours <= 0 {
		t.Fatalf("reactive %+v ratio %.2f", r[0], r[0].WarmReadyRatio())
	}
	if r[0].WarmReady+r[0].WarmStarting+r[0].Cold != r[0].Claims {
		t.Fatalf("claims not conserved: %+v", r[0])
	}
}
