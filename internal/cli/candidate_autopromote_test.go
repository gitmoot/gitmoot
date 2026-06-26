package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

func autoPromoteCandidateScore(candidate *skillopt.CandidatePackage, score float64) {
	candidate.Summary.Score = &score
}

func autoPromotePolicy(minSamples int, minScore float64) config.SkillOptPolicy {
	policy := config.DefaultSkillOptPolicy()
	policy.AutoPromote = true
	policy.AutoPromoteMinSamples = &minSamples
	policy.AutoPromoteMinScore = &minScore
	policy.AutoPromoteRequireExternalCI = true
	return policy
}

// realCIFeedbackEvent mirrors a harvested real-CI positive: choice "a" + the
// shared marker phrase AND the harvester provenance (reviewer/source) the hardened
// external-CI guardrail now requires so a human-typed review row can never spoof it.
func realCIFeedbackEvent() db.FeedbackEvent {
	return db.FeedbackEvent{
		Choice:    "a",
		Reviewer:  skillopt.AutoTraceReviewer,
		Source:    skillopt.AutoTraceSource,
		Reasoning: "PR #7 merged with passing external CI.",
	}
}

// TestRunCandidateNotifyAutoPromoteOffStaysPending proves auto_promote=false (the
// default) leaves the candidate pending: only candidate.awaiting_promotion is
// emitted, no candidate.auto_promoted, and PromoteAgentTemplateVersion is not run.
func TestRunCandidateNotifyAutoPromoteOffStaysPending(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)
	autoPromoteCandidateScore(&candidate, 0.99)
	sink := &recordingSink{}

	if err := runCandidateNotify(ctx, store, sink, config.DefaultSkillOptPolicy(), candidate, version, []db.FeedbackEvent{realCIFeedbackEvent()}, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify returned error: %v", err)
	}

	if got := len(sink.byType(events.EventCandidateAwaitingPromotion)); got != 1 {
		t.Fatalf("awaiting_promotion emits = %d, want 1", got)
	}
	if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 0 {
		t.Fatalf("auto_promoted emits = %d, want 0", got)
	}
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "pending" {
		t.Fatalf("version state = %q, want pending", after.State)
	}
}

// TestRunCandidateNotifyAutoPromoteGuardrailsPass proves auto_promote=true with all
// guardrails satisfied promotes the version to current AND emits both
// candidate.awaiting_promotion and candidate.auto_promoted.
func TestRunCandidateNotifyAutoPromoteGuardrailsPass(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)
	autoPromoteCandidateScore(&candidate, 0.96)
	sink := &recordingSink{}

	if err := runCandidateNotify(ctx, store, sink, autoPromotePolicy(1, 0.9), candidate, version, []db.FeedbackEvent{realCIFeedbackEvent()}, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify returned error: %v", err)
	}

	if got := len(sink.byType(events.EventCandidateAwaitingPromotion)); got != 1 {
		t.Fatalf("awaiting_promotion emits = %d, want 1", got)
	}
	promoted := sink.byType(events.EventCandidateAutoPromoted)
	if len(promoted) != 1 {
		t.Fatalf("auto_promoted emits = %d, want 1", len(promoted))
	}
	if promoted[0].JobID != version.ID || promoted[0].RootID != version.TemplateID {
		t.Fatalf("auto_promoted ids = %q/%q, want %q/%q", promoted[0].JobID, promoted[0].RootID, version.ID, version.TemplateID)
	}
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "current" {
		t.Fatalf("version state = %q, want current (auto-promoted)", after.State)
	}
}

// TestRunCandidateNotifyAutoPromoteGuardrailFailStaysPending proves auto_promote=true
// with a FAILING guardrail (here: no real external-CI feedback) does NOT promote —
// only candidate.awaiting_promotion fires and the version stays pending.
func TestRunCandidateNotifyAutoPromoteGuardrailFailStaysPending(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)
	autoPromoteCandidateScore(&candidate, 0.96)
	sink := &recordingSink{}

	// A near-neutral (no external CI) feedback event fails require_external_ci.
	nearNeutral := db.FeedbackEvent{Choice: "a", Reasoning: "PR #7 merged through an empty gate (no external CI); near-neutral, not a strong positive."}
	if err := runCandidateNotify(ctx, store, sink, autoPromotePolicy(1, 0.9), candidate, version, []db.FeedbackEvent{nearNeutral}, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify returned error: %v", err)
	}

	if got := len(sink.byType(events.EventCandidateAwaitingPromotion)); got != 1 {
		t.Fatalf("awaiting_promotion emits = %d, want 1", got)
	}
	if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 0 {
		t.Fatalf("auto_promoted emits = %d, want 0 (guardrail failed)", got)
	}
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "pending" {
		t.Fatalf("version state = %q, want pending (no promotion)", after.State)
	}
}

// floatPtrCLI is a local pointer helper for the Mode B confidence cases.
func floatPtrCLI(v float64) *float64 { return &v }

// TestRunCandidateNotifyBanditConfidencePromotes proves the #473 Mode B
// confidence reaches EvaluateAutoPromote AND the awaiting_promotion Detail: with
// a confidence above auto_promote_min_confidence and enough samples, the
// candidate auto-promotes and the awaiting detail carries the "NN% likely better
// over K samples" string.
func TestRunCandidateNotifyBanditConfidencePromotes(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)
	autoPromoteCandidateScore(&candidate, 0.96)
	sink := &recordingSink{}

	policy := autoPromotePolicy(1, 0.9)
	policy.AutoPromoteMinConfidence = floatPtrCLI(0.95)

	confidence := 0.97
	summary := skillopt.ConfidenceSummary(confidence, 80)
	if err := runCandidateNotify(ctx, store, sink, policy, candidate, version, []db.FeedbackEvent{realCIFeedbackEvent()}, false, &confidence, 80, summary); err != nil {
		t.Fatalf("runCandidateNotify returned error: %v", err)
	}

	awaiting := sink.byType(events.EventCandidateAwaitingPromotion)
	if len(awaiting) != 1 {
		t.Fatalf("awaiting_promotion emits = %d, want 1", len(awaiting))
	}
	if !strings.Contains(awaiting[0].Detail, "97% likely better over 80 samples") {
		t.Fatalf("awaiting detail missing confidence string: %q", awaiting[0].Detail)
	}
	if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 1 {
		t.Fatalf("auto_promoted emits = %d, want 1", got)
	}
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "current" {
		t.Fatalf("version state = %q, want current (auto-promoted on confidence)", after.State)
	}
}

// TestRunCandidateNotifyBanditConfidenceBelowFloorStaysPending proves a set
// confidence floor that is NOT met fails safe to notify-only — the candidate
// stays pending even though the other guardrails pass.
func TestRunCandidateNotifyBanditConfidenceBelowFloorStaysPending(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)
	autoPromoteCandidateScore(&candidate, 0.96)
	sink := &recordingSink{}

	policy := autoPromotePolicy(1, 0.9)
	policy.AutoPromoteMinConfidence = floatPtrCLI(0.95)

	confidence := 0.60
	if err := runCandidateNotify(ctx, store, sink, policy, candidate, version, []db.FeedbackEvent{realCIFeedbackEvent()}, false, &confidence, 80, skillopt.ConfidenceSummary(confidence, 80)); err != nil {
		t.Fatalf("runCandidateNotify returned error: %v", err)
	}
	if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 0 {
		t.Fatalf("auto_promoted emits = %d, want 0 (confidence below floor)", got)
	}
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "pending" {
		t.Fatalf("version state = %q, want pending", after.State)
	}
}

// TestRunCandidateNotifyNoBanditConfidenceUnchanged proves byte-identical #471
// behavior when Mode B was never exercised: nil confidence + empty summary +
// nil floor means the awaiting detail carries no confidence string and the
// auto-promote decision is the same as before #473.
func TestRunCandidateNotifyNoBanditConfidenceUnchanged(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)
	autoPromoteCandidateScore(&candidate, 0.96)
	sink := &recordingSink{}

	// No min_confidence floor; nil confidence; empty summary.
	if err := runCandidateNotify(ctx, store, sink, autoPromotePolicy(1, 0.9), candidate, version, []db.FeedbackEvent{realCIFeedbackEvent()}, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify returned error: %v", err)
	}
	awaiting := sink.byType(events.EventCandidateAwaitingPromotion)
	if len(awaiting) != 1 {
		t.Fatalf("awaiting_promotion emits = %d, want 1", len(awaiting))
	}
	if strings.Contains(awaiting[0].Detail, "likely better over") {
		t.Fatalf("awaiting detail should carry NO confidence string when Mode B is unused: %q", awaiting[0].Detail)
	}
	// The other guardrails still pass, so it auto-promotes exactly as #471 did.
	if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 1 {
		t.Fatalf("auto_promoted emits = %d, want 1 (471 behavior unchanged)", got)
	}
}
