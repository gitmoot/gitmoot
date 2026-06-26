package skillopt

import (
	"math"
	"math/rand"
	"testing"
)

// TestNewArmAndUpdate proves the prior and the win/loss accounting: Beta(1,1)
// with zero pulls, a win bumps Alpha and pulls, a loss bumps Beta and pulls.
func TestNewArmAndUpdate(t *testing.T) {
	arm := NewArm()
	if arm.Alpha != 1 || arm.Beta != 1 || arm.Pulls != 0 {
		t.Fatalf("NewArm = Beta(%.0f,%.0f) pulls=%d, want Beta(1,1) pulls=0", arm.Alpha, arm.Beta, arm.Pulls)
	}
	won := arm.Update(true)
	if won.Alpha != 2 || won.Beta != 1 || won.Pulls != 1 {
		t.Fatalf("after win = Beta(%.0f,%.0f) pulls=%d, want Beta(2,1) pulls=1", won.Alpha, won.Beta, won.Pulls)
	}
	lost := won.Update(false)
	if lost.Alpha != 2 || lost.Beta != 2 || lost.Pulls != 2 {
		t.Fatalf("after loss = Beta(%.0f,%.0f) pulls=%d, want Beta(2,2) pulls=2", lost.Alpha, lost.Beta, lost.Pulls)
	}
}

// TestProbChallengerBeatsPriorIsHalf proves two identical Beta(1,1) posteriors
// give P(challenger>champion) ~ 0.5 (the uniform prior is symmetric), and that
// it is exactly reproducible for a fixed seed.
func TestProbChallengerBeatsPriorIsHalf(t *testing.T) {
	prior := BetaParams{Alpha: 1, Beta: 1}
	got := ProbChallengerBeats(prior, prior, rand.New(rand.NewSource(42)), 20000)
	if math.Abs(got-0.5) > 0.02 {
		t.Fatalf("P(>) for equal priors = %.4f, want ~0.5", got)
	}
	// Determinism: same seed, same draws, identical value.
	again := ProbChallengerBeats(prior, prior, rand.New(rand.NewSource(42)), 20000)
	if got != again {
		t.Fatalf("P(>) not deterministic for a fixed seed: %.6f vs %.6f", got, again)
	}
}

// TestSampleThetaDeterministic pins the seed and asserts SampleTheta reproduces
// exactly — the acceptance-criterion reproducibility guarantee.
func TestSampleThetaDeterministic(t *testing.T) {
	p := BetaParams{Alpha: 3, Beta: 2}
	first := p.SampleTheta(rand.New(rand.NewSource(7)))
	second := p.SampleTheta(rand.New(rand.NewSource(7)))
	if first != second {
		t.Fatalf("SampleTheta not deterministic: %.10f vs %.10f", first, second)
	}
	if first < 0 || first > 1 {
		t.Fatalf("SampleTheta out of [0,1]: %.10f", first)
	}
}

// TestProbChallengerBeatsMonotone proves the confidence rises monotonically as
// the challenger's win margin grows over the champion — the gate must move the
// right direction.
func TestProbChallengerBeatsMonotone(t *testing.T) {
	champion := BetaParams{Alpha: 5, Beta: 5} // 4 wins, 4 losses
	margins := []BetaParams{
		{Alpha: 5, Beta: 5},  // even
		{Alpha: 8, Beta: 3},  // ahead
		{Alpha: 15, Beta: 2}, // well ahead
	}
	prev := -1.0
	for _, chal := range margins {
		got := ProbChallengerBeats(champion, chal, rand.New(rand.NewSource(99)), 40000)
		if got < prev {
			t.Fatalf("P(>) not monotone: Beta(%.0f,%.0f) -> %.4f after %.4f", chal.Alpha, chal.Beta, got, prev)
		}
		prev = got
	}
	if prev <= 0.9 {
		t.Fatalf("strong challenger P(>) = %.4f, want > 0.9", prev)
	}
}

// TestProbChallengerBeatsClosedFormCrossCheck is the MANDATORY sampler-validation
// test: for several small integer alpha/beta pairs the Monte Carlo estimate must
// match the exact closed-form Beta-exceedance value within tolerance. This proves
// the Gamma/Beta sampler is unbiased — without it a confidently-wrong confidence
// could ship.
func TestProbChallengerBeatsClosedFormCrossCheck(t *testing.T) {
	cases := []struct {
		name       string
		champion   BetaParams
		challenger BetaParams
	}{
		{"both prior", BetaParams{1, 1}, BetaParams{1, 1}},
		{"challenger one win", BetaParams{1, 1}, BetaParams{2, 1}},
		{"champion one win", BetaParams{2, 1}, BetaParams{1, 1}},
		{"challenger ahead", BetaParams{3, 4}, BetaParams{6, 2}},
		{"champion ahead", BetaParams{7, 2}, BetaParams{3, 5}},
		{"larger counts", BetaParams{10, 8}, BetaParams{14, 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exact, ok := probChallengerBeatsClosedForm(tc.champion, tc.challenger)
			if !ok {
				t.Fatalf("closed form unavailable for integer params %v / %v", tc.champion, tc.challenger)
			}
			mc := ProbChallengerBeats(tc.champion, tc.challenger, rand.New(rand.NewSource(2024)), 200000)
			if math.Abs(mc-exact) > 0.01 {
				t.Fatalf("MC %.4f vs closed-form %.4f differ by more than tolerance (sampler may be biased)", mc, exact)
			}
		})
	}
}

// TestClosedFormSanity checks the closed-form oracle itself against hand values:
// symmetric priors give exactly 0.5, and a strict win ordering is > 0.5.
func TestClosedFormSanity(t *testing.T) {
	half, ok := probChallengerBeatsClosedForm(BetaParams{1, 1}, BetaParams{1, 1})
	if !ok || math.Abs(half-0.5) > 1e-9 {
		t.Fatalf("closed form P(Beta(1,1)>Beta(1,1)) = %.6f, want 0.5", half)
	}
	// P(Beta(2,1) > Beta(1,1)): challenger has one win. Exact value is 2/3.
	ahead, ok := probChallengerBeatsClosedForm(BetaParams{1, 1}, BetaParams{2, 1})
	if !ok || math.Abs(ahead-2.0/3.0) > 1e-9 {
		t.Fatalf("closed form P(Beta(2,1)>Beta(1,1)) = %.6f, want 0.6667", ahead)
	}
	rejected, ok := probChallengerBeatsClosedForm(BetaParams{1.5, 1}, BetaParams{1, 1})
	if ok {
		t.Fatalf("closed form should reject non-integer params, got %.4f", rejected)
	}
}

// TestConfidenceSummary proves the human-facing string format.
func TestConfidenceSummary(t *testing.T) {
	if got := ConfidenceSummary(0.962, 80); got != "96% likely better over 80 samples" {
		t.Fatalf("ConfidenceSummary = %q", got)
	}
}
