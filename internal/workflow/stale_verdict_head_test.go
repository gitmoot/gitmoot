package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1524. A verdict is evidence about a COMMIT, not about the branch, so it may
// transition the task only when it describes the pull request's CURRENT head.
//
// These arms reuse changes_requested_wedge_test.go's fixtures deliberately: that
// file already drives Engine.AdvanceJob against a real PolicyMergeGate (a nil
// gate short-circuits to setTaskState and would pin nothing), and #1871's seven
// arms there pin the APPROVAL side of the same rule. What follows pins the
// objection side, which was left unconditional.

func staleHeadSkipReason(t *testing.T, store *db.Store, jobID string) string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "advance_skipped_stale_head" {
			return event.Message
		}
	}
	return ""
}

// THE DEFECT ARM: an objection bound to a superseded head must transition the
// task NEITHER way. Before this change the objection arm called
// setTaskState(TaskChangesRequested) unconditionally, so a stale objection could
// pull a ready_to_merge PR back over a commit the branch had already moved past.
//
// The task starts in ready_to_merge precisely because that is the state the
// defect destroys; asserting it survives is the whole point.
func TestObjectionAtSupersededHeadTransitionsNothing(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	seedObservedPullRequest(t, store, "head-new")
	seedImplementAttribution(t, store)
	engine, _ := wedgeEngine(t, store)

	seedReviewJob(t, store, "review-stale-objection", "auditor", "head-old", "changes_requested", JobSucceeded)

	if err := engine.AdvanceJob(ctx, "review-stale-objection"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-9", TaskReadyToMerge)
	reason := staleHeadSkipReason(t, store, "review-stale-objection")
	if reason == "" {
		t.Fatal("no advance_skipped_stale_head event: a refusal must be recorded, not silent")
	}
	for _, want := range []string{"head-old", "head-new", "superseded head"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q is missing %q", reason, want)
		}
	}
}

// THE PRESERVED ARM: an objection at the CURRENT head still transitions the task
// exactly as before. This is the guard against "fixing" the stale case by
// refusing objections generally - the failure mode that would leave a real
// objection unable to stop a merge.
func TestObjectionAtCurrentHeadStillRequestsChanges(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	seedObservedPullRequest(t, store, "head-new")
	seedImplementAttribution(t, store)
	engine, _ := wedgeEngine(t, store)

	seedReviewJob(t, store, "review-live-objection", "auditor", "head-new", "changes_requested", JobSucceeded)

	if err := engine.AdvanceJob(ctx, "review-live-objection"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-9", TaskChangesRequested)
	if reason := staleHeadSkipReason(t, store, "review-live-objection"); reason != "" {
		t.Fatalf("a current-head objection must not be skipped: %q", reason)
	}
}

// MISSING EVIDENCE RETRIES, mirroring approvalSupersedesChangesRequested's own
// retryable split rather than inventing retry semantics: with no observed pull
// request row the objection's head cannot be compared, the row appears as soon
// as the daemon polls the PR, and settling here would drop a verdict about to
// become checkable.
func TestObjectionWithNoObservedPullRequestRowIsRetriedNotSettled(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	seedImplementAttribution(t, store)
	engine, _ := wedgeEngine(t, store)

	seedReviewJob(t, store, "review-unobserved", "auditor", "head-old", "changes_requested", JobSucceeded)

	err := engine.AdvanceJob(ctx, "review-unobserved")
	if err == nil {
		t.Fatal("AdvanceJob returned nil: an unconfirmable head must be retried, not settled")
	}
	if !strings.Contains(err.Error(), "no observed pull request row") {
		t.Fatalf("error = %v, want it to name the missing observation", err)
	}
	assertTaskState(t, store, "task-9", TaskReadyToMerge)
	if reason := staleHeadSkipReason(t, store, "review-unobserved"); reason != "" {
		t.Fatalf("a transient refusal must not record a terminal skip event: %q", reason)
	}
}

// AN UNBOUND OBJECTION STILL ADVANCES, pinned deliberately rather than left as a
// side effect of six tests written about ownership routing (#1900's shape: a
// property owned by one file, relied on by another, asserted nowhere).
//
// A CLI review dispatched as `gitmoot agent review <r> --repo o/r --pr N` with no
// --head-sha produces exactly this payload today, so this is real traffic rather
// than a fixture artefact. Admitting is the claim-nothing direction: refusing a
// headless APPROVAL fails safe because the PR does not merge, while refusing a
// headless OBJECTION fails PERMISSIVE - it drops a real complaint and leaves the
// PR in whatever merge-ward state it held.
func TestObjectionWithNoHeadStillRequestsChanges(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	seedObservedPullRequest(t, store, "head-new")
	seedImplementAttribution(t, store)
	engine, _ := wedgeEngine(t, store)

	seedReviewJob(t, store, "review-headless", "auditor", "", "changes_requested", JobSucceeded)

	if err := engine.AdvanceJob(ctx, "review-headless"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-9", TaskChangesRequested)
	if reason := staleHeadSkipReason(t, store, "review-headless"); reason != "" {
		t.Fatalf("a headless objection must advance, not be skipped: %q", reason)
	}
}
