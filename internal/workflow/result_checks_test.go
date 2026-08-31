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

	// An approved review carries no FINDINGS obligation — but #1685 gave it an
	// EVIDENCE obligation. This assertion previously read "approved review should
	// pass all checks" for a bare summary, which is precisely the shape that
	// reached a near-merge twice on #1682 and #1691.
	bare := failedIDs(ResultCheckInput{
		Action: "review",
		Result: AgentResult{Decision: "approved", Summary: "looks good"},
	})
	if _, ok := bare["review-evidence-present"]; ok {
		t.Fatalf("approved review must not owe findings; failed=%v", keys(bare))
	}
	if _, ok := bare["review-verdict-has-evidence"]; !ok {
		t.Fatalf("evidence-free approval must fail the evidence floor; failed=%v", keys(bare))
	}

	// With evidence, an approval owes nothing further.
	approved := failedIDs(ResultCheckInput{
		Action: "review",
		Result: AgentResult{
			Decision: "approved", Summary: "looks good",
			TestsRun: []string{"go test ./... -> ok"},
		},
	})
	if len(approved) != 0 {
		t.Fatalf("evidence-bearing approved review should pass all checks; failed=%v", keys(approved))
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

// #1685. The two review-verdict guards overlap on the shape that was actually
// caught live, which is exactly why each needs a case the OTHER one clears:
// an overlapping pair is one guard wearing two names, and deleting either would
// then look free. The table's first two rows are those disjoint cases.
func TestRunResultChecksReviewVerdictIntegrityGuardsAreDistinct(t *testing.T) {
	panelDelegations := []Delegation{
		{ID: "lens-a", Agent: "r1", Action: "review"},
		{ID: "lens-b", Agent: "r2", Action: "review"},
	}
	cases := []struct {
		name       string
		result     AgentResult
		wantFailed []string
		wantPassed []string
	}{
		{
			// Only the continuation guard can see this: the row carries ten real
			// tests_run entries, so the evidence floor is satisfied and clears it.
			name: "fan-out carrying evidence is caught only by the continuation guard",
			result: AgentResult{
				Decision: "approved", Summary: "convening a three-reviewer panel",
				TestsRun: []string{"go build ./... -> ok"}, Delegations: panelDelegations,
			},
			wantFailed: []string{"review-verdict-not-a-continuation"},
			wantPassed: []string{"review-verdict-has-evidence"},
		},
		{
			// Only the evidence floor can see this: no delegations are declared, so
			// the continuation guard has nothing to fire on. This is the variant
			// whose cause nobody has diagnosed yet.
			name: "evidence-free approval with no delegations is caught only by the evidence floor",
			result: AgentResult{
				Decision: "approved", Summary: "looks good to me",
			},
			wantFailed: []string{"review-verdict-has-evidence"},
			wantPassed: []string{"review-verdict-not-a-continuation"},
		},
		{
			// The shape measured live on #1682 and #1691: both guards fire. Pinned so
			// a future refactor cannot quietly leave only one covering it.
			name: "the live vacuous approval trips both guards",
			result: AgentResult{
				Decision: "approved", Summary: "Convening a three-reviewer panel for PR #1682",
				Delegations: panelDelegations,
			},
			wantFailed: []string{"review-verdict-not-a-continuation", "review-verdict-has-evidence"},
		},
		{
			// changes_requested is a terminal verdict too, so a fan-out announcing one
			// is just as much a non-verdict as a fan-out announcing an approval.
			name: "a changes-requested fan-out is a continuation as well",
			result: AgentResult{
				Decision: "changes_requested", Summary: "panel will report",
				Findings: []json.RawMessage{json.RawMessage(`{"severity":"P2"}`)},
				TestsRun: []string{"go test ./... -> ok"}, Delegations: panelDelegations,
			},
			wantFailed: []string{"review-verdict-not-a-continuation"},
			wantPassed: []string{"review-verdict-has-evidence", "review-evidence-present"},
		},
		{
			// blocked/failed are self-describing non-answers that no consumer reads as
			// an approval, and they legitimately carry no findings or tests. Guarding
			// them would turn an honest "I could not review this" into a hard failure.
			name: "a blocked review is not held to the verdict guards",
			result: AgentResult{
				Decision: "blocked", Summary: "checkout unavailable",
				Needs: []string{"a working checkout"}, Delegations: panelDelegations,
			},
			wantPassed: []string{"review-verdict-not-a-continuation", "review-verdict-has-evidence"},
		},
		{
			// ACCEPTANCE: the real g7-review verdict shape from #1690 — findings[]
			// empty beside a POPULATED tests_run. A guard that rejects this has made
			// honest approvals impossible, which is the failure that gets guards
			// switched off.
			name: "a real evidence-bearing approval passes every guard",
			result: AgentResult{
				Decision: "approved", Summary: "verified at exact head",
				TestsRun: []string{"go build ./... -> ok", "go test ./internal/cli -> ok, 12 tests"},
			},
			wantPassed: []string{
				"review-verdict-not-a-continuation", "review-verdict-has-evidence", "review-evidence-present",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failed := failedIDs(ResultCheckInput{Action: "review", Result: tc.result})
			for _, id := range tc.wantFailed {
				if _, ok := failed[id]; !ok {
					t.Fatalf("%s did not fail; failed = %v", id, keys(failed))
				}
			}
			for _, id := range tc.wantPassed {
				if c, ok := failed[id]; ok {
					t.Fatalf("%s must not fail here: %s", id, c.Explanation)
				}
			}
		})
	}
}

// The guards are scoped to review jobs. An implement or ask job legitimately
// returns delegations with a terminal decision — that is the ordinary
// coordinator fan-out — and must not be caught by a review-slot guard.
func TestRunResultChecksVerdictGuardsAreScopedToReviewJobs(t *testing.T) {
	for _, action := range []string{"implement", "ask"} {
		failed := failedIDs(ResultCheckInput{
			Action: action,
			Result: AgentResult{
				Decision: "approved", Summary: "fanning out the work",
				Delegations: []Delegation{{ID: "child", Agent: "a", Action: "implement"}},
			},
		})
		for _, id := range []string{"review-verdict-not-a-continuation", "review-verdict-has-evidence"} {
			if _, ok := failed[id]; ok {
				t.Fatalf("action %q must not run review guard %s; failed = %v", action, id, keys(failed))
			}
		}
	}
}

func keys(m map[string]ResultCheck) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
