package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// #1834: once a task reached changes_requested, an approval at a LATER head could
// never clear the merge gate. The approved arm handed runMergeGate a CONSTANT
// TaskReviewing, so PolicyMergeGate's ClaimTaskState CAS compared "reviewing"
// against a row reading "changes_requested" and refused forever, while the only
// re-arm helper (setReviewingIfNotChangesRequested) is one-way by design and
// correctly declines to erase a live objection. Four live tasks wedged.
//
// These tests drive the PRODUCTION entry point, Engine.AdvanceJob, against a
// PolicyMergeGate backed by the real store, so the CAS actually executes. A gate
// left nil short-circuits to setTaskState and would pin nothing.
func wedgeEngine(t *testing.T, store *db.Store) (Engine, *fakeMergeGateGitHub) {
	t.Helper()
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head-new", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	engine := testEngine(store)
	engine.MergeGate = PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}
	return engine, gh
}

// seedObservedPullRequest records what the forge last reported, which is where
// the rule reads the current head from. The daemon poll, the PR lifecycle and the
// merge gate all maintain this row in production.
func seedObservedPullRequest(t *testing.T, store *db.Store, head string) {
	t.Helper()
	if err := store.UpsertPullRequest(context.Background(), db.PullRequest{
		RepoFullName: "gitmoot/gitmoot",
		Number:       9,
		HeadBranch:   "task-9",
		BaseBranch:   "main",
		HeadSHA:      head,
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
}

// The merge gate refuses to merge without a durable implement row: it cannot
// establish independence otherwise. Without this the gate parks in
// awaiting_human_merge and the state CAS - the thing #1834 wedges on - never runs,
// which would make every test below vacuous.
func seedImplementAttribution(t *testing.T, store *db.Store) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "implementer", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head-new",
		TaskID:      "task-9",
		Result:      &AgentResult{Decision: "implemented", Summary: "implemented"},
	})
}

func seedWedgedTask(t *testing.T, store *db.Store) {
	t.Helper()
	if err := store.UpsertTask(context.Background(), db.Task{
		ID:           "task-9",
		RepoFullName: "gitmoot/gitmoot",
		Branch:       "task-9",
		State:        string(TaskChangesRequested),
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
}

func seedReviewJob(t *testing.T, store *db.Store, id, agent, head, decision string, state JobState) {
	t.Helper()
	payload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     head,
		TaskID:      "task-9",
	}
	if decision != "" {
		payload.Result = &AgentResult{Decision: decision, Summary: decision + " at " + head}
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID: id, Agent: agent, Type: "review", State: string(state), Payload: encoded,
	}, db.JobEvent{Kind: string(state), Message: "fixture"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
}

func heldReason(t *testing.T, store *db.Store, jobID string) string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "review_advance_held" {
			return event.Message
		}
	}
	return ""
}

// ACCEPTANCE 1: an approval bound to the CURRENT head admits the task out of
// changes_requested. This is the wedge itself; before the fix the task stayed in
// changes_requested forever and the PR could not merge.
func TestApprovalAtCurrentHeadClearsChangesRequested(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedWedgedTask(t, store)
	seedObservedPullRequest(t, store, "head-new")
	seedImplementAttribution(t, store)
	engine, gh := wedgeEngine(t, store)

	seedReviewJob(t, store, "review-old", "auditor", "head-old", "changes_requested", JobSucceeded)
	seedReviewJob(t, store, "review-new", "auditor", "head-new", "approved", JobSucceeded)

	if err := engine.AdvanceJob(ctx, "review-new"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v; the wedge surfaces here as ErrMergeTaskStateChanged", err)
	}
	// The MERGE is the observable, not merely "the state changed": the wedge lives
	// in ClaimTaskState inside executePullRequestMergeFenced, and every path that
	// stops short of the merge leaves the task in some other state without ever
	// exercising the CAS.
	if len(gh.merges) != 1 {
		t.Fatalf("merge calls = %d, want 1: an approval at the current head must clear changes_requested and merge (#1834); held=%q",
			len(gh.merges), heldReason(t, store, "review-new"))
	}
	if state := taskState(t, store, "task-9"); state == string(TaskChangesRequested) {
		t.Fatalf("task stayed in changes_requested after a merge")
	}
}

func taskState(t *testing.T, store *db.Store, taskID string) string {
	t.Helper()
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	return task.State
}

// ACCEPTANCE 2: an approval at a SUPERSEDED head must not clear the objection -
// a newer review job dates a newer head, so this approval reviewed an old tree.
func TestApprovalAtSupersededHeadLeavesChangesRequested(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedWedgedTask(t, store)
	seedObservedPullRequest(t, store, "head-new")
	seedImplementAttribution(t, store)
	engine, gh := wedgeEngine(t, store)

	seedReviewJob(t, store, "review-stale", "auditor", "head-old", "approved", JobSucceeded)
	// A re-review is already open at the newer head; it has not reported yet.
	seedReviewJob(t, store, "review-pending", "auditor", "head-new", "", JobRunning)

	if err := engine.AdvanceJob(ctx, "review-stale"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-9", TaskChangesRequested)
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %d, want 0: a superseded approval must not merge", len(gh.merges))
	}
	reason := heldReason(t, store, "review-stale")
	if !strings.Contains(reason, "superseded head") {
		t.Fatalf("held reason = %q, want it to name the superseded head; a silent no-op is what hid #1834", reason)
	}
}

// ACCEPTANCE 3: if the LATEST verdict at the current head is itself
// changes_requested, an earlier approval at that same head must not clear it.
func TestChangesRequestedAtCurrentHeadOutranksEarlierApproval(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedWedgedTask(t, store)
	seedObservedPullRequest(t, store, "head-new")
	seedImplementAttribution(t, store)
	engine, _ := wedgeEngine(t, store)

	seedReviewJob(t, store, "review-approve", "auditor", "head-new", "approved", JobSucceeded)
	seedReviewJob(t, store, "review-reject", "auditor", "head-new", "changes_requested", JobSucceeded)

	if err := engine.AdvanceJob(ctx, "review-approve"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-9", TaskChangesRequested)
	if reason := heldReason(t, store, "review-approve"); !strings.Contains(reason, "requested changes") {
		t.Fatalf("held reason = %q, want it to name the objection at the current head", reason)
	}
}

// ACCEPTANCE 4: the ambiguous same-head case, ruled EXPLICITLY rather than left
// to iteration order. Two DIFFERENT reviewers at the same head, one objecting and
// one approving: the objection wins. An approval must never merge over a peer's
// live objection; the objector re-reviews, or a new head supersedes them both.
func TestSameHeadObjectionFromAnotherReviewerBeatsApproval(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedWedgedTask(t, store)
	seedObservedPullRequest(t, store, "head-new")
	seedImplementAttribution(t, store)
	engine, _ := wedgeEngine(t, store)

	seedReviewJob(t, store, "review-a", "reviewer-a", "head-new", "changes_requested", JobSucceeded)
	seedReviewJob(t, store, "review-b", "reviewer-b", "head-new", "approved", JobSucceeded)

	if err := engine.AdvanceJob(ctx, "review-b"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-9", TaskChangesRequested)
	if reason := heldReason(t, store, "review-b"); !strings.Contains(reason, "requested changes") {
		t.Fatalf("held reason = %q, want the same-head objection named", reason)
	}
}

// A task that never objected keeps the pre-#1834 behaviour exactly: the gate is
// claimed as reviewing. This is the guard against "fixing" the wedge by loosening
// the claim for every task.
func TestApprovalOnReviewingTaskStillClaimsReviewing(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9", State: string(TaskReviewing),
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	seedObservedPullRequest(t, store, "head-new")
	seedImplementAttribution(t, store)
	engine, _ := wedgeEngine(t, store)
	seedReviewJob(t, store, "review-new", "auditor", "head-new", "approved", JobSucceeded)

	if err := engine.AdvanceJob(ctx, "review-new"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if got := heldReason(t, store, "review-new"); got != "" {
		t.Fatalf("a reviewing task must not be held: %q", got)
	}
	task, err := store.GetTask(ctx, "task-9")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State == string(TaskReviewing) {
		t.Fatal("task stayed in reviewing; the approval should have advanced it")
	}
}

// The missing-pull_requests-row branch, in BOTH directions (#1871 review P1/P3).
// Every other test here seeds the observed row, so without these the refusal path
// has no coverage - and the first version of it ADMITTED whenever some review had
// objected at a different head, which carries no ordering at all and could merge
// over a live objection.
func TestApprovalWithoutAnObservedPullRequestRowIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name            string
		objectionAtHead string
	}{
		// The dangerous case: an objection elsewhere used to be read as "the tree
		// moved on", which it is not - nothing orders the two heads.
		{name: "objection at another head", objectionAtHead: "head-old"},
		{name: "objection at this head", objectionAtHead: "head-new"},
		{name: "no objection recorded", objectionAtHead: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openEngineStore(t)
			seedWedgedTask(t, store)
			seedImplementAttribution(t, store)
			// Deliberately NO seedObservedPullRequest: the forge head is unknown.
			engine, gh := wedgeEngine(t, store)
			if tc.objectionAtHead != "" {
				seedReviewJob(t, store, "review-objection", "auditor", tc.objectionAtHead, "changes_requested", JobSucceeded)
			}
			seedReviewJob(t, store, "review-new", "auditor", "head-new", "approved", JobSucceeded)

			// The hold must be RETRYABLE, not settled: AdvanceJob returns an error so
			// the advancement stays unreconciled and the daemon's advance-retry
			// re-drives it once the pull_requests row lands. Returning nil here would
			// record the approval as advanced and nothing would ever re-drive it -
			// measured as a recovery wedge in the #1871 round-3 review.
			err := engine.AdvanceJob(ctx, "review-new")
			if err == nil {
				t.Fatal("AdvanceJob returned nil: an unconfirmable head must leave the advancement retryable, not settle it")
			}
			if !strings.Contains(err.Error(), "no observed pull request row") {
				t.Fatalf("error = %v, want it to name the missing pull request row", err)
			}
			assertTaskState(t, store, "task-9", TaskChangesRequested)
			if len(gh.merges) != 0 {
				t.Fatalf("merge calls = %d, want 0: an unconfirmed current head must never admit an approval", len(gh.merges))
			}
			// And it must NOT write a durable held event on this path: it is re-entered
			// every tick, and a row per tick is what grew job_events to ~1.8M once.
			if reason := heldReason(t, store, "review-new"); reason != "" {
				t.Fatalf("retryable hold wrote a review_advance_held event (%q); the deduped advance_retry marker carries it instead", reason)
			}
		})
	}
}

// ...and once the row appears, the same approval is admitted. This is the pair to
// the refusal above: it proves the refusal is about the MISSING EVIDENCE and not a
// blanket block that would re-wedge every task.
func TestApprovalAdmittedOnceThePullRequestRowAppears(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedWedgedTask(t, store)
	seedImplementAttribution(t, store)
	engine, gh := wedgeEngine(t, store)
	seedReviewJob(t, store, "review-old", "auditor", "head-old", "changes_requested", JobSucceeded)
	seedReviewJob(t, store, "review-new", "auditor", "head-new", "approved", JobSucceeded)

	if err := engine.AdvanceJob(ctx, "review-new"); err == nil {
		t.Fatal("AdvanceJob (no row) returned nil; the hold must stay retryable")
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merged with no observed pull request row")
	}
	seedObservedPullRequest(t, store, "head-new")
	if err := engine.AdvanceJob(ctx, "review-new"); err != nil {
		t.Fatalf("AdvanceJob (row present) returned error: %v", err)
	}
	if len(gh.merges) != 1 {
		t.Fatalf("merge calls = %d, want 1 once the current head is observable", len(gh.merges))
	}
}
