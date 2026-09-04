package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1512 round-2 F-1: the mid-run permission branch in handleRunJobError keys on
// the ERROR's shape (`latest.Type == "implement" && runtimePermissionFailure`),
// not on whether the child ever ran — and it is reachable by a child that never
// did, because markJobPermissionBlockedAtGeneration accepts a transition from
// JobQueued as well as JobRunning and JobFailed (agent_permissions.go:41). A
// still-QUEUED delegation child whose run errors with a permission failure was
// therefore finalized as "failed mid-run without a result", which is the same
// false claim as the timeout verb this issue removed, one level down: one
// hard-coded cause for three observed states inside a single caller.
//
// The recorded cause must follow the child's observed state. This drives
// handleRunJobError — the seam whose state-keying is the defect — with the row
// still queued, which is precisely the precondition the branch failed to read.
func TestQueuedPermissionBlockedDelegationChildIsRecordedAsRefused(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "gitmoot/gitmoot", checkout)
	seedDaemonWorkerAgent(t, store, "coord", runtime.ShellRuntime, "unused", []string{"ask"}, "gitmoot/gitmoot")
	seedDaemonWorkerAgent(t, store, "api", runtime.ShellRuntime, "unused", []string{"implement"}, "gitmoot/gitmoot")

	// A coordinator parent with a delegation child that is STILL QUEUED: no claim,
	// no adapter, no delivery.
	parent := db.Job{ID: "parent-job", Agent: "coord", Type: "ask", State: string(workflow.JobSucceeded), Payload: mustJobPayload(t, workflow.JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "feature", TaskID: "task-q", TaskTitle: "Coordinator", Sender: "coord",
		Result: &workflow.AgentResult{Decision: "approved", Summary: "fan out", Delegations: []workflow.Delegation{{ID: "api", Agent: "api", Action: "implement", Prompt: "build it"}}},
	})}
	if err := store.CreateJobWithEvent(ctx, parent, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(parent) returned error: %v", err)
	}
	child := db.Job{ID: "parent-job/delegation/api", Agent: "api", Type: "implement", State: string(workflow.JobQueued), Payload: mustJobPayload(t, workflow.JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "feature", TaskID: "task-q", TaskTitle: "Child", Sender: "coord",
		ParentJobID: "parent-job", DelegationID: "api", DelegationDepth: 1, DelegatedBy: "coord", RootJobID: "parent-job",
	})}
	if err := store.CreateJobWithEvent(ctx, child, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(child) returned error: %v", err)
	}

	worker := defaultJobWorker(store, io.Discard)
	engine := daemonWorkflowEngine(store, github.NewClient(checkout), checkout, "")
	worker.WorkflowFactory = func(string) workflow.Engine { return engine }

	queued, err := store.GetJob(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetJob(child) returned error: %v", err)
	}
	if queued.State != string(workflow.JobQueued) {
		t.Fatalf("setup: child state = %q, want queued", queued.State)
	}
	cause := errors.New("write /workspace/file: read-only file system")
	if !runtimePermissionFailure(cause) {
		t.Fatal("setup: the cause must match runtimePermissionFailure to reach the branch under test")
	}
	if err := worker.handleRunJobError(ctx, child.ID, observedJobLifecycle(queued), cause); err != nil {
		t.Fatalf("handleRunJobError returned error: %v", err)
	}

	// A child that never started is recorded as REFUSED, never as a mid-run failure.
	if got := countWorkerJobEvents(t, store, child.ID, "delegation_refused_finalized"); got != 1 {
		t.Fatalf("delegation_refused_finalized events = %d, want 1 for a child that never ran", got)
	}
	if got := countWorkerJobEvents(t, store, child.ID, "delegation_runtime_failure_finalized"); got != 0 {
		t.Fatalf("delegation_runtime_failure_finalized events = %d, want 0: this child was never claimed", got)
	}
	if got := countWorkerJobEvents(t, store, child.ID, "delegation_timeout_finalized"); got != 0 {
		t.Fatalf("delegation_timeout_finalized events = %d, want 0: no deadline elapsed", got)
	}
	// And the message names the refusal rather than a run that did not happen.
	found := 0
	for _, ev := range workerJobEvents(t, store, child.ID) {
		if ev.Kind != "delegation_refused_finalized" {
			continue
		}
		found++
		if !strings.Contains(ev.Message, "refused before it ran") {
			t.Fatalf("refused finalize message = %q, want it to name the refusal", ev.Message)
		}
	}
	if found != 1 {
		t.Fatalf("refused finalize messages inspected = %d, want 1: the loop above asserted nothing", found)
	}
	// The parent DAG is still advanced: the disposition changed, not the routing.
	settled, err := store.GetJob(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetJob(settled) returned error: %v", err)
	}
	settledPayload, err := daemonJobPayload(settled)
	if err != nil {
		t.Fatalf("daemonJobPayload(settled) returned error: %v", err)
	}
	if settledPayload.Result == nil {
		t.Fatal("the refused child stored no synthetic result, so the parent DAG was never advanced")
	}
}
