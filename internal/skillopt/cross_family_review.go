package skillopt

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// reviewReviewerPrefix / reviewSelfReviewerPrefix are the distinct, judge-derived
// reviewer ids the cross-family review-agent soft signal carries (#469 +
// REFINEMENT #1). They are non-human sentinels (like autoTraceReviewer) so the
// soft review row is never mistaken for a human ranking, and they are DISTINCT
// from autoTraceReviewer so the review row coexists with the verifiable-floor row
// under the run's UNIQUE (run_id,item_id,reviewer,source,source_url) key instead
// of overwriting it.
//
// Weight tiers the export/optimizer can recognize from the reviewer id + the
// item's judge tags: human gold > verifiable floor (gitmoot-auto) > cross-family
// judge (gitmoot-review:<rt>) > same-family judge (gitmoot-review-self:<rt>).
//
// MEASURE-THE-JUDGE (#344/#345): the judge is judge-derived + weighted low here
// and is NOT calibrated against human gold in this slice — TODO wire a judge↔human
// agreement capture per task-kind (#344/#345) so the optimizer can trust it more.
// It is weighted-low + judge-tagged; subject to the configurable auto_promote
// policy (#463) when that lands — NOT barred from promotion.
const (
	reviewReviewerPrefix     = "gitmoot-review:"
	reviewSelfReviewerPrefix = "gitmoot-review-self:"
)

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
)

// reviewReviewer builds the judge-derived reviewer id for a review outcome: a
// cross-family review is gitmoot-review:<rt>, a same-family fallback (REFINEMENT
// #1) is gitmoot-review-self:<rt> so it weights below a cross-family review and
// never collides with it on the UNIQUE key.
func reviewReviewer(outcome workflow.Outcome) string {
	rt := strings.TrimSpace(outcome.Reviewer)
	if rt == "" {
		rt = "unknown"
	}
	if outcome.SelfFamily {
		return reviewSelfReviewerPrefix + rt
	}
	return reviewReviewerPrefix + rt
}

// crossFamilyReviewItemID is the DISTINCT eval item id for the soft review row (review#repo#pr)
// so it never overwrites the verifiable-floor row (repo#pr) in the same run, while
// a re-review of the SAME PR re-upserts the SAME review row in place (stable row
// count, corrective overwrite).
func crossFamilyReviewItemID(outcome workflow.Outcome) string {
	return reviewItemIDPrefix + autoTraceItemID(outcome)
}

// projectReview maps a cross-family review rubric to a NormalizedSignal via
// ProjectSignal over a synthetic EvaluatorScore whose DimensionScores ARE the
// rubric (#469). ProjectSignal takes the arithmetic MEAN of DimensionScores when
// Soft is absent (the #462 rubric-as-score path), so no new aggregation is
// invented. An EMPTY rubric yields HasScore=false (no fabricated neutral 0.5).
//
// The review is always recorded as choice "a" (a soft, advisory positive-leaning
// signal attached to the current promoted template); it is weighted-low and
// judge-tagged via the reviewer id + item metadata, NOT via the choice, so it can
// never outrank the verifiable floor.
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
// optimizer sees both), under a DISTINCT item id (review#repo#pr) and a
// judge-derived reviewer (gitmoot-review[-self]:<rt>), so it coexists with the
// floor row on the UNIQUE (run_id,item_id,reviewer,source,source_url) key instead
// of overwriting it. The run keeps feedback_source=automatic_trace (no contract
// bump, ContractVersion=1 unchanged); the review item carries judge_derived (and
// self_family on the fallback) so the export/optimizer down-weights it.
//
// It writes ONLY eval_runs/eval_review_items/feedback_events — no candidate, no
// promotion path — so promotion stays manual (subject to the configurable
// auto_promote policy, #463, when that lands).
func (h *OutcomeHarvester) writeReviewFeedback(ctx context.Context, version db.AgentTemplateVersion, outcome workflow.Outcome, signal NormalizedSignal) error {
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
		MetadataJSON: reviewItemMetadata(outcome),
	}); err != nil {
		return fmt.Errorf("upsert auto-trace eval_review_item (review): %w", err)
	}

	if err := h.Store.UpsertFeedbackEvent(ctx, db.FeedbackEvent{
		RunID:     runID,
		ItemID:    itemID,
		Choice:    "a",
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
// a cross-family judge.
func reviewItemMetadata(outcome workflow.Outcome) string {
	meta := map[string]any{
		"repo":                        strings.TrimSpace(outcome.Repo),
		"pull_request":                outcome.PullRequest,
		reviewItemMetadataJudgeKey:    true,
		reviewItemMetadataReviewerKey: strings.TrimSpace(outcome.Reviewer),
	}
	if outcome.SelfFamily {
		meta[reviewItemMetadataSelfFamilyKey] = true
	}
	raw, _ := json.Marshal(meta)
	return string(raw)
}
