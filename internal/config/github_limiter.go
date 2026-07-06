package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultGitHubBackoffBase / DefaultGitHubBackoffMax bound the exponential
// secondary-rate-limit fallback used when a 403/429 secondary response carries no
// Retry-After (#683). They mirror the github package's Default*Backoff so the
// config default reproduces the library default.
const (
	DefaultGitHubBackoffBase = 60 * time.Second
	DefaultGitHubBackoffMax  = 5 * time.Minute
)

// GitHubLimiterPolicy is the resolved [github] section: the process-wide GitHub
// call budget + secondary-rate-limit backoff the daemon installs at startup (#683).
//
// SAFE, near-byte-identical defaults: MaxConcurrent 0 (no concurrency cap) and
// MinInterval 0 (no spacing) leave single-call latency and steady-state throughput
// unchanged — the proactive smoothing is OPT-IN. SecondaryBackoffEnabled defaults
// TRUE because the reactive pause is invisible on the happy path (it engages only
// after a gh call actually fails with a secondary/abuse limit) and it is exactly
// the protection the #683 incident needed: without it, callers retry into the abuse
// window and prolong the freeze. Set max_concurrent / min_interval to also smooth
// bursts proactively.
type GitHubLimiterPolicy struct {
	// MaxConcurrent caps in-flight gh calls process-wide. 0 (default) = unlimited.
	MaxConcurrent int
	// MinInterval is the minimum spacing between successive gh call starts. 0
	// (default) = no spacing.
	MinInterval time.Duration
	// SecondaryBackoffEnabled turns the process-wide secondary-rate-limit pause on.
	// Default true.
	SecondaryBackoffEnabled bool
	// BackoffBase / BackoffMax bound the exponential fallback used when a secondary
	// response carries no Retry-After. Defaults 60s / 5m.
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

// DefaultGitHubLimiterPolicy returns the safe default: no proactive caps, reactive
// secondary-limit backoff ON with the default exponential bounds.
func DefaultGitHubLimiterPolicy() GitHubLimiterPolicy {
	return GitHubLimiterPolicy{
		MaxConcurrent:           0,
		MinInterval:             0,
		SecondaryBackoffEnabled: true,
		BackoffBase:             DefaultGitHubBackoffBase,
		BackoffMax:              DefaultGitHubBackoffMax,
	}
}

// ProactiveSmoothingEnabled reports whether either proactive gate (concurrency cap
// or min-interval spacing) is active. When false the limiter only reacts to
// secondary limits and steady-state scheduling is unchanged.
func (p GitHubLimiterPolicy) ProactiveSmoothingEnabled() bool {
	return p.MaxConcurrent > 0 || p.MinInterval > 0
}

// LoadGitHubLimiterPolicy reads the [github] section, returning the default policy
// when the section is absent (so a config with no [github] block is byte-identical
// except for the invisible secondary-limit backoff).
func LoadGitHubLimiterPolicy(paths Paths) (GitHubLimiterPolicy, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return GitHubLimiterPolicy{}, err
	}
	policy := DefaultGitHubLimiterPolicy()
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			current = section == "github"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applyGitHubLimiterField(&policy, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return GitHubLimiterPolicy{}, fmt.Errorf("parse [github].%s: %w", strings.TrimSpace(key), err)
		}
	}
	if err := validateGitHubLimiterPolicy(policy); err != nil {
		return GitHubLimiterPolicy{}, err
	}
	return policy, nil
}

func applyGitHubLimiterField(policy *GitHubLimiterPolicy, key string, value string) error {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	switch key {
	case "max_concurrent":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		policy.MaxConcurrent = parsed
	case "min_interval":
		d, err := parseGitHubDuration(value)
		if err != nil {
			return err
		}
		policy.MinInterval = d
	case "secondary_backoff":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		policy.SecondaryBackoffEnabled = parsed
	case "backoff_base":
		d, err := parseGitHubDuration(value)
		if err != nil {
			return err
		}
		policy.BackoffBase = d
	case "backoff_max":
		d, err := parseGitHubDuration(value)
		if err != nil {
			return err
		}
		policy.BackoffMax = d
	default:
		// Unknown keys are ignored so a newer config on an older binary still loads.
	}
	return nil
}

// parseGitHubDuration accepts either a Go duration string ("500ms", "2s") or a
// bare integer read as whole seconds, matching how operators tend to write these.
func parseGitHubDuration(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second, nil
	}
	return time.ParseDuration(value)
}

func validateGitHubLimiterPolicy(policy GitHubLimiterPolicy) error {
	if policy.MaxConcurrent < 0 {
		return fmt.Errorf("github.max_concurrent must be 0 (unlimited) or positive")
	}
	if policy.MinInterval < 0 {
		return fmt.Errorf("github.min_interval must be 0 or positive")
	}
	if policy.BackoffBase < 0 {
		return fmt.Errorf("github.backoff_base must be 0 or positive")
	}
	if policy.BackoffMax < 0 {
		return fmt.Errorf("github.backoff_max must be 0 or positive")
	}
	return nil
}
