package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// PRStalledPolicy is the [pr_stalled] section (#2677): the opt-in daemon sweep
// that flags open pull requests stuck between steps and alerts their seat.
type PRStalledPolicy struct {
	// Enabled turns the daemon sweep on. Default false. `gitmoot pr stalled`
	// evaluates on demand either way.
	Enabled bool
	// Interval is how often one repository is evaluated (one GraphQL query).
	Interval time.Duration
	// The five state thresholds.
	NoCIAfter           time.Duration
	CIRedAfter          time.Duration
	AwaitingReviewAfter time.Duration
	GreenNotMergedAfter time.Duration
	DraftStaleAfter     time.Duration
	// Repos, when set, limits the sweep to these repositories; SkipRepos
	// excludes repositories. Both take org-scope patterns: owner/repo,
	// owner/* or *.
	Repos     []string
	SkipRepos []string
}

func DefaultPRStalledPolicy() PRStalledPolicy {
	return PRStalledPolicy{
		Interval:            30 * time.Minute,
		NoCIAfter:           30 * time.Minute,
		CIRedAfter:          6 * time.Hour,
		AwaitingReviewAfter: time.Hour,
		GreenNotMergedAfter: 2 * time.Hour,
		DraftStaleAfter:     12 * time.Hour,
	}
}

// Covers reports whether the sweep evaluates repo.
func (p PRStalledPolicy) Covers(repo string) bool {
	if len(p.Repos) > 0 && !ScopeMatches(p.Repos, repo) {
		return false
	}
	return !ScopeMatches(p.SkipRepos, repo)
}

// LoadPRStalledPolicy reads [pr_stalled]. A missing file or section is the
// default (disabled). Unknown keys and invalid values are refused, and so is a
// malformed [pr_stalled header, so a typo cannot silently change thresholds.
func LoadPRStalledPolicy(paths Paths) (PRStalledPolicy, error) {
	policy := DefaultPRStalledPolicy()
	content, err := os.ReadFile(paths.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return policy, nil
	}
	if err != nil {
		return PRStalledPolicy{}, err
	}
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			if section == "" && malformedHeaderTargets(line, "pr_stalled") {
				return PRStalledPolicy{}, fmt.Errorf("malformed section header %q", line)
			}
			current = section == "pr_stalled"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return PRStalledPolicy{}, fmt.Errorf("parse [pr_stalled]: expected key = value, got %q", line)
		}
		key = strings.TrimSpace(key)
		if err := applyPRStalledField(&policy, key, strings.TrimSpace(value)); err != nil {
			return PRStalledPolicy{}, fmt.Errorf("parse [pr_stalled].%s: %w", key, err)
		}
	}
	return policy, nil
}

func applyPRStalledField(policy *PRStalledPolicy, key, value string) error {
	durations := map[string]*time.Duration{
		"interval":               &policy.Interval,
		"no_ci_after":            &policy.NoCIAfter,
		"ci_red_after":           &policy.CIRedAfter,
		"awaiting_review_after":  &policy.AwaitingReviewAfter,
		"green_not_merged_after": &policy.GreenNotMergedAfter,
		"draft_stale_after":      &policy.DraftStaleAfter,
	}
	if target, ok := durations[key]; ok {
		parsed, err := parseConfigDuration(value)
		if err != nil {
			return err
		}
		if parsed <= 0 {
			return fmt.Errorf("must be a positive duration")
		}
		if key == "interval" && parsed < time.Minute {
			return fmt.Errorf("must be at least 1m")
		}
		*target = parsed
		return nil
	}
	switch key {
	case "enabled":
		parsed, err := parseConfigBool(value)
		policy.Enabled = parsed
		return err
	case "repos", "skip_repos":
		patterns, err := parseConfigStringArray(value)
		if err != nil {
			return err
		}
		for i, pattern := range patterns {
			normalized, err := normalizeOrgScope(pattern)
			if err != nil {
				return fmt.Errorf("%w; use owner/repo, owner/* or *", err)
			}
			patterns[i] = normalized
		}
		if key == "repos" {
			policy.Repos = patterns
		} else {
			policy.SkipRepos = patterns
		}
		return nil
	default:
		return fmt.Errorf("unknown key")
	}
}
