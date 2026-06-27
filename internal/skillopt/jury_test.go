package skillopt

import (
	"math"
	"testing"
)

// pick is a tiny helper building a verdict with only a boolean decision (the
// pairwise A/B shape: no rubric dimensions).
func pick(decision bool) JuryJudgeVerdict {
	return JuryJudgeVerdict{Decision: decision}
}

// scored builds a verdict with both a boolean decision and per-dimension scores.
func scored(decision bool, dims map[string]float64) JuryJudgeVerdict {
	return JuryJudgeVerdict{Decision: decision, DimensionScores: dims}
}

// TestEvaluateJuryMedian covers the per-dimension median for ODD and EVEN N
// (even averages the two middle values) and robustness to one outlier judge.
func TestEvaluateJuryMedian(t *testing.T) {
	cases := []struct {
		name     string
		verdicts []JuryJudgeVerdict
		dim      string
		want     float64
	}{
		{
			// Odd N=3: the middle value, unmoved by the 0.1 outlier.
			name: "odd N median ignores outlier",
			verdicts: []JuryJudgeVerdict{
				scored(true, map[string]float64{"quality": 0.8}),
				scored(true, map[string]float64{"quality": 0.7}),
				scored(true, map[string]float64{"quality": 0.1}),
			},
			dim:  "quality",
			want: 0.7,
		},
		{
			// Even N=4: average of the two middle values (0.6 and 0.8 => 0.7).
			name: "even N median averages middle two",
			verdicts: []JuryJudgeVerdict{
				scored(true, map[string]float64{"quality": 0.9}),
				scored(true, map[string]float64{"quality": 0.8}),
				scored(true, map[string]float64{"quality": 0.6}),
				scored(true, map[string]float64{"quality": 0.5}),
			},
			dim:  "quality",
			want: 0.7,
		},
		{
			// A dimension only some judges scored: median over the judges that have it.
			name: "median over judges that scored the dim",
			verdicts: []JuryJudgeVerdict{
				scored(true, map[string]float64{"safety": 0.4}),
				scored(true, map[string]float64{"safety": 0.6}),
				pick(true), // no dims
			},
			dim:  "safety",
			want: 0.5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateJury(JuryConfig{}, tc.verdicts)
			if math.Abs(got.MedianScores[tc.dim]-tc.want) > 1e-9 {
				t.Fatalf("median[%q] = %v, want %v", tc.dim, got.MedianScores[tc.dim], tc.want)
			}
		})
	}
}

// TestEvaluateJuryMajority covers the boolean majority vote INCLUDING tie
// handling (an even-N tie resolves to the fail-safe false AND flags disagreement).
func TestEvaluateJuryMajority(t *testing.T) {
	cases := []struct {
		name             string
		verdicts         []JuryJudgeVerdict
		wantDecision     bool
		wantApprove      int
		wantReject       int
		wantDisagreement bool
	}{
		{"unanimous promote", []JuryJudgeVerdict{pick(true), pick(true), pick(true)}, true, 3, 0, false},
		{"unanimous reject", []JuryJudgeVerdict{pick(false), pick(false)}, false, 0, 2, false},
		{"2:1 promote with disagreement", []JuryJudgeVerdict{pick(true), pick(true), pick(false)}, true, 2, 1, true},
		{"1:2 reject with disagreement", []JuryJudgeVerdict{pick(true), pick(false), pick(false)}, false, 1, 2, true},
		// Even-N tie: fail-safe false, and the split is flagged.
		{"2:2 tie is fail-safe reject + disagreement", []JuryJudgeVerdict{pick(true), pick(true), pick(false), pick(false)}, false, 2, 2, true},
		{"1:1 tie is fail-safe reject + disagreement", []JuryJudgeVerdict{pick(true), pick(false)}, false, 1, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateJury(JuryConfig{}, tc.verdicts)
			if got.Decision != tc.wantDecision {
				t.Fatalf("Decision = %v, want %v", got.Decision, tc.wantDecision)
			}
			if got.Approve != tc.wantApprove || got.Reject != tc.wantReject {
				t.Fatalf("tally = %d:%d, want %d:%d", got.Approve, got.Reject, tc.wantApprove, tc.wantReject)
			}
			if got.Disagreement != tc.wantDisagreement {
				t.Fatalf("Disagreement = %v, want %v (reason %q)", got.Disagreement, tc.wantDisagreement, got.DisagreementReason)
			}
		})
	}
}

// TestEvaluateJuryMinorityVeto covers the fail-closed minority veto: ONE judge
// below the floor on a configured safety dimension BLOCKS even when the majority
// would promote; an unconfigured/absent dimension never vetoes.
func TestEvaluateJuryMinorityVeto(t *testing.T) {
	cfg := JuryConfig{VetoDimensions: []string{"safety"}, VetoFloor: 0.5}

	t.Run("one judge below floor blocks despite majority promote", func(t *testing.T) {
		got := EvaluateJury(cfg, []JuryJudgeVerdict{
			scored(true, map[string]float64{"safety": 0.9}),
			scored(true, map[string]float64{"safety": 0.8}),
			scored(true, map[string]float64{"safety": 0.2}), // one veto
		})
		if !got.Vetoed {
			t.Fatal("expected Vetoed=true (one judge below the safety floor)")
		}
		if got.Decision {
			t.Fatal("expected Decision=false: a veto must force the fail-closed reject despite the 3:0 promote")
		}
	})

	t.Run("all above floor does not veto", func(t *testing.T) {
		got := EvaluateJury(cfg, []JuryJudgeVerdict{
			scored(true, map[string]float64{"safety": 0.9}),
			scored(true, map[string]float64{"safety": 0.6}),
		})
		if got.Vetoed {
			t.Fatal("expected Vetoed=false: every judge cleared the floor")
		}
		if !got.Decision {
			t.Fatal("expected Decision=true: unanimous promote, no veto")
		}
	})

	t.Run("veto dimension absent never vetoes", func(t *testing.T) {
		got := EvaluateJury(cfg, []JuryJudgeVerdict{
			scored(true, map[string]float64{"quality": 0.1}), // low, but not a veto dim
			scored(true, map[string]float64{"quality": 0.2}),
		})
		if got.Vetoed {
			t.Fatal("expected Vetoed=false: the safety dimension was never scored")
		}
	})

	t.Run("default floor zero is inert", func(t *testing.T) {
		got := EvaluateJury(JuryConfig{VetoDimensions: []string{"safety"}}, []JuryJudgeVerdict{
			scored(true, map[string]float64{"safety": 0.0}),
		})
		if got.Vetoed {
			t.Fatal("expected Vetoed=false: a [0,1] score is never < 0, so floor 0 cannot fire")
		}
	})
}

// TestEvaluateJuryDisagreement covers BOTH disagreement triggers: a per-dimension
// std above tau, and a 2:1 vote split. A tau of 0 disables the std check.
func TestEvaluateJuryDisagreement(t *testing.T) {
	t.Run("std above tau flags disagreement on a unanimous vote", func(t *testing.T) {
		// Unanimous promote (no vote split) but the scores are far apart.
		cfg := JuryConfig{DisagreementTau: 0.2}
		got := EvaluateJury(cfg, []JuryJudgeVerdict{
			scored(true, map[string]float64{"quality": 0.9}),
			scored(true, map[string]float64{"quality": 0.1}),
		})
		if !got.Disagreement {
			t.Fatalf("expected Disagreement=true (std 0.4 > tau 0.2) even on a unanimous vote")
		}
	})

	t.Run("tight scores below tau do not flag", func(t *testing.T) {
		cfg := JuryConfig{DisagreementTau: 0.2}
		got := EvaluateJury(cfg, []JuryJudgeVerdict{
			scored(true, map[string]float64{"quality": 0.55}),
			scored(true, map[string]float64{"quality": 0.50}),
		})
		if got.Disagreement {
			t.Fatalf("expected Disagreement=false (std 0.025 < tau 0.2, unanimous vote), got reason %q", got.DisagreementReason)
		}
	})

	t.Run("tau zero disables std check", func(t *testing.T) {
		// Wide spread but tau unset: only a vote split could flag, and the vote is
		// unanimous, so no disagreement.
		got := EvaluateJury(JuryConfig{}, []JuryJudgeVerdict{
			scored(true, map[string]float64{"quality": 0.9}),
			scored(true, map[string]float64{"quality": 0.1}),
		})
		if got.Disagreement {
			t.Fatal("expected Disagreement=false: tau=0 disables the std check and the vote is unanimous")
		}
	})

	t.Run("2:1 vote split flags regardless of tau", func(t *testing.T) {
		got := EvaluateJury(JuryConfig{}, []JuryJudgeVerdict{pick(true), pick(true), pick(false)})
		if !got.Disagreement {
			t.Fatal("expected Disagreement=true for a 2:1 vote split")
		}
	})
}

// TestEvaluateJuryEmptyIsTotal proves the aggregator is total: zero judges yields
// the zero decision and never panics.
func TestEvaluateJuryEmptyIsTotal(t *testing.T) {
	got := EvaluateJury(JuryConfig{VetoDimensions: []string{"safety"}, VetoFloor: 0.9, DisagreementTau: 0.1}, nil)
	if got.JudgeCount != 0 || got.Decision || got.Vetoed || got.Disagreement {
		t.Fatalf("empty jury must be the zero decision, got %+v", got)
	}
	if len(got.MedianScores) != 0 {
		t.Fatalf("empty jury must have no medians, got %v", got.MedianScores)
	}
}
