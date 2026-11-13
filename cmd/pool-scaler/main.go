// Command pool-scaler decides how many free prewarmed pods each EduIDE
// AppDefinition keeps, by patching spec.targetIdle. `pool-scaler backtest`
// replays Prometheus history against the strategies instead.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time/tzdata" // the image has no zoneinfo

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/EduIDE/EduIDE-pool-scaler/internal/backtest"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/budget"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/config"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/metrics"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/model"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/provider/artemis"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/provider/calendar"
	kube "github.com/EduIDE/EduIDE-pool-scaler/internal/provider/kubernetes"
	promprovider "github.com/EduIDE/EduIDE-pool-scaler/internal/provider/prometheus"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/scaler"
	"github.com/EduIDE/EduIDE-pool-scaler/internal/strategy"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "run":
		err = run(args, log)
	case "backtest":
		err = runBacktest(args)
	default:
		err = fmt.Errorf("unknown command %q, want run or backtest", cmd)
	}
	if err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/pool-scaler/config.yaml", "configuration file")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	loc, _ := time.LoadLocation(cfg.Timezone)

	rest, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(rest)
	if err != nil {
		return err
	}
	core, err := kubernetes.NewForConfig(rest)
	if err != nil {
		return err
	}
	overhead, err := resources(cfg.Budget.PodOverhead)
	if err != nil {
		return fmt.Errorf("budget.podOverhead: %w", err)
	}
	k := &kube.Provider{Dynamic: dyn, Core: core, Namespace: cfg.Namespace, NodeSelector: cfg.Budget.NodeSelector, PodOverhead: overhead}

	m := metrics.New()
	s := scaler.New(cfg, loc, k, m, log)
	if err := addOptional(s, cfg, loc); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server", "err", err)
			stop()
		}
	}()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	log.Info("pool-scaler started", "namespace", cfg.Namespace, "mode", cfg.Mode, "interval", cfg.Interval.String())
	t := time.NewTicker(cfg.Interval.Duration)
	defer t.Stop()
	for {
		tctx, cancel := context.WithTimeout(ctx, cfg.Interval.Duration)
		if err := s.Tick(tctx, time.Now()); err != nil {
			log.Error("decision round failed", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// addOptional wires Prometheus, the event sources, predictive and the
// decision model when configured.
func addOptional(s *scaler.Scaler, cfg config.Config, loc *time.Location) error {
	if cfg.Prometheus.URL != "" {
		p, err := promprovider.New(cfg.Prometheus.URL, cfg.Namespace, cfg.Prometheus.Timeout.Duration)
		if err != nil {
			return err
		}
		s.Prom = p
		pred := strategy.NewPredictive(cfg.Predictive, loc, cfg.Reactive.DefaultReadySeconds)
		s.AddPredictive(pred)
		if cfg.DecisionModel.URL != "" {
			c, err := strategy.NewHTTPDecisionClient(cfg.DecisionModel.URL, os.Getenv("DECISION_MODEL_TOKEN"), nil)
			if err != nil {
				return err
			}
			s.AddDecisionModel(strategy.NewDecisionModel(cfg.DecisionModel, c, pred, loc))
		}
	}
	if cfg.Calendar.File != "" {
		s.Sources = append(s.Sources, calendarSource{&calendar.Provider{Path: cfg.Calendar.File}})
	}
	if cfg.Artemis.URL != "" {
		token := os.Getenv("ARTEMIS_TOKEN")
		if token == "" {
			return errors.New("artemis.url is set but $ARTEMIS_TOKEN is empty")
		}
		s.Sources = append(s.Sources, artemisSource{&artemis.Provider{
			BaseURL: cfg.Artemis.URL, Token: token, Days: cfg.Artemis.Days, Interval: cfg.Artemis.Interval.Duration,
			Client: &http.Client{Timeout: cfg.Artemis.Timeout.Duration},
		}})
	}
	return nil
}

type calendarSource struct{ p *calendar.Provider }

func (c calendarSource) Name() string { return "calendar" }
func (c calendarSource) Events(context.Context, time.Time) ([]model.Event, error) {
	return c.p.Events()
}

type artemisSource struct{ p *artemis.Provider }

func (a artemisSource) Name() string { return "artemis" }
func (a artemisSource) Events(ctx context.Context, now time.Time) ([]model.Event, error) {
	return a.p.Events(ctx, now)
}

func resources(q config.Quantity) (budget.Resources, error) {
	var r budget.Resources
	for _, f := range []struct {
		s   string
		dst *float64
	}{{q.CPU, &r.CPU}, {q.Memory, &r.Memory}} {
		if f.s == "" {
			continue
		}
		v, err := resource.ParseQuantity(f.s)
		if err != nil {
			return r, err
		}
		*f.dst = v.AsApproximateFloat64()
	}
	return r, nil
}

func runBacktest(args []string) error {
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	cfgPath := fs.String("config", "", "configuration file for the strategy parameters (optional)")
	promURL := fs.String("prometheus", "", "Prometheus URL (default: prometheus.url from the config)")
	ns := fs.String("namespace", "", "namespace of the operator metrics (default: from the config)")
	from := fs.String("from", "", "start, RFC3339 (default: 7 days ago)")
	to := fs.String("to", "", "end, RFC3339 (default: now)")
	step := fs.Duration("step", time.Minute, "simulation step")
	names := fs.String("strategies", "static,reactive,predictive", "strategies to compare")
	ready := fs.Float64("ready-seconds", 0, "pool pod ready time (default: reactive.defaultReadySeconds)")
	_ = fs.Parse(args)

	cfg := config.Default()
	if *cfgPath != "" {
		c, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		cfg = c
	}
	if *promURL != "" {
		cfg.Prometheus.URL = *promURL
	}
	if *ns != "" {
		cfg.Namespace = *ns
	}
	if cfg.Prometheus.URL == "" || cfg.Namespace == "" {
		return errors.New("backtest needs a Prometheus URL and a namespace")
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return err
	}
	end, start := time.Now(), time.Now().Add(-7*24*time.Hour)
	if *to != "" {
		if end, err = time.Parse(time.RFC3339, *to); err != nil {
			return err
		}
	}
	if *from != "" {
		if start, err = time.Parse(time.RFC3339, *from); err != nil {
			return err
		}
	}
	if *ready <= 0 {
		*ready = cfg.Reactive.DefaultReadySeconds
	}

	ctx := context.Background()
	p, err := promprovider.New(cfg.Prometheus.URL, cfg.Namespace, cfg.Prometheus.Timeout.Duration)
	if err != nil {
		return err
	}
	claims, err := p.History(ctx, start, end, *step)
	if err != nil {
		return err
	}
	var events []model.Event
	if cfg.Calendar.File != "" {
		if events, err = (&calendar.Provider{Path: cfg.Calendar.File}).Events(); err != nil {
			return err
		}
	}

	reactive := strategy.NewReactive(cfg.Reactive)
	in := backtest.Input{Claims: claims, Events: events, From: start, To: end, Step: *step, ReadySeconds: *ready}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "STRATEGY\tAPPDEF\tCLAIMS\tWARM_READY\tWARM_STARTING\tCOLD\tWARM_READY_RATIO\tIDLE_POD_HOURS\tMEAN_TARGET")
	for _, name := range strings.Split(*names, ",") {
		var st strategy.Strategy
		floor := reactive
		switch name {
		case config.StrategyStatic:
			st, floor = strategy.NewStatic(cfg.Limits), nil
		case config.StrategyReactive:
			st = reactive
		case config.StrategyPredictive, config.StrategyDecisionModel:
			// shortcut: trained once on the weeks before --from, while the live
			// scaler retrains daily; fine for runs of a few weeks.
			pred := strategy.NewPredictive(cfg.Predictive, loc, *ready)
			hist, err := p.ClaimHistory(ctx, start.Add(-time.Duration(cfg.Predictive.TrainWeeks)*7*24*time.Hour), start)
			if err != nil {
				return err
			}
			pred.Train(start, hist, events)
			st = pred
			if name == config.StrategyDecisionModel {
				c, err := strategy.NewHTTPDecisionClient(cfg.DecisionModel.URL, os.Getenv("DECISION_MODEL_TOKEN"), nil)
				if err != nil {
					return err
				}
				st = strategy.NewDecisionModel(cfg.DecisionModel, c, pred, loc)
			}
		default:
			return fmt.Errorf("unknown strategy %q", name)
		}
		res, err := backtest.Run(ctx, in, st, floor, cfg)
		if err != nil {
			return err
		}
		for _, r := range res {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%.3f\t%.1f\t%.1f\n", r.Strategy, r.AppDefinition, r.Claims, r.WarmReady, r.WarmStarting, r.Cold, r.WarmReadyRatio(), r.IdlePodHours, r.MeanTarget)
		}
	}
	return w.Flush()
}
