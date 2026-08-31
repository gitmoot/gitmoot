package daemon

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedBlockedReviewTask stages the exact live shape from #1562: the local review
// task review-pr-1699-3f3a1026, blocked by the merge gate while its pull request
// is still open.
//
// The empty Branch is load-bearing, not incidental. A local review task
// deliberately carries no branch so it cannot collide with the implement task that
// owns (repo, head branch), and that is precisely why the wedge survived: every
// branch-keyed path — daemon.pullRequestReadyToMerge, handleReadyToMergeWorkflow,
// reconcilePROpenTasks — resolves tasks from pull.HeadRef and cannot see this row.
// A fixture with a branch would be released by reconcilePROpenTasks instead and
// would therefore pass against a fix that never runs.
func seedBlockedReviewTask(t *testing.T, store *db.Store, repo github.Repository, class workflow.MergeBlockClass, reason string) github.PullRequest {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "review-pr-1699-3f3a1026",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1699",
		Title:        "Review PR #1699",
		State:        string(workflow.TaskBlocked),
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertMergeGate(ctx, db.MergeGate{
		RepoFullName: repo.FullName(),
		PullRequest:  1699,
		State:        "blocked",
		Reason:       reason,
		BlockClass:   int(class),
	}); err != nil {
		t.Fatalf("UpsertMergeGate returned error: %v", err)
	}
	// The merge gate attributes its own block on the task (blockMergeGate), which is
	// what distinguishes it from a coordinator block_parent or quorum failure
	// reaching the same state through the shared e.block helper.
	if err := store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID:  "review-pr-1699-3f3a1026",
		Kind:    "merge_gate_blocked",
		ToState: string(workflow.TaskBlocked),
		Reason:  reason,
	}); err != nil {
		t.Fatalf("AddTaskEvent(merge_gate_blocked) returned error: %v", err)
	}
	return github.PullRequest{
		Number:  1699,
		Title:   "Review PR #1699",
		State:   "open",
		URL:     "https://github.com/gitmoot/gitmoot/pull/1699",
		HeadRef: "task-1699",
		BaseRef: "main",
		HeadSHA: "3f3a1026",
	}
}

func blockedReviewTaskDaemon(t *testing.T, store *db.Store, repo github.Repository, pull github.PullRequest) (Daemon, *fakeWorkflowMergeGate) {
	t.Helper()
	client := &fakeGitHub{
		pulls:    []github.PullRequest{pull},
		comments: map[int64][]github.IssueComment{pull.Number: {}},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	return Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}, gate
}

func hasTaskEventKind(events []db.TaskEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

// TestPollOnceLeavesTaskBlockedByDelegationFailureBlocked is the cause-attribution
// direction, found by gm-omp-fanout while we coordinated the PR #1731 collision.
//
// e.block is SHARED: the merge gate uses it, and so do the coordinator failure
// paths — block_parent and the vote/quorum synthesis gates in
// engine_continuation_synthesis.go. So `state == blocked` says nothing about WHICH
// mechanism blocked the task. A task carrying a stale transient merge-gate row from
// an earlier evaluation, then blocked by a delegation quorum failure, satisfies
// every other test in the reconciler — and releasing it would discard a quorum
// failure as though it were self-clearing infrastructure. The gate row describes the
// CONDITION; only the task's latest blocking event establishes the CURRENT CAUSE.
//
// Mutant M5 ("cause ignored"): delete the mergeGateOwnsCurrentBlock guard from
// reconcileTransientlyBlockedMergeGates. Only this test fails; every other
// direction, including both two-poll tests, still passes.
func TestPollOnceLeavesTaskBlockedByDelegationFailureBlocked(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	// A genuine transient gate block happened earlier and its row is still on disk.
	pull := seedBlockedReviewTask(t, store, repo, workflow.MergeBlockTransient, "local worktree is not clean")
	// Then a delegation quorum failure blocked the task through the shared helper.
	// It is LATER, so it is the current cause.
	if err := store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID:  "review-pr-1699-3f3a1026",
		Kind:    "quorum_unmet",
		ToState: string(workflow.TaskBlocked),
		Reason:  "2 of 3 delegation children failed",
	}); err != nil {
		t.Fatalf("AddTaskEvent(quorum_unmet) returned error: %v", err)
	}
	daemon, gate := blockedReviewTaskDaemon(t, store, repo, pull)

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked: a quorum failure is not a self-clearing merge-gate condition", task.State)
	}
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate evaluations = %d, want 0 for a task blocked by a delegation failure", len(gate.requests))
	}
	events, err := store.ListTaskEvents(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("ListTaskEvents returned error: %v", err)
	}
	if hasTaskEventKind(events, "merge_gate_transient_retry") {
		t.Fatalf("task events = %+v, want no transient release recorded", events)
	}
}

// TestPollOnceHealsPreAttributionBlockOnce is the UPGRADE path, raised by
// gm-omp-fanout: every task blocked before this change has no attributed blocking
// event, because neither the merge gate nor the synthesis gates wrote one — both
// reached blocked through the shared e.block. A strict "no event means not
// gate-owned" rule would refuse exactly the population that motivated #1562,
// including the 95-minute review-pr-1699-3f3a1026 wedge, and the feature would
// clear only blocks created after the deploy.
//
// So a row with a transient gate row and NO competing blocking event is healed
// once. "Once" is structural rather than counted: the release writes
// merge_gate_transient_retry, so the row is no longer unattributed, and any later
// real block arrives through an attributed path.
//
// Mutant M6 ("no upgrade path"): make mergeGateOwnsCurrentBlock return
// (false, false, nil) for an unattributed row. Only this test fails — every other
// direction, including the delegation-failure refusal, still passes, which is what
// makes the two behaviours distinguishable rather than a matter of opinion.
func TestPollOnceHealsPreAttributionBlockOnce(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	// Seeded WITHOUT attribution: exactly what the old binary left behind.
	pull := seedBlockedReviewTaskAttributed(t, store, repo, workflow.MergeBlockTransient, "local worktree is not clean", false)
	daemon, _ := blockedReviewTaskDaemon(t, store, repo, pull)

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	task, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("task state = %q, want a pre-attribution wedge healed to ready_to_merge", task.State)
	}
	events, err := store.ListTaskEvents(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("ListTaskEvents returned error: %v", err)
	}
	if !hasTaskEventKind(events, "merge_gate_transient_retry") {
		t.Fatalf("task events = %+v, want the heal recorded so the row is no longer unattributed", events)
	}
}

// TestPollOnceLeavesTaskBlockedByUnknownMechanismBlocked closes the guard over
// mechanisms this change does not know about. Raised by a review advisory: a fixed
// list of blocking kinds is not closed under future blockers, so a NEW mechanism
// blocking a task would simply not match, an older merge_gate_blocked would remain
// the newest RECOGNISED event, and the exit would release a block it did not cause.
//
// The event here carries ToState=blocked with a kind this code has never heard of,
// which is exactly the shape a future blocker will have.
//
// Mutant M7 ("kind list only"): drop the `event.ToState == blocked` arm from
// mergeGateOwnsCurrentBlock so only the enumerated kinds count as blocking. Only
// this test fails.
func TestPollOnceLeavesTaskBlockedByUnknownMechanismBlocked(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := seedBlockedReviewTask(t, store, repo, workflow.MergeBlockTransient, "local worktree is not clean")
	if err := store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID:  "review-pr-1699-3f3a1026",
		Kind:    "some_future_guard_blocked",
		ToState: string(workflow.TaskBlocked),
		Reason:  "a mechanism this change has never heard of",
	}); err != nil {
		t.Fatalf("AddTaskEvent(some_future_guard_blocked) returned error: %v", err)
	}
	daemon, gate := blockedReviewTaskDaemon(t, store, repo, pull)

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	task, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked: the newest blocking event is not the merge gate's", task.State)
	}
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate evaluations = %d, want 0", len(gate.requests))
	}
}
// TestPollOnceReleasesTransientlyBlockedReviewTask is the release direction of
// #1562. A task blocked on "local worktree is not clean" — a condition the gate
// itself classified MergeBlockTransient — must regain an exit from the ordinary
// poll, WITHOUT a human merging the pull request. ready_to_merge is that exit: it
// is the documented retry authority and it is also the state `task resume-work`
// accepts, so the row becomes reachable both automatically and by an operator.
//
// Mutant M1 ("releases nothing"): delete the
// reconcileTransientlyBlockedMergeGates call from PollOnce, or invert its class
// test to MergeBlockQuality. The task then stays blocked and this test fails,
// while TestPollOnceLeavesQualityBlockedReviewTaskBlocked still passes.
func TestPollOnceReleasesTransientlyBlockedReviewTask(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := seedBlockedReviewTask(t, store, repo, workflow.MergeBlockTransient, "local worktree is not clean")
	daemon, _ := blockedReviewTaskDaemon(t, store, repo, pull)

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("task state = %q, want ready_to_merge once the transient block is re-evaluated", task.State)
	}
	events, err := store.ListTaskEvents(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("ListTaskEvents returned error: %v", err)
	}
	if !hasTaskEventKind(events, "merge_gate_transient_retry") {
		t.Fatalf("task events = %+v, want an auditable merge_gate_transient_retry release", events)
	}
}

// TestSecondPollEvaluatesGateAfterTransientRelease is the reviewer-requested
// completion of the release direction, and the one that distinguishes a REAL exit
// from a merely cosmetic state change. Releasing the row to ready_to_merge is
// worthless if nothing consumes that state: a branchless review task is invisible
// to a pull.HeadRef lookup, so before lookupReadyPullRequestTask existed the
// released task sat in ready_to_merge and the gate was never re-run — the
// automatic self-clearing retry #1562 asks for did not happen, only manual
// `task resume-work` recovery.
//
// Poll 1 releases; poll 2 MUST reach the merge gate.
//
// Mutant M3 ("released but never consumed"): revert
// pullRequestReadyToMerge/handleReadyToMergeWorkflow to
// d.lookupPullRequestTask(ctx, d.Repo.FullName(), pull.HeadRef). Poll 1 still
// releases and every other test in this file still passes; only this one fails,
// which is exactly the gap the reviewer found at head d73d3d9c.
func TestSecondPollEvaluatesGateAfterTransientRelease(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := seedBlockedReviewTask(t, store, repo, workflow.MergeBlockTransient, "local worktree is not clean")
	daemon, gate := blockedReviewTaskDaemon(t, store, repo, pull)

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("poll 1 PollOnce returned error: %v", err)
	}
	task, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("GetTask after poll 1 returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("poll 1 task state = %q, want ready_to_merge", task.State)
	}

	ready, err := daemon.pullRequestReadyToMerge(ctx, pull)
	if err != nil {
		t.Fatalf("pullRequestReadyToMerge returned error: %v", err)
	}
	if !ready {
		t.Fatalf("pullRequestReadyToMerge = false for a released branchless review task; the ready path cannot see it")
	}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("poll 2 PollOnce returned error: %v", err)
	}
	if len(gate.requests) == 0 {
		t.Fatalf("merge gate evaluations after release = 0, want the released task re-evaluated by a later poll")
	}
	for _, request := range gate.requests {
		if request.PullRequest != 1699 {
			t.Fatalf("merge gate evaluated the wrong pull request: %+v", request)
		}
	}
	// The ref must name the review task, never the branch's canonical implement
	// task: a branchless row carrying the PR branch would advance the wrong task.
	events, err := store.ListTaskEvents(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("ListTaskEvents returned error: %v", err)
	}
	if !hasTaskEventKind(events, "merge_gate_transient_retry") {
		t.Fatalf("task events = %+v, want the release recorded on the review task itself", events)
	}
}

// TestSecondPollEvaluatesGateWhenBranchOwnerIsNotReady is the two-row form of the
// release direction, and it is the shape production actually has. Two rows
// legitimately describe one PR: the implement task owning (repo, head branch), and
// the branchless local review task keyed by its durable id. That coexistence is
// already modeled at external_merge_reconcile_test.go:190-196.
//
// A branch-FIRST ready lookup strands the released review task here: it finds the
// implementation task, sees pull_request_open, reports not-ready, and performs zero
// merge-gate evaluations, so the transient release is again a state change nothing
// consumes. The one-row fixture cannot see this, which is why it passed against
// the branch-first version at head 9ba794d8.
//
// Mutant M4 ("branch owner wins"): in lookupReadyPullRequestTask, return branchTask
// whenever branchFound, before the branchless scan. Only this test fails; the
// single-row two-poll test still passes.
func TestSecondPollEvaluatesGateWhenBranchOwnerIsNotReady(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := seedBlockedReviewTask(t, store, repo, workflow.MergeBlockTransient, "local worktree is not clean")
	// The implement task owns the unique (repo, branch) slot and is NOT ready.
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "implementation-task",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1699",
		Title:        "Implement 1699",
		State:        string(workflow.TaskPullRequestOpen),
		Branch:       "task-1699",
	}); err != nil {
		t.Fatalf("UpsertTask(implementation-task) returned error: %v", err)
	}
	daemon, gate := blockedReviewTaskDaemon(t, store, repo, pull)

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("poll 1 PollOnce returned error: %v", err)
	}
	review, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("GetTask(review) after poll 1 returned error: %v", err)
	}
	if review.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("poll 1 review task state = %q, want ready_to_merge", review.State)
	}

	ready, err := daemon.pullRequestReadyToMerge(ctx, pull)
	if err != nil {
		t.Fatalf("pullRequestReadyToMerge returned error: %v", err)
	}
	if !ready {
		t.Fatalf("pullRequestReadyToMerge = false: the released review task is hidden by the non-ready branch owner")
	}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("poll 2 PollOnce returned error: %v", err)
	}
	if len(gate.requests) == 0 {
		t.Fatalf("merge gate evaluations after release = 0, want the released review task re-evaluated despite the branch owner")
	}
	for _, request := range gate.requests {
		if request.TaskID != "review-pr-1699-3f3a1026" {
			t.Fatalf("merge gate evaluated task %q, want the released review task, never the branch owner", request.TaskID)
		}
	}
	// The implementation task must be untouched: this path may not advance work it
	// does not own.
	implementation, err := store.GetTask(ctx, "implementation-task")
	if err != nil {
		t.Fatalf("GetTask(implementation-task) returned error: %v", err)
	}
	if implementation.State != string(workflow.TaskPullRequestOpen) {
		t.Fatalf("implementation task state = %q, want pull_request_open left alone", implementation.State)
	}
}

// TestPollOnceLeavesQualityBlockedReviewTaskBlocked is the direction that stops
// the obvious wrong fix. An authoritative quality rejection must stay blocked.
//
// Mutant M2 ("releases everything"): drop the BlockClass condition from
// reconcileTransientlyBlockedMergeGates so any blocked row with a blocked gate is
// released. This test then fails while
// TestPollOnceReleasesTransientlyBlockedReviewTask still passes.
func TestPollOnceLeavesQualityBlockedReviewTaskBlocked(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := seedBlockedReviewTask(t, store, repo, workflow.MergeBlockQuality, "external CI failed")
	daemon, _ := blockedReviewTaskDaemon(t, store, repo, pull)

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked to persist for an authoritative quality rejection", task.State)
	}
}

// TestPollOnceLeavesUnclassifiedBlockedReviewTaskBlocked covers the migration
// boundary: every merge-gate row that predates the block_class column reads 0
// (MergeBlockNone). Those rows must not be released, because an unclassified block
// is not evidence of a self-clearing condition.
func TestPollOnceLeavesUnclassifiedBlockedReviewTaskBlocked(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := seedBlockedReviewTask(t, store, repo, workflow.MergeBlockNone, "legacy row with no class")
	daemon, _ := blockedReviewTaskDaemon(t, store, repo, pull)

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want an unclassified legacy block to stay blocked", task.State)
	}
}

// TestPollOnceLeavesTransientBlockOnClosedPullRequestBlocked pins the open-PR
// requirement. A closed PR cannot clear a transient condition, and releasing it to
// ready_to_merge would hand a moot subject to the merge path; that population
// belongs to the external-merge and closed-reviewing reconcilers.
func TestPollOnceLeavesTransientBlockOnClosedPullRequestBlocked(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	// Seed the blocked row, then present a poll in which PR #1699 is NOT open.
	_ = seedBlockedReviewTask(t, store, repo, workflow.MergeBlockTransient, "local worktree is not clean")
	closed := github.PullRequest{Number: 1699, State: "closed", HeadRef: "task-1699", BaseRef: "main", HeadSHA: "3f3a1026"}
	client := &fakeGitHub{
		pulls:         []github.PullRequest{},
		comments:      map[int64][]github.IssueComment{},
		pullsByNumber: map[int64]github.PullRequest{1699: closed},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task state = %q, want blocked while the pull request is not open", task.State)
	}
}

// TestMergeGateBlockClassSurvivesTheBlockingCallStack pins the #1562 root cause
// directly: before this fix the class existed only on the returned MergeDecision,
// so no exit path outside the blocking call stack could read it.
func TestMergeGateBlockClassSurvivesTheBlockingCallStack(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	for _, tt := range []struct {
		name  string
		class workflow.MergeBlockClass
	}{
		{"transient", workflow.MergeBlockTransient},
		{"quality", workflow.MergeBlockQuality},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := store.UpsertMergeGate(ctx, db.MergeGate{
				RepoFullName: repo.FullName(),
				PullRequest:  4242,
				State:        "blocked",
				Reason:       tt.name,
				BlockClass:   int(tt.class),
			}); err != nil {
				t.Fatalf("UpsertMergeGate returned error: %v", err)
			}
			gate, err := store.GetMergeGate(ctx, repo.FullName(), 4242)
			if err != nil {
				t.Fatalf("GetMergeGate returned error: %v", err)
			}
			if gate.BlockClass != int(tt.class) {
				t.Fatalf("persisted block class = %d, want %d", gate.BlockClass, int(tt.class))
			}
		})
	}
	// A pending row is not a block: it must store the zero class so it can never be
	// selected for release.
	if err := store.UpsertMergeGate(ctx, db.MergeGate{
		RepoFullName: repo.FullName(),
		PullRequest:  4242,
		State:        "pending",
		Reason:       "waiting on CI",
	}); err != nil {
		t.Fatalf("UpsertMergeGate(pending) returned error: %v", err)
	}
	gate, err := store.GetMergeGate(ctx, repo.FullName(), 4242)
	if err != nil {
		t.Fatalf("GetMergeGate returned error: %v", err)
	}
	if gate.BlockClass != int(workflow.MergeBlockNone) {
		t.Fatalf("pending row block class = %d, want MergeBlockNone", gate.BlockClass)
	}
}
