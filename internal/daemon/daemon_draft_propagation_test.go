package daemon

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestDaemonPullRequestOpenedDraftDoesNotParkTask(t *testing.T) {
	ctx := context.Background()
	store, repo, pull := setupDaemonDraftPropagationTest(t, workflow.TaskPlanned)
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{LeaveOpen: true, Reason: workflow.PlainReason("human merge required")}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, Workflow: &engine}

	if _, err := daemon.handlePullRequestWorkflowChange(ctx, pull, newReviewJobsMemo(store)); err != nil {
		t.Fatalf("handlePullRequestWorkflowChange returned error: %v", err)
	}
	assertDaemonDraftReachedGateAndTaskState(t, store, gate, workflow.TaskPullRequestOpen)
}

func TestDaemonReadyToMergeDraftDoesNotParkTask(t *testing.T) {
	ctx := context.Background()
	store, repo, pull := setupDaemonDraftPropagationTest(t, workflow.TaskReadyToMerge)
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{LeaveOpen: true, Reason: workflow.PlainReason("human merge required")}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, Workflow: &engine}

	if err := daemon.handleReadyToMergeWorkflow(ctx, pull); err != nil {
		t.Fatalf("handleReadyToMergeWorkflow returned error: %v", err)
	}
	assertDaemonDraftReachedGateAndTaskState(t, store, gate, workflow.TaskReadyToMerge)
}

func TestDaemonMergeCommandDraftDoesNotParkTask(t *testing.T) {
	ctx := context.Background()
	store, repo, pull := setupDaemonDraftPropagationTest(t, workflow.TaskReadyToMerge)
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{LeaveOpen: true, Reason: workflow.PlainReason("human merge required")}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	client := &fakeGitHub{}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.handleMergeCommand(ctx, pull, github.IssueComment{ID: 91, Body: "/gitmoot merge", Author: "owner"}); err != nil {
		t.Fatalf("handleMergeCommand returned error: %v", err)
	}
	assertDaemonDraftReachedGateAndTaskState(t, store, gate, workflow.TaskReadyToMerge)
}

func setupDaemonDraftPropagationTest(t *testing.T, state workflow.TaskState) (*db.Store, github.Repository, github.PullRequest) {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-draft",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-draft",
		Title:        "Draft task",
		State:        string(state),
		Branch:       "draft-branch",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "builder",
		Role:           "builder",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: repo.FullName(),
		Branch:       "draft-branch",
		Owner:        "builder",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	return store, repo, github.PullRequest{
		Number:  91,
		Title:   "Draft task",
		State:   "open",
		HeadRef: "draft-branch",
		BaseRef: "main",
		HeadSHA: "draft-head",
		Draft:   true,
	}
}

func assertDaemonDraftReachedGateAndTaskState(t *testing.T, store *db.Store, gate *fakeWorkflowMergeGate, want workflow.TaskState) {
	t.Helper()
	task, err := store.GetTask(context.Background(), "task-draft")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(want) {
		t.Fatalf("task state = %q, want %q (draft PR must not park at awaiting_human_merge)", task.State, want)
	}
	if len(gate.requests) != 1 {
		t.Fatalf("merge gate requests = %+v, want exactly 1", gate.requests)
	}
	if !gate.requests[0].PullRequestDraft {
		t.Fatalf("merge request PullRequestDraft = false, want true")
	}
}
