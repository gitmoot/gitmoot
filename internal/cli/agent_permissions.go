package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const agentPermissionBlockedMessage = "This Gitmoot worker does not have write permissions, so implementation was not started. Set --policy danger-full-access for full headless implementation (file writes plus go/git/gh via Bash), or --policy workspace-write for edits-only (acceptEdits; does NOT unblock Bash, so go/git/gh stay blocked). Then start or subscribe a writable worker and rerun."

// readOnlyImplementationBlocked reports whether an implement job must be refused
// because its agent's autonomy policy grants no headless write. This now covers
// BOTH read-only AND auto/empty (the default): auto emits no --permission-mode, so
// its write capability is inherited from ambient Claude config and is
// non-deterministic across hosts — fail-closed is the safe default. The shared
// runtime predicate backs every fail-closed seam so the rule stays consistent.
func readOnlyImplementationBlocked(jobType string, agent runtime.Agent) bool {
	if strings.TrimSpace(jobType) != "implement" {
		return false
	}
	return !runtime.PolicyGrantsImplementWrite(agent.AutonomyPolicy)
}

// markJobPermissionBlockedAtGeneration blocks a job for a permission failure.
// atGeneration pins the lifecycle this verdict was formed about, making the
// transition atomic in both state and generation.
//
// The anchored form exists because a check-then-act is not enough (#1407). handleRunJobError
// re-reads the row and checks the generation, but a retry can be claimed in the window between
// that read and this write; the state-only CAS below then accepts the NEWER run's `running` and
// blocks it. Review reproduced exactly that: new run state = "blocked", want running. The
// guarantee has to live in the WRITE, not in a preceding read.
// The matched source state is returned alongside the outcome: the CAS is the only
// party that knows WHICH `from` actually won, and a caller that DESCRIBES the
// transition needs that fact. A pre-write read cannot serve — the very window this
// anchored form exists to close (#1407) sits between the read and the write — so
// "did this child ever run" is answered by the arm that matched, never by a
// snapshot taken before it (#1848).
func markJobPermissionBlockedAtGeneration(ctx context.Context, store *db.Store, jobID string, atGeneration int64) (bool, workflow.JobState, error) {
	if store == nil {
		return false, "", errors.New("job store is required")
	}
	for _, from := range []workflow.JobState{workflow.JobQueued, workflow.JobRunning, workflow.JobFailed} {
		event := db.JobEvent{
			JobID:   jobID,
			Kind:    string(workflow.JobBlocked),
			Message: agentPermissionBlockedMessage,
		}
		if atGeneration >= 0 {
			transitioned, err := store.TransitionJobStateWithEventAtGeneration(ctx, jobID, string(from), atGeneration, string(workflow.JobBlocked), event)
			if err != nil {
				return false, "", err
			}
			if transitioned {
				if err := store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "permission_blocked", Message: agentPermissionBlockedMessage}); err != nil {
					return false, "", err
				}
				return true, from, nil
			}
			continue
		}
		transitioned, err := store.TransitionJobStateWithEvent(ctx, jobID, string(from), string(workflow.JobBlocked), db.JobEvent{
			JobID:   jobID,
			Kind:    string(workflow.JobBlocked),
			Message: agentPermissionBlockedMessage,
		})
		if err != nil {
			return false, "", err
		}
		if transitioned {
			if err := store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "permission_blocked", Message: agentPermissionBlockedMessage}); err != nil {
				return false, "", err
			}
			return true, from, nil
		}
	}
	return false, "", nil
}

// markJobPermissionBlocked derives the atomic-write anchor from the admitted
// row. Callers must retain that row rather than re-read before writing: a queued
// cancellation and retry can produce the same state at a newer generation. It
// drops the matched source state: its callers block a job, they do not describe
// the transition afterwards.
func markJobPermissionBlocked(ctx context.Context, store *db.Store, job db.Job) (bool, error) {
	transitioned, _, err := markJobPermissionBlockedAtGeneration(ctx, store, job.ID, job.LifecycleGeneration)
	return transitioned, err
}

func runtimePermissionFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, pattern := range []string{
		"read-only file system",
		"read-only mode",
		"sandbox rejected write",
		"sandbox denied write",
		"sandbox blocked write",
		"sandbox prevented write",
		"sandbox is read-only",
		"write permissions",
		"write permission",
		"write access denied",
		"write operation denied",
		"not allowed to write",
		"cannot write",
		"can't write",
	} {
		if strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}
