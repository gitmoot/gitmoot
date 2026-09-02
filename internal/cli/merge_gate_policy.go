package cli

import (
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func applyResolvedMergeGatePolicy(gate *workflow.PolicyMergeGate, policy config.MergeGatePolicy) {
	gate.AutoMerge = policy.AutoMerge
	gate.RequireExternalCI = policy.RequireExternalCI
	gate.MinCIWait = policy.MinCIWait
	gate.MaxCIWait = policy.MaxCIWait
}

// resolvedMergeGatePolicy is the single config path for the native merge gate
// and pipeline auto-merge executor, including per-repo overrides.
func resolvedMergeGatePolicy(home string, repo string) (config.MergeGatePolicy, bool) {
	cfg := resolveConfigFile(home)
	if cfg == "" {
		return config.MergeGatePolicy{}, false
	}
	loaded, err := config.LoadMergeGatePolicy(config.Paths{ConfigFile: cfg})
	if err != nil {
		return config.MergeGatePolicy{}, false
	}
	return loaded.For(repo), true
}

// autoMergeEnabledResolver re-reads the merge policy on every daemon poll so an
// operator can explicitly re-arm auto-merge-disabled parked tasks without a
// daemon restart. Invalid or unreadable configuration fails closed.
func autoMergeEnabledResolver(home string) func(repo string) bool {
	return func(repo string) bool {
		policy, ok := resolvedMergeGatePolicy(home, repo)
		return ok && policy.AutoMerge
	}
}
