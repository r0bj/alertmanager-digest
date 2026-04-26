package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCompactDurationOmitsZeroUnits(t *testing.T) {
	tests := map[string]struct {
		duration time.Duration
		want     string
	}{
		"day as hours": {
			duration: 24 * time.Hour,
			want:     "24h",
		},
		"hours and minutes": {
			duration: 25*time.Hour + 30*time.Minute,
			want:     "25h30m",
		},
		"minutes and seconds": {
			duration: 90 * time.Second,
			want:     "1m30s",
		},
		"zero": {
			duration: 0,
			want:     "0s",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := compactDuration(tc.duration); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestFormatHistoricalAlertUsesRelativeLastSeen(t *testing.T) {
	alert := HistoricalAlert{
		Labels: map[string]string{
			"alertname": "HighLatency",
			"severity":  "warning",
			"cluster":   "c1",
		},
		Count:    3,
		LastSeen: time.Now().Add(-2*time.Hour - 3*time.Minute),
		LogsURL:  "https://console.cloud.google.com/logs/query;query=insertId%3D%22abc%22?project=bethink-prod",
	}

	line := formatHistoricalAlert(alert, []string{"severity", "alertname", "cluster"}, nil)

	if !strings.Contains(line, "occurrences: 3, last seen 2h 3m") {
		t.Fatalf("expected relative last seen duration, got %q", line)
	}
	if strings.Contains(line, "T") || strings.Contains(line, "Z") {
		t.Fatalf("expected no RFC3339 timestamp, got %q", line)
	}
	if !strings.Contains(line, "|GCP Logs>") {
		t.Fatalf("expected GCP Logs link, got %q", line)
	}
}

func TestFormatAlertShowsAllLabels(t *testing.T) {
	alert := Alert{
		Labels: map[string]string{
			"alertname":   "HighLatency",
			"cluster":     "c1",
			"environment": "prod",
			"severity":    "warning",
			"team":        "lms",
		},
		StartsAt: time.Now(),
	}

	line := formatAlert(alert, []string{"*"}, nil)

	for _, want := range []string{
		"cluster=c1",
		"environment=prod",
		"severity=warning",
		"team=lms",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("expected all-label output to contain %q, got %q", want, line)
		}
	}
	if strings.Contains(line, "alertname=HighLatency") {
		t.Fatalf("expected alertname to be excluded from label fields, got %q", line)
	}
}

func TestFormatAlertShowsConfiguredDisplayLabels(t *testing.T) {
	alert := Alert{
		Labels: map[string]string{
			"alertname":   "HighLatency",
			"cluster":     "c1",
			"environment": "prod",
			"severity":    "warning",
			"team":        "lms",
		},
		StartsAt: time.Now(),
	}

	line := formatAlert(alert, []string{"severity", "alertname", "team"}, nil)

	if !strings.Contains(line, "severity=warning") || !strings.Contains(line, "team=lms") {
		t.Fatalf("expected configured labels, got %q", line)
	}
	if strings.Contains(line, "cluster=c1") || strings.Contains(line, "environment=prod") {
		t.Fatalf("expected only configured labels, got %q", line)
	}
}

func TestFormatAlertExcludesLabelsFromAllLabels(t *testing.T) {
	alert := Alert{
		Labels: map[string]string{
			"alertname":   "HighLatency",
			"cluster":     "c1",
			"environment": "prod",
			"severity":    "warning",
			"team":        "lms",
		},
		StartsAt: time.Now(),
	}

	line := formatAlert(alert, []string{"*"}, []string{"environment", "team"})

	if !strings.Contains(line, "cluster=c1") || !strings.Contains(line, "severity=warning") {
		t.Fatalf("expected non-excluded labels, got %q", line)
	}
	if strings.Contains(line, "environment=prod") || strings.Contains(line, "team=lms") {
		t.Fatalf("expected excluded labels to be omitted, got %q", line)
	}
}

func TestFormatAlertExcludesConfiguredDisplayLabels(t *testing.T) {
	alert := Alert{
		Labels: map[string]string{
			"alertname": "HighLatency",
			"cluster":   "c1",
			"severity":  "warning",
			"team":      "lms",
		},
		StartsAt: time.Now(),
	}

	line := formatAlert(alert, []string{"severity", "team", "cluster"}, []string{"team"})

	if !strings.Contains(line, "severity=warning") || !strings.Contains(line, "cluster=c1") {
		t.Fatalf("expected non-excluded configured labels, got %q", line)
	}
	if strings.Contains(line, "team=lms") {
		t.Fatalf("expected excluded configured label to be omitted, got %q", line)
	}
}

func TestAppendHistoryBlocksDoesNotUseInlineCodeForWindow(t *testing.T) {
	cfg := Config{
		History: HistoryConfig{
			Window: Duration{Duration: 24 * time.Hour},
		},
	}
	alerts := []HistoricalAlert{
		{
			Labels: map[string]string{
				"alertname": "HighLatency",
				"severity":  "warning",
			},
			Count:    1,
			LastSeen: time.Now(),
		},
	}

	blocks := appendHistoryBlocks(nil, cfg, alerts)

	if len(blocks) != 2 {
		t.Fatalf("expected 2 history blocks, got %d", len(blocks))
	}

	summary := blocks[0].Text.Text
	if !strings.Contains(summary, "Alerts sent in the last 24h") {
		t.Fatalf("expected plain history window, got %q", summary)
	}
	if strings.Contains(summary, "`24h`") {
		t.Fatalf("expected no inline-code formatting for history window, got %q", summary)
	}
}

func TestBuildSlackPayloadUsesTopLevelBlocks(t *testing.T) {
	cfg := Config{
		Title: "Daily Alertmanager digest",
		Slack: SlackConfig{
			SendEmptyMessage: true,
		},
	}

	payload := buildSlackPayload(cfg, nil, nil, nil, nil)

	if len(payload.Blocks) == 0 {
		t.Fatal("expected top-level blocks")
	}
	if payload.Text == "" {
		t.Fatal("expected fallback text")
	}
}

func TestBuildSlackPayloadDoesNotUseAttachments(t *testing.T) {
	cfg := Config{
		Title: "Daily Alertmanager digest",
		Slack: SlackConfig{
			SendEmptyMessage: true,
		},
	}

	payload := buildSlackPayload(cfg, nil, nil, nil, nil)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if strings.Contains(string(encoded), `"attachments":`) {
		t.Fatalf("expected no attachments in payload JSON, got %s", encoded)
	}
}

func TestBuildSlackPayloadSeparatesActiveAndHistoricalSections(t *testing.T) {
	cfg := Config{
		Title: "Daily Alertmanager digest",
		History: HistoryConfig{
			Enabled: true,
			Window:  Duration{Duration: 24 * time.Hour},
		},
		Slack: SlackConfig{
			SendEmptyMessage: true,
		},
	}

	payload := buildSlackPayload(cfg, nil, nil, nil, nil)
	blocks := payload.Blocks

	foundDivider := false
	for _, block := range blocks {
		if block.Type == "divider" {
			foundDivider = true
			break
		}
	}
	if !foundDivider {
		t.Fatalf("expected divider block between active and historical sections, got %#v", blocks)
	}
}
