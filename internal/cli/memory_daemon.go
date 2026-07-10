package cli

import (
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// daemonMemoryController resolves the off-by-default agent persistent-memory
// controller (#626) for a home, or nil when memory is entirely off for this box.
// It returns nil — so Engine.Memory stays nil and the Mailbox is built with nil
// memory hooks, byte-identical — whenever:
//   - config cannot be loaded (fail-safe to disabled), OR
//   - the global [memory].disabled kill switch is set, OR
//   - NO agent has [agents.<name>].memory = true (nothing to read or write for).
//
// When at least one agent is enrolled, it constructs a controller whose Enabled
// closure reports true only for those enrolled agents, folding in the global
// kill switch. This mirrors the other off-by-default daemon seams
// (daemonReviewLegDispatcher etc.): one resolution point, fail-safe to off.
func daemonMemoryController(store *db.Store, home string) *workflow.MemoryController {
	if store == nil {
		return nil
	}
	paths, err := memoryPathsForHome(home)
	if err != nil {
		return nil
	}
	settings, err := config.LoadMemorySettings(paths)
	if err != nil || settings.Disabled {
		return nil
	}
	agentTypes, err := config.LoadAgentTypes(paths)
	if err != nil {
		return nil
	}
	enrolled := make(map[string]bool)
	for name, entry := range agentTypes {
		if entry.Memory {
			enrolled[name] = true
		}
	}
	// With no enrolled agent there is normally nothing to read or write for, so the
	// controller stays nil (byte-identical). The exceptions are box-wide distill
	// producers: distill_at_terminal + distill_all_jobs stages failure signal for
	// EVERY job, and distill_successes + distill_all_jobs stages recovered-failure
	// success observations the same way. All keys are off by default, so this
	// widening is inert unless explicitly enabled.
	if len(enrolled) == 0 && !((settings.DistillAtTerminal || settings.DistillSuccesses) && settings.DistillAllJobs) {
		return nil
	}
	return &workflow.MemoryController{
		Store:             store,
		Enabled:           func(name string) bool { return enrolled[name] },
		TokenBudget:       settings.TokenBudget,
		MaxEntries:        settings.MaxEntries,
		DistillAtTerminal: settings.DistillAtTerminal,
		DistillSuccesses:  settings.DistillSuccesses,
		DistillMaxPerJob:  settings.DistillMaxPerJob,
		DistillAllJobs:    settings.DistillAllJobs,
	}
}

// memoryPathsForHome resolves the config paths for a home the SAME dual-mode way
// the other home-scoped daemon seams do (via resolveConfigFile), so it works both
// when handed the RAW --home and when handed the already-RESOLVED <home>/.gitmoot
// root. The daemon wiring path (defaultWorkflow -> daemonWorkflowEngine ->
// daemonMemoryController) passes w.workflowHome(), which is config.Paths.Home —
// i.e. <home>/.gitmoot — so a naive config.PathsForHome(home) here re-appended
// ".gitmoot" a SECOND time and read a phantom <home>/.gitmoot/.gitmoot/config.toml
// with no [memory]/[agents.*] section, leaving daemonMemoryController nil and
// silently disabling memory for EVERY enrolled agent through the live daemon
// (the same #446/#459 double-resolution the [events] seam was fixed for). Only
// daemonMemoryController's two config loaders (LoadMemorySettings/LoadAgentTypes)
// read the returned Paths, and both use only ConfigFile, so resolving that one
// field is sufficient.
func memoryPathsForHome(home string) (config.Paths, error) {
	if strings.TrimSpace(home) == "" {
		return config.DefaultPaths()
	}
	return config.Paths{ConfigFile: resolveConfigFile(home)}, nil
}
