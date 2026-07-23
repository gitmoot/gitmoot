package daemon

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestPollOnceReconcilesExternallyMergedLifecycleTasks(t *testing.T) {
	states := []workflow.TaskState{
		workflow.TaskPullRequestOpen,
		workflow.TaskReviewing,
		workflow.TaskChangesRequested,
		workflow.TaskReadyToMerge,
		workflow.TaskBlocked,
	}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			repo := github.Repository{Owner: "owner", Name: "repo"}
			store := testStore(t)
			seedExternalMergeTask(t, store, repo, "task-7", "feature/seven", state, 7)
			client := &fakeGitHub{
				pullsByState:  map[string][]github.PullRequest{"open": nil, "closed": nil},
				pullsByNumber: map[int64]github.PullRequest{7: mergedPull(7, "feature/seven")},
				comments:      map[int64][]github.IssueComment{},
			}
			engine := workflow.Engine{Store: store}
			daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

			if err := daemon.PollOnce(ctx); err != nil {
				t.Fatalf("PollOnce: %v", err)
			}
			assertExternalMergeState(t, store, repo.FullName(), "task-7", 7, workflow.TaskMerged, "merged")
			if !reflect.DeepEqual(client.getPullRequestCalls, []int64{7}) {
				t.Fatalf("GetPullRequest calls = %v, want [7]", client.getPullRequestCalls)
			}
		})
	}
}

func TestBlockedTaskExternalMergeReconcileE2E(t *testing.T) {
	// PollOnce's reconcile path performs no runtime delivery; the fake GitHub
	// client and t.TempDir-backed store make this a deterministic no-LLM E2E.
	// Keep the process environment isolated if that path ever grows orchestration.
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/throwaway")
	t.Setenv("HERDR_ENV", "")

	ctx := context.Background()
	repo := github.Repository{Owner: "owner", Name: "repo"}
	store := testStore(t)
	seedExternalMergeTask(t, store, repo, "blocked-task", "feature/blocked", workflow.TaskBlocked, 953)
	if err := store.CreateJob(ctx, db.Job{ID: "workflow-job", Agent: "worker", Type: "implement", State: "succeeded",
		Payload: `{"workflow_id":"gitmoot4/blocked-reconcile-953","repo":"owner/repo","branch":"feature/blocked","pull_request":953}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	client := &fakeGitHub{
		pullsByState:  map[string][]github.PullRequest{"open": nil, "closed": nil},
		pullsByNumber: map[int64]github.PullRequest{953: mergedPull(953, "feature/blocked")},
		comments:      map[int64][]github.IssueComment{},
	}
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	assertExternalMergeState(t, store, repo.FullName(), "blocked-task", 953, workflow.TaskMerged, "merged")
	notes, err := store.ListWorkflowNotes(ctx, "gitmoot4/blocked-reconcile-953", 0)
	if err != nil || len(notes) != 1 || notes[0].Author != db.WorkflowAutoNoteAuthor || notes[0].Body != "[auto:pr:953:merged] PR #953 merged" {
		t.Fatalf("workflow notes after two ticks = %+v, err=%v", notes, err)
	}
	meta, err := store.GetWorkflowMeta(ctx, "gitmoot4/blocked-reconcile-953")
	if err != nil || meta.Status != "active" || meta.Description != "blocked-reconcile-953" {
		t.Fatalf("workflow meta after two ticks = %+v, err=%v", meta, err)
	}
}

func TestExternalMergeCandidateState(t *testing.T) {
	tests := []struct {
		state workflow.TaskState
		want  bool
	}{
		{workflow.TaskPullRequestOpen, true},
		{workflow.TaskReviewing, true},
		{workflow.TaskChangesRequested, true},
		{workflow.TaskReadyToMerge, true},
		{workflow.TaskBlocked, true},
		{workflow.TaskAwaitingHuman, false},
		{workflow.TaskPlanned, false},
	}
	for _, test := range tests {
		t.Run(string(test.state), func(t *testing.T) {
			if got := externalMergeCandidateState(string(test.state)); got != test.want {
				t.Fatalf("externalMergeCandidateState(%q) = %v, want %v", test.state, got, test.want)
			}
		})
	}
}

func TestPollOnceBlocksClosedUnmergedPullRequestOpenTask(t *testing.T) {
	ctx := context.Background()
	repo := github.Repository{Owner: "owner", Name: "repo"}
	store := testStore(t)
	seedExternalMergeTask(t, store, repo, "task-7", "feature/seven", workflow.TaskPullRequestOpen, 7)
	client := &fakeGitHub{
		pullsByState: map[string][]github.PullRequest{"open": nil, "closed": {
			{Number: 7, State: "closed", HeadRef: "feature/seven", HeadSHA: "head-7"},
		}},
		pullsByNumber: map[int64]github.PullRequest{7: {Number: 7, State: "closed", HeadRef: "feature/seven", HeadSHA: "head-7"}},
		comments:      map[int64][]github.IssueComment{},
	}
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	assertExternalMergeState(t, store, repo.FullName(), "task-7", 7, workflow.TaskBlocked, "closed")
	events, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil || len(events) != 1 || events[0].Kind != "pr_closed_unmerged" {
		t.Fatalf("task events = %+v, err=%v", events, err)
	}
}

// TestPollOnceRecordsClosedBreadcrumbForWorkflowLinkedPROpenTask covers #958's
// acceptance criterion that a closed-unmerged PR reads as "closed" on the
// workflow view even when the task never entered `reviewing`. The clean closed-
// unmerged detection blocks the task while the workflow breadcrumb remains
// idempotent across ticks and never implies success.
func TestPollOnceRecordsClosedBreadcrumbForWorkflowLinkedPROpenTask(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/throwaway")
	t.Setenv("HERDR_ENV", "")

	ctx := context.Background()
	repo := github.Repository{Owner: "owner", Name: "repo"}
	store := testStore(t)
	seedExternalMergeTask(t, store, repo, "task-7", "feature/seven", workflow.TaskPullRequestOpen, 7)
	if err := store.CreateJob(ctx, db.Job{ID: "wf-job", Agent: "worker", Type: "implement", State: "succeeded",
		Payload: `{"workflow_id":"gitmoot4/selfdesc-958","repo":"owner/repo","branch":"feature/seven","pull_request":7}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	client := &fakeGitHub{
		pullsByState: map[string][]github.PullRequest{"open": nil, "closed": {
			{Number: 7, State: "closed", HeadRef: "feature/seven", HeadSHA: "head-7"},
		}},
		pullsByNumber: map[int64]github.PullRequest{7: {Number: 7, State: "closed", HeadRef: "feature/seven", HeadSHA: "head-7"}},
		comments:      map[int64][]github.IssueComment{},
	}
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}

	assertExternalMergeState(t, store, repo.FullName(), "task-7", 7, workflow.TaskBlocked, "closed")
	taskEvents, err := store.ListTaskEvents(ctx, "task-7")
	if err != nil || len(taskEvents) != 1 || taskEvents[0].Kind != "pr_closed_unmerged" {
		t.Fatalf("task events after two ticks = %+v, err=%v", taskEvents, err)
	}

	notes, err := store.ListWorkflowNotes(ctx, "gitmoot4/selfdesc-958", 0)
	if err != nil || len(notes) != 1 || notes[0].Author != db.WorkflowAutoNoteAuthor || notes[0].Body != "[auto:pr:7:closed] PR #7 closed without merging" {
		t.Fatalf("workflow notes after two ticks = %+v, err=%v", notes, err)
	}
	meta, err := store.GetWorkflowMeta(ctx, "gitmoot4/selfdesc-958")
	if err != nil || meta.Status != "active" {
		t.Fatalf("workflow meta after two ticks = %+v, err=%v", meta, err)
	}
}

func TestPollOnceReconcilesEmptyBranchReviewTaskByPullRequestNumber(t *testing.T) {
	ctx := context.Background()
	repo := github.Repository{Owner: "owner", Name: "repo"}
	store := testStore(t)
	// The implement task owns the unique (repo, branch) slot. The legacy review
	// task is intentionally branchless and can only be associated through its id.
	if err := store.UpsertTask(ctx, db.Task{ID: "implement-11", RepoFullName: repo.FullName(), Title: "Implement", State: string(workflow.TaskImplementing), Branch: "feature/eleven"}); err != nil {
		t.Fatalf("Upsert implement task: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "review-pr-11-44cd8322", RepoFullName: repo.FullName(), GoalID: "local-review", Title: "Review PR #11", State: string(workflow.TaskReviewing)}); err != nil {
		t.Fatalf("Upsert review task: %v", err)
	}
	client := &fakeGitHub{
		pullsByState:  map[string][]github.PullRequest{"open": nil, "closed": nil},
		pullsByNumber: map[int64]github.PullRequest{11: mergedPull(11, "feature/eleven")},
		comments:      map[int64][]github.IssueComment{},
	}
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	reviewTask, err := store.GetTask(ctx, "review-pr-11-44cd8322")
	if err != nil || reviewTask.State != string(workflow.TaskMerged) || reviewTask.Branch != "" {
		t.Fatalf("review task = %+v, err=%v; want merged with empty branch", reviewTask, err)
	}
	implementTask, err := store.GetTask(ctx, "implement-11")
	if err != nil || implementTask.State != string(workflow.TaskImplementing) || implementTask.Branch != "feature/eleven" {
		t.Fatalf("implement task changed = %+v, err=%v", implementTask, err)
	}
	pr, err := store.GetPullRequest(ctx, repo.FullName(), 11)
	if err != nil || pr.State != "merged" || pr.HeadBranch != "feature/eleven" {
		t.Fatalf("PR mirror = %+v, err=%v", pr, err)
	}
}

func TestExternalMergeReconcileCapsTargetedLookupsPerTick(t *testing.T) {
	ctx := context.Background()
	repo := github.Repository{Owner: "owner", Name: "repo"}
	store := testStore(t)
	client := &fakeGitHub{
		pullsByState:  map[string][]github.PullRequest{"open": nil, "closed": nil},
		pullsByNumber: map[int64]github.PullRequest{},
		comments:      map[int64][]github.IssueComment{},
	}
	for i := 1; i <= externalMergeReconcileLookupLimit+5; i++ {
		id := fmt.Sprintf("task-%02d", i)
		branch := fmt.Sprintf("feature/%02d", i)
		seedExternalMergeTask(t, store, repo, id, branch, workflow.TaskPullRequestOpen, int64(i))
		client.pullsByNumber[int64(i)] = mergedPull(int64(i), branch)
	}
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(client.getPullRequestCalls) != externalMergeReconcileLookupLimit {
		t.Fatalf("GetPullRequest calls = %d (%v), want cap %d", len(client.getPullRequestCalls), client.getPullRequestCalls, externalMergeReconcileLookupLimit)
	}
	merged := 0
	for i := 1; i <= externalMergeReconcileLookupLimit+5; i++ {
		task, err := store.GetTask(ctx, fmt.Sprintf("task-%02d", i))
		if err != nil {
			t.Fatalf("GetTask(%d): %v", i, err)
		}
		if task.State == string(workflow.TaskMerged) {
			merged++
		}
	}
	if merged != externalMergeReconcileLookupLimit {
		t.Fatalf("merged tasks = %d, want %d", merged, externalMergeReconcileLookupLimit)
	}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	if len(client.getPullRequestCalls) != externalMergeReconcileLookupLimit+5 {
		t.Fatalf("GetPullRequest calls after second tick = %d (%v), want %d", len(client.getPullRequestCalls), client.getPullRequestCalls, externalMergeReconcileLookupLimit+5)
	}
	for i := 1; i <= externalMergeReconcileLookupLimit+5; i++ {
		task, err := store.GetTask(ctx, fmt.Sprintf("task-%02d", i))
		if err != nil || task.State != string(workflow.TaskMerged) {
			t.Fatalf("task %d after second tick = %+v, err=%v; want merged", i, task, err)
		}
	}
}

func TestReviewTaskPullRequestNumber(t *testing.T) {
	tests := []struct {
		id     string
		number int64
		ok     bool
	}{
		{id: "review-pr-11-44cd8322", number: 11, ok: true},
		{id: "review-pr-1-a", number: 1, ok: true},
		{id: "review-pr-0-a"},
		{id: "review-pr-x-a"},
		{id: "review-pr-11"},
		{id: "task-11-a"},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			number, ok := reviewTaskPullRequestNumber(test.id)
			if number != test.number || ok != test.ok {
				t.Fatalf("reviewTaskPullRequestNumber(%q) = (%d,%v), want (%d,%v)", test.id, number, ok, test.number, test.ok)
			}
		})
	}
}

func seedExternalMergeTask(t *testing.T, store *db.Store, repo github.Repository, id, branch string, state workflow.TaskState, number int64) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertTask(ctx, db.Task{ID: id, RepoFullName: repo.FullName(), GoalID: "goal-1", Title: id, State: string(state), Branch: branch}); err != nil {
		t.Fatalf("UpsertTask(%s): %v", id, err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: number, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo.FullName(), number),
		HeadBranch: branch, BaseBranch: "main", HeadSHA: fmt.Sprintf("head-%d", number), State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest(%d): %v", number, err)
	}
}

func mergedPull(number int64, branch string) github.PullRequest {
	return github.PullRequest{Number: number, State: "closed", Merged: true, MergedAt: "2026-07-13T00:00:00Z", HeadRef: branch, BaseRef: "main", HeadSHA: fmt.Sprintf("head-%d", number)}
}

func assertExternalMergeState(t *testing.T, store *db.Store, repo, taskID string, number int64, taskState workflow.TaskState, prState string) {
	t.Helper()
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil || task.State != string(taskState) {
		t.Fatalf("task %s = %+v, err=%v; want state %s", taskID, task, err, taskState)
	}
	pr, err := store.GetPullRequest(context.Background(), repo, number)
	if err != nil || pr.State != prState {
		t.Fatalf("PR #%d = %+v, err=%v; want state %s", number, pr, err, prState)
	}
}
