package skillopt

import (
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

// realCIFeedback is a feedback event the harvester would write for a merge that
// passed genuine external CI (choice "a", the real-CI marker phrase, AND the
// harvester provenance the hardened predicate now requires).
func realCIFeedback() db.FeedbackEvent {
	return db.FeedbackEvent{Choice: "a", Reviewer: autoTraceReviewer, Source: autoTraceSource, Reasoning: "PR #7 merged with " + realExternalCIPhrase + "."}
}

// noCIFeedback is a feedback event for a near-neutral empty-gate merge (choice
// "a" but NO external CI) — it must NOT count as a real-CI positive.
func noCIFeedback() db.FeedbackEvent {
	return db.FeedbackEvent{Choice: "a", Reviewer: autoTraceReviewer, Source: autoTraceSource, Reasoning: "PR #7 merged through an empty gate (no external CI); near-neutral, not a strong positive."}
}

func candidateWithScore(score float64) CandidatePackage {
	return CandidatePackage{Summary: CandidateSummary{Score: &score}}
}

func TestEvaluateAutoPromote(t *testing.T) {
	cases := []struct {
		name                string
		policy              config.SkillOptPolicy
		candidate           CandidatePackage
		feedback            []db.FeedbackEvent
		feedbackUnavailable bool
		confidence          *float64
		confidenceSamples   int
		wantPromote         bool
		reasonHas           string
	}{
		{
			name:        "off by default never promotes",
			policy:      config.SkillOptPolicy{AutoPromote: false, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: floatPtr(0.5)},
			candidate:   candidateWithScore(0.99),
			feedback:    []db.FeedbackEvent{realCIFeedback()},
			wantPromote: false,
			reasonHas:   "auto_promote is off",
		},
		{
			name:        "all guardrails pass promotes",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: floatPtr(0.9), AutoPromoteRequireExternalCI: true},
			candidate:   candidateWithScore(0.96),
			feedback:    []db.FeedbackEvent{realCIFeedback()},
			wantPromote: true,
			reasonHas:   "external CI",
		},
		{
			name:        "all guardrails pass reason mentions score and samples",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: floatPtr(0.9), AutoPromoteRequireExternalCI: true},
			candidate:   candidateWithScore(0.96),
			feedback:    []db.FeedbackEvent{realCIFeedback(), realCIFeedback()},
			wantPromote: true,
			reasonHas:   "samples",
		},
		{
			name:        "nil score fails safe",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: floatPtr(0.5)},
			candidate:   CandidatePackage{Summary: CandidateSummary{Score: nil}},
			feedback:    []db.FeedbackEvent{realCIFeedback()},
			wantPromote: false,
			reasonHas:   "no score",
		},
		{
			name:        "samples below min no promote",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(3), AutoPromoteMinScore: floatPtr(0.5)},
			candidate:   candidateWithScore(0.96),
			feedback:    []db.FeedbackEvent{realCIFeedback()},
			wantPromote: false,
			reasonHas:   "below auto_promote_min_samples",
		},
		{
			name:        "unset min_samples is hard do-not-promote not zero",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: nil, AutoPromoteMinScore: floatPtr(0.5)},
			candidate:   candidateWithScore(0.96),
			feedback:    []db.FeedbackEvent{realCIFeedback()},
			wantPromote: false,
			reasonHas:   "auto_promote_min_samples is not set",
		},
		{
			// An explicit min_samples=0 is legal, but the absolute zero-evidence floor
			// must still reject a candidate with no feedback (0 < 0 == false would have
			// let it through before the fix).
			name:        "explicit min_samples zero still rejects zero evidence",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(0), AutoPromoteMinScore: floatPtr(0.5)},
			candidate:   candidateWithScore(0.96),
			feedback:    nil,
			wantPromote: false,
			reasonHas:   "zero evidence",
		},
		{
			// A read error / unresolvable eval_run is uncertainty, not "zero samples":
			// it fails safe even with an otherwise-satisfiable min_samples=0.
			name:                "feedback unavailable fails safe even at min_samples zero",
			policy:              config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(0), AutoPromoteMinScore: floatPtr(0.5)},
			candidate:           candidateWithScore(0.96),
			feedback:            nil,
			feedbackUnavailable: true,
			wantPromote:         false,
			reasonHas:           "feedback is unavailable",
		},
		{
			name:        "unset min_score is hard do-not-promote",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: nil},
			candidate:   candidateWithScore(0.96),
			feedback:    []db.FeedbackEvent{realCIFeedback()},
			wantPromote: false,
			reasonHas:   "auto_promote_min_score is not set",
		},
		{
			name:        "score below min no promote",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: floatPtr(0.9)},
			candidate:   candidateWithScore(0.5),
			feedback:    []db.FeedbackEvent{realCIFeedback()},
			wantPromote: false,
			reasonHas:   "below auto_promote_min_score",
		},
		{
			name:        "require external ci but only near-neutral no promote",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: floatPtr(0.5), AutoPromoteRequireExternalCI: true},
			candidate:   candidateWithScore(0.96),
			feedback:    []db.FeedbackEvent{noCIFeedback()},
			wantPromote: false,
			reasonHas:   "no eval_run feedback recorded a real external-CI pass",
		},
		{
			name:        "external ci satisfied by one real positive among near-neutral",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: floatPtr(0.5), AutoPromoteRequireExternalCI: true},
			candidate:   candidateWithScore(0.96),
			feedback:    []db.FeedbackEvent{noCIFeedback(), realCIFeedback()},
			wantPromote: true,
			reasonHas:   "external CI confirmed",
		},
		{
			name:        "require measured judge fails safe deferred",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: floatPtr(0.5), AutoPromoteRequireMeasuredJudge: true},
			candidate:   candidateWithScore(0.96),
			feedback:    []db.FeedbackEvent{realCIFeedback()},
			wantPromote: false,
			reasonHas:   "require_measured_judge",
		},
		{
			name:        "canary fails safe deferred",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: floatPtr(0.5), AutoPromoteCanary: true},
			candidate:   candidateWithScore(0.96),
			feedback:    []db.FeedbackEvent{realCIFeedback()},
			wantPromote: false,
			reasonHas:   "canary",
		},
		{
			// MODE B (#473): a GENUINE ask-agent candidate has NO harvester feedback rows
			// and NO score. Without the Mode B confidence path the empty-feedback
			// zero-evidence floor (or the nil-score hard stop) would make this candidate
			// structurally unreachable by the confidence gate. With a set min_confidence,
			// a present confidence, and enough bandit pulls, the bandit evidence stands in
			// for the sample/score floors and it promotes.
			name:              "mode B confidence-backed promotes with no feedback and no score",
			policy:            config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(30), AutoPromoteMinScore: floatPtr(0.5), AutoPromoteMinConfidence: floatPtr(0.95)},
			candidate:         CandidatePackage{Summary: CandidateSummary{Score: nil}},
			feedback:          nil,
			confidence:        floatPtr(0.97),
			confidenceSamples: 80,
			wantPromote:       true,
			reasonHas:         "confidence 97%",
		},
		{
			// Mode B thin evidence: a high confidence but FEWER bandit pulls than the
			// sample floor must NOT promote — small-sample over-confidence is exactly the
			// tiering failure the floor exists to block. It also must not fall back to the
			// Mode A zero-evidence path (that would still refuse, but for the wrong reason).
			name:              "mode B confidence below sample floor stays pending",
			policy:            config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(30), AutoPromoteMinScore: floatPtr(0.5), AutoPromoteMinConfidence: floatPtr(0.95)},
			candidate:         CandidatePackage{Summary: CandidateSummary{Score: nil}},
			feedback:          nil,
			confidence:        floatPtr(0.99),
			confidenceSamples: 5,
			wantPromote:       false,
			reasonHas:         "below auto_promote_min_samples",
		},
		{
			// Mode B with a confidence below the floor fails safe even though the pull
			// count clears the sample floor.
			name:              "mode B confidence below floor stays pending",
			policy:            config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(30), AutoPromoteMinScore: floatPtr(0.5), AutoPromoteMinConfidence: floatPtr(0.95)},
			candidate:         CandidatePackage{Summary: CandidateSummary{Score: nil}},
			feedback:          nil,
			confidence:        floatPtr(0.60),
			confidenceSamples: 80,
			wantPromote:       false,
			reasonHas:         "below auto_promote_min_confidence",
		},
		{
			// A nil score with NO bandit confidence (min_confidence unset) is still the
			// Mode A hard stop — the score floor is not waived without bandit evidence.
			name:        "nil score without confidence is still a hard stop",
			policy:      config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(1), AutoPromoteMinScore: floatPtr(0.5)},
			candidate:   CandidatePackage{Summary: CandidateSummary{Score: nil}},
			feedback:    []db.FeedbackEvent{realCIFeedback()},
			wantPromote: false,
			reasonHas:   "no score",
		},
		{
			// Mode B path still honors a PRESENT score that is below the floor: the bandit
			// stands in only for an ABSENT score, never to override a real failing one.
			name:              "mode B with present failing score stays pending",
			policy:            config.SkillOptPolicy{AutoPromote: true, AutoPromoteMinSamples: intPtr(30), AutoPromoteMinScore: floatPtr(0.9), AutoPromoteMinConfidence: floatPtr(0.95)},
			candidate:         candidateWithScore(0.4),
			feedback:          nil,
			confidence:        floatPtr(0.99),
			confidenceSamples: 80,
			wantPromote:       false,
			reasonHas:         "below auto_promote_min_score",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := EvaluateAutoPromote(tc.policy, tc.candidate, tc.feedback, tc.feedbackUnavailable, tc.confidence, tc.confidenceSamples)
			if decision.Promote != tc.wantPromote {
				t.Fatalf("Promote = %v, want %v (reason: %q)", decision.Promote, tc.wantPromote, decision.Reason)
			}
			if tc.reasonHas != "" && !strings.Contains(decision.Reason, tc.reasonHas) {
				t.Fatalf("reason %q does not contain %q", decision.Reason, tc.reasonHas)
			}
		})
	}
}

func TestFeedbackEventIsRealExternalCIPositive(t *testing.T) {
	if !FeedbackEventIsRealExternalCIPositive(realCIFeedback()) {
		t.Fatalf("real-CI positive should be detected")
	}
	if FeedbackEventIsRealExternalCIPositive(noCIFeedback()) {
		t.Fatalf("near-neutral empty-gate merge must NOT count as real CI")
	}
	// A negative (choice "b") carrying the phrase still must not count.
	if FeedbackEventIsRealExternalCIPositive(db.FeedbackEvent{Choice: "b", Reviewer: autoTraceReviewer, Source: autoTraceSource, Reasoning: realExternalCIPhrase}) {
		t.Fatalf("a negative choice must never be a real-CI positive")
	}
	// Provenance guard: the marker phrase WITHOUT the harvester's reviewer/source
	// (e.g. a raw human-typed review row) must NOT count — only the harvester writes
	// the real-CI marker.
	if FeedbackEventIsRealExternalCIPositive(db.FeedbackEvent{Choice: "a", Reasoning: realExternalCIPhrase}) {
		t.Fatalf("the marker without harvester provenance must NOT count as real CI")
	}
	// Spoof guard: a cross-family review row shares the auto-trace source but carries
	// the DISTINCT gitmoot-review reviewer and FREE-TEXT findings; even if its prose
	// mentions the phrase it must NEVER satisfy the real-CI gate.
	spoof := db.FeedbackEvent{Choice: "a", Reviewer: reviewReviewerID, Source: autoTraceSource, Reasoning: "merged after " + realExternalCIPhrase}
	if FeedbackEventIsRealExternalCIPositive(spoof) {
		t.Fatalf("a cross-family review row must never spoof the real-CI marker")
	}
}
