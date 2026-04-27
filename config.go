package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SlackWebhookURL string           `yaml:"slackWebhookUrl"`
	Title           string           `yaml:"title"`
	DryRun          bool             `yaml:"dryRun"`
	Timeouts        TimeoutsConfig   `yaml:"timeouts"`
	Alertmanagers   []Alertmanager   `yaml:"alertmanagers"`
	Filters         []string         `yaml:"filters"`
	GroupBy         []string         `yaml:"groupBy"`
	ExcludeLabels   []string         `yaml:"excludeLabels"`
	Slack           SlackConfig      `yaml:"slack"`
	HTTP            HTTPClientConfig `yaml:"http"`
	History         HistoryConfig    `yaml:"history"`
}

type TimeoutsConfig struct {
	Alertmanager Duration `yaml:"alertmanager"`
	History      Duration `yaml:"history"`
	Slack        Duration `yaml:"slack"`
}

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}

	d.Duration = parsed
	return nil
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}

	if cfg.Title == "" {
		cfg.Title = "Alertmanager daily digest"
	}
	if cfg.Timeouts.Alertmanager.Duration == 0 {
		cfg.Timeouts.Alertmanager.Duration = 10 * time.Second
	}
	if cfg.Timeouts.History.Duration == 0 {
		cfg.Timeouts.History.Duration = 30 * time.Second
	}
	if cfg.Timeouts.Slack.Duration == 0 {
		cfg.Timeouts.Slack.Duration = 10 * time.Second
	}
	if cfg.Slack.MaxAlerts == 0 {
		cfg.Slack.MaxAlerts = 40
	}
	if cfg.History.Window.Duration == 0 {
		cfg.History.Window.Duration = 24 * time.Hour
	}
	if cfg.History.MaxAlerts == 0 {
		cfg.History.MaxAlerts = cfg.Slack.MaxAlerts
	}
	if cfg.History.PageSize == 0 {
		cfg.History.PageSize = 1000
	}
	if len(cfg.History.ProjectIDs) == 0 {
		if projectID := os.Getenv("GOOGLE_CLOUD_PROJECT"); projectID != "" {
			cfg.History.ProjectIDs = []string{projectID}
		} else if projectID := os.Getenv("GCLOUD_PROJECT"); projectID != "" {
			cfg.History.ProjectIDs = []string{projectID}
		}
	}

	if envWebhook := os.Getenv("SLACK_WEBHOOK_URL"); envWebhook != "" {
		cfg.SlackWebhookURL = envWebhook
	}

	return cfg, nil
}

func validateConfig(cfg Config) error {
	if cfg.SlackWebhookURL == "" && !cfg.DryRun {
		return errors.New("slackWebhookUrl is required unless dryRun=true; alternatively set SLACK_WEBHOOK_URL env var")
	}

	if len(cfg.Alertmanagers) == 0 {
		return errors.New("at least one alertmanager is required")
	}

	if cfg.Timeouts.Alertmanager.Duration <= 0 {
		return errors.New("timeouts.alertmanager must be greater than 0")
	}
	if cfg.Timeouts.History.Duration <= 0 {
		return errors.New("timeouts.history must be greater than 0")
	}
	if cfg.Timeouts.Slack.Duration <= 0 {
		return errors.New("timeouts.slack must be greater than 0")
	}
	if containsLabel(cfg.GroupBy, "*") {
		return errors.New(`groupBy no longer supports "*"; omit groupBy to group by all labels`)
	}
	if conflicts := groupByExcludeLabelConflicts(cfg.GroupBy, cfg.ExcludeLabels); len(conflicts) > 0 {
		return fmt.Errorf("excludeLabels cannot contain labels from groupBy: %s", strings.Join(conflicts, ", "))
	}

	if cfg.History.Enabled {
		if cfg.History.Window.Duration <= 0 {
			return errors.New("history.window must be greater than 0")
		}
		if len(cfg.History.ProjectIDs) == 0 {
			return errors.New("history.projectIds is required when history.enabled=true; alternatively set GOOGLE_CLOUD_PROJECT env var")
		}
		if cfg.History.PageSize < 0 {
			return errors.New("history.pageSize cannot be negative")
		}
	}

	for _, am := range cfg.Alertmanagers {
		if am.Name == "" {
			return errors.New("alertmanager name is required")
		}
		if am.URL == "" {
			return fmt.Errorf("alertmanager %q url is required", am.Name)
		}
		if _, err := url.ParseRequestURI(am.URL); err != nil {
			return fmt.Errorf("invalid alertmanager url for %q: %w", am.Name, err)
		}
	}

	return nil
}

func groupByExcludeLabelConflicts(groupBy []string, excludeLabels []string) []string {
	if len(groupBy) == 0 {
		return nil
	}

	grouped := make(map[string]bool, len(groupBy))
	for _, label := range groupBy {
		if label != "" {
			grouped[label] = true
		}
	}

	conflicts := make([]string, 0)
	seen := map[string]bool{}
	for _, label := range excludeLabels {
		if label == "" || !grouped[label] || seen[label] {
			continue
		}
		conflicts = append(conflicts, label)
		seen[label] = true
	}
	sort.Strings(conflicts)

	return conflicts
}

func containsLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}

	return false
}
