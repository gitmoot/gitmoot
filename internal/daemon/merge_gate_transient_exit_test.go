package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
	return seedBlockedReviewTaskAttributed(t, store, repo, class, reason, true)
}

func seedBlockedReviewTaskAttributed(t *testing.T, store *db.Store, repo github.Repository, class workflow.MergeBlockClass, reason string, attribute bool) github.PullRequest {
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
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-1699", PullRequest: 1699,
		HeadSHA: "3f3a1026", TaskID: "review-pr-1699-3f3a1026",
		Result: &workflow.AgentResult{Decision: "approved", Summary: "approved current head"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: "review-current-1699", Agent: "reviewer", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(payload),
	}); err != nil {
		t.Fatalf("CreateJob(review-current-1699) returned error: %v", err)
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
	if attribute {
		if err := store.AddTaskEvent(ctx, db.TaskEvent{
			TaskID:  "review-pr-1699-3f3a1026",
			Kind:    "merge_gate_blocked",
			ToState: string(workflow.TaskBlocked),
			Reason:  reason,
		}); err != nil {
			t.Fatalf("AddTaskEvent(merge_gate_blocked) returned error: %v", err)
		}
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

type delayedClearanceMergeGateGit struct {
	clean  bool
	checks int
}

func (g *delayedClearanceMergeGateGit) WorktreeClean(context.Context) (bool, error) {
	g.checks++
	return g.clean, nil
}

func (*delayedClearanceMergeGateGit) UpdateBase(context.Context, string, string) error {
	return nil
}

func hasTaskEventKind(events []db.TaskEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func seedReadyBranchlessReviewTask(t *testing.T, store *db.Store, repo github.Repository, taskID string, pull github.PullRequest, headSHA string) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertTask(ctx, db.Task{
		ID: taskID, RepoFullName: repo.FullName(), State: string(workflow.TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: repo.FullName(), Branch: pull.HeadRef, PullRequest: int(pull.Number),
		HeadSHA: headSHA, TaskID: taskID,
		Result: &workflow.AgentResult{Decision: "approved", Summary: "approved"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: "review-" + taskID, Agent: "reviewer", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(payload),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPollOnceLeavesTaskBlockedByDelegationFailureBlocked is the cause-attribution
// direction, found by gm-omp-fanout while we coordinated the PR #1731 collision.
//
// All real block paths now converge on the shared blockTask choke point, but the
// latest ownership event still matters: a task carrying a stale transient
// merge-gate row and then blocked by a delegation quorum failure satisfies every
// state/gate check in the reconciler. Releasing it would discard a quorum failure
// as though it were self-clearing infrastructure. The gate row describes the
// CONDITION; only the task's latest blocking event establishes the CURRENT CAUSE.
//
// Compile-valid mutant M5 ("cause ignored"): force the ownership result to true.
// This test and the unknown-blocker direction both fail.
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
// gm-omp-fanout: tasks blocked by an old binary may have no ownership event. A
// strict "no event means not gate-owned" rule would refuse exactly the population
// that motivated #1562, including the 95-minute review-pr-1699-3f3a1026 wedge.
//
// A row with a transient gate row and no competing blocking event is healed once.
// The release writes merge_gate_transient_retry, and the reconciler treats that
// durable marker as a consumed retry even if a later gate evaluation blocks the
// task again.
//
// Compile-valid mutant M6 ("no upgrade path"): return unattributed=false for a
// row with no blocking event. This test fails while attributed release remains.
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
// Compile-valid mutant M4 ("branch owner wins"): return branchTask whenever
// branchErr == nil before the branchless scan. This and the current-head
// branchless selection test fail; the single-row two-poll direction still passes.
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

func TestTransientMergeGateRetryRecoversAfterDelayedClearanceAndStaysBounded(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := seedBlockedReviewTask(t, store, repo, workflow.MergeBlockTransient, "local worktree is not clean")
	initialTask, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
	if err != nil {
		t.Fatal(err)
	}
	implementPayload, err := json.Marshal(workflow.JobPayload{
		Repo: repo.FullName(), Branch: pull.HeadRef, PullRequest: int(pull.Number),
		HeadSHA: pull.HeadSHA, TaskID: initialTask.ID,
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "implemented current head"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: "implement-current-1699", Agent: "implementer", Type: "implement",
		State: string(workflow.JobSucceeded), Payload: string(implementPayload),
	}); err != nil {
		t.Fatalf("CreateJob(implement-current-1699) returned error: %v", err)
	}
	baseClient := &fakeGitHub{
		pulls:    []github.PullRequest{pull},
		comments: map[int64][]github.IssueComment{pull.Number: {}},
	}
	client := &mergeGateRaceGitHub{
		fakeGitHub: baseClient,
		checks: []github.PullRequestCheck{{
			Name: "ci", Bucket: "pass", State: "SUCCESS",
		}},
		statuses: []github.CommitStatusInput{{
			Repo: repo, SHA: pull.HeadSHA, State: "success", Context: "ci",
		}},
		statusSucceeded: []bool{true},
	}
	git := &delayedClearanceMergeGateGit{}
	gate := &workflow.PolicyMergeGate{
		AutoMerge: true, Store: store, GitHub: client, Git: git,
	}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	now := time.Now().UTC().Add(time.Hour)
	daemon := Daemon{
		Repo: repo, Store: store, GitHub: client, Workflow: &engine,
		Now: func() time.Time { return now },
	}
	poll := func(label string) {
		t.Helper()
		pollErr := daemon.PollOnce(ctx)
		var blocked workflow.BlockedError
		if pollErr != nil && !errors.As(pollErr, &blocked) {
			t.Fatalf("%s: %v", label, pollErr)
		}
	}

	for attempt := 1; attempt <= 3; attempt++ {
		poll(fmt.Sprintf("release %d", attempt))
		poll(fmt.Sprintf("dirty evaluation %d", attempt))
		task, loadErr := store.GetTask(ctx, initialTask.ID)
		if loadErr != nil || task.State != string(workflow.TaskBlocked) {
			events, _ := store.ListTaskEvents(ctx, initialTask.ID)
			t.Fatalf("attempt %d task=%+v err=%v checks=%d auto_merge=%v events=%+v, want still blocked",
				attempt, task, loadErr, git.checks, gate.AutoMerge, events)
		}
		now = now.Add(transientMergeGateRetryInterval + time.Second)
	}
	if git.checks != 3 {
		t.Fatalf("dirty worktree evaluations = %d, want 3 beyond the former lifetime budget", git.checks)
	}
	blockedTask, err := store.GetTask(ctx, initialTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blockedTask.UpdatedAt != initialTask.UpdatedAt {
		t.Fatalf("blocked age refreshed from %q to %q across transient retries", initialTask.UpdatedAt, blockedTask.UpdatedAt)
	}

	git.clean = true
	poll("release after real condition clearance")
	poll("merge after real condition clearance")
	mergedTask, err := store.GetTask(ctx, initialTask.ID)
	if err != nil || mergedTask.State != string(workflow.TaskMerged) {
		t.Fatalf("task after delayed clearance = %+v err=%v, want merged", mergedTask, err)
	}
	if git.checks != 4 || len(client.merges) != 1 {
		t.Fatalf("worktree evaluations=%d merges=%d, want four evaluations and one merge", git.checks, len(client.merges))
	}
	events, err := store.ListTaskEvents(ctx, initialTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	for _, event := range events {
		if event.Kind == "merge_gate_transient_retry" {
			retries++
		}
	}
	if retries != 4 {
		t.Fatalf("transient retry events = %d, want recurring release through delayed clearance", retries)
	}
}

func TestPollOnceBindsBranchlessReadyTaskToCurrentHead(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := github.PullRequest{
		Number: 1699, State: "open", HeadRef: "task-1699", BaseRef: "main",
		HeadSHA: "current-head",
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID: "implementation-task", RepoFullName: repo.FullName(),
		Branch: pull.HeadRef, State: string(workflow.TaskPullRequestOpen),
	}); err != nil {
		t.Fatal(err)
	}
	seedReadyBranchlessReviewTask(t, store, repo, "review-pr-1699-legacy", pull, "stale-head")
	seedReadyBranchlessReviewTask(t, store, repo, "review-pr-1699-current", pull, pull.HeadSHA)
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: pull.Number, HeadBranch: pull.HeadRef,
		BaseBranch: pull.BaseRef, HeadSHA: pull.HeadSHA, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	daemon, gate := blockedReviewTaskDaemon(t, store, repo, pull)

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(gate.requests) != 1 || gate.requests[0].TaskID != "review-pr-1699-current" ||
		gate.requests[0].HeadSHA != pull.HeadSHA {
		t.Fatalf("merge gate requests = %+v, want current-head task only", gate.requests)
	}
}

func TestReadyResolverRejectsBranchTaskBoundToStaleHead(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := github.PullRequest{
		Number: 1699, State: "open", HeadRef: "task-1699", BaseRef: "main",
		HeadSHA: "current-head",
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID: "implementation-task", RepoFullName: repo.FullName(),
		Branch: pull.HeadRef, State: string(workflow.TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: pull.Number, HeadBranch: pull.HeadRef,
		BaseBranch: pull.BaseRef, HeadSHA: "stale-head", State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	daemon, gate := blockedReviewTaskDaemon(t, store, repo, pull)

	ready, err := daemon.pullRequestReadyToMerge(ctx, pull)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("stale-head branch task reported ready for current PR head")
	}
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate requests = %+v, want none for stale-head task", gate.requests)
	}
}

func TestPollOnceRejectsAmbiguousBranchlessReadyTasks(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := github.PullRequest{
		Number: 1699, State: "open", HeadRef: "task-1699", BaseRef: "main",
		HeadSHA: "current-head",
	}
	seedReadyBranchlessReviewTask(t, store, repo, "review-pr-1699-first", pull, pull.HeadSHA)
	seedReadyBranchlessReviewTask(t, store, repo, "review-pr-1699-second", pull, pull.HeadSHA)
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: pull.Number, HeadBranch: pull.HeadRef,
		BaseBranch: pull.BaseRef, HeadSHA: pull.HeadSHA, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	daemon, gate := blockedReviewTaskDaemon(t, store, repo, pull)

	err := daemon.PollOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "ambiguous ready tasks") {
		t.Fatalf("PollOnce error = %v, want ambiguous ready tasks", err)
	}
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate requests = %+v, want none on ambiguous identity", gate.requests)
	}
}

func TestReadyHandlerRevalidatesResolvedTaskBeforeGate(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	pull := github.PullRequest{
		Number: 1699, State: "open", HeadRef: "task-1699", BaseRef: "main",
		HeadSHA: "current-head",
	}
	seedReadyBranchlessReviewTask(t, store, repo, "review-pr-1699-current", pull, pull.HeadSHA)
	daemon, gate := blockedReviewTaskDaemon(t, store, repo, pull)
	task, err := daemon.lookupReadyPullRequestTask(ctx, pull, nil)
	if err != nil {
		t.Fatal(err)
	}
	task.State = string(workflow.TaskBlocked)
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	if err := daemon.handleReadyToMergeWorkflow(ctx, pull, task); err != nil {
		t.Fatal(err)
	}
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate requests = %+v, want none after ready state changed", gate.requests)
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
