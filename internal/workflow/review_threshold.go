package workflow

import (
	"strings"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

const reviewApprovedWithNotesEventKind = "review_approved_with_notes"

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

func normalizedReviewBlockingSeverity(value string) string {
	severity := strings.ToUpper(strings.TrimSpace(value))
	if !reviewseverity.Valid(severity) {
		return reviewseverity.DefaultBlocking
	}
	return severity
}
