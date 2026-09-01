package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestDaemonDraftFieldAbsentDoesNotParkTask(t *testing.T) {
	ctx := context.Background()
	store, repo, _ := setupDaemonDraftPropagationTest(t, workflow.TaskPlanned)
	pull := decodeDaemonDraftPull(t, "")
	gate := &draftKnowledgeGate{}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, Workflow: &engine}

	if _, err := daemon.handlePullRequestWorkflowChange(ctx, pull, newReviewJobsMemo(store)); err != nil {
		t.Fatalf("handlePullRequestWorkflowChange returned error: %v", err)
	}
	assertDraftTaskState(t, store, workflow.TaskReadyToMerge)
	assertDraftKnowledgeRequest(t, gate.requests, true)
}

func TestDaemonDraftFieldNullDoesNotParkTask(t *testing.T) {
	ctx := context.Background()
	store, repo, _ := setupDaemonDraftPropagationTest(t, workflow.TaskReadyToMerge)
	pull := decodeDaemonDraftPull(t, "null")
	gate := &draftKnowledgeGate{}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, Workflow: &engine}

	task, err := store.GetTask(ctx, "task-draft")
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.handleReadyToMergeWorkflow(ctx, pull, task); err != nil {
		t.Fatalf("handleReadyToMergeWorkflow returned error: %v", err)
	}
	assertDraftTaskState(t, store, workflow.TaskReadyToMerge)
	assertDraftKnowledgeRequest(t, gate.requests, true)
}

func TestDaemonUnknownDraftPolarityDoesNotParkTask(t *testing.T) {
	ctx := context.Background()
	store, repo, pull := setupDaemonDraftPropagationTest(t, workflow.TaskReadyToMerge)
	pull.Draft = false
	pull.DraftUnknown = true
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{LeaveOpen: true, Reason: workflow.PlainReason("human merge required")}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: &fakeGitHub{}, Workflow: &engine}

	if err := daemon.handleMergeCommand(ctx, pull, github.IssueComment{ID: 92, Body: "/gitmoot merge", Author: "owner"}); err != nil {
		t.Fatalf("handleMergeCommand returned error: %v", err)
	}
	assertDraftTaskState(t, store, workflow.TaskReadyToMerge)
	assertDraftKnowledgeRequest(t, gate.requests, true)
}

func TestDaemonConfirmedNonDraftParksTask(t *testing.T) {
	ctx := context.Background()
	store, repo, _ := setupDaemonDraftPropagationTest(t, workflow.TaskReadyToMerge)
	pull := decodeDaemonDraftPull(t, "false")
	gate := &draftKnowledgeGate{}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, Workflow: &engine}

	task, err := store.GetTask(ctx, "task-draft")
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.handleReadyToMergeWorkflow(ctx, pull, task); err != nil {
		t.Fatalf("handleReadyToMergeWorkflow returned error: %v", err)
	}
	assertDraftTaskState(t, store, workflow.TaskAwaitingHumanMerge)
	assertDraftKnowledgeRequest(t, gate.requests, false)
}

type draftKnowledgeGate struct {
	requests []workflow.MergeRequest
}

func (g *draftKnowledgeGate) Evaluate(_ context.Context, request workflow.MergeRequest) (workflow.MergeDecision, error) {
	g.requests = append(g.requests, request)
	if request.PullRequestDraftUnknown {
		return workflow.MergeDecision{Ready: true}, nil
	}
	return workflow.MergeDecision{LeaveOpen: true, Reason: workflow.PlainReason("human merge required")}, nil
}

func decodeDaemonDraftPull(t *testing.T, draftJSON string) github.PullRequest {
	t.Helper()
	draftField := ""
	if draftJSON != "" {
		draftField = `,"draft":` + draftJSON
	}
	body := `{"number":91,"title":"Draft task","state":"open","head":{"ref":"draft-branch","sha":"draft-head"},"base":{"ref":"main"}` + draftField + `}`
	var pull github.PullRequest
	if err := json.Unmarshal([]byte(body), &pull); err != nil {
		t.Fatalf("Unmarshal pull request returned error: %v", err)
	}
	return pull
}

func assertDraftKnowledgeRequest(t *testing.T, requests []workflow.MergeRequest, wantUnknown bool) {
	t.Helper()
	if len(requests) != 1 {
		t.Fatalf("merge gate requests = %+v, want exactly 1", requests)
	}
	if requests[0].PullRequestDraftUnknown != wantUnknown {
		t.Fatalf("PullRequestDraftUnknown = %v, want %v", requests[0].PullRequestDraftUnknown, wantUnknown)
	}
}

func assertDraftTaskState(t *testing.T, store *db.Store, want workflow.TaskState) {
	t.Helper()
	task, err := store.GetTask(context.Background(), "task-draft")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(want) {
		t.Fatalf("task state = %q, want %q", task.State, want)
	}
}
