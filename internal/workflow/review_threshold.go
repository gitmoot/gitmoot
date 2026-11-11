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
// findingsAdvisory resolves the #1969 declaration, defaulting to FALSE so a
// missing resolver can never relax the obligation gate.
func (e Engine) findingsAdvisory(repo string) bool {
	if e.FindingsAdvisory == nil {
		return false
	}
	return e.FindingsAdvisory(repo)
}

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
//
// INVARIANT: this takes a bare *AgentResult and therefore CANNOT see whether a
// stored review is a fan-out, so it must never be called directly on a stored
// review payload — it would fold a coordinator's dispatch record into an
// approval. Its only two callers are effectiveReviewDecisionForPayload (stored
// review payloads) and effectiveDelegationDecision (delegation children).
// Adding a third caller reintroduces that bug; route it through the payload
// form instead.
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

// effectiveReviewDecisionForPayload is the single authority for turning a stored
// review payload into the decision gitmoot acts on.
func effectiveReviewDecisionForPayload(payload JobPayload, blockingSeverity string) string {
	if payload.Result == nil {
		return ""
	}
	// Phantom-verdict class #1351/#1417/#1557. A FAN-OUT is a coordinator's
	// dispatch record, not an answer about the code, so there is nothing to
	// discount: folding its below-bar severity to "approved" grants a panel the
	// credit for an inspection none of its lenses has reported. Measured on
	// #1910, where a parent announcing three lenses - two of which never ran -
	// was rendered `approved-with-notes` on the PR.
	//
	// The raw decision is returned rather than a refusal so the row keeps
	// describing itself; every consumer that already classifies fan-out (the
	// merge gate, the wake path, required-reviewer credit) is unaffected, and
	// the three that did not now inherit the exclusion instead of re-deriving
	// it. The severity BAR is untouched: an ordinary below-bar review still
	// folds, which TestDelegatedReviewEvidenceUsesBlockingSeverity pins.
	if ResultIsFanOut(payload.Result) {
		return strings.TrimSpace(payload.Result.Decision)
	}
	return effectiveReviewDecision(payload.Result, blockingSeverity)
}

// recordsApprovedWithNotes reports whether a stored review earns the durable
// ReviewApprovedWithNotesEventKind outcome: a raw changes_requested verdict
// folded to approved by repository severity policy, for the WHOLE head. A part
// of a split review (#2372) never approves the head, and proof/project.go turns
// this event into a review.approved claim, so a folded part verdict must not
// carry it (#2373).
func recordsApprovedWithNotes(payload JobPayload, blockingSeverity string) bool {
	return payload.Result != nil && payload.Result.Decision == "changes_requested" &&
		reviewPartOf(payload) == "" &&
		effectiveReviewDecisionForPayload(payload, blockingSeverity) == "approved"
}

// effectiveDelegationDecision applies repository review policy only to
// delegations that are reviews. Ask and implement legs retain their raw
// decisions even when they happen to report a review-shaped severity.
func effectiveDelegationDecision(result *AgentResult, childType string, action string, blockingSeverity string) string {
	if result == nil {
		return ""
	}
	isReview := strings.EqualFold(strings.TrimSpace(action), "review") ||
		strings.EqualFold(strings.TrimSpace(childType), "review")
	if !isReview {
		return strings.TrimSpace(result.Decision)
	}
	// A review child that delegates is still a coordinator announcement. Callers
	// capable of traversing the tree inspect its descendants; flat consumers must
	// not promote the announcement to an approval of its parent's work.
	if ResultIsFanOut(result) {
		return ""
	}
	return effectiveReviewDecision(result, blockingSeverity)
}

func normalizedReviewBlockingSeverity(value string) string {
	severity := strings.ToUpper(strings.TrimSpace(value))
	if !reviewseverity.Valid(severity) {
		return reviewseverity.DefaultBlocking
	}
	return severity
}
