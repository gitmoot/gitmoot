package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultMinCIWait is the grace window the merge gate waits between two
// consecutive zero-external-CI observations at the same head before it concludes
// a repo has no CI (#596). It is deliberately a few GitHub-Actions run-creation
// latencies wide: the observed live race (issue #596) hit a ~4s window, so 60s
// leaves comfortable margin while a genuinely CI-less repo merges only one grace
// window later.
const DefaultMinCIWait = 60 * time.Second

// DefaultMaxCIWait is the upper bound the merge gate waits when `.github/workflows/`
// exists at the head but no external check ever appears (#596, layer 2). Past this
// window with the head unchanged and still zero external CI, the gate concludes
// no-CI and stamps the synthetic gitmoot/ci success instead of staying pending
// forever, so a PR whose workflows never trigger for it (docs-only under paths
// filters, tag-only / workflow_dispatch-only workflows, or a non-targeted branch)
// still merges. It is wide (GitHub Actions creates a check-run within seconds) yet
// finite to restore liveness.
const DefaultMaxCIWait = 10 * time.Minute

// MergeGatePolicy is the resolved merge-gate behavior for a repo. Native task
// auto-merge defaults off; when explicitly enabled, the historical no-CI handling
// still applies with its grace window and require_external_ci defaults false.
type MergeGatePolicy struct {
	// AutoMerge permits Gitmoot's native task merge gate to merge an approved PR.
	// It defaults false: leave the PR open for a human unless this is explicitly
	// enabled globally or for the repository.
	AutoMerge bool
	// RequireExternalCI, when true, HARD-BLOCKS a merge whose head reports zero
	// external CI (no external commit-statuses AND no check-runs) instead of ever
	// stamping the synthetic gitmoot/ci success. Use it for repos the operator
	// knows always have CI. Default false.
	RequireExternalCI bool
	// MinCIWait is the grace window between the first and second zero-external
	// observation at the same head before the gate concludes no-CI. Default
	// DefaultMinCIWait (60s).
	MinCIWait time.Duration
	// MaxCIWait is the upper bound the gate waits when `.github/workflows/` exists at
	// the head but no external check appears (#596, layer 2). Past it the gate
	// concludes no-CI instead of staying pending forever. Default DefaultMaxCIWait
	// (10m).
	MaxCIWait time.Duration
}

// DefaultMergeGatePolicy leaves native task PRs open for human merge. If an
// operator opts in, no external CI is not required and the no-CI conclusion is
// deferred by the default grace window (and bounded by the default max window).
func DefaultMergeGatePolicy() MergeGatePolicy {
	return MergeGatePolicy{AutoMerge: false, RequireExternalCI: false, MinCIWait: DefaultMinCIWait, MaxCIWait: DefaultMaxCIWait}
}

// MergeGateConfig is the parsed [merge_gate] configuration: a global default plus
// optional per-repo [repos."owner/repo".merge_gate] overrides (#596). It mirrors
// how [repos."owner/repo"] scopes RepoConcurrency — a repo with no override uses
// the global default, which itself defaults to DefaultMergeGatePolicy.
type MergeGateConfig struct {
	Global MergeGatePolicy
	repos  map[string]mergeGateOverride
}

// mergeGateOverride tracks which per-repo keys were explicitly set so an override
// merges onto the global default field-by-field (a missing key inherits the
// global value rather than resetting it to a zero value).
type mergeGateOverride struct {
	autoMerge         *bool
	requireExternalCI *bool
	minCIWait         *time.Duration
	maxCIWait         *time.Duration
}

// For resolves the effective policy for a repo: the global default with any
// per-repo override applied on top (#596). A repo with no override returns the
// global policy verbatim.
func (c MergeGateConfig) For(repo string) MergeGatePolicy {
	policy := c.Global
	if policy.MinCIWait <= 0 {
		policy.MinCIWait = DefaultMinCIWait
	}
	if policy.MaxCIWait <= 0 {
		policy.MaxCIWait = DefaultMaxCIWait
	}
	override, ok := c.repos[strings.TrimSpace(repo)]
	if !ok {
		return policy
	}
	if override.autoMerge != nil {
		policy.AutoMerge = *override.autoMerge
	}
	if override.requireExternalCI != nil {
		policy.RequireExternalCI = *override.requireExternalCI
	}
	if override.minCIWait != nil {
		policy.MinCIWait = *override.minCIWait
	}
	if override.maxCIWait != nil {
		policy.MaxCIWait = *override.maxCIWait
	}
	return policy
}

// LoadMergeGatePolicy parses the [merge_gate] section (global) and every
// [repos."owner/repo".merge_gate] section (per-repo override) from the config
// file. It is safe by default: a config with neither section leaves every native
// task PR open for a human merge.
//
// It reuses the same naive line-scanner shape as LoadRepoConcurrency /
// LoadAdmissionPolicy. Unrelated sections are ignored.
func LoadMergeGatePolicy(paths Paths) (MergeGateConfig, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return MergeGateConfig{}, err
	}
	cfg := MergeGateConfig{Global: DefaultMergeGatePolicy(), repos: map[string]mergeGateOverride{}}
	// section is "" for the global [merge_gate], a repo full name for a per-repo
	// override, and unset (inSection=false) for any other section.
	var repo string
	inSection := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			repo, inSection = parseMergeGateSection(section)
			if inSection && repo != "" {
				if _, ok := cfg.repos[repo]; !ok {
					cfg.repos[repo] = mergeGateOverride{}
				}
			}
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if repo == "" {
			if err := applyMergeGateGlobalField(&cfg.Global, key, value); err != nil {
				return MergeGateConfig{}, fmt.Errorf("parse [merge_gate].%s: %w", key, err)
			}
			continue
		}
		override := cfg.repos[repo]
		if err := applyMergeGateOverrideField(&override, key, value); err != nil {
			return MergeGateConfig{}, fmt.Errorf("parse [repos.%q.merge_gate].%s: %w", repo, key, err)
		}
		cfg.repos[repo] = override
	}
	if err := validateMergeGatePolicy("[merge_gate]", cfg.Global); err != nil {
		return MergeGateConfig{}, err
	}
	for name := range cfg.repos {
		if err := validateMergeGatePolicy(fmt.Sprintf("[repos.%q.merge_gate]", name), cfg.For(name)); err != nil {
			return MergeGateConfig{}, err
		}
	}
	return cfg, nil
}

// parseMergeGateSection classifies a section header. It returns ("", true) for the
// global [merge_gate], (repo, true) for a per-repo [repos."owner/repo".merge_gate],
// and ("", false) for any other section (which the loader ignores).
func parseMergeGateSection(section string) (string, bool) {
	section = strings.TrimSpace(section)
	if section == "merge_gate" {
		return "", true
	}
	if !strings.HasPrefix(section, "repos.") || !strings.HasSuffix(section, ".merge_gate") {
		return "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(section, "repos."), ".merge_gate")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	if strings.HasPrefix(rest, "\"") {
		unquoted, err := strconv.Unquote(rest)
		if err != nil || strings.TrimSpace(unquoted) == "" {
			return "", false
		}
		return strings.TrimSpace(unquoted), true
	}
	return rest, true
}

func applyMergeGateGlobalField(policy *MergeGatePolicy, key string, value string) error {
	switch key {
	case "auto_merge":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		policy.AutoMerge = parsed
		return nil
	case "require_external_ci":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		policy.RequireExternalCI = parsed
		return nil
	case "min_ci_wait":
		parsed, err := parseConfigDuration(value)
		if err != nil {
			return err
		}
		policy.MinCIWait = parsed
		return nil
	case "max_ci_wait":
		parsed, err := parseConfigDuration(value)
		if err != nil {
			return err
		}
		policy.MaxCIWait = parsed
		return nil
	default:
		return nil
	}
}

func applyMergeGateOverrideField(override *mergeGateOverride, key string, value string) error {
	switch key {
	case "auto_merge":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		override.autoMerge = &parsed
		return nil
	case "require_external_ci":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		override.requireExternalCI = &parsed
		return nil
	case "min_ci_wait":
		parsed, err := parseConfigDuration(value)
		if err != nil {
			return err
		}
		override.minCIWait = &parsed
		return nil
	case "max_ci_wait":
		parsed, err := parseConfigDuration(value)
		if err != nil {
			return err
		}
		override.maxCIWait = &parsed
		return nil
	default:
		return nil
	}
}

func validateMergeGatePolicy(label string, policy MergeGatePolicy) error {
	if policy.MinCIWait < 0 {
		return fmt.Errorf("%s: min_ci_wait must be a non-negative duration", label)
	}
	if policy.MaxCIWait < 0 {
		return fmt.Errorf("%s: max_ci_wait must be a non-negative duration", label)
	}
	return nil
}
