package daemon

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestPollOnceReconcilesClosedReviewingPullRequest covers #543: a task wedged in
// `reviewing` whose duplicate/superseded PR was closed on GitHub must be
// reconciled off `reviewing` with its stale local `open` PR row rewritten,
// without disturbing the genuinely-open or already-merged review paths.
func TestPollOnceReconcilesClosedReviewingPullRequest(t *testing.T) {
	repo := github.Repository{Owner: "jerryfane", Name: "expensy"}
	const branch = "task-7304-csv-export"
	const headSHA = "d0b6891d074f7b5af39c2555c6fe4b6fd3284003"

	cases := []struct {
		name       string
		startState workflow.TaskState
		// openPulls / closedPulls are what GitHub reports for each list state.
		openPulls   []github.PullRequest
		closedPulls []github.PullRequest
		pinnedPull  *github.PullRequest
		wantTask    workflow.TaskState
		wantPRState string
	}{
		{
			// The wedge from the bug report: PR #6 is a duplicate that a cleanup
			// job closed (unmerged) on GitHub, but locally it is still open and the
			// task is still reviewing. Reconcile to terminal blocked + closed PR.
			name:        "closed unmerged duplicate is reconciled to blocked",
			openPulls:   nil,
			closedPulls: []github.PullRequest{{Number: 6, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: headSHA}},
			wantTask:    workflow.TaskBlocked,
			wantPRState: "closed",
		},
		{
			name:        "closed unmerged pr_open is reconciled to blocked",
			startState:  workflow.TaskPullRequestOpen,
			closedPulls: []github.PullRequest{{Number: 6, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: headSHA}},
			wantTask:    workflow.TaskBlocked,
			wantPRState: "closed",
		},
		{
			name:        "closed unmerged changes_requested is reconciled to blocked",
			startState:  workflow.TaskChangesRequested,
			closedPulls: []github.PullRequest{{Number: 6, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: headSHA}},
			wantTask:    workflow.TaskBlocked,
			wantPRState: "closed",
		},
		{
			// A PR merged outside the open list (GitHub reports merged PRs in the
			// closed list with merged_at set) must resolve the task to merged.
			name:        "closed but merged is reconciled to merged",
			openPulls:   nil,
			closedPulls: []github.PullRequest{{Number: 6, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: headSHA, MergedAt: "2026-06-30T09:48:20Z"}},
			wantTask:    workflow.TaskMerged,
			wantPRState: "merged",
		},
		{
			// Healthy path: the PR is genuinely still open and under review. It is in
			// the open set, so the closed-PR reconciler must leave it untouched.
			name:        "genuinely open PR is left reviewing",
			openPulls:   []github.PullRequest{{Number: 6, State: "open", HeadRef: branch, BaseRef: "main", HeadSHA: headSHA}},
			closedPulls: nil,
			wantTask:    workflow.TaskReviewing,
			wantPRState: "open",
		},
		{
			// A closed PR for a DIFFERENT number on the same branch must not
			// reconcile the task pinned to PR #6.
			name:        "non-matching closed PR number is ignored",
			openPulls:   nil,
			closedPulls: []github.PullRequest{{Number: 9, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: "other"}},
			pinnedPull:  &github.PullRequest{Number: 6, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: headSHA},
			wantTask:    workflow.TaskReviewing,
			wantPRState: "open",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			startState := tc.startState
			if startState == "" {
				startState = workflow.TaskReviewing
			}
			if err := store.UpsertTask(ctx, db.Task{
				ID:           "task-7304",
				RepoFullName: repo.FullName(),
				GoalID:       "goal-1",
				Title:        "CSV Export",
				State:        string(startState),
				Branch:       branch,
			}); err != nil {
				t.Fatalf("UpsertTask returned error: %v", err)
			}
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: repo.FullName(),
				Number:       6,
				URL:          "https://github.com/jerryfane/expensy/pull/6",
				HeadBranch:   branch,
				BaseBranch:   "main",
				HeadSHA:      headSHA,
				State:        "open",
			}); err != nil {
				t.Fatalf("UpsertPullRequest returned error: %v", err)
			}

			comments := map[int64][]github.IssueComment{}
			for _, p := range tc.openPulls {
				comments[p.Number] = nil
			}
			client := &fakeGitHub{
				pullsByState: map[string][]github.PullRequest{
					"open":   tc.openPulls,
					"closed": tc.closedPulls,
				},
				comments: comments,
			}
			if tc.pinnedPull != nil {
				client.pullsByNumber = map[int64]github.PullRequest{tc.pinnedPull.Number: *tc.pinnedPull}
			}
			engine := workflow.Engine{Store: store}
			daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

			if err := daemon.PollOnce(ctx); err != nil {
				t.Fatalf("PollOnce returned error: %v", err)
			}

			task, err := store.GetTask(ctx, "task-7304")
			if err != nil {
				t.Fatalf("GetTask returned error: %v", err)
			}
			if task.State != string(tc.wantTask) {
				t.Fatalf("task state = %q, want %q", task.State, tc.wantTask)
			}
			pr, err := store.GetPullRequest(ctx, repo.FullName(), 6)
			if err != nil {
				t.Fatalf("GetPullRequest returned error: %v", err)
			}
			if pr.State != tc.wantPRState {
				t.Fatalf("PR #6 state = %q, want %q", pr.State, tc.wantPRState)
			}
			// The PR row identity must be preserved, never blanked, by reconciliation.
			if pr.URL != "https://github.com/jerryfane/expensy/pull/6" || pr.HeadBranch != branch {
				t.Fatalf("PR #6 row not preserved: url=%q head=%q", pr.URL, pr.HeadBranch)
			}
			if tc.wantTask == workflow.TaskBlocked {
				events, err := store.ListTaskEvents(ctx, "task-7304")
				if err != nil || len(events) != 1 || events[0].Kind != "pr_closed_unmerged" || events[0].FromState != string(startState) {
					t.Fatalf("task events = %+v, err=%v", events, err)
				}
			}
		})
	}
}

// TestPollOnceDoesNotFuzzyMergeNonReviewingTasks guards the #1054-review finding:
// pr_open and changes_requested are folded into reconcileClosedReviewingTasks
// ONLY for the closed-unmerged -> blocked arm; they must NOT take the fuzzy
// same-branch merged-sibling resolution that reviewing owns for #543. Otherwise
// an UNRELATED merged PR that merely shares a (fork) branch name — with the head-
// SHA guard skipped because the stored PR row has an empty SHA — would drive a
// still-open task to a spurious `merged` and force-remove its worktree. reviewing
// keeps the fuzzy resolution; the precise pinned-number reconcileExternallyMergedTasks
// pass still owns the real merged case for the folded-in states.
func TestPollOnceDoesNotFuzzyMergeNonReviewingTasks(t *testing.T) {
	repo := github.Repository{Owner: "acme", Name: "widgets"}
	const branch = "patch-1" // a generic fork head ref reused across unrelated PRs

	for _, startState := range []workflow.TaskState{
		workflow.TaskPullRequestOpen,
		workflow.TaskChangesRequested,
	} {
		t.Run(string(startState), func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			if err := store.UpsertTask(ctx, db.Task{
				ID:           "task-fork",
				RepoFullName: repo.FullName(),
				GoalID:       "goal-1",
				Title:        "Fork PR work",
				State:        string(startState),
				Branch:       branch,
			}); err != nil {
				t.Fatalf("UpsertTask returned error: %v", err)
			}
			// The stored PR row (our own PR #12) carries an EMPTY head SHA, so the
			// selectReconciledPull SHA guard is skipped.
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: repo.FullName(),
				Number:       12,
				URL:          "https://github.com/acme/widgets/pull/12",
				HeadBranch:   branch,
				BaseBranch:   "main",
				State:        "open",
			}); err != nil {
				t.Fatalf("UpsertPullRequest returned error: %v", err)
			}
			client := &fakeGitHub{
				pullsByState: map[string][]github.PullRequest{
					"open": nil,
					"closed": {
						// The task's OWN PR #12 is closed-unmerged...
						{Number: 12, State: "closed", HeadRef: branch, BaseRef: "main"},
						// ...alongside an UNRELATED already-merged PR #5 on the same fork head ref.
						{Number: 5, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: "unrelated-sha", MergedAt: "2026-06-30T09:48:20Z"},
					},
				},
				// reconcileExternallyMergedTasks looks up the pinned PR by number; #12
				// is closed-unmerged, so the precise pass records a breadcrumb only.
				pullsByNumber: map[int64]github.PullRequest{12: {Number: 12, State: "closed", HeadRef: branch, BaseRef: "main"}},
				comments:      map[int64][]github.IssueComment{},
			}
			engine := workflow.Engine{Store: store}
			daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}
			if err := daemon.PollOnce(ctx); err != nil {
				t.Fatalf("PollOnce returned error: %v", err)
			}
			task, err := store.GetTask(ctx, "task-fork")
			if err != nil {
				t.Fatalf("GetTask returned error: %v", err)
			}
			if task.State == string(workflow.TaskMerged) {
				t.Fatalf("task falsely resolved to merged by an unrelated same-branch sibling (state=%q)", task.State)
			}
			if task.State != string(startState) {
				t.Fatalf("task state = %q, want it left at %q (no fuzzy merge, no worktree delete)", task.State, startState)
			}
		})
	}
}

// TestPollOnceReconcilesMergedSiblingPullRequest covers the literal #543 scenario
// (finding 1): the real PR (#5) is MERGED while a duplicate (#6) on the SAME
// branch was closed unmerged. Only #6 was ever recorded locally
// (GetPullRequestByRepoBranch returns the highest number), so keying the
// reconcile on the pinned #6 alone drives the task to a spurious `blocked` and
// re-surfaces already-merged work to a human. The merge signal for #5 is already
// in the same closed list the reconciler fetches, so the task must resolve to
// `merged`.
func TestPollOnceReconcilesMergedSiblingPullRequest(t *testing.T) {
	repo := github.Repository{Owner: "jerryfane", Name: "expensy"}
	const branch = "task-7304-csv-export"
	const headSHA = "d0b6891d074f7b5af39c2555c6fe4b6fd3284003"

	cases := []struct {
		name        string
		closedPulls []github.PullRequest
	}{
		{
			// Duplicate #6 shares the head SHA with the merged real PR #5 (same
			// branch tip), which is the normal duplicate-PR shape.
			name: "merged sibling with matching head sha",
			closedPulls: []github.PullRequest{
				{Number: 5, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: headSHA, MergedAt: "2026-06-30T09:48:20Z"},
				{Number: 6, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: headSHA},
			},
		},
		{
			// The merged sibling's head SHA is unknown: a merged PR on the branch is
			// still preferred over the closed-unmerged pin.
			name: "merged sibling with unknown head sha",
			closedPulls: []github.PullRequest{
				{Number: 5, State: "closed", HeadRef: branch, BaseRef: "main", MergedAt: "2026-06-30T09:48:20Z"},
				{Number: 6, State: "closed", HeadRef: branch, BaseRef: "main", HeadSHA: headSHA},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			if err := store.UpsertTask(ctx, db.Task{
				ID:           "task-7304",
				RepoFullName: repo.FullName(),
				GoalID:       "goal-1",
				Title:        "CSV Export",
				State:        string(workflow.TaskReviewing),
				Branch:       branch,
			}); err != nil {
				t.Fatalf("UpsertTask returned error: %v", err)
			}
			// Only the duplicate #6 was ever recorded locally, as in the bug.
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: repo.FullName(),
				Number:       6,
				URL:          "https://github.com/jerryfane/expensy/pull/6",
				HeadBranch:   branch,
				BaseBranch:   "main",
				HeadSHA:      headSHA,
				State:        "open",
			}); err != nil {
				t.Fatalf("UpsertPullRequest returned error: %v", err)
			}

			client := &fakeGitHub{
				pullsByState: map[string][]github.PullRequest{
					"open":   nil,
					"closed": tc.closedPulls,
				},
				comments: map[int64][]github.IssueComment{},
			}
			engine := workflow.Engine{Store: store}
			daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

			if err := daemon.PollOnce(ctx); err != nil {
				t.Fatalf("PollOnce returned error: %v", err)
			}

			task, err := store.GetTask(ctx, "task-7304")
			if err != nil {
				t.Fatalf("GetTask returned error: %v", err)
			}
			if task.State != string(workflow.TaskMerged) {
				t.Fatalf("task state = %q, want %q (merged sibling must not surface as blocked)", task.State, workflow.TaskMerged)
			}
			// The merged sibling #5 must be recorded as merged.
			merged, err := store.GetPullRequest(ctx, repo.FullName(), 5)
			if err != nil {
				t.Fatalf("GetPullRequest(#5) returned error: %v", err)
			}
			if merged.State != "merged" {
				t.Fatalf("PR #5 state = %q, want merged", merged.State)
			}
		})
	}
}
