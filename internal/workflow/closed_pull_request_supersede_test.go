package workflow

import (
	"context"
	"database/sql"
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

	job, superseded, err := SupersedeClosedPullRequestJob(ctx, store, "workflow-stranded",
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
	again, supersededAgain, err := SupersedeClosedPullRequestJob(ctx, store, "workflow-stranded", "second observation")
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

	job, superseded, err := SupersedeClosedPullRequestJob(ctx, store, "workflow-running", "pr closed")
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
	finalized, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, child,
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

	finalized, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, "workflow-top-level", "pr closed")
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

// TestFinalizeClosedPullRequestDelegationChildOfMergedTaskReleasesCoordinator is
// the PR's headline case, driven through the real fan-out route. A review fan-out
// on a pull request that has since MERGED leaves queued children; the sweep must be
// able to hand each one to the coordinator-releasing path without the parent's
// failure_policy rewriting `merged` to `blocked` on the way (#1673).
//
// It asserts on the PARENT, not just the child: under the default block_parent the
// coordinator's policy runs to its own decision (a BlockedError naming the closed
// PR) while the task stays merged, and under `continue` the coordinator continuation
// job is actually minted — the row that read ErrNoRows while the sweep was refusing
// to route these children at all.
func TestFinalizeClosedPullRequestDelegationChildOfMergedTaskReleasesCoordinator(t *testing.T) {
	for _, tc := range []struct {
		policy string
		// wantBlocked is the block_parent contract: the parent's policy surfaces a
		// BlockedError, which is the DAG deciding, not the sweep failing.
		wantBlocked bool
		// wantContinuation is the coordinator continuation the `continue` policy
		// mints once every delegation is resolved.
		wantContinuation bool
	}{
		{policy: "", wantBlocked: true},
		{policy: "continue", wantContinuation: true},
	} {
		name := tc.policy
		if name == "" {
			name = "block_parent_default"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
			seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
			seedAgent(t, store, "ui", []string{"review"}, "gitmoot/gitmoot")
			engine := testEngine(store)
			// The task is MERGED before the sweep runs: the daemon's own
			// reconcileExternallyMergedTasks drives it there in the same PollOnce.
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-7", RepoFullName: "gitmoot/gitmoot", GoalID: "goal-1", Title: "Parent",
				State: string(TaskMerged), Branch: "task-7",
			}); err != nil {
				t.Fatalf("UpsertTask(task-7) returned error: %v", err)
			}
			delegations := []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api", FailurePolicy: tc.policy},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "review ui", FailurePolicy: tc.policy},
			}
			insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
				Repo:        "gitmoot/gitmoot",
				Branch:      "task-7",
				PullRequest: 7,
				TaskID:      "task-7",
				TaskTitle:   "Parent",
				Sender:      "coord",
				Result: &AgentResult{
					Decision:    "approved",
					Summary:     "fan out",
					Delegations: delegations,
				},
			})
			if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
				t.Fatalf("AdvanceJob(parent) returned error: %v", err)
			}

			children := []string{"parent-job/delegation/api", "parent-job/delegation/ui"}
			for _, child := range children {
				if mustJob(t, store, child).State != string(JobQueued) {
					t.Fatalf("child %s is not queued", child)
				}
				finalized, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, child,
					"queued review job superseded: gitmoot/gitmoot pull request #7 is no longer open")
				var blocked BlockedError
				switch {
				case tc.wantBlocked:
					if !errors.As(err, &blocked) {
						t.Fatalf("finalize %s error = %v, want a BlockedError from the parent's block_parent policy", child, err)
					}
					if !strings.Contains(blocked.Reason, "pull request #7 is no longer open") {
						t.Fatalf("blocked reason = %q, want the closed PR named", blocked.Reason)
					}
				case err != nil:
					t.Fatalf("finalize %s returned error: %v", child, err)
				}
				if !finalized {
					t.Fatalf("finalized = false for %s, want the queued child terminated and its parent advanced", child)
				}

				terminal := mustJob(t, store, child)
				if terminal.State != string(JobFailed) {
					t.Fatalf("child %s state = %q, want failed (the only terminal state a child can advance a parent from)", child, terminal.State)
				}
				payload, err := unmarshalPayload(terminal.Payload)
				if err != nil {
					t.Fatalf("unmarshalPayload(%s) returned error: %v", child, err)
				}
				if payload.Result == nil {
					t.Fatalf("child %s has no result: the coordinator would wait forever", child)
				}
				if !strings.Contains(payload.Result.Summary, "pull request #7 is no longer open") {
					t.Fatalf("child %s result summary = %q, want the closed PR named", child, payload.Result.Summary)
				}
			}

			// PARENT SIDE. The merged record survives the whole sweep: this is the
			// state whose loss the pre-fix daemon avoided only by refusing to route
			// these children, which stranded the coordinator instead.
			assertTaskState(t, store, "task-7", TaskMerged)

			// The coordinator's delegation barrier no longer waits on anything: every
			// delegation is resolved as far as the parent's own resolver is concerned.
			resolved, err := engine.childDelegationJobs(ctx, "parent-job")
			if err != nil {
				t.Fatalf("childDelegationJobs returned error: %v", err)
			}
			if !allDelegationsResolved(delegations, resolved, nil) {
				t.Fatalf("coordinator still waiting: children = %+v", resolved)
			}

			continuation, err := store.GetJob(ctx, DelegationContinuationID("parent-job"))
			switch {
			case tc.wantContinuation:
				if err != nil {
					t.Fatalf("GetJob(continuation) returned error: %v; the coordinator was not released", err)
				}
				if continuation.State != string(JobQueued) {
					t.Fatalf("continuation state = %q, want queued", continuation.State)
				}
			default:
				// block_parent deliberately mints no continuation; the parent's
				// BlockedError above is its terminal decision.
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("GetJob(continuation) = %+v err=%v, want no row under block_parent", continuation, err)
				}
			}

			// The refused block is legible after the fact, not silent.
			events, err := store.ListTaskEvents(ctx, "task-7")
			if err != nil {
				t.Fatalf("ListTaskEvents returned error: %v", err)
			}
			refusals := 0
			for _, event := range events {
				if event.Kind == TaskEventMergedBlockRefused {
					refusals++
				}
			}
			if tc.wantBlocked && refusals == 0 {
				t.Fatalf("no %s task event: the dropped block left no trace", TaskEventMergedBlockRefused)
			}
			if !tc.wantBlocked && refusals != 0 {
				t.Fatalf("%s events = %d under %s, want 0 (no block was ever attempted)", TaskEventMergedBlockRefused, refusals, name)
			}
		})
	}
}
