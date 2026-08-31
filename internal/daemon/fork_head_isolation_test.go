package daemon

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// A fork pull request shares nothing with a local branch but its NAME (#1714
// round-6 review). These three cases pin the two directions that matter: a fork
// head must never resolve, store or advance the local task that happens to own
// that branch name, and a LEGITIMATE same-repo head must keep routing normally
// even when GitHub reports its repository with different letter case.

// TestPollOnceForkHeadWithChangedStateLeavesLocalTaskAlone covers the changed-PR
// routing path. The fork PR is deliberately NOT mirrored locally, so
// pullRequestChanged reports "changed" and routing would run: that is the exact
// gap which let the earlier fork regression miss this path.
func TestPollOnceForkHeadWithChangedStateLeavesLocalTaskAlone(t *testing.T) {
	ctx := context.Background()
	store, client, daemon, _ := newSkippedFanoutPendingGateDaemon(t, workflow.TaskReadyToMerge)
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true}}
	daemon.Workflow = &workflow.Engine{Store: store, MergeGate: gate}

	// The local PR #7 stays open and mirrored (fixture default); the fork PR is
	// the only unmirrored one, so only it can drive changed-PR routing.
	fork := client.pulls[0]
	fork.Number = 99
	fork.HeadRepoFullName = "fork/gitmoot"
	fork.HeadSHA = "fork123"
	client.pulls = append(client.pulls, fork)
	client.comments[99] = []github.IssueComment{}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	// The local ready task is reconciled through its OWN PR #7, never through #99.
	for _, request := range gate.requests {
		if request.PullRequest != 7 {
			t.Fatalf("merge gate request = %+v, want none for the fork pull request", request)
		}
	}
	// recordPullRequest must not mint a mirror row for the fork PR: a row keyed on
	// branch text alone later becomes that branch's authoritative pull request.
	if _, err := store.GetPullRequest(ctx, "gitmoot/gitmoot", 99); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetPullRequest(#99) error = %v, want no stored mirror row for a fork head", err)
	}
	for _, status := range client.statuses {
		if status.SHA != "abc123" {
			t.Fatalf("status = %+v, want no commit status written against the fork head", status)
		}
	}
}

// TestPollOnceMergedForkHeadLeavesLocalReviewingTaskAlone covers closed-PR
// reconciliation: a MERGED fork PR sharing a reviewing task's branch name must
// not drive that task to merged and delete its worktree.
func TestPollOnceMergedForkHeadLeavesLocalReviewingTaskAlone(t *testing.T) {
	ctx := context.Background()
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	const branch = "task-shared"
	store := testStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-shared",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Shared branch name",
		State:        string(workflow.TaskReviewing),
		Branch:       branch,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       6,
		URL:          "https://github.com/gitmoot/gitmoot/pull/6",
		HeadBranch:   branch,
		BaseBranch:   "main",
		// An EMPTY stored head SHA is the documented hole (daemon.go:1530): the
		// head-SHA guard is skipped when either side is unknown, so without the
		// fork check the merged fork PR wins the branch-keyed resolution.
		HeadSHA: "",
		State:   "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	client := &fakeGitHub{
		pullsByState: map[string][]github.PullRequest{
			// The branch must be ABSENT from the open listing, or closed-reviewing
			// reconciliation skips it before the fork guard is ever consulted. That
			// skip is what made the first version of this test vacuous.
			"open": nil,
			"closed": {{
				Number:           99,
				State:            "closed",
				HeadRef:          branch,
				HeadRepoFullName: "fork/gitmoot",
				HeadSHA:          "fork123",
				MergedAt:         "2026-08-31T12:00:00Z",
			}},
		},
		// The task's own PR is still open on GitHub, just not on the listing page,
		// so pinned-number reconciliation finds nothing to advance and the merged
		// fork PR is the only signal that can reach the branch-keyed resolution.
		pullsByNumber: map[int64]github.PullRequest{6: {
			Number:  6,
			State:   "open",
			HeadRef: branch,
			BaseRef: "main",
			HeadSHA: "local123",
		}},
		comments: map[int64][]github.IssueComment{},
	}
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	task, err := store.GetTask(ctx, "task-shared")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReviewing) {
		t.Fatalf("task state = %q, want %q: a merged fork PR must not advance a local task", task.State, workflow.TaskReviewing)
	}
}

// TestPollOnceMixedCaseSameRepoHeadStillRoutes is the failure direction the fix
// itself could introduce: rejecting a legitimate same-repo pull request over
// letter case would silently disable routing for ordinary work.
func TestPollOnceMixedCaseSameRepoHeadStillRoutes(t *testing.T) {
	ctx := context.Background()
	store, client, daemon, _ := newSkippedFanoutPendingGateDaemon(t, workflow.TaskChangesRequested)
	client.pulls[0].HeadRepoFullName = "GitMoot/GitMoot"

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.statuses) != 1 || client.statuses[0].SHA != "abc123" {
		t.Fatalf("statuses = %+v, want the mixed-case same-repo head treated as local", client.statuses)
	}
	observation, err := store.GetMergeGateStatusObservation(ctx, "gitmoot/gitmoot", 7)
	if err != nil {
		t.Fatalf("GetMergeGateStatusObservation returned error: %v", err)
	}
	if observation.HeadSHA != "abc123" || observation.Kind != mergeGateStatusMarker {
		t.Fatalf("status observation = %+v, want a marker for a legitimate same-repo head", observation)
	}
}

// TestPollOnceMarkerReappearsAfterHeadChange pins the exact-head term of the
// dedup at daemon.go:986, which is the invariant this whole change exists to
// establish: a stale observation at an OLD head must not suppress the marker at
// a NEW one, or a force-pushed PR silently goes back to reading CLEAN while
// unjudged. Round-8 review found this term mutation-uncovered.
func TestPollOnceMarkerReappearsAfterHeadChange(t *testing.T) {
	ctx := context.Background()
	store, client, daemon, _ := newSkippedFanoutPendingGateDaemon(t, workflow.TaskChangesRequested)
	if err := store.UpsertMergeGateStatusObservation(ctx, db.MergeGateStatusObservation{
		RepoFullName: "gitmoot/gitmoot",
		PullRequest:  7,
		HeadSHA:      "old123",
		Kind:         mergeGateStatusMarker,
	}); err != nil {
		t.Fatalf("UpsertMergeGateStatusObservation returned error: %v", err)
	}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.statuses) != 1 || client.statuses[0].SHA != "abc123" {
		t.Fatalf("statuses = %+v, want a marker at the NEW head abc123", client.statuses)
	}
	observation, err := store.GetMergeGateStatusObservation(ctx, "gitmoot/gitmoot", 7)
	if err != nil {
		t.Fatalf("GetMergeGateStatusObservation returned error: %v", err)
	}
	if observation.HeadSHA != "abc123" || observation.Kind != mergeGateStatusMarker {
		t.Fatalf("status observation = %+v, want the observation advanced to abc123", observation)
	}
}

// TestMergeCommandOnForkHeadRequestsNoMerge pins the fork guard inside
// lookupPolledPullRequestTask. handleComment runs OUTSIDE the PollOnce identity
// fence by design, and daemon.go:176-178 cites this resolver as the reason that
// is safe, so the resolver's own guard is the only thing standing between a fork
// PR comment and a local task's merge.
func TestMergeCommandOnForkHeadRequestsNoMerge(t *testing.T) {
	ctx := context.Background()
	store, _, daemon, _ := newSkippedFanoutPendingGateDaemon(t, workflow.TaskReadyToMerge)
	if err := store.SetBranchLockReviewFanout(ctx, "gitmoot/gitmoot", "task-7", false); err != nil {
		t.Fatalf("SetBranchLockReviewFanout returned error: %v", err)
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true}}
	client := &fakeGitHub{comments: map[int64][]github.IssueComment{}}
	daemon.GitHub = client
	daemon.Workflow = &workflow.Engine{Store: store, MergeGate: gate}

	if err := daemon.handleMergeCommand(ctx,
		github.PullRequest{
			Number:           99,
			Title:            "Drive-by",
			State:            "open",
			HeadRef:          "task-7",
			HeadRepoFullName: "fork/gitmoot",
			BaseRef:          "main",
			HeadSHA:          "fork123",
		},
		github.IssueComment{ID: 911, Body: "/gitmoot merge", Author: "outsider"},
	); err != nil {
		t.Fatalf("handleMergeCommand returned error: %v", err)
	}
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate requests = %+v, want none for a fork pull request comment", gate.requests)
	}
	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("task state = %q, want %q untouched by a fork pull request comment", task.State, workflow.TaskReadyToMerge)
	}
}

// TestPollOnceExternallyMergedForkMirrorLeavesLocalTaskAlone pins the identity
// re-check in reconcileExternallyMergedTasks (daemon.go:587-593), a DIFFERENT
// guard from the closed-reviewing one, which is why the earlier merged-fork test
// did not cover it. The branch-keyed mirror row is the pre-upgrade shape.
func TestPollOnceExternallyMergedForkMirrorLeavesLocalTaskAlone(t *testing.T) {
	ctx := context.Background()
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	const branch = "task-shared"
	store := testStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-shared",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Shared branch name",
		State:        string(workflow.TaskPullRequestOpen),
		Branch:       branch,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       99,
		HeadBranch:   branch,
		BaseBranch:   "main",
		HeadSHA:      "fork123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	client := &fakeGitHub{
		pullsByState: map[string][]github.PullRequest{"open": nil, "closed": nil},
		pullsByNumber: map[int64]github.PullRequest{99: {
			Number:           99,
			State:            "closed",
			HeadRef:          branch,
			HeadRepoFullName: "fork/gitmoot",
			HeadSHA:          "fork123",
			MergedAt:         "2026-08-31T12:00:00Z",
		}},
		comments: map[int64][]github.IssueComment{},
	}
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	task, err := store.GetTask(ctx, "task-shared")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskPullRequestOpen) {
		t.Fatalf("task state = %q, want %q: a merged fork mirror must not advance a local task", task.State, workflow.TaskPullRequestOpen)
	}
}

// TestMergeGateMarkerAppliesMatchesDocumentedStates pins the state list the docs
// promise. Round-8 review found docs/troubleshooting.md and the website page
// claiming that any task "parked for a human or terminal" gets the not-applied
// clearance, while blocked and awaiting_human in fact KEEP the marker. A prose
// sentence cannot fail; this table can.
func TestMergeGateMarkerAppliesMatchesDocumentedStates(t *testing.T) {
	marked := map[workflow.TaskState]bool{
		workflow.TaskPullRequestOpen:  true,
		workflow.TaskReviewing:        true,
		workflow.TaskChangesRequested: true,
		workflow.TaskReadyToMerge:     true,
		workflow.TaskBlocked:          true,
		workflow.TaskAwaitingHuman:    true,

		workflow.TaskPlanned:            false,
		workflow.TaskImplementing:       false,
		workflow.TaskMerged:             false,
		workflow.TaskSuperseded:         false,
		workflow.TaskStranded:           false,
		workflow.TaskAwaitingHumanMerge: false,
		workflow.TaskDismissed:          false,
	}
	if len(marked) != 13 {
		t.Fatalf("table covers %d states, want all 13 workflow.TaskState values", len(marked))
	}
	for state, want := range marked {
		if got := mergeGateMarkerApplies(string(state)); got != want {
			t.Fatalf("mergeGateMarkerApplies(%q) = %v, want %v; docs name the clearing states explicitly, so update both together", state, got, want)
		}
	}
}
