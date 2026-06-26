package skillopt

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// reviewReviewerID is the FIXED, judge-derived reviewer id every cross-family
// review row carries (#469 + REFINEMENT #1). It is a single non-human sentinel
// (like autoTraceReviewer) so the soft review row is never mistaken for a human
// ranking, and it is DISTINCT from autoTraceReviewer so the review row coexists
// with the verifiable-floor row under the run's UNIQUE
// (run_id,item_id,reviewer,source,source_url) key instead of overwriting it.
//
// CRITICAL: the reviewer id is INTENTIONALLY independent of the selected reviewer
// family/runtime so a re-review of the SAME PR by a DIFFERENT family (e.g. a first
// cross-family claude review, a later same-family codex fallback after claude is
// deauthed) OVERWRITES the prior review row in place instead of accumulating a
// second, stale, contradictory row (row count stays stable, corrective overwrite).
// The actual reviewer family + the same-family flag are carried in the item
// METADATA (reviewer_runtime / self_family), which is where the export/optimizer
// reads them — so the weight tiers below survive without varying the conflict key.
//
// Weight tiers the export/optimizer recognizes from the reviewer sentinel + the
// item's judge/self-family tags: human gold > verifiable floor (gitmoot-auto) >
// cross-family judge (self_family absent) > same-family judge (self_family=true).
//
// MEASURE-THE-JUDGE (#344/#345): the judge is judge-derived + weighted low here
// and is NOT calibrated against human gold in this slice — TODO wire a judge↔human
// agreement capture per task-kind (#344/#345) so the optimizer can trust it more.
// It is weighted-low + judge-tagged; subject to the configurable auto_promote
// policy (#463) when that lands — NOT barred from promotion.
const reviewReviewerID = "gitmoot-review"

// reviewItemIDPrefix namespaces the cross-family review eval item so it is a
// DISTINCT item id from the verifiable-floor item (which is repo#pr): the review
// item is review#<repo>#<pr>. A distinct item id means the soft review row never
// overwrites the verifiable-floor row even though both live in the same
// auto-trace:<versionID> run.
const reviewItemIDPrefix = "review#"

// reviewItemMetadataJudgeKey / reviewItemMetadataSelfFamilyKey are the per-item
// metadata flags that mark a review row judge-derived (and same-family when the
// fallback fired). The run-level feedback_source stays automatic_trace (no
// contract bump); these item tags are the seam the export/optimizer reads to
// down-weight judge rows below the verifiable floor and to weight same-family
// judge rows below cross-family ones, and the seam #344/#345 calibration reads.
const (
	reviewItemMetadataJudgeKey      = "judge_derived"
	reviewItemMetadataSelfFamilyKey = "self_family"
	reviewItemMetadataReviewerKey   = "reviewer_runtime"
	// reviewItemMetadataScoreKey carries the projected rubric MEAN ([0,1]) on the
	// review item so the magnitude the judge measured is recoverable by the
	// export/optimizer (the score also drives the a/b choice). It is item metadata
	// (no contract bump), the same no-new-field down-weight lever the run uses.
	reviewItemMetadataScoreKey = "rubric_score"
)

// reviewReviewer returns the FIXED judge-derived reviewer id for the review row's
// UNIQUE key. It is deliberately constant across reviewer families so a re-review
// of the same PR by a DIFFERENT family overwrites the prior row rather than
// accumulating a stale duplicate; the actual family and the same-family flag are
// carried in the item metadata (reviewer_runtime / self_family) instead.
func reviewReviewer(workflow.Outcome) string {
	return reviewReviewerID
}

// crossFamilyReviewItemID is the DISTINCT eval item id for the soft review row (review#repo#pr)
// so it never overwrites the verifiable-floor row (repo#pr) in the same run, while
// a re-review of the SAME PR re-upserts the SAME review row in place (stable row
// count, corrective overwrite).
func crossFamilyReviewItemID(outcome workflow.Outcome) string {
	return reviewItemIDPrefix + autoTraceItemID(outcome)
}

// reviewChoiceThreshold is the projected-mean cutoff that maps a review's quality
// LEVEL onto the a/b choice the auto-trace pipeline actually reads
// (recommendValidate counts "a" => baselineWins, "b" => candidateWins). It mirrors
// the verifiable floor's positive=>"a" / negative=>"b" convention: a review whose
// mean rubric score is below this cutoff is a NEGATIVE quality signal and must
// register as "b" (a non-baseline vote), not as a baseline win. A review at or
// above it is positive-leaning and registers as "a".
const reviewChoiceThreshold = 0.5

// reviewChoice derives the a/b choice from the projected quality so a poor review
// actually registers as a non-baseline vote instead of silently inverting into a
// baseline win (#469 review-fix). When the rubric was empty (HasScore=false) the
// signal carries no quality level; the caller skips the write entirely rather than
// fabricate a vote, so reviewChoice is only consulted for a scored signal.
func reviewChoice(signal NormalizedSignal) string {
	if signal.HasScore && signal.Score < reviewChoiceThreshold {
		return "b"
	}
	return "a"
}

// projectReview maps a cross-family review rubric to a NormalizedSignal via
// ProjectSignal over a synthetic EvaluatorScore whose DimensionScores ARE the
// rubric (#469). ProjectSignal takes the arithmetic MEAN of DimensionScores when
// Soft is absent (the #462 rubric-as-score path), so no new aggregation is
// invented. An EMPTY rubric yields HasScore=false (no fabricated neutral 0.5).
//
// The projected mean is load-bearing: writeReviewFeedback skips the write when the
// rubric is empty (HasScore=false, non-informative) and maps the score LEVEL onto
// the a/b choice via reviewChoice, so a low-quality review registers as a "b"
// (non-baseline) vote rather than a fabricated baseline win. The row stays
// weighted-low + judge-tagged via the reviewer id + item metadata so it can never
// outrank the verifiable floor regardless of the choice.
func projectReview(outcome workflow.Outcome) NormalizedSignal {
	dims := map[string]float64{}
	for k, v := range outcome.Rubric {
		dims[k] = v
	}
	findings := strings.TrimSpace(outcome.Findings)
	if findings == "" {
		findings = fmt.Sprintf("Cross-family review of PR #%d.", outcome.PullRequest)
	}
	return ProjectSignal(&EvaluatorScore{DimensionScores: dims}, &RankedFeedbackEvent{Reasoning: findings}, nil)
}

// writeReviewFeedback upserts the SOFT cross-family review row into the EXISTING
// auto-trace:<versionID> run (#469): the same run as the verifiable floor (so the
// optimizer sees both), under a DISTINCT item id (review#repo#pr) and the FIXED
// judge-derived reviewer sentinel (gitmoot-review), so it coexists with the floor
// row on the UNIQUE (run_id,item_id,reviewer,source,source_url) key instead of
// overwriting it — and so a re-review by a different family overwrites the review
// row in place rather than accumulating a stale duplicate. The run keeps
// feedback_source=automatic_trace (no contract bump, ContractVersion=1 unchanged);
// the review item carries judge_derived, the reviewer_runtime family, the projected
// rubric_score, and self_family (on the fallback) so the export/optimizer
// down-weights it. The a/b choice tracks the projected mean (poor review => "b").
//
// It writes ONLY eval_runs/eval_review_items/feedback_events — no candidate, no
// promotion path — so promotion stays manual (subject to the configurable
// auto_promote policy, #463, when that lands).
func (h *OutcomeHarvester) writeReviewFeedback(ctx context.Context, version db.AgentTemplateVersion, outcome workflow.Outcome, signal NormalizedSignal) error {
	// An empty rubric (HasScore=false) carries NO quality level: it is
	// non-informative, so writing a row would either fabricate a neutral vote or
	// (worse) default to a baseline win. Skip it entirely — matching the
	// HasScore=false intent — so only a scored review ever lands a feedback row.
	if !signal.HasScore {
		return nil
	}
	runID := autoTraceRunIDPrefix + strings.TrimSpace(version.ID)
	itemID := crossFamilyReviewItemID(outcome)
	reviewer := reviewReviewer(outcome)
	sourceURL := pullRequestURL(outcome.Repo, outcome.PullRequest)

	// Reuse the EXISTING auto-trace run (same metadata: feedback_source=automatic_trace,
	// validate mode); an upsert keyed by the run id is idempotent so the verifiable
	// floor's run metadata is preserved verbatim.
	if err := h.Store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                runID,
		TemplateID:        strings.TrimSpace(version.TemplateID),
		TemplateVersionID: strings.TrimSpace(version.ID),
		TargetRepo:        strings.TrimSpace(outcome.Repo),
		State:             "ready",
		Mode:              db.EvalRunModeValidate,
		MetadataJSON:      autoTraceRunMetadata(),
	}); err != nil {
		return fmt.Errorf("upsert auto-trace eval_run (review): %w", err)
	}

	if err := h.Store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:        runID,
		ItemID:       itemID,
		Title:        reviewItemTitle(outcome),
		MetadataJSON: reviewItemMetadata(outcome, signal),
	}); err != nil {
		return fmt.Errorf("upsert auto-trace eval_review_item (review): %w", err)
	}

	if err := h.Store.UpsertFeedbackEvent(ctx, db.FeedbackEvent{
		RunID:     runID,
		ItemID:    itemID,
		Choice:    reviewChoice(signal),
		Reasoning: signal.Feedback,
		Reviewer:  reviewer,
		Source:    autoTraceSource,
		SourceURL: sourceURL,
	}); err != nil {
		return fmt.Errorf("upsert auto-trace feedback_event (review): %w", err)
	}
	return nil
}

// reviewItemTitle is the human-readable title of the soft review item.
func reviewItemTitle(outcome workflow.Outcome) string {
	repo := strings.TrimSpace(outcome.Repo)
	if outcome.PullRequest > 0 {
		return fmt.Sprintf("Cross-family review: %s PR #%d", repo, outcome.PullRequest)
	}
	return "Cross-family review: " + repo
}

// reviewItemMetadata carries the judge-derived (+ self-family) tags on the review
// eval item so the export/optimizer can down-weight it relative to the verifiable
// floor and to human gold, WITHOUT a new contract field. self_family is present
// (true) ONLY on the same-family fallback row so a same-family judge weights below
// a cross-family judge. It also persists the projected rubric MEAN (rubric_score)
// so the quality MAGNITUDE the judge measured reaches the row — not just the a/b
// choice derived from it — keeping projectReview's score path load-bearing.
func reviewItemMetadata(outcome workflow.Outcome, signal NormalizedSignal) string {
	meta := map[string]any{
		"repo":                        strings.TrimSpace(outcome.Repo),
		"pull_request":                outcome.PullRequest,
		reviewItemMetadataJudgeKey:    true,
		reviewItemMetadataReviewerKey: strings.TrimSpace(outcome.Reviewer),
	}
	if signal.HasScore {
		meta[reviewItemMetadataScoreKey] = signal.Score
	}
	if outcome.SelfFamily {
		meta[reviewItemMetadataSelfFamilyKey] = true
	}
	raw, _ := json.Marshal(meta)
	return string(raw)
}
