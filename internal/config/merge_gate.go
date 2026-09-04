package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultMinCIWait is retained for compatibility with existing merge_gate
// configuration. The mandatory native merge gate never treats zero external CI
// as green.
const DefaultMinCIWait = 60 * time.Second

// DefaultMaxCIWait is retained for compatibility with existing merge_gate
// configuration. The mandatory native merge gate never treats zero external CI
// as green.
const DefaultMaxCIWait = 10 * time.Minute

// MergeGatePolicy is the resolved merge-gate behavior for a repo.
type MergeGatePolicy struct {
	// AutoMerge permits Gitmoot's native task merge gate to merge a PR that has
	// an exact-head approval and green SHA-scoped CI. It defaults true; false is
	// an explicit operator kill-switch.
	AutoMerge bool
	// RequireExternalCI is retained for config compatibility. Exact-head external
	// CI is mandatory for native auto-merge regardless of this legacy value.
	RequireExternalCI bool
	// MinCIWait is retained for config compatibility. Default DefaultMinCIWait.
	MinCIWait time.Duration
	// MaxCIWait is retained for config compatibility. Default DefaultMaxCIWait.
	MaxCIWait time.Duration
}

// DefaultMergeGatePolicy permits native task merges only through the mandatory
// exact-head review and SHA-scoped CI gate.
func DefaultMergeGatePolicy() MergeGatePolicy {
	return MergeGatePolicy{AutoMerge: true, RequireExternalCI: false, MinCIWait: DefaultMinCIWait, MaxCIWait: DefaultMaxCIWait}
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
// file. A config with neither section uses the mandatory review-and-CI gate.
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
		if section, ok := sectionHeader(line); ok {
			if section == "" && malformedHeaderTargets(line, "merge_gate") {
				// A malformed merge_gate header must not fall through to
				// DefaultMergeGatePolicy: that default is the PERMISSIVE one
				// (auto-merge on, external CI not required), so a typo would
				// silently loosen the gate while returning a nil error.
				return MergeGateConfig{}, fmt.Errorf("parse merge_gate section: missing closing ]")
			}
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
