package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestPollOnceReconcilesExternallyMergedLifecycleTasks(t *testing.T) {
	states := []workflow.TaskState{
		workflow.TaskPullRequestOpen,
		workflow.TaskReviewing,
		workflow.TaskChangesRequested,
		workflow.TaskReadyToMerge,
		workflow.TaskAwaitingHumanMerge,
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
		{workflow.TaskAwaitingHumanMerge, true},
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

func TestPollOnceReconcilesBranchlessQueuedMergeAsMerged(t *testing.T) {
	runBranchlessQueuedMergeTerminalPolls(t, mergedPull(1732, "task-1732"),
		workflow.TaskMerged, "merged", "pull_request_merged")
}

func TestPollOnceReconcilesBranchlessQueuedMergeAsClosedUnmerged(t *testing.T) {
	runBranchlessQueuedMergeTerminalPolls(t, github.PullRequest{
		Number: 1732, Title: "Review PR #1732", State: "closed",
		HeadRef: "task-1732", BaseRef: "main", HeadSHA: "queued-head",
	}, workflow.TaskBlocked, "closed", "pr_closed_unmerged")
}

func runBranchlessQueuedMergeTerminalPolls(t *testing.T, terminal github.PullRequest, wantTask workflow.TaskState, wantPRState, wantEvent string) {
	t.Helper()
	ctx := context.Background()
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	store := testStore(t)
	mergeable := true
	open := github.PullRequest{
		Number: 1732, Title: "Review PR #1732", State: "open",
		HeadRef: "task-1732", BaseRef: "main", HeadSHA: "queued-head",
		Mergeable: &mergeable,
	}
	taskID := "review-pr-1732-retained"
	workflowID := "gitmoot4/branchless-cleanup-1732"
	seedReadyBranchlessReviewTask(t, store, repo, taskID, open, open.HeadSHA)
	worktreePath := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{
		ID: taskID, RepoFullName: repo.FullName(), State: string(workflow.TaskReadyToMerge),
		WorktreePath: worktreePath,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "builder", Role: "implementer", Runtime: "codex", RuntimeRef: "last",
		RepoScope: repo.FullName(), Capabilities: []string{"implement"},
		AutonomyPolicy: "danger-full-access", HealthStatus: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: repo.FullName(), Branch: open.HeadRef, Owner: "builder",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock = acquired %v err %v", acquired, err)
	}
	if err := store.SetBranchLockReviewFanout(ctx, repo.FullName(), open.HeadRef, true); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: open.Number,
		URL:        "https://github.com/gitmoot/gitmoot/pull/1732",
		HeadBranch: open.HeadRef, BaseBranch: open.BaseRef, HeadSHA: open.HeadSHA, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	implementPayload, err := json.Marshal(workflow.JobPayload{
		Repo: repo.FullName(), Branch: open.HeadRef, PullRequest: int(open.Number),
		HeadSHA: open.HeadSHA, TaskID: taskID, WorkflowID: workflowID,
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "implement-1732", Agent: "builder", Type: "implement",
		State: string(workflow.JobSucceeded), Payload: string(implementPayload),
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "done"}); err != nil {
		t.Fatal(err)
	}
	base := &fakeGitHub{
		pullsByState:  map[string][]github.PullRequest{"open": {open}, "closed": nil},
		pullsByNumber: map[int64]github.PullRequest{open.Number: open},
		comments:      map[int64][]github.IssueComment{open.Number: {}},
	}
	runner := &queuedPollMergeRunner{}
	client := &queuedMergePollGitHub{
		mergeGateRaceGitHub: &mergeGateRaceGitHub{
			fakeGitHub: base,
			checks: []github.PullRequestCheck{{
				Name: "ci", Bucket: "pass", State: "SUCCESS",
			}},
		},
		mergeClient: github.GhClient{Runner: runner},
	}
	postMergeGit := &canonicalPostMergeGit{}
	worktrees := &recordingWorktreeCleaner{}
	nextTasks := &recordingNextTaskEnqueuer{}
	gate := &workflow.PolicyMergeGate{
		AutoMerge: true, Store: store, GitHub: client, Git: postMergeGit,
		Worktrees: worktrees, NextTasks: nextTasks,
	}
	engine := workflow.Engine{Store: store, MergeGate: gate, RequiredReviewers: []string{"reviewer"}}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("queued PollOnce: %v", err)
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil || task.State != string(workflow.TaskReadyToMerge) || task.Branch != "" {
		gateState, _ := store.GetMergeGate(ctx, repo.FullName(), open.Number)
		events, _ := store.ListTaskEvents(ctx, taskID)
		t.Fatalf("queued task = %+v err=%v gate=%+v events=%+v, want branchless ready_to_merge", task, err, gateState, events)
	}
	if len(client.merges) != 1 || runner.calls != 2 {
		t.Fatalf("queued production merge attempts=%d adapter calls=%d, want one command and one confirmation",
			len(client.merges), runner.calls)
	}
	queuedGate, err := store.GetMergeGate(ctx, repo.FullName(), open.Number)
	if err != nil || queuedGate.State != "pending" {
		t.Fatalf("queued merge gate = %+v err=%v, want pending", queuedGate, err)
	}
	writer, err := db.OpenAlreadyMigrated(store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	changed, _, writeErr := writer.CompareAndSwapTaskState(ctx, taskID,
		string(workflow.TaskReadyToMerge), string(workflow.TaskDismissed))
	if writeErr == nil || changed || !strings.Contains(writeErr.Error(), "claimed for an external merge") {
		t.Fatalf("queued conflicting writer = changed %v err %v, want retained claim rejection", changed, writeErr)
	}

	base.pullsByState["open"] = nil
	base.pullsByState["closed"] = []github.PullRequest{terminal}
	base.pullsByNumber[terminal.Number] = terminal
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("terminal PollOnce: %v", err)
	}
	task, err = store.GetTask(ctx, taskID)
	if err != nil || task.State != string(wantTask) || task.Branch != "" {
		t.Fatalf("terminal task = %+v err=%v, want branchless %s", task, err, wantTask)
	}
	pr, err := store.GetPullRequest(ctx, repo.FullName(), terminal.Number)
	if err != nil || pr.State != wantPRState || pr.HeadBranch != terminal.HeadRef {
		t.Fatalf("terminal PR mirror = %+v err=%v, want state %q branch %q", pr, err, wantPRState, terminal.HeadRef)
	}
	events, err := store.ListTaskEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for _, event := range events {
		if event.Kind == wantEvent {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("terminal task events = %+v, want one %q", events, wantEvent)
	}
	notes, err := store.ListWorkflowNotes(ctx, workflowID, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantNote := fmt.Sprintf("[auto:pr:%d:merged] PR #%d merged", terminal.Number, terminal.Number)
	if wantTask != workflow.TaskMerged {
		wantNote = fmt.Sprintf("[auto:pr:%d:closed] PR #%d closed without merging", terminal.Number, terminal.Number)
	}
	terminalNotes := 0
	for _, note := range notes {
		if note.Author == db.WorkflowAutoNoteAuthor && note.Body == wantNote {
			terminalNotes++
		}
	}
	if terminalNotes != 1 {
		t.Fatalf("terminal workflow notes = %+v, want one %q", notes, wantNote)
	}
	token, claimed, current, err := store.ClaimTaskState(ctx, taskID, string(wantTask),
		db.TaskStateClaimKindExternalMerge, time.Minute)
	if err != nil || !claimed || current != string(wantTask) {
		t.Fatalf("post-reconcile claim = token %q claimed %v current %q err %v, want resolved retained claim",
			token, claimed, current, err)
	}
	if err := store.ReleaseTaskStateClaim(ctx, taskID, token); err != nil {
		t.Fatal(err)
	}
	mergedOutcome := wantTask == workflow.TaskMerged
	terminalGate, err := store.GetMergeGate(ctx, repo.FullName(), terminal.Number)
	if err != nil {
		t.Fatal(err)
	}
	if mergedOutcome {
		if terminalGate.State != "merged" {
			t.Fatalf("terminal merge gate = %+v, want merged", terminalGate)
		}
		if _, err := store.GetBranchLock(ctx, repo.FullName(), open.HeadRef); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("branch lock after merged PollOnce = %v, want released", err)
		}
		if task.WorktreePath != "" || len(worktrees.paths) != 1 || worktrees.paths[0] != worktreePath {
			t.Fatalf("merged worktree task=%+v removals=%v, want one canonical cleanup", task, worktrees.paths)
		}
		if len(postMergeGit.updates) != 1 || postMergeGit.updates[0] != "origin/main" {
			t.Fatalf("post-merge base updates = %v, want [origin/main]", postMergeGit.updates)
		}
		if len(nextTasks.taskIDs) != 1 || nextTasks.taskIDs[0] != taskID {
			t.Fatalf("post-merge continuations = %v, want [%s]", nextTasks.taskIDs, taskID)
		}
	} else {
		if terminalGate.State != "pending" {
			t.Fatalf("closed-unmerged merge gate = %+v, want retained pending history", terminalGate)
		}
		if _, err := store.GetBranchLock(ctx, repo.FullName(), open.HeadRef); err != nil {
			t.Fatalf("closed-unmerged branch lock = %v, want preserved", err)
		}
		if task.WorktreePath != worktreePath || len(worktrees.paths) != 0 ||
			len(postMergeGit.updates) != 0 || len(nextTasks.taskIDs) != 0 {
			t.Fatalf("closed-unmerged cleanup task=%+v removals=%v updates=%v continuations=%v, want none",
				task, worktrees.paths, postMergeGit.updates, nextTasks.taskIDs)
		}
	}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("stable PollOnce: %v", err)
	}
	task, err = store.GetTask(ctx, taskID)
	if err != nil || task.State != string(wantTask) {
		t.Fatalf("stable task = %+v err=%v, want %s", task, err, wantTask)
	}
	stablePR, err := store.GetPullRequest(ctx, repo.FullName(), terminal.Number)
	if err != nil || stablePR.State != pr.State || stablePR.HeadBranch != pr.HeadBranch {
		t.Fatalf("stable PR mirror = %+v err=%v, want unchanged from %+v", stablePR, err, pr)
	}
	stableGate, err := store.GetMergeGate(ctx, repo.FullName(), terminal.Number)
	if err != nil || stableGate.State != terminalGate.State || stableGate.Reason != terminalGate.Reason {
		t.Fatalf("stable merge gate = %+v err=%v, want unchanged from %+v", stableGate, err, terminalGate)
	}
	stableEvents, err := store.ListTaskEvents(ctx, taskID)
	if err != nil || len(stableEvents) != len(events) {
		t.Fatalf("stable task events = %+v err=%v, want %d events", stableEvents, err, len(events))
	}
	stableNotes, err := store.ListWorkflowNotes(ctx, workflowID, 0)
	if err != nil || !reflect.DeepEqual(stableNotes, notes) {
		t.Fatalf("stable workflow notes = %+v err=%v, want unchanged from %+v", stableNotes, err, notes)
	}
	wantCleanupCount := 0
	if mergedOutcome {
		wantCleanupCount = 1
	}
	if len(worktrees.paths) != wantCleanupCount || len(postMergeGit.updates) != wantCleanupCount ||
		len(nextTasks.taskIDs) != wantCleanupCount {
		t.Fatalf("stable cleanup removals=%v updates=%v continuations=%v, want count %d",
			worktrees.paths, postMergeGit.updates, nextTasks.taskIDs, wantCleanupCount)
	}
	if len(client.merges) != 1 || runner.calls != 2 {
		t.Fatalf("stable production merge attempts=%d adapter calls=%d, want one merge command total",
			len(client.merges), runner.calls)
	}
	if mergedOutcome {
		if _, err := store.GetBranchLock(ctx, repo.FullName(), open.HeadRef); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("stable merged branch lock = %v, want absent", err)
		}
	}
}

type canonicalPostMergeGit struct {
	updates []string
}

func (*canonicalPostMergeGit) WorktreeClean(context.Context) (bool, error) {
	return true, nil
}

func (g *canonicalPostMergeGit) UpdateBase(_ context.Context, remote, branch string) error {
	g.updates = append(g.updates, remote+"/"+branch)
	return nil
}

type recordingWorktreeCleaner struct {
	paths []string
}

func (c *recordingWorktreeCleaner) RemoveWorktree(_ context.Context, path string) error {
	c.paths = append(c.paths, path)
	return nil
}

type recordingNextTaskEnqueuer struct {
	taskIDs []string
}

func (e *recordingNextTaskEnqueuer) EnqueueNextTask(_ context.Context, completedTaskID string) error {
	e.taskIDs = append(e.taskIDs, completedTaskID)
	return nil
}

type queuedMergePollGitHub struct {
	*mergeGateRaceGitHub
	mergeClient github.GhClient
}

func (g *queuedMergePollGitHub) MergePullRequest(ctx context.Context, input github.MergePullRequestInput) (github.MergeResult, error) {
	g.merges = append(g.merges, input)
	return g.mergeClient.MergePullRequest(ctx, input)
}

type queuedPollMergeRunner struct {
	calls int
}

func (r *queuedPollMergeRunner) Run(_ context.Context, _ string, _ string, _ ...string) (subprocess.Result, error) {
	r.calls++
	switch r.calls {
	case 1:
		return subprocess.Result{Stdout: "queued"}, nil
	case 2:
		return subprocess.Result{Stdout: `{"number":1732,"title":"Review PR #1732","state":"open","html_url":"https://github.com/gitmoot/gitmoot/pull/1732","head":{"ref":"task-1732","sha":"queued-head"},"base":{"ref":"main"}}`}, nil
	default:
		return subprocess.Result{}, fmt.Errorf("unexpected queued merge adapter call %d", r.calls)
	}
}

func (*queuedPollMergeRunner) LookPath(file string) (string, error) {
	return file, nil
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
