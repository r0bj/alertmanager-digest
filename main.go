package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	Timeout         Duration         `yaml:"timeout"`
	Alertmanagers   []Alertmanager   `yaml:"alertmanagers"`
	Filters         []string         `yaml:"filters"`
	GroupBy         []string         `yaml:"groupBy"`
	Slack           SlackConfig      `yaml:"slack"`
	HTTP            HTTPClientConfig `yaml:"http"`
}

type Alertmanager struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

type SlackConfig struct {
	SendEmptyMessage bool `yaml:"sendEmptyMessage"`
	MaxAlerts        int  `yaml:"maxAlerts"`
}

type HTTPClientConfig struct {
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
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

type Alert struct {
	Annotations  map[string]string `json:"annotations"`
	Labels       map[string]string `json:"labels"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
	Status       AlertStatus       `json:"status"`

	SourceAlertmanager    string `json:"-"`
	SourceAlertmanagerURL string `json:"-"`
}

type AlertStatus struct {
	State       string   `json:"state"`
	SilencedBy  []string `json:"silencedBy"`
	InhibitedBy []string `json:"inhibitedBy"`
}

type SlackPayload struct {
	Text   string       `json:"text"`
	Blocks []SlackBlock `json:"blocks,omitempty"`
}

type SlackBlock struct {
	Type string     `json:"type"`
	Text *SlackText `json:"text,omitempty"`
}

type SlackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func main() {
	var configPath string
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [--config PATH]\n\n", os.Args[0])
		fmt.Fprintln(flag.CommandLine.Output(), "Options:")
		fmt.Fprintln(flag.CommandLine.Output(), "  --config PATH  Path to config YAML file (default \"config.yaml\")")
	}
	flag.StringVar(&configPath, "config", "config.yaml", "Path to config YAML file")
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

	if len(alerts) == 0 && !cfg.Slack.SendEmptyMessage {
		slog.Info("no active unsilenced and uninhibited alerts; skipping Slack message")
		return
	}

	payload := buildSlackPayload(cfg, alerts, fetchErrors)

	if cfg.DryRun {
		encoded, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Println(string(encoded))
		return
	}

	if err := sendSlackMessage(ctx, client, cfg.SlackWebhookURL, payload); err != nil {
		slog.Error("failed to send Slack message", "error", err)
		os.Exit(1)
	}

	slog.Info("Slack message sent", "alerts", len(alerts), "fetch_errors", len(fetchErrors))
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
	if cfg.Timeout.Duration == 0 {
		cfg.Timeout.Duration = 10 * time.Second
	}
	if cfg.Slack.MaxAlerts == 0 {
		cfg.Slack.MaxAlerts = 40
	}
	if len(cfg.GroupBy) == 0 {
		cfg.GroupBy = []string{"severity", "alertname", "cluster", "namespace"}
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

func newHTTPClient(cfg Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	if cfg.HTTP.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec
		}
	}

	return &http.Client{
		Timeout:   cfg.Timeout.Duration,
		Transport: transport,
	}
}

func fetchAllAlerts(ctx context.Context, client *http.Client, cfg Config) ([]Alert, []error) {
	var allAlerts []Alert
	var errs []error

	for _, am := range cfg.Alertmanagers {
		alerts, err := fetchAlerts(ctx, client, am, cfg.Filters)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		allAlerts = append(allAlerts, alerts...)
	}

	return allAlerts, errs
}

func fetchAlerts(ctx context.Context, client *http.Client, am Alertmanager, filters []string) ([]Alert, error) {
	endpoint, err := url.JoinPath(strings.TrimRight(am.URL, "/"), "/api/v2/alerts")
	if err != nil {
		return nil, fmt.Errorf("alertmanager %q: invalid URL: %w", am.Name, err)
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("alertmanager %q: invalid endpoint: %w", am.Name, err)
	}

	query := u.Query()
	query.Set("active", "true")
	query.Set("silenced", "false")
	query.Set("inhibited", "false")

	for _, filter := range filters {
		query.Add("filter", filter)
	}

	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("alertmanager %q: create request: %w", am.Name, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alertmanager %q: request failed: %w", am.Name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("alertmanager %q: read response: %w", am.Name, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("alertmanager %q: status=%d body=%s", am.Name, resp.StatusCode, string(body))
	}

	var alerts []Alert
	if err := json.Unmarshal(body, &alerts); err != nil {
		return nil, fmt.Errorf("alertmanager %q: decode response: %w", am.Name, err)
	}

	for i := range alerts {
		alerts[i].SourceAlertmanager = am.Name
		alerts[i].SourceAlertmanagerURL = strings.TrimRight(am.URL, "/")
	}

	slog.Info("fetched alerts", "alertmanager", am.Name, "count", len(alerts))

	return alerts, nil
}

func deduplicateAlerts(alerts []Alert) []Alert {
	seen := map[string]Alert{}

	for _, alert := range alerts {
		key := alert.Fingerprint
		if key == "" {
			key = fallbackFingerprint(alert)
		}

		existing, exists := seen[key]
		if !exists {
			seen[key] = alert
			continue
		}

		existing.SourceAlertmanager = mergeSource(existing.SourceAlertmanager, alert.SourceAlertmanager)
		existing.SourceAlertmanagerURL = mergeSource(existing.SourceAlertmanagerURL, alert.SourceAlertmanagerURL)
		seen[key] = existing
	}

	result := make([]Alert, 0, len(seen))
	for _, alert := range seen {
		result = append(result, alert)
	}

	return result
}

func fallbackFingerprint(alert Alert) string {
	keys := make([]string, 0, len(alert.Labels))
	for key := range alert.Labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(alert.Labels[key])
		b.WriteString(",")
	}

	return b.String()
}

func mergeSource(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || strings.Contains(","+a+",", ","+b+",") {
		return a
	}
	return a + "," + b
}

func sortAlerts(alerts []Alert) {
	severityRank := map[string]int{
		"critical": 0,
		"warning":  1,
		"info":     2,
	}

	sort.SliceStable(alerts, func(i, j int) bool {
		ai := alerts[i]
		aj := alerts[j]

		si := severityRank[strings.ToLower(ai.Labels["severity"])]
		sj := severityRank[strings.ToLower(aj.Labels["severity"])]

		if si != sj {
			return si < sj
		}

		if ai.Labels["alertname"] != aj.Labels["alertname"] {
			return ai.Labels["alertname"] < aj.Labels["alertname"]
		}

		if ai.Labels["cluster"] != aj.Labels["cluster"] {
			return ai.Labels["cluster"] < aj.Labels["cluster"]
		}

		return ai.StartsAt.Before(aj.StartsAt)
	})
}

func buildSlackPayload(cfg Config, alerts []Alert, fetchErrors []error) SlackPayload {
	now := time.Now().Format("02 Jan 2006, 15:04 MST")

	headerText := fmt.Sprintf("*%s*\nGenerated at: %s", escapeSlack(cfg.Title), now)

	if len(alerts) == 0 {
		text := ":white_check_mark: No active unsilenced and uninhibited alerts."

		blocks := []SlackBlock{
			mrkdwnBlock(headerText),
			mrkdwnBlock(text),
		}

		blocks = appendErrorBlocks(blocks, fetchErrors)

		return SlackPayload{
			Text:   text,
			Blocks: blocks,
		}
	}

	maxAlerts := cfg.Slack.MaxAlerts
	if maxAlerts <= 0 {
		maxAlerts = 40
	}

	visibleAlerts := alerts
	truncated := 0
	if len(alerts) > maxAlerts {
		visibleAlerts = alerts[:maxAlerts]
		truncated = len(alerts) - maxAlerts
	}

	summary := fmt.Sprintf("*Active alerts: %d*", len(alerts))
	if truncated > 0 {
		summary += fmt.Sprintf("\nShowing first `%d`, truncated `%d`.", len(visibleAlerts), truncated)
	}

	blocks := []SlackBlock{
		mrkdwnBlock(headerText),
		mrkdwnBlock(summary),
	}

	for _, alert := range visibleAlerts {
		blocks = append(blocks, mrkdwnBlock(formatAlert(alert, cfg.GroupBy)))
	}

	blocks = appendErrorBlocks(blocks, fetchErrors)

	return SlackPayload{
		Text:   fmt.Sprintf("Active unsilenced and uninhibited alerts: %d", len(alerts)),
		Blocks: blocks,
	}
}

func appendErrorBlocks(blocks []SlackBlock, errs []error) []SlackBlock {
	if len(errs) == 0 {
		return blocks
	}

	lines := []string{":warning: *Some Alertmanagers could not be queried:*"}
	for _, err := range errs {
		lines = append(lines, "• `"+escapeSlack(err.Error())+"`")
	}

	return append(blocks, mrkdwnBlock(strings.Join(lines, "\n")))
}

func formatAlert(alert Alert, groupBy []string) string {
	alertname := value(alert.Labels, "alertname", "unknown")
	severity := value(alert.Labels, "severity", "unknown")

	var parts []string
	for _, label := range groupBy {
		if label == "alertname" || label == "severity" {
			continue
		}

		if val := alert.Labels[label]; val != "" {
			parts = append(parts, fmt.Sprintf("%s=%s", label, escapeSlack(val)))
		}
	}

	labelFields := append([]string{fmt.Sprintf("severity=%s", escapeSlack(severity))}, parts...)
	activeSince := fmt.Sprintf("active for %s", humanDurationSince(alert.StartsAt))

	line := fmt.Sprintf(
		"• *%s* (%s) %s",
		escapeSlack(alertname),
		strings.Join(labelFields, ", "),
		activeSince,
	)

	links := alertLinks(alert)
	if len(links) > 0 {
		line += "\n  " + strings.Join(links, " | ")
	}

	return line
}

func alertLinks(alert Alert) []string {
	var links []string
	if alert.GeneratorURL != "" {
		links = append(links, fmt.Sprintf("<%s|Prometheus>", alert.GeneratorURL))
	}

	alertmanagerURLs := splitSourceList(alert.SourceAlertmanagerURL)
	alertmanagerNames := splitSourceList(alert.SourceAlertmanager)
	for i, alertmanagerURL := range alertmanagerURLs {
		label := "Alertmanager"
		if len(alertmanagerURLs) > 1 {
			if i < len(alertmanagerNames) && alertmanagerNames[i] != "" {
				label = "Alertmanager " + escapeSlack(alertmanagerNames[i])
			} else {
				label = fmt.Sprintf("Alertmanager %d", i+1)
			}
		}

		links = append(links, fmt.Sprintf("<%s|%s>", alertmanagerURL, label))
	}

	return links
}

func splitSourceList(source string) []string {
	if source == "" {
		return nil
	}

	return strings.Split(source, ",")
}

func humanDurationSince(t time.Time) string {
	seconds := int(time.Since(t).Round(time.Second).Seconds())
	if seconds < 0 {
		seconds = 0
	}

	days := seconds / int((24 * time.Hour).Seconds())
	seconds %= int((24 * time.Hour).Seconds())
	hours := seconds / int(time.Hour.Seconds())
	seconds %= int(time.Hour.Seconds())
	minutes := seconds / int(time.Minute.Seconds())
	seconds %= int(time.Minute.Seconds())

	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 || len(parts) > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 || len(parts) > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	parts = append(parts, fmt.Sprintf("%ds", seconds))

	return strings.Join(parts, " ")
}

func value(m map[string]string, key string, fallback string) string {
	if v := m[key]; v != "" {
		return v
	}
	return fallback
}

func mrkdwnBlock(text string) SlackBlock {
	const maxSlackTextLen = 3000

	if len(text) > maxSlackTextLen {
		text = text[:maxSlackTextLen-20] + "\n…truncated…"
	}

	return SlackBlock{
		Type: "section",
		Text: &SlackText{
			Type: "mrkdwn",
			Text: text,
		},
	}
}

func escapeSlack(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func sendSlackMessage(ctx context.Context, client *http.Client, webhookURL string, payload SlackPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal Slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Slack request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send Slack request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Slack webhook returned status=%d body=%s", resp.StatusCode, string(respBody))
	}

	return nil
}
