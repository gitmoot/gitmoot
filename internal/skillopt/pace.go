package skillopt

import "fmt"

// PACE (#687) — a PURE, model-free anytime-valid commit gate for candidate
// promotion, the accept-side complement of measure-the-judge (#344). It is a
// testing-by-betting e-process over a stream of PAIRED candidate-vs-champion
// outcomes evaluated on the SAME instances (McNemar pairing):
//
//   - Each pair is a candidate WIN, a candidate LOSS, or a TIE (both right or both
//     wrong). Ties carry no information under the null and are DISCARDED.
//   - Wealth E starts at 1. For each DISCORDANT pair with w in {1 (win), 0 (loss)}:
//         E <- E * (1 + lambda*(2w-1))
//     i.e. a win multiplies by (1+lambda), a loss by (1-lambda).
//   - Under H0 "candidate is not better," a discordant pair is a candidate-win with
//     probability 1/2, so (1+lambda*(2w-1)) has mean 1 and E is a nonnegative
//     (super)martingale with E[E] <= 1. By Ville's inequality the probability E ever
//     reaches 1/alpha is <= alpha, so COMMITTING when E >= 1/alpha bounds the
//     false-commit probability by alpha at ANY stopping time (anytime-valid): you may
//     peek after every pair, stop early when decisive, and reject when the pair
//     budget is exhausted without crossing.
//
// This file contains NO model calls and NO I/O — it is arithmetic (x(1+lambda) /
// x(1-lambda)) and is unit-testable in isolation. Wiring it onto the Mode B
// pairwise/bandit stream as an optional, off-by-default promotion gate lives in the
// CLI notify seam.

// PaceOutcome is one paired candidate-vs-champion result on a shared instance.
type PaceOutcome int

const (
	// PaceTie is a concordant pair (both sides right or both wrong): it carries no
	// information under the null and is discarded by the accumulator.
	PaceTie PaceOutcome = iota
	// PaceWin is a discordant pair the candidate won (candidate right, champion wrong).
	PaceWin
	// PaceLoss is a discordant pair the candidate lost (champion right, candidate wrong).
	PaceLoss
)

// PaceVerdict is the accumulator's total decision after the pairs seen so far.
type PaceVerdict int

const (
	// PaceContinue means the e-process has neither crossed the commit threshold nor
	// exhausted the pair budget: more discordant pairs are needed to decide.
	PaceContinue PaceVerdict = iota
	// PaceCommit means wealth E has reached 1/alpha — promote (false-commit prob <= alpha).
	PaceCommit
	// PaceReject means the pair budget (MaxPairs discordant pairs) was exhausted
	// without E crossing 1/alpha — the evidence never accumulated, so do not promote.
	PaceReject
)

// Default PACE parameters (matching the RFC / arXiv:2606.08106):
//   - alpha = 0.05  -> commit threshold 1/alpha = 20.
//   - lambda = 0.5  -> a win multiplies wealth by 1.5, a loss by 0.5.
//   - max_pairs      -> the discordant-pair budget before a non-decisive stream rejects.
const (
	DefaultPaceAlpha    = 0.05
	DefaultPaceLambda   = 0.5
	DefaultPaceMaxPairs = 200
)

// PaceConfig is the e-process configuration: the false-commit bound Alpha, the bet
// fraction Lambda, and the discordant-pair budget MaxPairs. The zero value is not
// meaningful; use withDefaults (applied by the constructor) to fill clearly-invalid
// fields from the Default* constants while preserving legal edge values (e.g.
// Lambda == 0, the no-op bet).
type PaceConfig struct {
	// Alpha is the target false-commit probability in (0,1); the commit threshold is
	// 1/Alpha. Out of range -> DefaultPaceAlpha.
	Alpha float64
	// Lambda is the bet fraction in [0,1]: each win scales wealth by (1+Lambda), each
	// loss by (1-Lambda). Lambda == 0 is a legal no-op (wealth never moves, never
	// commits). Out of [0,1] -> DefaultPaceLambda.
	Lambda float64
	// MaxPairs is the discordant-pair budget: after this many win/loss pairs without
	// crossing the threshold the accumulator rejects. <= 0 -> DefaultPaceMaxPairs.
	MaxPairs int
}

// withDefaults returns the config with only clearly-invalid fields replaced by the
// Default* constants. Legal edge values are preserved: Lambda == 0 (no-op bet) is
// kept, and any Alpha in (0,1) / Lambda in [0,1] / MaxPairs > 0 passes through
// unchanged. This keeps hand-built configs in tests exact while making a zero-value
// or misconfigured struct safe.
func (c PaceConfig) withDefaults() PaceConfig {
	out := c
	if !(out.Alpha > 0 && out.Alpha < 1) {
		out.Alpha = DefaultPaceAlpha
	}
	if out.Lambda < 0 || out.Lambda > 1 {
		out.Lambda = DefaultPaceLambda
	}
	if out.MaxPairs <= 0 {
		out.MaxPairs = DefaultPaceMaxPairs
	}
	return out
}

// PaceAccumulator is the streaming e-process. It consumes paired outcomes one at a
// time (ties discarded), updates wealth on each discordant pair, and latches
// PaceCommit the moment wealth first reaches the commit threshold (the anytime-valid
// early stop). It is not safe for concurrent use; a promotion decision is a single
// goroutine feeding a private accumulator.
type PaceAccumulator struct {
	cfg       PaceConfig
	threshold float64
	wealth    float64
	pairs     int // discordant pairs consumed (ties excluded)
	committed bool
}

// NewPaceAccumulator returns a fresh e-process seeded at wealth 1 for the given
// config (clearly-invalid fields defaulted). Ties fed later are discarded.
func NewPaceAccumulator(cfg PaceConfig) *PaceAccumulator {
	cfg = cfg.withDefaults()
	return &PaceAccumulator{
		cfg:       cfg,
		threshold: 1.0 / cfg.Alpha,
		wealth:    1.0,
	}
}

// Observe feeds one paired outcome and returns the accumulator's verdict AFTER it.
// A tie is discarded (no state change). A win/loss updates wealth and the discordant
// pair count. Once committed the accumulator latches: further outcomes are ignored
// and the verdict stays PaceCommit (an anytime-valid stop is final).
func (a *PaceAccumulator) Observe(outcome PaceOutcome) PaceVerdict {
	if a.committed {
		return PaceCommit
	}
	switch outcome {
	case PaceWin:
		a.wealth *= 1 + a.cfg.Lambda
		a.pairs++
	case PaceLoss:
		a.wealth *= 1 - a.cfg.Lambda
		a.pairs++
	default:
		// PaceTie (or any unknown value): concordant / no information -> discard.
		return a.Verdict()
	}
	if a.wealth >= a.threshold {
		a.committed = true
	}
	return a.Verdict()
}

// Verdict returns the current total decision without consuming an outcome: commit if
// wealth has crossed the threshold, reject if the discordant-pair budget is spent,
// else continue.
func (a *PaceAccumulator) Verdict() PaceVerdict {
	if a.committed {
		return PaceCommit
	}
	if a.pairs >= a.cfg.MaxPairs {
		return PaceReject
	}
	return PaceContinue
}

// EvaluatePaceCounts decides commit/reject/continue from AGGREGATE discordant-pair
// counts (candidate wins and losses), which is what the Mode B bandit arm records
// (skillopt_bandit_arms tallies win/loss; ties are never recorded). The bandit is a
// COUNT, not an ordered log, so this feeds the accumulator LOSSES-FIRST then wins:
// with losses first the wealth path descends monotonically to its minimum and then
// ascends monotonically, so its only threshold crossing is at the terminal pair —
// making the commit decision equal to the ORDER-INVARIANT terminal-wealth test
// (E = (1+lambda)^wins * (1-lambda)^losses >= 1/alpha). This is the conservative
// reading: it never manufactures the false EARLY commit an arbitrary win-first
// ordering could, so promoting on a count summary stays within the alpha bound.
// Ordered streams should feed observations directly to PaceAccumulator.
func EvaluatePaceCounts(cfg PaceConfig, wins, losses int) PaceVerdict {
	if wins < 0 {
		wins = 0
	}
	if losses < 0 {
		losses = 0
	}
	acc := NewPaceAccumulator(cfg)
	for i := 0; i < losses; i++ {
		acc.Observe(PaceLoss)
	}
	for i := 0; i < wins; i++ {
		acc.Observe(PaceWin)
	}
	return acc.Verdict()
}

// PaceGateReason evaluates aggregate candidate-vs-champion counts (via
// EvaluatePaceCounts) and returns the verdict plus a short, human-facing reason for
// the candidate.* event Detail. It is the single call the promotion seam makes so
// the block/commit reason is worded consistently.
func PaceGateReason(cfg PaceConfig, wins, losses int) (PaceVerdict, string) {
	cfg = cfg.withDefaults()
	verdict := EvaluatePaceCounts(cfg, wins, losses)
	threshold := 1.0 / cfg.Alpha
	// Terminal wealth for the (wins,losses) tally — the order-invariant e-process
	// value EvaluatePaceCounts decides on.
	wealth := 1.0
	for i := 0; i < wins; i++ {
		wealth *= 1 + cfg.Lambda
	}
	for i := 0; i < losses; i++ {
		wealth *= 1 - cfg.Lambda
	}
	switch verdict {
	case PaceCommit:
		return verdict, fmt.Sprintf("PACE commit: e-process wealth %.3g >= 1/alpha=%.3g over %d win / %d loss pair(s) (alpha=%.3g, lambda=%.3g)", wealth, threshold, wins, losses, cfg.Alpha, cfg.Lambda)
	case PaceReject:
		return verdict, fmt.Sprintf("PACE reject: pair budget exhausted (%d/%d discordant pairs) with wealth %.3g < 1/alpha=%.3g; notify only", wins+losses, cfg.MaxPairs, wealth, threshold)
	default:
		return verdict, fmt.Sprintf("PACE continue: wealth %.3g < 1/alpha=%.3g after %d/%d discordant pair(s); need more paired outcomes; notify only", wealth, threshold, wins+losses, cfg.MaxPairs)
	}
}
