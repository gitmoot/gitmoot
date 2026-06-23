package workflow

import (
	"strings"
	"testing"
)

// TestScoreComplexityRange asserts the score always lands in [0, 1], including
// for a maximally loaded delegation that would otherwise overflow the clamp.
func TestScoreComplexityRange(t *testing.T) {
	cases := []Delegation{
		{Action: "ask", Prompt: "hi"},
		{
			Action:    "implement",
			Prompt:    strings.Repeat("architecture oauth schema concurrency migration security refactor ", 50),
			Artifacts: []string{"a", "b", "c"},
			Worktree:  "wt",
			Quorum:    2,
		},
	}
	for i, d := range cases {
		got := ScoreComplexity(d)
		if got < 0 || got > 1 {
			t.Fatalf("case %d: score %v out of [0,1]", i, got)
		}
	}
}

// TestScoreComplexityHardKeywords checks the hard-keyword signal: a single hard
// keyword lifts the score, multiple keywords lift it further, and the keyword
// contribution is capped.
func TestScoreComplexityHardKeywords(t *testing.T) {
	base := Delegation{ID: "x", Agent: "a", Action: "ask", Prompt: "do a small thing"}
	one := Delegation{ID: "x", Agent: "a", Action: "ask", Prompt: "redesign the architecture"}
	many := Delegation{ID: "x", Agent: "a", Action: "ask", Prompt: "architecture oauth schema concurrency migration security"}

	baseScore := ScoreComplexity(base)
	oneScore := ScoreComplexity(one)
	manyScore := ScoreComplexity(many)

	if !(baseScore < oneScore) {
		t.Fatalf("one hard keyword should raise the score: base=%v one=%v", baseScore, oneScore)
	}
	if !(oneScore < manyScore) {
		t.Fatalf("more hard keywords should raise the score: one=%v many=%v", oneScore, manyScore)
	}
	// The hard-keyword contribution is capped (maxHardKeywordScore = 0.55), so
	// piling on keywords alone (with a cheap ask action) clears the cheap band
	// but does not by itself reach the deep band — extra signals are needed.
	if got := TierFor(manyScore); got == TierCheap {
		t.Fatalf("six hard keywords should clear the cheap band, got %q (score=%v)", got, manyScore)
	}
	// Add an implement action to the same hard-keyword prompt and it should now
	// clear the deep threshold.
	deep := Delegation{ID: "x", Agent: "a", Action: "implement", Prompt: many.Prompt}
	if got := TierFor(ScoreComplexity(deep)); got != TierDeep {
		t.Fatalf("hard keywords plus an implement action should reach deep, got %q (score=%v)", got, ScoreComplexity(deep))
	}
}

// TestScoreComplexityActionType checks that an implement-style action scores
// higher than a review, which scores higher than a bare ask, holding the prompt
// constant.
func TestScoreComplexityActionType(t *testing.T) {
	const prompt = "touch the handler"
	ask := ScoreComplexity(Delegation{Action: "ask", Prompt: prompt})
	review := ScoreComplexity(Delegation{Action: "review", Prompt: prompt})
	implement := ScoreComplexity(Delegation{Action: "implement", Prompt: prompt})

	if !(ask < review) {
		t.Fatalf("review should score above ask: ask=%v review=%v", ask, review)
	}
	if !(review < implement) {
		t.Fatalf("implement should score above review: review=%v implement=%v", review, implement)
	}
	// An ask contributes nothing from the action signal.
	if ask != ScoreComplexity(Delegation{Action: "unknown-verb", Prompt: prompt}) {
		t.Fatalf("ask and an unknown action should score equally on the action signal")
	}
}

// TestScoreComplexityScopeSignal checks the scope signal fires on artifact
// breadth or a worktree, and not for a single-target leg.
func TestScoreComplexityScopeSignal(t *testing.T) {
	const prompt = "edit one file"
	narrow := ScoreComplexity(Delegation{Action: "ask", Prompt: prompt})
	oneArtifact := ScoreComplexity(Delegation{Action: "ask", Prompt: prompt, Artifacts: []string{"a"}})
	manyArtifacts := ScoreComplexity(Delegation{Action: "ask", Prompt: prompt, Artifacts: []string{"a", "b"}})
	withWorktree := ScoreComplexity(Delegation{Action: "ask", Prompt: prompt, Worktree: "wt"})

	if narrow != oneArtifact {
		t.Fatalf("a single artifact should not fire the scope signal: narrow=%v one=%v", narrow, oneArtifact)
	}
	if !(narrow < manyArtifacts) {
		t.Fatalf("multiple artifacts should fire the scope signal: narrow=%v many=%v", narrow, manyArtifacts)
	}
	if !(narrow < withWorktree) {
		t.Fatalf("a worktree should fire the scope signal: narrow=%v worktree=%v", narrow, withWorktree)
	}
}

// TestScoreComplexityQuorum checks a quorum-critical leg scores above an
// otherwise identical non-quorum leg.
func TestScoreComplexityQuorum(t *testing.T) {
	const prompt = "review this"
	plain := ScoreComplexity(Delegation{Action: "review", Prompt: prompt})
	quorum := ScoreComplexity(Delegation{Action: "review", Prompt: prompt, Quorum: 2})
	if !(plain < quorum) {
		t.Fatalf("a quorum leg should score above a non-quorum leg: plain=%v quorum=%v", plain, quorum)
	}
}

// TestScoreComplexityPromptLength checks that a longer prompt scores at least as
// high as a short one, all else equal, and saturates at the configured cap.
func TestScoreComplexityPromptLength(t *testing.T) {
	short := ScoreComplexity(Delegation{Action: "ask", Prompt: "short"})
	long := ScoreComplexity(Delegation{Action: "ask", Prompt: strings.Repeat("x", 600)})
	saturated := ScoreComplexity(Delegation{Action: "ask", Prompt: strings.Repeat("x", 5000)})

	if !(short < long) {
		t.Fatalf("a longer prompt should score higher: short=%v long=%v", short, long)
	}
	if !(long < saturated) {
		t.Fatalf("a much longer prompt should score higher up to saturation: long=%v saturated=%v", long, saturated)
	}
	// Beyond saturation, additional length adds nothing.
	beyond := ScoreComplexity(Delegation{Action: "ask", Prompt: strings.Repeat("x", 50000)})
	if saturated != beyond {
		t.Fatalf("length signal should saturate: saturated=%v beyond=%v", saturated, beyond)
	}
}

// TestTierForBoundaries pins the fixed thresholds from issue #379, including the
// exact boundary values (the `<` comparisons mean 0.3 and 0.6 fall into the
// upper band).
func TestTierForBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		score float64
		want  Tier
	}{
		{"zero", 0.0, TierCheap},
		{"just below cheap cap", 0.299, TierCheap},
		{"exactly 0.3 is standard", 0.30, TierStandard},
		{"mid standard", 0.45, TierStandard},
		{"just below deep cap", 0.599, TierStandard},
		{"exactly 0.6 is deep", 0.60, TierDeep},
		{"high", 0.95, TierDeep},
		{"one", 1.0, TierDeep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TierFor(tc.score); got != tc.want {
				t.Fatalf("TierFor(%v) = %q, want %q", tc.score, got, tc.want)
			}
		})
	}
}

// TestTierForNeverMechanical asserts TierFor never returns mechanical, no matter
// the score — mechanical is reachable only via explicit intent.
func TestTierForNeverMechanical(t *testing.T) {
	for s := 0.0; s <= 1.0; s += 0.05 {
		if got := TierFor(s); got == TierMechanical {
			t.Fatalf("TierFor(%v) returned mechanical; it must come only from intent", s)
		}
	}
}

// TestTierForDelegationMechanicalIntent checks that an explicit mechanical
// keyword selects TierMechanical even when the numeric score would otherwise be
// high, and that without such a keyword the tier follows the score.
func TestTierForDelegationMechanicalIntent(t *testing.T) {
	cases := []struct {
		name string
		d    Delegation
		want Tier
	}{
		{
			name: "rename intent wins over a high score",
			d: Delegation{
				Action: "implement",
				// Loaded with hard keywords so the score is high, but the explicit
				// rename intent should still pin it to mechanical.
				Prompt:    "rename the Foo symbol across the security and migration architecture",
				Artifacts: []string{"a", "b"},
			},
			want: TierMechanical,
		},
		{
			name: "codemod intent",
			d:    Delegation{Action: "implement", Prompt: "run a codemod to update the import paths"},
			want: TierMechanical,
		},
		{
			name: "no intent, low score falls to cheap",
			d:    Delegation{Action: "ask", Prompt: "what does this do"},
			want: TierCheap,
		},
		{
			name: "no intent, hard work falls to deep",
			d: Delegation{
				Action: "implement",
				Prompt: "redesign the oauth and concurrency architecture with a schema migration",
				Quorum: 2,
			},
			want: TierDeep,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TierForDelegation(tc.d); got != tc.want {
				t.Fatalf("TierForDelegation(%q) = %q, want %q (score=%v)",
					tc.d.Prompt, got, tc.want, ScoreComplexity(tc.d))
			}
		})
	}
}

// TestScoreComplexityDeterministic checks the helper is pure: repeated calls on
// the same input return the same score.
func TestScoreComplexityDeterministic(t *testing.T) {
	d := Delegation{
		Action:    "implement",
		Prompt:    "implement an oauth flow with a schema migration",
		Artifacts: []string{"a", "b"},
		Quorum:    3,
	}
	first := ScoreComplexity(d)
	for i := 0; i < 5; i++ {
		if got := ScoreComplexity(d); got != first {
			t.Fatalf("ScoreComplexity not deterministic: first=%v call %d=%v", first, i, got)
		}
	}
}
