package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1871 round-3 P1, at the production boundary. The fail-closed refusal added for
// the round-2 P1 was correct but it SETTLED: it recorded a durable event and
// returned nil, so the approval read as advanced and nothing ever re-drove it once
// the daemon inserted the pull_requests row. A two-poll probe ended with the task
// still changes_requested and zero gate requests.
//
// The recovery has two halves and this test drives both, because neither half
// lives where the other does: the daemon poll makes the head OBSERVABLE
// (recordPullRequest), and the cli worker's advance-retry RE-DRIVES the held
// advancement. The engine's contract between them is that a transient hold
// returns an ERROR - that is what leaves the advancement unreconciled and keeps
// the job a retry candidate. If the hold ever goes back to returning nil, step 1
// fails here.
func TestHeldApprovalRecoversOnceThePullRequestRowIsObservable(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}

	if err := store.UpsertTask(ctx, db.Task{
		ID:           "review-pr-9-3f3a1026",
		RepoFullName: repo.FullName(),
		GoalID:       "local-review",
		Title:        "Review PR #9",
		State:        string(workflow.TaskChangesRequested),
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	seed := func(id, agent, head, decision, state string) {
		t.Helper()
		payload, err := json.Marshal(workflow.JobPayload{
			Repo: repo.FullName(), Branch: "task-9", PullRequest: 9, HeadSHA: head,
			TaskID: "review-pr-9-3f3a1026",
			Result: &workflow.AgentResult{Decision: decision, Summary: decision},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateJob(ctx, db.Job{
			ID: id, Agent: agent, Type: "review", State: state, Payload: string(payload),
		}); err != nil {
			t.Fatalf("CreateJob(%s) returned error: %v", id, err)
		}
	}
	implement, err := json.Marshal(workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-9", PullRequest: 9, HeadSHA: "head-new",
		TaskID: "review-pr-9-3f3a1026",
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: "implement-job", Agent: "implementer", Type: "implement",
		State: string(workflow.JobSucceeded), Payload: string(implement),
	}); err != nil {
		t.Fatal(err)
	}
	seed("review-old", "auditor", "head-old", "changes_requested", string(workflow.JobSucceeded))
	seed("review-new", "auditor", "head-new", "approved", string(workflow.JobSucceeded))

	pull := github.PullRequest{
		Number: 9, Title: "Review PR #9", State: "open",
		URL:     "https://github.com/gitmoot/gitmoot/pull/9",
		HeadRef: "task-9", BaseRef: "main", HeadSHA: "head-new",
	}
	client := &fakeGitHub{
		pulls:    []github.PullRequest{pull},
		comments: map[int64][]github.IssueComment{pull.Number: {}},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	// STEP 1: no pull_requests row yet. The approval must be HELD, and held in the
	// retryable shape - an error, not a settled nil.
	err = engine.AdvanceJob(ctx, "review-new")
	if err == nil {
		t.Fatal("AdvanceJob returned nil with no observable head: the hold must stay retryable, or nothing will re-drive it")
	}
	if !strings.Contains(err.Error(), "no observed pull request row") {
		t.Fatalf("hold error = %v, want the missing pull request row named", err)
	}
	if len(gate.requests) != 0 {
		t.Fatalf("gate requests = %d, want 0 while the head is unobservable", len(gate.requests))
	}
	assertHeldTaskState(t, store, workflow.TaskChangesRequested)

	// STEP 2: the daemon poll observes the PR. It must make the head observable
	// WITHOUT resetting the objection - this task holds no branch lock, so the
	// lock-gated reset path is skipped while recordPullRequest still runs.
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	stored, err := store.GetPullRequest(ctx, repo.FullName(), 9)
	if err != nil {
		t.Fatalf("GetPullRequest after poll: %v", err)
	}
	if stored.HeadSHA != "head-new" {
		t.Fatalf("stored head = %q, want head-new: the poll must make the current head observable", stored.HeadSHA)
	}
	assertHeldTaskState(t, store, workflow.TaskChangesRequested)

	// STEP 3: the advance-retry re-drives the SAME approval, which is now admitted.
	// This is the step that returned gate_requests=0 before the fix.
	if err := engine.AdvanceJob(ctx, "review-new"); err != nil {
		t.Fatalf("AdvanceJob after the row appeared: %v", err)
	}
	if len(gate.requests) == 0 {
		t.Fatal("gate was never asked: the held approval was not recovered after the head became observable")
	}
	if got := gate.requests[len(gate.requests)-1].ExpectedTaskState; got != string(workflow.TaskChangesRequested) {
		t.Fatalf("gate claimed %q, want changes_requested: the CAS must match the row it is fencing", got)
	}
	task, err := store.GetTask(ctx, "review-pr-9-3f3a1026")
	if err != nil {
		t.Fatal(err)
	}
	if task.State == string(workflow.TaskChangesRequested) {
		t.Fatal("task is still changes_requested after recovery")
	}
}

func assertHeldTaskState(t *testing.T, store *db.Store, want workflow.TaskState) {
	t.Helper()
	task, err := store.GetTask(context.Background(), "review-pr-9-3f3a1026")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(want) {
		t.Fatalf("task state = %q, want %q", task.State, want)
	}
}
