package skillopt

import (
	"fmt"
	"math"
	"math/rand"
)

// DefaultProbDraws is the Monte Carlo draw count used when a caller passes 0 to
// ProbChallengerBeats. 20000 draws gives a P(>) estimate stable to ~0.003 while
// staying cheap enough to recompute on every pick.
const DefaultProbDraws = 20000

// Mode B champion-challenger Thompson-sampling bandit (#473, scoped from RFC
// #463). This file is the PURE, I/O-free engine: a Beta-Bernoulli posterior per
// arm, where an arm is one (template_id, version_id) variant. The champion arm
// is the current promoted version, the challenger arm a pending candidate
// version. Persistence (the skillopt_bandit_arms table) and the CLI A/B wiring
// live elsewhere; this file only does the math, and every randomized routine
// takes an INJECTED *rand.Rand so tests pin a seed and assert exact values.
//
// A "win" for an arm means that variant's answer was the human's preferred pick
// in a pairwise A/B; the other arm takes a "loss". The promotion confidence #471
// consumes is P(challenger > champion), the probability a fresh sample from the
// challenger's posterior beats one from the champion's.

// BetaParams is a Beta(Alpha, Beta) posterior. Alpha = 1 + wins and Beta =
// 1 + losses under the uniform Beta(1,1) prior, so the pair is the Bernoulli
// sufficient statistic and a stored arm row is exactly reconstructable.
type BetaParams struct {
	Alpha float64
	Beta  float64
}

// BanditArm is a persisted arm's mutable counters: the Beta posterior plus the
// total number of pulls (wins + losses) it has accrued. Pulls drives the
// human-facing "over K samples" string and the low-traffic tiering floor.
type BanditArm struct {
	Alpha float64
	Beta  float64
	Pulls int
}

// SampleTheta draws one Thompson sample theta ~ Beta(Alpha, Beta) from the arm's
// posterior using the injected rng. The draw is X/(X+Y) with X~Gamma(Alpha,1)
// and Y~Gamma(Beta,1) — the standard Beta-from-two-Gammas construction — so the
// argmax-over-arms selection used by the (deferred) live loop is regret
// minimizing. Deterministic given a seeded rng.
func (p BetaParams) SampleTheta(rng *rand.Rand) float64 {
	x := sampleGamma(rng, p.Alpha)
	y := sampleGamma(rng, p.Beta)
	if x+y <= 0 {
		// Degenerate only when both shapes are ~0; fall back to the prior mean.
		return 0.5
	}
	return x / (x + y)
}

// ProbChallengerBeats estimates P(theta_challenger > theta_champion) by Monte
// Carlo: it draws `draws` independent pairs from the two posteriors using the
// SAME injected rng and returns the fraction where the challenger sample is
// strictly larger. This is the promotion confidence #471's auto_promote_min_
// confidence guardrail compares against. Deterministic given a seeded rng; the
// MANDATORY closed-form cross-check in bandit_test.go proves the sampler is
// unbiased. Equal samples (a measure-zero tie for continuous
// Beta, but possible at the float boundary) count as NOT beating, the
// conservative direction for a promotion gate.
func ProbChallengerBeats(champion, challenger BetaParams, rng *rand.Rand, draws int) float64 {
	if draws <= 0 {
		draws = DefaultProbDraws
	}
	wins := 0
	for i := 0; i < draws; i++ {
		champTheta := champion.SampleTheta(rng)
		chalTheta := challenger.SampleTheta(rng)
		if chalTheta > champTheta {
			wins++
		}
	}
	return float64(wins) / float64(draws)
}

// ConfidenceSummary renders the human-facing promotion-confidence string the
// candidate.awaiting_promotion event Detail carries, e.g. "96% likely better
// over 80 samples". samples is the challenger arm's pull count.
func ConfidenceSummary(prob float64, samples int) string {
	return fmt.Sprintf("%.0f%% likely better over %d samples", prob*100, samples)
}

// sampleGamma draws X ~ Gamma(shape, 1) using the injected rng. For shape >= 1
// it uses Marsaglia-Tsang's squeeze method (rejection on a normal proposal),
// the same algorithm math/rand's own gamma helpers use; for 0 < shape < 1 it
// boosts via the Gamma(shape+1) * U^(1/shape) identity. shape <= 0 returns 0.
// All randomness flows through rng so the whole engine is reproducible.
func sampleGamma(rng *rand.Rand, shape float64) float64 {
	if shape <= 0 {
		return 0
	}
	if shape < 1 {
		// Boosting: if G ~ Gamma(shape+1, 1) and U ~ Uniform(0,1) then
		// G * U^(1/shape) ~ Gamma(shape, 1).
		u := rng.Float64()
		// Guard against U==0 producing -Inf through the log path.
		for u <= 0 {
			u = rng.Float64()
		}
		return sampleGamma(rng, shape+1) * math.Pow(u, 1/shape)
	}
	// Marsaglia & Tsang (2000).
	d := shape - 1.0/3.0
	c := 1.0 / math.Sqrt(9*d)
	for {
		x := rng.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rng.Float64()
		x2 := x * x
		// Squeeze: cheap acceptance test first, full log test as the fallback.
		if u < 1-0.0331*x2*x2 {
			return d * v
		}
		if math.Log(u) < 0.5*x2+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}
