package main

import (
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

	line := formatHistoricalAlert(alert, []string{"severity", "alertname", "cluster"})

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
