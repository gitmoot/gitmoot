package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestSupersedeClosedPullRequestJobTerminatesQueuedLegLegibly pins the top-level
// path: a queued leg bound to a closed PR becomes cancelled AND carries a reason
// somebody can read. An indefinite `queued` row is camouflage; a terminal state with
// a named cause is the whole point of #1673.
func TestSupersedeClosedPullRequestJobTerminatesQueuedLegLegibly(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	insertQueuedJob(t, store, db.Job{ID: "workflow-stranded", Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})

	job, superseded, err := SupersedeClosedPullRequestJob(ctx, store, mustJob(t, store, "workflow-stranded"),
		"queued implement job superseded: gitmoot/gitmoot pull request #7 is no longer open")
	if err != nil {
		t.Fatalf("SupersedeClosedPullRequestJob returned error: %v", err)
	}
	if !superseded {
		t.Fatal("superseded = false, want the queued leg terminated")
	}
	if job.State != string(JobCancelled) {
		t.Fatalf("state = %q, want cancelled", job.State)
	}
	events, err := store.ListJobEvents(ctx, "workflow-stranded")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	var reason string
	for _, event := range events {
		if event.Kind == JobEventSupersededPullRequestClosed {
			reason = event.Message
		}
	}
	if !strings.Contains(reason, "pull request #7 is no longer open") {
		t.Fatalf("event %q message = %q, want the closed PR named", JobEventSupersededPullRequestClosed, reason)
	}
	// The cancellation must NOT look like a cancel-from-running: that shape is what
	// task liveness reads as a still-live successor until a daemon restart.
	if strings.Contains(reason, "cancel requested from running") {
		t.Fatalf("reason wears the cancel-from-running prefix: %q", reason)
	}

	// Idempotent: a second poll observing the same closed PR changes nothing.
	again, supersededAgain, err := SupersedeClosedPullRequestJob(ctx, store, mustJob(t, store, "workflow-stranded"), "second observation")
	if err != nil {
		t.Fatalf("second SupersedeClosedPullRequestJob returned error: %v", err)
	}
	if supersededAgain {
		t.Fatal("second call reported a transition; the row was already terminal")
	}
	if again.State != string(JobCancelled) {
		t.Fatalf("state after second call = %q, want cancelled", again.State)
	}
}

// TestSupersedeClosedPullRequestJobLeavesRunningWorkAlone keeps the sweep off work
// in flight: a running job's output may still be worth harvesting, and killing it is
// a different decision from clearing a leg that never started.
func TestSupersedeClosedPullRequestJobLeavesRunningWorkAlone(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	insertQueuedJob(t, store, db.Job{ID: "workflow-running", Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	if err := store.UpdateJobState(ctx, "workflow-running", string(JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}

	job, superseded, err := SupersedeClosedPullRequestJob(ctx, store, mustJob(t, store, "workflow-running"), "pr closed")
	if err != nil {
		t.Fatalf("SupersedeClosedPullRequestJob returned error: %v", err)
	}
	if superseded || job.State != string(JobRunning) {
		t.Fatalf("running job = %q superseded=%t, want untouched", job.State, superseded)
	}
}

// TestFinalizeClosedPullRequestDelegationChildReleasesCoordinator is the half a
// plain cancel cannot do. finalizeTimedOutJob's state gate rejects `cancelled`, so a
// cancelled child would leave its coordinator waiting forever — the strand would
// move from the child to the parent. The child therefore lands in `failed` with the
// same legible event and a synthetic result, which is what advanceDelegations reads.
func TestFinalizeClosedPullRequestDelegationChildReleasesCoordinator(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Parent",
		Sender:      "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "review ui"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	child := "parent-job/delegation/api"
	if mustJob(t, store, child).State != string(JobQueued) {
		t.Fatalf("child %s is not queued", child)
	}
	// The parent's own failure_policy decides what a dead child means. With the
	// default block_parent that surfaces as a BlockedError, which is the DAG making a
	// decision, not the sweep failing — the daemon treats it as a normal outcome.
	finalized, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, mustJob(t, store, child),
		"queued review job superseded: gitmoot/gitmoot pull request #7 is no longer open")
	var blocked BlockedError
	if err != nil && !errors.As(err, &blocked) {
		t.Fatalf("FinalizeClosedPullRequestDelegationChild returned error: %v", err)
	}
	if err != nil && !strings.Contains(blocked.Reason, "pull request #7 is no longer open") {
		t.Fatalf("blocked reason = %q, want the closed PR named", blocked.Reason)
	}
	if !finalized {
		t.Fatal("finalized = false, want the queued child terminated and its parent advanced")
	}
	terminal := mustJob(t, store, child)
	if terminal.State != string(JobFailed) {
		t.Fatalf("child state = %q, want failed (the only terminal state a child can advance a parent from)", terminal.State)
	}
	payload, err := unmarshalPayload(terminal.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(child) returned error: %v", err)
	}
	// The synthetic result is what the coordinator's advanceDelegations consumes; a
	// child with a nil result is exactly the shape that strands the parent.
	if payload.Result == nil {
		t.Fatal("child has no result: the coordinator would wait forever")
	}
	if !strings.Contains(payload.Result.Summary, "pull request #7 is no longer open") {
		t.Fatalf("child result summary = %q, want the closed PR named", payload.Result.Summary)
	}
	events, err := store.ListJobEvents(ctx, child)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	legible := false
	for _, event := range events {
		if event.Kind == JobEventSupersededPullRequestClosed {
			legible = true
		}
	}
	if !legible {
		t.Fatalf("child carries no %s event", JobEventSupersededPullRequestClosed)
	}
}

// TestFinalizeClosedPullRequestDelegationChildIgnoresTopLevelAndRunningJobs keeps
// the child path from becoming a second, wider terminator.
func TestFinalizeClosedPullRequestDelegationChildIgnoresTopLevelAndRunningJobs(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	insertQueuedJob(t, store, db.Job{ID: "workflow-top-level", Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})

	finalized, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, mustJob(t, store, "workflow-top-level"), "pr closed")
	if err != nil {
		t.Fatalf("FinalizeClosedPullRequestDelegationChild returned error: %v", err)
	}
	if finalized {
		t.Fatal("a job with no ParentJobID was finalized as a delegation child")
	}
	if mustJob(t, store, "workflow-top-level").State != string(JobQueued) {
		t.Fatal("top-level job was not left queued for the top-level path")
	}
}

// insertQueuedJob seeds a QUEUED job with the given payload, the state the #1673
// population is stuck in.
func insertQueuedJob(t *testing.T, store *db.Store, job db.Job, payload JobPayload) {
	t.Helper()
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	job.State = string(JobQueued)
	job.Payload = encoded
	if err := store.CreateJob(context.Background(), job); err != nil {
		t.Fatalf("CreateJob(%s) returned error: %v", job.ID, err)
	}
}
