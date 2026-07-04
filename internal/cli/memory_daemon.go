package cli

import (
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
	if len(enrolled) == 0 {
		return nil
	}
	return &workflow.MemoryController{
		Store:       store,
		Enabled:     func(name string) bool { return enrolled[name] },
		TokenBudget: settings.TokenBudget,
		MaxEntries:  settings.MaxEntries,
	}
}

// memoryPathsForHome resolves config paths for a home the same way the other
// home-scoped seams do (config.PathsForHome when a home is given, else the
// default paths), so it works both under GITMOOT_HOME isolation and on the live
// home.
func memoryPathsForHome(home string) (config.Paths, error) {
	if home != "" {
		return config.PathsForHome(home), nil
	}
	return config.DefaultPaths()
}
