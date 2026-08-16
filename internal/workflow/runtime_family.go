package workflow

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// ResolveRuntimeFamily resolves the runtime family an agent's job ran on — or,
// for a job not yet dispatched, would run on. This is the ONE resolver shared
// by the review-loop guard (#1528) and, in a later round, the merge gate's
// independence check (#1531); do not grow a second copy.
//
// Precedence: a runtime recorded on the job itself (payload effective_runtime)
// wins over the agent registry default, so an override-run job attributes
// correctly even after the agent's registered default later changes. The
// registry default (agents.runtime, with GetAgent's agent_instances fallback)
// covers jobs that predate the #1528 recording.
//
// ok is false when neither source can name a family (agent absent from the
// registry with nothing recorded, or an empty registry runtime). Callers
// protecting a safety property MUST treat !ok as fail-closed: an unknown
// family that silently counted as "new" would convert the guard into a way to
// add unlimited reviews.
func ResolveRuntimeFamily(ctx context.Context, store *db.Store, agentName string, recordedRuntime string) (family string, ok bool, err error) {
	if recorded := normalizeRuntimeFamily(recordedRuntime); recorded != "" {
		return recorded, true, nil
	}
	name := strings.TrimSpace(agentName)
	if name == "" {
		return "", false, nil
	}
	agent, err := store.GetAgent(ctx, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	if family := normalizeRuntimeFamily(agent.Runtime); family != "" {
		return family, true, nil
	}
	return "", false, nil
}

func normalizeRuntimeFamily(runtimeName string) string {
	return strings.ToLower(strings.TrimSpace(runtimeName))
}
