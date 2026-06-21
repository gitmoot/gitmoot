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
}

func DefaultOrchestratePolicy() OrchestratePolicy {
	return OrchestratePolicy{
		CockpitMode:     CockpitModeAuto,
		CockpitSession:  "",
		CockpitMaxPanes: 4,
		CockpitPaneKey:  CockpitPaneKeyJob,
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
	return nil
}
