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

// PIN 1 - AN OBJECTION WITH A HEAD BUT NO OBSERVED PULL REQUEST ROW STILL
// ADVANCES. This is the case a retracted ruling would have refused transiently,
// and its ABSENCE from the suite is what let that ruling look safe.
//
// Refusing here would be a LIVENESS loss, not a merge risk. An earlier version
// of this comment claimed the opposite - that refusing would let the gate "merge
// on an approval over a real current-head objection nobody recorded" - and
// #1903's third review round found that claim still living here after the
// function comment had been corrected.
//
// PolicyMergeGate blocks on the objection ROW independently of task state: it
// derives the head live (merge_gate.go:288 GetPullRequest -> :311), enters
// unconditionally (:320 -> :439), selects every review row whose payload.HeadSHA
// equals that head (:809-816), and returns mergeBlocked for changes_requested,
// blocked or failed (:895-898). Refusing the TASK transition does not un-record
// the REVIEW ROW, so it cannot open a merge. What it does cost is the
// conservative transition and the inline fix pass, withheld from an objection
// nobody can show is stale.
func TestObjectionWithNoObservedPullRequestRowStillRequestsChanges(t *testing.T) {
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

	if err := engine.AdvanceJob(ctx, "review-unobserved"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-9", TaskChangesRequested)
	if reason := staleHeadSkipReason(t, store, "review-unobserved"); reason != "" {
		t.Fatalf("an unconfirmable head must admit, not skip: %q", reason)
	}
}

// PIN 2 - A STALE OBJECTION DISPATCHES NO FIX LEG. dispatchFix is called INLINE
// from this arm, so refusing the transition without refusing the dispatch would
// leave the worse half running: a fix job carrying findings about a commit the
// branch has already moved past is wrong work, not late work (#1730's family).
func TestStaleObjectionDispatchesNoFixLeg(t *testing.T) {
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
	enableAutoFix(t, store, 9)
	engine, _ := wedgeEngine(t, store)

	before, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	seedReviewJob(t, store, "review-stale-nofix", "auditor", "head-old", "changes_requested", JobSucceeded)

	if err := engine.AdvanceJob(ctx, "review-stale-nofix"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	after, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	// The only new row may be the review fixture itself: no implement job.
	for _, job := range after {
		if job.Type != "implement" {
			continue
		}
		known := false
		for _, existing := range before {
			if existing.ID == job.ID {
				known = true
				break
			}
		}
		if !known {
			t.Fatalf("a stale objection dispatched a fix leg: %s", job.ID)
		}
	}
	if reason := staleHeadSkipReason(t, store, "review-stale-nofix"); reason == "" {
		t.Fatal("no advance_skipped_stale_head event recorded for the stale objection")
	}
}

// AN UNBOUND OBJECTION STILL ADVANCES, pinned deliberately rather than left as a
// side effect of six tests written about ownership routing (#1900's shape: a
// property owned by one file, relied on by another, asserted nowhere).
//
// A CLI review dispatched as `gitmoot agent review <r> --repo o/r --pr N` with no
// --head-sha produces exactly this payload today, so this is real traffic rather
// than a fixture artefact.
//
// THIS ARM'S RATIONALE IS SAFETY, NOT LIVENESS, AND IT IS THE ONE PLACE THAT
// DIFFERS. The gate's protection does not extend to a headless row: merge_gate.go
// SKIPS any review whose head is empty (`if reviewHead == "" || reviewHead !=
// headSHA { continue }`, :809-816), so a headless objection is invisible to it.
// For an objection that HAS a head the gate blocks independently and refusing
// costs only liveness - but refusing a HEADLESS objection drops a real complaint
// that nothing else will catch, leaving the PR in whatever merge-ward state it
// held. Refusing a headless APPROVAL still fails safe, because nothing merges.
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
