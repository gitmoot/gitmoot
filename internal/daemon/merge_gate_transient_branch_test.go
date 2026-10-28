package daemon

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestTransientlyBlockedBranchTaskIsReleasedNotDemoted is #2374
// (appkit#7). An approved implement task owns the PR's head branch; the merge
// gate blocked it TRANSIENTLY (dirty worktree). reconcilePROpenTasks used to
// promote every blocked task on an open same-repo PR to pr_open before the
// transient retry ran, so the retry (blocked only) and `/gitmoot merge`
// (ready_to_merge / awaiting_human_merge only) both ignored it and nothing could
// recover it. The transient block must instead be retried back to
// ready_to_merge; a QUALITY block keeps the old pr_open promotion.
func TestTransientlyBlockedBranchTaskIsReleasedNotDemoted(t *testing.T) {
	for _, tc := range []struct {
		name         string
		class        workflow.MergeBlockClass
		otherBlocker bool
		want         workflow.TaskState
	}{
		{"transient block is retried, not demoted", workflow.MergeBlockTransient, false, workflow.TaskReadyToMerge},
		{"quality block still promotes to pr_open", workflow.MergeBlockQuality, false, workflow.TaskPullRequestOpen},
		// The gate row is transient, but a LATER blocker owns the task's block; the
		// retry would never release it, so holding it would wedge it forever.
		{"task blocked by another blocker still promotes", workflow.MergeBlockTransient, true, workflow.TaskPullRequestOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
			pull := seedBlockedReviewTask(t, store, repo, tc.class, "local worktree is not clean")
			// Give the task the PR's head branch, the implement-task shape #2374 hit,
			// and record the PR so the transient retry can resolve it by branch.
			task, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
			if err != nil {
				t.Fatal(err)
			}
			task.Branch = pull.HeadRef
			if err := store.UpsertTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: repo.FullName(), Number: pull.Number, URL: pull.URL,
				HeadBranch: pull.HeadRef, BaseBranch: pull.BaseRef, HeadSHA: pull.HeadSHA, State: "open",
			}); err != nil {
				t.Fatal(err)
			}
			if tc.otherBlocker {
				if err := store.AddTaskEvent(ctx, db.TaskEvent{TaskID: task.ID, Kind: "quorum_unmet", ToState: string(workflow.TaskBlocked), Reason: "2 of 3 delegation children failed"}); err != nil {
					t.Fatal(err)
				}
			}
			daemon, _ := blockedReviewTaskDaemon(t, store, repo, pull)
			if err := daemon.reconcilePROpenTasks(ctx, []github.PullRequest{pull}); err != nil {
				t.Fatalf("reconcilePROpenTasks: %v", err)
			}
			if err := daemon.reconcileTransientlyBlockedMergeGates(ctx, map[int64]struct{}{pull.Number: {}}); err != nil {
				t.Fatalf("reconcileTransientlyBlockedMergeGates: %v", err)
			}
			got, err := store.GetTask(ctx, "review-pr-1699-3f3a1026")
			if err != nil {
				t.Fatal(err)
			}
			if got.State != string(tc.want) {
				t.Fatalf("task state = %q, want %q", got.State, tc.want)
			}
		})
	}
}
