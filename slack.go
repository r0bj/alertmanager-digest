package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type SlackConfig struct {
	SendEmptyMessage bool `yaml:"sendEmptyMessage"`
	MaxAlerts        int  `yaml:"maxAlerts"`
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

func buildSlackPayload(cfg Config, alerts []Alert, historicalAlerts []HistoricalAlert, fetchErrors []error, historyErrors []error) SlackPayload {
	now := time.Now().Format("02 Jan 2006, 15:04 MST")

	headerText := fmt.Sprintf("*%s*\nGenerated at: %s", escapeSlack(cfg.Title), now)

	blocks := []SlackBlock{
		mrkdwnBlock(headerText),
	}

	if len(alerts) == 0 {
		blocks = append(blocks, mrkdwnBlock(":white_check_mark: No active unsilenced and uninhibited alerts."))
	} else {
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

		totalActiveAlerts := countActiveAlerts(alerts)
		summary := fmt.Sprintf("*Active alerts: %d*", totalActiveAlerts)
		if totalActiveAlerts != len(alerts) {
			summary = fmt.Sprintf("*Active alerts: %d groups, %d alerts*", len(alerts), totalActiveAlerts)
		}
		if truncated > 0 {
			summary += fmt.Sprintf("\nShowing first %d, truncated %d.", len(visibleAlerts), truncated)
		}

		blocks = append(blocks, mrkdwnBlock(summary))

		for _, alert := range visibleAlerts {
			blocks = append(blocks, mrkdwnBlock(formatAlert(alert, cfg.GroupBy, cfg.ExcludeLabels)))
		}
	}

	blocks = appendErrorBlocks(blocks, fetchErrors)

	if cfg.History.Enabled {
		blocks = append(blocks, dividerBlock())
		blocks = appendHistoryBlocks(blocks, cfg, historicalAlerts)
		blocks = appendHistoryErrorBlocks(blocks, historyErrors)
	}

	return SlackPayload{
		Text:   slackFallbackText(cfg, alerts, historicalAlerts),
		Blocks: blocks,
	}
}

func appendHistoryBlocks(blocks []SlackBlock, cfg Config, historicalAlerts []HistoricalAlert) []SlackBlock {
	historyWindow := compactDuration(cfg.History.Window.Duration)

	if len(historicalAlerts) == 0 {
		return append(blocks, mrkdwnBlock(fmt.Sprintf(":white_check_mark: No alerts were sent in the last %s.", historyWindow)))
	}

	maxAlerts := cfg.History.MaxAlerts
	if maxAlerts <= 0 {
		maxAlerts = 40
	}

	visibleAlerts := historicalAlerts
	truncated := 0
	if len(historicalAlerts) > maxAlerts {
		visibleAlerts = historicalAlerts[:maxAlerts]
		truncated = len(historicalAlerts) - maxAlerts
	}

	totalOccurrences := countHistoricalOccurrences(historicalAlerts)
	totalNotifications := countHistoricalNotifications(historicalAlerts)
	summary := fmt.Sprintf(
		"*Alerts sent in the last %s: %d groups, %d occurrences, %d notifications*",
		historyWindow,
		len(historicalAlerts),
		totalOccurrences,
		totalNotifications,
	)
	if truncated > 0 {
		summary += fmt.Sprintf("\nShowing first %d, truncated %d.", len(visibleAlerts), truncated)
	}

	blocks = append(blocks, mrkdwnBlock(summary))
	for _, alert := range visibleAlerts {
		blocks = append(blocks, mrkdwnBlock(formatHistoricalAlert(alert, cfg.GroupBy, cfg.ExcludeLabels)))
	}

	return blocks
}

func slackFallbackText(cfg Config, alerts []Alert, historicalAlerts []HistoricalAlert) string {
	if !cfg.History.Enabled {
		return fmt.Sprintf("Active unsilenced and uninhibited alerts: %d", countActiveAlerts(alerts))
	}

	return fmt.Sprintf(
		"Active unsilenced and uninhibited alerts: %d. Alerts sent in the last %s: %d occurrences, %d notifications.",
		countActiveAlerts(alerts),
		compactDuration(cfg.History.Window.Duration),
		countHistoricalOccurrences(historicalAlerts),
		countHistoricalNotifications(historicalAlerts),
	)
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

func appendHistoryErrorBlocks(blocks []SlackBlock, errs []error) []SlackBlock {
	if len(errs) == 0 {
		return blocks
	}

	lines := []string{":warning: *Historical alerts could not be fully queried:*"}
	for _, err := range errs {
		lines = append(lines, "• `"+escapeSlack(err.Error())+"`")
	}

	return append(blocks, mrkdwnBlock(strings.Join(lines, "\n")))
}

func formatAlert(alert Alert, groupBy []string, excludeLabels []string) string {
	alertname := value(alert.Labels, "alertname", "unknown")
	labelFields := formatLabelFields(alert.Labels, groupBy, excludeLabels)
	activeSince := fmt.Sprintf("active for %s", humanDurationSince(alert.StartsAt))
	if alert.Count > 1 {
		activeSince = fmt.Sprintf("%d alerts, %s", alert.Count, activeSince)
	}

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

func countActiveAlerts(alerts []Alert) int {
	total := 0
	for _, alert := range alerts {
		if alert.Count > 0 {
			total += alert.Count
			continue
		}
		total++
	}
	return total
}

func formatHistoricalAlert(alert HistoricalAlert, groupBy []string, excludeLabels []string) string {
	alertname := value(alert.Labels, "alertname", "unknown")
	labelFields := formatLabelFields(alert.Labels, groupBy, excludeLabels)
	line := fmt.Sprintf(
		"• *%s* (%s) occurrences: %d, notifications: %d, last notified %s ago",
		escapeSlack(alertname),
		strings.Join(labelFields, ", "),
		alert.Count,
		alert.Notifications,
		humanDurationSince(alert.LastSeen),
	)

	var links []string
	if alert.GeneratorURL != "" {
		links = append(links, fmt.Sprintf("<%s|Prometheus>", alert.GeneratorURL))
	}
	if alert.LogsURL != "" {
		links = append(links, fmt.Sprintf("<%s|GCP Logs>", alert.LogsURL))
	}
	if len(links) > 0 {
		line += "\n  " + strings.Join(links, " | ")
	}

	return line
}

func formatLabelFields(labels map[string]string, groupBy []string, excludeLabels []string) []string {
	excluded := excludedLabelSet(excludeLabels)
	excluded["alertname"] = true

	if len(groupBy) == 0 {
		return formatAllLabelFields(labels, excluded)
	}

	var fields []string
	for _, label := range groupBy {
		if excluded[label] {
			continue
		}

		if val := labels[label]; val != "" {
			fields = append(fields, fmt.Sprintf("%s=%s", label, escapeSlack(val)))
		}
	}

	return fields
}

func excludedLabelSet(excludeLabels []string) map[string]bool {
	excluded := make(map[string]bool, len(excludeLabels)+1)
	for _, label := range excludeLabels {
		if label != "" {
			excluded[label] = true
		}
	}

	return excluded
}

func formatAllLabelFields(labels map[string]string, excluded map[string]bool) []string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		if excluded[key] {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	fields := make([]string, 0, len(keys))
	for _, key := range keys {
		if val := labels[key]; val != "" {
			fields = append(fields, fmt.Sprintf("%s=%s", key, escapeSlack(val)))
		}
	}

	return fields
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

func compactDuration(d time.Duration) string {
	seconds := int(d.Round(time.Second).Seconds())
	if seconds < 0 {
		seconds = 0
	}

	hours := seconds / int(time.Hour.Seconds())
	seconds %= int(time.Hour.Seconds())
	minutes := seconds / int(time.Minute.Seconds())
	seconds %= int(time.Minute.Seconds())

	var parts []string
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}

	return strings.Join(parts, "")
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
		text = truncateSlackText(text, maxSlackTextLen)
	}

	return SlackBlock{
		Type: "section",
		Text: &SlackText{
			Type: "mrkdwn",
			Text: text,
		},
	}
}

func truncateSlackText(text string, maxLen int) string {
	const suffix = "\n…truncated…"

	if len(text) <= maxLen {
		return text
	}

	text = text[:maxLen-len(suffix)]
	if linkStart := strings.LastIndex(text, "<"); linkStart > strings.LastIndex(text, ">") {
		text = strings.TrimRight(text[:linkStart], " \n|")
	}

	return text + suffix
}

func dividerBlock() SlackBlock {
	return SlackBlock{
		Type: "divider",
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
