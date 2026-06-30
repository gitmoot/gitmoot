package workflow

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// TestHandleReviewPullRequestClosedMergedReleasesLockAndWorktree covers #543
// finding 2: when the reconciler resolves an externally-merged reviewing task to
// `merged`, it must release the branch lock and remove the task worktree exactly
// as the canonical merge path (PolicyMergeGate.finishMerged) does — otherwise the
// lock is stranded (held forever) and the worktree leaks on disk. The
// closed-unmerged (`blocked`) branch must NOT clean up, since a blocked task
// intentionally keeps its worktree/lock for human resumption.
func TestHandleReviewPullRequestClosedMergedReleasesLockAndWorktree(t *testing.T) {
	const (
		repo    = "jerryfane/gitmoot"
		branch  = "task-7304-csv-export"
		path    = "/tmp/gitmoot/worktrees/jerryfane--gitmoot/task-7304"
		headSHA = "d0b6891d074f7b5af39c2555c6fe4b6fd3284003"
	)

	cases := []struct {
		name          string
		merged        bool
		wantTask      TaskState
		wantPRState   string
		wantRemoved   bool
		wantLockGone  bool
		wantPathEmpty bool
	}{
		{
			name:          "merged releases lock and removes worktree",
			merged:        true,
			wantTask:      TaskMerged,
			wantPRState:   "merged",
			wantRemoved:   true,
			wantLockGone:  true,
			wantPathEmpty: true,
		},
		{
			name:          "closed unmerged keeps lock and worktree for human",
			merged:        false,
			wantTask:      TaskBlocked,
			wantPRState:   "closed",
			wantRemoved:   false,
			wantLockGone:  false,
			wantPathEmpty: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo, Branch: branch, Owner: "lead"}); err != nil || !acquired {
				t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
			}
			if err := store.UpsertTask(ctx, db.Task{
				ID:           "task-7304",
				RepoFullName: repo,
				GoalID:       "goal-1",
				Title:        "CSV Export",
				State:        string(TaskReviewing),
				Branch:       branch,
				WorktreePath: path,
			}); err != nil {
				t.Fatalf("UpsertTask returned error: %v", err)
			}
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: repo,
				Number:       6,
				URL:          "https://github.com/jerryfane/gitmoot/pull/6",
				HeadBranch:   branch,
				BaseBranch:   "main",
				HeadSHA:      headSHA,
				State:        "open",
			}); err != nil {
				t.Fatalf("UpsertPullRequest returned error: %v", err)
			}

			manager := &fakeWorktreeManager{}
			engine := Engine{Store: store, DelegationWorktrees: manager}

			if err := engine.HandleReviewPullRequestClosed(ctx, PullRequestEvent{
				Repo:        repo,
				Branch:      branch,
				PullRequest: 6,
				HeadSHA:     headSHA,
				GoalID:      "goal-1",
				TaskID:      "task-7304",
				TaskTitle:   "CSV Export",
				LeadAgent:   "lead",
				Sender:      "github",
			}, tc.merged); err != nil {
				t.Fatalf("HandleReviewPullRequestClosed returned error: %v", err)
			}

			task, err := store.GetTask(ctx, "task-7304")
			if err != nil {
				t.Fatalf("GetTask returned error: %v", err)
			}
			if task.State != string(tc.wantTask) {
				t.Fatalf("task state = %q, want %q", task.State, tc.wantTask)
			}
			if (task.WorktreePath == "") != tc.wantPathEmpty {
				t.Fatalf("task worktree path = %q, wantEmpty=%v", task.WorktreePath, tc.wantPathEmpty)
			}
			pr, err := store.GetPullRequest(ctx, repo, 6)
			if err != nil {
				t.Fatalf("GetPullRequest returned error: %v", err)
			}
			if pr.State != tc.wantPRState {
				t.Fatalf("PR #6 state = %q, want %q", pr.State, tc.wantPRState)
			}
			gotRemoved := len(manager.removedForce) == 1 && manager.removedForce[0] == path
			if gotRemoved != tc.wantRemoved {
				t.Fatalf("worktree removed = %v (%v), want %v", gotRemoved, manager.removedForce, tc.wantRemoved)
			}
			_, lockErr := store.GetBranchLock(ctx, repo, branch)
			lockGone := errors.Is(lockErr, sql.ErrNoRows)
			if lockGone != tc.wantLockGone {
				t.Fatalf("branch lock gone = %v (err=%v), want %v", lockGone, lockErr, tc.wantLockGone)
			}
		})
	}
}
