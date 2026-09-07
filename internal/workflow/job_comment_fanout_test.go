package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

// Phantom-verdict class #1351/#1417/#1557, reproduced on #1910: a coordinator's
// dispatch record is rendered with the same headline scalars as a verdict about
// the code. The parent on #1910 posted "**Decision:** `approved`" while its own
// summary said "Exact-head dispatch only, not the final verdict", and two of its
// three lens legs never ran.
//
// A fan-out result carrying changes_requested is the sharper case: ResultIsFanOut
// covers BOTH terminal verdicts (isTerminalReviewVerdict is approved and
// changes_requested; result_checks.go:632), so a coordinator that announces a
// panel AND reports a below-bar severity currently reaches the severity fold and
// is rendered `approved-with-notes` — an approval-flavoured outcome for a row
// that answered nothing yet.
func TestRenderJobResultCommentNeverFoldsAFanOutDispatchRecord(t *testing.T) {
	comment := JobResultComment{
		AgentName:              "g6-review-sol",
		Runtime:                "codex",
		JobID:                  "review-panel-parent",
		JobType:                "review",
		JobState:               string(JobSucceeded),
		ReviewBlockingSeverity: reviewseverity.P2,
		Result: &AgentResult{
			Decision:    "changes_requested",
			Severity:    reviewseverity.P3,
			Summary:     "Exact-head dispatch only, not the final verdict: convening three lenses.",
			Delegations: []Delegation{{ID: "lens-correctness"}, {ID: "lens-security"}, {ID: "lens-regression"}},
		},
	}
	// Premise stated as an assertion, not a comment: if this stops being a
	// fan-out the case is no longer testing what it claims to.
	if !ResultIsFanOut(comment.Result) {
		t.Fatal("fixture is not a fan-out result, so it cannot exercise the dispatch-record path")
	}

	body := RenderJobResultComment(comment)

	if strings.Contains(body, "approved-with-notes") {
		t.Fatalf("a fan-out dispatch record was rendered as approved-with-notes; it has answered nothing yet:\n%s", body)
	}
}

// TestRenderJobResultCommentHeadlineDecisionIsNotAVerdictForAFanOut is #1963,
// and the fixture is a VERBATIM reproduction of a comment this renderer already
// published: gitmoot/gitmoot#1731 issuecomment-5481374373, from job
// local-review-g6-review-sol-18d0f0b0b9134f90, which reads
//
//	**Decision:** `approved`
//	**Summary:** Convening three report-only reviewers against exact head ...
//
// The summary says it is CONVENING reviewers. The headline said it approved.
//
// The exclusion above only kept a fan-out out of the approved-with-notes FOLD;
// the headline scalar still asserted the verdict, which is the field a human
// reads first and the only one a skimmer reads at all. Measured across the live
// store: 70 PR-attached rows on 27 pull requests, 62 with an empty tests_run,
// and every one of them rendered "approved", because a fan-out carrying a
// terminal verdict has never once carried changes_requested.
func TestRenderJobResultCommentHeadlineDecisionIsNotAVerdictForAFanOut(t *testing.T) {
	comment := JobResultComment{
		AgentName: "g6-review-sol",
		Runtime:   "codex",
		JobID:     "local-review-g6-review-sol-18d0f0b0b9134f90",
		JobType:   "review",
		JobState:  string(JobSucceeded),
		Payload:   JobPayload{TemplateID: "review-panel"},
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "Convening three report-only reviewers against exact head 2322fc865580de5d078676fe861219ded5db9352.",
			Delegations: []Delegation{
				{ID: "lens-correctness"}, {ID: "lens-security"}, {ID: "lens-regression"},
			},
		},
	}
	if !ResultIsFanOut(comment.Result) {
		t.Fatal("fixture is not a fan-out result, so it cannot exercise the announcement path")
	}

	body := RenderJobResultComment(comment)

	if strings.Contains(body, "**Decision:** `approved`") {
		t.Fatalf("the published record still asserts a verdict for a coordinator announcement:\n%s", body)
	}
	if !strings.Contains(body, "**Decision:** `fan-out`") {
		t.Fatalf("the headline scalar does not name the row as a dispatch:\n%s", body)
	}
	// The announced value must remain auditable. Suppressing it would trade one
	// dishonest record for an incomplete one.
	if !strings.Contains(body, "announced `approved`") {
		t.Fatalf("the announced decision was dropped instead of being labelled:\n%s", body)
	}
	if !strings.Contains(body, "3 delegated child job(s)") {
		t.Fatalf("the record does not say where the evidence actually is:\n%s", body)
	}
}

// The negative control, on the exact shape that must NOT change: an ordinary
// review verdict with no delegations keeps its headline decision. Without this,
// relabelling every decision would satisfy the case above.
func TestRenderJobResultCommentKeepsTheHeadlineDecisionForARealVerdict(t *testing.T) {
	body := RenderJobResultComment(JobResultComment{
		AgentName: "g7-review",
		Runtime:   "codex",
		JobID:     "local-review-g7-review-real",
		JobType:   "review",
		JobState:  string(JobSucceeded),
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "Exact-head review of the diff; every check executed.",
			TestsRun: []string{"go test ./internal/workflow/"},
		},
	})
	if !strings.Contains(body, "**Decision:** `approved`") {
		t.Fatalf("a real verdict lost its headline decision:\n%s", body)
	}
	if strings.Contains(body, "fan-out") {
		t.Fatalf("a real verdict was labelled a dispatch:\n%s", body)
	}
}

// The other half, and the one a one-sided guard would break: an ORDINARY
// below-bar review must still fold. TestRenderJobResultCommentExplainsApprovedWithNotes
// already pins the positive case; this restates it beside the exclusion so the
// two are read together and a future edit cannot satisfy one by sacrificing the
// other.
func TestRenderJobResultCommentStillFoldsAnOrdinaryBelowBarReview(t *testing.T) {
	body := RenderJobResultComment(JobResultComment{
		AgentName:              "audit",
		Runtime:                "claude",
		JobID:                  "review-notes",
		JobType:                "review",
		JobState:               string(JobSucceeded),
		ReviewBlockingSeverity: reviewseverity.P2,
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: reviewseverity.P3,
			Summary:  "non-blocking polish",
			Findings: []json.RawMessage{json.RawMessage(`{"severity":"P3","summary":"rename helper"}`)},
		},
	})

	if !strings.Contains(body, "approved-with-notes") {
		t.Fatalf("an ordinary below-bar review lost its approved-with-notes outcome; the severity contract must survive the fan-out exclusion:\n%s", body)
	}
}
