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
	Fingerprint  string
	Count        int
	FirstSeen    time.Time
	LastSeen     time.Time
	GeneratorURL string
	LogsURL      string
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

	alerts := aggregateHistoricalAlerts(entries, labelMatchers)
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
		projectAlerts := aggregateHistoricalAlerts(projectEntries, labelMatchers)
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

func aggregateHistoricalAlerts(entries []cloudLogEntry, labelMatchers []labelMatcher) []HistoricalAlert {
	seen := map[string]HistoricalAlert{}

	for _, entry := range entries {
		for _, webhookAlert := range entry.JSONPayload.Alerts.Alerts {
			labels := webhookAlert.Labels
			if len(labels) == 0 {
				labels = entry.JSONPayload.Alerts.CommonLabels
			}
			if !labelsMatchFilters(labels, labelMatchers) {
				continue
			}

			key := webhookAlert.Fingerprint
			if key == "" {
				key = labelsFingerprint(labels)
			}
			if key == "" {
				continue
			}

			seenAt := entry.Timestamp
			if seenAt.IsZero() {
				seenAt = webhookAlert.StartsAt
			}

			current, exists := seen[key]
			if !exists {
				seen[key] = HistoricalAlert{
					Labels:       labels,
					Fingerprint:  key,
					Count:        1,
					FirstSeen:    seenAt,
					LastSeen:     seenAt,
					GeneratorURL: webhookAlert.GeneratorURL,
					LogsURL:      cloudLogEntryURL(entry),
				}
				continue
			}

			current.Count++
			if current.FirstSeen.IsZero() || (!seenAt.IsZero() && seenAt.Before(current.FirstSeen)) {
				current.FirstSeen = seenAt
			}
			if current.LastSeen.IsZero() || seenAt.After(current.LastSeen) {
				current.LastSeen = seenAt
				current.LogsURL = cloudLogEntryURL(entry)
			}
			if current.GeneratorURL == "" {
				current.GeneratorURL = webhookAlert.GeneratorURL
			}
			if current.LogsURL == "" {
				current.LogsURL = cloudLogEntryURL(entry)
			}
			seen[key] = current
		}
	}

	result := make([]HistoricalAlert, 0, len(seen))
	for _, alert := range seen {
		result = append(result, alert)
	}

	return result
}

func cloudLogEntryURL(entry cloudLogEntry) string {
	projectID := cloudLogEntryProjectID(entry)
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

func cloudLogEntryProjectID(entry cloudLogEntry) string {
	const prefix = "projects/"

	if !strings.HasPrefix(entry.LogName, prefix) {
		return ""
	}

	rest := strings.TrimPrefix(entry.LogName, prefix)
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
