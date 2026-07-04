package skillopt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/workflow"
)

// GateCorpusKind is the fixed kind tag every replay-gate corpus file carries so a
// stray JSON document is never mistaken for a corpus (#627). It mirrors the
// candidate/training package kind discipline.
const GateCorpusKind = "gitmoot-skillopt-gate-corpus"

// GateCorpusItem is one fixed, deterministic (job fixture -> expected/verifiable
// outcome) entry in the replay corpus (#627). The Prompt is the job fixture fed to
// the replay driver; Expected is the verifiable expectation the deterministic
// replay command scores against (a build/test target, a golden diff, a checker
// bound). Neither is interpreted by the gate itself — they are handed verbatim to
// the deterministic replay command, which owns the actual scoring — so the corpus
// stays a plain, versioned data file with no live-LLM coupling.
type GateCorpusItem struct {
	ID       string `json:"id"`
	Prompt   string `json:"prompt"`
	Expected string `json:"expected,omitempty"`
}

// GateCorpus is the versioned, fixed job corpus a candidate template is replayed
// against before it is allowed to proceed to canary (#627, AutoMem A.2). It is a
// plain data file (or config-embedded document): the same fixed seeds every replay
// runs on, so the champion and candidate are compared on identical inputs. The
// ReplayCommand, when set, is the corpus-scoped deterministic replay driver
// (`sh -c`); a caller/config default is used when it is empty.
type GateCorpus struct {
	Kind          string           `json:"kind"`
	Version       int              `json:"version"`
	ReplayCommand string           `json:"replay_command,omitempty"`
	Items         []GateCorpusItem `json:"items"`
}

// ParseGateCorpus decodes and validates a corpus document (#627). It is strict: the
// kind must match, the version must be >= 1 (a corpus is versioned so a scoring
// change is auditable), there must be at least one item, and every item must carry
// a unique non-empty id and a non-empty prompt. A malformed corpus is rejected
// rather than silently scoring an empty/duplicated set.
func ParseGateCorpus(data []byte) (GateCorpus, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return GateCorpus{}, errors.New("gate corpus is empty")
	}
	var corpus GateCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		return GateCorpus{}, fmt.Errorf("decode gate corpus: %w", err)
	}
	if corpus.Kind != GateCorpusKind {
		return GateCorpus{}, fmt.Errorf("gate corpus kind must be %q, got %q", GateCorpusKind, corpus.Kind)
	}
	if corpus.Version < 1 {
		return GateCorpus{}, fmt.Errorf("gate corpus version must be >= 1, got %d", corpus.Version)
	}
	if len(corpus.Items) == 0 {
		return GateCorpus{}, errors.New("gate corpus has no items")
	}
	seen := make(map[string]struct{}, len(corpus.Items))
	for i, item := range corpus.Items {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			return GateCorpus{}, fmt.Errorf("gate corpus item %d has an empty id", i)
		}
		if _, dup := seen[id]; dup {
			return GateCorpus{}, fmt.Errorf("gate corpus item id %q is duplicated", id)
		}
		seen[id] = struct{}{}
		if strings.TrimSpace(item.Prompt) == "" {
			return GateCorpus{}, fmt.Errorf("gate corpus item %q has an empty prompt", id)
		}
	}
	return corpus, nil
}

// LoadGateCorpus reads and validates a corpus file from disk (#627).
func LoadGateCorpus(path string) (GateCorpus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GateCorpus{}, fmt.Errorf("read gate corpus %q: %w", path, err)
	}
	return ParseGateCorpus(data)
}

// GateReplayResult is the deterministic per-item outcome a replay driver emits for
// one corpus item (#627). It is intentionally the SAME shape the hard-verifier
// (#474) and deterministic-checker (#485) tiers already produce — a per-command/
// per-dimension Rubric and/or a binary hard verdict — so the gate reuses the
// EXISTING deterministic scorers (projectHardVerifier / projectChecker) instead of
// inventing a new judge. The replay command owns producing this verdict
// deterministically (no live LLM in the gate itself).
type GateReplayResult struct {
	// ItemID is the corpus item this result scores; the driver stamps it so a
	// misaligned emit is caught.
	ItemID string `json:"item_id"`
	// Rubric carries per-dimension [0,1] scores (the deterministic-checker path) or
	// per-command pass/fail (the hard-verifier audit map).
	Rubric map[string]float64 `json:"rubric,omitempty"`
	// HardVerifier marks this as an authoritative binary hard verdict (#474) rather
	// than a soft objective rubric (#485); the gate scores it via projectHardVerifier.
	HardVerifier bool `json:"hard_verifier,omitempty"`
	// HardPassed is the binary verdict, meaningful only when HardVerifier is true.
	HardPassed bool `json:"hard_passed,omitempty"`
	// Findings is free-text audit detail carried into the replay log.
	Findings string `json:"findings,omitempty"`
}

// outcome maps the replay result onto the internal workflow.Outcome the existing
// deterministic scorers consume, so the gate never invents a new scoring scale.
func (r GateReplayResult) outcome() workflow.Outcome {
	outcome := workflow.Outcome{
		Kind:     workflow.OutcomeReviewed,
		Rubric:   r.Rubric,
		Findings: r.Findings,
	}
	if r.HardVerifier {
		outcome.HardVerifier = true
		outcome.HardPassed = r.HardPassed
	} else {
		outcome.Objective = true
	}
	return outcome
}

// Signal projects the replay result into the SAME NormalizedSignal the auto-trace
// harvester uses (#627): a hard-verifier result scores via projectHardVerifier (a
// pass -> 1.0, a fail -> 0.0), any other result via projectChecker (the mean of the
// deterministic-checker dimensions). An all-empty result yields HasScore=false.
func (r GateReplayResult) Signal() NormalizedSignal {
	outcome := r.outcome()
	if outcome.HardVerifier {
		return projectHardVerifier(outcome)
	}
	return projectChecker(outcome)
}

// Score returns the projected [0,1] quality score and whether it was scorable at
// all, reusing the existing deterministic metrics.
func (r GateReplayResult) Score() (float64, bool) {
	signal := r.Signal()
	return signal.Score, signal.HasScore
}

// GateItemScore is a single corpus item's projected [0,1] deterministic score for
// one side (champion or candidate) of the comparison (#627).
type GateItemScore struct {
	ItemID   string
	Score    float64
	HasScore bool
}

// GateItemDelta is the per-item champion->candidate movement, surfaced in the
// verdict + persisted for audit so a reviewer sees exactly which fixtures the
// candidate improved or regressed on.
type GateItemDelta struct {
	ItemID         string  `json:"item_id"`
	ChampionScore  float64 `json:"champion_score"`
	CandidateScore float64 `json:"candidate_score"`
	Delta          float64 `json:"delta"`
	Regressed      bool    `json:"regressed"`
}

// GateVerdict is the pure result of EvaluateGate: pass/fail on STRICT improvement
// plus the aggregate means and per-item deltas (#627). It performs no I/O.
type GateVerdict struct {
	Pass          bool            `json:"pass"`
	Reason        string          `json:"reason"`
	ChampionMean  float64         `json:"champion_mean"`
	CandidateMean float64         `json:"candidate_mean"`
	Deltas        []GateItemDelta `json:"deltas"`
}

// strictlyImproves is the SINGLE comparison the gate turns on (#627, AutoMem A.2):
// a candidate is accepted ONLY when its corpus mean is STRICTLY greater than the
// champion's on the same fixed corpus. A tie (equal means) is NOT an improvement
// and FAILS — parity never earns a promotion. The deliberately-worse-candidate
// mutation test inverts exactly this comparison (a worse candidate would then
// pass), so keeping the rule in one named, total function makes the mutation crisp.
func strictlyImproves(championMean, candidateMean float64) bool {
	return candidateMean > championMean
}

// EvaluateGate compares the champion's and candidate's per-item deterministic
// scores over the SAME corpus and returns a strict-improvement verdict (#627). It
// is pure and total. The corpus item set is the union of the two sides' ids; every
// item MUST carry a scorable value on BOTH sides — a missing/unscorable item on
// either side fails the gate (we can never confirm improvement on a fixture we
// could not score on both templates). The primary decision is the aggregate strict
// improvement (strictlyImproves); the per-item deltas are computed for audit.
func EvaluateGate(champion, candidate []GateItemScore) GateVerdict {
	champ := indexGateScores(champion)
	cand := indexGateScores(candidate)
	ids := sortedGateItemIDs(champ, cand)
	if len(ids) == 0 {
		return GateVerdict{Pass: false, Reason: "empty corpus: no items to compare"}
	}

	deltas := make([]GateItemDelta, 0, len(ids))
	var champSum, candSum float64
	for _, id := range ids {
		c, cOK := champ[id]
		if !cOK || !c.HasScore {
			return GateVerdict{
				Pass:   false,
				Reason: fmt.Sprintf("champion has no scorable outcome for corpus item %q; cannot confirm improvement", id),
				Deltas: deltas,
			}
		}
		d, dOK := cand[id]
		if !dOK || !d.HasScore {
			return GateVerdict{
				Pass:   false,
				Reason: fmt.Sprintf("candidate has no scorable outcome for corpus item %q; cannot confirm improvement", id),
				Deltas: deltas,
			}
		}
		delta := d.Score - c.Score
		deltas = append(deltas, GateItemDelta{
			ItemID:         id,
			ChampionScore:  c.Score,
			CandidateScore: d.Score,
			Delta:          delta,
			Regressed:      delta < 0,
		})
		champSum += c.Score
		candSum += d.Score
	}

	n := float64(len(ids))
	champMean := champSum / n
	candMean := candSum / n
	verdict := GateVerdict{ChampionMean: champMean, CandidateMean: candMean, Deltas: deltas}

	if !strictlyImproves(champMean, candMean) {
		verdict.Pass = false
		verdict.Reason = fmt.Sprintf("candidate corpus mean %.4f is not a strict improvement over champion %.4f (tie or regression fails)", candMean, champMean)
		return verdict
	}
	verdict.Pass = true
	verdict.Reason = fmt.Sprintf("candidate corpus mean %.4f strictly improves over champion %.4f (+%.4f)", candMean, champMean, candMean-champMean)
	return verdict
}

// indexGateScores keys a score slice by item id. A duplicate id keeps the LAST
// occurrence (a replay re-run overwrites in place), matching the corpus's unique-id
// invariant.
func indexGateScores(scores []GateItemScore) map[string]GateItemScore {
	byID := make(map[string]GateItemScore, len(scores))
	for _, score := range scores {
		byID[strings.TrimSpace(score.ItemID)] = score
	}
	return byID
}

// sortedGateItemIDs returns the sorted union of the two sides' item ids so the
// comparison and the persisted deltas are deterministic regardless of replay order.
func sortedGateItemIDs(a, b map[string]GateItemScore) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for id := range a {
		seen[id] = struct{}{}
	}
	for id := range b {
		seen[id] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// GateReplayLog is the record of one replay pass over the corpus (#627): the
// template content that was replayed, the raw per-item deterministic results, and
// the resulting verdict. On a gate FAILURE it is the "failing eval log" fed back to
// the optimizer step for the single allowed retry (AutoMem A.2).
type GateReplayLog struct {
	TemplateContent string             `json:"template_content,omitempty"`
	Results         []GateReplayResult `json:"results"`
	Verdict         GateVerdict        `json:"verdict"`
}

// GateAttempt records one attempt of the gate protocol (attempt 1, or the single
// retry as attempt 2) for audit.
type GateAttempt struct {
	Attempt          int           `json:"attempt"`
	CandidateContent string        `json:"-"`
	Verdict          GateVerdict   `json:"verdict"`
	Log              GateReplayLog `json:"log"`
}

// GateProtocolOutcome is the terminal result of RunGateProtocol (#627): accepted or
// rejected, the accepted template content (empty on reject), the per-attempt
// history, and a human-facing reason.
type GateProtocolOutcome struct {
	Accepted     bool          `json:"accepted"`
	FinalContent string        `json:"-"`
	Attempts     []GateAttempt `json:"attempts"`
	Reason       string        `json:"reason"`
}

// GateReplayFunc replays a template's content against the fixed corpus and returns
// its per-item deterministic scores plus the raw replay log. It is injected so the
// pure protocol is testable with a deterministic fake (no subprocess) and the CLI
// supplies the real shell-runtime deterministic driver.
type GateReplayFunc func(ctx context.Context, templateContent string) ([]GateItemScore, GateReplayLog, error)

// GateOptimizeFunc re-runs the optimizer step feeding it the FAILING replay log and
// returns a revised candidate template content for the single allowed retry (#627).
type GateOptimizeFunc func(ctx context.Context, failing GateReplayLog) (string, error)

// RunGateProtocol runs the AutoMem A.2 pre-canary gate protocol (#627): replay the
// candidate against the fixed corpus, accept ONLY on strict improvement over the
// champion on the SAME corpus; on failure take EXACTLY ONE retry that feeds the
// failing replay log back to the optimizer step and re-gates the revised candidate;
// a second failure is a REJECT (a clean restart is the caller's choice, not this
// function's). The champion is scored ONCE by the caller (championScores) since it
// does not change between attempts. It is pure orchestration over injected replay/
// optimize funcs — no I/O of its own — so the one-retry protocol is unit-testable.
func RunGateProtocol(ctx context.Context, championScores []GateItemScore, candidateContent string, replay GateReplayFunc, optimize GateOptimizeFunc) (GateProtocolOutcome, error) {
	if replay == nil {
		return GateProtocolOutcome{}, errors.New("gate protocol requires a replay function")
	}

	out := GateProtocolOutcome{}

	// Attempt 1: replay the candidate as produced.
	verdict, log, err := runGateAttempt(ctx, championScores, candidateContent, replay)
	if err != nil {
		return out, fmt.Errorf("gate attempt 1 replay: %w", err)
	}
	out.Attempts = append(out.Attempts, GateAttempt{Attempt: 1, CandidateContent: candidateContent, Verdict: verdict, Log: log})
	if verdict.Pass {
		out.Accepted = true
		out.FinalContent = candidateContent
		out.Reason = "gate passed on attempt 1: " + verdict.Reason
		return out, nil
	}

	// No optimizer wired: there is no retry to take, so the failure is terminal.
	if optimize == nil {
		out.Reason = "gate failed on attempt 1 and no optimizer retry is configured; rejecting: " + verdict.Reason
		return out, nil
	}

	// The SINGLE allowed retry: feed the failing replay log to the optimizer step and
	// re-gate the revised candidate.
	revised, err := optimize(ctx, log)
	if err != nil {
		return out, fmt.Errorf("gate optimizer retry: %w", err)
	}
	verdict2, log2, err := runGateAttempt(ctx, championScores, revised, replay)
	if err != nil {
		return out, fmt.Errorf("gate attempt 2 replay: %w", err)
	}
	out.Attempts = append(out.Attempts, GateAttempt{Attempt: 2, CandidateContent: revised, Verdict: verdict2, Log: log2})
	if verdict2.Pass {
		out.Accepted = true
		out.FinalContent = revised
		out.Reason = "gate passed on the single retry (attempt 2) after feeding the failing replay log to the optimizer: " + verdict2.Reason
		return out, nil
	}

	out.Reason = "gate rejected: candidate failed the fixed corpus twice (a second failure ends the protocol; a clean restart is the caller's choice): " + verdict2.Reason
	return out, nil
}

// runGateAttempt replays one candidate content against the corpus and evaluates the
// verdict, stitching the verdict + content into the returned log for audit.
func runGateAttempt(ctx context.Context, championScores []GateItemScore, candidateContent string, replay GateReplayFunc) (GateVerdict, GateReplayLog, error) {
	scores, log, err := replay(ctx, candidateContent)
	if err != nil {
		return GateVerdict{}, GateReplayLog{}, err
	}
	verdict := EvaluateGate(championScores, scores)
	log.TemplateContent = candidateContent
	log.Verdict = verdict
	return verdict, log, nil
}

// GatePromotionGuard is the pure guard that blocks candidate->canary promotion when
// the pre-canary replay gate is enabled but the candidate carries no ACCEPTED gate
// run (#627). It is additive and off by default: when gateEnabled is false it never
// blocks (byte-identical to the pre-#627 promote path). When the gate IS enabled, a
// candidate must have a passing gate run on record before it may be promoted to
// canary; otherwise promotion is blocked with an actionable reason. It performs no
// I/O — the caller resolves whether an accepted gate run exists and passes the
// boolean.
func GatePromotionGuard(gateEnabled bool, hasAcceptedGateRun bool) (blocked bool, reason string) {
	if !gateEnabled {
		return false, ""
	}
	if hasAcceptedGateRun {
		return false, ""
	}
	return true, "replay gate is enabled ([skillopt].gate_enabled) but this candidate has no passing gate run; run `gitmoot skillopt gate run --candidate <id>` before promotion"
}
