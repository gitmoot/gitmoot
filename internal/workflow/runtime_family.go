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
//
// SYNTHETIC AGENTS RESOLVE THROUGH THEIR PARENT (#2004). Temp and ephemeral
// agents are deliberately absent from the registry, so the three tiers above
// cannot name a family for them and the merge gate had to fall through to a
// name comparison rather than fail closed. Measured on review and implement
// jobs since 2026-08-25: 119 of 1,903 resolve through none of the three tiers,
// and 102 of those 119 are recoverable from a parent - 86 temp rows whose
// parent agent is in the registry (86/86) and 16 ephemeral rows whose parent
// job's agent is (16/16). The remaining 17 are 11 rows with no agent at all and
// 6 naming agents that are simply not registered, neither of which has a parent
// to recover.
func ResolveRuntimeFamily(ctx context.Context, store *db.Store, jobID string, agentName string, recordedRuntime string) (family string, ok bool, err error) {
	family, ok, err = resolveRuntimeFamilyDirect(ctx, store, jobID, agentName, recordedRuntime)
	if ok || err != nil {
		return family, ok, err
	}
	return resolveRuntimeFamilyViaParent(ctx, store, jobID, agentName)
}

// resolveRuntimeFamilyDirect is the three-tier resolution that does not leave
// the job it is asked about.
func resolveRuntimeFamilyDirect(ctx context.Context, store *db.Store, jobID string, agentName string, recordedRuntime string) (family string, ok bool, err error) {
	if recorded := normalizeRuntimeFamily(recordedRuntime); recorded != "" {
		return recorded, true, nil
	}
	// THE APPEND-ONLY TIER (#1534), consulted before the registry default and
	// after the payload field.
	//
	// Ordered that way on purpose. The payload field, when present, is written by
	// the same run-phase step that writes the event, so the two agree and reading
	// the payload first costs no query. When the payload field is ABSENT - 1,813
	// jobs on the live store carry a runtime event and no payload field - the
	// registry default is a GUESS about what the agent usually runs, and it is
	// measurably wrong: 56 review and implement jobs have an override event whose
	// runtime differs from their agent's registered default, so resolving them
	// through the registry attributes the execution to a family that did not run
	// it. Agent `lead` defaults to claude; several of those ran on codex.
	//
	// It reads the event COLUMN, never the message. #1534 forbids parsing the
	// prose, and this campaign has already filed a defect about a pattern that
	// silently matched nothing.
	name := strings.TrimSpace(agentName)
	if jobID != "" && name != "" && store != nil {
		recorded, readErr := store.JobRecordedRuntime(ctx, jobID, name)
		if readErr != nil {
			return "", false, readErr
		}
		if family := normalizeRuntimeFamily(recorded); family != "" {
			return family, true, nil
		}
	}
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

// maxSyntheticParentHops bounds the walk. A temp worker forked from an
// ephemeral delegation child is two hops, so four leaves headroom while making
// a cycle in parent_job_id terminate instead of hanging the caller.
const maxSyntheticParentHops = 4

// tempAgentInfix and ephemeralAgentInfix are the two synthetic shapes.
// daemon_worker.go mints `<parent-agent>-temp-<parent-job-id>`, so the parent
// AGENT is the prefix and needs no store read. ephemeralAgentName mints
// `<slug(delegation-id)>-ephemeral-<shortHash(parent-job+delegation)>`, which
// HASHES the parent job: nothing in that name recovers it, so the parent comes
// from the job row's parent_job_id column instead.
//
// #2004 as filed claimed the parent job is recoverable from the name in both
// shapes. That is true of temp and false of ephemeral; the hash is one-way. The
// column is the reason the ephemeral half works at all.
const (
	tempAgentInfix      = "-temp-"
	ephemeralAgentInfix = "-ephemeral-"
)

// splitTempAgentName recovers the parent agent from a temp worker's name.
// It splits at the FIRST infix because the prefix is a registered agent name
// and the suffix is a job id, which is where a second "-temp-" could appear.
func splitTempAgentName(name string) (parentAgent string, ok bool) {
	index := strings.Index(name, tempAgentInfix)
	if index <= 0 {
		return "", false
	}
	parent := strings.TrimSpace(name[:index])
	if parent == "" || parent == name {
		return "", false
	}
	return parent, true
}

// resolveRuntimeFamilyViaParent walks up from a synthetic agent to one that can
// name a family. Each hop resolves the parent through the same three tiers, so
// a parent that ran on an override attributes to what it actually ran rather
// than to its registered default.
func resolveRuntimeFamilyViaParent(ctx context.Context, store *db.Store, jobID string, agentName string) (string, bool, error) {
	if store == nil {
		return "", false, nil
	}
	name := strings.TrimSpace(agentName)
	currentJobID := strings.TrimSpace(jobID)
	for range maxSyntheticParentHops {
		parentAgent, parentJobID := "", ""
		if parent, ok := splitTempAgentName(name); ok {
			parentAgent = parent
		} else if strings.Contains(name, ephemeralAgentInfix) && currentJobID != "" {
			job, err := store.GetJob(ctx, currentJobID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return "", false, nil
				}
				return "", false, err
			}
			parentJobID = strings.TrimSpace(job.ParentJobID)
		}
		if parentAgent == "" && parentJobID == "" {
			// Not a synthetic name, or an ephemeral row with no parent recorded.
			// Either way there is nothing above this job to ask.
			return "", false, nil
		}
		if parentJobID != "" {
			parent, err := store.GetJob(ctx, parentJobID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return "", false, nil
				}
				return "", false, err
			}
			if parentJobID == currentJobID {
				// A self-parent would spin the walk against one row.
				return "", false, nil
			}
			parentAgent = strings.TrimSpace(parent.Agent)
			currentJobID = parentJobID
		} else {
			// A temp name identifies its parent agent but not a parent job whose
			// own recorded runtime we may read, so the next hop is name-only.
			currentJobID = ""
		}
		if parentAgent == "" {
			return "", false, nil
		}
		family, ok, err := resolveRuntimeFamilyDirect(ctx, store, currentJobID, parentAgent, "")
		if err != nil {
			return "", false, err
		}
		if ok {
			return family, true, nil
		}
		name = parentAgent
	}
	return "", false, nil
}

func normalizeRuntimeFamily(runtimeName string) string {
	return strings.ToLower(strings.TrimSpace(runtimeName))
}
