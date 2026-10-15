package workflow

import (
	"encoding/json"
	"regexp"
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

// pathShapedInProse matches something a reader could open, mentioned anywhere in
// a prose field: `internal/db/store.go:88`, `store.go:88`, `website/styles.css`,
// `go.mod`, `Makefile`, `.gitignore`.
//
// THE WIDTH OF THIS PATTERN IS THE WHOLE RULE, and two review rounds moved it in
// opposite directions:
//
//   - Round 2: an extension whitelist missed `go.mod`, `Makefile`, `Dockerfile`,
//     `LICENSE`, `.gitignore` and `.css`, folding verdicts that did say where.
//   - Round 3: the replacement was so wide that `e.g.`, `i.e.`, `n/a`, `and/or`,
//     `I/O` and `U.S.` all matched — ordinary English in any finding kept the
//     block, which made the rule inert rather than conservative.
//
// So the slash arm now requires a path segment that looks like a filename
// (a dot-extension or a known extensionless build file), not merely a slash, and
// the dotted arm requires a plausible file extension of 2-9 characters rather
// than any single letter. `n/a` and `e.g.` are excluded by construction; a real
// locator is not.
var pathShapedInProse = regexp.MustCompile(
	`(?:[\w.-]+/)+[\w.-]*[\w-]\.[A-Za-z][A-Za-z0-9]{1,8}(?::\d+)?` + // path/to/file.ext[:12]
		`|(?:^|[\s"'` + "`" + `(\[])[\w-]{2,}\.[A-Za-z][A-Za-z0-9]{1,8}(?::\d+)?\b` + // file.ext[:12]
		`|(?:^|[\s"'` + "`" + `(\[])\.[A-Za-z][\w-]{2,20}\b` + // .gitignore
		`|\b(?:Makefile|Dockerfile|LICENSE|CODEOWNERS|Procfile|Justfile|Rakefile|go\.mod|go\.sum)\b` +
		`|(?:[\w.-]+/)+[\w-]{2,}(?::\d+)\b`) // path/to/thing:88

// locatorFinding is the minimum shape the locator gate needs. AgentResult keeps
// findings as raw JSON on purpose (reviewers add fields), so this decodes the
// keys the gate reasons about and ignores everything else.
//
// THE KEY SET IS MEASURED, NEVER GUESSED, and it is deliberately the same set
// findings_ledger_writer.go reads (reviewFindingWire, :39-132). Two earlier cuts
// of this gate got it wrong in the same way from opposite ends: the first read
// only `locator` and so read almost every located finding as unlocated (`file`
// appears 7,989 times against 806 for `location` since 2026-08-01); the second
// still missed `evidence_locator`, which the ledger declares canonical with
// `locator` as its alternate. A gate whose key set is narrower than the reader
// beside it will disagree with that reader about the same finding.
type locatorFinding struct {
	Severity string `json:"severity"`
	// Location keys, canonical first.
	EvidenceLocator string `json:"evidence_locator"`
	Locator         string `json:"locator"`
	File            string `json:"file"`
	Location        string `json:"location"`
	Evidence        string `json:"evidence"`
	// Prose keys. A finding whose only locator rides inside its prose still told
	// the reader where to look, so these are searched for a path shape rather
	// than accepted whole: prose is present on ~88% of findings, and accepting
	// any prose at all would disable this gate rather than narrow it.
	Detail      string `json:"detail"`
	Details     string `json:"details"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Summary     string `json:"summary"`
	Message     string `json:"message"`
	Finding     string `json:"finding"`
	// Rationale is read by the ledger writer (findings_ledger_writer.go:64) and
	// carries a locator often enough to matter. Title is prose too: a finding
	// titled "nil deref in agent_dispatch.go:1442" has said where.
	Rationale string `json:"rationale"`
	Title     string `json:"title"`
}

// isEmpty reports whether the decoded finding carried no content at all. `[{}]`
// and `[null]` decode cleanly into a zero value, and without this they would
// inherit the verdict severity, count as unlocated, and fold the verdict — which
// would turn the no-findings guard into a way to unblock a head by sending
// nothing. An empty element is treated as the no-findings case: keep blocking.
func (f locatorFinding) isEmpty() bool {
	for _, field := range []string{
		f.Severity, f.EvidenceLocator, f.Locator, f.File, f.Location, f.Evidence,
		f.Detail, f.Details, f.Description, f.Body, f.Summary, f.Message, f.Finding,
		f.Rationale, f.Title,
	} {
		if strings.TrimSpace(field) != "" {
			return false
		}
	}
	return true
}

// saysWhere reports whether the finding points at anything a reader could open.
func (f locatorFinding) saysWhere() bool {
	for _, field := range []string{f.EvidenceLocator, f.Locator, f.File, f.Location, f.Evidence} {
		if strings.TrimSpace(field) != "" {
			return true
		}
	}
	for _, prose := range []string{
		f.Detail, f.Details, f.Description, f.Body, f.Summary, f.Message, f.Finding,
		f.Rationale, f.Title,
	} {
		if pathShapedInProse.MatchString(prose) {
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
//   - no findings at all, or only contentless elements such as `[{}]` / `[null]`:
//     nothing to inspect, so the severity stands. A reviewer that reports a
//     blocking verdict with no readable findings is a separate defect and must
//     not be quietly downgraded by this one — nor may an empty element become a
//     way to unblock a head by sending nothing.
//   - a finding that does not decode: an unreadable finding is not evidence of
//     absence.
//   - a severity this repository cannot rank, on the verdict or on a finding:
//     reviewseverity.Blocks fails closed on unrankable input, and this gate must
//     not undo that by reading "unrankable" as "unlocated".
//   - any blocking finding that says where: one located finding justifies the
//     block, however many vague ones sit beside it.
//
// Evidence counts as saying where: a reviewer that quotes the command output
// proving the defect has told the reader where to look, even without a file:line.
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
		if finding.isEmpty() {
			return false
		}
		severity := strings.TrimSpace(finding.Severity)
		if severity == "" {
			// A finding that declines to rate itself inherits the verdict's
			// severity: that is the severity the gate would act on.
			severity = strings.TrimSpace(result.Severity)
		}
		if !reviewseverity.Valid(severity) {
			// Unrankable severity: Blocks() would fail closed, and so does this.
			return false
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
	if !reviewseverity.Valid(strings.TrimSpace(result.Severity)) {
		// An unrankable verdict severity blocks (reviewseverity.Blocks fails
		// closed) and must stay blocking: the gate cannot reason about a
		// severity it cannot rank.
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
