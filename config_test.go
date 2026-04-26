package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadConfigUsesDisplayLabels(t *testing.T) {
	configPath := writeTestConfig(t, `
dryRun: true
alertmanagers:
- name: prod
  url: https://alertmanager.example.com
displayLabels:
- severity
- team
`)

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	want := []string{"severity", "team"}
	if !reflect.DeepEqual(cfg.DisplayLabels, want) {
		t.Fatalf("expected displayLabels %#v, got %#v", want, cfg.DisplayLabels)
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

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}
