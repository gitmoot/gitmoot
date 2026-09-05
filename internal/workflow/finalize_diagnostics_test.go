package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1351/#1417/#1557, the failure-diagnostics half of the class. A delegation
// child terminalized WITHOUT a stored result gets a synthesized
// AgentResult{Decision:"failed"} from finalizeTimedOutJob, and nothing writes
// FailureDiagnostics on that path — so phase, exit_code and signal are all
// absent and the row cannot say WHY the leg ended.
//
// Measured on the live store before this change: 169 failed delegation legs
// carried NO diagnostics object at all against 2 that did. The two that did
// were legs whose DELIVERY failed, which is the only path that records one
// (recordDeliveryFailureDiagnostics). A leg finalized by the engine after the
// fact — spent deadline, supersession, refusal, mid-run runtime failure — got
// nothing, which is what made 28 such legs read as error/signal/phase empty.
func TestFinalizeDelegationChildRecordsWhyItEnded(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store}

	parentPayload := JobPayload{Repo: "gitmoot/gitmoot", PullRequest: 1910, TaskID: "review-pr-1910"}
	insertQueuedJob(t, store, db.Job{ID: "parent-review", Agent: "coordinator", Type: "review"}, parentPayload)

	childPayload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 1910,
		TaskID:      "review-pr-1910",
		ParentJobID: "parent-review",
	}
	insertQueuedJob(t, store, db.Job{
		ID:           "parent-review/delegation/lens-correctness",
		Agent:        "lens",
		Type:         "review",
		DelegationID: "lens-correctness",
	}, childPayload)
	if _, err := store.TransitionJobState(ctx, "parent-review/delegation/lens-correctness", string(JobQueued), string(JobRunning)); err != nil {
		t.Fatalf("TransitionJobState: %v", err)
	}

	const reason = "delegation child timed out before returning a result"
	// The finalizer propagates the parent's block_parent policy as an error
	// AFTER writing the child's payload, so a "workflow blocked" error is the
	// expected shape here and not a fixture fault. Any OTHER error is.
	recovered, err := engine.FinalizeTimedOutDelegationChild(ctx, "parent-review/delegation/lens-correctness", reason)
	if err != nil && !strings.Contains(err.Error(), "workflow blocked") {
		t.Fatalf("FinalizeTimedOutDelegationChild returned an unexpected error: %v", err)
	}
	_ = recovered

	job, err := store.GetJob(ctx, "parent-review/delegation/lens-correctness")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	// Premise: the synthesized result is what the engine writes on this path.
	if payload.Result == nil || payload.Result.Decision != "failed" {
		t.Fatalf("synthesized result = %+v, want a failed decision", payload.Result)
	}
	if payload.FailureDiagnostics == nil {
		t.Fatal("a leg finalized without a result carries NO failure diagnostics; the row cannot say why it ended")
	}
	if strings.TrimSpace(payload.FailureDiagnostics.Phase) == "" {
		t.Fatalf("failure diagnostics phase is empty: %+v", payload.FailureDiagnostics)
	}
	if !strings.Contains(payload.FailureDiagnostics.DeliveryError, reason) {
		t.Fatalf("failure diagnostics do not carry the observed cause: %+v", payload.FailureDiagnostics)
	}
}

// The control: a leg that ALREADY carries diagnostics from its own delivery
// failure must keep them. Overwriting a real crash report with a generic
// finalize marker would destroy the better evidence — the two rows in the live
// store that DID have diagnostics are exactly this case.
func TestFinalizeDelegationChildPreservesExistingDiagnostics(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store}

	insertQueuedJob(t, store, db.Job{ID: "parent-2", Agent: "coordinator", Type: "review"},
		JobPayload{Repo: "gitmoot/gitmoot", PullRequest: 1910, TaskID: "task-2"})

	exit := 1
	childPayload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 1910,
		TaskID:      "task-2",
		ParentJobID: "parent-2",
		FailureDiagnostics: &FailureDiagnostics{
			Phase:         FailurePhaseLaunched,
			ExitCode:      &exit,
			StderrTail:    `sandbox-exec: resolve sandbox target "claude"`,
			DeliveryError: `exec: "claude": executable file not found in $PATH`,
		},
	}
	insertQueuedJob(t, store, db.Job{
		ID: "parent-2/delegation/lens-security", Agent: "lens", Type: "review", DelegationID: "lens-security",
	}, childPayload)
	if _, err := store.TransitionJobState(ctx, "parent-2/delegation/lens-security", string(JobQueued), string(JobRunning)); err != nil {
		t.Fatalf("TransitionJobState: %v", err)
	}

	if _, err := engine.FinalizeTimedOutDelegationChild(ctx, "parent-2/delegation/lens-security", "deadline spent"); err != nil &&
		!strings.Contains(err.Error(), "workflow blocked") {
		t.Fatalf("FinalizeTimedOutDelegationChild returned an unexpected error: %v", err)
	}

	job, err := store.GetJob(ctx, "parent-2/delegation/lens-security")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.FailureDiagnostics == nil {
		t.Fatal("existing diagnostics were dropped by the finalizer")
	}
	if payload.FailureDiagnostics.Phase != FailurePhaseLaunched ||
		!strings.Contains(payload.FailureDiagnostics.DeliveryError, "executable file not found") {
		t.Fatalf("the finalizer overwrote a real crash report with a generic marker: %+v", payload.FailureDiagnostics)
	}
}

// The other finalizer. FinalizeFailedDelegationChild is a DIFFERENT entry point
// with its own event kind (JobEventDelegationRuntimeFailureFinalized), and #1512
// split these four kinds apart precisely because a timed-out leg, a superseded
// one, a refused one and a mid-run runtime failure used to be indistinguishable.
// A guard proven only on the timeout path would leave three of the four causes
// still unreadable, so this pins the runtime-failure entry too.
func TestFinalizeFailedDelegationChildRecordsWhyItEnded(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store}

	insertQueuedJob(t, store, db.Job{ID: "parent-3", Agent: "coordinator", Type: "review"},
		JobPayload{Repo: "gitmoot/gitmoot", PullRequest: 1910, TaskID: "task-3"})
	insertQueuedJob(t, store, db.Job{
		ID: "parent-3/delegation/lens-regression", Agent: "lens", Type: "review", DelegationID: "lens-regression",
	}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 1910, TaskID: "task-3", ParentJobID: "parent-3",
	})
	if _, err := store.TransitionJobState(ctx, "parent-3/delegation/lens-regression", string(JobQueued), string(JobRunning)); err != nil {
		t.Fatalf("TransitionJobState: %v", err)
	}

	const reason = `sandbox-exec: resolve sandbox target "claude": executable file not found in $PATH`
	if _, err := engine.FinalizeFailedDelegationChild(ctx, "parent-3/delegation/lens-regression", reason); err != nil &&
		!strings.Contains(err.Error(), "workflow blocked") {
		t.Fatalf("FinalizeFailedDelegationChild returned an unexpected error: %v", err)
	}

	job, err := store.GetJob(ctx, "parent-3/delegation/lens-regression")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.FailureDiagnostics == nil {
		t.Fatal("a mid-run runtime failure finalized without a result carries NO diagnostics")
	}
	if payload.FailureDiagnostics.Phase != FailurePhaseFinalized {
		t.Fatalf("phase = %q, want %q: no CLI session state was observed, so it must not claim one", payload.FailureDiagnostics.Phase, FailurePhaseFinalized)
	}
	if !strings.Contains(payload.FailureDiagnostics.DeliveryError, "executable file not found") {
		t.Fatalf("the observed cause was lost: %+v", payload.FailureDiagnostics)
	}
}
