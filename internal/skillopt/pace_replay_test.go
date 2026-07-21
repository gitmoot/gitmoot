package skillopt

import (
	"math/rand"
	"testing"
)

// replayHistory drives a fresh PACE accumulator through an ordered synthetic
// candidate-vs-champion history (peeking after every pair, as an online
// anytime-valid test does) and returns the verdict plus how many DISCORDANT pairs
// were consumed before it stopped (the "early stop" measurement). Ties advance the
// history but never consume budget or move wealth.
func replayHistory(cfg PaceConfig, history []PaceOutcome) (PaceVerdict, int) {
	acc := NewPaceAccumulator(cfg)
	for _, o := range history {
		v := acc.Observe(o)
		if v == PaceCommit || v == PaceReject {
			return v, acc.pairs
		}
	}
	return acc.Verdict(), acc.pairs
}

// bernoulliHistory builds a length-n history where each discordant pair is a
// candidate win with probability p (no ties): the synthetic paired signal for a
// candidate whose true per-pair win rate is p.
func bernoulliHistory(rng *rand.Rand, n int, p float64) []PaceOutcome {
	out := make([]PaceOutcome, n)
	for i := range out {
		if rng.Float64() < p {
			out[i] = PaceWin
		} else {
			out[i] = PaceLoss
		}
	}
	return out
}

// TestPaceReplayClearlyBetterCandidateCommitsEarly is the accept-side of the replay
// harness (#687): a clearly-better candidate (true win rate 0.85) commits, and it
// commits EARLY — far inside the pair budget — demonstrating the anytime-valid
// early-stop that makes PACE cheaper than a fixed "run N" gate. Averaged over many
// independent synthetic histories the mean stopping point is a small number of
// pairs.
func TestPaceReplayClearlyBetterCandidateCommitsEarly(t *testing.T) {
	const (
		runs    = 2000
		budget  = 200
		winRate = 0.85
	)
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: DefaultPaceLambda, MaxPairs: budget}
	rng := rand.New(rand.NewSource(7))
	commits := 0
	totalPairs := 0
	maxPairs := 0
	for r := 0; r < runs; r++ {
		verdict, pairs := replayHistory(cfg, bernoulliHistory(rng, budget, winRate))
		if verdict == PaceCommit {
			commits++
			totalPairs += pairs
			if pairs > maxPairs {
				maxPairs = pairs
			}
		}
	}
	// A strongly-better candidate should almost always commit.
	commitRate := float64(commits) / float64(runs)
	if commitRate < 0.95 {
		t.Fatalf("clearly-better candidate committed in only %.1f%% of runs, want >= 95%%", commitRate*100)
	}
	// And it stops EARLY: the average committing run uses a small fraction of budget.
	avgPairs := float64(totalPairs) / float64(commits)
	if avgPairs > 40 {
		t.Fatalf("mean commit stopping point %.1f pairs is not 'early' (budget %d)", avgPairs, budget)
	}
	t.Logf("clearly-better candidate: commit rate %.1f%%, mean stop %.1f pairs, max stop %d (budget %d)", commitRate*100, avgPairs, maxPairs, budget)
}

// TestPaceReplayFiftyFiftyNoiseRarelyCommits is the false-commit-control side of the
// replay harness (#687): a candidate that is NO better than the champion (true win
// rate 0.5, pure noise) must almost never commit within budget — the whole point of
// the anytime-valid gate is that peeking-until-you-win does NOT p-hack a promotion.
// The empirical false-commit rate is bounded by alpha (Ville).
func TestPaceReplayFiftyFiftyNoiseRarelyCommits(t *testing.T) {
	const (
		runs   = 20000
		budget = 200
		alpha  = 0.05
	)
	cfg := PaceConfig{Alpha: alpha, Lambda: DefaultPaceLambda, MaxPairs: budget}
	rng := rand.New(rand.NewSource(99))
	commits := 0
	rejects := 0
	for r := 0; r < runs; r++ {
		verdict, _ := replayHistory(cfg, bernoulliHistory(rng, budget, 0.5))
		switch verdict {
		case PaceCommit:
			commits++
		case PaceReject:
			rejects++
		}
	}
	falseRate := float64(commits) / float64(runs)
	if falseRate > alpha+0.01 {
		t.Fatalf("50/50 noise false-commit rate %.4f exceeds alpha=%.2f (+margin)", falseRate, alpha)
	}
	// The overwhelming majority of pure-noise candidates should be correctly rejected
	// at budget (the safe failure mode).
	rejectRate := float64(rejects) / float64(runs)
	if rejectRate < 0.90 {
		t.Fatalf("50/50 noise reject rate %.4f too low; want the safe reject to dominate", rejectRate)
	}
	t.Logf("50/50 noise: false-commit rate %.4f (alpha %.2f), reject rate %.4f", falseRate, alpha, rejectRate)
}

// TestPaceReplayDeterministic proves the replay harness is deterministic for a fixed
// seed: identical histories yield identical (verdict, stop) results across runs, so
// a recorded candidate history is reproducible.
func TestPaceReplayDeterministic(t *testing.T) {
	cfg := PaceConfig{Alpha: DefaultPaceAlpha, Lambda: DefaultPaceLambda, MaxPairs: 200}
	history := bernoulliHistory(rand.New(rand.NewSource(1234)), 200, 0.8)
	v1, p1 := replayHistory(cfg, history)
	v2, p2 := replayHistory(cfg, history)
	if v1 != v2 || p1 != p2 {
		t.Fatalf("replay not deterministic: (%v,%d) vs (%v,%d)", v1, p1, v2, p2)
	}
}
