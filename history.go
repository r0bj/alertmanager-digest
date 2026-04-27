package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2/google"
)

const defaultHistoryProjectConcurrency = 4

type HistoryConfig struct {
	Enabled    bool     `yaml:"enabled"`
	Window     Duration `yaml:"window"`
	ProjectIDs []string `yaml:"projectIds"`
	Filter     string   `yaml:"filter"`
	MaxAlerts  int      `yaml:"maxAlerts"`
	PageSize   int      `yaml:"pageSize"`
}

type HistoricalAlert struct {
	Labels       map[string]string
	GroupLabels  map[string]string
	Fingerprint  string
	Count        int
	FirstSeen    time.Time
	LastSeen     time.Time
	GeneratorURL string
	LogsURL      string
	LogName      string
}

type cloudLoggingListRequest struct {
	ResourceNames []string `json:"resourceNames"`
	Filter        string   `json:"filter"`
	OrderBy       string   `json:"orderBy,omitempty"`
	PageSize      int      `json:"pageSize,omitempty"`
	PageToken     string   `json:"pageToken,omitempty"`
}

type cloudLoggingListResponse struct {
	Entries       []cloudLogEntry `json:"entries"`
	NextPageToken string          `json:"nextPageToken"`
}

type cloudLogEntry struct {
	InsertID    string               `json:"insertId"`
	LogName     string               `json:"logName"`
	Timestamp   time.Time            `json:"timestamp"`
	JSONPayload webhookLoggerPayload `json:"jsonPayload"`
}

type webhookLoggerPayload struct {
	Alerts webhookPayload `json:"alerts"`
}

type webhookPayload struct {
	Status       string            `json:"status"`
	GroupLabels  map[string]string `json:"groupLabels"`
	CommonLabels map[string]string `json:"commonLabels"`
	Alerts       []webhookAlert    `json:"alerts"`
}

type webhookAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

func fetchHistoricalAlerts(ctx context.Context, client *http.Client, cfg Config, now time.Time) ([]HistoricalAlert, []error) {
	if !cfg.History.Enabled {
		return nil, nil
	}

	labelMatchers, err := parseLabelMatchers(cfg.Filters)
	if err != nil {
		return nil, []error{fmt.Errorf("history filters: %w", err)}
	}

	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/logging.read")
	if err != nil {
		return nil, []error{fmt.Errorf("cloud logging credentials: %w", err)}
	}

	token, err := creds.TokenSource.Token()
	if err != nil {
		return nil, []error{fmt.Errorf("cloud logging token: %w", err)}
	}

	start := now.Add(-cfg.History.Window.Duration)
	filter := buildHistoryFilter(cfg.History.Filter, start, now)

	entries, errs := fetchHistoricalAlertEntriesForProjects(ctx, client, token.AccessToken, cfg, filter, labelMatchers)

	alerts := aggregateHistoricalAlerts(entries, labelMatchers, cfg.GroupBy)
	sortHistoricalAlerts(alerts)

	return alerts, errs
}

func fetchHistoricalAlertEntriesForProjects(ctx context.Context, client *http.Client, accessToken string, cfg Config, filter string, labelMatchers []labelMatcher) ([]cloudLogEntry, []error) {
	type projectResult struct {
		entries []cloudLogEntry
		err     error
	}

	results := make([]projectResult, len(cfg.History.ProjectIDs))
	concurrency := min(defaultHistoryProjectConcurrency, len(cfg.History.ProjectIDs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, projectID := range cfg.History.ProjectIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i].err = ctx.Err()
				return
			}

			projectEntries, err := fetchHistoricalAlertEntriesForProject(ctx, client, accessToken, cfg, projectID, filter)
			results[i] = projectResult{
				entries: projectEntries,
				err:     err,
			}
		}()
	}

	wg.Wait()

	var entries []cloudLogEntry
	var errs []error
	for i, projectID := range cfg.History.ProjectIDs {
		projectEntries := results[i].entries
		err := results[i].err
		projectAlerts := aggregateHistoricalAlerts(projectEntries, labelMatchers, cfg.GroupBy)
		sortHistoricalAlerts(projectAlerts)

		if err != nil {
			errs = append(errs, fmt.Errorf("project %q: %w", projectID, err))
			logFetchedHistoricalAlerts(projectID, projectAlerts, cfg, err)
		} else {
			logFetchedHistoricalAlerts(projectID, projectAlerts, cfg, nil)
		}

		entries = append(entries, projectEntries...)
	}

	return entries, errs
}

func fetchHistoricalAlertEntriesForProject(ctx context.Context, client *http.Client, accessToken string, cfg Config, projectID string, filter string) ([]cloudLogEntry, error) {
	requestBody := cloudLoggingListRequest{
		ResourceNames: []string{cloudLoggingResourceName(projectID)},
		Filter:        filter,
		OrderBy:       "timestamp desc",
		PageSize:      cfg.History.PageSize,
	}

	var entries []cloudLogEntry
	for {
		resp, err := fetchCloudLoggingPage(ctx, client, accessToken, requestBody)
		if err != nil {
			return entries, err
		}

		entries = append(entries, resp.Entries...)
		if resp.NextPageToken == "" {
			break
		}

		requestBody.PageToken = resp.NextPageToken
	}

	return entries, nil
}

func logFetchedHistoricalAlerts(projectID string, alerts []HistoricalAlert, cfg Config, err error) {
	args := []any{
		"project_id", projectID,
		"count", len(alerts),
		"occurrences", countHistoricalOccurrences(alerts),
		"window", cfg.History.Window.Duration,
	}
	if err != nil {
		args = append(args, "error", err)
		slog.Warn("failed to fetch historical alerts", args...)
		return
	}

	slog.Info("fetched historical alerts", args...)
}

func fetchCloudLoggingPage(ctx context.Context, client *http.Client, accessToken string, requestBody cloudLoggingListRequest) (cloudLoggingListResponse, error) {
	body, err := json.Marshal(requestBody)
	if err != nil {
		return cloudLoggingListResponse{}, fmt.Errorf("cloud logging marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://logging.googleapis.com/v2/entries:list", bytes.NewReader(body))
	if err != nil {
		return cloudLoggingListResponse{}, fmt.Errorf("cloud logging create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return cloudLoggingListResponse{}, fmt.Errorf("cloud logging request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return cloudLoggingListResponse{}, fmt.Errorf("cloud logging read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cloudLoggingListResponse{}, fmt.Errorf("cloud logging returned status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var listResponse cloudLoggingListResponse
	if err := json.Unmarshal(respBody, &listResponse); err != nil {
		return cloudLoggingListResponse{}, fmt.Errorf("cloud logging decode response: %w", err)
	}

	return listResponse, nil
}

func buildHistoryFilter(configuredFilter string, start time.Time, end time.Time) string {
	timeFilter := fmt.Sprintf(
		`timestamp >= "%s" AND timestamp <= "%s"`,
		start.UTC().Format(time.RFC3339Nano),
		end.UTC().Format(time.RFC3339Nano),
	)
	if configuredFilter == "" {
		configuredFilter = `resource.type="k8s_container" AND resource.labels.container_name="alertmanager-webhook-logger" AND jsonPayload.message="Events received"`
	}

	return fmt.Sprintf("(%s) AND (%s)", configuredFilter, timeFilter)
}

func cloudLoggingResourceName(projectID string) string {
	return "projects/" + projectID
}

func aggregateHistoricalAlerts(entries []cloudLogEntry, labelMatchers []labelMatcher, groupBy []string) []HistoricalAlert {
	seen := map[string]HistoricalAlert{}
	seenInstances := map[string]struct{}{}

	for _, entry := range entries {
		for _, webhookAlert := range entry.JSONPayload.Alerts.Alerts {
			labels := webhookAlert.Labels
			if len(labels) == 0 {
				labels = entry.JSONPayload.Alerts.CommonLabels
			}
			if !labelsMatchFilters(labels, labelMatchers) {
				continue
			}

			groupLabels := historicalAlertGroupLabels(labels, groupBy)
			key := labelsFingerprint(groupLabels)
			if key == "" {
				continue
			}
			instanceKey := historicalAlertInstanceKey(key, webhookAlert, labels)

			seenAt := entry.Timestamp
			if seenAt.IsZero() {
				seenAt = webhookAlert.StartsAt
			}

			current, exists := seen[key]
			if !exists {
				seen[key] = HistoricalAlert{
					Labels:       labels,
					GroupLabels:  groupLabels,
					Fingerprint:  key,
					FirstSeen:    seenAt,
					LastSeen:     seenAt,
					GeneratorURL: webhookAlert.GeneratorURL,
					LogsURL:      cloudLogEntryURL(entry),
					LogName:      entry.LogName,
				}
				if _, instanceSeen := seenInstances[instanceKey]; !instanceSeen {
					current := seen[key]
					current.Count = 1
					seen[key] = current
					seenInstances[instanceKey] = struct{}{}
				}
				continue
			}

			if _, instanceSeen := seenInstances[instanceKey]; !instanceSeen {
				current.Count++
				seenInstances[instanceKey] = struct{}{}
			}
			if current.FirstSeen.IsZero() || (!seenAt.IsZero() && seenAt.Before(current.FirstSeen)) {
				current.FirstSeen = seenAt
			}
			if current.LastSeen.IsZero() || seenAt.After(current.LastSeen) {
				current.LastSeen = seenAt
				current.LogName = entry.LogName
			}
			if current.GeneratorURL == "" {
				current.GeneratorURL = webhookAlert.GeneratorURL
			}
			seen[key] = current
		}
	}

	result := make([]HistoricalAlert, 0, len(seen))
	for _, alert := range seen {
		alert.LogsURL = cloudLogGroupURL(alert.LogName, alert.GroupLabels, alert.FirstSeen, alert.LastSeen)
		result = append(result, alert)
	}

	return result
}

func historicalAlertInstanceKey(groupKey string, alert webhookAlert, labels map[string]string) string {
	fingerprint := alert.Fingerprint
	if fingerprint == "" {
		fingerprint = labelsFingerprint(labels)
	}

	startsAt := ""
	if !alert.StartsAt.IsZero() {
		startsAt = alert.StartsAt.UTC().Format(time.RFC3339Nano)
	}

	return groupKey + "\xff" + fingerprint + "\xff" + startsAt
}

func historicalAlertGroupLabels(labels map[string]string, groupBy []string) map[string]string {
	if len(groupBy) == 0 {
		return labels
	}

	groupLabels := make(map[string]string, len(groupBy))
	for _, label := range groupBy {
		if val := labels[label]; val != "" {
			groupLabels[label] = val
		}
	}

	if len(groupLabels) > 0 {
		return groupLabels
	}

	return labels
}

func cloudLogEntryURL(entry cloudLogEntry) string {
	projectID := cloudLogNameProjectID(entry.LogName)
	if projectID == "" || entry.InsertID == "" || entry.LogName == "" {
		return ""
	}

	query := fmt.Sprintf(
		"insertId=%q\nlogName=%q\ntimestamp=%q",
		entry.InsertID,
		entry.LogName,
		entry.Timestamp.UTC().Format(time.RFC3339Nano),
	)
	escapedQuery := strings.ReplaceAll(url.QueryEscape(query), "+", "%20")

	return fmt.Sprintf(
		"https://console.cloud.google.com/logs/query;query=%s?project=%s",
		escapedQuery,
		url.QueryEscape(projectID),
	)
}

func cloudLogGroupURL(logName string, labels map[string]string, start time.Time, end time.Time) string {
	projectID := cloudLogNameProjectID(logName)
	if projectID == "" || logName == "" || len(labels) == 0 {
		return ""
	}

	var lines []string
	lines = append(lines,
		`jsonPayload.message="Events received"`,
		fmt.Sprintf("logName=%q", logName),
	)
	if !start.IsZero() {
		lines = append(lines, fmt.Sprintf("timestamp >= %q", start.UTC().Format(time.RFC3339Nano)))
	}
	if !end.IsZero() {
		lines = append(lines, fmt.Sprintf("timestamp <= %q", end.UTC().Format(time.RFC3339Nano)))
	}

	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if labels[key] == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf(
			"jsonPayload.alerts.alerts.labels.%s=%q",
			cloudLoggingFieldPathSegment(key),
			labels[key],
		))
	}

	escapedQuery := strings.ReplaceAll(url.QueryEscape(strings.Join(lines, "\n")), "+", "%20")

	return fmt.Sprintf(
		"https://console.cloud.google.com/logs/query;query=%s?project=%s",
		escapedQuery,
		url.QueryEscape(projectID),
	)
}

func cloudLoggingFieldPathSegment(segment string) string {
	if segment == "" {
		return `""`
	}

	for i, r := range segment {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}

		return strconv.Quote(segment)
	}

	return segment
}

func cloudLogNameProjectID(logName string) string {
	const prefix = "projects/"

	if !strings.HasPrefix(logName, prefix) {
		return ""
	}

	rest := strings.TrimPrefix(logName, prefix)
	projectID, _, found := strings.Cut(rest, "/")
	if !found {
		return ""
	}

	return projectID
}

func sortHistoricalAlerts(alerts []HistoricalAlert) {
	sort.SliceStable(alerts, func(i, j int) bool {
		ai := alerts[i]
		aj := alerts[j]

		if ai.Count != aj.Count {
			return ai.Count > aj.Count
		}

		si := severityRank(ai.Labels["severity"])
		sj := severityRank(aj.Labels["severity"])

		if si != sj {
			return si < sj
		}

		if ai.Labels["alertname"] != aj.Labels["alertname"] {
			return ai.Labels["alertname"] < aj.Labels["alertname"]
		}

		if ai.Labels["cluster"] != aj.Labels["cluster"] {
			return ai.Labels["cluster"] < aj.Labels["cluster"]
		}

		return ai.LastSeen.After(aj.LastSeen)
	})
}

func countHistoricalOccurrences(alerts []HistoricalAlert) int {
	total := 0
	for _, alert := range alerts {
		total += alert.Count
	}
	return total
}
