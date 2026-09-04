package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	ParallelSessionQueue           = "queue"
	ParallelSessionForkTempSession = "fork_temp_session"

	ParallelSessionMergeBackOff     = "off"
	ParallelSessionMergeBackSummary = "summary"
)

type ParallelSessionPolicy struct {
	SameSession             string
	MergeBack               string
	MaxTempSessionsPerAgent int
	EligibleActions         []string
}

func DefaultParallelSessionPolicy() ParallelSessionPolicy {
	return ParallelSessionPolicy{
		SameSession:             ParallelSessionForkTempSession,
		MergeBack:               ParallelSessionMergeBackSummary,
		MaxTempSessionsPerAgent: 4,
		EligibleActions:         []string{"ask", "review", "implement"},
	}
}

func LoadParallelSessionPolicy(paths Paths) (ParallelSessionPolicy, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return ParallelSessionPolicy{}, err
	}
	policy := DefaultParallelSessionPolicy()
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			current = section == "parallel_sessions"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applyParallelSessionPolicyField(&policy, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return ParallelSessionPolicy{}, fmt.Errorf("parse [parallel_sessions].%s: %w", strings.TrimSpace(key), err)
		}
	}
	if err := validateParallelSessionPolicy(policy); err != nil {
		return ParallelSessionPolicy{}, err
	}
	return policy, nil
}

func applyParallelSessionPolicyField(policy *ParallelSessionPolicy, key string, value string) error {
	switch key {
	case "same_session":
		parsed, err := parseConfigString(value)
		policy.SameSession = strings.TrimSpace(parsed)
		return err
	case "merge_back":
		parsed, err := parseConfigString(value)
		policy.MergeBack = strings.TrimSpace(parsed)
		return err
	case "max_temp_sessions_per_agent":
		parsed, err := strconv.Atoi(value)
		policy.MaxTempSessionsPerAgent = parsed
		return err
	case "eligible_actions":
		parsed, err := parseConfigStringArray(value)
		policy.EligibleActions = compactConfigStrings(parsed)
		return err
	default:
		return nil
	}
}

func validateParallelSessionPolicy(policy ParallelSessionPolicy) error {
	switch policy.SameSession {
	case ParallelSessionQueue, ParallelSessionForkTempSession:
	default:
		return fmt.Errorf("unsupported parallel_sessions.same_session %q; use queue or fork_temp_session", policy.SameSession)
	}
	switch policy.MergeBack {
	case ParallelSessionMergeBackOff, ParallelSessionMergeBackSummary:
	default:
		return fmt.Errorf("unsupported parallel_sessions.merge_back %q; use off or summary", policy.MergeBack)
	}
	if policy.MaxTempSessionsPerAgent < 1 {
		return fmt.Errorf("parallel_sessions.max_temp_sessions_per_agent must be positive")
	}
	for _, action := range policy.EligibleActions {
		switch strings.TrimSpace(action) {
		case "ask", "review", "implement":
		default:
			return fmt.Errorf("unsupported parallel_sessions.eligible_actions value %q; use ask, review, or implement", action)
		}
	}
	return nil
}
