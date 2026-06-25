package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// resumeFixture seeds a coordinator whose single escalate_human delegation failed
// and drives the engine to the durable awaiting_human pause, returning the engine
// (wired into a Daemon) and the PR the coordinator belongs to. The coordinator id
// is "coord" with PR #7.
func resumeFixture(t *testing.T, store *db.Store) (*workflow.Engine, github.PullRequest) {
	t.Helper()
	ctx := context.Background()
	repo := "jerryfane/gitmoot"
	seedResumeAgent(t, store, "coord", []string{"ask"}, repo)
	seedResumeAgent(t, store, "api", []string{"review"}, repo)

	engine := &workflow.Engine{
		Store: store,
		JobID: func(request workflow.JobRequest) string { return request.ID },
	}

	coordPayload := workflow.JobPayload{
		Repo:        repo,
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Parent",
		Sender:      "coord",
		Result: &workflow.AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []workflow.Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "escalate_human"},
			},
		},
	}
	createResumeJob(t, store, db.Job{ID: "coord", Agent: "coord", Type: "ask"}, coordPayload)
	if err := engine.AdvanceJob(ctx, "coord"); err != nil {
		t.Fatalf("AdvanceJob(coord) returned error: %v", err)
	}

	// The dispatched child exists; mark it failed and advance to pause.
	childID := "coord/delegation/api"
	childPayload := workflow.JobPayload{
		Repo:         repo,
		Branch:       "task-7",
		PullRequest:  7,
		TaskID:       "task-7",
		ParentJobID:  "coord",
		DelegationID: "api",
		RootJobID:    "coord",
		Result:       &workflow.AgentResult{Decision: "failed", Summary: "api broke"},
	}
	setResumeJobResult(t, store, childID, childPayload)
	if err := engine.AdvanceJob(ctx, childID); err == nil {
		t.Fatal("AdvanceJob(child) should return AwaitingHumanError")
	}

	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskAwaitingHuman) {
		t.Fatalf("task state = %q, want awaiting_human", task.State)
	}

	return engine, github.PullRequest{Number: 7, HeadRef: "task-7", HeadSHA: "abc"}
}

func seedResumeAgent(t *testing.T, store *db.Store, name string, caps []string, repo string) {
	t.Helper()
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           name,
		Role:           "agent",
		Runtime:        "shell",
		RuntimeRef:     "printf ok",
		RepoScope:      repo,
		Capabilities:   caps,
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent(%s) returned error: %v", name, err)
	}
}

func createResumeJob(t *testing.T, store *db.Store, job db.Job, payload workflow.JobPayload) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	job.State = string(workflow.JobSucceeded)
	job.Payload = string(encoded)
	job.ParentJobID = payload.ParentJobID
	job.DelegationID = payload.DelegationID
	if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "done"}); err != nil {
		t.Fatalf("CreateJobWithEvent(%s) returned error: %v", job.ID, err)
	}
}

func setResumeJobResult(t *testing.T, store *db.Store, jobID string, payload workflow.JobPayload) {
	t.Helper()
	ctx := context.Background()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, jobID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload(%s) returned error: %v", jobID, err)
	}
	if err := store.UpdateJobState(ctx, jobID, string(workflow.JobFailed)); err != nil {
		t.Fatalf("UpdateJobState(%s) returned error: %v", jobID, err)
	}
}

func TestHandleResumeRetryReenqueuesLeg(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	engine, pull := resumeFixture(t, store)
	client := &fakeGitHub{}
	d := Daemon{Repo: github.Repository{Owner: "jerryfane", Name: "gitmoot"}, Store: store, GitHub: client, Workflow: engine}

	if err := d.handleResumeCommand(ctx, pull, Command{Action: "resume", JobID: "coord", Decision: "retry", Instructions: "use staging"}); err != nil {
		t.Fatalf("handleResumeCommand(retry) returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "coord/delegation/api/resume"); err != nil {
		t.Fatalf("retry must re-enqueue the failing leg: %v", err)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "resumed job `coord` with `retry`") {
		t.Fatalf("ack = %+v", client.posted)
	}
}

func TestHandleResumeContinueEnqueuesContinuation(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	engine, pull := resumeFixture(t, store)
	client := &fakeGitHub{}
	d := Daemon{Repo: github.Repository{Owner: "jerryfane", Name: "gitmoot"}, Store: store, GitHub: client, Workflow: engine}

	if err := d.handleResumeCommand(ctx, pull, Command{Action: "resume", JobID: "coord", Decision: "continue"}); err != nil {
		t.Fatalf("handleResumeCommand(continue) returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "coord/continuation"); err != nil {
		t.Fatalf("continue must enqueue the coordinator continuation: %v", err)
	}
}

func TestHandleResumeAbortFinalizes(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	engine, pull := resumeFixture(t, store)
	client := &fakeGitHub{}
	d := Daemon{Repo: github.Repository{Owner: "jerryfane", Name: "gitmoot"}, Store: store, GitHub: client, Workflow: engine}

	if err := d.handleResumeCommand(ctx, pull, Command{Action: "resume", JobID: "coord", Decision: "abort"}); err != nil {
		t.Fatalf("handleResumeCommand(abort) returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "coord/continuation")
	if err != nil {
		t.Fatalf("abort must enqueue a finalize continuation: %v", err)
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("unmarshal continuation payload: %v", err)
	}
	if !payload.DelegationFinalize {
		t.Fatalf("abort continuation must carry DelegationFinalize: %+v", payload)
	}
}

func TestHandleResumeRejectsOutOfScopeJob(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	engine, _ := resumeFixture(t, store)
	client := &fakeGitHub{}
	d := Daemon{Repo: github.Repository{Owner: "jerryfane", Name: "gitmoot"}, Store: store, GitHub: client, Workflow: engine}

	// A PR that does not own the coordinator job (#999) must be rejected without
	// re-enqueueing anything.
	otherPull := github.PullRequest{Number: 999, HeadRef: "other"}
	if err := d.handleResumeCommand(ctx, otherPull, Command{Action: "resume", JobID: "coord", Decision: "retry"}); err != nil {
		t.Fatalf("handleResumeCommand returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "coord/delegation/api/resume"); err == nil {
		t.Fatal("an out-of-scope resume must not re-enqueue the leg")
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "could not resume") {
		t.Fatalf("expected a scope-rejection ack, got %+v", client.posted)
	}
}

// TestPollOnceResumeIsAuthorizeGated proves the resume verb runs through the same
// authorize-commenter gate as retry/cancel: an unauthorized commenter's resume is
// ignored and never re-enqueues the failing leg.
func TestPollOnceResumeIsAuthorizeGated(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	engine, _ := resumeFixture(t, store)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	client := &fakeGitHub{
		permissions: map[string]string{"mallory": "read"},
		pulls:       []github.PullRequest{{Number: 7, Title: "Task 7", State: "open", HeadRef: "task-7", BaseRef: "main", HeadSHA: "abc"}},
		comments: map[int64][]github.IssueComment{
			7: {{ID: 707, Body: "/gitmoot resume coord retry", Author: "mallory"}},
		},
	}
	d := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: engine}
	pull := github.PullRequest{Number: 7, HeadRef: "task-7", HeadSHA: "abc"}
	comment := github.IssueComment{ID: 707, Body: "/gitmoot resume coord retry", Author: "mallory"}
	if err := d.handleComment(ctx, pull, comment); err != nil {
		t.Fatalf("handleComment returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "coord/delegation/api/resume"); err == nil {
		t.Fatal("an unauthorized resume must not re-enqueue the failing leg")
	}
	if _, err := store.GetJob(ctx, "coord/continuation"); err == nil {
		t.Fatal("an unauthorized resume must not enqueue a continuation")
	}
	found := false
	for _, p := range client.posted {
		if strings.Contains(p.body, "ignored comment 707") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an authorize-rejection ack, got %+v", client.posted)
	}
}

func TestHandleResumeRequiresWorkflowEngine(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	client := &fakeGitHub{}
	d := Daemon{Repo: github.Repository{Owner: "jerryfane", Name: "gitmoot"}, Store: store, GitHub: client}
	pull := github.PullRequest{Number: 7, HeadRef: "task-7"}
	if err := d.handleResumeCommand(ctx, pull, Command{Action: "resume", JobID: "coord", Decision: "retry"}); err != nil {
		t.Fatalf("handleResumeCommand returned error: %v", err)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "workflow engine is not configured") {
		t.Fatalf("expected a not-configured ack, got %+v", client.posted)
	}
}

// askGateFixture seeds a HEALTHY coordinator whose result carries
// human_questions[] (#445) and drives it to the awaiting_human ask pause. The
// coordinator id is "coord" with PR #7.
func askGateFixture(t *testing.T, store *db.Store) (*workflow.Engine, github.PullRequest) {
	t.Helper()
	ctx := context.Background()
	repo := "jerryfane/gitmoot"
	seedResumeAgent(t, store, "coord", []string{"ask"}, repo)

	engine := &workflow.Engine{
		Store: store,
		JobID: func(request workflow.JobRequest) string { return request.ID },
	}

	coordPayload := workflow.JobPayload{
		Repo:        repo,
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Parent",
		Sender:      "coord",
		Result: &workflow.AgentResult{
			Decision: "approved",
			Summary:  "need a decision",
			HumanQuestions: []workflow.HumanQuestion{
				{ID: "q1", Prompt: "Target v2 or v3 API?", Choices: []string{"v2", "v3"}},
			},
		},
	}
	createResumeJob(t, store, db.Job{ID: "coord", Agent: "coord", Type: "ask"}, coordPayload)
	if err := engine.AdvanceJob(ctx, "coord"); err == nil {
		t.Fatal("AdvanceJob(coord) should return AwaitingHumanError for an ask-gate result")
	}

	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskAwaitingHuman) {
		t.Fatalf("task state = %q, want awaiting_human", task.State)
	}
	return engine, github.PullRequest{Number: 7, HeadRef: "task-7", HeadSHA: "abc"}
}

func TestHandleResumeAnswerEnqueuesContinuation(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	engine, pull := askGateFixture(t, store)
	client := &fakeGitHub{}
	d := Daemon{Repo: github.Repository{Owner: "jerryfane", Name: "gitmoot"}, Store: store, GitHub: client, Workflow: engine}

	if err := d.handleResumeCommand(ctx, pull, Command{Action: "resume", JobID: "coord", Decision: "answer", Instructions: "q1: v3"}); err != nil {
		t.Fatalf("handleResumeCommand(answer) returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "coord/continuation")
	if err != nil {
		t.Fatalf("answer must enqueue the coordinator continuation: %v", err)
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("unmarshal continuation payload: %v", err)
	}
	if !strings.Contains(payload.HumanAnswer, "v3") {
		t.Fatalf("continuation must carry the human answer: %+v", payload.HumanAnswer)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "resumed job `coord` with `answer`") {
		t.Fatalf("ack = %+v", client.posted)
	}
}

// TestHandleResumeAnswerRejectedOnFailureRound proves the verb/round-kind gate is
// enforced through the daemon: `answer` on a failure escalation round is rejected
// with a clear ack and no continuation is enqueued.
func TestHandleResumeAnswerRejectedOnFailureRound(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	engine, pull := resumeFixture(t, store)
	client := &fakeGitHub{}
	d := Daemon{Repo: github.Repository{Owner: "jerryfane", Name: "gitmoot"}, Store: store, GitHub: client, Workflow: engine}

	if err := d.handleResumeCommand(ctx, pull, Command{Action: "resume", JobID: "coord", Decision: "answer", Instructions: "q1: v3"}); err != nil {
		t.Fatalf("handleResumeCommand(answer) returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "coord/continuation"); err == nil {
		t.Fatal("answer on a failure round must not enqueue a continuation")
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "could not resume") {
		t.Fatalf("expected a verb-mismatch ack, got %+v", client.posted)
	}
}
