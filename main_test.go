package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShouldSendSlackMessageSkipsEmptyDigestByDefault(t *testing.T) {
	if shouldSendSlackMessage(Config{}, nil, nil, nil, nil) {
		t.Fatal("expected empty digest to be skipped by default")
	}
}

func TestShouldSendSlackMessageAllowsEmptyDigestWhenConfigured(t *testing.T) {
	cfg := Config{
		Slack: SlackConfig{
			SendEmptyMessage: true,
		},
	}

	if !shouldSendSlackMessage(cfg, nil, nil, nil, nil) {
		t.Fatal("expected configured empty digest to be sent")
	}
}

func TestShouldSendSlackMessageSendsWhenThereIsContentOrErrors(t *testing.T) {
	tests := map[string]struct {
		alerts           []Alert
		historicalAlerts []HistoricalAlert
		fetchErrors      []error
		historyErrors    []error
	}{
		"active alerts": {
			alerts: []Alert{{Labels: map[string]string{"alertname": "HighLatency"}}},
		},
		"historical alerts": {
			historicalAlerts: []HistoricalAlert{{Labels: map[string]string{"alertname": "HighLatency"}}},
		},
		"fetch errors": {
			fetchErrors: []error{errors.New("alertmanager failed")},
		},
		"history errors": {
			historyErrors: []error{errors.New("history failed")},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if !shouldSendSlackMessage(Config{}, tc.alerts, tc.historicalAlerts, tc.fetchErrors, tc.historyErrors) {
				t.Fatal("expected Slack message to be sent")
			}
		})
	}
}

func TestAggregateHistoricalAlertsCountsUniqueAlertInstances(t *testing.T) {
	firstSeen := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	lastSeen := time.Date(2026, 4, 25, 11, 0, 0, 0, time.UTC)
	startsAt := time.Date(2026, 4, 25, 9, 55, 0, 0, time.UTC)

	entries := []cloudLogEntry{
		{
			InsertID:  "first-insert-id",
			LogName:   "projects/bethink-prod/logs/stdout",
			Timestamp: firstSeen,
			JSONPayload: webhookLoggerPayload{
				Alerts: webhookPayload{
					Alerts: []webhookAlert{
						{
							Fingerprint: "abc",
							StartsAt:    startsAt,
							Labels: map[string]string{
								"alertname": "HighLatency",
								"severity":  "warning",
								"cluster":   "c1",
							},
						},
					},
				},
			},
		},
		{
			InsertID:  "last-insert-id",
			LogName:   "projects/bethink-prod/logs/stdout",
			Timestamp: lastSeen,
			JSONPayload: webhookLoggerPayload{
				Alerts: webhookPayload{
					Alerts: []webhookAlert{
						{
							Fingerprint: "abc",
							StartsAt:    startsAt,
							Labels: map[string]string{
								"alertname": "HighLatency",
								"severity":  "warning",
								"cluster":   "c1",
							},
						},
						{
							Fingerprint: "def",
							Labels: map[string]string{
								"alertname": "DiskFull",
								"severity":  "critical",
								"cluster":   "c2",
							},
						},
					},
				},
			},
		},
	}

	alerts := aggregateHistoricalAlerts(entries, nil, nil)
	sortHistoricalAlerts(alerts)

	if len(alerts) != 2 {
		t.Fatalf("expected 2 unique alerts, got %d", len(alerts))
	}
	highLatency := findHistoricalAlertByName(alerts, "HighLatency")
	if highLatency == nil {
		t.Fatalf("expected HighLatency alert, got %#v", alerts)
	}
	if highLatency.Count != 1 {
		t.Fatalf("expected repeated webhook alert to count once, got %d", highLatency.Count)
	}
	if highLatency.Notifications != 2 {
		t.Fatalf("expected repeated webhook alert to count as 2 notifications, got %d", highLatency.Notifications)
	}
	if !highLatency.FirstSeen.Equal(firstSeen) {
		t.Fatalf("expected first seen %s, got %s", firstSeen, highLatency.FirstSeen)
	}
	if !highLatency.LastSeen.Equal(lastSeen) {
		t.Fatalf("expected last seen %s, got %s", lastSeen, highLatency.LastSeen)
	}
	if !strings.Contains(highLatency.LogsURL, "console.cloud.google.com/logs/query") {
		t.Fatalf("expected GCP Logs URL, got %q", highLatency.LogsURL)
	}
	if !strings.Contains(highLatency.LogsURL, "jsonPayload.alerts.alerts.labels.alertname%3D%22HighLatency%22") {
		t.Fatalf("expected grouped alertname log query, got %q", highLatency.LogsURL)
	}
	if !strings.Contains(highLatency.LogsURL, "jsonPayload.alerts.alerts.labels.cluster%3D%22c1%22") {
		t.Fatalf("expected grouped cluster log query, got %q", highLatency.LogsURL)
	}
	if strings.Contains(highLatency.LogsURL, "last-insert-id") {
		t.Fatalf("expected group log query, got single log entry link %q", highLatency.LogsURL)
	}
	if !strings.Contains(highLatency.LogsURL, "project=bethink-prod") {
		t.Fatalf("expected project in GCP Logs URL, got %q", highLatency.LogsURL)
	}
}

func TestAggregateActiveAlertsGroupsByConfiguredLabels(t *testing.T) {
	alerts := []Alert{
		{
			Labels: map[string]string{
				"alertname": "ArgocdApplicationNotSynced",
				"severity":  "warning",
				"cluster":   "gke-prod",
				"team":      "websites",
				"name":      "site-one",
			},
			StartsAt:              time.Date(2026, 5, 15, 7, 0, 0, 0, time.UTC),
			SourceAlertmanager:    "primary",
			SourceAlertmanagerURL: "https://alertmanager-primary.example",
		},
		{
			Labels: map[string]string{
				"alertname": "ArgocdApplicationNotSynced",
				"severity":  "warning",
				"cluster":   "gke-prod",
				"team":      "websites",
				"name":      "site-two",
			},
			StartsAt:              time.Date(2026, 5, 15, 7, 2, 0, 0, time.UTC),
			SourceAlertmanager:    "secondary",
			SourceAlertmanagerURL: "https://alertmanager-secondary.example",
		},
	}

	grouped := aggregateActiveAlerts(alerts, []string{"severity", "alertname", "cluster", "team"})

	if len(grouped) != 1 {
		t.Fatalf("expected active alerts to be grouped by configured labels, got %d", len(grouped))
	}
	if grouped[0].Count != 2 {
		t.Fatalf("expected grouped active count 2, got %d", grouped[0].Count)
	}
	if !grouped[0].StartsAt.Equal(alerts[0].StartsAt) {
		t.Fatalf("expected grouped alert to keep earliest startsAt, got %s", grouped[0].StartsAt)
	}
	if grouped[0].SourceAlertmanager != "primary,secondary" {
		t.Fatalf("expected merged alertmanager names, got %q", grouped[0].SourceAlertmanager)
	}
}

func findHistoricalAlertByName(alerts []HistoricalAlert, alertname string) *HistoricalAlert {
	for i := range alerts {
		if alerts[i].Labels["alertname"] == alertname {
			return &alerts[i]
		}
	}

	return nil
}

func TestAggregateHistoricalAlertsCountsRefireWithSameFingerprint(t *testing.T) {
	entries := []cloudLogEntry{
		{
			Timestamp: time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC),
			JSONPayload: webhookLoggerPayload{
				Alerts: webhookPayload{
					Alerts: []webhookAlert{
						{
							Fingerprint: "abc",
							StartsAt:    time.Date(2026, 4, 25, 9, 55, 0, 0, time.UTC),
							Labels: map[string]string{
								"alertname": "HighLatency",
								"severity":  "warning",
								"cluster":   "c1",
							},
						},
					},
				},
			},
		},
		{
			Timestamp: time.Date(2026, 4, 25, 11, 0, 0, 0, time.UTC),
			JSONPayload: webhookLoggerPayload{
				Alerts: webhookPayload{
					Alerts: []webhookAlert{
						{
							Fingerprint: "abc",
							StartsAt:    time.Date(2026, 4, 25, 10, 55, 0, 0, time.UTC),
							Labels: map[string]string{
								"alertname": "HighLatency",
								"severity":  "warning",
								"cluster":   "c1",
							},
						},
					},
				},
			},
		},
	}

	alerts := aggregateHistoricalAlerts(entries, nil, nil)

	if len(alerts) != 1 {
		t.Fatalf("expected same fingerprint to stay in one alert group, got %d", len(alerts))
	}
	if alerts[0].Count != 2 {
		t.Fatalf("expected same fingerprint with new startsAt to count as refire, got %d", alerts[0].Count)
	}
}

func TestAggregateHistoricalAlertsAppliesLabelFilters(t *testing.T) {
	entries := []cloudLogEntry{
		{
			Timestamp: time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC),
			JSONPayload: webhookLoggerPayload{
				Alerts: webhookPayload{
					Alerts: []webhookAlert{
						{
							Fingerprint: "warning",
							Labels: map[string]string{
								"alertname": "HighLatency",
								"severity":  "warning",
								"team":      "lms",
							},
						},
						{
							Fingerprint: "info",
							Labels: map[string]string{
								"alertname": "InfoAlert",
								"severity":  "info",
								"team":      "ops",
							},
						},
					},
				},
			},
		},
	}
	matchers, err := parseLabelMatchers([]string{`severity=~"warning|critical"`, "team!=ops"})
	if err != nil {
		t.Fatalf("parse matchers: %v", err)
	}

	alerts := aggregateHistoricalAlerts(entries, matchers, nil)

	if len(alerts) != 1 {
		t.Fatalf("expected 1 historical alert after filtering, got %d", len(alerts))
	}
	if alerts[0].Labels["alertname"] != "HighLatency" {
		t.Fatalf("expected warning alert, got %#v", alerts[0].Labels)
	}
}

func TestAggregateHistoricalAlertsGroupsByConfiguredLabels(t *testing.T) {
	entries := []cloudLogEntry{
		{
			Timestamp: time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC),
			JSONPayload: webhookLoggerPayload{
				Alerts: webhookPayload{
					Alerts: []webhookAlert{
						{
							Fingerprint: "one",
							Labels: map[string]string{
								"alertname": "PlatformWorkerLastHeartbeatTooOld",
								"severity":  "critical",
								"cluster":   "gke-prod",
								"namespace": "lek",
								"team":      "lms",
								"pod":       "worker-1",
							},
						},
						{
							Fingerprint: "two",
							Labels: map[string]string{
								"alertname": "PlatformWorkerLastHeartbeatTooOld",
								"severity":  "critical",
								"cluster":   "gke-prod",
								"namespace": "lek",
								"team":      "lms",
								"pod":       "worker-2",
							},
						},
					},
				},
			},
		},
	}

	alerts := aggregateHistoricalAlerts(entries, nil, []string{"severity", "alertname", "cluster", "namespace", "team"})

	if len(alerts) != 1 {
		t.Fatalf("expected alerts to be grouped by configured labels, got %d", len(alerts))
	}
	if alerts[0].Count != 2 {
		t.Fatalf("expected grouped count 2, got %d", alerts[0].Count)
	}
}

func TestBuildHistoryFilterAddsTimeWindow(t *testing.T) {
	start := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	filter := buildHistoryFilter(`resource.type="k8s_container"`, start, end)

	for _, want := range []string{
		`resource.type="k8s_container"`,
		`timestamp >= "2026-04-24T12:00:00Z"`,
		`timestamp <= "2026-04-25T12:00:00Z"`,
	} {
		if !strings.Contains(filter, want) {
			t.Fatalf("expected filter to contain %q, got %q", want, filter)
		}
	}
}

func TestLogFetchedHistoricalAlertsIncludesProjectID(t *testing.T) {
	var buf bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
	})

	cfg := Config{
		History: HistoryConfig{
			Window: Duration{Duration: 24 * time.Hour},
		},
	}
	alerts := []HistoricalAlert{
		{Count: 2},
		{Count: 1},
	}

	logFetchedHistoricalAlerts("bethink-prod", alerts, cfg, nil)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("decode log record: %v", err)
	}
	if record["project_id"] != "bethink-prod" {
		t.Fatalf("unexpected project_id: %#v", record["project_id"])
	}
}

func TestFetchHistoricalAlertEntriesForProjectUsesSingleProjectAndKeepsPartialEntries(t *testing.T) {
	var requests []cloudLoggingListRequest
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var requestBody cloudLoggingListRequest
			if err := json.NewDecoder(req.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			requests = append(requests, requestBody)

			if requestBody.PageToken == "" {
				body := `{"entries":[{"insertId":"first","logName":"projects/bethink-prod/logs/stdout"}],"nextPageToken":"next"}`
				return stringResponse(http.StatusOK, body), nil
			}

			return stringResponse(http.StatusForbidden, `{"error":"denied"}`), nil
		}),
	}
	cfg := Config{
		History: HistoryConfig{
			PageSize: 1000,
		},
	}

	entries, err := fetchHistoricalAlertEntriesForProject(context.Background(), client, "token", cfg, "bethink-prod", `timestamp >= "2026-04-25T00:00:00Z"`)

	if err == nil {
		t.Fatal("expected second page error")
	}
	if len(entries) != 1 {
		t.Fatalf("expected partial entries from first page, got %d", len(entries))
	}
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	for _, requestBody := range requests {
		if len(requestBody.ResourceNames) != 1 || requestBody.ResourceNames[0] != "projects/bethink-prod" {
			t.Fatalf("unexpected resource names: %#v", requestBody.ResourceNames)
		}
	}
}

func TestFetchHistoricalAlertEntriesForProjectsRunsConcurrently(t *testing.T) {
	var mu sync.Mutex
	activeRequests := 0
	maxActiveRequests := 0
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			activeRequests++
			maxActiveRequests = max(maxActiveRequests, activeRequests)
			mu.Unlock()

			time.Sleep(25 * time.Millisecond)

			mu.Lock()
			activeRequests--
			mu.Unlock()

			return stringResponse(http.StatusOK, `{}`), nil
		}),
	}
	cfg := Config{
		History: HistoryConfig{
			ProjectIDs: []string{"bethink-prod", "bethink-dev", "bethink-stage"},
			PageSize:   1000,
			Window:     Duration{Duration: 24 * time.Hour},
		},
	}

	_, errs := fetchHistoricalAlertEntriesForProjects(context.Background(), client, "token", cfg, `timestamp >= "2026-04-25T00:00:00Z"`, nil)

	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
	if maxActiveRequests < 2 {
		t.Fatalf("expected concurrent project requests, max active requests was %d", maxActiveRequests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func stringResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestCloudLogEntryURL(t *testing.T) {
	entry := cloudLogEntry{
		InsertID:  "abc123",
		LogName:   "projects/bethink-prod/logs/stdout",
		Timestamp: time.Date(2026, 4, 25, 19, 33, 7, 0, time.UTC),
	}

	logsURL := cloudLogEntryURL(entry)

	for _, want := range []string{
		"https://console.cloud.google.com/logs/query;query=",
		"insertId%3D%22abc123%22",
		"logName%3D%22projects%2Fbethink-prod%2Flogs%2Fstdout%22",
		"timestamp%3D%222026-04-25T19%3A33%3A07Z%22",
		"project=bethink-prod",
	} {
		if !strings.Contains(logsURL, want) {
			t.Fatalf("expected URL to contain %q, got %q", want, logsURL)
		}
	}
}

func TestCloudLogGroupURL(t *testing.T) {
	logsURL := cloudLogGroupURL(
		"projects/bethink-prod/logs/stdout",
		map[string]string{
			"alertname":              "PodContainerRestartDetected",
			"app.kubernetes.io/name": "worker",
			"namespace":              "lek",
		},
		time.Date(2026, 4, 25, 19, 33, 7, 0, time.UTC),
		time.Date(2026, 4, 25, 20, 33, 7, 0, time.UTC),
	)

	for _, want := range []string{
		"https://console.cloud.google.com/logs/query;query=",
		"jsonPayload.message%3D%22Events%20received%22",
		"logName%3D%22projects%2Fbethink-prod%2Flogs%2Fstdout%22",
		"timestamp%20%3E%3D%20%222026-04-25T19%3A33%3A07Z%22",
		"timestamp%20%3C%3D%20%222026-04-25T20%3A33%3A07Z%22",
		"jsonPayload.alerts.alerts.labels.alertname%3D%22PodContainerRestartDetected%22",
		"jsonPayload.alerts.alerts.labels.%22app.kubernetes.io%2Fname%22%3D%22worker%22",
		"jsonPayload.alerts.alerts.labels.namespace%3D%22lek%22",
		"project=bethink-prod",
	} {
		if !strings.Contains(logsURL, want) {
			t.Fatalf("expected URL to contain %q, got %q", want, logsURL)
		}
	}
}
