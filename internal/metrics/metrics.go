// Package metrics defines the scaler's own Prometheus metrics. Names and
// labels are the ones in the plan; the Grafana board and the alerts in the
// EduIDE-Helm chart query exactly these.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds every metric the scaler exports.
type Metrics struct {
	Registry *prometheus.Registry

	DesiredIdle          *prometheus.GaugeVec
	TargetIdle           *prometheus.GaugeVec
	ForecastStarts       *prometheus.GaugeVec
	ForecastDistribution *prometheus.GaugeVec
	Budget               *prometheus.GaugeVec
	BudgetClamped        *prometheus.CounterVec
	ProviderUp           *prometheus.GaugeVec
	ProviderErrors       *prometheus.CounterVec
	StrategyErrors       *prometheus.CounterVec
	StrategyFallbacks    *prometheus.CounterVec
	OverrideActive       *prometheus.GaugeVec
	Paused               *prometheus.GaugeVec
	LastDecision         prometheus.Gauge
	Patches              *prometheus.CounterVec
	UpcomingEvents       *prometheus.GaugeVec
	ActiveStrategy       *prometheus.GaugeVec
	Mode                 *prometheus.GaugeVec
	DecisionModelBreaker prometheus.Gauge
}

// New registers every metric on a fresh registry.
func New() *Metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := func(c prometheus.Collector) { r.MustRegister(c) }
	m := &Metrics{
		Registry: r,
		DesiredIdle: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_desired_idle",
			Help: "targetIdle each strategy recommends, before guardrails and budget.",
		}, []string{"appdef", "strategy", "active", "active_strategy"}),
		TargetIdle: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_target_idle",
			Help: "targetIdle the scaler decided after guardrails, override and budget. In shadow mode it is not patched.",
		}, []string{"appdef", "active_strategy"}),
		ForecastStarts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_forecast_starts",
			Help: "Expected session starts within the strategy's horizon.",
		}, []string{"appdef", "strategy"}),
		ForecastDistribution: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_forecast_demand_probability",
			Help: "P(demand <= level) within the horizon, for calibration.",
		}, []string{"appdef", "strategy", "level"}),
		Budget: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_budget",
			Help: "Free budget per source, CPU in cores and memory in bytes.",
		}, []string{"resource", "limit_source"}),
		BudgetClamped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pool_scaler_budget_clamped_total",
			Help: "Decisions the budget cut below the guarded wish.",
		}, []string{"appdef"}),
		ProviderUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_provider_up",
			Help: "1 when the provider's last call succeeded.",
		}, []string{"provider"}),
		ProviderErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pool_scaler_provider_errors_total",
			Help: "Failed provider calls.",
		}, []string{"provider"}),
		StrategyErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pool_scaler_strategy_errors_total",
			Help: "Strategy failures, including failed decision model calls.",
		}, []string{"strategy"}),
		StrategyFallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pool_scaler_strategy_fallbacks_total",
			Help: "Decisions where the active strategy could not be used. strategy is the one that failed.",
		}, []string{"strategy", "reason", "active_strategy"}),
		OverrideActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_override_active",
			Help: "1 while a manual override annotation is in force.",
		}, []string{"appdef"}),
		Paused: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_paused",
			Help: "1 while the AppDefinition carries the pause annotation.",
		}, []string{"appdef"}),
		LastDecision: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pool_scaler_last_decision_timestamp_seconds",
			Help: "Unix time of the last completed decision round.",
		}),
		Patches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pool_scaler_patches_total",
			Help: "Patches of spec.targetIdle by result (success, error).",
		}, []string{"appdef", "result"}),
		UpcomingEvents: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_upcoming_events",
			Help: "Known events in the next 7 days.",
		}, []string{"appdef"}),
		ActiveStrategy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_active_strategy",
			Help: "1 for the strategy active today.",
		}, []string{"strategy"}),
		Mode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_scaler_mode",
			Help: "1 for the configured mode (shadow, active).",
		}, []string{"mode"}),
		DecisionModelBreaker: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pool_scaler_decision_model_breaker_open",
			Help: "1 while the decision model circuit breaker blocks calls.",
		}),
	}
	for _, c := range []prometheus.Collector{
		m.DesiredIdle, m.TargetIdle, m.ForecastStarts, m.ForecastDistribution, m.Budget, m.BudgetClamped,
		m.ProviderUp, m.ProviderErrors, m.StrategyErrors, m.StrategyFallbacks, m.OverrideActive, m.Paused,
		m.LastDecision, m.Patches, m.UpcomingEvents, m.ActiveStrategy, m.Mode, m.DecisionModelBreaker,
	} {
		f(c)
	}
	return m
}

// Provider records the outcome of one provider call.
func (m *Metrics) Provider(name string, err error) {
	if err != nil {
		m.ProviderUp.WithLabelValues(name).Set(0)
		m.ProviderErrors.WithLabelValues(name).Inc()
		return
	}
	m.ProviderUp.WithLabelValues(name).Set(1)
}
