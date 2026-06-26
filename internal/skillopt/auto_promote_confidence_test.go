package skillopt

import (
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

// TestEvaluateAutoPromoteConfidenceGuardrail covers the additive #473 Mode B
// min_confidence guardrail: nil floor leaves #471 behavior untouched (confidence
// ignored, even when present), a set floor with a nil/low/thin confidence fails
// safe to notify-only, and a set floor met (with enough samples) passes and names
// the confidence in the reason.
func TestEvaluateAutoPromoteConfidenceGuardrail(t *testing.T) {
	basePolicy := func() config.SkillOptPolicy {
		return config.SkillOptPolicy{
			AutoPromote:                  true,
			AutoPromoteMinSamples:        intPtr(1),
			AutoPromoteMinScore:          floatPtr(0.9),
			AutoPromoteRequireExternalCI: true,
		}
	}
	// Enough feedback events to clear the largest AutoPromoteMinSamples floor used
	// below (30): the FEEDBACK-side sample guardrail and the bandit confidenceSamples
	// guardrail both read AutoPromoteMinSamples, so the feedback slice must be at
	// least as large as the floor for the confidence cases to be the deciding gate.
	feedback := make([]db.FeedbackEvent, 0, 40)
	for i := 0; i < 40; i++ {
		feedback = append(feedback, realCIFeedback())
	}

	cases := []struct {
		name              string
		minConfidence     *float64
		minSamples        *int
		confidence        *float64
		confidenceSamples int
		wantPromote       bool
		reasonHas         string
	}{
		{
			name:          "nil floor ignores confidence (471 unchanged)",
			minConfidence: nil,
			confidence:    floatPtr(0.1), // a low confidence is IGNORED when the floor is unset
			wantPromote:   true,
			reasonHas:     "external CI",
		},
		{
			name:          "nil floor with nil confidence still promotes",
			minConfidence: nil,
			confidence:    nil,
			wantPromote:   true,
			reasonHas:     "score",
		},
		{
			name:              "floor set, confidence nil -> notify only",
			minConfidence:     floatPtr(0.95),
			confidence:        nil,
			confidenceSamples: 0,
			wantPromote:       false,
			reasonHas:         "no bandit confidence",
		},
		{
			name:              "floor set, confidence below floor -> notify only",
			minConfidence:     floatPtr(0.95),
			minSamples:        intPtr(10),
			confidence:        floatPtr(0.80),
			confidenceSamples: 50,
			wantPromote:       false,
			reasonHas:         "below auto_promote_min_confidence",
		},
		{
			name:              "floor met but too few samples -> notify only",
			minConfidence:     floatPtr(0.95),
			minSamples:        intPtr(30),
			confidence:        floatPtr(0.99),
			confidenceSamples: 5,
			wantPromote:       false,
			reasonHas:         "below auto_promote_min_samples",
		},
		{
			name:              "floor met with enough samples -> promote",
			minConfidence:     floatPtr(0.95),
			minSamples:        intPtr(30),
			confidence:        floatPtr(0.97),
			confidenceSamples: 80,
			wantPromote:       true,
			reasonHas:         "confidence 97%",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := basePolicy()
			policy.AutoPromoteMinConfidence = tc.minConfidence
			if tc.minSamples != nil {
				policy.AutoPromoteMinSamples = tc.minSamples
			}
			decision := EvaluateAutoPromote(policy, candidateWithScore(0.96), feedback, false, tc.confidence, tc.confidenceSamples)
			if decision.Promote != tc.wantPromote {
				t.Fatalf("Promote = %v, want %v (reason: %q)", decision.Promote, tc.wantPromote, decision.Reason)
			}
			if tc.reasonHas != "" && !strings.Contains(decision.Reason, tc.reasonHas) {
				t.Fatalf("reason %q does not contain %q", decision.Reason, tc.reasonHas)
			}
		})
	}
}
