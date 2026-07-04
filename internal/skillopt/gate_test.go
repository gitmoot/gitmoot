package skillopt

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseGateCorpusValid(t *testing.T) {
	data := []byte(`{
		"kind": "gitmoot-skillopt-gate-corpus",
		"version": 3,
		"replay_command": "sh replay.sh",
		"items": [
			{"id": "a", "prompt": "do a", "expected": "a-ok"},
			{"id": "b", "prompt": "do b"}
		]
	}`)
	corpus, err := ParseGateCorpus(data)
	if err != nil {
		t.Fatalf("ParseGateCorpus returned error: %v", err)
	}
	if corpus.Version != 3 || len(corpus.Items) != 2 || corpus.ReplayCommand != "sh replay.sh" {
		t.Fatalf("unexpected corpus: %+v", corpus)
	}
}

func TestParseGateCorpusRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":        ``,
		"wrong kind":   `{"kind":"other","version":1,"items":[{"id":"a","prompt":"p"}]}`,
		"zero version": `{"kind":"gitmoot-skillopt-gate-corpus","version":0,"items":[{"id":"a","prompt":"p"}]}`,
		"no items":     `{"kind":"gitmoot-skillopt-gate-corpus","version":1,"items":[]}`,
		"empty id":     `{"kind":"gitmoot-skillopt-gate-corpus","version":1,"items":[{"id":"","prompt":"p"}]}`,
		"dup id":       `{"kind":"gitmoot-skillopt-gate-corpus","version":1,"items":[{"id":"a","prompt":"p"},{"id":"a","prompt":"q"}]}`,
		"empty prompt": `{"kind":"gitmoot-skillopt-gate-corpus","version":1,"items":[{"id":"a","prompt":"   "}]}`,
		"invalid json": `{not json`,
	}
	for name, body := range cases {
		if _, err := ParseGateCorpus([]byte(body)); err == nil {
			t.Errorf("%s: ParseGateCorpus accepted a malformed corpus", name)
		}
	}
}

func TestGateReplayResultScoreReusesDeterministicMetrics(t *testing.T) {
	// Hard-verifier pass -> 1.0, fail -> 0.0 (projectHardVerifier).
	pass := GateReplayResult{HardVerifier: true, HardPassed: true}
	if s, ok := pass.Score(); !ok || s != 1.0 {
		t.Fatalf("hard pass score = %v (ok=%v), want 1.0", s, ok)
	}
	fail := GateReplayResult{HardVerifier: true, HardPassed: false}
	if s, ok := fail.Score(); !ok || s != 0.0 {
		t.Fatalf("hard fail score = %v (ok=%v), want 0.0", s, ok)
	}
	// Checker rubric -> mean of dimensions (projectChecker).
	checker := GateReplayResult{Rubric: map[string]float64{"a": 1.0, "b": 0.0}}
	if s, ok := checker.Score(); !ok || s != 0.5 {
		t.Fatalf("checker score = %v (ok=%v), want 0.5", s, ok)
	}
	// An empty result is not scorable.
	if _, ok := (GateReplayResult{}).Score(); ok {
		t.Fatalf("empty result reported HasScore=true")
	}
}

func TestEvaluateGateStrictImprovementPasses(t *testing.T) {
	champion := []GateItemScore{{ItemID: "a", Score: 0.5, HasScore: true}, {ItemID: "b", Score: 0.5, HasScore: true}}
	candidate := []GateItemScore{{ItemID: "a", Score: 0.9, HasScore: true}, {ItemID: "b", Score: 0.6, HasScore: true}}
	verdict := EvaluateGate(champion, candidate)
	if !verdict.Pass {
		t.Fatalf("strict improvement should pass; reason=%q", verdict.Reason)
	}
	if verdict.CandidateMean <= verdict.ChampionMean {
		t.Fatalf("candidate mean %.3f not > champion mean %.3f", verdict.CandidateMean, verdict.ChampionMean)
	}
	if len(verdict.Deltas) != 2 {
		t.Fatalf("deltas = %d, want 2", len(verdict.Deltas))
	}
}

// TestEvaluateGateTieFails is the load-bearing "tie = fail" rule (AutoMem A.2):
// parity is not improvement. This is the assertion the invert-the-comparison
// mutation flips.
func TestEvaluateGateTieFails(t *testing.T) {
	scores := []GateItemScore{{ItemID: "a", Score: 0.7, HasScore: true}, {ItemID: "b", Score: 0.3, HasScore: true}}
	verdict := EvaluateGate(scores, scores)
	if verdict.Pass {
		t.Fatalf("a tie (identical means) must FAIL the gate, got pass")
	}
	if verdict.CandidateMean != verdict.ChampionMean {
		t.Fatalf("expected equal means, got champ=%.3f cand=%.3f", verdict.ChampionMean, verdict.CandidateMean)
	}
}

func TestEvaluateGateWorseCandidateFails(t *testing.T) {
	champion := []GateItemScore{{ItemID: "a", Score: 0.9, HasScore: true}}
	candidate := []GateItemScore{{ItemID: "a", Score: 0.4, HasScore: true}}
	if EvaluateGate(champion, candidate).Pass {
		t.Fatalf("a worse candidate must fail the gate")
	}
}

func TestEvaluateGateMissingScoreFails(t *testing.T) {
	champion := []GateItemScore{{ItemID: "a", Score: 0.5, HasScore: true}}
	// Candidate has no scorable outcome for the item.
	candidate := []GateItemScore{{ItemID: "a", HasScore: false}}
	verdict := EvaluateGate(champion, candidate)
	if verdict.Pass {
		t.Fatalf("an unscorable candidate item must fail the gate")
	}
	if !strings.Contains(verdict.Reason, "candidate has no scorable") {
		t.Fatalf("unexpected reason: %q", verdict.Reason)
	}
}

func TestEvaluateGateEmptyCorpusFails(t *testing.T) {
	if EvaluateGate(nil, nil).Pass {
		t.Fatalf("an empty corpus must fail the gate")
	}
}

// TestStrictlyImprovesIsTheSingleComparison guards the exact predicate the
// deliberately-worse-candidate mutation targets: strictly-greater, tie excluded.
func TestStrictlyImprovesIsTheSingleComparison(t *testing.T) {
	if !strictlyImproves(0.5, 0.6) {
		t.Fatalf("0.6 should strictly improve over 0.5")
	}
	if strictlyImproves(0.5, 0.5) {
		t.Fatalf("a tie must NOT be a strict improvement")
	}
	if strictlyImproves(0.6, 0.5) {
		t.Fatalf("a regression must NOT be a strict improvement")
	}
}

// fixedReplay returns a replay func that maps a template content to a fixed set of
// per-item scores, and records the failing-log content passed to the optimizer.
func fixedReplay(scoresByContent map[string][]GateItemScore) GateReplayFunc {
	return func(_ context.Context, content string) ([]GateItemScore, GateReplayLog, error) {
		scores, ok := scoresByContent[content]
		if !ok {
			return nil, GateReplayLog{}, errors.New("no scripted scores for content")
		}
		results := make([]GateReplayResult, 0, len(scores))
		for _, s := range scores {
			results = append(results, GateReplayResult{ItemID: s.ItemID, Rubric: map[string]float64{"q": s.Score}})
		}
		return scores, GateReplayLog{Results: results}, nil
	}
}

func TestRunGateProtocolPassesOnAttemptOne(t *testing.T) {
	champion := []GateItemScore{{ItemID: "a", Score: 0.5, HasScore: true}}
	replay := fixedReplay(map[string][]GateItemScore{
		"cand": {{ItemID: "a", Score: 0.9, HasScore: true}},
	})
	out, err := RunGateProtocol(context.Background(), champion, "cand", replay, nil)
	if err != nil {
		t.Fatalf("RunGateProtocol returned error: %v", err)
	}
	if !out.Accepted || len(out.Attempts) != 1 || out.FinalContent != "cand" {
		t.Fatalf("expected accept on attempt 1; got %+v", out)
	}
}

func TestRunGateProtocolRetrySucceeds(t *testing.T) {
	champion := []GateItemScore{{ItemID: "a", Score: 0.5, HasScore: true}}
	replay := fixedReplay(map[string][]GateItemScore{
		"cand-v1": {{ItemID: "a", Score: 0.4, HasScore: true}}, // fails (worse)
		"cand-v2": {{ItemID: "a", Score: 0.8, HasScore: true}}, // retry passes
	})
	var fedLog GateReplayLog
	optimize := func(_ context.Context, failing GateReplayLog) (string, error) {
		fedLog = failing
		return "cand-v2", nil
	}
	out, err := RunGateProtocol(context.Background(), champion, "cand-v1", replay, optimize)
	if err != nil {
		t.Fatalf("RunGateProtocol returned error: %v", err)
	}
	if !out.Accepted || len(out.Attempts) != 2 || out.FinalContent != "cand-v2" {
		t.Fatalf("expected accept on retry; got accepted=%v attempts=%d final=%q", out.Accepted, len(out.Attempts), out.FinalContent)
	}
	// The optimizer must have received the FAILING attempt-1 replay log (its content
	// and its failed verdict), which is what AutoMem A.2 feeds back.
	if fedLog.TemplateContent != "cand-v1" || fedLog.Verdict.Pass {
		t.Fatalf("optimizer fed the wrong log: content=%q pass=%v", fedLog.TemplateContent, fedLog.Verdict.Pass)
	}
}

func TestRunGateProtocolRejectsAfterSecondFailure(t *testing.T) {
	champion := []GateItemScore{{ItemID: "a", Score: 0.5, HasScore: true}}
	replay := fixedReplay(map[string][]GateItemScore{
		"cand-v1": {{ItemID: "a", Score: 0.4, HasScore: true}},
		"cand-v2": {{ItemID: "a", Score: 0.45, HasScore: true}}, // still worse
	})
	optimize := func(_ context.Context, _ GateReplayLog) (string, error) { return "cand-v2", nil }
	out, err := RunGateProtocol(context.Background(), champion, "cand-v1", replay, optimize)
	if err != nil {
		t.Fatalf("RunGateProtocol returned error: %v", err)
	}
	if out.Accepted {
		t.Fatalf("a second failure must REJECT; got accepted")
	}
	if len(out.Attempts) != 2 {
		t.Fatalf("expected exactly 2 attempts (one retry), got %d", len(out.Attempts))
	}
	if !strings.Contains(out.Reason, "twice") {
		t.Fatalf("unexpected reject reason: %q", out.Reason)
	}
}

func TestRunGateProtocolNoOptimizerRejectsWithoutRetry(t *testing.T) {
	champion := []GateItemScore{{ItemID: "a", Score: 0.5, HasScore: true}}
	replay := fixedReplay(map[string][]GateItemScore{
		"cand": {{ItemID: "a", Score: 0.4, HasScore: true}},
	})
	out, err := RunGateProtocol(context.Background(), champion, "cand", replay, nil)
	if err != nil {
		t.Fatalf("RunGateProtocol returned error: %v", err)
	}
	if out.Accepted || len(out.Attempts) != 1 {
		t.Fatalf("no optimizer => single attempt, reject; got accepted=%v attempts=%d", out.Accepted, len(out.Attempts))
	}
}

func TestGatePromotionGuard(t *testing.T) {
	if blocked, _ := GatePromotionGuard(false, false); blocked {
		t.Fatalf("gate off must never block")
	}
	if blocked, _ := GatePromotionGuard(true, true); blocked {
		t.Fatalf("gate on with an accepted run must not block")
	}
	blocked, reason := GatePromotionGuard(true, false)
	if !blocked {
		t.Fatalf("gate on without an accepted run must block")
	}
	if !strings.Contains(reason, "gate run") {
		t.Fatalf("unexpected block reason: %q", reason)
	}
}
