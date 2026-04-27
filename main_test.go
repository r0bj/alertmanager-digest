package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAggregateHistoricalAlertsCountsByFingerprint(t *testing.T) {
	firstSeen := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	lastSeen := time.Date(2026, 4, 25, 11, 0, 0, 0, time.UTC)

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
	if alerts[0].Labels["alertname"] != "HighLatency" {
		t.Fatalf("expected HighLatency alert first by count, got %#v", alerts[0].Labels)
	}
	if alerts[0].Count != 2 {
		t.Fatalf("expected count 2, got %d", alerts[0].Count)
	}
	if !alerts[0].FirstSeen.Equal(firstSeen) {
		t.Fatalf("expected first seen %s, got %s", firstSeen, alerts[0].FirstSeen)
	}
	if !alerts[0].LastSeen.Equal(lastSeen) {
		t.Fatalf("expected last seen %s, got %s", lastSeen, alerts[0].LastSeen)
	}
	if !strings.Contains(alerts[0].LogsURL, "console.cloud.google.com/logs/query") {
		t.Fatalf("expected GCP Logs URL, got %q", alerts[0].LogsURL)
	}
	if !strings.Contains(alerts[0].LogsURL, "last-insert-id") {
		t.Fatalf("expected latest log entry link, got %q", alerts[0].LogsURL)
	}
	if !strings.Contains(alerts[0].LogsURL, "project=bethink-prod") {
		t.Fatalf("expected project in GCP Logs URL, got %q", alerts[0].LogsURL)
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
