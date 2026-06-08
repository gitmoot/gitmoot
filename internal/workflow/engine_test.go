package workflow

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

func TestEngineHandlePullRequestOpenedDispatchesReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		GoalID:            "goal-1",
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		Sender:            "lead",
		RequiredReviewers: []string{"audit"},
	})

	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	job := mustJob(t, store, "review-audit-task-7-review-1")
	if job.Agent != "audit" || job.Type != "review" || job.State != string(JobQueued) {
		t.Fatalf("review job = %+v", job)
	}
	if !strings.Contains(job.Payload, `"lead_agent":"lead"`) {
		t.Fatalf("payload missing lead agent: %s", job.Payload)
	}
	pr, err := store.GetPullRequest(ctx, "jerryfane/gitmoot", 7)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.HeadBranch != "task-7" || pr.HeadSHA != "" {
		t.Fatalf("pull request baseline = %+v", pr)
	}
}

func TestEngineHandlePullRequestOpenedBlocksOnBranchLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-7", Owner: "other"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	})

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "branch lock") {
		t.Fatalf("error = %v, want branch lock BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineHandlePullRequestOpenedBlocksWhenLeadLacksCapability(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	})

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "lacks") {
		t.Fatalf("error = %v, want capability BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineHandlePullRequestOpenedRunsMergeGateWithoutReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Reason: "ci is pending"}}
	engine.MergeGate = gate

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	})

	var blocked BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "ci is pending" {
		t.Fatalf("error = %v, want merge gate BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
	if len(gate.requests) != 1 || gate.requests[0].Reviewer != "" || gate.requests[0].PullRequest != 7 || !gate.requests[0].ReviewOptional {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestEngineHandlePullRequestOpenedDoesNotOverwriteNoReviewerMerge(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{
		decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"},
		onEvaluate: func(request MergeRequest) {
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName:   request.Repo,
				Number:         int64(request.PullRequest),
				URL:            "https://github.com/jerryfane/gitmoot/pull/7",
				HeadBranch:     request.Branch,
				BaseBranch:     "main",
				HeadSHA:        request.HeadSHA,
				MergeCommitSHA: "merge123",
				State:          "merged",
			}); err != nil {
				t.Fatalf("UpsertPullRequest returned error: %v", err)
			}
		},
	}
	engine.MergeGate = gate

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "head123",
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	})

	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskMerged)
	pr, err := store.GetPullRequest(ctx, "jerryfane/gitmoot", 7)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.State != "merged" || pr.MergeCommitSHA != "merge123" {
		t.Fatalf("stored pull request = %+v", pr)
	}
}

func TestEngineAdvanceImplementDispatchesReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"audit"}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		GoalID:      "goal-1",
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "implemented", Summary: "opened PR"},
	})

	err := engine.AdvanceJob(ctx, "implement-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	mustJob(t, store, "review-audit-task-7-review-1")
}

func TestEngineAdvanceImplementDefaultsLeadAgent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"audit"}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Result:      &AgentResult{Decision: "implemented", Summary: "opened PR"},
	})

	err := engine.AdvanceJob(ctx, "implement-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	mustJob(t, store, "review-audit-task-7-review-1")
}

func TestEngineAdvanceImplementSkipsPullRequestFlowWhenNoPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-7",
		GoalID:    "goal-1",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		LeadAgent: "lead",
		Result:    &AgentResult{Decision: "implemented", Summary: "done locally"},
	})

	err := engine.AdvanceJob(ctx, "implement-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "implement-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "advance_skipped_no_pr" {
			return
		}
	}
	t.Fatalf("events = %+v, want advance_skipped_no_pr", events)
}

func TestEngineRunJobAdvancesCompletedResult(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"audit"}
	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "lead"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"opened PR","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
	}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:          "implement-job",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	result, err := engine.RunJob(ctx, "implement-job", agent, adapter)

	if err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}
	if result.Decision != "implemented" {
		t.Fatalf("result = %+v", result)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	mustJob(t, store, "review-audit-task-7-review-1")
}

func TestEngineRunJobPreflightsPolicyBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-7", Owner: "other"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	engine := testEngine(store)
	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "lead"}
	delivered := false
	adapter := &fakeDelivery{
		outputs: []string{
			`{"gitmoot_result":{"decision":"implemented","summary":"opened PR","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":[]}}`,
		},
		onDeliver: func() {
			delivered = true
		},
	}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:          "implement-job",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	_, err := engine.RunJob(ctx, "implement-job", agent, adapter)

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "branch lock") {
		t.Fatalf("error = %v, want branch lock BlockedError", err)
	}
	if delivered {
		t.Fatal("adapter delivered job before branch-lock preflight")
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineAdvanceReviewChangesRequestedDispatchesFix(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	job := mustJob(t, store, "implement-lead-task-7")
	if !strings.Contains(job.Payload, "fix edge case") {
		t.Fatalf("fix job payload = %s", job.Payload)
	}
}

func TestEngineAdvanceReviewChangesRequestedReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("first AdvanceJob returned error: %v", err)
	}
	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("second AdvanceJob returned error: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	implementJobs := 0
	for _, job := range jobs {
		if job.ID == "implement-lead-task-7" {
			implementJobs++
		}
	}
	if implementJobs != 1 {
		t.Fatalf("implement job count = %d, want 1", implementJobs)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
}

func TestEngineAdvanceReviewChangesRequestedUsesBranchLockLead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "manual-review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix manual review"},
	})

	err := engine.AdvanceJob(ctx, "manual-review-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	job := mustJob(t, store, "implement-lead-task-7")
	if !strings.Contains(job.Payload, "fix manual review") {
		t.Fatalf("fix job payload = %s", job.Payload)
	}
}

func TestEngineAdvanceReviewApprovalRunsMergeGate(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if len(gate.requests) != 1 || gate.requests[0].Reviewer != "audit" || gate.requests[0].PullRequest != 7 || gate.requests[0].ReviewOptional {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestEngineAdvanceReviewApprovalWaitsForAllRequiredReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	reviewers := []string{"audit", "security"}
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		Result:      &AgentResult{Decision: "approved", Summary: "audit ready"},
	})

	if err := engine.AdvanceJob(ctx, "audit-review"); err != nil {
		t.Fatalf("first AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran before all approvals: %+v", gate.requests)
	}

	insertCompletedJob(t, store, db.Job{
		ID:    "security-review",
		Agent: "security",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		Result:      &AgentResult{Decision: "approved", Summary: "security ready"},
	})

	if err := engine.AdvanceJob(ctx, "security-review"); err != nil {
		t.Fatalf("second AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if len(gate.requests) != 1 || gate.requests[0].Reviewer != "security" {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestEngineAdvanceReviewApprovalPreservesChangesRequested(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	reviewers := []string{"audit", "security"}
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Reviewers:   reviewers,
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})
	if err := engine.AdvanceJob(ctx, "audit-review"); err != nil {
		t.Fatalf("changes requested AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)

	insertCompletedJob(t, store, db.Job{
		ID:    "security-review",
		Agent: "security",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Reviewers:   reviewers,
		Result:      &AgentResult{Decision: "approved", Summary: "security ready"},
	})
	if err := engine.AdvanceJob(ctx, "security-review"); err != nil {
		t.Fatalf("approval AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran despite requested changes: %+v", gate.requests)
	}
}

func TestEngineHandlePullRequestOpenedCreatesNewReviewRoundForUpdates(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	event := PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		RequiredReviewers: []string{"audit"},
	}

	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("first HandlePullRequestOpened returned error: %v", err)
	}
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("second HandlePullRequestOpened returned error: %v", err)
	}

	mustJob(t, store, "review-audit-task-7-review-1")
	mustJob(t, store, "review-audit-task-7-review-2")
}

func TestEngineHandlePullRequestOpenedPreflightsReviewersBeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		RequiredReviewers: []string{"audit", "missing"},
	})

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "not subscribed") {
		t.Fatalf("error = %v, want missing reviewer BlockedError", err)
	}
	jobs, listErr := store.ListJobs(ctx)
	if listErr != nil {
		t.Fatalf("ListJobs returned error: %v", listErr)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("review job was enqueued before reviewer preflight completed: %+v", job)
		}
	}
}

func TestEngineAdvanceReviewApprovalIgnoresEarlierReviewRounds(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	reviewers := []string{"audit", "security"}
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-1",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "old approval"},
	})
	insertCompletedJob(t, store, db.Job{
		ID:    "security-review-round-2",
		Agent: "security",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "approved", Summary: "security ready"},
	})

	if err := engine.AdvanceJob(ctx, "security-review-round-2"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran with stale approval: %+v", gate.requests)
	}

	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-2",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "approved", Summary: "audit ready"},
	})
	if err := engine.AdvanceJob(ctx, "audit-review-round-2"); err != nil {
		t.Fatalf("second AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if len(gate.requests) != 1 || gate.requests[0].Reviewer != "audit" {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestEngineAdvanceReviewApprovalIgnoresStaleRoundWhenNewerRoundExists(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	if err := store.UpsertTask(ctx, db.Task{
		ID:     "task-7",
		GoalID: "goal-1",
		Title:  "Workflow Engine",
		State:  string(TaskReviewing),
		Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-1",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "old approval"},
	})
	encoded, err := marshalPayload(JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-2",
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      "audit-review-round-2",
		Agent:   "audit",
		Type:    "review",
		State:   string(JobQueued),
		Payload: encoded,
	}, db.JobEvent{Kind: string(JobQueued), Message: "newer round queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	if err := engine.AdvanceJob(ctx, "audit-review-round-1"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran with stale approval: %+v", gate.requests)
	}
}

func TestEngineAdvanceStaleReviewDoesNotDispatchNextAgents(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	seedAgent(t, store, "planner", []string{"ask"}, "jerryfane/gitmoot")
	if err := store.UpsertTask(ctx, db.Task{
		ID:     "task-7",
		GoalID: "goal-1",
		Title:  "Workflow Engine",
		State:  string(TaskReviewing),
		Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-1",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "old approval", NextAgents: []string{"planner"}},
	})
	encoded, err := marshalPayload(JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-2",
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      "audit-review-round-2",
		Agent:   "audit",
		Type:    "review",
		State:   string(JobQueued),
		Payload: encoded,
	}, db.JobEvent{Kind: string(JobQueued), Message: "newer round queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	if err := engine.AdvanceJob(ctx, "audit-review-round-1"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "ask" {
			t.Fatalf("stale review dispatched next-agent job: %+v", job)
		}
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
}

func TestEngineAdvanceStaleReviewDoesNotRegressReadyState(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-1",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "old approval"},
	})
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-2",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "approved", Summary: "new approval"},
	})

	if err := engine.AdvanceJob(ctx, "audit-review-round-2"); err != nil {
		t.Fatalf("newer AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if err := engine.AdvanceJob(ctx, "audit-review-round-1"); err != nil {
		t.Fatalf("stale AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
}

func TestEngineAdvanceReviewApprovalIgnoresOtherRepoTaskID(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	reviewers := []string{"audit", "security"}
	insertCompletedJob(t, store, db.Job{
		ID:    "other-repo-audit-review",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "other/repo",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "other repo ready"},
	})
	insertCompletedJob(t, store, db.Job{
		ID:    "security-review",
		Agent: "security",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "security ready"},
	})

	if err := engine.AdvanceJob(ctx, "security-review"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran with other repo approval: %+v", gate.requests)
	}
}

func TestEngineAdvanceReviewApprovalBlocksOnMergeGateRejection(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Reason: "checks are pending"}}
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	var blocked BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "checks are pending" {
		t.Fatalf("error = %v, want merge gate BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineAdvanceBlocksOnAgentBlockedResult(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-7",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		Result:    &AgentResult{Decision: "blocked", Summary: "needs credentials"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	var blocked BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "needs credentials" {
		t.Fatalf("error = %v, want agent BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineAdvanceDispatchesNextAgents(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "planner", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-7",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		Result:    &AgentResult{Decision: "approved", Summary: "ask planner", NextAgents: []string{"planner"}},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	mustJob(t, store, "ask-planner-task-7")
}

func TestEngineAdvanceBlocksWhenNextAgentScopeRejected(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "planner", []string{"ask"}, "other/repo")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-7",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		Result:    &AgentResult{Decision: "approved", Summary: "ask planner", NextAgents: []string{"planner"}},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "not allowed") {
		t.Fatalf("error = %v, want scope BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineAdvancePreflightsNextAgentsBeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "planner", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-7",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		Result:    &AgentResult{Decision: "approved", Summary: "ask planner", NextAgents: []string{"planner", "missing"}},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "not subscribed") {
		t.Fatalf("error = %v, want missing next-agent BlockedError", err)
	}
	jobs, listErr := store.ListJobs(ctx)
	if listErr != nil {
		t.Fatalf("ListJobs returned error: %v", listErr)
	}
	for _, job := range jobs {
		if job.Type == "ask" {
			t.Fatalf("ask job was enqueued before next-agent preflight completed: %+v", job)
		}
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineSetTaskStatePreservesExistingMetadata(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:     "task-7",
		GoalID: "goal-1",
		Title:  "Workflow Engine",
		State:  string(TaskPlanned),
		Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	engine := testEngine(store)

	if err := engine.setTaskState(ctx, taskRef{ID: "task-7"}, TaskReviewing); err != nil {
		t.Fatalf("setTaskState returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.GoalID != "goal-1" || task.Title != "Workflow Engine" || task.Branch != "task-7" || task.State != string(TaskReviewing) {
		t.Fatalf("task metadata was not preserved: %+v", task)
	}
}

func openEngineStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

func testEngine(store *db.Store) Engine {
	return Engine{
		Store: store,
		JobID: func(request JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
	}
}

func seedAgent(t *testing.T, store *db.Store, name string, capabilities []string, repo string) {
	t.Helper()
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           name,
		Role:           "agent",
		Runtime:        runtime.ShellRuntime,
		RuntimeRef:     "printf ok",
		RepoScope:      repo,
		Capabilities:   capabilities,
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
}

func insertCompletedJob(t *testing.T, store *db.Store, job db.Job, payload JobPayload) {
	t.Helper()
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	job.State = string(JobSucceeded)
	job.Payload = encoded
	if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{Kind: string(JobSucceeded), Message: "done"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
}

func assertTaskState(t *testing.T, store *db.Store, taskID string, want TaskState) {
	t.Helper()
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(want) {
		t.Fatalf("task state = %q, want %q", task.State, want)
	}
}

func mustJob(t *testing.T, store *db.Store, jobID string) db.Job {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", jobID, err)
	}
	return job
}

type fakeMergeGate struct {
	decision   MergeDecision
	onEvaluate func(MergeRequest)
	requests   []MergeRequest
}

func (f *fakeMergeGate) Evaluate(_ context.Context, request MergeRequest) (MergeDecision, error) {
	f.requests = append(f.requests, request)
	if f.onEvaluate != nil {
		f.onEvaluate(request)
	}
	return f.decision, nil
}
