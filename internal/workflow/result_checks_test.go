package workflow

import (
	"encoding/json"
	"testing"
)

// failedIDs runs the audit for the given input and returns the set of failed
// check ids, for compact assertions.
func failedIDs(in ResultCheckInput) map[string]ResultCheck {
	out := map[string]ResultCheck{}
	for _, c := range FailedResultChecks(in) {
		out[c.ID] = c
	}
	return out
}

func TestRunResultChecksImplementIncomplete(t *testing.T) {
	// An implement job claiming "implemented" with no changes and no tests fails
	// both implement checks.
	failed := failedIDs(ResultCheckInput{
		Action: "implement",
		Result: AgentResult{Decision: "implemented", Summary: "did the thing"},
	})
	for _, id := range []string{"implement-changes-listed", "implement-tests-listed"} {
		if c, ok := failed[id]; !ok {
			t.Fatalf("expected %s to fail; failed=%v", id, keys(failed))
		} else if c.Pass || c.Explanation == "" {
			t.Fatalf("%s should be a failed check with an explanation: %+v", id, c)
		}
	}
}

func TestRunResultChecksImplementClean(t *testing.T) {
	// A complete implement result passes every check.
	failed := failedIDs(ResultCheckInput{
		Action: "implement",
		Result: AgentResult{
			Decision:    "implemented",
			Summary:     "implemented feature X",
			ChangesMade: []string{"added foo.go"},
			TestsRun:    []string{"go test ./..."},
		},
	})
	if len(failed) != 0 {
		t.Fatalf("clean implement result should pass all checks; failed=%v", keys(failed))
	}
}

func TestRunResultChecksImplementNonImplementedDecisionSkipsChecks(t *testing.T) {
	// The implement checks only apply to a decision of "implemented"; a blocked
	// implement job is audited by the blocked check instead.
	failed := failedIDs(ResultCheckInput{
		Action: "implement",
		Result: AgentResult{Decision: "blocked", Summary: "cannot proceed", Needs: []string{"need creds"}},
	})
	if _, ok := failed["implement-changes-listed"]; ok {
		t.Fatalf("implement checks must not fire on a non-implemented decision; failed=%v", keys(failed))
	}
}

func TestRunResultChecksReviewChangesRequestedNeedsEvidence(t *testing.T) {
	// changes_requested with no findings fails; with findings passes.
	failed := failedIDs(ResultCheckInput{
		Action: "review",
		Result: AgentResult{Decision: "changes_requested", Summary: "please fix"},
	})
	if _, ok := failed["review-evidence-present"]; !ok {
		t.Fatalf("expected review-evidence-present to fail; failed=%v", keys(failed))
	}

	withFindings := failedIDs(ResultCheckInput{
		Action: "review",
		Result: AgentResult{
			Decision: "changes_requested",
			Summary:  "please fix",
			Findings: []json.RawMessage{json.RawMessage(`{"file":"a.go","issue":"bug"}`)},
		},
	})
	if _, ok := withFindings["review-evidence-present"]; ok {
		t.Fatalf("review with findings must pass; failed=%v", keys(withFindings))
	}

	// An approved review carries no evidence obligation.
	approved := failedIDs(ResultCheckInput{
		Action: "review",
		Result: AgentResult{Decision: "approved", Summary: "looks good"},
	})
	if len(approved) != 0 {
		t.Fatalf("approved review should pass all checks; failed=%v", keys(approved))
	}
}

func TestRunResultChecksAskAnswerActionable(t *testing.T) {
	// A degenerate single-char answer with no body fails; a real answer passes.
	failed := failedIDs(ResultCheckInput{
		Action: "ask",
		Result: AgentResult{Decision: "approved", Summary: "s"},
	})
	if _, ok := failed["ask-answer-actionable"]; !ok {
		t.Fatalf("expected ask-answer-actionable to fail on a 1-char answer; failed=%v", keys(failed))
	}

	ok := failedIDs(ResultCheckInput{
		Action: "ask",
		Result: AgentResult{Decision: "approved", Summary: "yes, use approach B because it is simpler"},
	})
	if _, bad := ok["ask-answer-actionable"]; bad {
		t.Fatalf("a substantive ask answer must pass; failed=%v", keys(ok))
	}

	// An answer delivered via artifact_body (short summary) still passes.
	viaBody := failedIDs(ResultCheckInput{
		Action: "ask",
		Result: AgentResult{Decision: "approved", Summary: "s", ArtifactBody: "# Answer\n\nDetailed body here."},
	})
	if _, bad := viaBody["ask-answer-actionable"]; bad {
		t.Fatalf("an ask answer in artifact_body must pass; failed=%v", keys(viaBody))
	}
}

func TestRunResultChecksBlockedBlockersActionable(t *testing.T) {
	// blocked with an empty/blank needs list fails the decision-scoped check
	// regardless of action.
	failed := failedIDs(ResultCheckInput{
		Action: "review",
		Result: AgentResult{Decision: "blocked", Summary: "stuck", Needs: []string{"   ", ""}},
	})
	if _, ok := failed["blocked-blockers-actionable"]; !ok {
		t.Fatalf("expected blocked-blockers-actionable to fail on blank needs; failed=%v", keys(failed))
	}

	ok := failedIDs(ResultCheckInput{
		Action: "review",
		Result: AgentResult{Decision: "blocked", Summary: "stuck", Needs: []string{"missing GITHUB_TOKEN"}},
	})
	if _, bad := ok["blocked-blockers-actionable"]; bad {
		t.Fatalf("blocked with an actionable need must pass; failed=%v", keys(ok))
	}
}

func TestRunResultChecksCoordinatorFinalize(t *testing.T) {
	// A finalize continuation (action ask + IsFinalize) is audited by the
	// coordinator check, NOT the ask-answer check.
	failed := failedIDs(ResultCheckInput{
		Action:     "ask",
		IsFinalize: true,
		Result:     AgentResult{Decision: "failed", Summary: "x"},
	})
	if _, ok := failed["coordinator-outcome-reconciled"]; !ok {
		t.Fatalf("expected coordinator-outcome-reconciled to fail on a terse finalize; failed=%v", keys(failed))
	}
	if _, ok := failed["ask-answer-actionable"]; ok {
		t.Fatalf("a finalize continuation must not run the plain ask-answer check; failed=%v", keys(failed))
	}

	ok := failedIDs(ResultCheckInput{
		Action:     "ask",
		IsFinalize: true,
		Result:     AgentResult{Decision: "failed", Summary: "reconciled: child A merged, child B failed and was escalated"},
	})
	if _, bad := ok["coordinator-outcome-reconciled"]; bad {
		t.Fatalf("a substantive reconciliation must pass; failed=%v", keys(ok))
	}
}

func TestFailedResultChecksCleanIsEmpty(t *testing.T) {
	if got := FailedResultChecks(ResultCheckInput{Action: "ask", Result: AgentResult{Decision: "approved", Summary: "a real answer"}}); len(got) != 0 {
		t.Fatalf("clean result should yield no failures, got %+v", got)
	}
}

func TestSummarizeResultChecks(t *testing.T) {
	if got := SummarizeResultChecks(nil); got != "all result checks passed" {
		t.Fatalf("empty summary = %q", got)
	}
	s := SummarizeResultChecks([]ResultCheck{{ID: "a", Explanation: "because"}, {ID: "b", Explanation: "reasons"}})
	if want := "2 result check(s) failed: a (because); b (reasons)"; s != want {
		t.Fatalf("summary = %q, want %q", s, want)
	}
}

func keys(m map[string]ResultCheck) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
