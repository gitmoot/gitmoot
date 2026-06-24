package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	// CockpitMode values for [orchestrate].cockpit_mode. "auto" lets the daemon
	// decide per job (gated on herdr reachability), "on" forces panes when herdr
	// is reachable, and "off" disables cockpit panes entirely regardless of the
	// per-job --cockpit flag.
	CockpitModeAuto = "auto"
	CockpitModeOn   = "on"
	CockpitModeOff  = "off"

	// CockpitPaneKey values for [orchestrate].cockpit_pane_key. "job" gives each
	// job its own pane (P0); "seat" reuses one pane per opaque seat key (P2).
	CockpitPaneKeyJob  = "job"
	CockpitPaneKeySeat = "seat"
)

// OrchestratePolicy is the host-level cockpit policy read from the
// [orchestrate] section of the gitmoot config. The daemon combines it with the
// per-job JobPayload.Cockpit flag to decide whether to wrap a job's delivery in
// a herdr pane (see issue #357). It never affects engine/DAG behavior.
type OrchestratePolicy struct {
	CockpitMode     string
	CockpitSession  string
	CockpitMaxPanes int
	CockpitPaneKey  string
	// InlineArtifactBodies opts the coordinator continuation prompt into inlining
	// each finished child's artifact_body as a fenced block (see issue #368). It is
	// off by default because inlined briefs can be large.
	InlineArtifactBodies bool
	// InlineArtifactMaxBytes is the per-body byte cap applied when
	// InlineArtifactBodies is set; 0 means the engine's built-in default.
	InlineArtifactMaxBytes int
	// MaxDelegationTokenBudget is the cumulative per-root token budget (input +
	// output, summed across a delegation tree) that bounds a tree by cost in
	// addition to depth/width/total-jobs/wall-clock (#338 Part B). 0 (the default)
	// means unlimited/off, so default behavior is unchanged. The daemon wires this
	// into Engine.MaxDelegationTokenBudget at startup.
	MaxDelegationTokenBudget int
	// MaxDelegationCostUSD is the cumulative per-root dollar-cost budget that bounds
	// a delegation tree by its measured spend (token usage × a per-model price
	// table), layered on top of the token budget (#380). 0 (the default) means
	// unlimited/off, so default behavior is unchanged. The daemon wires this into
	// Engine.MaxDelegationCostUSD at startup.
	MaxDelegationCostUSD float64
	// MaxDelegationNonProgressStreak is the per-root threshold for the result-aware
	// non-progress loop detector (#339): how many consecutive continuation
	// generations a delegation tree may produce with no new durable side effect
	// before the loop ladder trips. 0 (the default) means use the engine's built-in
	// default (2), so default behavior is unchanged. The daemon wires this into
	// Engine.MaxDelegationNonProgressStreak at startup.
	MaxDelegationNonProgressStreak int
}

func DefaultOrchestratePolicy() OrchestratePolicy {
	return OrchestratePolicy{
		CockpitMode:                    CockpitModeAuto,
		CockpitSession:                 "",
		CockpitMaxPanes:                4,
		CockpitPaneKey:                 CockpitPaneKeyJob,
		InlineArtifactBodies:           false,
		InlineArtifactMaxBytes:         0,
		MaxDelegationTokenBudget:       0,
		MaxDelegationCostUSD:           0,
		MaxDelegationNonProgressStreak: 0,
	}
}

func LoadOrchestratePolicy(paths Paths) (OrchestratePolicy, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return OrchestratePolicy{}, err
	}
	policy := DefaultOrchestratePolicy()
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = strings.TrimSpace(section) == "orchestrate"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applyOrchestratePolicyField(&policy, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return OrchestratePolicy{}, fmt.Errorf("parse [orchestrate].%s: %w", strings.TrimSpace(key), err)
		}
	}
	if err := validateOrchestratePolicy(policy); err != nil {
		return OrchestratePolicy{}, err
	}
	return policy, nil
}

func applyOrchestratePolicyField(policy *OrchestratePolicy, key string, value string) error {
	switch key {
	case "cockpit_mode":
		parsed, err := parseConfigString(value)
		policy.CockpitMode = strings.TrimSpace(parsed)
		return err
	case "cockpit_session":
		parsed, err := parseConfigString(value)
		policy.CockpitSession = strings.TrimSpace(parsed)
		return err
	case "cockpit_max_panes":
		parsed, err := strconv.Atoi(value)
		policy.CockpitMaxPanes = parsed
		return err
	case "cockpit_pane_key":
		parsed, err := parseConfigString(value)
		policy.CockpitPaneKey = strings.TrimSpace(parsed)
		return err
	case "inline_artifact_bodies":
		parsed, err := strconv.ParseBool(value)
		policy.InlineArtifactBodies = parsed
		return err
	case "inline_artifact_max_bytes":
		parsed, err := strconv.Atoi(value)
		policy.InlineArtifactMaxBytes = parsed
		return err
	case "max_delegation_token_budget":
		parsed, err := strconv.Atoi(value)
		policy.MaxDelegationTokenBudget = parsed
		return err
	case "max_delegation_cost_usd":
		parsed, err := strconv.ParseFloat(value, 64)
		policy.MaxDelegationCostUSD = parsed
		return err
	case "max_delegation_non_progress_streak":
		parsed, err := strconv.Atoi(value)
		policy.MaxDelegationNonProgressStreak = parsed
		return err
	default:
		return nil
	}
}

func validateOrchestratePolicy(policy OrchestratePolicy) error {
	switch policy.CockpitMode {
	case CockpitModeAuto, CockpitModeOn, CockpitModeOff:
	default:
		return fmt.Errorf("unsupported orchestrate.cockpit_mode %q; use on, off, or auto", policy.CockpitMode)
	}
	if policy.CockpitMaxPanes < 1 {
		return fmt.Errorf("orchestrate.cockpit_max_panes must be positive")
	}
	switch policy.CockpitPaneKey {
	case CockpitPaneKeyJob, CockpitPaneKeySeat:
	default:
		return fmt.Errorf("unsupported orchestrate.cockpit_pane_key %q; use job or seat", policy.CockpitPaneKey)
	}
	if policy.MaxDelegationTokenBudget < 0 {
		return fmt.Errorf("orchestrate.max_delegation_token_budget must be 0 (unlimited) or positive")
	}
	if policy.MaxDelegationCostUSD < 0 {
		return fmt.Errorf("orchestrate.max_delegation_cost_usd must be 0 (unlimited) or positive")
	}
	if policy.MaxDelegationNonProgressStreak < 0 {
		return fmt.Errorf("orchestrate.max_delegation_non_progress_streak must be 0 (engine default) or positive")
	}
	return nil
}
