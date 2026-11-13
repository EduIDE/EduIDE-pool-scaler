// Package config loads the scaler configuration file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

// Mode decides whether the scaler patches anything.
type Mode string

const (
	// ModeShadow computes and exports every decision but patches nothing.
	ModeShadow Mode = "shadow"
	// ModeActive patches spec.targetIdle.
	ModeActive Mode = "active"
)

// Strategy names.
const (
	StrategyStatic        = "static"
	StrategyReactive      = "reactive"
	StrategyPredictive    = "predictive"
	StrategyDecisionModel = "decision-model"
)

// Duration reads "30s" style strings.
type Duration struct{ time.Duration }

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Config is the whole configuration file.
type Config struct {
	// Namespace holding the AppDefinitions. Defaults to $POD_NAMESPACE.
	Namespace string `json:"namespace"`
	Mode      Mode   `json:"mode"`
	// Interval between two decisions.
	Interval Duration `json:"interval"`
	// Timezone used for the daily switchback schedule and the weekly profile.
	Timezone string `json:"timezone"`
	// MetricsAddr serves /metrics and /healthz.
	MetricsAddr string `json:"metricsAddr"`

	// ActiveStrategy is used when switchback is off.
	ActiveStrategy string `json:"activeStrategy"`
	// Strategies computed on every tick. The active one is always included.
	Strategies []string `json:"strategies"`

	// Apps limits which AppDefinitions the scaler manages. Empty means all.
	Apps       []string             `json:"apps"`
	Defaults   AppLimits            `json:"defaults"`
	AppLimits  map[string]AppLimits `json:"appLimits"`
	Guardrails Guardrails           `json:"guardrails"`

	Reactive      Reactive      `json:"reactive"`
	Predictive    Predictive    `json:"predictive"`
	DecisionModel DecisionModel `json:"decisionModel"`
	Budget        Budget        `json:"budget"`
	Prometheus    Prometheus    `json:"prometheus"`
	Artemis       Artemis       `json:"artemis"`
	Calendar      Calendar      `json:"calendar"`
	Switchback    Switchback    `json:"switchback"`
}

// AppLimits are the per AppDefinition bounds on targetIdle.
type AppLimits struct {
	Floor   *int `json:"floor,omitempty"`
	Ceiling *int `json:"ceiling,omitempty"`
	// Static is what the static strategy recommends.
	Static *int `json:"static,omitempty"`
}

// Limits resolves the bounds for one AppDefinition.
func (c *Config) Limits(app string) (floor, ceiling, static int) {
	pick := func(get func(AppLimits) *int, fallback int) int {
		if v := get(c.AppLimits[app]); v != nil {
			return *v
		}
		if v := get(c.Defaults); v != nil {
			return *v
		}
		return fallback
	}
	floor = pick(func(l AppLimits) *int { return l.Floor }, 0)
	ceiling = pick(func(l AppLimits) *int { return l.Ceiling }, 100)
	static = pick(func(l AppLimits) *int { return l.Static }, floor)
	return floor, ceiling, static
}

// Guardrails bound how fast targetIdle moves.
type Guardrails struct {
	MaxStepUp   int `json:"maxStepUp"`
	MaxStepDown int `json:"maxStepDown"`
	// Hysteresis is how far below the current value a wish must be before it
	// scales down at all.
	Hysteresis int `json:"hysteresis"`
	// ScaleDownCooldown is the minimum time after any increase before the next
	// decrease.
	ScaleDownCooldown Duration `json:"scaleDownCooldown"`
}

// Reactive configures the reactive strategy.
type Reactive struct {
	// Window over which the claim rate is measured.
	Window Duration `json:"window"`
	// SafetyFactor is added on top: 0.2 means 20 percent more.
	SafetyFactor float64 `json:"safetyFactor"`
	// DefaultReadySeconds is used until Prometheus knows the real value.
	DefaultReadySeconds float64 `json:"defaultReadySeconds"`
}

// EventProfile describes the demand one kind of event causes.
type EventProfile struct {
	// Factor is starts per participant. Replaced by the estimate from past
	// events once enough of them exist.
	Factor float64  `json:"factor"`
	Before Duration `json:"before"`
	After  Duration `json:"after"`
}

// Predictive configures the predictive strategy.
type Predictive struct {
	// Alpha is the EWMA weight of the newest week.
	Alpha float64 `json:"alpha"`
	// TrainWeeks of Prometheus history used for the weekly profile.
	TrainWeeks      int      `json:"trainWeeks"`
	RetrainInterval Duration `json:"retrainInterval"`
	// Quantile q: targetIdle is the smallest level with P(demand <= level) >= q.
	Quantile float64 `json:"quantile"`
	// HorizonBuffer is added to the p95 pool pod ready time.
	HorizonBuffer Duration `json:"horizonBuffer"`
	// Levels are the demand buckets, ascending.
	Levels []int `json:"levels"`
	// Events maps an event kind (exam, release, due) to its demand profile.
	Events map[string]EventProfile `json:"events"`
	// MinEventsForFactor past events of one kind are needed before the
	// estimated factor replaces the configured one.
	MinEventsForFactor int `json:"minEventsForFactor"`
}

// DecisionModel configures the decision-model strategy.
type DecisionModel struct {
	// URL of the externally hosted decision model. Must be https.
	URL string `json:"url"`
	// Interval between two calls. Between calls the last answer is reused.
	Interval Duration `json:"interval"`
	Timeout  Duration `json:"timeout"`
	Quantile float64  `json:"quantile"`
	// PlausibilityFactor k: an answer more than k times off predictive is
	// rejected.
	PlausibilityFactor float64 `json:"plausibilityFactor"`
	// BreakerFailures in a row open the circuit breaker for BreakerPause.
	BreakerFailures int      `json:"breakerFailures"`
	BreakerPause    Duration `json:"breakerPause"`
	// Levels offered as choices; defaults to the predictive levels.
	Levels []int `json:"levels"`
}

// Quantity is a resource amount in base units (cores, bytes).
type Quantity struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// Budget configures the dynamic budget.
type Budget struct {
	// NodeSelector picks the nodes session pods run on.
	NodeSelector map[string]string `json:"nodeSelector"`
	// PodOverhead is added to every pool pod's requests (the oauth2-proxy sidecar).
	PodOverhead Quantity `json:"podOverhead"`
	// UsageQuery returns the memory, in bytes, still free by actual usage on
	// the session nodes. Empty disables the usage limit.
	UsageQuery string `json:"usageQuery"`
	// UsageHeadroomFraction of that free memory may go to idle pods.
	UsageHeadroomFraction float64 `json:"usageHeadroomFraction"`
}

// Prometheus configures the Prometheus provider.
type Prometheus struct {
	URL string `json:"url"`
	// Timeout per query.
	Timeout Duration `json:"timeout"`
}

// Artemis configures the Artemis provider. The token comes from $ARTEMIS_TOKEN.
type Artemis struct {
	URL      string   `json:"url"`
	Days     int      `json:"days"`
	Interval Duration `json:"interval"`
	Timeout  Duration `json:"timeout"`
}

// Calendar configures the static calendar provider.
type Calendar struct {
	File string `json:"file"`
}

// Switchback configures the daily switchback experiment.
type Switchback struct {
	Enabled bool  `json:"enabled"`
	Seed    int64 `json:"seed"`
	// Strategies taking turns.
	Strategies []string `json:"strategies"`
	// ExamDayStrategy runs on every day with an exam.
	ExamDayStrategy string `json:"examDayStrategy"`
	// Schedule pins a strategy to a date (YYYY-MM-DD), winning over the rest.
	Schedule map[string]string `json:"schedule"`
}

// Default returns the configuration used for every field the file omits.
func Default() Config {
	return Config{
		Mode:           ModeShadow,
		Interval:       Duration{30 * time.Second},
		Timezone:       "UTC",
		MetricsAddr:    ":9090",
		ActiveStrategy: StrategyReactive,
		Strategies:     []string{StrategyReactive},
		Guardrails: Guardrails{
			MaxStepUp:         20,
			MaxStepDown:       5,
			Hysteresis:        1,
			ScaleDownCooldown: Duration{10 * time.Minute},
		},
		Reactive: Reactive{
			Window:              Duration{5 * time.Minute},
			SafetyFactor:        0.2,
			DefaultReadySeconds: 60,
		},
		Predictive: Predictive{
			Alpha:           0.3,
			TrainWeeks:      6,
			RetrainInterval: Duration{24 * time.Hour},
			Quantile:        0.9,
			HorizonBuffer:   Duration{time.Minute},
			Levels:          []int{0, 1, 2, 3, 5, 8, 10, 15, 20, 30, 40, 60, 80, 120, 160},
			Events: map[string]EventProfile{
				"exam":    {Factor: 0.9, Before: Duration{15 * time.Minute}, After: Duration{15 * time.Minute}},
				"release": {Factor: 0.2, After: Duration{2 * time.Hour}},
				"due":     {Factor: 0.2, Before: Duration{6 * time.Hour}},
			},
			MinEventsForFactor: 3,
		},
		DecisionModel: DecisionModel{
			Interval:           Duration{10 * time.Minute},
			Timeout:            Duration{10 * time.Second},
			Quantile:           0.9,
			PlausibilityFactor: 3,
			BreakerFailures:    3,
			BreakerPause:       Duration{15 * time.Minute},
		},
		Budget: Budget{
			PodOverhead:           Quantity{CPU: "100m", Memory: "128Mi"},
			UsageHeadroomFraction: 0.8,
		},
		Prometheus: Prometheus{Timeout: Duration{30 * time.Second}},
		Artemis: Artemis{
			Days:     14,
			Interval: Duration{15 * time.Minute},
			Timeout:  Duration{10 * time.Second},
		},
	}
}

// Load reads a YAML file over the defaults and validates the result.
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		if err := yaml.UnmarshalStrict(b, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	}
	if c.Namespace == "" {
		c.Namespace = os.Getenv("POD_NAMESPACE")
	}
	if len(c.DecisionModel.Levels) == 0 {
		c.DecisionModel.Levels = c.Predictive.Levels
	}
	return c, c.Validate()
}

// Validate rejects configurations the scaler cannot run with.
func (c *Config) Validate() error {
	known := map[string]bool{StrategyStatic: true, StrategyReactive: true, StrategyPredictive: true, StrategyDecisionModel: true}
	if c.Mode != ModeShadow && c.Mode != ModeActive {
		return fmt.Errorf("mode must be shadow or active, got %q", c.Mode)
	}
	if c.Namespace == "" {
		return fmt.Errorf("namespace is empty and $POD_NAMESPACE is unset")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	if c.Interval.Duration <= 0 {
		return fmt.Errorf("interval must be positive")
	}
	for _, s := range append(append([]string{c.ActiveStrategy}, c.Strategies...), c.Switchback.Strategies...) {
		if !known[s] {
			return fmt.Errorf("unknown strategy %q", s)
		}
	}
	for d, s := range c.Switchback.Schedule {
		if !known[s] {
			return fmt.Errorf("switchback.schedule[%s]: unknown strategy %q", d, s)
		}
		if _, err := time.Parse(time.DateOnly, d); err != nil {
			return fmt.Errorf("switchback.schedule: %q is not YYYY-MM-DD", d)
		}
	}
	if c.Switchback.Enabled && len(c.Switchback.Strategies) == 0 {
		return fmt.Errorf("switchback.enabled needs switchback.strategies")
	}
	if c.Switchback.ExamDayStrategy != "" && !known[c.Switchback.ExamDayStrategy] {
		return fmt.Errorf("unknown switchback.examDayStrategy %q", c.Switchback.ExamDayStrategy)
	}
	for _, q := range []float64{c.Predictive.Quantile, c.DecisionModel.Quantile} {
		if q <= 0 || q >= 1 {
			return fmt.Errorf("quantiles must be in (0, 1), got %v", q)
		}
	}
	for _, levels := range [][]int{c.Predictive.Levels, c.DecisionModel.Levels} {
		if len(levels) == 0 {
			return fmt.Errorf("demand levels must not be empty")
		}
		for i := 1; i < len(levels); i++ {
			if levels[i] <= levels[i-1] {
				return fmt.Errorf("demand levels must be strictly ascending")
			}
		}
	}
	if c.uses(StrategyPredictive) || c.uses(StrategyDecisionModel) {
		if c.Prometheus.URL == "" {
			return fmt.Errorf("predictive and decision-model need prometheus.url for training")
		}
	}
	if c.uses(StrategyDecisionModel) && c.DecisionModel.URL == "" {
		return fmt.Errorf("decision-model needs decisionModel.url")
	}
	return nil
}

func (c *Config) uses(name string) bool {
	if c.ActiveStrategy == name {
		return true
	}
	for _, s := range append(append([]string{}, c.Strategies...), c.Switchback.Strategies...) {
		if s == name {
			return true
		}
	}
	return c.Switchback.ExamDayStrategy == name
}
