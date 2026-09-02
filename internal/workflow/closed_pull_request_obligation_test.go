package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestClosedPullRequestChildKeepsItsAdvanceObligationWhenTheSecondStageFails is
// Finding B of the #1763 exact-head review: the queued -> failed transition and the
// re-drive obligation must commit TOGETHER.
//
// The trigger is an error in the SECOND stage - stamping the synthetic result and
// advancing the parent. Before the fix, the child was already `failed` with no result
// and no marker, so ListQueuedJobs would never select it again and the coordinator
// waited forever. After the fix the child carries `advance_retry`, which the liveness
// table treats as still-pending advancement, so a sweep re-drives it.
//
// SEMANTIC REVERSION THIS KILLS: drop the obligation event from the transition (write
// only the supersession event, or write the marker as a separate call after it) and the
// failed child is stranded with no pending advancement.
func TestClosedPullRequestChildKeepsItsAdvanceObligationWhenTheSecondStageFails(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
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
			},
		},
	})
	// BREAK THE SECOND STAGE THROUGH THE REAL PATH: this queued child names a
	// delegation its parent's result does not carry, so stage 1 (the state transition)
	// commits and stage 2 (stamp the synthetic result, advance the parent) errors. That
	// is the shape of ANY store or advancement failure after the first commit.
	child := "missing-parent/delegation/api"
	insertQueuedJob(t, store, db.Job{
		ID:           child,
		Agent:        "api",
		Type:         "review",
		ParentJobID:  "missing-parent",
		DelegationID: "api",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		ParentJobID: "missing-parent",
		Sender:      "coord",
	})

	finalized, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, mustJob(t, store, child),
		"queued review job superseded: gitmoot/gitmoot pull request #7 is no longer open")
	var blocked BlockedError
	if err == nil || errors.As(err, &blocked) {
		t.Fatalf("second stage error = %v, finalized = %v: this test needs the second stage to fail", err, finalized)
	}

	// THE TRANSITION LANDED - that half is not in question.
	terminal := mustJob(t, store, child)
	if terminal.State != string(JobFailed) {
		t.Fatalf("child state = %q, want failed", terminal.State)
	}

	// AND THE OBLIGATION LANDED WITH IT. This is the assertion the old code fails.
	//
	// The marker is supersede_finalize_pending, not advance_retry: this path now commits
	// the debt in the SAME transaction as the terminal write (#1731), so there is no
	// window where the child is settled and the debt is not yet recorded. Asserting the
	// QUEUE rather than the event kind is what makes this a test of the behaviour - a
	// child that is not in the re-drive queue is stranded no matter which marker it
	// carries.
	events, err := store.ListJobEvents(ctx, child)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	obligations := 0
	for _, event := range events {
		if event.Kind == JobEventSupersedeFinalizePending {
			obligations++
		}
	}
	if obligations != 1 {
		t.Fatalf("%s events = %d, want exactly 1: a failed child with no marker is stranded", JobEventSupersedeFinalizePending, obligations)
	}
	pending, err := store.JobIDsWithPendingSupersedeFinalization(ctx)
	if err != nil {
		t.Fatalf("JobIDsWithPendingSupersedeFinalization: %v", err)
	}
	if len(pending) != 1 || pending[0] != child {
		t.Fatalf("re-drive queue = %v, want exactly [%s]: the sweep will never reach this child", pending, child)
	}
	if !advancementPending(events) {
		t.Fatal("advancementPending = false: the sweep will never re-drive this child")
	}
	if !jobKeepsTaskLive(terminal, events) {
		t.Fatal("jobKeepsTaskLive = false: the coordinator's task is dead while the parent still waits")
	}
}

// TestClosedPullRequestChildObligationClearsOnSuccess is the SUCCESS-PATH control, and
// it is the half that stops the fix from being a bound that rejects valid input: on an
// uninterrupted run the obligation must not leave the child permanently "pending
// advancement" once the parent actually advanced.
func TestClosedPullRequestChildObligationClearsOnSuccess(t *testing.T) {
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
		t.Fatalf("AdvanceJob(parent): %v", err)
	}
	child := "parent-job/delegation/api"

	finalized, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, mustJob(t, store, child),
		"queued review job superseded: gitmoot/gitmoot pull request #7 is no longer open")
	var blocked BlockedError
	if err != nil && !errors.As(err, &blocked) {
		t.Fatalf("FinalizeClosedPullRequestDelegationChild: %v", err)
	}
	if !finalized {
		t.Fatal("finalized = false on the success path")
	}

	terminal := mustJob(t, store, child)
	payload, err := unmarshalPayload(terminal.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.Result == nil || !strings.Contains(payload.Result.Summary, "pull request #7 is no longer open") {
		t.Fatalf("child result = %+v, want the synthetic result naming the closed PR", payload.Result)
	}
	events, err := store.ListJobEvents(ctx, child)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	// SELF-CLEARING: the obligation is written first, the finalizer's own advancement
	// outcome is a later event, and the scan is last-writer-wins - so a completed
	// advancement must leave nothing pending.
	if advancementPending(events) {
		t.Fatalf("advancementPending = true after a successful advance: the obligation never cleared; events = %+v", events)
	}
}
