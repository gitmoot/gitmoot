package skillopt

import (
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// negFeedback is a harvested negative outcome (choice "b"): a changes-requested
// or reverted result, the lowest band.
func negFeedback() db.FeedbackEvent {
	return db.FeedbackEvent{Choice: "b", Reviewer: autoTraceReviewer, Source: autoTraceSource, Reasoning: "changes requested"}
}

func repeat(event db.FeedbackEvent, n int) []db.FeedbackEvent {
	out := make([]db.FeedbackEvent, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, event)
	}
	return out
}

func TestEvaluateCanaryRegression(t *testing.T) {
	cases := []struct {
		name                string
		canary              []db.FeedbackEvent
		champion            []db.FeedbackEvent
		minSamples          int
		canaryUnavailable   bool
		championUnavailable bool
		want                CanaryDecision
		reasonHas           string
	}{
		{
			// (a) canary MATERIALLY worse than champion with >= minSamples => rollback.
			name:       "material regression rolls back",
			canary:     repeat(negFeedback(), 4),
			champion:   repeat(realCIFeedback(), 4),
			minSamples: 3,
			want:       CanaryRollback,
			reasonHas:  "rolling back",
		},
		{
			// (b) fewer than minSamples canary outcomes => continue (keep sampling),
			// even though the few it has are terrible.
			name:       "thin canary samples hold",
			canary:     repeat(negFeedback(), 2),
			champion:   repeat(realCIFeedback(), 10),
			minSamples: 3,
			want:       CanaryContinue,
			reasonHas:  "below min_samples",
		},
		{
			// (c) canary feedback unavailable (read error) => continue: NEVER roll back
			// on evidence we could not read.
			name:              "canary unavailable holds (never roll back on unread evidence)",
			canary:            nil,
			champion:          repeat(realCIFeedback(), 10),
			minSamples:        3,
			canaryUnavailable: true,
			want:              CanaryContinue,
			reasonHas:         "unavailable",
		},
		{
			// (d) canary at parity-or-better than champion => graduate.
			name:       "parity or better graduates",
			canary:     repeat(realCIFeedback(), 5),
			champion:   repeat(realCIFeedback(), 5),
			minSamples: 3,
			want:       CanaryGraduate,
			reasonHas:  "graduating",
		},
		{
			// Canary strictly better than champion => graduate.
			name:       "improvement graduates",
			canary:     repeat(realCIFeedback(), 5),
			champion:   repeat(noCIFeedback(), 5),
			minSamples: 3,
			want:       CanaryGraduate,
			reasonHas:  "graduating",
		},
		{
			// No champion baseline (empty) => continue: cannot confirm regression OR
			// non-regression, so HOLD (fail-safe, never graduate without a baseline).
			name:       "no champion baseline holds",
			canary:     repeat(realCIFeedback(), 5),
			champion:   nil,
			minSamples: 3,
			want:       CanaryContinue,
			reasonHas:  "no champion baseline",
		},
		{
			// Champion feedback unavailable (read error) => continue (no baseline).
			name:                "champion unavailable holds",
			canary:              repeat(realCIFeedback(), 5),
			champion:            nil,
			minSamples:          3,
			championUnavailable: true,
			want:                CanaryContinue,
			reasonHas:           "champion feedback unavailable",
		},
		{
			// minSamples <= 0 (unset floor) => continue: never act without a real floor.
			name:       "no sample floor holds",
			canary:     repeat(negFeedback(), 10),
			champion:   repeat(realCIFeedback(), 10),
			minSamples: 0,
			want:       CanaryContinue,
			reasonHas:  "no canary sample floor",
		},
		{
			// A mild dip WITHIN the tolerance band (champion all real-CI=1.0, canary all
			// near-positive=0.6 => delta 0.4 >= 0.2) is material => rollback. Confirms the
			// band semantics: near-positive is materially below strong-positive.
			name:       "near-positive canary vs strong-positive champion is material",
			canary:     repeat(noCIFeedback(), 5),
			champion:   repeat(realCIFeedback(), 5),
			minSamples: 3,
			want:       CanaryRollback,
			reasonHas:  "rolling back",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := EvaluateCanaryRegression(tc.canary, tc.champion, tc.minSamples, tc.canaryUnavailable, tc.championUnavailable)
			if verdict.Decision != tc.want {
				t.Fatalf("Decision = %q, want %q (reason: %q)", verdict.Decision, tc.want, verdict.Reason)
			}
			if tc.reasonHas != "" && !strings.Contains(verdict.Reason, tc.reasonHas) {
				t.Fatalf("reason %q does not contain %q", verdict.Reason, tc.reasonHas)
			}
		})
	}
}

// TestCanaryVersionScoreBands proves the coarse per-version score reuses the #465
// harvest vocabulary: a real-CI positive is the strong band, a near-neutral
// choice-"a" the mid band, a choice-"b" the low band, and non-a/b rows are not
// countable.
func TestCanaryVersionScoreBands(t *testing.T) {
	if score, n := canaryVersionScore([]db.FeedbackEvent{realCIFeedback()}); score != canaryScoreStrongPositive || n != 1 {
		t.Fatalf("real-CI score = (%v, %d), want (%v, 1)", score, n, canaryScoreStrongPositive)
	}
	if score, n := canaryVersionScore([]db.FeedbackEvent{noCIFeedback()}); score != canaryScoreNearPositive || n != 1 {
		t.Fatalf("near-positive score = (%v, %d), want (%v, 1)", score, n, canaryScoreNearPositive)
	}
	if score, n := canaryVersionScore([]db.FeedbackEvent{negFeedback()}); score != canaryScoreNegative || n != 1 {
		t.Fatalf("negative score = (%v, %d), want (%v, 1)", score, n, canaryScoreNegative)
	}
	// A non-a/b row carries no verdict and is not counted.
	if score, n := canaryVersionScore([]db.FeedbackEvent{{Choice: ""}}); score != 0 || n != 0 {
		t.Fatalf("uncountable score = (%v, %d), want (0, 0)", score, n)
	}
}
