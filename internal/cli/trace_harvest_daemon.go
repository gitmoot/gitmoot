package cli

import (
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// daemonOutcomeHarvester returns the best-effort Mode-A trace-harvester for this
// home, or nil when [skillopt].auto_trace_enabled is OFF (the default, or any
// config load failure — fail-safe to disabled so a malformed config never starts
// harvesting). When nil, the engine constructs no Outcome and calls no Harvest,
// so daemon behavior and every human-run TrainingPackage stay byte-identical
// (#465). It mirrors daemonEventSink's off-by-default admission gate.
//
// Unlike the webhook sink, the harvester owns no goroutine and holds only a store
// + a read-only GitHub status reader, so it is constructed per engine without
// caching — the gate read is the only cost, and that only matters once enabled.
func daemonOutcomeHarvester(store *db.Store, gh github.Client, home string) workflow.OutcomeHarvester {
	if store == nil {
		return nil
	}
	policy, err := loadSkillOptPolicy(home)
	if err != nil || !policy.Enabled() {
		return nil
	}
	return skillopt.NewOutcomeHarvester(store, gh)
}

// loadSkillOptPolicy resolves the [skillopt] policy for a home, fail-safe to the
// disabled default when the home or config cannot be resolved/parsed so the
// trace-harvester stays OFF rather than erroring the daemon (mirrors
// loadEventsPolicy / the #446 fail-safe-to-disabled pattern).
func loadSkillOptPolicy(home string) (config.SkillOptPolicy, error) {
	cfg := resolveConfigFile(home)
	if cfg == "" {
		return config.DefaultSkillOptPolicy(), nil
	}
	return config.LoadSkillOptPolicy(config.Paths{ConfigFile: cfg})
}
