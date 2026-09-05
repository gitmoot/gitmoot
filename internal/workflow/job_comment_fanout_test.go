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
