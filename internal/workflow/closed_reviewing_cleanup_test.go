package workflow

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestHandleReviewPullRequestClosedMergedReleasesLockAndWorktree covers #543
// finding 2: when the reconciler resolves an externally-merged reviewing task to
// `merged`, it must release the branch lock and remove the task worktree exactly
// as the canonical merge path (PolicyMergeGate.finishMerged) does — otherwise the
// lock is stranded (held forever) and the worktree leaks on disk. The
// closed-unmerged (`blocked`) branch must NOT clean up, since a blocked task
// intentionally keeps its worktree/lock for human resumption. A task parked for
// a human merge is the exception: closing that PR unmerged preserves its
// worktree, but releases the lock so the abandoned PR cannot strand it.
//
// It runs from both `reviewing` and `blocked` start states (#953): a blocked
// task whose branch merged externally is the exact live incident that motivated
// #953 (a 21h stranded branch lock), so the merged case must release the lock +
// worktree from `blocked` too, while blocked+closed-unmerged keeps them.
func TestHandleReviewPullRequestClosedMergedReleasesLockAndWorktree(t *testing.T) {
	const (
		repo    = "gitmoot/gitmoot"
		branch  = "task-7304-csv-export"
		path    = "/tmp/gitmoot/worktrees/gitmoot--gitmoot/task-7304"
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
			name:          "closed unmerged preserves worktree for human",
			merged:        false,
			wantTask:      TaskBlocked,
			wantPRState:   "closed",
			wantRemoved:   false,
			wantLockGone:  false,
			wantPathEmpty: false,
		},
	}

	for _, startState := range []TaskState{TaskReviewing, TaskAwaitingHumanMerge, TaskBlocked} {
		for _, tc := range cases {
			t.Run(string(startState)+"/"+tc.name, func(t *testing.T) {
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
					State:        string(startState),
					Branch:       branch,
					WorktreePath: path,
				}); err != nil {
					t.Fatalf("UpsertTask returned error: %v", err)
				}
				if err := store.UpsertPullRequest(ctx, db.PullRequest{
					RepoFullName: repo,
					Number:       6,
					URL:          "https://github.com/gitmoot/gitmoot/pull/6",
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
				// blocked + closed-unmerged is a complete no-op: the handler returns
				// early before touching the PR mirror, so it stays "open". A parked
				// awaiting_human_merge task instead resolves to blocked and marks the
				// PR closed, so a human-abandoned merge cannot stay parked forever.
				wantPRState := tc.wantPRState
				if !tc.merged && startState == TaskBlocked {
					wantPRState = "open"
				}
				if pr.State != wantPRState {
					t.Fatalf("PR #6 state = %q, want %q", pr.State, wantPRState)
				}
				gotRemoved := len(manager.removedForce) == 1 && manager.removedForce[0] == path
				if gotRemoved != tc.wantRemoved {
					t.Fatalf("worktree removed = %v (%v), want %v", gotRemoved, manager.removedForce, tc.wantRemoved)
				}
				_, lockErr := store.GetBranchLock(ctx, repo, branch)
				lockGone := errors.Is(lockErr, sql.ErrNoRows)
				if !tc.merged && startState == TaskAwaitingHumanMerge {
					if !lockGone {
						t.Fatalf("parked closed-unmerged task kept branch lock: %v", lockErr)
					}
				} else if lockGone != tc.wantLockGone {
					t.Fatalf("branch lock gone = %v (err=%v), want %v", lockGone, lockErr, tc.wantLockGone)
				}
			})
		}
	}
}

func TestHandleReviewPullRequestClosedMergedLifecycleStates(t *testing.T) {
	states := []TaskState{
		TaskPullRequestOpen,
		TaskReviewing,
		TaskChangesRequested,
		TaskReadyToMerge,
		TaskAwaitingHumanMerge,
		TaskBlocked,
	}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			if err := store.UpsertTask(ctx, db.Task{
				ID:           "task-893",
				RepoFullName: "owner/repo",
				Title:        "Reconcile external merge",
				State:        string(state),
				Branch:       "feature/893",
			}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: "owner/repo",
				Number:       893,
				HeadBranch:   "feature/893",
				State:        "open",
			}); err != nil {
				t.Fatalf("UpsertPullRequest: %v", err)
			}

			engine := Engine{Store: store}
			if err := engine.HandleReviewPullRequestClosed(ctx, PullRequestEvent{
				Repo:        "owner/repo",
				Branch:      "feature/893",
				PullRequest: 893,
				TaskID:      "task-893",
				TaskTitle:   "Reconcile external merge",
				LeadAgent:   "github",
				Sender:      "github",
			}, true); err != nil {
				t.Fatalf("HandleReviewPullRequestClosed: %v", err)
			}

			task, err := store.GetTask(ctx, "task-893")
			if err != nil || task.State != string(TaskMerged) {
				t.Fatalf("task = %+v, err=%v; want merged", task, err)
			}
			pr, err := store.GetPullRequest(ctx, "owner/repo", 893)
			if err != nil || pr.State != "merged" {
				t.Fatalf("pull request = %+v, err=%v; want merged", pr, err)
			}
		})
	}
}

func TestHandleReviewPullRequestClosedUnmergedLifecycleStates(t *testing.T) {
	states := []struct {
		state       TaskState
		want        TaskState
		wantPRState string
		wantEvent   bool
	}{
		{state: TaskPullRequestOpen, want: TaskBlocked, wantPRState: "closed", wantEvent: true},
		{state: TaskReviewing, want: TaskBlocked, wantPRState: "closed", wantEvent: true},
		{state: TaskChangesRequested, want: TaskBlocked, wantPRState: "closed", wantEvent: true},
		{state: TaskAwaitingHumanMerge, want: TaskBlocked, wantPRState: "closed", wantEvent: true},
		{state: TaskReadyToMerge, want: TaskReadyToMerge, wantPRState: "open"},
		{state: TaskBlocked, want: TaskBlocked, wantPRState: "open"},
	}
	for _, state := range states {
		t.Run(string(state.state), func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			if err := store.UpsertTask(ctx, db.Task{
				ID:           "task-893",
				RepoFullName: "owner/repo",
				Title:        "Conservative close handling",
				State:        string(state.state),
				Branch:       "feature/893",
			}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: "owner/repo",
				Number:       893,
				HeadBranch:   "feature/893",
				State:        "open",
			}); err != nil {
				t.Fatalf("UpsertPullRequest: %v", err)
			}

			engine := Engine{Store: store}
			for i := 0; i < 2; i++ {
				if err := engine.HandleReviewPullRequestClosed(ctx, PullRequestEvent{
					Repo:        "owner/repo",
					Branch:      "feature/893",
					PullRequest: 893,
					TaskID:      "task-893",
					TaskTitle:   "Conservative close handling",
					LeadAgent:   "github",
					Sender:      "github",
				}, false); err != nil {
					t.Fatalf("HandleReviewPullRequestClosed pass %d: %v", i+1, err)
				}
			}

			task, err := store.GetTask(ctx, "task-893")
			if err != nil || task.State != string(state.want) {
				t.Fatalf("task = %+v, err=%v; want %s", task, err, state.want)
			}
			pr, err := store.GetPullRequest(ctx, "owner/repo", 893)
			if err != nil || pr.State != state.wantPRState {
				t.Fatalf("pull request = %+v, err=%v; want %s", pr, err, state.wantPRState)
			}
			events, err := store.ListTaskEvents(ctx, "task-893")
			if err != nil {
				t.Fatalf("ListTaskEvents: %v", err)
			}
			if state.wantEvent {
				if len(events) != 1 || events[0].Kind != "pr_closed_unmerged" || events[0].FromState != string(state.state) || events[0].ToState != string(TaskBlocked) {
					t.Fatalf("task events = %+v", events)
				}
			} else if len(events) != 0 {
				t.Fatalf("unexpected task events = %+v", events)
			}
		})
	}
}

func TestHandleReviewPullRequestClosedEmptyBranchDoesNotAdvanceCanonicalTask(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "implement-11",
		RepoFullName: "owner/repo",
		Title:        "Canonical implementation",
		State:        string(TaskImplementing),
		Branch:       "feature/eleven",
	}); err != nil {
		t.Fatalf("Upsert implement task: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "review-pr-11-44cd8322",
		RepoFullName: "owner/repo",
		Title:        "Review PR #11",
		State:        string(TaskReviewing),
	}); err != nil {
		t.Fatalf("Upsert review task: %v", err)
	}

	engine := Engine{Store: store}
	if err := engine.HandleReviewPullRequestClosed(ctx, PullRequestEvent{
		Repo:        "owner/repo",
		Branch:      "feature/eleven",
		PullRequest: 11,
		TaskID:      "review-pr-11-44cd8322",
		TaskTitle:   "Review PR #11",
		LeadAgent:   "github",
		Sender:      "github",
	}, true); err != nil {
		t.Fatalf("HandleReviewPullRequestClosed: %v", err)
	}

	reviewTask, err := store.GetTask(ctx, "review-pr-11-44cd8322")
	if err != nil || reviewTask.State != string(TaskMerged) || reviewTask.Branch != "" {
		t.Fatalf("review task = %+v, err=%v; want merged with empty branch", reviewTask, err)
	}
	implementTask, err := store.GetTask(ctx, "implement-11")
	if err != nil || implementTask.State != string(TaskImplementing) || implementTask.Branch != "feature/eleven" {
		t.Fatalf("implement task changed = %+v, err=%v", implementTask, err)
	}
}
