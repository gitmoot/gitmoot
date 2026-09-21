package workflow

import (
	"encoding/json"
	"strings"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

// Fold reasons returned by reviewFoldReason. They are recorded in the
// review_approved_with_notes event so an operator can tell WHY a blocking
// verdict stopped blocking without re-deriving it from the result.
const (
	reviewFoldBelowThreshold = "below_threshold"
	reviewFoldUnlocated      = "unlocated_blocking_findings"
)

// locatorFinding is the minimum shape the locator gate needs. AgentResult keeps
// findings as raw JSON on purpose (reviewers add fields), so this decodes the
// keys the gate reasons about and ignores everything else.
//
// There is no single locator convention in this repo, and assuming one is how
// the first cut of this gate got it wrong: measured over every review finding
// since 2026-08-01, `file` appears 7,989 times and `line` 6,334, against 806 for
// `location`. A gate that only understood `locator` would have read almost every
// located finding as unlocated.
type locatorFinding struct {
	Severity string `json:"severity"`
	Locator  string `json:"locator"`
	File     string `json:"file"`
	Location string `json:"location"`
	Evidence string `json:"evidence"`
}

// saysWhere reports whether the finding points at anything a reader could open.
func (f locatorFinding) saysWhere() bool {
	for _, field := range []string{f.Locator, f.File, f.Location, f.Evidence} {
		if strings.TrimSpace(field) != "" {
			return true
		}
	}
	return false
}

// blockingFindingsAreUnlocated reports whether EVERY finding that blocks at the
// repository bar fails to say where the defect is.
//
// A blocking severity is a claim that a merge must stop. A finding that names no
// file, symbol or reproducible behaviour cannot be acted on and cannot be
// checked, so it is a claim the gate has no way to establish.
//
// This fires rarely BY DESIGN. Measured over September 2026 review findings,
// 88-97% already point somewhere depending on the model, so the gate's job is to
// catch the residue, not to re-litigate ordinary reviews.
//
// It is deliberately conservative, and returns false — keep blocking — whenever
// the gate cannot see the whole picture:
//
//   - no findings at all: nothing to inspect, so the severity stands. A reviewer
//     that reports a blocking verdict with no structured findings is a separate
//     defect and must not be quietly downgraded by this one.
//   - a finding that does not decode: an unreadable finding is not evidence of
//     absence.
//   - any blocking finding carrying a locator: one located finding is enough to
//     justify the block, however many unlocated ones sit beside it.
//
// Evidence counts as a locator when the locator field is empty: a reviewer that
// quotes the command output proving the defect has said where to look, even
// without a file:line.
func blockingFindingsAreUnlocated(result *AgentResult, blockingSeverity string) bool {
	if result == nil || len(result.Findings) == 0 {
		return false
	}
	bar := normalizedReviewBlockingSeverity(blockingSeverity)
	sawBlocking := false
	for _, raw := range result.Findings {
		var finding locatorFinding
		if err := json.Unmarshal(raw, &finding); err != nil {
			return false
		}
		severity := strings.TrimSpace(finding.Severity)
		if severity == "" {
			// A finding that declines to rate itself inherits the verdict's
			// severity: that is the severity the gate would act on.
			severity = result.Severity
		}
		if !reviewseverity.Blocks(severity, bar) {
			continue
		}
		sawBlocking = true
		if finding.saysWhere() {
			return false
		}
	}
	return sawBlocking
}

// reviewFoldReason names why a raw changes_requested verdict does not block, or
// returns "" when it does block. It is the single place that answers the
// question, so the two sites that record the fold event cannot drift from the
// decision itself.
func reviewFoldReason(result *AgentResult, blockingSeverity string) string {
	if result == nil || strings.TrimSpace(result.Decision) != "changes_requested" {
		return ""
	}
	if !reviewseverity.Blocks(result.Severity, normalizedReviewBlockingSeverity(blockingSeverity)) {
		return reviewFoldBelowThreshold
	}
	if blockingFindingsAreUnlocated(result, blockingSeverity) {
		return reviewFoldUnlocated
	}
	return ""
}

// reviewFoldMessage renders the durable explanation for the fold event.
func reviewFoldMessage(reason string, severity string, blockingSeverity string) string {
	switch reason {
	case reviewFoldUnlocated:
		return "review severity " + strings.TrimSpace(severity) +
			" blocks at repository blocking severity " + normalizedReviewBlockingSeverity(blockingSeverity) +
			", but no finding at that severity names a locator or cites evidence; " +
			"findings remain recorded, the verdict is advisory and no fix is dispatched"
	default:
		return "review severity " + strings.TrimSpace(severity) +
			" is below repository blocking severity " + normalizedReviewBlockingSeverity(blockingSeverity) +
			"; findings remain recorded and no fix is dispatched"
	}
}
