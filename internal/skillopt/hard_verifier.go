package skillopt

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// hardVerifierReviewerID is the FIXED, tool-derived reviewer id every HARD-verifier
// row carries (#474). It is a single non-human sentinel (like autoTraceReviewer,
// reviewReviewerID, and checkerReviewerID) so the hard row is never mistaken for a
// human ranking, and it is DISTINCT from ALL THREE existing sentinels — the
// verifiable floor (gitmoot-auto), the subjective LLM rubric (gitmoot-review), and
// the objective soft checkers (gitmoot-checker) — so the hard row coexists with all
// of them under the run's UNIQUE (run_id,item_id,reviewer,source,source_url) key
// instead of overwriting any. A re-verification of the SAME PR re-upserts the SAME
// hard row in place (the reviewer id is constant), so the row count stays stable.
//
// The hard row is the AUTHORITATIVE HARD tier: it carries EvaluatorScore.Hard from a
// plain exit code in an isolated checkout, so it is un-gameable by the LLM judge's
// prose. The export/optimizer weights it distinctly via this reviewer sentinel + the
// hard_verifier item tag.
const hardVerifierReviewerID = "gitmoot-verifier"

// hardVerifierItemIDPrefix namespaces the hard-verifier eval item so it is a DISTINCT
// item id from the verifiable-floor item (repo#pr), the subjective review item
// (review#repo#pr), AND the objective checker item (checker#repo#pr): the hard item
// is hard#<repo>#<pr>. A distinct item id means the hard row never overwrites any
// sibling even though all four live in the same auto-trace:<versionID> run.
const hardVerifierItemIDPrefix = "hard#"

const (
	// hardVerifierItemMetadataKey marks a hard-verifier row on its eval item so the
	// export/optimizer can recognize it as the un-gameable HARD tier, WITHOUT a new
	// contract field. The run-level feedback_source stays automatic_trace (no contract
	// bump, ContractVersion=1 unchanged).
	hardVerifierItemMetadataKey = "hard_verifier"
	// hardVerifierItemMetadataPassedKey carries the binary pass/fail verdict on the
	// item so the outcome is recoverable by the export/optimizer.
	hardVerifierItemMetadataPassedKey = "hard_passed"
	// hardVerifierItemMetadataScoreKey carries the projected Hard score (1.0 pass /
	// 0.0 fail) so the magnitude reaches the row (the same no-new-field lever the
	// review/checker rows use).
	hardVerifierItemMetadataScoreKey = "hard_score"
	// hardVerifierItemMetadataCommandsKey persists the per-command pass/fail map (each
	// command → 1.0 pass / 0.0 fail) so which specific verifier failed is auditable,
	// not just the aggregate verdict.
	hardVerifierItemMetadataCommandsKey = "command_results"
)

// projectHardVerifier maps the deterministic hard-verifier verdict to a
// NormalizedSignal via ProjectSignal over a synthetic EvaluatorScore whose Hard
// field IS the verdict (#474): HardPassed → Hard=1.0 (a strong, evidence-backed
// positive), else Hard=0.0 (an authoritative gate-fail 0). ProjectSignal reads
// Hard==0 as an informative 0 (HasScore=true) and Hard>0 as the quality component,
// so a pass projects to Score=1.0 and a fail to Score=0.0 — BOTH HasScore=true (a
// hard verdict is never "absent"). A FAIL additionally rides an
// EvaluatorFailurePacket so the feedback explains it (like negativeSignal).
func projectHardVerifier(outcome workflow.Outcome) NormalizedSignal {
	findings := strings.TrimSpace(outcome.Findings)
	if findings == "" {
		findings = fmt.Sprintf("Hard verifiers on PR #%d.", outcome.PullRequest)
	}
	if outcome.HardPassed {
		h := 1.0
		return ProjectSignal(&EvaluatorScore{Hard: &h}, &RankedFeedbackEvent{Reasoning: findings}, nil)
	}
	h := 0.0
	return ProjectSignal(&EvaluatorScore{Hard: &h}, nil, &EvaluatorFailurePacket{OptimizerHint: findings})
}

// hardVerifierChoice derives the a/b choice from the hard verdict so a FAIL registers
// as a non-baseline "b" vote instead of a baseline win, mirroring reviewChoice/
// checkerChoice. A hard verdict is always scored (HasScore=true), so an unscored
// signal cannot occur here; the HasScore guard keeps it total.
func hardVerifierChoice(signal NormalizedSignal) string {
	if signal.HasScore && signal.Score < hardVerifierChoiceThreshold {
		return "b"
	}
	return "a"
}

// hardVerifierChoiceThreshold maps the Hard score onto a/b: a FAIL (Hard=0) is below
// it (=> "b", a non-baseline vote), a PASS (Hard=1.0) is at/above it (=> "a").
const hardVerifierChoiceThreshold = 0.5

// writeHardVerifierFeedback upserts the AUTHORITATIVE hard-verifier row into the
// EXISTING auto-trace:<versionID> run (#474): the same run as the verifiable floor,
// the subjective review, and the objective checker (so the optimizer sees all four),
// under a DISTINCT item id (hard#repo#pr) and the FIXED tool-derived reviewer
// sentinel (gitmoot-verifier), so it coexists with all three siblings on the UNIQUE
// (run_id,item_id,reviewer,source,source_url) key instead of overwriting any — and so
// a re-verification of the same PR overwrites the hard row in place rather than
// accumulating a stale duplicate. The run keeps feedback_source=automatic_trace (no
// contract bump); the hard item carries the hard_verifier tag, the pass/fail verdict,
// the projected Hard score, and the per-command results so the export/optimizer can
// weight it as the un-gameable HARD tier. The a/b choice tracks the verdict (fail =>
// "b").
//
// It writes ONLY eval_runs/eval_review_items/feedback_events — no candidate, no
// promotion path — so promotion stays manual.
func (h *OutcomeHarvester) writeHardVerifierFeedback(ctx context.Context, version db.AgentTemplateVersion, outcome workflow.Outcome, signal NormalizedSignal) error {
	// A hard verdict is ALWAYS scored (pass=1.0 / fail=0.0), so HasScore is true here;
	// the guard is defensive so a malformed projection never lands an unscored row.
	if !signal.HasScore {
		return nil
	}
	runID := AutoTraceRunID(version.ID)
	itemID := hardVerifierItemID(outcome)
	sourceURL := pullRequestURL(outcome.Repo, outcome.PullRequest)

	if err := h.Store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                runID,
		TemplateID:        strings.TrimSpace(version.TemplateID),
		TemplateVersionID: strings.TrimSpace(version.ID),
		TargetRepo:        strings.TrimSpace(outcome.Repo),
		State:             "ready",
		Mode:              db.EvalRunModeValidate,
		MetadataJSON:      autoTraceRunMetadata(),
	}); err != nil {
		return fmt.Errorf("upsert auto-trace eval_run (hard verifier): %w", err)
	}

	if err := h.Store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:        runID,
		ItemID:       itemID,
		Title:        hardVerifierItemTitle(outcome),
		MetadataJSON: hardVerifierItemMetadata(outcome, signal),
	}); err != nil {
		return fmt.Errorf("upsert auto-trace eval_review_item (hard verifier): %w", err)
	}

	if err := h.Store.UpsertFeedbackEvent(ctx, db.FeedbackEvent{
		RunID:     runID,
		ItemID:    itemID,
		Choice:    hardVerifierChoice(signal),
		Reasoning: signal.Feedback,
		Reviewer:  hardVerifierReviewerID,
		Source:    autoTraceSource,
		SourceURL: sourceURL,
	}); err != nil {
		return fmt.Errorf("upsert auto-trace feedback_event (hard verifier): %w", err)
	}
	return nil
}

// hardVerifierItemID is the DISTINCT eval item id for the hard-verifier row
// (hard#repo#pr) so it never overwrites the verifiable-floor row (repo#pr), the
// review row (review#repo#pr), or the checker row (checker#repo#pr) in the same run,
// while a re-verification of the SAME PR re-upserts the SAME hard row in place.
func hardVerifierItemID(outcome workflow.Outcome) string {
	return hardVerifierItemIDPrefix + autoTraceItemID(outcome)
}

// hardVerifierItemTitle is the human-readable title of the hard-verifier item.
func hardVerifierItemTitle(outcome workflow.Outcome) string {
	repo := strings.TrimSpace(outcome.Repo)
	verdict := "fail"
	if outcome.HardPassed {
		verdict = "pass"
	}
	if outcome.PullRequest > 0 {
		return fmt.Sprintf("Hard verifiers (%s): %s PR #%d", verdict, repo, outcome.PullRequest)
	}
	return fmt.Sprintf("Hard verifiers (%s): %s", verdict, repo)
}

// hardVerifierItemMetadata carries the hard_verifier tag, the binary verdict, the
// projected Hard score, and the per-command pass/fail map on the hard eval item so
// the export/optimizer can recognize it as the un-gameable HARD tier and recover
// exactly which verifier failed, WITHOUT a new contract field. The per-command map
// is derived from outcome.Rubric (each command → 1.0 pass / 0.0 fail), which the
// dispatcher populates for audit only — it is NOT used as the score (the score is the
// binary Hard verdict, not the mean of the commands).
func hardVerifierItemMetadata(outcome workflow.Outcome, signal NormalizedSignal) string {
	commands := map[string]float64{}
	names := make([]string, 0, len(outcome.Rubric))
	for k, v := range outcome.Rubric {
		commands[k] = v
		names = append(names, k)
	}
	sort.Strings(names)
	meta := map[string]any{
		"repo":                              strings.TrimSpace(outcome.Repo),
		"pull_request":                      outcome.PullRequest,
		hardVerifierItemMetadataKey:         true,
		hardVerifierItemMetadataPassedKey:   outcome.HardPassed,
		hardVerifierItemMetadataCommandsKey: commands,
		"commands":                          names,
	}
	if signal.HasScore {
		meta[hardVerifierItemMetadataScoreKey] = signal.Score
	}
	raw, _ := json.Marshal(meta)
	return string(raw)
}
