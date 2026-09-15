package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// ReviewRouterSettings keeps selection policy separate from execution. Entries
// are ordered OMP provider/model names; a verdict never triggers model fallback.
// DefaultReviewJobTimeout is the deadline a REVIEW gets regardless of which
// reviewer answers it (#2191).
//
// Measured on this box over seven days: 253 completed gm-review-opus reviews,
// median 18.5 minutes, MAX 163.9 minutes. Review-capable agents carried
// per-agent deadlines of 10m, 30m, 45m and 2h, so the deadline a review got
// depended on WHICH AGENT was picked - and the first router-rotated review drew
// a 10-minute agent for a class whose observed upper bound is 164 minutes and
// died mid-analysis after nine.
//
// Duration is a property of the PROMPT CLASS, not of the reviewer. 3h covers
// the observed maximum with headroom; note that the 2h some agents carry would
// itself have truncated that 164-minute review.
const DefaultReviewJobTimeout = 3 * time.Hour

type ReviewRouterSettings struct {
	Pools map[string][]string
	// JobTimeout is the floor applied to every review. An agent configured
	// LONGER keeps its own value; an agent configured shorter no longer
	// truncates a review it was merely selected for.
	JobTimeout time.Duration
}

func DefaultReviewRouterSettings() ReviewRouterSettings {
	return ReviewRouterSettings{
		Pools: map[string][]string{
			"code": {"devin/swe-2", "openai-codex/gpt-5.6-sol"},
		},
		JobTimeout: DefaultReviewJobTimeout,
	}
}

func (s ReviewRouterSettings) Models(purpose string) ([]string, error) {
	switch purpose {
	case "code", "security", "ui", "architecture":
	default:
		return nil, fmt.Errorf("unknown review purpose %q (use code, security, ui, or architecture)", purpose)
	}
	models := s.Pools[purpose]
	if len(models) == 0 {
		models = s.Pools["code"]
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no review models configured for %s", purpose)
	}
	return models, nil
}

// LoadReviewRouterSettings reads [review_router]. Each purpose key is an
// ordered array of provider-qualified models. An explicit empty pool is an
// error rather than silently turning off reviews or falling back to defaults.
func LoadReviewRouterSettings(paths Paths) (ReviewRouterSettings, error) {
	settings := DefaultReviewRouterSettings()
	content, err := os.ReadFile(paths.ConfigFile)
	if os.IsNotExist(err) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	section := ""
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if name, ok := sectionHeader(line); ok {
			section = name
			continue
		}
		if section != "review_router" || line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok {
			return settings, fmt.Errorf("invalid [review_router] entry %q", line)
		}
		if key == "job_timeout" {
			parsed, err := time.ParseDuration(strings.Trim(strings.TrimSpace(value), `"`))
			if err != nil {
				return settings, fmt.Errorf("parse [review_router].job_timeout: %w", err)
			}
			if parsed <= 0 {
				return settings, fmt.Errorf("[review_router].job_timeout must be positive, got %q", value)
			}
			settings.JobTimeout = parsed
			continue
		}
		if _, err := settings.Models(key); err != nil {
			return settings, err
		}
		models, err := parseConfigStringArray(strings.TrimSpace(value))
		if err != nil {
			return settings, fmt.Errorf("parse [review_router].%s: %w", key, err)
		}
		if len(models) == 0 || len(models) > 8 {
			return settings, fmt.Errorf("[review_router].%s requires between 1 and 8 models", key)
		}
		seen := make(map[string]bool, len(models))
		for _, model := range models {
			provider, name, qualified := strings.Cut(model, "/")
			if !qualified || provider == "" || name == "" || strings.ContainsAny(model, " \t\r\n") || seen[model] {
				return settings, fmt.Errorf("[review_router].%s: invalid or duplicate provider/model %q", key, model)
			}
			seen[model] = true
		}
		settings.Pools[key] = models
	}
	return settings, nil
}
