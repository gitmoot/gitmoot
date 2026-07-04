package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// This file adds the CROSS-FEATURE leg the #635 gate tests do not cover: the
// verdict produced by the ACTUAL `gitmoot skillopt gate run` command surface
// (driven through Run, the top-level dispatcher — arg routing included) is then
// CONSUMED by the real promotion guard (runCandidateNotify's #627 gate check).
//
// Already covered by #635 (NOT re-proved here): TestSkillOptGateRunRejects/
// AcceptsCandidate (accept/reject → persisted gate run via runSkillOptGateRun
// directly), TestSkillOptGateReplayDeterminism, and TestGatePromotionGuardBlocks-
// ThenAllows — but that last one hand-INSERTS the accepted gate run into the DB.
// The added value below is that the accepted/rejected row is PRODUCED by the gate
// CLI end to end and then drives the guard, including the case #635 never
// exercises: a persisted-but-REJECTED run must still block promotion.

// gateGuardPolicy builds a [skillopt] policy whose auto-promote guardrails PASS
// (so runCandidateNotify reaches the #627 gate check) with the replay gate ON.
// Mirrors the setup in TestGatePromotionGuardBlocksThenAllows.
func gateGuardPolicy() config.SkillOptPolicy {
	minSamples := 1
	minScore := 0.5
	policy := config.DefaultSkillOptPolicy()
	policy.AutoPromote = true
	policy.AutoPromoteMinSamples = &minSamples
	policy.AutoPromoteMinScore = &minScore
	policy.Gate = true
	return policy
}

// gateGuardInputs are the passing-guardrails candidate + feedback the guard needs.
func gateGuardInputs() (skillopt.CandidatePackage, []db.FeedbackEvent) {
	score := 0.82
	return skillopt.CandidatePackage{Summary: skillopt.CandidateSummary{Score: &score}},
		[]db.FeedbackEvent{{Choice: "a"}}
}

// runGateRunCLI drives the REAL `gitmoot skillopt gate run` through the top-level
// Run dispatcher and returns its exit code.
func runGateRunCLI(t *testing.T, home, candidateID, corpusPath string) int {
	t.Helper()
	var out, errBuf bytes.Buffer
	return Run([]string{"skillopt", "gate", "run", "--home", home, "--candidate", candidateID, "--corpus", corpusPath, "--json"}, &out, &errBuf)
}

// TestGateRejectedRunViaCLIStillBlocksPromotionE2E is the leg #635 does NOT cover:
// a candidate that the REAL gate CLI REJECTS has a persisted gate run, but that
// rejected run does NOT satisfy the promotion guard — the guard still blocks
// (gate_blocked, candidate stays pending). #635's block case has ZERO gate runs;
// this proves a persisted-but-rejected run is not mistaken for a passing one.
//
// MUTATION: inverting strictlyImproves in internal/skillopt/gate.go accepts the
// worse candidate, so the CLI would persist an ACCEPTED run and the guard would
// promote — flipping the pending/gate_blocked assertions RED.
func TestGateRejectedRunViaCLIStillBlocksPromotionE2E(t *testing.T) {
	ctx := context.Background()
	store, home, version := seedGateTemplate(t, 3 /*champion*/, 1 /*candidate is worse*/)
	corpusPath := writeGateCorpus(t, home)
	withGateReplayRunner(t, &markerScoringRunner{marker: "STRONG"})

	// The REAL gate CLI rejects the worse candidate (exit 1) and persists the run.
	if code := runGateRunCLI(t, home, version.ID, corpusPath); code != 1 {
		t.Fatalf("gate run CLI exit = %d, want 1 (reject)", code)
	}
	accepted, err := store.HasAcceptedSkillOptGateRun(ctx, version.ID)
	if err != nil {
		t.Fatalf("HasAcceptedSkillOptGateRun: %v", err)
	}
	if accepted {
		t.Fatalf("a rejected gate run must not count as accepted")
	}
	runs, err := store.ListSkillOptGateRuns(ctx, version.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected exactly 1 persisted gate run, got %d (err=%v)", len(runs), err)
	}

	// The promotion guard consumes that persisted-but-rejected run: guardrails pass,
	// gate enabled, but no ACCEPTED run → BLOCKED. Candidate stays pending.
	policy := gateGuardPolicy()
	candidate, feedback := gateGuardInputs()
	sink := &recordingSink{}
	if err := runCandidateNotify(ctx, store, sink, policy, candidate, version, feedback, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify: %v", err)
	}
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID: %v", err)
	}
	if after.State != "pending" {
		t.Fatalf("candidate state = %q, want pending (rejected gate run must not unblock promotion)", after.State)
	}
	if !hasGateBlockedEvent(sink) {
		t.Fatalf("expected a gate_blocked awaiting event; got %+v", sink.byType(events.EventCandidateAwaitingPromotion))
	}
	if len(sink.byType(events.EventCandidateAutoPromoted)) != 0 {
		t.Fatalf("a rejected candidate must not emit auto_promoted")
	}
}

// TestGateAcceptedRunViaCLIUnblocksPromotionE2E is the full cross-feature chain:
// the accepted verdict from the REAL gate CLI (not a hand-inserted row) unblocks
// the REAL promotion guard, which promotes the candidate to current. Together with
// the rejected-blocks test above this proves the gate CLI's persisted output is
// what drives promotion, end to end.
func TestGateAcceptedRunViaCLIUnblocksPromotionE2E(t *testing.T) {
	ctx := context.Background()
	store, home, version := seedGateTemplate(t, 1 /*champion*/, 3 /*candidate is better*/)
	corpusPath := writeGateCorpus(t, home)
	withGateReplayRunner(t, &markerScoringRunner{marker: "STRONG"})

	policy := gateGuardPolicy()
	candidate, feedback := gateGuardInputs()

	// (1) BEFORE any gate run: guardrails pass + gate enabled → BLOCKED (no accepted
	// run yet). This mirrors #635's block leg but as the baseline for the unblock.
	sink1 := &recordingSink{}
	if err := runCandidateNotify(ctx, store, sink1, policy, candidate, version, feedback, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify (pre-gate): %v", err)
	}
	if pre, _ := store.GetAgentTemplateVersionByID(ctx, version.ID); pre.State != "pending" {
		t.Fatalf("candidate state before gate run = %q, want pending", pre.State)
	}
	if !hasGateBlockedEvent(sink1) {
		t.Fatalf("expected gate_blocked before any gate run")
	}

	// (2) The REAL gate CLI accepts the better candidate (exit 0) and persists an
	// ACCEPTED run.
	if code := runGateRunCLI(t, home, version.ID, corpusPath); code != 0 {
		t.Fatalf("gate run CLI exit = %d, want 0 (accept)", code)
	}
	accepted, err := store.HasAcceptedSkillOptGateRun(ctx, version.ID)
	if err != nil || !accepted {
		t.Fatalf("expected an accepted gate run on record (accepted=%v err=%v)", accepted, err)
	}

	// (3) The SAME promotion guard now promotes the candidate to current, consuming
	// the CLI-produced accepted run.
	sink2 := &recordingSink{}
	if err := runCandidateNotify(ctx, store, sink2, policy, candidate, version, feedback, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify (post-gate): %v", err)
	}
	promoted, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID: %v", err)
	}
	if promoted.State != "current" {
		t.Fatalf("candidate state = %q, want current (accepted gate run should unblock promotion)", promoted.State)
	}
	if len(sink2.byType(events.EventCandidateAutoPromoted)) != 1 {
		t.Fatalf("expected exactly one auto_promoted emit, got %d", len(sink2.byType(events.EventCandidateAutoPromoted)))
	}
	if hasGateBlockedEvent(sink2) {
		t.Fatalf("must not emit gate_blocked once an accepted gate run exists")
	}
}
