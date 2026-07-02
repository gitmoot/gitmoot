package cli

import (
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// applyMergeGatePolicy loads the [merge_gate] policy for `home` and applies the
// per-repo resolved knobs (require_external_ci, min_ci_wait, max_ci_wait) onto a
// constructed merge gate (#596). It is fail-safe: an empty home, a missing config,
// or a parse error leaves the gate at its off-by-default behavior (no external CI
// required, built-in grace/max windows) rather than erroring the daemon —
// mirroring how loadEventsPolicy / resolveEscalationTTL degrade to defaults.
func applyMergeGatePolicy(gate *workflow.PolicyMergeGate, home string, repo string) {
	cfg := resolveConfigFile(home)
	if cfg == "" {
		return
	}
	loaded, err := config.LoadMergeGatePolicy(config.Paths{ConfigFile: cfg})
	if err != nil {
		return
	}
	policy := loaded.For(repo)
	gate.RequireExternalCI = policy.RequireExternalCI
	gate.MinCIWait = policy.MinCIWait
	gate.MaxCIWait = policy.MaxCIWait
}
