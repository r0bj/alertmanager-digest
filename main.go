package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"
)

func main() {
	var configPath string
	var historyWindowOverride string
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [--config PATH] [--history-window DURATION]\n\n", os.Args[0])
		fmt.Fprintln(flag.CommandLine.Output(), "Options:")
		fmt.Fprintln(flag.CommandLine.Output(), "  --config PATH                 Path to config YAML file (default \"config.yaml\")")
		fmt.Fprintln(flag.CommandLine.Output(), "  --history-window DURATION     Override history window, for example 24h")
	}
	flag.StringVar(&configPath, "config", "config.yaml", "Path to config YAML file")
	flag.StringVar(&historyWindowOverride, "history-window", "", "Override history window, for example 24h")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := loadConfig(configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if historyWindowOverride != "" {
		window, err := time.ParseDuration(historyWindowOverride)
		if err != nil {
			slog.Error("invalid --history-window", "error", err)
			os.Exit(1)
		}
		cfg.History.Window.Duration = window
	}

	if err := validateConfig(cfg); err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}

	client := newHTTPClient(cfg)

	alertmanagerCtx, cancelAlertmanager := context.WithTimeout(context.Background(), cfg.Timeouts.Alertmanager.Duration)
	alerts, fetchErrors := fetchAllAlerts(alertmanagerCtx, client, cfg)
	cancelAlertmanager()

	for _, err := range fetchErrors {
		slog.Error("failed to fetch alerts", "error", err)
	}

	alerts = deduplicateAlerts(alerts)
	sortAlerts(alerts)

	historyCtx, cancelHistory := context.WithTimeout(context.Background(), cfg.Timeouts.History.Duration)
	historicalAlerts, historyErrors := fetchHistoricalAlerts(historyCtx, client, cfg, time.Now())
	cancelHistory()

	for _, err := range historyErrors {
		slog.Error("failed to fetch historical alerts", "error", err)
	}

	if !shouldSendSlackMessage(cfg, alerts, historicalAlerts, fetchErrors, historyErrors) {
		slog.Info("no active or historical alerts; skipping Slack message")
		return
	}

	payload := buildSlackPayload(cfg, alerts, historicalAlerts, fetchErrors, historyErrors)

	if cfg.DryRun {
		encoded, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Println(string(encoded))
		return
	}

	slackCtx, cancelSlack := context.WithTimeout(context.Background(), cfg.Timeouts.Slack.Duration)
	err = sendSlackMessage(slackCtx, client, cfg.SlackWebhookURL, payload)
	cancelSlack()
	if err != nil {
		slog.Error("failed to send Slack message", "error", err)
		os.Exit(1)
	}

	slog.Info("Slack message sent", "alerts", len(alerts), "historical_alerts", len(historicalAlerts), "fetch_errors", len(fetchErrors), "history_errors", len(historyErrors))
}

func shouldSendSlackMessage(cfg Config, alerts []Alert, historicalAlerts []HistoricalAlert, fetchErrors []error, historyErrors []error) bool {
	return cfg.Slack.SendEmptyMessage ||
		len(alerts) > 0 ||
		len(historicalAlerts) > 0 ||
		len(fetchErrors) > 0 ||
		len(historyErrors) > 0
}
