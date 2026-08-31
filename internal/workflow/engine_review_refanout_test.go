package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestReviewingTaskResumeWorkRearmsNextHeadFanout(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)

	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-one")); err != nil {
		t.Fatalf("initial HandlePullRequestOpened: %v", err)
	}
	task, err := store.GetTask(ctx, "task-1678")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	// gitmoot task resume-work moves a settled review task back to implementing.
	// A new head without a completed prior verdict still receives a full review.
	task.State = string(TaskImplementing)
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatalf("UpsertTask implementing: %v", err)
	}

	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
		t.Fatalf("re-armed HandlePullRequestOpened: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("review jobs = %d, want 2 after explicit re-arm", len(jobs))
	}
	second := mustJob(t, store, "review-audit-task-1678-review-2")
	payload, err := unmarshalPayload(second.Payload)
	if err != nil {
		t.Fatalf("unmarshal second review payload: %v", err)
	}
	if payload.HeadSHA != "head-two" {
		t.Fatalf("second review head = %q, want head-two", payload.HeadSHA)
	}
	assertTaskState(t, store, "task-1678", TaskReviewing)
}

func TestFollowUpReviewScopesFindingsAndFilesFromReviewerHead(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	insertCompletedJob(t, store, db.Job{ID: "initial-implement", Agent: "lead", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		TaskID:      "task-1678",
		LeadAgent:   "lead",
	})
	// #1712 (landed on main after this branch opened) made the fix DISPATCH an
	// explicit per-PR opt-in: report-only is the default, because the requester
	// already owns the context and worktree. This test is about what the fix job
	// CARRIES, so it enables the policy as a production precondition — exactly the
	// row AdvanceJob reads — and still drives the real dispatch path afterwards
	// (PullRequestAutoFixPolicyFor -> dispatchFix -> autoFixOwner ->
	// fixBranchLockOwner -> FixWorktreeAllocator -> enqueue).
	enableAutoFix(t, store, 1678)
	insertPriorReviewResult(t, store, "prior-review", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Fix the boundary check.",
		Findings: []json.RawMessage{
			json.RawMessage(`{"id":"F-1","file":"internal/boundary.go","summary":"Fix the boundary check."}`),
		},
	})
	if err := engine.AdvanceJob(ctx, "prior-review"); err != nil {
		t.Fatalf("AdvanceJob prior review: %v", err)
	}
	fix := mustJob(t, store, "implement-lead-task-1678-review-1")
	fixPayload, err := unmarshalPayload(fix.Payload)
	if err != nil {
		t.Fatalf("unmarshal fix payload: %v", err)
	}
	if !strings.Contains(fixPayload.Instructions, `"id":"F-1"`) {
		t.Fatalf("fix instructions lost the named finding: %q", fixPayload.Instructions)
	}
	var gotPrevious, gotCurrent string
	engine.ReviewChangedFiles = func(_ context.Context, _ string, _ int, previousHead, currentHead string) ([]string, error) {
		gotPrevious, gotCurrent = previousHead, currentHead
		return []string{"internal/z.go", "internal/boundary.go", "internal/boundary.go"}, nil
	}

	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	if gotPrevious != "head-one" || gotCurrent != "head-two" {
		t.Fatalf("changed-files diff = %s..%s, want reviewer head-one..head-two", gotPrevious, gotCurrent)
	}
	job := mustJob(t, store, "review-audit-task-1678-review-2")
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.ReviewScope == nil {
		t.Fatal("follow-up review has no persisted scope")
	}
	if payload.ReviewScope.PreviousHeadSHA != "head-one" {
		t.Fatalf("scope previous head = %q, want head-one", payload.ReviewScope.PreviousHeadSHA)
	}
	if len(payload.ReviewScope.Findings) != 1 || !strings.Contains(payload.ReviewScope.Findings[0], `"id":"F-1"`) {
		t.Fatalf("scope findings = %#v, want named F-1", payload.ReviewScope.Findings)
	}
	wantFiles := []string{"internal/boundary.go", "internal/z.go"}
	if strings.Join(payload.ReviewScope.ChangedFiles, ",") != strings.Join(wantFiles, ",") {
		t.Fatalf("scope changed files = %#v, want %#v", payload.ReviewScope.ChangedFiles, wantFiles)
	}
	if !strings.Contains(payload.Instructions, "Do not re-review the full PR-to-base diff") ||
		!strings.Contains(payload.Instructions, "internal/boundary.go") {
		t.Fatalf("scoped review instructions do not constrain the follow-up: %q", payload.Instructions)
	}
}

func TestFollowUpReviewEmptyScopeApprovesWithoutFullReread(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	insertPriorReviewResult(t, store, "prior-approval", "head-one", AgentResult{
		Decision: "approved",
	})
	engine.ReviewChangedFiles = func(context.Context, string, int, string, string) ([]string, error) {
		return nil, nil
	}
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge-scoped"}}
	engine.MergeGate = gate

	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	job := mustJob(t, store, "review-audit-task-1678-review-2")
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.ReviewScope == nil || len(payload.ReviewScope.Findings) != 0 || len(payload.ReviewScope.ChangedFiles) != 0 {
		t.Fatalf("empty follow-up scope = %+v", payload.ReviewScope)
	}
	if !strings.Contains(payload.Instructions, "The scope is empty: approve without rereading the full diff.") {
		t.Fatalf("empty scope did not select cheap approval instructions: %q", payload.Instructions)
	}
	completeQueuedReview(t, store, job, payload, AgentResult{
		Decision: "approved",
		Summary:  "No scoped work remains.",
		TestsRun: []string{"scoped review"},
	})
	if err := engine.AdvanceJob(ctx, job.ID); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}
	if len(gate.requests) != 1 || gate.requests[0].HeadSHA != "head-two" {
		t.Fatalf("merge gate requests = %+v, want one exact-head scoped verdict", gate.requests)
	}
	assertTaskState(t, store, "task-1678", TaskMerged)
}

func TestFollowUpReviewFailsClosedWithoutChangedFilesResolver(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	insertPriorReviewResult(t, store, "prior-review", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Fix the boundary check.",
	})

	err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two"))
	if err == nil || !strings.Contains(err.Error(), "requires a changed-files resolver") {
		t.Fatalf("HandlePullRequestOpened error = %v, want fail-closed scope resolver error", err)
	}
	jobs, listErr := store.ListJobs(ctx)
	if listErr != nil {
		t.Fatalf("ListJobs: %v", listErr)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs after missing scope resolver = %d, want prior review only", len(jobs))
	}
}

func insertPriorReviewResult(t *testing.T, store *db.Store, id string, head string, result AgentResult) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{ID: id, Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		HeadSHA:     head,
		TaskID:      "task-1678",
		TaskTitle:   "Bound native review fanout",
		LeadAgent:   "lead",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
		Result:      &result,
	})
}

func completeQueuedReview(t *testing.T, store *db.Store, job db.Job, payload JobPayload, result AgentResult) {
	t.Helper()
	payload.Result = &result
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := store.UpdateJobPayload(context.Background(), job.ID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}
	if err := store.UpdateJobState(context.Background(), job.ID, string(JobSucceeded)); err != nil {
		t.Fatalf("UpdateJobState: %v", err)
	}
}

func reviewRefanoutFixture(t *testing.T) (*db.Store, Engine) {
	t.Helper()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequireWorkflowPolicy = func(string) RequireWorkflowPolicy {
		return RequireWorkflowPolicy{Enabled: true, Mode: "strict"}
	}
	return store, engine
}

func reviewRefanoutEvent(head string) PullRequestEvent {
	return PullRequestEvent{
		Repo:              "gitmoot/gitmoot",
		Branch:            "task-1678",
		PullRequest:       1678,
		HeadSHA:           head,
		GoalID:            "goal-fanout",
		TaskID:            "task-1678",
		TaskTitle:         "Bound native review fanout",
		LeadAgent:         "lead",
		Sender:            "github",
		RequiredReviewers: []string{"audit"},
	}
}
