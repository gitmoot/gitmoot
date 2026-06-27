package cli

import (
	"context"
	"fmt"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// seedAutoTraceFeedback writes n harvested feedback events of the given a/b choice
// into a version's auto-trace eval_run (#465), so the #484 regression comparator
// has read evidence for that version. A choice "a" carries the real-CI marker so
// it scores the strong-positive band.
func seedAutoTraceFeedback(t *testing.T, store *db.Store, templateID, versionID, choice string, n int) {
	t.Helper()
	ctx := context.Background()
	runID := skillopt.AutoTraceRunID(versionID)
	if err := store.UpsertEvalRun(ctx, db.EvalRun{ID: runID, TemplateID: templateID, TemplateVersionID: versionID, TargetRepo: "o/r", State: "ready"}); err != nil {
		t.Fatalf("UpsertEvalRun: %v", err)
	}
	reasoning := "changes requested"
	if choice == "a" {
		reasoning = "PR merged with passing external CI."
	}
	for i := 0; i < n; i++ {
		itemID := fmt.Sprintf("o/r#%d", i+1)
		if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{RunID: runID, ItemID: itemID, Title: itemID}); err != nil {
			t.Fatalf("UpsertEvalReviewItem: %v", err)
		}
		if err := store.UpsertFeedbackEvent(ctx, db.FeedbackEvent{
			RunID:     runID,
			ItemID:    itemID,
			Choice:    choice,
			Reasoning: reasoning,
			Reviewer:  skillopt.AutoTraceReviewer,
			Source:    skillopt.AutoTraceSource,
		}); err != nil {
			t.Fatalf("UpsertFeedbackEvent: %v", err)
		}
	}
}

// canaryHarvesterFixture installs a planner template (v1 champion current) + a
// canary v2, returning the store, the champion id, and the canary id.
func canaryHarvesterFixture(t *testing.T, sample float64) (store *db.Store, championID, canaryID string) {
	t.Helper()
	ctx := context.Background()
	store, version, _, _ := candidateNotifyFixture(t)
	champion, err := store.GetAgentTemplate(ctx, version.TemplateID)
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	canary, err := store.CanaryPromoteAgentTemplateVersion(ctx, version.ID, sample)
	if err != nil {
		t.Fatalf("CanaryPromoteAgentTemplateVersion: %v", err)
	}
	return store, champion.VersionID, canary.ID
}

// TestCanaryHarvesterGraduates proves the daemon evaluator graduates a canary at
// parity-or-better than the champion: the canary becomes current, the prior
// champion is superseded, and candidate.auto_promoted is emitted.
func TestCanaryHarvesterGraduates(t *testing.T) {
	ctx := context.Background()
	store, championID, canaryID := canaryHarvesterFixture(t, 1.0)
	seedAutoTraceFeedback(t, store, "planner", canaryID, "a", 3)    // canary all strong-positive
	seedAutoTraceFeedback(t, store, "planner", championID, "a", 3)  // champion strong-positive too
	sink := &recordingSink{}
	h := &canaryRegressionHarvester{store: store, sink: sink, minSamples: floatPtrCLIInt(3)}

	h.evaluate(ctx, workflow.JobPayload{TemplateID: "planner"})

	if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 1 {
		t.Fatalf("auto_promoted emits = %d, want 1 (graduate)", got)
	}
	graduated, err := store.GetAgentTemplateVersionByID(ctx, canaryID)
	if err != nil {
		t.Fatalf("get canary: %v", err)
	}
	if graduated.State != "current" {
		t.Fatalf("graduated state = %q, want current", graduated.State)
	}
}

// TestCanaryHarvesterRollsBack proves the daemon evaluator rolls back a canary
// that materially regresses vs the champion: the champion STAYS the live current
// version, the canary is rejected, and candidate.rolled_back is emitted.
func TestCanaryHarvesterRollsBack(t *testing.T) {
	ctx := context.Background()
	store, championID, canaryID := canaryHarvesterFixture(t, 1.0)
	seedAutoTraceFeedback(t, store, "planner", canaryID, "b", 4)   // canary all negative
	seedAutoTraceFeedback(t, store, "planner", championID, "a", 4) // champion all strong-positive
	sink := &recordingSink{}
	h := &canaryRegressionHarvester{store: store, sink: sink, minSamples: floatPtrCLIInt(3)}

	h.evaluate(ctx, workflow.JobPayload{TemplateID: "planner"})

	if got := len(sink.byType(events.EventCandidateRolledBack)); got != 1 {
		t.Fatalf("rolled_back emits = %d, want 1", got)
	}
	rejected, err := store.GetAgentTemplateVersionByID(ctx, canaryID)
	if err != nil {
		t.Fatalf("get canary: %v", err)
	}
	if rejected.State != "rejected" {
		t.Fatalf("rolled-back canary state = %q, want rejected", rejected.State)
	}
	// The champion is still the live current version — never left without one.
	tmpl, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if tmpl.VersionID != championID {
		t.Fatalf("current version = %q, want champion %q", tmpl.VersionID, championID)
	}
}

// TestCanaryHarvesterHoldsOnThinSamples proves the daemon evaluator HOLDS (no
// graduate, no rollback, no event, canary stays canary) when the canary has fewer
// than minSamples outcomes — the insufficient-data fail-safe.
func TestCanaryHarvesterHoldsOnThinSamples(t *testing.T) {
	ctx := context.Background()
	store, championID, canaryID := canaryHarvesterFixture(t, 1.0)
	seedAutoTraceFeedback(t, store, "planner", canaryID, "b", 1)   // only 1 canary sample
	seedAutoTraceFeedback(t, store, "planner", championID, "a", 5) // ample champion
	sink := &recordingSink{}
	h := &canaryRegressionHarvester{store: store, sink: sink, minSamples: floatPtrCLIInt(3)}

	h.evaluate(ctx, workflow.JobPayload{TemplateID: "planner"})

	if sink.count() != 0 {
		t.Fatalf("thin samples must emit nothing, got %d", sink.count())
	}
	held, err := store.GetAgentTemplateVersionByID(ctx, canaryID)
	if err != nil {
		t.Fatalf("get canary: %v", err)
	}
	if held.State != "canary" {
		t.Fatalf("held canary state = %q, want canary (still sampling)", held.State)
	}
}

// TestRunCandidateNotifyCanaryRoutesToCanary proves a canary-mode guardrails-pass
// promotes the candidate to the `canary` state (NOT current), emits
// candidate.canary_started (not auto_promoted), and leaves the prior champion as
// the live current version.
func TestRunCandidateNotifyCanaryRoutesToCanary(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)
	autoPromoteCandidateScore(&candidate, 0.96)
	sink := &recordingSink{}

	championBefore, err := store.GetAgentTemplate(ctx, version.TemplateID)
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}

	policy := autoPromotePolicy(1, 0.9)
	policy.AutoPromoteCanary = true
	policy.AutoPromoteCanarySample = floatPtrCLI(0.1)

	if err := runCandidateNotify(ctx, store, sink, policy, candidate, version, []db.FeedbackEvent{realCIFeedbackEvent()}, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify returned error: %v", err)
	}

	// candidate.canary_started fires exactly once; NO auto_promoted.
	started := sink.byType(events.EventCandidateCanaryStarted)
	if len(started) != 1 {
		t.Fatalf("canary_started emits = %d, want 1", len(started))
	}
	if started[0].JobID != version.ID || started[0].RootID != version.TemplateID {
		t.Fatalf("canary_started ids = %q/%q, want %q/%q", started[0].JobID, started[0].RootID, version.ID, version.TemplateID)
	}
	if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 0 {
		t.Fatalf("auto_promoted emits = %d, want 0 (canary, not direct promote)", got)
	}
	// The candidate is now a canary; the champion is unchanged and still current.
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "canary" {
		t.Fatalf("version state = %q, want canary", after.State)
	}
	if after.CanarySample != 0.1 {
		t.Fatalf("canary sample = %v, want 0.1", after.CanarySample)
	}
	championAfter, err := store.GetAgentTemplate(ctx, version.TemplateID)
	if err != nil {
		t.Fatalf("GetAgentTemplate after returned error: %v", err)
	}
	if championAfter.VersionID != championBefore.VersionID {
		t.Fatalf("current version changed under canary: %q -> %q", championBefore.VersionID, championAfter.VersionID)
	}
}

// TestRunCandidateNotifyCanaryOffByDefaultDirectPromote proves that with
// auto_promote ON but canary OFF the path is byte-identical to #471: a direct
// promote to current with candidate.auto_promoted (NO canary_started).
func TestRunCandidateNotifyCanaryOffByDefaultDirectPromote(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)
	autoPromoteCandidateScore(&candidate, 0.96)
	sink := &recordingSink{}

	if err := runCandidateNotify(ctx, store, sink, autoPromotePolicy(1, 0.9), candidate, version, []db.FeedbackEvent{realCIFeedbackEvent()}, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify returned error: %v", err)
	}
	if got := len(sink.byType(events.EventCandidateCanaryStarted)); got != 0 {
		t.Fatalf("canary_started emits = %d, want 0 (canary off)", got)
	}
	if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 1 {
		t.Fatalf("auto_promoted emits = %d, want 1 (direct promote)", got)
	}
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "current" {
		t.Fatalf("version state = %q, want current (direct promote)", after.State)
	}
}

// TestRunCandidateNotifyCanaryWithoutSampleStaysPending proves auto_promote_canary
// set WITHOUT a sample fails safe to notify-only: no canary, no promote, the
// candidate stays pending.
func TestRunCandidateNotifyCanaryWithoutSampleStaysPending(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)
	autoPromoteCandidateScore(&candidate, 0.96)
	sink := &recordingSink{}

	policy := autoPromotePolicy(1, 0.9)
	policy.AutoPromoteCanary = true // no AutoPromoteCanarySample => misconfigured

	if err := runCandidateNotify(ctx, store, sink, policy, candidate, version, []db.FeedbackEvent{realCIFeedbackEvent()}, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify returned error: %v", err)
	}
	if got := len(sink.byType(events.EventCandidateCanaryStarted)); got != 0 {
		t.Fatalf("canary_started emits = %d, want 0 (misconfigured canary)", got)
	}
	if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 0 {
		t.Fatalf("auto_promoted emits = %d, want 0 (fail-safe notify-only)", got)
	}
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "pending" {
		t.Fatalf("version state = %q, want pending (notify-only fail-safe)", after.State)
	}
}
