package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1852 finding 2: the pre-flight permission-block site keyed its finalize label
// on the row it was HANDED, which cannot witness whether the child ran. The CAS
// it calls is anchored to a generation, and queued→running does NOT bump the
// generation — the bump fires only on a transition TO queued
// (store_jobs.go's bumpLifecycleGenerationSQL) — so a concurrent claim leaves the
// row RUNNING at the same generation, the CAS's JobRunning arm matches, and the
// site recorded "refused before it ran and never started" for a child that did
// run. This stages exactly that interleaving: the store row is running while the
// caller holds the stale queued snapshot it was admitted with.
func TestPreflightPermissionBlockRoutesOnMatchedStateNotTheHandedRow(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "gitmoot/gitmoot", checkout)
	seedDaemonWorkerAgent(t, store, "coord", runtime.ShellRuntime, "unused", []string{"ask"}, "gitmoot/gitmoot")
	// A read-only implement agent: readOnlyImplementationBlocked is true for it, so
	// run() takes the pre-flight permission-block branch.
	seedDaemonWorkerAgentWithPolicy(t, store, "api", runtime.ShellRuntime, "unused", []string{"implement"}, "gitmoot/gitmoot", runtime.AutonomyPolicyAuto)

	parent := db.Job{ID: "parent-job", Agent: "coord", Type: "ask", State: string(workflow.JobSucceeded), Payload: mustJobPayload(t, workflow.JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "feature", TaskID: "task-1852", TaskTitle: "Coordinator", Sender: "coord",
		Result: &workflow.AgentResult{Decision: "approved", Summary: "fan out", Delegations: []workflow.Delegation{{ID: "api", Agent: "api", Action: "implement", Prompt: "build it"}}},
	})}
	if err := store.CreateJobWithEvent(ctx, parent, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(parent) returned error: %v", err)
	}
	child := db.Job{ID: "parent-job/delegation/api", Agent: "api", Type: "implement", State: string(workflow.JobQueued), Payload: mustJobPayload(t, workflow.JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "feature", TaskID: "task-1852", TaskTitle: "Child", Sender: "coord",
		ParentJobID: "parent-job", DelegationID: "api", DelegationDepth: 1, DelegatedBy: "coord", RootJobID: "parent-job",
	})}
	if err := store.CreateJobWithEvent(ctx, child, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(child) returned error: %v", err)
	}

	// The row the worker was admitted with: queued, at generation G.
	admitted, err := store.GetJob(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetJob(admitted) returned error: %v", err)
	}
	if admitted.State != string(workflow.JobQueued) {
		t.Fatalf("setup: admitted state = %q, want queued", admitted.State)
	}
	// THE INTERLEAVING: something claims the job queued→running. No generation bump,
	// so the CAS anchored to G still matches — on its JobRunning arm.
	claimed, err := store.TransitionJobStateWithEvent(ctx, child.ID, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{
		JobID: child.ID, Kind: string(workflow.JobRunning), Message: "claimed by a concurrent worker",
	})
	if err != nil || !claimed {
		t.Fatalf("staging the concurrent claim: claimed=%v err=%v", claimed, err)
	}
	running, err := store.GetJob(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetJob(running) returned error: %v", err)
	}
	if running.State != string(workflow.JobRunning) {
		t.Fatalf("setup: state after the claim = %q, want running", running.State)
	}
	if running.LifecycleGeneration != admitted.LifecycleGeneration {
		t.Fatalf("setup: generation moved %d -> %d; this test's premise is that queued->running does NOT bump it",
			admitted.LifecycleGeneration, running.LifecycleGeneration)
	}

	worker := defaultJobWorker(store, io.Discard)
	engine := daemonWorkflowEngine(store, github.NewClient(checkout), checkout, "")
	worker.WorkflowFactory = func(string) workflow.Engine { return engine }
	// run() is handed the STALE queued row, exactly as the admitted worker holds it.
	if err := worker.run(ctx, admitted); err != nil {
		t.Fatalf("worker.run(stale queued row) returned error: %v", err)
	}

	// The child was claimed, so the recorded cause must be the mid-run one. Labelling
	// it "refused before it ran" is the mirror image of the #1848 defect.
	if got := countWorkerJobEvents(t, store, child.ID, "delegation_runtime_failure_finalized"); got != 1 {
		t.Fatalf("delegation_runtime_failure_finalized events = %d, want 1: the CAS matched JobRunning, so this child ran", got)
	}
	if got := countWorkerJobEvents(t, store, child.ID, "delegation_refused_finalized"); got != 0 {
		t.Fatalf("delegation_refused_finalized events = %d, want 0: this child was claimed before the block", got)
	}
	found := 0
	for _, ev := range workerJobEvents(t, store, child.ID) {
		if ev.Kind != "delegation_runtime_failure_finalized" {
			continue
		}
		found++
		if !strings.Contains(ev.Message, "failed mid-run without a result") {
			t.Fatalf("finalize message = %q, want it to name the mid-run failure", ev.Message)
		}
	}
	if found != 1 {
		t.Fatalf("finalize messages inspected = %d, want 1", found)
	}
	// The parent DAG still advances: only the recorded cause changed.
	settled, err := store.GetJob(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetJob(settled) returned error: %v", err)
	}
	settledPayload, err := daemonJobPayload(settled)
	if err != nil {
		t.Fatalf("daemonJobPayload(settled) returned error: %v", err)
	}
	if settledPayload.Result == nil {
		t.Fatal("the blocked child stored no synthetic result, so the parent DAG was never advanced")
	}
}

// #1852 finding 3: the routing tested one arm by equality and let every other
// state default to the mid-run label, so appending a fourth `from` to the CAS
// would silently acquire "failed mid-run without a result". The mapper is now
// exhaustive over the CAS's three arms and REFUSES anything else, so a new arm
// fails loudly here instead of quietly mislabelling.
func TestPermissionBlockFinalizerRefusesAnUnenumeratedState(t *testing.T) {
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)

	for _, tc := range []struct {
		state workflow.JobState
		want  string
	}{
		{workflow.JobQueued, "refused"},
		{workflow.JobRunning, "midrun"},
		{workflow.JobFailed, "midrun"},
	} {
		finalize, err := worker.permissionBlockFinalizerFor(tc.state)
		if err != nil {
			t.Fatalf("permissionBlockFinalizerFor(%q) returned error: %v", tc.state, err)
		}
		if finalize == nil {
			t.Fatalf("permissionBlockFinalizerFor(%q) returned a nil finalizer", tc.state)
		}
	}

	// A state the CAS does not accept today. If someone adds it there, this is where
	// the omission surfaces.
	finalize, err := worker.permissionBlockFinalizerFor(workflow.JobCancelled)
	if err == nil {
		t.Fatalf("permissionBlockFinalizerFor(%q) returned a finalizer with no error; an unenumerated state must not inherit a label", workflow.JobCancelled)
	}
	if finalize != nil {
		t.Fatal("permissionBlockFinalizerFor returned both a finalizer and an error")
	}
	if !strings.Contains(err.Error(), string(workflow.JobCancelled)) {
		t.Fatalf("error = %v, want it to name the unexpected state", err)
	}
}
