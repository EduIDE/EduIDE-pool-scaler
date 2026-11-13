package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	c, err := Load(write(t, `
namespace: eduide-test
mode: active
interval: 1m
timezone: Europe/Berlin
activeStrategy: predictive
prometheus: {url: http://prometheus:9090}
defaults: {floor: 1}
appLimits:
  java-17: {floor: 3, ceiling: 40}
switchback:
  enabled: true
  strategies: [static, reactive, predictive]
  schedule: {"2026-11-20": predictive}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval.Duration != time.Minute || c.Mode != ModeActive {
		t.Fatalf("%+v", c)
	}
	if f, ce, s := c.Limits("java-17"); f != 3 || ce != 40 || s != 3 {
		t.Fatalf("java-17 limits %d %d %d", f, ce, s)
	}
	if f, ce, _ := c.Limits("c"); f != 1 || ce != 100 {
		t.Fatalf("defaults %d %d", f, ce)
	}
	if len(c.DecisionModel.Levels) == 0 {
		t.Fatal("decision model levels default to the predictive ones")
	}
}

func TestLoadRejects(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "ns")
	for name, s := range map[string]string{
		"unknown field":      "modee: active",
		"bad mode":           "mode: live",
		"unknown strategy":   "activeStrategy: magic",
		"predictive no prom": "activeStrategy: predictive",
		"dm without url":     "strategies: [decision-model]\nprometheus: {url: http://p}",
		"bad quantile":       "predictive: {quantile: 1}",
		"bad levels":         "predictive: {levels: [0, 5, 5]}",
		"bad date":           "switchback: {schedule: {tomorrow: reactive}}",
		"switchback empty":   "switchback: {enabled: true}",
		"bad duration":       "interval: 5",
		"bad timezone":       "timezone: Mars/Olympus",
	} {
		if _, err := Load(write(t, s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if c, err := Load(""); err != nil || c.Namespace != "ns" {
		t.Fatalf("defaults with $POD_NAMESPACE: %v", err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	if _, err := Load("../../examples/config.yaml"); err != nil {
		t.Fatal(err)
	}
}
