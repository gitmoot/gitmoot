package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

const agentPermissionBlockedMessage = "This Gitmoot worker does not have write permissions, so implementation was not started. Start or subscribe a writable worker for implementation tasks, then rerun."

func readOnlyImplementationBlocked(jobType string, agent runtime.Agent) bool {
	return strings.TrimSpace(jobType) == "implement" &&
		runtime.NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy) == runtime.AutonomyPolicyReadOnly
}

func markJobPermissionBlocked(ctx context.Context, store *db.Store, jobID string) (bool, error) {
	if store == nil {
		return false, errors.New("job store is required")
	}
	for _, from := range []workflow.JobState{workflow.JobQueued, workflow.JobRunning, workflow.JobFailed} {
		transitioned, err := store.TransitionJobStateWithEvent(ctx, jobID, string(from), string(workflow.JobBlocked), db.JobEvent{
			JobID:   jobID,
			Kind:    string(workflow.JobBlocked),
			Message: agentPermissionBlockedMessage,
		})
		if err != nil {
			return false, err
		}
		if transitioned {
			if err := store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "permission_blocked", Message: agentPermissionBlockedMessage}); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
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
