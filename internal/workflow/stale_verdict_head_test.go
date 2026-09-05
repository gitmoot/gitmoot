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
// PolicyMergeGate reaches the objection ROW independently of task state, by
// EITHER of two paths - naming only the first is what produced the P1 on the
// comment below this one:
//
//   - STRICT evaluated-head population: the head is derived live
//     (merge_gate.go:288 GetPullRequest -> :311), review evaluation is entered
//     unconditionally (:320 -> :439), rows whose payload.HeadSHA equals that head
//     are collected (:809-816), and changes_requested/blocked/failed among them
//     returns mergeBlocked (:895-898).
//   - LATEST-ROUND fallback, taken only when that population is EMPTY
//     (:984-1004 select the latest round over all taskReviews, :1052-1058 call
//     ensureReviewMatchesHead before any decision is inspected, :1635-1656 refuse
//     a NON-EMPTY head that mismatches). An ABSENT head is governed by the
//     integration markers instead, not by this arm - see
//     TestObjectionWithNoHeadStillRequestsChanges below.
//
// This arm's row HAS a head, so it lands in the strict population or, if
// superseded there, is refused by the fallback. Refusing the TASK transition does
// not un-record the REVIEW ROW, so it cannot open a merge. What it costs is the
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
// WHY ADMITTING MATTERS HERE IS CONDITIONAL ON THE REVIEW POPULATION, not a
// property of headlessness. Headlessness is EXCLUSION from the strict
// evaluated-head population (merge_gate.go:809-816, whose empty-head arm is first
// and independent of the equality arm), not invisibility to the gate:
//
//   - No current-head review exists -> the strict population is empty, so the
//     LATEST-ROUND fallback runs (:984-1004) and calls ensureReviewMatchesHead on
//     each eligible row BEFORE any decision is inspected (:1052-1058). What
//     happens next depends on the INTEGRATION MARKERS, and this branch is
//     first-class rather than a footnote: a headless row lacking BOTH
//     DelegationID and WorktreePath is refused at :1635-1656 ("does not record a
//     head SHA; rerun review"), while an integration-marked headless row is
//     ACCEPTED by isIntegrationWorktreeReview (:1662-1664) and evaluated on its
//     decision. Measured on this host's store when this comment was written: 179
//     headless review rows, of which 164 carry both markers and take the ACCEPT
//     path and 15 take "rerun review". So "the fallback refuses headless rows" is
//     NOT a statement this code supports; only the marker test is.
//   - A current-head review exists -> the strict population is non-empty, the
//     fallback never runs, and a headless objection does not block that merge.
//     THAT is the case this arm's admit exists for.
//
// These are the branches the CODE TESTS FOR, not an exhaustive account of how
// rows arise: 1,222 rows in the same store carry WorktreePath with no
// DelegationID and therefore satisfy neither marker branch.
//
// Both branches are pinned, and both fixtures stop short of this exact row:
// TestPolicyMergeGateBlocksLegacyReviewWithoutHeadSHA (merge_gate_test.go:4939)
// pins the head-check rejection, but its headless row carries decision
// "approved" - the rejection is decision-agnostic BY CODE PATH, because the head
// check at :1052-1058 runs before the decision switch, so the OBJECTION variant
// follows from shared code rather than from a fixture.
// TestPolicyMergeGateHeadlessIntegrationObjectionDoesNotMatchEveryHead
// (merge_gate_test.go:4862) pins the strict-population/skip mechanism, but its
// headless objection carries DelegationID + WorktreePath, i.e. the
// integration-worktree class ensureReviewMatchesHead deliberately ADMITS - so it
// does not pin the ordinary-CLI-headless variant.
//
// Refusing a headless APPROVAL still fails safe, because nothing merges while the
// doubt stands.
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
