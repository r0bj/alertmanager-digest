package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Alertmanager struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
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
	Count                 int    `json:"-"`
}

type AlertStatus struct {
	State       string   `json:"state"`
	SilencedBy  []string `json:"silencedBy"`
	InhibitedBy []string `json:"inhibitedBy"`
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
	return labelsFingerprint(alert.Labels)
}

func aggregateActiveAlerts(alerts []Alert, groupBy []string) []Alert {
	seen := map[string]Alert{}

	for _, alert := range alerts {
		groupLabels := activeAlertGroupLabels(alert.Labels, groupBy)
		key := labelsFingerprint(groupLabels)
		if key == "" {
			key = fallbackFingerprint(alert)
		}

		current, exists := seen[key]
		if !exists {
			if alert.Count <= 0 {
				alert.Count = 1
			}
			seen[key] = alert
			continue
		}

		increment := alert.Count
		if increment <= 0 {
			increment = 1
		}
		current.Count += increment
		if current.StartsAt.IsZero() || (!alert.StartsAt.IsZero() && alert.StartsAt.Before(current.StartsAt)) {
			current.StartsAt = alert.StartsAt
		}
		if current.UpdatedAt.IsZero() || alert.UpdatedAt.After(current.UpdatedAt) {
			current.UpdatedAt = alert.UpdatedAt
		}
		if current.GeneratorURL == "" {
			current.GeneratorURL = alert.GeneratorURL
		}
		current.SourceAlertmanager = mergeSource(current.SourceAlertmanager, alert.SourceAlertmanager)
		current.SourceAlertmanagerURL = mergeSource(current.SourceAlertmanagerURL, alert.SourceAlertmanagerURL)
		seen[key] = current
	}

	result := make([]Alert, 0, len(seen))
	for _, alert := range seen {
		result = append(result, alert)
	}

	return result
}

func activeAlertGroupLabels(labels map[string]string, groupBy []string) map[string]string {
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

func labelsFingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(labels[key])
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

		return ai.StartsAt.Before(aj.StartsAt)
	})
}

func severityRank(severity string) int {
	switch strings.ToLower(severity) {
	case "critical":
		return 0
	case "warning":
		return 1
	case "info":
		return 2
	default:
		return 99
	}
}
