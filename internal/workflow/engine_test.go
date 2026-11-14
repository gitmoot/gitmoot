package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/reviewseverity"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

func TestEngineHandlePullRequestOpenedDispatchesReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequireWorkflowPolicy = func(string) RequireWorkflowPolicy { return RequireWorkflowPolicy{Enabled: true, Mode: "strict"} }

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "gitmoot/gitmoot",
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
	pr, err := store.GetPullRequest(ctx, "gitmoot/gitmoot", 7)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.HeadBranch != "task-7" || pr.HeadSHA != "" {
		t.Fatalf("pull request baseline = %+v", pr)
	}
}

func TestEngineHandlePullRequestOpenedBlocksWhenLeadLacksCapability(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "gitmoot/gitmoot",
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
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Reason: PlainReason("ci is pending")}}
	engine.MergeGate = gate

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "gitmoot/gitmoot",
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
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{
		decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"},
		onEvaluate: func(request MergeRequest) {
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName:   request.Repo,
				Number:         int64(request.PullRequest),
				URL:            "https://github.com/gitmoot/gitmoot/pull/7",
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
		Repo:        "gitmoot/gitmoot",
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
	pr, err := store.GetPullRequest(ctx, "gitmoot/gitmoot", 7)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.State != "merged" || pr.MergeCommitSHA != "merge123" {
		t.Fatalf("stored pull request = %+v", pr)
	}
}

func TestEngineHandlePullRequestOpenedSkipsReviewFanout(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "gitmoot/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		HeadSHA:           "head123",
		GoalID:            "goal-1",
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		Sender:            "lead",
		RequiredReviewers: []string{"audit"},
		SkipReviewFanout:  true,
	})

	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskPullRequestOpen)
	// Zero review jobs enqueued despite a required reviewer being present.
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("expected no review jobs, found %+v", job)
		}
	}
	// The native merge gate does not run; an external council gate owns merge
	// authority for skip-native-review-fanout branches.
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
	// Baseline still recorded so the PR advances.
	pr, err := store.GetPullRequest(ctx, "gitmoot/gitmoot", 7)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.HeadBranch != "task-7" || pr.HeadSHA != "head123" {
		t.Fatalf("pull request baseline = %+v", pr)
	}
}

func TestEngineAdvanceImplementDispatchesReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"audit"}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
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
	events, err := store.ListJobEvents(ctx, "implement-job")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == "advance_skipped_no_pr" {
			t.Fatalf("implemented job with PR recorded no-PR event: %+v", events)
		}
	}
}

func TestEngineAdvanceImplementDefaultsLeadAgent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"audit"}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
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
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:      "gitmoot/gitmoot",
		Branch:    "task-7",
		GoalID:    "goal-1",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		LeadAgent: "lead",
		Result:    &AgentResult{Decision: "implemented", Summary: "done locally"},
	})

	for i := 0; i < 2; i++ {
		if err := engine.AdvanceJob(ctx, "implement-job"); err != nil {
			t.Fatalf("AdvanceJob call %d returned error: %v", i+1, err)
		}
	}
	events, err := store.ListJobEvents(ctx, "implement-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "advance_skipped_no_pr" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("advance_skipped_no_pr count = %d, want 1; events=%+v", count, events)
	}
}

func TestEngineAdvanceImplementTerminalDecisionsRecordNoPROnce(t *testing.T) {
	for _, decision := range []string{"approved", "changes_requested", "blocked", "failed"} {
		t.Run(decision, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
			engine := testEngine(store)
			insertCompletedJob(t, store, db.Job{
				ID:    "implement-job",
				Agent: "lead",
				Type:  "implement",
			}, JobPayload{
				Repo:      "gitmoot/gitmoot",
				Branch:    "task-817",
				TaskID:    "task-817",
				TaskTitle: "Terminal implement decision",
				Result:    &AgentResult{Decision: decision, Summary: "terminal without PR"},
			})
			if decision == "blocked" || decision == "failed" {
				if err := store.UpdateJobState(ctx, "implement-job", string(stateForDecision(decision))); err != nil {
					t.Fatalf("UpdateJobState: %v", err)
				}
			}

			for i := 0; i < 2; i++ {
				err := engine.AdvanceJob(ctx, "implement-job")
				if decision == "blocked" || decision == "failed" {
					var blocked BlockedError
					if !errors.As(err, &blocked) {
						t.Fatalf("AdvanceJob call %d error = %v, want BlockedError", i+1, err)
					}
				} else if err != nil {
					t.Fatalf("AdvanceJob call %d: %v", i+1, err)
				}
			}

			events, err := store.ListJobEvents(ctx, "implement-job")
			if err != nil {
				t.Fatalf("ListJobEvents: %v", err)
			}
			count := 0
			for _, event := range events {
				if event.Kind != "advance_skipped_no_pr" {
					continue
				}
				count++
				if !strings.Contains(event.Message, decision) {
					t.Fatalf("event message %q does not identify decision %q", event.Message, decision)
				}
			}
			if count != 1 {
				t.Fatalf("advance_skipped_no_pr count = %d, want 1; events=%+v", count, events)
			}
		})
	}
}

func TestEngineAdvanceImplementNoPREventConcurrent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:   "gitmoot/gitmoot",
		Branch: "task-817",
		Result: &AgentResult{Decision: "approved", Summary: "terminal without PR"},
	})

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- engine.AdvanceJob(ctx, "implement-job")
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent AdvanceJob: %v", err)
		}
	}

	events, err := store.ListJobEvents(ctx, "implement-job")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "advance_skipped_no_pr" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("advance_skipped_no_pr count = %d, want 1; events=%+v", count, events)
	}
}

func TestEngineAdvanceImplementWithoutPullRequestSkips(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:      "gitmoot/gitmoot",
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
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"audit"}
	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "lead"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"opened PR","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}`,
	}}
	if _, err := (Mailbox{store: store}).Enqueue(ctx, JobRequest{
		ID:          "implement-job",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "gitmoot/gitmoot",
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

// A benign post-success advance condition (here: a merge-gate block on the
// freshly-"opened" PR) makes AdvanceJob return after the agent delivery + job
// already succeeded terminally. RunJob must wrap that error in AdvanceError
// (so callers can recover the persisted result) while still returning the
// result; the job stays JobSucceeded.
func TestEngineRunJobWrapsPostSuccessAdvanceError(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	// A pending (not-ready) merge gate blocks the no-reviewers tail -> AdvanceJob
	// returns a BlockedError AFTER the result is stored.
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Reason: PlainReason("ci is pending")}}
	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "lead"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"opened PR","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}`,
	}}
	if _, err := (Mailbox{store: store}).Enqueue(ctx, JobRequest{
		ID:          "implement-job",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "head123",
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	result, err := engine.RunJob(ctx, "implement-job", agent, adapter)

	var advErr AdvanceError
	if !errors.As(err, &advErr) {
		t.Fatalf("RunJob error = %v, want AdvanceError", err)
	}
	// The wrapped cause is still reachable; the result is in hand.
	var blocked BlockedError
	if !errors.As(advErr, &blocked) || blocked.Reason != "ci is pending" {
		t.Fatalf("AdvanceError did not wrap the merge-gate BlockedError: %v", advErr)
	}
	if result.Decision != "implemented" || result.Summary != "opened PR" {
		t.Fatalf("result = %+v, want the delivered implemented result", result)
	}
	// The job is terminally succeeded despite the post-success advance error.
	job := mustJob(t, store, "implement-job")
	if job.State != string(JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
}

// A delivery/run failure (here: a non-existent runtime command) errors BEFORE
// AdvanceJob, so RunJob must return that error raw, NOT wrapped in AdvanceError,
// and the job must not be JobSucceeded.
func TestEngineRunJobDeliveryFailureStaysRaw(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "lead"}
	// A delivery error before the result is stored.
	adapter := &fakeDelivery{err: errors.New("runtime exploded")}
	if _, err := (Mailbox{store: store}).Enqueue(ctx, JobRequest{
		ID:        "implement-job",
		Agent:     "lead",
		Action:    "implement",
		Repo:      "gitmoot/gitmoot",
		Branch:    "task-7",
		TaskID:    "task-7",
		LeadAgent: "lead",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	_, err := engine.RunJob(ctx, "implement-job", agent, adapter)

	if err == nil {
		t.Fatal("RunJob returned nil error, want delivery failure")
	}
	var advErr AdvanceError
	if errors.As(err, &advErr) {
		t.Fatalf("delivery failure wrapped as AdvanceError: %v", err)
	}
	job := mustJob(t, store, "implement-job")
	if job.State == string(JobSucceeded) {
		t.Fatalf("job state = %q, want non-succeeded after delivery failure", job.State)
	}
}

func TestEngineAdvanceReviewSubthresholdApprovesWithNotes(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.ReviewBlockingSeverity = func(repo string) string {
		if repo != "mobile/app" {
			t.Fatalf("blocking severity repo = %q, want mobile/app", repo)
		}
		return reviewseverity.P1
	}
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	finding := json.RawMessage(`{"severity":"P2","summary":"medium polish"}`)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-notes",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "mobile/app",
		Branch:      "task-8",
		PullRequest: 8,
		TaskID:      "task-8",
		TaskTitle:   "Mobile App",
		LeadAgent:   "lead",
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: reviewseverity.P2,
			Summary:  "non-blocking polish",
			Findings: []json.RawMessage{finding},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-notes"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	assertTaskState(t, store, "task-8", TaskReadyToMerge)
	if _, err := store.GetJob(ctx, "implement-lead-task-8"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("sub-threshold review fix job error = %v, want sql.ErrNoRows", err)
	}
	if len(gate.requests) != 1 || gate.requests[0].ReviewBlockingSeverity != reviewseverity.P1 {
		t.Fatalf("merge gate requests = %+v, want threshold P1", gate.requests)
	}
	if err := engine.AdvanceJob(ctx, "review-notes"); err != nil {
		t.Fatalf("replayed AdvanceJob returned error: %v", err)
	}
	if len(gate.requests) != 1 {
		t.Fatalf("replayed approved-with-notes review evaluated merge gate %d times, want 1", len(gate.requests))
	}
	stored := mustJob(t, store, "review-notes")
	payload, err := ParseJobPayload(stored.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.Result == nil || payload.Result.Decision != "changes_requested" || payload.Result.Severity != reviewseverity.P2 ||
		len(payload.Result.Findings) != 1 || string(payload.Result.Findings[0]) != string(finding) {
		t.Fatalf("stored result was rewritten or findings lost: %+v", payload.Result)
	}
	if got := countJobEvents(t, store, "review-notes", ReviewApprovedWithNotesEventKind); got != 1 {
		t.Fatalf("%s events = %d, want 1", ReviewApprovedWithNotesEventKind, got)
	}
}

func TestAllRequiredReviewersApprovedCountsSubthresholdReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.ReviewBlockingSeverity = func(string) string { return reviewseverity.P1 }
	insertCompletedJob(t, store, db.Job{ID: "review-audit", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "mobile/app",
		PullRequest: 8,
		TaskID:      "task-8",
		ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: reviewseverity.P2,
			Summary:  "non-blocking audit note",
		},
	})
	payload := JobPayload{
		Repo:        "mobile/app",
		PullRequest: 8,
		TaskID:      "task-8",
		ReviewRound: "review-1",
		Reviewers:   []string{"audit", "security"},
	}

	approved, err := engine.allRequiredReviewersApproved(ctx, "security", payload)
	if err != nil {
		t.Fatalf("allRequiredReviewersApproved returned error: %v", err)
	}
	if !approved {
		t.Fatal("sub-threshold audit review must count toward required-reviewer approval")
	}
}

// A review recorded with no ReviewRound shares the round of the current review
// (sameReviewRound treats two empty rounds as one round), so a sub-threshold
// changes_requested from an earlier head still folds to approved and satisfies
// its reviewer slot.
func TestAllRequiredReviewersApprovedFoldsEmptyRoundSubthresholdReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.ReviewBlockingSeverity = func(string) string { return reviewseverity.P1 }
	insertCompletedJob(t, store, db.Job{ID: "review-stale", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "mobile/app",
		PullRequest: 8,
		TaskID:      "task-8",
		HeadSHA:     "oldhead",
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: reviewseverity.P2,
			Summary:  "non-blocking note",
		},
	})
	payload := JobPayload{
		Repo:        "mobile/app",
		PullRequest: 8,
		TaskID:      "task-8",
		HeadSHA:     "head123",
		Reviewers:   []string{"audit", "security"},
	}

	approved, err := engine.allRequiredReviewersApproved(ctx, "security", payload)
	if err != nil {
		t.Fatalf("allRequiredReviewersApproved returned error: %v", err)
	}
	if !approved {
		t.Fatal("allRequiredReviewersApproved = false, want the sub-threshold review to count")
	}
}

func TestEngineAdvanceReviewSubthresholdEventIsIdempotentAfterMergeError(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.ReviewBlockingSeverity = func(string) string { return reviewseverity.P1 }
	gate := &fakeMergeGate{err: errors.New("temporary merge gate failure")}
	engine.MergeGate = gate
	insertCompletedJob(t, store, db.Job{ID: "review-notes-retry", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "mobile/app",
		Branch:      "task-9",
		PullRequest: 9,
		TaskID:      "task-9",
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: reviewseverity.P2,
			Summary:  "non-blocking retry note",
		},
	})

	for attempt := 1; attempt <= 2; attempt++ {
		if err := engine.AdvanceJob(ctx, "review-notes-retry"); err == nil || !strings.Contains(err.Error(), "temporary merge gate failure") {
			t.Fatalf("AdvanceJob attempt %d error = %v, want temporary merge gate failure", attempt, err)
		}
	}
	if len(gate.requests) != 2 {
		t.Fatalf("merge gate requests = %d, want 2 retry attempts", len(gate.requests))
	}
	if got := countJobEvents(t, store, "review-notes-retry", ReviewApprovedWithNotesEventKind); got != 1 {
		t.Fatalf("%s events = %d, want 1 across retries", ReviewApprovedWithNotesEventKind, got)
	}
}

// A sub-threshold review that advances once the task has ALREADY reached
// ready_to_merge must still record the durable review_approved_with_notes event.
// Writing it on the far side of the approval replay guard drops it silently and
// leaves the job's event history contradicting the PR comment for the same job.
func TestEngineAdvanceReviewSubthresholdRecordsOutcomeAfterTaskReadyToMerge(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.ReviewBlockingSeverity = func(string) string { return reviewseverity.P1 }
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Ready: true}}
	base := JobPayload{
		Repo: "mobile/app", Branch: "task-8", PullRequest: 8,
		TaskID: "task-8", TaskTitle: "Mobile App", LeadAgent: "lead",
	}
	first := base
	first.Result = &AgentResult{Decision: "approved", Summary: "ship it"}
	insertCompletedJob(t, store, db.Job{ID: "review-first", Agent: "audit", Type: "review"}, first)
	if err := engine.AdvanceJob(ctx, "review-first"); err != nil {
		t.Fatalf("AdvanceJob(review-first) returned error: %v", err)
	}
	assertTaskState(t, store, "task-8", TaskReadyToMerge)

	second := base
	second.Result = &AgentResult{
		Decision: "changes_requested", Severity: reviewseverity.P2, Summary: "non-blocking polish",
	}
	insertCompletedJob(t, store, db.Job{ID: "review-second", Agent: "sec", Type: "review"}, second)
	if err := engine.AdvanceJob(ctx, "review-second"); err != nil {
		t.Fatalf("AdvanceJob(review-second) returned error: %v", err)
	}
	if got := countJobEvents(t, store, "review-second", ReviewApprovedWithNotesEventKind); got != 1 {
		t.Fatalf("%s events = %d, want 1 despite the task already being ready_to_merge",
			ReviewApprovedWithNotesEventKind, got)
	}
	// The replay guard must still hold: advancing again records no second event.
	if err := engine.AdvanceJob(ctx, "review-second"); err != nil {
		t.Fatalf("replayed AdvanceJob(review-second) returned error: %v", err)
	}
	if got := countJobEvents(t, store, "review-second", ReviewApprovedWithNotesEventKind); got != 1 {
		t.Fatalf("%s events = %d after replay, want 1", ReviewApprovedWithNotesEventKind, got)
	}
}

// A PR-less review still records its folded sub-threshold outcome. Gating the
// outcome write on PullRequest > 0 would suppress the only event proof/project.go
// recognizes for a review whose folded verdict is load-bearing.
func TestEngineAdvanceReviewSubthresholdRecordsOutcomeWithoutPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.ReviewBlockingSeverity = func(string) string { return reviewseverity.P1 }
	insertCompletedJob(t, store, db.Job{ID: "review-noprr", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "mobile/app", TaskID: "task-8", TaskTitle: "Mobile App", LeadAgent: "lead",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: reviewseverity.P2, Summary: "non-blocking polish",
		},
	})
	if err := engine.AdvanceJob(ctx, "review-noprr"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if got := countJobEvents(t, store, "review-noprr", ReviewApprovedWithNotesEventKind); got != 1 {
		t.Fatalf("%s events = %d, want 1 for a PR-less folded review", ReviewApprovedWithNotesEventKind, got)
	}
	// The PR-less arm still terminates without review advancement.
	if got := countJobEvents(t, store, "review-noprr", "advance_skipped_no_pr"); got != 1 {
		t.Fatalf("advance_skipped_no_pr events = %d, want 1", got)
	}
}

// Negative controls for the hoisted write. Moving it to the top of AdvanceJob put
// it AHEAD of the guards that used to sit between the function entry and the old
// write site — the stale-review-round return among them.
// Each exclusion therefore has to be a property of recordFoldedReviewOutcome
// itself, not of where it is called. These pin that.
func TestRecordFoldedReviewOutcomeExclusionsSurviveTheHoist(t *testing.T) {
	base := func() JobPayload {
		return JobPayload{
			Repo: "mobile/app", Branch: "task-8", PullRequest: 8, TaskID: "task-8",
			TaskTitle: "Mobile App", LeadAgent: "lead",
		}
	}
	cases := []struct {
		name    string
		jobType string
		mutate  func(*JobPayload)
		want    int
	}{
		{
			name: "blocked review records nothing", jobType: "review",
			mutate: func(p *JobPayload) {
				p.Result = &AgentResult{Decision: "blocked", Severity: reviewseverity.P2, Summary: "stuck", Needs: []string{"a checkout"}}
			},
			want: 0,
		},
		{
			name: "failed review records nothing", jobType: "review",
			mutate: func(p *JobPayload) {
				p.Result = &AgentResult{Decision: "failed", Severity: reviewseverity.P2, Summary: "crashed"}
			},
			want: 0,
		},
		{
			name: "at-threshold review records nothing", jobType: "review",
			mutate: func(p *JobPayload) {
				p.Result = &AgentResult{Decision: "changes_requested", Severity: reviewseverity.P1, Summary: "blocking"}
			},
			want: 0,
		},
		{
			name: "an ask job is never folded", jobType: "ask",
			mutate: func(p *JobPayload) {
				p.Result = &AgentResult{Decision: "changes_requested", Severity: reviewseverity.P2, Summary: "not a review"}
			},
			want: 0,
		},
		{
			name: "an implement job is never folded", jobType: "implement",
			mutate: func(p *JobPayload) {
				p.Result = &AgentResult{Decision: "changes_requested", Severity: reviewseverity.P2, Summary: "not a review"}
			},
			want: 0,
		},
		{
			// #2373: a part never approves the head, and proof turns this event
			// into a review.approved claim.
			name: "a folded part verdict records nothing", jobType: "review",
			mutate: func(p *JobPayload) {
				p.ReviewPart = "mm-1-mm-9"
				p.Result = &AgentResult{Decision: "changes_requested", Severity: reviewseverity.P2, Summary: "non-blocking"}
			},
			want: 0,
		},
		{
			name: "the shape that SHOULD be recorded still is", jobType: "review",
			mutate: func(p *JobPayload) {
				p.Result = &AgentResult{Decision: "changes_requested", Severity: reviewseverity.P2, Summary: "non-blocking"}
			},
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			engine := testEngine(store)
			engine.ReviewBlockingSeverity = func(string) string { return reviewseverity.P1 }
			payload := base()
			tc.mutate(&payload)
			job := db.Job{ID: "j-" + tc.jobType, Agent: "audit", Type: tc.jobType}
			if err := engine.recordFoldedReviewOutcome(ctx, job, payload); err != nil {
				t.Fatalf("recordFoldedReviewOutcome returned error: %v", err)
			}
			if got := countJobEvents(t, store, job.ID, ReviewApprovedWithNotesEventKind); got != tc.want {
				t.Fatalf("%s events = %d, want %d", ReviewApprovedWithNotesEventKind, got, tc.want)
			}
		})
	}
}

// The one BEHAVIOUR CHANGE the hoist introduces: the write now runs ahead of the
// stale-review-round return, so a stale round records its own folded outcome
// where previously it recorded nothing. That is defensible — the event is a fact
// about THAT job's verdict, not about whether its round is current — but it is
// only safe if it cannot buy merge eligibility. This enters through AdvanceJob
// to pin both the hoisted write and the gate's rejection of the superseded round.
func TestStaleRoundOutcomeEventDoesNotGrantMergeEligibility(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.ReviewBlockingSeverity = func(string) string { return reviewseverity.P1 }

	stale := JobPayload{
		Repo: "mobile/app", Branch: "task-9", PullRequest: 9, HeadSHA: "head123",
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: reviewseverity.P2, Summary: "old round",
		},
	}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-stale", Agent: "audit", Type: "review"}, stale)

	// A LATER round from the same reviewer makes review-1 stale before AdvanceJob
	// sees it and supersedes it with a real blocking verdict.
	current := stale
	current.ReviewRound = "review-2"
	current.Result = &AgentResult{
		Decision: "changes_requested", Severity: reviewseverity.P0, Summary: "blocking now",
	}
	insertCompletedJob(t, store, db.Job{ID: "review-current", Agent: "audit", Type: "review"}, current)

	// AdvanceJob must still record the stale round's folded outcome before taking
	// its stale-round return.
	if err := engine.AdvanceJob(ctx, "review-stale"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if got := countJobEvents(t, store, "review-stale", ReviewApprovedWithNotesEventKind); got != 1 {
		t.Fatalf("%s events = %d, want 1", ReviewApprovedWithNotesEventKind, got)
	}

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "audit",
		ReviewBlockingSeverity: reviewseverity.P1,
	}, "head123")
	var blocked mergeBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("ensureFinalReviewCaptured = %v, want the current blocking round to block", err)
	}
}

func TestEngineAdvanceReviewSkipsPullRequestFlowWhenNoPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	// A non-nil merge gate that fails the test if it is ever evaluated: a PR-less
	// approved review must not route into the merge gate (the daemon wires a real
	// gate, so this mirrors that configuration).
	engine.MergeGate = &fakeMergeGate{onEvaluate: func(MergeRequest) {
		t.Fatalf("merge gate evaluated for a PR-less review; want skip")
	}}
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:      "gitmoot/gitmoot",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		// PullRequest=0 / Branch="" (a PR-less review).
		Result: &AgentResult{Decision: "changes_requested", Summary: "review stands as a comment"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	// No fix job is dispatched from a context-free review.
	if _, err := store.GetJob(ctx, "implement-lead-task-7"); err == nil {
		t.Fatalf("a PR-less review dispatched a fix job; want none")
	}
	events, err := store.ListJobEvents(ctx, "review-job")
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

func TestEngineAdvanceReviewApprovedSkipsMergeGateWhenNoPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{onEvaluate: func(MergeRequest) {
		t.Fatalf("merge gate evaluated for a PR-less review; want skip")
	}}
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:      "gitmoot/gitmoot",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		Result:    &AgentResult{Decision: "approved", Summary: "looks good"},
	})

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "review-job")
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

func TestEngineFailedResultWritesCurrentBlockOwnership(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{ID: "failed-result", Agent: "worker", Type: "ask"}, JobPayload{
		Repo:      "gitmoot/gitmoot",
		Branch:    "task-8",
		TaskID:    "task-8",
		TaskTitle: "Failed result",
		Result:    &AgentResult{Decision: "failed", Summary: "production failure"},
	})

	err := engine.AdvanceJob(ctx, "failed-result")
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("AdvanceJob error = %v, want BlockedError", err)
	}
	assertTaskState(t, store, "task-8", TaskBlocked)
	events, err := store.ListTaskEvents(ctx, "task-8")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "workflow_blocked" ||
		events[0].ToState != string(TaskBlocked) ||
		events[0].Reason != "production failure" {
		t.Fatalf("task events = %+v, want current workflow_blocked ownership", events)
	}
}

func TestHandlePullRequestReadyToMergeWritesMergeGateBlockOwnership(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	engine := testEngine(store)
	expectedState := ""
	engine.MergeGate = &fakeMergeGate{
		decision: MergeDecision{
			Ready: false, Reason: PlainReason("dirty worktree"), BlockClass: MergeBlockTransient,
		},
		onEvaluate: func(request MergeRequest) {
			expectedState = request.ExpectedTaskState
		},
	}

	err := engine.HandlePullRequestReadyToMerge(ctx, PullRequestEvent{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 9,
		HeadSHA: "head-9", TaskID: "task-9", LeadAgent: "lead",
	})
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("HandlePullRequestReadyToMerge error = %v, want BlockedError", err)
	}
	if expectedState != string(TaskReadyToMerge) {
		t.Fatalf("merge request expected state = %q, want ready_to_merge", expectedState)
	}
	assertTaskState(t, store, "task-9", TaskBlocked)
	events, err := store.ListTaskEvents(ctx, "task-9")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "merge_gate_blocked" ||
		events[0].ToState != string(TaskBlocked) {
		t.Fatalf("task events = %+v, want merge_gate_blocked ownership", events)
	}
}

// The block state and its owner are one transaction. If ownership persistence
// fails, neither half commits and callers receive the store error rather than a
// false durable BlockedError.
func TestMergeGateAttributionFailureRollsBackBlockedState(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-10", RepoFullName: "gitmoot/gitmoot", Branch: "task-10",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `
CREATE TRIGGER fail_merge_gate_block_attribution
BEFORE INSERT ON task_events
WHEN NEW.kind = 'merge_gate_blocked'
BEGIN
	SELECT RAISE(ABORT, 'forced merge gate attribution failure');
END`); err != nil {
		t.Fatal(err)
	}
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{
		Ready: false, Reason: PlainReason("dirty worktree"), BlockClass: MergeBlockTransient,
	}}

	err = engine.HandlePullRequestReadyToMerge(ctx, PullRequestEvent{
		Repo: "gitmoot/gitmoot", Branch: "task-10", PullRequest: 10,
		HeadSHA: "head-10", TaskID: "task-10", LeadAgent: "lead",
	})
	var blocked BlockedError
	if errors.As(err, &blocked) {
		t.Fatalf("error = %v, must not claim a durable block", err)
	}
	if !strings.Contains(err.Error(), "forced merge gate attribution failure") {
		t.Fatalf("error = %v, want attribution store failure", err)
	}
	assertTaskState(t, store, "task-10", TaskReadyToMerge)
	events, listErr := store.ListTaskEvents(ctx, "task-10")
	if listErr != nil {
		t.Fatalf("ListTaskEvents: %v", listErr)
	}
	if len(events) != 0 {
		t.Fatalf("task events = %+v, want atomic rollback", events)
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
		Repo:        "gitmoot/gitmoot",
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
		Repo:        "gitmoot/gitmoot",
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
		Repo:        "gitmoot/gitmoot",
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
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	reviewers := []string{"audit", "security"}
	insertCompletedJob(t, store, db.Job{
		ID: "original-implement", Agent: "lead", Type: "implement",
	}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		Result: &AgentResult{Decision: "implemented"},
	})
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
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
		Repo:        "gitmoot/gitmoot",
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
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	event := PullRequestEvent{
		Repo:              "gitmoot/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		RequiredReviewers: []string{"audit"},
	}
	// Empty-head dispatch remains allowed before any succeeded review evidence
	// exists. Both jobs below stay queued, preserving the pre-#1427 escalating-
	// round contract while the fail-closed post-success case is pinned separately.

	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("first HandlePullRequestOpened returned error: %v", err)
	}
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("second HandlePullRequestOpened returned error: %v", err)
	}

	mustJob(t, store, "review-audit-task-7-review-1")
	mustJob(t, store, "review-audit-task-7-review-2")
}

// TestEngineReviewLoopBlocksStableVerdictBeforeDispatch kills the unguarded
// restore mutant and an event-only mutant: the second dispatch must block the
// task and leave the review JOB COUNT unchanged, not merely emit an event.
func TestEngineReviewLoopBlocksStableVerdictBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	insertCompletedJob(t, store, db.Job{ID: "prior-review", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head-a",
		TaskID: "task-7", ReviewRound: "review-1",
		Result: &AgentResult{Decision: "changes_requested", Summary: "fix it"},
	})
	engine := testEngine(store)
	event := PullRequestEvent{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head-a",
		TaskID: "task-7", TaskTitle: "Workflow Engine", LeadAgent: "lead",
		RequiredReviewers: []string{"audit"},
	}

	before := reviewJobCount(t, store)
	for attempt := 0; attempt < 2; attempt++ {
		err := engine.HandlePullRequestOpened(ctx, event)
		var blocked BlockedError
		if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "review loop detected") {
			t.Fatalf("attempt %d error = %v, want review-loop BlockedError", attempt+1, err)
		}
		if got := reviewJobCount(t, store); got != before {
			t.Fatalf("attempt %d review job count = %d, want unchanged %d", attempt+1, got, before)
		}
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
	if got := countJobEvents(t, store, "prior-review", ReviewLoopDetectedEventKind); got != 1 {
		t.Fatalf("review_loop_detected events = %d, want one idempotent claim", got)
	}
}

// TestEngineReviewLoopAllowsNewHead kills a repo/PR-only suppression mutant: a
// real new commit must create the next review job.
func TestEngineReviewLoopAllowsNewHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	insertCompletedJob(t, store, db.Job{ID: "prior-review", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head-a",
		TaskID: "task-7", ReviewRound: "review-1", Result: &AgentResult{Decision: "changes_requested"},
	})
	engine := testEngine(store)
	engine.ReviewChangedFiles = func(context.Context, string, int, string, string) ([]string, error) {
		return []string{"internal/workflow/engine.go"}, nil
	}
	if err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head-b",
		TaskID: "task-7", LeadAgent: "lead", RequiredReviewers: []string{"audit"},
	}); err != nil {
		t.Fatalf("new-head dispatch: %v", err)
	}
	if got := reviewJobCount(t, store); got != 2 {
		t.Fatalf("review job count = %d, want prior plus new-head review", got)
	}
}

func TestEngineReviewLoopSparseHistoryDispatchesNextNumericRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	for _, prior := range []struct {
		id    string
		round string
	}{
		{id: "prior-review-10-a", round: "review-10"},
		{id: "prior-review-10-b", round: "review-10"},
		{id: "prior-review-malformed", round: "manual-round"},
		{id: "prior-review-empty", round: ""},
	} {
		insertCompletedJob(t, store, db.Job{ID: prior.id, Agent: "audit", Type: "review"}, JobPayload{
			Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head-a",
			TaskID: "task-7", ReviewRound: prior.round,
			Result: &AgentResult{Decision: "changes_requested", Summary: "fix it"},
		})
	}
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	engine.ReviewChangedFiles = func(context.Context, string, int, string, string) ([]string, error) {
		return []string{"internal/workflow/engine.go"}, nil
	}
	if err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head-b",
		TaskID: "task-7", LeadAgent: "lead", RequiredReviewers: []string{"audit"},
	}); err != nil {
		t.Fatalf("new-head dispatch: %v", err)
	}

	const jobID = "review-audit-task-7-review-11"
	job := mustJob(t, store, jobID)
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.ReviewRound != "review-11" {
		t.Fatalf("review round = %q, want review-11", payload.ReviewRound)
	}
	payload.Result = &AgentResult{Decision: "approved", Summary: "ready"}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, jobID, encoded); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}
	if err := store.UpdateJobState(ctx, jobID, string(JobSucceeded)); err != nil {
		t.Fatalf("UpdateJobState: %v", err)
	}
	if err := engine.AdvanceJob(ctx, jobID); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if len(gate.requests) != 1 {
		t.Fatalf("merge gate requests = %d, want 1", len(gate.requests))
	}
}

// TestEngineReviewLoopAllowsMixedDecisionsAtSameHead kills a decision-blind
// mutant. A verdict flip is unstable evidence and must not freeze progress.
func TestEngineReviewLoopAllowsMixedDecisionsAtSameHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	for _, prior := range []struct{ id, round, decision string }{
		{id: "prior-approved", round: "review-1", decision: "approved"},
		{id: "prior-changes", round: "review-2", decision: "changes_requested"},
	} {
		insertCompletedJob(t, store, db.Job{ID: prior.id, Agent: "historical", Type: "review"}, JobPayload{
			Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head-a",
			TaskID: "task-7", ReviewRound: prior.round, Result: &AgentResult{Decision: prior.decision},
		})
	}
	engine := testEngine(store)
	if err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head-a",
		TaskID: "task-7", LeadAgent: "lead", RequiredReviewers: []string{"audit"},
	}); err != nil {
		t.Fatalf("mixed-decision dispatch: %v", err)
	}
	if got := reviewJobCount(t, store); got != 3 {
		t.Fatalf("review job count = %d, want two historical plus one new review", got)
	}
}

// TestEngineReviewLoopEmptyHeadFailsClosedAfterSucceededHistory kills the
// manual-retry fail-open mutant without rejecting the first-ever empty-head
// dispatch pinned above.
func TestEngineReviewLoopEmptyHeadFailsClosedAfterSucceededHistory(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	insertCompletedJob(t, store, db.Job{ID: "prior-review", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "known-head",
		TaskID: "task-7", ReviewRound: "review-1", Result: &AgentResult{Decision: "changes_requested"},
	})
	engine := testEngine(store)
	before := reviewJobCount(t, store)
	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7,
		TaskID: "task-7", LeadAgent: "lead", RequiredReviewers: []string{"audit"},
	})
	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "head SHA is empty") {
		t.Fatalf("error = %v, want empty-head review-loop BlockedError", err)
	}
	if got := reviewJobCount(t, store); got != before {
		t.Fatalf("review job count = %d, want unchanged %d", got, before)
	}
}

func TestEngineHandlePullRequestOpenedPreflightsReviewersBeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "gitmoot/gitmoot",
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
		Repo:        "gitmoot/gitmoot",
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
		Repo:        "gitmoot/gitmoot",
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
		Repo:        "gitmoot/gitmoot",
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
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "old approval"},
	})
	encoded, err := marshalPayload(JobPayload{
		Repo:        "gitmoot/gitmoot",
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
		Repo:        "gitmoot/gitmoot",
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
		Repo:        "gitmoot/gitmoot",
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
		Repo:        "gitmoot/gitmoot",
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
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Reason: PlainReason("checks are pending")}}
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
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
		Repo:      "gitmoot/gitmoot",
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
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
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
		// Default the #536 physical worktree-process probe to "no live process" so
		// cleanup tests are not influenced by whatever real processes happen to live
		// on the host running the suite. Tests exercising the lease-expiry-boundary
		// gate (finding 1) override this explicitly.
		WorktreeHasLiveProcess: func(string) bool { return false },
		// Most cleanup behavior tests predate deterministic-path identity and use
		// compact synthetic payloads. Keep those tests focused on their original
		// axis; identity/containment tests clear this seam and exercise production.
		cleanupTargetValidator: func(home, _, _ string, payload JobPayload) error {
			return cleanupPathContained(filepath.Join(home, "worktrees"), payload.WorktreePath)
		},
	}
}

func seedAgent(t *testing.T, store *db.Store, name string, capabilities []string, repo string) {
	t.Helper()
	// Implement-capable agents need a write policy or the fail-closed dispatch
	// preflight (ensureAgentAllowedWithBranchOwner) BLOCKS their jobs (#452). Pick a
	// writable policy automatically so the ~dozens of implement seeds stay valid
	// without per-call-site churn; non-implement seeds keep the default "auto".
	policy := "auto"
	if runtime.HasImplementCapability(capabilities) {
		policy = runtime.AutonomyPolicyWorkspaceWrite
	}
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           name,
		Role:           "agent",
		Runtime:        runtime.ShellRuntime,
		RuntimeRef:     "printf ok",
		RepoScope:      repo,
		Capabilities:   capabilities,
		AutonomyPolicy: policy,
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
}

func insertCompletedJob(t *testing.T, store *db.Store, job db.Job, payload JobPayload) {
	t.Helper()
	// #2004: the merge gate now resolves a runtime FAMILY and fails closed when it
	// cannot. These fixtures predate that and name agents without registering
	// them, which is a shape production does not have - a dispatched job's agent
	// is registered, or is a temp/ephemeral name derived from one that is.
	// Registering each on a family of its own preserves what every name-only
	// expectation in this suite already assumed.
	seedMergeGateFixtureAgent(t, store, job.Agent)
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

func jobExists(t *testing.T, store *db.Store, jobID string) bool {
	t.Helper()
	_, err := store.GetJob(context.Background(), jobID)
	if err == nil {
		return true
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	t.Fatalf("GetJob(%s) returned error: %v", jobID, err)
	return false
}

func countJobEvents(t *testing.T, store *db.Store, jobID, kind string) int {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s) returned error: %v", jobID, err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func reviewJobCount(t *testing.T, store *db.Store) int {
	t.Helper()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	count := 0
	for _, job := range jobs {
		if job.Type == "review" {
			count++
		}
	}
	return count
}

// jobEventMessage returns the message of the first event of kind on jobID, or ""
// when none exists.
func jobEventMessage(t *testing.T, store *db.Store, jobID, kind string) string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s) returned error: %v", jobID, err)
	}
	for _, event := range events {
		if event.Kind == kind {
			return event.Message
		}
	}
	return ""
}

type fakeMergeGate struct {
	decision   MergeDecision
	onEvaluate func(MergeRequest)
	err        error
	requests   []MergeRequest
}

func (f *fakeMergeGate) Evaluate(_ context.Context, request MergeRequest) (MergeDecision, error) {
	f.requests = append(f.requests, request)
	if f.onEvaluate != nil {
		f.onEvaluate(request)
	}
	return f.decision, f.err
}
