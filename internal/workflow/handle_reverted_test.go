package workflow

import (
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// TestHandlePullRequestRevertedFiresCorrectiveHarvest proves the wired revert
// trigger (#467): HandlePullRequestReverted resolves the ORIGINAL implement job
// via implementJobForTask (sameTask) and fires the harvester with
// Outcome{Kind: OutcomeReverted, Repo, PullRequest:<orig>}, attributed to the
// implement job that produced the original diff.
func TestHandlePullRequestRevertedFiresCorrectiveHarvest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester
	seedImplementJobForHarvest(t, store)

	if err := engine.HandlePullRequestReverted(ctx, RevertEvent{
		Repo:                "jerryfane/gitmoot",
		OriginalPullRequest: 7,
		OriginalBranch:      "task-7",
		OriginalTaskID:      "task-7",
	}); err != nil {
		t.Fatalf("HandlePullRequestReverted returned error: %v", err)
	}

	got := harvester.snapshot()
	if len(got) != 1 {
		t.Fatalf("Harvest calls = %d, want 1; %+v", len(got), got)
	}
	if got[0].Kind != OutcomeReverted {
		t.Fatalf("outcome kind = %q, want reverted", got[0].Kind)
	}
	if got[0].PullRequest != 7 || got[0].Repo != "jerryfane/gitmoot" {
		t.Fatalf("outcome attribution = %+v, want repo=jerryfane/gitmoot pr=7", got[0])
	}
	if harvester.jobIDs[0] != "implement-job" {
		t.Fatalf("reverted outcome attributed to %q, want implement-job", harvester.jobIDs[0])
	}
}

// TestHandlePullRequestRevertedResolvesByRepoPROnly proves the corrective harvest
// still resolves the original implement job when only Repo+PR are known (no
// branch/task hints), matching sameTask's Repo+PullRequest path — so the daemon
// need not fetch the original head/task to map the revert.
func TestHandlePullRequestRevertedResolvesByRepoPROnly(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester
	// Seed an implement job WITHOUT a TaskID so sameTask must match on Repo+PR.
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-no-task",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:                   "jerryfane/gitmoot",
		PullRequest:            42,
		TemplateID:             "planner",
		TemplateResolvedCommit: "commit-1",
		Result:                 &AgentResult{Decision: "implemented", Summary: "did the work"},
	})

	if err := engine.HandlePullRequestReverted(ctx, RevertEvent{
		Repo:                "jerryfane/gitmoot",
		OriginalPullRequest: 42,
	}); err != nil {
		t.Fatalf("HandlePullRequestReverted returned error: %v", err)
	}

	got := harvester.snapshot()
	if len(got) != 1 || got[0].Kind != OutcomeReverted || got[0].PullRequest != 42 {
		t.Fatalf("Repo+PR-only resolution = %+v, want one reverted outcome for pr 42", got)
	}
}

// TestHandlePullRequestRevertedNilHarvesterNoOp proves the nil-harvester (default,
// auto_trace off) path is a byte-identical no-op: no lookup, no fire, no error.
func TestHandlePullRequestRevertedNilHarvesterNoOp(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	// No OutcomeHarvester.
	seedImplementJobForHarvest(t, store)

	if err := engine.HandlePullRequestReverted(ctx, RevertEvent{
		Repo:                "jerryfane/gitmoot",
		OriginalPullRequest: 7,
	}); err != nil {
		t.Fatalf("HandlePullRequestReverted with nil harvester returned error: %v", err)
	}
}

// TestHandlePullRequestRevertedUnresolvableSkips proves the corrective harvest is
// SKIPPED (no fire, no error) when no implement job owns the original PR — so a
// revert of a PR opened outside the implement flow never forges a spurious fresh
// negative.
func TestHandlePullRequestRevertedUnresolvableSkips(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester
	// No implement job seeded for PR 99.

	if err := engine.HandlePullRequestReverted(ctx, RevertEvent{
		Repo:                "jerryfane/gitmoot",
		OriginalPullRequest: 99,
	}); err != nil {
		t.Fatalf("HandlePullRequestReverted returned error: %v", err)
	}
	if got := harvester.snapshot(); len(got) != 0 {
		t.Fatalf("Harvest calls = %d, want 0 (unresolvable original PR); %+v", len(got), got)
	}
}

// TestHandlePullRequestRevertedInvalidEventSkips proves a malformed event (empty
// repo or non-positive PR) is fail-safe: no fire, no error, no lookup.
func TestHandlePullRequestRevertedInvalidEventSkips(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester
	seedImplementJobForHarvest(t, store)

	for _, event := range []RevertEvent{
		{Repo: "", OriginalPullRequest: 7},
		{Repo: "jerryfane/gitmoot", OriginalPullRequest: 0},
		{Repo: "jerryfane/gitmoot", OriginalPullRequest: -1},
	} {
		if err := engine.HandlePullRequestReverted(ctx, event); err != nil {
			t.Fatalf("HandlePullRequestReverted(%+v) returned error: %v", event, err)
		}
	}
	if got := harvester.snapshot(); len(got) != 0 {
		t.Fatalf("Harvest calls = %d, want 0 (invalid events); %+v", len(got), got)
	}
}
