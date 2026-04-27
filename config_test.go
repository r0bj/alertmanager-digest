package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigUsesGroupBy(t *testing.T) {
	configPath := writeTestConfig(t, `
dryRun: true
alertmanagers:
- name: prod
  url: https://alertmanager.example.com
groupBy:
- severity
- team
`)

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	want := []string{"severity", "team"}
	if !reflect.DeepEqual(cfg.GroupBy, want) {
		t.Fatalf("expected groupBy %#v, got %#v", want, cfg.GroupBy)
	}
}

func TestLoadConfigLeavesGroupByEmptyWhenOmitted(t *testing.T) {
	configPath := writeTestConfig(t, `
dryRun: true
alertmanagers:
- name: prod
  url: https://alertmanager.example.com
`)

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if len(cfg.GroupBy) != 0 {
		t.Fatalf("expected omitted groupBy to stay empty, got %#v", cfg.GroupBy)
	}
}

func TestLoadConfigDefaultsStageTimeouts(t *testing.T) {
	configPath := writeTestConfig(t, `
dryRun: true
alertmanagers:
- name: prod
  url: https://alertmanager.example.com
`)

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Timeouts.Alertmanager.Duration != 10*time.Second {
		t.Fatalf("expected alertmanager timeout 10s, got %s", cfg.Timeouts.Alertmanager.Duration)
	}
	if cfg.Timeouts.History.Duration != 30*time.Second {
		t.Fatalf("expected history timeout 30s, got %s", cfg.Timeouts.History.Duration)
	}
	if cfg.Timeouts.Slack.Duration != 10*time.Second {
		t.Fatalf("expected slack timeout 10s, got %s", cfg.Timeouts.Slack.Duration)
	}
}

func TestLoadConfigSupportsStageTimeoutOverrides(t *testing.T) {
	configPath := writeTestConfig(t, `
dryRun: true
timeouts:
  alertmanager: 5s
  history: 30s
  slack: 7s
alertmanagers:
- name: prod
  url: https://alertmanager.example.com
`)

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Timeouts.Alertmanager.Duration != 5*time.Second {
		t.Fatalf("expected alertmanager timeout 5s, got %s", cfg.Timeouts.Alertmanager.Duration)
	}
	if cfg.Timeouts.History.Duration != 30*time.Second {
		t.Fatalf("expected history timeout 30s, got %s", cfg.Timeouts.History.Duration)
	}
	if cfg.Timeouts.Slack.Duration != 7*time.Second {
		t.Fatalf("expected slack timeout 7s, got %s", cfg.Timeouts.Slack.Duration)
	}
}

func TestValidateConfigRejectsExcludedGroupByLabels(t *testing.T) {
	cfg := validTestConfig()
	cfg.GroupBy = []string{"severity", "alertname", "team"}
	cfg.ExcludeLabels = []string{"team"}

	err := validateConfig(cfg)

	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "excludeLabels cannot contain labels from groupBy: team") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidateConfigAllowsExcludeLabelsWhenGroupByIsOmitted(t *testing.T) {
	cfg := validTestConfig()
	cfg.GroupBy = nil
	cfg.ExcludeLabels = []string{"team"}

	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validate config: %v", err)
	}
}

func TestValidateConfigRejectsStarGroupBy(t *testing.T) {
	cfg := validTestConfig()
	cfg.GroupBy = []string{"*"}

	err := validateConfig(cfg)

	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), `groupBy no longer supports "*"`) {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func validTestConfig() Config {
	return Config{
		DryRun: true,
		Timeouts: TimeoutsConfig{
			Alertmanager: Duration{Duration: 10 * time.Second},
			History:      Duration{Duration: 30 * time.Second},
			Slack:        Duration{Duration: 10 * time.Second},
		},
		Alertmanagers: []Alertmanager{
			{
				Name: "prod",
				URL:  "https://alertmanager.example.com",
			},
		},
		GroupBy: []string{"severity", "alertname", "cluster", "namespace"},
	}
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}
