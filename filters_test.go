package main

import "testing"

func TestLabelMatchers(t *testing.T) {
	labels := map[string]string{
		"alertname": "HighLatency",
		"severity":  "warning",
		"team":      "lms",
	}

	tests := map[string]struct {
		filters []string
		want    bool
	}{
		"exact match": {
			filters: []string{"team=lms"},
			want:    true,
		},
		"quoted exact match": {
			filters: []string{`team="lms"`},
			want:    true,
		},
		"exact mismatch": {
			filters: []string{"team=ops"},
			want:    false,
		},
		"negative match": {
			filters: []string{"team!=ops"},
			want:    true,
		},
		"regex match": {
			filters: []string{`severity=~"warning|critical"`},
			want:    true,
		},
		"negative regex match": {
			filters: []string{`alertname!~"Disk.*"`},
			want:    true,
		},
		"all matchers must match": {
			filters: []string{`severity=~"warning|critical"`, "team=lms", `alertname=~"High.*"`},
			want:    true,
		},
		"one matcher fails": {
			filters: []string{`severity=~"warning|critical"`, "team=ops"},
			want:    false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			matchers, err := parseLabelMatchers(tc.filters)
			if err != nil {
				t.Fatalf("parse matchers: %v", err)
			}
			if got := labelsMatchFilters(labels, matchers); got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestParseLabelMatcherRejectsInvalidRegex(t *testing.T) {
	if _, err := parseLabelMatchers([]string{`severity=~"["`}); err == nil {
		t.Fatal("expected invalid regex error")
	}
}
