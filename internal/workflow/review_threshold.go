package workflow

import (
	"strings"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

// ReviewApprovedWithNotesEventKind records the durable effective outcome for a
// raw changes-requested review whose severity is below repository policy.
const ReviewApprovedWithNotesEventKind = "review_approved_with_notes"

// reviewBlockingSeverity resolves the repository policy while preserving the
// historical fail-closed behavior for engines that are not wired from config.
func (e Engine) reviewBlockingSeverity(repo string) string {
	if e.ReviewBlockingSeverity == nil {
		return reviewseverity.DefaultBlocking
	}
	severity := strings.ToUpper(strings.TrimSpace(e.ReviewBlockingSeverity(repo)))
	if !reviewseverity.Valid(severity) {
		return reviewseverity.DefaultBlocking
	}
	return severity
}

// effectiveReviewDecision converts only a sub-threshold changes-requested
// result into an engine-level approval. The stored AgentResult remains unchanged
// so its summary and findings remain available to comment/rendering surfaces.
func effectiveReviewDecision(result *AgentResult, blockingSeverity string) string {
	if result == nil {
		return ""
	}
	decision := strings.TrimSpace(result.Decision)
	if decision == "changes_requested" && !reviewseverity.Blocks(result.Severity, normalizedReviewBlockingSeverity(blockingSeverity)) {
		return "approved"
	}
	return decision
}

// IsPipelineReviewPayload reports whether a review job was dispatched by the
// pipeline advancer rather than the native review lifecycle.
func IsPipelineReviewPayload(payload JobPayload) bool {
	return strings.EqualFold(strings.TrimSpace(payload.Sender), PipelineJobSender)
}

// effectiveReviewDecisionForPayload is the single authority for turning a stored
// review payload into the decision gitmoot acts on. Repository severity policy
// applies to NATIVE reviews only: a pipeline-sender review is report-only, the
// pipeline advancer owns folding its verdict, and no gitmoot surface — the merge
// gate included — may re-interpret it into an approval the pipeline never gave.
func effectiveReviewDecisionForPayload(payload JobPayload, blockingSeverity string) string {
	if payload.Result == nil {
		return ""
	}
	if IsPipelineReviewPayload(payload) {
		return strings.TrimSpace(payload.Result.Decision)
	}
	return effectiveReviewDecision(payload.Result, blockingSeverity)
}

// effectiveDelegationDecision applies repository review policy only to
// delegations that are reviews. Ask and implement legs retain their raw
// decisions even when they happen to report a review-shaped severity.
func effectiveDelegationDecision(result *AgentResult, childType string, action string, blockingSeverity string) string {
	if result == nil {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(action), "review") ||
		strings.EqualFold(strings.TrimSpace(childType), "review") {
		return effectiveReviewDecision(result, blockingSeverity)
	}
	return strings.TrimSpace(result.Decision)
}

func normalizedReviewBlockingSeverity(value string) string {
	severity := strings.ToUpper(strings.TrimSpace(value))
	if !reviewseverity.Valid(severity) {
		return reviewseverity.DefaultBlocking
	}
	return severity
}
