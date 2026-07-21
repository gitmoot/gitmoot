package skillopt

import (
	"math"
	"math/rand"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
)

// TestPaceDefaultsInLockstepWithConfig pins the numeric PACE defaults duplicated in
// internal/config (which cannot import skillopt without a cycle) to the canonical
// skillopt constants, so the two never drift.
func TestPaceDefaultsInLockstepWithConfig(t *testing.T) {
	if config.DefaultPaceAlpha != DefaultPaceAlpha {
		t.Fatalf("config.DefaultPaceAlpha %v != skillopt %v", config.DefaultPaceAlpha, DefaultPaceAlpha)
	}
	if config.DefaultPaceLambda != DefaultPaceLambda {
		t.Fatalf("config.DefaultPaceLambda %v != skillopt %v", config.DefaultPaceLambda, DefaultPaceLambda)
	}
	if config.DefaultPaceMaxPairs != DefaultPaceMaxPairs {
		t.Fatalf("config.DefaultPaceMaxPairs %v != skillopt %v", config.DefaultPaceMaxPairs, DefaultPaceMaxPairs)
	}
}

// TestPaceCommitsOnConsecutiveWins pins the exact crossing point for the default
// config (alpha=0.05 -> threshold 20, lambda=0.5 -> win x1.5): 1.5^n first reaches
// 20 at n=8 (1.5^7=17.09 < 20 <= 1.5^8=25.63). Before 8 wins the verdict is
// continue; at 8 it commits.
func TestPaceCommitsOnConsecutiveWins(t *testing.T) {
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: DefaultPaceLambda, MaxPairs: DefaultPaceMaxPairs}
	acc := NewPaceAccumulator(cfg)
	for i := 1; i <= 7; i++ {
		if v := acc.Observe(PaceWin); v != PaceContinue {
			t.Fatalf("after %d wins verdict = %v, want continue", i, v)
		}
	}
	if v := acc.Observe(PaceWin); v != PaceCommit {
		t.Fatalf("after 8 wins verdict = %v, want commit", v)
	}
	if !acc.committed {
		t.Fatal("accumulator should have latched committed after 8 wins")
	}
	// Threshold crossed exactly at 8: wealth = 1.5^8.
	if got, want := acc.wealth, math.Pow(1.5, 8); math.Abs(got-want) > 1e-9 {
		t.Fatalf("wealth = %v, want %v", got, want)
	}
}

// TestPaceCommitLatches proves an anytime-valid stop is final: once committed,
// further losses (which would otherwise drag wealth below the threshold) never
// un-commit it.
func TestPaceCommitLatches(t *testing.T) {
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: DefaultPaceLambda, MaxPairs: DefaultPaceMaxPairs}
	acc := NewPaceAccumulator(cfg)
	for i := 0; i < 8; i++ {
		acc.Observe(PaceWin)
	}
	if !acc.committed {
		t.Fatal("precondition: expected commit after 8 wins")
	}
	for i := 0; i < 100; i++ {
		if v := acc.Observe(PaceLoss); v != PaceCommit {
			t.Fatalf("committed accumulator returned %v after a loss, want commit (latched)", v)
		}
	}
	// The wealth is frozen once latched: losses after commit do not move it.
	if got, want := acc.wealth, math.Pow(1.5, 8); math.Abs(got-want) > 1e-9 {
		t.Fatalf("wealth moved after commit: got %v, want frozen %v", got, want)
	}
}

// TestPaceTiesDiscarded proves ties carry no information: an unbounded stream of
// ties never changes wealth, never increments the discordant-pair count, and never
// decides (stays continue forever, even past MaxPairs worth of ties).
func TestPaceTiesDiscarded(t *testing.T) {
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: DefaultPaceLambda, MaxPairs: 5}
	acc := NewPaceAccumulator(cfg)
	for i := 0; i < 1000; i++ {
		if v := acc.Observe(PaceTie); v != PaceContinue {
			t.Fatalf("tie %d produced verdict %v, want continue", i, v)
		}
	}
	if acc.pairs != 0 {
		t.Fatalf("discordant pairs = %d after 1000 ties, want 0", acc.pairs)
	}
	if acc.wealth != 1.0 {
		t.Fatalf("wealth = %v after 1000 ties, want 1 (unchanged)", acc.wealth)
	}
	// Ties interleaved with a real pair must not consume budget: only the win counts.
	if v := acc.Observe(PaceWin); v != PaceContinue {
		t.Fatalf("verdict after 1 win = %v, want continue", v)
	}
	if acc.pairs != 1 {
		t.Fatalf("discordant pairs = %d, want 1 (ties excluded)", acc.pairs)
	}
}

// TestPaceLambdaZeroIsNoOp proves lambda=0 is a legal no-op: wealth never moves off
// 1, so the e-process can never commit no matter how many wins arrive; it rejects
// only when the pair budget is exhausted.
func TestPaceLambdaZeroIsNoOp(t *testing.T) {
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: 0, MaxPairs: 10}
	acc := NewPaceAccumulator(cfg)
	for i := 0; i < 9; i++ {
		if v := acc.Observe(PaceWin); v != PaceContinue {
			t.Fatalf("lambda=0 win %d verdict = %v, want continue", i, v)
		}
		if acc.wealth != 1.0 {
			t.Fatalf("lambda=0 wealth = %v, want 1 (no-op)", acc.wealth)
		}
	}
	// 10th discordant pair exhausts the budget without ever crossing -> reject.
	if v := acc.Observe(PaceWin); v != PaceReject {
		t.Fatalf("lambda=0 at budget verdict = %v, want reject", v)
	}
}

// TestPaceRejectsOnBudgetExhaustion proves an all-loss (or mixed non-decisive)
// stream rejects exactly when MaxPairs discordant pairs are consumed without a
// crossing.
func TestPaceRejectsOnBudgetExhaustion(t *testing.T) {
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: DefaultPaceLambda, MaxPairs: 6}
	acc := NewPaceAccumulator(cfg)
	for i := 1; i <= 5; i++ {
		if v := acc.Observe(PaceLoss); v != PaceContinue {
			t.Fatalf("after %d losses verdict = %v, want continue", i, v)
		}
	}
	if v := acc.Observe(PaceLoss); v != PaceReject {
		t.Fatalf("after 6 losses (budget) verdict = %v, want reject", v)
	}
}

// TestPaceDeterministicSequenceOrderIndependentTerminal proves the streaming
// accumulator's terminal wealth is order-independent (multiplication commutes):
// wins-first and losses-first give the same final wealth for the same counts. The
// commit MOMENT differs, but the terminal value does not.
func TestPaceDeterministicSequenceOrderIndependentTerminal(t *testing.T) {
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: DefaultPaceLambda, MaxPairs: DefaultPaceMaxPairs}
	winsFirst := NewPaceAccumulator(cfg)
	for i := 0; i < 3; i++ {
		winsFirst.Observe(PaceWin)
	}
	for i := 0; i < 3; i++ {
		winsFirst.Observe(PaceLoss)
	}
	lossFirst := NewPaceAccumulator(cfg)
	for i := 0; i < 3; i++ {
		lossFirst.Observe(PaceLoss)
	}
	for i := 0; i < 3; i++ {
		lossFirst.Observe(PaceWin)
	}
	if math.Abs(winsFirst.wealth-lossFirst.wealth) > 1e-9 {
		t.Fatalf("terminal wealth differs by order: wins-first %v vs loss-first %v", winsFirst.wealth, lossFirst.wealth)
	}
	// 3 wins (x1.5) and 3 losses (x0.5): 1.5^3 * 0.5^3 = 3.375 * 0.125 = 0.421875.
	if got := lossFirst.wealth; math.Abs(got-0.421875) > 1e-9 {
		t.Fatalf("terminal wealth = %v, want 0.421875", got)
	}
}

// TestEvaluatePaceCountsCommitsOnDominantCandidate proves a clearly-better candidate
// (many more wins than losses) commits via the count summary.
func TestEvaluatePaceCountsCommitsOnDominantCandidate(t *testing.T) {
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: DefaultPaceLambda, MaxPairs: DefaultPaceMaxPairs}
	// 10 wins, 1 loss: 1.5^10 * 0.5 = 57.67 * 0.5 = 28.8 >= 20 -> commit.
	if v := EvaluatePaceCounts(cfg, 10, 1); v != PaceCommit {
		t.Fatalf("dominant candidate verdict = %v, want commit", v)
	}
}

// TestEvaluatePaceCountsConservativeVsStream guards the count-summary reading:
// terminal wealth below the threshold must not commit even if wins-first order
// would have crossed it transiently.
func TestEvaluatePaceCountsConservativeVsStream(t *testing.T) {
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: DefaultPaceLambda, MaxPairs: DefaultPaceMaxPairs}
	if v := EvaluatePaceCounts(cfg, 8, 2); v == PaceCommit {
		t.Fatalf("EvaluatePaceCounts committed on terminal wealth below threshold (wins=8 losses=2)")
	}
}

// TestEvaluatePaceCountsNegativeClamped proves negative counts are clamped to zero
// (a defensive guard for a malformed arm) and decide continue with no evidence.
func TestEvaluatePaceCountsNegativeClamped(t *testing.T) {
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: DefaultPaceLambda, MaxPairs: DefaultPaceMaxPairs}
	if v := EvaluatePaceCounts(cfg, -5, -3); v != PaceContinue {
		t.Fatalf("negative counts verdict = %v, want continue (clamped to zero evidence)", v)
	}
}

// TestPaceConfigDefaults proves clearly-invalid config fields fall back to the RFC
// defaults while a legal lambda=0 is preserved.
func TestPaceConfigDefaults(t *testing.T) {
	got := PaceConfig{Alpha: 0, Lambda: -1, MaxPairs: 0}.withDefaults()
	if got.Alpha != DefaultPaceAlpha || got.Lambda != DefaultPaceLambda || got.MaxPairs != DefaultPaceMaxPairs {
		t.Fatalf("invalid config defaulted to %+v, want the Default* constants", got)
	}
	kept := PaceConfig{Alpha: 0.1, Lambda: 0, MaxPairs: 12}.withDefaults()
	if kept.Alpha != 0.1 || kept.Lambda != 0 || kept.MaxPairs != 12 {
		t.Fatalf("legal config was altered: %+v (lambda=0 must be preserved)", kept)
	}
}

// TestPaceMartingaleFalseCommitRateBounded is the martingale property check: under
// H0 (each discordant pair is a candidate win with probability 1/2), the average
// terminal wealth stays ~1 (E[E] <= 1) and the fraction of runs that ever commit is
// bounded by alpha (Ville's inequality). We run many independent 50/50 histories
// and assert both.
func TestPaceMartingaleFalseCommitRateBounded(t *testing.T) {
	const (
		alpha = 0.05
		runs  = 20000
		pairs = 500
	)
	cfg := PaceConfig{Alpha: alpha, Lambda: DefaultPaceLambda, MaxPairs: pairs}
	rng := rand.New(rand.NewSource(42))
	commits := 0
	sumWealth := 0.0
	for r := 0; r < runs; r++ {
		acc := NewPaceAccumulator(cfg)
		for p := 0; p < pairs; p++ {
			outcome := PaceLoss
			if rng.Intn(2) == 1 {
				outcome = PaceWin
			}
			if acc.Observe(outcome) == PaceCommit {
				break
			}
		}
		if acc.committed {
			commits++
		}
		sumWealth += acc.wealth
	}
	falseRate := float64(commits) / float64(runs)
	// Ville: P(ever commit) <= alpha. Allow a small Monte-Carlo margin.
	if falseRate > alpha+0.01 {
		t.Fatalf("false-commit rate %.4f exceeds alpha=%.2f (+margin)", falseRate, alpha)
	}
	// A valid e-process has E[terminal wealth] <= 1 (supermartingale). The empirical
	// mean should sit at or below ~1 (allow a modest upper margin for finite runs and
	// the heavy right tail; it must not blow up).
	meanWealth := sumWealth / float64(runs)
	if meanWealth > 1.5 {
		t.Fatalf("mean terminal wealth %.4f is implausibly high for a fair martingale", meanWealth)
	}
}
