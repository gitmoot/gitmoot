package workflow

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

const ompProviderVerifiedEventKind = "omp_provider_verified"

// ResolveRuntimeFamily resolves the runtime family an agent's job ran on — or,
// for a native job not yet dispatched, would run on. Prospective OMP jobs remain
// unresolved because their provider is known only after execution. This is the
// ONE resolver shared by the review-loop guard (#1528) and merge gate's
// independence check (#1531); do not grow a second copy.
//
// Precedence: a runtime recorded on the job itself (payload effective_runtime)
// wins over the agent registry default, so an override-run job attributes
// correctly even after the agent's registered default later changes. The
// registry default (agents.runtime, with GetAgent's agent_instances fallback)
// covers jobs that predate the #1528 recording.
//
// ok is false when no trusted source can name a family: an absent/unregistered
// agent, an empty native runtime, or an OMP job without successful provider
// evidence. Callers protecting a safety property MUST treat !ok as fail-closed:
// an unknown family that silently counted as "new" would convert the guard into
// a way to add unlimited reviews.
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
	family, ok, resolvedRuntime, err := resolveRuntimeFamilyDirect(ctx, store, jobID, agentName, recordedRuntime)
	if ok || err != nil || resolvedRuntime {
		return family, ok, err
	}
	return resolveRuntimeFamilyViaParent(ctx, store, jobID, agentName)
}

// resolveRuntimeFamilyDirect resolves only the job it is asked about.
// resolvedRuntime distinguishes "no runtime evidence; parent recovery may help"
// from "OMP ran, but its successful upstream-provider evidence is missing".
// The latter is terminal: inheriting a parent's provider would manufacture
// execution evidence for a different job.
func resolveRuntimeFamilyDirect(ctx context.Context, store *db.Store, jobID string, agentName string, recordedRuntime string) (family string, ok bool, resolvedRuntime bool, err error) {
	runtimeName, ok, err := resolveRuntimeNameDirect(ctx, store, jobID, agentName, recordedRuntime)
	if err != nil || !ok {
		return "", false, false, err
	}
	if runtimeName != runtime.OmpRuntime {
		return runtimeName, true, true, nil
	}
	if strings.TrimSpace(jobID) == "" {
		// A prospective OMP dispatch has only registry/runtime and requested-model
		// data. Neither proves which provider will execute, so native fan-out must
		// leave it unresolved; an explicit review can run and record real evidence.
		return "", false, true, nil
	}
	if store == nil || strings.TrimSpace(agentName) == "" {
		return "", false, true, nil
	}
	provider, err := store.JobRecordedProvider(ctx, jobID, agentName)
	if err != nil {
		return "", false, true, err
	}
	provider = normalizeRuntimeFamily(provider)
	if provider == "" {
		return "", false, true, nil
	}
	return ompProviderRuntimeFamily(provider), true, true, nil
}

// ompProviderRuntimeFamily preserves the native family for OMP routes that use
// the same upstream provider as a native adapter. The wrapper is still recorded
// as effective_runtime=omp; only the independence comparison is canonicalized.
// Unknown providers stay namespaced so an unrelated native runtime name cannot
// collide with an operator-defined OMP provider.
func ompProviderRuntimeFamily(provider string) string {
	switch provider = normalizeRuntimeFamily(provider); provider {
	case "anthropic":
		return runtime.ClaudeRuntime
	case "kimi-code":
		return runtime.KimiRuntime
	case "openai", "openai-codex":
		return runtime.CodexRuntime
	default:
		return runtime.OmpRuntime + ":" + provider
	}
}

func resolveRuntimeNameDirect(ctx context.Context, store *db.Store, jobID string, agentName string, recordedRuntime string) (runtimeName string, ok bool, err error) {
	if recorded := normalizeRuntimeFamily(recordedRuntime); recorded != "" {
		return recorded, true, nil
	}
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
	if name == "" || store == nil {
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
func splitTempAgentName(name string) (parentAgent string, parentJobID string, ok bool) {
	index := strings.Index(name, tempAgentInfix)
	if index <= 0 {
		return "", "", false
	}
	parent := strings.TrimSpace(name[:index])
	if parent == "" || parent == name {
		return "", "", false
	}
	// The suffix is the parent JOB id, and carrying it matters for the chained
	// case: a temp worker forked from an ephemeral delegation child resolves its
	// parent agent by name, but that agent is itself synthetic and its own parent
	// lives in a job row. Dropping the suffix here would strand that second hop
	// with no job to read, so the documented two-hop path would not exist.
	return parent, strings.TrimSpace(name[index+len(tempAgentInfix):]), true
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
		if parent, parentJob, ok := splitTempAgentName(name); ok {
			parentAgent = parent
			// Resolve the parent agent by name, but keep the parent JOB for the next
			// hop and for its own recorded runtime. A job id that names no row simply
			// leaves the walk on the name-only path.
			if parentJob != "" {
				if _, err := store.GetJob(ctx, parentJob); err == nil {
					currentJobID = parentJob
				} else if !errors.Is(err, sql.ErrNoRows) {
					return "", false, err
				} else {
					currentJobID = ""
				}
			} else {
				currentJobID = ""
			}
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
		}
		if parentAgent == "" {
			return "", false, nil
		}
		family, ok, resolvedRuntime, err := resolveRuntimeFamilyDirect(ctx, store, currentJobID, parentAgent, "")
		if err != nil {
			return "", false, err
		}
		if ok {
			return family, true, nil
		}
		if resolvedRuntime {
			return "", false, nil
		}
		name = parentAgent
	}
	return "", false, nil
}

func normalizeRuntimeFamily(runtimeName string) string {
	return strings.ToLower(strings.TrimSpace(runtimeName))
}
