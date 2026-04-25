package main

import (
	"strings"
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

	alerts := aggregateHistoricalAlerts(entries)
	sortHistoricalAlerts(alerts)

	if len(alerts) != 2 {
		t.Fatalf("expected 2 unique alerts, got %d", len(alerts))
	}
	if alerts[0].Fingerprint != "abc" {
		t.Fatalf("expected alert abc first by count, got %q", alerts[0].Fingerprint)
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
