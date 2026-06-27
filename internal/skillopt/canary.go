package skillopt

import (
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

// CanaryDecision is the verdict of the bounded regression-window comparator
// (#484). It is the canary analogue of #471's promote/notify-only decision, but
// THREE-valued because a canary has a third, fail-safe outcome: keep sampling.
type CanaryDecision string

const (
	// CanaryRollback retires the canary and keeps the prior champion live: the
	// canary accrued enough verifiable outcomes AND its quality is materially below
	// the champion's. This is the ONLY decision that reverses a canary; it is
	// reached only on read evidence (never on uncertainty).
	CanaryRollback CanaryDecision = "rollback"
	// CanaryGraduate promotes the canary to full champion: it has enough outcomes
	// AND is at parity-or-better than the prior champion.
	CanaryGraduate CanaryDecision = "graduate"
	// CanaryContinue keeps the canary sampling and the champion live: the
	// fail-safe DEFAULT for every uncertainty — too few canary samples, no champion
	// baseline to compare against, or feedback we could not read. Uncertainty never
	// rolls back (never throws away the canary on unread evidence) and never
	// graduates (never promotes the canary without confirming non-regression).
	CanaryContinue CanaryDecision = "continue"
)

// CanaryVerdict is the pure result of EvaluateCanaryRegression: the decision plus
// a short, human-facing reason that travels into the candidate.* event Detail.
type CanaryVerdict struct {
	Decision CanaryDecision
	Reason   string
}

// Per-version coarse quality scores derived from the #465 harvest vocabulary.
// They reuse the SAME band semantics the harvester writes (a strong real-CI
// positive vs. a near-neutral empty-gate merge vs. a negative) so the comparator
// never invents a new scoring scale.
const (
	// canaryScoreStrongPositive is a merge that passed GENUINE external CI (the
	// FeedbackEventIsRealExternalCIPositive / scoreMergedRealCI band).
	canaryScoreStrongPositive = 1.0
	// canaryScoreNearPositive is a positive choice ("a") WITHOUT a real-CI marker —
	// a near-neutral empty-gate merge or other soft positive.
	canaryScoreNearPositive = 0.6
	// canaryScoreNegative is a negative choice ("b").
	canaryScoreNegative = 0.0
)

// canaryRegressionDelta is the MATERIAL-regression threshold: the canary's mean
// quality score must be at least this far BELOW the prior champion's to trigger a
// rollback. It is deliberately conservative (a buffer band, not a hair-trigger) so
// ordinary sampling noise around parity GRADUATES rather than flapping into a
// rollback. It is derived from the harvest score bands (the 0.4 gap between a
// near-neutral 0.6 positive and a 1.0 strong positive) rather than a fresh magic
// number, so it moves with the bands if they ever change.
const canaryRegressionDelta = 0.2

// EvaluateCanaryRegression is the pure, deterministic, total bounded-window
// comparator (#484). Given the canary version's and the prior champion's harvested
// auto-trace feedback events, the minimum sample floor (reusing
// AutoPromoteMinSamples — no new knob), and per-side "could not read the feedback"
// flags, it returns rollback / graduate / continue with a reason. It performs NO
// I/O and NEVER mutates state: the daemon resolves the two runs, calls this, and
// acts on the verdict via the EXISTING store rollback/graduate transactions.
//
// FAIL-SAFE DISCIPLINE (inverted vs. #471's promote: here uncertainty leaves the
// champion live AND keeps the canary sampling — it never rolls back and never
// graduates):
//   - canaryUnavailable (a read error on the canary's run) -> continue: we never
//     throw away a canary on evidence we could not read.
//   - fewer than minSamples countable canary outcomes -> continue: the window is
//     not yet decisive; keep sampling.
//   - championUnavailable, or zero countable champion outcomes -> continue: with no
//     baseline we can neither confirm a regression (so never rollback) nor confirm
//     non-regression (so never graduate) — hold.
//   - minSamples <= 0 -> continue: an unset/zero floor would let a single noisy
//     sample decide; refuse to act without a real floor (mirrors the #471 unset
//     min_samples hard stop).
//
// DECISION (only when both sides have read, decisive evidence):
//   - canary score materially below champion (by >= canaryRegressionDelta) -> rollback.
//   - otherwise (parity or improvement, within the buffer) -> graduate.
func EvaluateCanaryRegression(canaryEvents, championEvents []db.FeedbackEvent, minSamples int, canaryUnavailable, championUnavailable bool) CanaryVerdict {
	if minSamples <= 0 {
		return CanaryVerdict{Decision: CanaryContinue, Reason: "no canary sample floor configured; holding (keep sampling)"}
	}
	if canaryUnavailable {
		return CanaryVerdict{Decision: CanaryContinue, Reason: "canary feedback unavailable (read error); holding — never roll back on unread evidence"}
	}
	canaryScore, canarySamples := canaryVersionScore(canaryEvents)
	if canarySamples < minSamples {
		return CanaryVerdict{Decision: CanaryContinue, Reason: fmt.Sprintf("only %d canary outcome(s), below min_samples=%d; holding (keep sampling)", canarySamples, minSamples)}
	}
	if championUnavailable {
		return CanaryVerdict{Decision: CanaryContinue, Reason: "champion feedback unavailable (read error); holding — no baseline to compare against"}
	}
	championScore, championSamples := canaryVersionScore(championEvents)
	if championSamples == 0 {
		return CanaryVerdict{Decision: CanaryContinue, Reason: "no champion baseline outcomes; holding — cannot confirm regression or non-regression"}
	}
	if canaryScore < championScore-canaryRegressionDelta {
		return CanaryVerdict{
			Decision: CanaryRollback,
			Reason:   fmt.Sprintf("canary score %.3g materially below champion %.3g (delta>=%.3g) over %d/%d samples; rolling back", canaryScore, championScore, canaryRegressionDelta, canarySamples, championSamples),
		}
	}
	return CanaryVerdict{
		Decision: CanaryGraduate,
		Reason:   fmt.Sprintf("canary score %.3g at parity-or-better than champion %.3g over %d/%d samples; graduating", canaryScore, championScore, canarySamples, championSamples),
	}
}

// canaryVersionScore derives a coarse mean quality score in [0,1] and a countable
// sample count from a version's harvested auto-trace feedback (#465 vocabulary). A
// real-CI positive scores canaryScoreStrongPositive, any other positive choice
// ("a") canaryScoreNearPositive, and a negative choice ("b") canaryScoreNegative;
// events with neither an "a" nor "b" choice are ignored (not countable). The score
// is the mean over countable events; with no countable events it is (0, 0).
func canaryVersionScore(events []db.FeedbackEvent) (score float64, samples int) {
	var sum float64
	for _, event := range events {
		switch strings.TrimSpace(strings.ToLower(event.Choice)) {
		case "a":
			if FeedbackEventIsRealExternalCIPositive(event) {
				sum += canaryScoreStrongPositive
			} else {
				sum += canaryScoreNearPositive
			}
			samples++
		case "b":
			sum += canaryScoreNegative
			samples++
		default:
			// Non-a/b rows (e.g. unscored placeholders) carry no verdict; skip.
		}
	}
	if samples == 0 {
		return 0, 0
	}
	return sum / float64(samples), samples
}
