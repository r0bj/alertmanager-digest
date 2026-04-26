package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type labelMatcher struct {
	Name    string
	Value   string
	Op      string
	Pattern *regexp.Regexp
}

func parseLabelMatchers(filters []string) ([]labelMatcher, error) {
	matchers := make([]labelMatcher, 0, len(filters))

	for _, filter := range filters {
		matcher, err := parseLabelMatcher(filter)
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, matcher)
	}

	return matchers, nil
}

func parseLabelMatcher(filter string) (labelMatcher, error) {
	filter = strings.TrimSpace(filter)
	for _, op := range []string{"!~", "=~", "!=", "="} {
		if idx := strings.Index(filter, op); idx >= 0 {
			name := strings.TrimSpace(filter[:idx])
			value := strings.TrimSpace(filter[idx+len(op):])
			if name == "" {
				return labelMatcher{}, fmt.Errorf("invalid label matcher %q: label name is required", filter)
			}
			if value == "" {
				return labelMatcher{}, fmt.Errorf("invalid label matcher %q: label value is required", filter)
			}

			unquotedValue, err := unquoteLabelMatcherValue(value)
			if err != nil {
				return labelMatcher{}, fmt.Errorf("invalid label matcher %q: %w", filter, err)
			}

			matcher := labelMatcher{
				Name:  name,
				Value: unquotedValue,
				Op:    op,
			}
			if op == "=~" || op == "!~" {
				pattern, err := regexp.Compile(unquotedValue)
				if err != nil {
					return labelMatcher{}, fmt.Errorf("invalid label matcher %q: compile regex: %w", filter, err)
				}
				matcher.Pattern = pattern
			}

			return matcher, nil
		}
	}

	return labelMatcher{}, fmt.Errorf("invalid label matcher %q: expected one of =, !=, =~, !~", filter)
}

func unquoteLabelMatcherValue(value string) (string, error) {
	if len(value) < 2 {
		return value, nil
	}

	if value[0] != '"' || value[len(value)-1] != '"' {
		return value, nil
	}

	return strconv.Unquote(value)
}

func labelsMatchFilters(labels map[string]string, matchers []labelMatcher) bool {
	for _, matcher := range matchers {
		if !matcher.Matches(labels) {
			return false
		}
	}

	return true
}

func (m labelMatcher) Matches(labels map[string]string) bool {
	value := labels[m.Name]

	switch m.Op {
	case "=":
		return value == m.Value
	case "!=":
		return value != m.Value
	case "=~":
		return m.Pattern.MatchString(value)
	case "!~":
		return !m.Pattern.MatchString(value)
	default:
		return false
	}
}
