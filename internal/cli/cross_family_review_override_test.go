package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedOverrideImplementJob seeds a completed implement job whose REGISTERED agent
// runs on agentRuntime while its payload carries a per-job --runtime override
// (#531) naming a different one — the exact disagreement resolveImplementerRuntime
// has to resolve. It mirrors seedCodexImplementJob (same ids/task/attribution) so
// the rest of the chain behaves identically; only the runtime pair differs.
func seedOverrideImplementJob(t *testing.T, store *db.Store, version db.AgentTemplateVersion, agentRuntime, agentRef, override string) {
	t.Helper()
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:         "lead",
		Runtime:      agentRuntime,
		RepoScope:    "gitmoot/gitmoot",
		Capabilities: []string{"implement"},
		RuntimeRef:   agentRef,
	}); err != nil {
		t.Fatalf("UpsertAgent(lead) returned error: %v", err)
	}
	insertChainJob(t, store, db.Job{ID: "implement-job", Agent: "lead", Type: "implement"}, workflow.JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head123",
		TaskID: "task-7", TaskTitle: "Workflow Engine", LeadAgent: "lead",
		TemplateID: version.TemplateID, TemplateResolvedCommit: version.ResolvedCommit,
		Instructions:    "Implement the cross-family review",
		RuntimeOverride: override,
		Result:          &workflow.AgentResult{Decision: "implemented", Summary: "did the work", ChangesMade: []string{"x.go"}},
	})
}

// reviewRefusalEvent returns the cross_family_review_failed event the engine
// records for a refused review leg (runReviewLeg, engine_routing_merge.go), or
// nil when the leg did not refuse.
func reviewRefusalEvent(t *testing.T, store *db.Store) *db.JobEvent {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), "implement-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for i := range events {
		if events[i].Kind == "cross_family_review_failed" {
			return &events[i]
		}
	}
	return nil
}

// TestCrossFamilyReviewHonorsRuntimeOverrideOmpRefusal is the MAJOR case: an agent
// registered on claude whose implement job carried `--runtime omp` ran on OMP, so
// the review leg must refuse loudly (#1428) exactly as it would for an
// omp-registered agent.
//
// Before this fix resolveImplementerRuntime consulted only the ephemeral spec and
// the STORE agent, never payload.RuntimeOverride — so a per-job override was a
// silent bypass of the refusal: the leg attributed the diff to claude, picked a
// "cross-family" reviewer for a family that never ran the job, and the merge gate
// harvested a review row whose diversity claim is false. The override is what
// actually executed, so it wins, matching resolveTranscriptRuntime (job.go).
//
// A cross-family reviewer IS registered and authed here on purpose: the refusal
// must come from the runtime resolution, not from "nobody was available".
func TestCrossFamilyReviewHonorsRuntimeOverrideOmpRefusal(t *testing.T) {
	ctx := context.Background()
	store := openTraceChainStore(t)
	version := installChainTemplate(t, store, "planner")
	gate := &stubMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := chainEngine(store, gate)
	engine.OutcomeHarvester = enabledHarvesterFor(t, store, harvestGitHubStub{status: realCIChainStatus()})
	engine.ReviewLegDispatcher = reviewDispatcherForTest(store, harvestGitHubStub{})

	seedOverrideImplementJob(t, store, version, runtime.ClaudeRuntime, "last", runtime.OmpRuntime)
	registerShellReviewer(t, store)
	seedChainApprovingReview(t, store, "head123")

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	refusal := reviewRefusalEvent(t, store)
	if refusal == nil {
		t.Fatal("an omp --runtime override must reach the loud refusal (cross_family_review_failed); the registered claude default is not what ran")
	}
	if !strings.Contains(refusal.Message, runtime.OmpRuntime) {
		t.Fatalf("refusal event %q must name omp so it is actionable", refusal.Message)
	}

	// No review row: the refusal writes none, so only the verifiable floor lands.
	for _, row := range autoTraceFeedback(t, store, version.ID) {
		if row.Reviewer == "gitmoot-review" {
			t.Fatalf("a refused review leg must write NO review row, got %+v", row)
		}
	}
	// The refusal never blocked the merge.
	if len(gate.requests) != 1 {
		t.Fatalf("merge gate requests = %d, want 1 (the review leg never blocks the merge)", len(gate.requests))
	}
}

// TestCrossFamilyReviewHonorsRuntimeOverrideAwayFromOmp pins the precedence in the
// OTHER direction, so the fix cannot degenerate into "omp always wins": an agent
// registered on omp whose job carried `--runtime claude` ran on CLAUDE, a mapped
// family, so normal cross-family selection proceeds and NOTHING refuses. Without
// the override consulted first this job would inherit the omp refusal it never
// earned, and its implement work would merge with no review at all.
func TestCrossFamilyReviewHonorsRuntimeOverrideAwayFromOmp(t *testing.T) {
	ctx := context.Background()
	store := openTraceChainStore(t)
	version := installChainTemplate(t, store, "planner")
	gate := &stubMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := chainEngine(store, gate)
	engine.OutcomeHarvester = enabledHarvesterFor(t, store, harvestGitHubStub{status: realCIChainStatus()})
	engine.ReviewLegDispatcher = reviewDispatcherForTest(store, harvestGitHubStub{})

	seedOverrideImplementJob(t, store, version, runtime.OmpRuntime, runtime.FreshRefPrefix+"omp-seat", runtime.ClaudeRuntime)
	registerShellReviewer(t, store)
	seedChainApprovingReview(t, store, "head123")

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	if refusal := reviewRefusalEvent(t, store); refusal != nil {
		t.Fatalf("a claude --runtime override must NOT inherit the omp refusal, got %q", refusal.Message)
	}
	// Selection really ran: the shell stub reviewer's rubric landed as a review row.
	reviewed := false
	for _, row := range autoTraceFeedback(t, store, version.ID) {
		if row.Reviewer == "gitmoot-review" {
			reviewed = true
		}
	}
	if !reviewed {
		t.Fatal("expected the normal cross-family review row for the overridden claude run")
	}
}
