package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// recordingHarvester captures OutcomeHarvester.Harvest calls for the engine
// best-effort tests. When err is set, Harvest returns it so a test can prove a
// harvest failure never fails AdvanceJob and is recorded as a best-effort job
// event.
type recordingHarvester struct {
	mu       sync.Mutex
	outcomes []Outcome
	jobIDs   []string
	err      error
}

func (r *recordingHarvester) Harvest(_ context.Context, job db.Job, _ JobPayload, outcome Outcome) error {
	r.mu.Lock()
	r.outcomes = append(r.outcomes, outcome)
	r.jobIDs = append(r.jobIDs, job.ID)
	r.mu.Unlock()
	return r.err
}

func (r *recordingHarvester) snapshot() []Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Outcome, len(r.outcomes))
	copy(out, r.outcomes)
	return out
}

// seedImplementJobForHarvest inserts a completed implement job carrying a template
// attribution so the harvester's implementJobForTask resolves it as the diff owner
// for the task's merge/review outcome.
func seedImplementJobForHarvest(t *testing.T, store *db.Store) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-7",
		PullRequest:            7,
		TaskID:                 "task-7",
		TaskTitle:              "Workflow Engine",
		LeadAgent:              "lead",
		TemplateID:             "planner",
		TemplateResolvedCommit: "commit-1",
		Result:                 &AgentResult{Decision: "implemented", Summary: "did the work"},
	})
}

// TestEngineHarvestsMergedOutcomeOnce proves AdvanceJob calls Harvest exactly once
// with OutcomeMerged when an approval drives runMergeGate to a merge, attributing
// it to the implement job.
func TestEngineHarvestsMergedOutcomeOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester
	seedImplementJobForHarvest(t, store)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "head123",
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "approved", Summary: "looks good"},
	})

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskMerged)

	got := harvester.snapshot()
	if len(got) != 1 {
		t.Fatalf("Harvest calls = %d, want 1; %+v", len(got), got)
	}
	if got[0].Kind != OutcomeMerged {
		t.Fatalf("outcome kind = %q, want merged", got[0].Kind)
	}
	// The no-CI guard reads statuses/checks at the PR HEAD SHA (where the merge gate
	// posted them), NOT the merge commit — GitHub does not copy them onto the merge
	// commit, so reading there would always look like no CI.
	if got[0].HeadSHA != "head123" {
		t.Fatalf("outcome head sha = %q, want the PR head SHA head123", got[0].HeadSHA)
	}
	if got[0].PullRequest != 7 || got[0].Repo != "gitmoot/gitmoot" {
		t.Fatalf("outcome attribution = %+v", got[0])
	}
	if harvester.jobIDs[0] != "implement-job" {
		t.Fatalf("merged outcome attributed to %q, want implement-job", harvester.jobIDs[0])
	}
}

// TestEngineHarvestsBlockedOutcomeOnce proves a not-ready merge gate harvests
// exactly one OutcomeBlocked carrying the rejection reason, AND that the job is
// still blocked (the harvest does not change the block transition).
func TestEngineHarvestsBlockedOutcomeOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Reason: "external CI failed", BlockClass: MergeBlockQuality}}
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester
	seedImplementJobForHarvest(t, store)
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
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "approved", Summary: "looks good"},
	})

	err := engine.AdvanceJob(ctx, "review-job")
	var blocked BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "external CI failed" {
		t.Fatalf("AdvanceJob error = %v, want blocked", err)
	}
	got := harvester.snapshot()
	if len(got) != 1 || got[0].Kind != OutcomeBlocked {
		t.Fatalf("Harvest calls = %+v, want one blocked", got)
	}
	if got[0].Reason != "external CI failed" {
		t.Fatalf("blocked outcome reason = %q", got[0].Reason)
	}
}

// TestEngineSkipsTransientBlockHarvest proves a transient/infra merge-gate block
// (BlockClass=MergeBlockTransient: branch staleness, dirty local worktree, …) is
// NOT harvested — only authoritative template-quality blocks score a negative, so
// branch-staleness/infra noise is never mis-attributed to the template (#465
// INFRA-NOISE-FILTERED). The block transition itself still happens.
func TestEngineSkipsTransientBlockHarvest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{
		Reason:     "pull request is not mergeable; rebase or update the branch",
		BlockClass: MergeBlockTransient,
	}}
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester
	seedImplementJobForHarvest(t, store)
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
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "approved", Summary: "looks good"},
	})

	err := engine.AdvanceJob(ctx, "review-job")
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("AdvanceJob error = %v, want a blocked transition", err)
	}
	if got := harvester.snapshot(); len(got) != 0 {
		t.Fatalf("transient block must not be harvested, got %+v", got)
	}
}

func TestRunMergeGateDeferredLeavesTaskReadyToMerge(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-deferred", RepoFullName: "gitmoot/gitmoot", GoalID: "goal-1",
		Title: "Deferred merge", State: string(TaskReadyToMerge), Branch: "fix-round",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{
		Ready: false, Deferred: true, Reason: "active implement job in flight",
		BlockClass: MergeBlockTransient,
	}}
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester

	decision, err := engine.runMergeGate(ctx, "", JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "fix-round", PullRequest: 17,
		TaskID: "task-deferred", GoalID: "goal-1", TaskTitle: "Deferred merge",
	}, taskRef{
		ID: "task-deferred", Repo: "gitmoot/gitmoot", GoalID: "goal-1",
		Title: "Deferred merge", Branch: "fix-round",
	})
	if err != nil {
		t.Fatalf("runMergeGate returned error for deferred decision: %v", err)
	}
	if decision.Ready || !decision.Deferred || decision.Merged {
		t.Fatalf("decision = %+v, want deferred not-ready hold", decision)
	}
	assertTaskState(t, store, "task-deferred", TaskReadyToMerge)
	if got := harvester.snapshot(); len(got) != 0 {
		t.Fatalf("deferred decision must not be harvested, got %+v", got)
	}
}

// A deferral reaching runMergeGate from the no-reviewers auto-merge path arrives
// with the task in pull_request_open (not ready_to_merge). The deferral must PARK
// it at ready_to_merge so the daemon's ready-to-merge poll re-drives it; leaving
// it in pull_request_open would wedge the PR unmerged forever.
func TestRunMergeGateDeferredParksPullRequestOpenTaskAtReadyToMerge(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-pr-open", RepoFullName: "gitmoot/gitmoot", GoalID: "goal-1",
		Title: "No-reviewers merge", State: string(TaskPullRequestOpen), Branch: "auto-round",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{
		Ready: false, Deferred: true, Reason: "active implement job in flight",
		BlockClass: MergeBlockTransient,
	}}

	decision, err := engine.runMergeGate(ctx, "", JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "auto-round", PullRequest: 21,
		TaskID: "task-pr-open", GoalID: "goal-1", TaskTitle: "No-reviewers merge",
	}, taskRef{
		ID: "task-pr-open", Repo: "gitmoot/gitmoot", GoalID: "goal-1",
		Title: "No-reviewers merge", Branch: "auto-round",
	})
	if err != nil {
		t.Fatalf("runMergeGate returned error for deferred decision: %v", err)
	}
	if decision.Ready || !decision.Deferred || decision.Merged {
		t.Fatalf("decision = %+v, want deferred not-ready hold", decision)
	}
	assertTaskState(t, store, "task-pr-open", TaskReadyToMerge)
}

func TestRunMergeGateNonDeferredNotReadyStillBlocks(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-blocked", RepoFullName: "gitmoot/gitmoot", GoalID: "goal-1",
		Title: "Blocked merge", State: string(TaskReadyToMerge), Branch: "blocked-round",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{
		Ready: false, Reason: "external CI failed", BlockClass: MergeBlockQuality,
	}}

	decision, err := engine.runMergeGate(ctx, "", JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "blocked-round", PullRequest: 18,
		TaskID: "task-blocked", GoalID: "goal-1", TaskTitle: "Blocked merge",
	}, taskRef{
		ID: "task-blocked", Repo: "gitmoot/gitmoot", GoalID: "goal-1",
		Title: "Blocked merge", Branch: "blocked-round",
	})
	var blocked BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "external CI failed" {
		t.Fatalf("runMergeGate error = %v, want BlockedError for unchanged block path", err)
	}
	if decision.Ready || decision.Deferred || decision.Merged {
		t.Fatalf("decision = %+v, want non-deferred not-ready block", decision)
	}
	assertTaskState(t, store, "task-blocked", TaskBlocked)
}

// TestEngineHarvestsChangesRequestedOnce proves a review changes_requested
// harvests exactly one OutcomeChangesRequested while still dispatching the fix.
func TestEngineHarvestsChangesRequestedOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester
	seedImplementJobForHarvest(t, store)
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
		LeadAgent:   "lead",
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	// The fix was dispatched (the JobID helper appends the review round).
	mustJob(t, store, "implement-lead-task-7-review-2")

	got := harvester.snapshot()
	if len(got) != 1 || got[0].Kind != OutcomeChangesRequested {
		t.Fatalf("Harvest calls = %+v, want one changes_requested", got)
	}
	if got[0].FixRounds != 2 {
		t.Fatalf("changes_requested fix rounds = %d, want 2 (review-2)", got[0].FixRounds)
	}
}

// TestEngineHarvestErrorDoesNotFailAdvance proves a Harvest that returns an error
// is best-effort: AdvanceJob still succeeds, the merge still happens, and an
// auto_trace_harvest_failed job event is recorded.
func TestEngineHarvestErrorDoesNotFailAdvance(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine.OutcomeHarvester = &recordingHarvester{err: errors.New("harvest boom")}
	seedImplementJobForHarvest(t, store)
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
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "approved", Summary: "looks good"},
	})

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob must not fail on a harvest error, got: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskMerged)

	// The failure is recorded as a best-effort job event on the implement job.
	events, err := store.ListJobEvents(ctx, "implement-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind == "auto_trace_harvest_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an auto_trace_harvest_failed job event, got %+v", events)
	}
}

// TestEngineNilHarvesterIsByteIdentical proves the off-by-default path: with no
// OutcomeHarvester the merge advances exactly as before and no harvest fires.
func TestEngineNilHarvesterIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	// No OutcomeHarvester.
	seedImplementJobForHarvest(t, store)
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
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "approved", Summary: "looks good"},
	})

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob with nil harvester returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskMerged)
}

// TestReviewRoundCount pins the fix-round mapping the changes_requested grading
// depends on.
func TestReviewRoundCount(t *testing.T) {
	cases := map[string]int{
		"":          1,
		"review-1":  1,
		"review-3":  3,
		"weird":     1,
		"review-0":  1,
		"review-10": 10,
	}
	for round, want := range cases {
		if got := reviewRoundCount(round); got != want {
			t.Fatalf("reviewRoundCount(%q) = %d, want %d", round, got, want)
		}
	}
}
