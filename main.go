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

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout.Duration)
	defer cancel()

	alerts, fetchErrors := fetchAllAlerts(ctx, client, cfg)

	for _, err := range fetchErrors {
		slog.Error("failed to fetch alerts", "error", err)
	}

	alerts = deduplicateAlerts(alerts)
	sortAlerts(alerts)

	historicalAlerts, historyErrors := fetchHistoricalAlerts(ctx, client, cfg, time.Now())

	for _, err := range historyErrors {
		slog.Error("failed to fetch historical alerts", "error", err)
	}

	if len(alerts) == 0 && len(historicalAlerts) == 0 && len(fetchErrors) == 0 && len(historyErrors) == 0 && !cfg.Slack.SendEmptyMessage {
		slog.Info("no active or historical alerts; skipping Slack message")
		return
	}

	payload := buildSlackPayload(cfg, alerts, historicalAlerts, fetchErrors, historyErrors)

	if cfg.DryRun {
		encoded, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Println(string(encoded))
		return
	}

	if err := sendSlackMessage(ctx, client, cfg.SlackWebhookURL, payload); err != nil {
		slog.Error("failed to send Slack message", "error", err)
		os.Exit(1)
	}

	slog.Info("Slack message sent", "alerts", len(alerts), "historical_alerts", len(historicalAlerts), "fetch_errors", len(fetchErrors), "history_errors", len(historyErrors))
}
