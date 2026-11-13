package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// AdmissionPolicy is the host-level, opt-in concurrency admission budget read
// from the [admission] section of the gitmoot config (issue #365). It is a
// process-global, memory-aware second gate the daemon applies BEFORE dispatching
// each session job, on top of --workers/pool and the per-repo checkout /
// runtime-session locks. With both caps 0 (the default) it is OFF and daemon
// scheduling is byte-identical to today.
//
// Scope: the budget is enforced per daemon process. The registered-repo
// supervisor runs ONE worker across all enabled repos, so a process-global
// counter is host-global for the normal single-daemon deployment. Multiple
// `daemon start --repo` processes each get their own in-memory budget — a
// documented limitation; a DB-backed cross-process gauge is a follow-up.
type AdmissionPolicy struct {
	// MaxConcurrentSessions caps the total number of in-flight session jobs
	// admitted across all repos in the daemon process. 0 (the default) means
	// off — the session-count gate is not applied.
	MaxConcurrentSessions int
	// MaxMemoryGB caps the sum of the per-runtime RAM estimates of all in-flight
	// session jobs. 0 (the default) means off — the memory gate is not applied.
	MaxMemoryGB float64
	// DefaultMemoryGB is the RAM estimate (in GB) for a session job. A job whose
	// runtime has no resumable session (the runtime session lock is also skipped
	// for it) contributes 0 and is not session-counted. The per-runtime
	// codex/claude/kimi priors went with those runtimes (#2587); their keys are
	// ignored if an old config still sets them.
	DefaultMemoryGB float64
}

// DefaultAdmissionPolicy returns the off-by-default policy: both caps 0
// (disabled) with the documented per-runtime RAM priors as defaults.
func DefaultAdmissionPolicy() AdmissionPolicy {
	return AdmissionPolicy{
		MaxConcurrentSessions: 0,
		MaxMemoryGB:           0,
		DefaultMemoryGB:       0.5,
	}
}

// Enabled reports whether either admission gate is active. When false the daemon
// skips admission accounting entirely (byte-identical default behavior).
func (p AdmissionPolicy) Enabled() bool {
	return p.MaxConcurrentSessions > 0 || p.MaxMemoryGB > 0
}

func LoadAdmissionPolicy(paths Paths) (AdmissionPolicy, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return AdmissionPolicy{}, err
	}
	policy := DefaultAdmissionPolicy()
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			if section == "" && malformedHeaderTargets(line, "admission") {
				// Falling back to the zero policy makes Enabled() false, which
				// makes the daemon skip admission accounting entirely.
				return AdmissionPolicy{}, fmt.Errorf("parse admission section: missing closing ]")
			}
			current = section == "admission"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applyAdmissionPolicyField(&policy, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return AdmissionPolicy{}, fmt.Errorf("parse [admission].%s: %w", strings.TrimSpace(key), err)
		}
	}
	if err := validateAdmissionPolicy(policy); err != nil {
		return AdmissionPolicy{}, err
	}
	return policy, nil
}

func applyAdmissionPolicyField(policy *AdmissionPolicy, key string, value string) error {
	switch key {
	case "max_concurrent_sessions":
		parsed, err := strconv.Atoi(value)
		policy.MaxConcurrentSessions = parsed
		return err
	case "max_memory_gb":
		parsed, err := strconv.ParseFloat(value, 64)
		policy.MaxMemoryGB = parsed
		return err
	case "default_memory_gb":
		parsed, err := strconv.ParseFloat(value, 64)
		policy.DefaultMemoryGB = parsed
		return err
	default:
		return nil
	}
}

func validateAdmissionPolicy(policy AdmissionPolicy) error {
	if policy.MaxConcurrentSessions < 0 {
		return fmt.Errorf("admission.max_concurrent_sessions must be 0 (off) or positive")
	}
	if policy.MaxMemoryGB < 0 {
		return fmt.Errorf("admission.max_memory_gb must be 0 (off) or positive")
	}
	if policy.DefaultMemoryGB < 0 {
		return fmt.Errorf("admission.default_memory_gb must be 0 or positive")
	}
	return nil
}
