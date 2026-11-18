package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2687: a ready_to_merge task whose PR closed UNMERGED re-ran the whole
// ready-to-merge workflow (merge gate included) on every poll, forever, because
// nothing changed. It is now settled on the first poll: a review record with no
// live job is retired to dismissed (#2668), anything else is parked blocked, and
// the second poll never reaches the merge gate.
func TestPollOnceSettlesClosedUnmergedReadyToMergeOnce(t *testing.T) {
	tests := []struct {
		name      string
		taskID    string
		goalID    string
		liveJob   bool
		wantState workflow.TaskState
		wantKind  string
	}{
		{name: "implementation task is blocked", taskID: "task-7", goalID: "goal-1",
			wantState: workflow.TaskBlocked, wantKind: workflow.TaskEventPullRequestClosedUnmerged},
		{name: "review-pr record is dismissed", taskID: "review-pr-7-0a1b2c3d", goalID: "goal-1",
			wantState: workflow.TaskDismissed, wantKind: workflow.TaskEventReviewRetiredPullRequestClosed},
		{name: "local-review record is dismissed", taskID: "local-review-task-7", goalID: workflow.LocalReviewGoalID,
			wantState: workflow.TaskDismissed, wantKind: workflow.TaskEventReviewRetiredPullRequestClosed},
		{name: "review record with a live job is blocked", taskID: "review-pr-7-0a1b2c3d", goalID: "goal-1", liveJob: true,
			wantState: workflow.TaskBlocked, wantKind: workflow.TaskEventPullRequestClosedUnmerged},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
			const branch = "feature/seven"
			if err := store.UpsertTask(ctx, db.Task{ID: test.taskID, RepoFullName: repo.FullName(), GoalID: test.goalID,
				Title: "Task 7", State: string(workflow.TaskReadyToMerge), Branch: branch}); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: repo.FullName(), Number: 7,
				HeadBranch: branch, BaseBranch: "main", HeadSHA: "abc123", State: "open"}); err != nil {
				t.Fatal(err)
			}
			if test.liveJob {
				if err := store.CreateJob(ctx, db.Job{ID: "review-job-7", Agent: "reviewer", Type: "review", State: string(workflow.JobRunning),
					Payload: `{"repo":"gitmoot/gitmoot","pull_request":7,"task_id":"` + test.taskID + `"}`}); err != nil {
					t.Fatal(err)
				}
			}
			closed := github.PullRequest{Number: 7, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: "abc123"}
			client := &fakeGitHub{
				pullsByState:  map[string][]github.PullRequest{"open": {}, "closed": {closed}},
				pullsByNumber: map[int64]github.PullRequest{7: closed},
				comments:      map[int64][]github.IssueComment{},
			}
			// A transient deferral leaves the task ready_to_merge, which is what
			// kept the old path re-running the gate every poll.
			gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Deferred: true, BlockClass: workflow.MergeBlockTransient,
				Reason: workflow.PlainReason("pull request is not open")}}
			engine := workflow.Engine{Store: store, MergeGate: gate}
			daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

			if err := daemon.PollOnce(ctx); err != nil {
				t.Fatalf("first PollOnce: %v", err)
			}
			assertExternalMergeState(t, store, repo.FullName(), test.taskID, 7, test.wantState, "closed")
			assertLatestTaskEventKind(t, store, test.taskID, test.wantKind)
			if err := daemon.PollOnce(ctx); err != nil {
				t.Fatalf("second PollOnce: %v", err)
			}
			if len(gate.requests) != 0 {
				t.Fatalf("merge gate ran %d times for a PR closed unmerged; want 0", len(gate.requests))
			}
			assertExternalMergeState(t, store, repo.FullName(), test.taskID, 7, test.wantState, "closed")
		})
	}
}

// A ready_to_merge task whose closed PR WAS merged keeps today's path: the
// ready-to-merge workflow runs and its gate reports the merge.
func TestPollOnceClosedMergedReadyToMergeStillRunsGate(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{ID: "review-pr-7-0a1b2c3d", RepoFullName: repo.FullName(), GoalID: "goal-1",
		Title: "Task 7", State: string(workflow.TaskReadyToMerge), Branch: "feature/seven"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: repo.FullName(), Number: 7,
		HeadBranch: "feature/seven", BaseBranch: "main", HeadSHA: "abc123", State: "open"}); err != nil {
		t.Fatal(err)
	}
	client := &fakeGitHub{
		pullsByState: map[string][]github.PullRequest{"open": {}, "closed": {{Number: 7, State: "closed", HeadRef: "feature/seven",
			BaseRef: "main", HeadSHA: "abc123", MergedAt: "2026-11-18T20:08:00Z"}}},
		comments: map[int64][]github.IssueComment{},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(gate.requests) != 1 || !gate.requests[0].PullRequestMerged {
		t.Fatalf("merge gate requests = %+v; want one merged-PR evaluation", gate.requests)
	}
	assertExternalMergeState(t, store, repo.FullName(), "review-pr-7-0a1b2c3d", 7, workflow.TaskMerged, "merged")
}

// #2690 review: a ready_to_merge task holding a retained merge claim is moved
// to blocked by the claim recovery before the close handler runs. The PR mirror
// must still record the close, for a branch-bearing implementation task (the
// closed ready-to-merge retry) and a branchless review record (the external
// merge reconcile) alike, and the second poll must change nothing.
func TestPollOnceRecordsClosedPRForRetainedClaimReadyToMerge(t *testing.T) {
	tests := []struct {
		name      string
		taskID    string
		branch    string
		wantState workflow.TaskState
	}{
		{name: "branch-bearing implementation task", taskID: "task-7", branch: "feature/seven", wantState: workflow.TaskBlocked},
		{name: "branchless review record", taskID: "review-pr-7-0a1b2c3d", wantState: workflow.TaskDismissed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
			if err := store.UpsertTask(ctx, db.Task{ID: test.taskID, RepoFullName: repo.FullName(), GoalID: "goal-1",
				Title: "Task 7", State: string(workflow.TaskReadyToMerge), Branch: test.branch}); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: repo.FullName(), Number: 7,
				HeadBranch: "feature/seven", BaseBranch: "main", HeadSHA: "abc123", State: "open"}); err != nil {
				t.Fatal(err)
			}
			token, claimed, _, err := store.ClaimTaskState(ctx, test.taskID, string(workflow.TaskReadyToMerge),
				db.TaskStateClaimKindExternalMerge, time.Minute)
			if err != nil || !claimed {
				t.Fatalf("ClaimTaskState = claimed %v err %v", claimed, err)
			}
			if retained, err := store.RetainTaskStateClaim(ctx, test.taskID, token, string(workflow.TaskReadyToMerge),
				db.TaskStateClaimKindExternalMergeUncertain); err != nil || !retained {
				t.Fatalf("RetainTaskStateClaim = retained %v err %v", retained, err)
			}
			closed := github.PullRequest{Number: 7, State: "closed", HeadRef: "feature/seven", BaseRef: "main", HeadSHA: "abc123"}
			client := &fakeGitHub{
				pullsByState:  map[string][]github.PullRequest{"open": {}, "closed": {closed}},
				pullsByNumber: map[int64]github.PullRequest{7: closed},
				comments:      map[int64][]github.IssueComment{},
			}
			gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Deferred: true, BlockClass: workflow.MergeBlockTransient,
				Reason: workflow.PlainReason("pull request is not open")}}
			engine := workflow.Engine{Store: store, MergeGate: gate}
			daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

			if err := daemon.PollOnce(ctx); err != nil {
				t.Fatalf("first PollOnce: %v", err)
			}
			assertExternalMergeState(t, store, repo.FullName(), test.taskID, 7, test.wantState, "closed")
			if claimed, err := store.HasTaskStateClaim(ctx, test.taskID); err != nil || claimed {
				t.Fatalf("retained claim after the close = %v, %v; want released", claimed, err)
			}
			events, err := store.ListTaskEvents(ctx, test.taskID)
			if err != nil {
				t.Fatal(err)
			}

			if err := daemon.PollOnce(ctx); err != nil {
				t.Fatalf("second PollOnce: %v", err)
			}
			assertExternalMergeState(t, store, repo.FullName(), test.taskID, 7, test.wantState, "closed")
			after, err := store.ListTaskEvents(ctx, test.taskID)
			if err != nil || len(after) != len(events) {
				t.Fatalf("task events after the second poll = %+v (err %v); want the %d from the first", after, err, len(events))
			}
			if len(gate.requests) != 0 {
				t.Fatalf("merge gate ran %d times for a PR closed unmerged; want 0", len(gate.requests))
			}
		})
	}
}

func assertLatestTaskEventKind(t *testing.T, store *db.Store, taskID, want string) {
	t.Helper()
	events, err := store.ListTaskEvents(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].ToState != "" {
			if events[i].Kind != want {
				t.Fatalf("latest state event kind = %q (%+v); want %q", events[i].Kind, events[i], want)
			}
			return
		}
	}
	t.Fatalf("no state event for %s; want %q", taskID, want)
}
