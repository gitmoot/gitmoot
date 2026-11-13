package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// unsupportedAgentRuntimes names every registered agent and configured agent
// type still on a removed job runtime (codex, claude, kimi; #2587), sorted, as
// one message each. Those rows load and list normally; their jobs are refused
// at adapter construction, so reporting them is how an operator finds what to
// re-register on omp. Ephemeral spawn-from-spec workers are skipped like in
// `agent list`.
func unsupportedAgentRuntimes(agents []db.Agent, types map[string]config.AgentType) []string {
	var messages []string
	for _, agent := range agents {
		if isEphemeralAgentName(agent.Name) || !runtime.IsRemovedRuntime(agent.Runtime) {
			continue
		}
		messages = append(messages, runtime.UnsupportedAgentRuntimeError(agent.Name, agent.Runtime).Error())
	}
	for name, agentType := range types {
		if !runtime.IsRemovedRuntime(agentType.Runtime) {
			continue
		}
		messages = append(messages, runtime.UnsupportedAgentRuntimeError("type "+name, agentType.Runtime).Error())
	}
	sort.Strings(messages)
	return messages
}

// unsupportedAgentRuntimeDoctorCheck reports agents on a removed runtime. It is
// best-effort like the other store-backed doctor checks: no initialized home
// yields ok=false. A config file that fails to parse is reported by the
// commands that need it, so only the store rows are reported then.
func unsupportedAgentRuntimeDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	if strings.TrimSpace(paths.Database) == "" {
		return doctor.Check{}, false
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return doctor.Check{}, false
	}
	defer store.Close()
	agents, err := store.ListAgents(context.Background())
	if err != nil {
		return doctor.Check{}, false
	}
	types, _ := config.LoadAgentTypes(paths)
	return buildUnsupportedAgentRuntimeCheck(unsupportedAgentRuntimes(agents, types)), true
}

func buildUnsupportedAgentRuntimeCheck(messages []string) doctor.Check {
	if len(messages) == 0 {
		return doctor.Check{Name: "agent runtimes", OK: true, Detail: "every agent runs on a supported runtime (omp, shell)"}
	}
	return doctor.Check{
		Name:   "agent runtimes",
		Detail: fmt.Sprintf("%d unsupported: %s", len(messages), strings.Join(messages, "; ")),
	}
}
