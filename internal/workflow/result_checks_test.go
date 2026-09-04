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

	// With evidence AND a declared execution, an approval owes nothing further.
	approved := failedIDs(ResultCheckInput{
		Action: "review",
		Result: AgentResult{
			Decision: "approved", Summary: "looks good",
			TestsRun: []string{"go test ./... -> ok"},
			Evidence: EvidenceExecuted,
		},
	})
	if len(approved) != 0 {
		t.Fatalf("executed evidence-bearing approved review should pass all checks; failed=%v", keys(approved))
	}
}

// TestRunResultChecksNeverPunishesAnHonestStaticOnlyReview is #1839's
// corrected contract, and it exists because the first version of this check
// was wrong in the dangerous direction.
//
// That version passed only when evidence was "executed", so EVERY honest
// static-only verdict failed it - and under result_checks=block a failed check
// is routed to m.fail as a contract violation, killing a verdict the shipped
// contract calls legitimate. The review that caught it was itself static-only.
//
// The distinction the gate needs is carried by the PERSISTED field, which this
// test asserts directly; the check only refuses an execution claim that names
// nothing.
func TestRunResultChecksNeverPunishesAnHonestStaticOnlyReview(t *testing.T) {
	honest := AgentResult{
		Decision: "approved",
		Summary:  "read the diff and the prior verdicts; the pinned toolchain is unreachable in this sandbox so nothing could be run",
		TestsRun: []string{"go build ./... -> could not execute: EACCES on the pinned toolchain"},
		Evidence: EvidenceStaticOnly,
	}
	if failed := failedIDs(ResultCheckInput{Action: "review", Result: honest}); len(failed) != 0 {
		t.Fatalf("an honest static-only review failed checks %v; under result_checks=block that KILLS the job", keys(failed))
	}

	// Omitted is stored as static_only and is equally not punished.
	omitted := honest
	omitted.Evidence = ""
	normalizeAgentResult(&omitted)
	if omitted.Evidence != EvidenceStaticOnly {
		t.Fatalf("omitted evidence normalized to %q, want %q: silence must never become an execution claim", omitted.Evidence, EvidenceStaticOnly)
	}
	if failed := failedIDs(ResultCheckInput{Action: "review", Result: omitted}); len(failed) != 0 {
		t.Fatalf("a verdict with no declaration failed checks %v; absence is not an error", keys(failed))
	}

	// An EXECUTED claim that names nothing it ran is the one shape refused.
	hollow := AgentResult{
		Decision: "approved",
		Summary:  "everything looks fine and the suite is green, trust me on this one please",
		Evidence: EvidenceExecuted,
	}
	failed := failedIDs(ResultCheckInput{Action: "review", Result: hollow})
	if _, ok := failed["review-executed-claim-is-substantiated"]; !ok {
		t.Fatalf("a hollow EXECUTED claim must be refused; failed=%v", keys(failed))
	}

	// The same claim WITH something a reader can check passes.
	substantiated := hollow
	substantiated.TestsRun = []string{"go test ./internal/workflow/ -> ok, 0 failures"}
	if failed := failedIDs(ResultCheckInput{Action: "review", Result: substantiated}); len(failed) != 0 {
		t.Fatalf("a substantiated executed review must pass; failed=%v", keys(failed))
	}

	// And the two remain DISTINGUISHABLE, which is the whole requirement: the
	// stored field differs even though both verdicts pass every check.
	if honest.Evidence == substantiated.Evidence {
		t.Fatal("static-only and executed verdicts carry the same evidence value: they are indistinguishable again")
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

// checkIDs returns every check the input RAN, passed or failed. Absence and
// passing are different facts: a check that stops running always "passes" a
// failure-only assertion, so an exemption can only be pinned by absence.
func checkIDs(in ResultCheckInput) map[string]ResultCheck {
	out := map[string]ResultCheck{}
	for _, check := range RunResultChecks(in) {
		out[check.ID] = check
	}
	return out
}

// #1685. The review-verdict check asks whether a terminal verdict ACCOUNTS FOR
// ITSELF, and the two rows that matter are the ones that broke a
// field-non-emptiness version in opposite directions: a one-token tests_run that
// passed while saying nothing, and an honest docs-only approval that failed while
// saying everything. A coordinator fan-out is exempt — it has nothing to report
// yet by construction, and the shipped review-panel template prescribes exactly
// that result.
func TestRunResultChecksReviewVerdictAccountsForItself(t *testing.T) {
	panelDelegations := []Delegation{
		{ID: "lens-a", Agent: "r1", Action: "review"},
		{ID: "lens-b", Agent: "r2", Action: "review"},
	}
	cases := []struct {
		name       string
		result     AgentResult
		wantFailed []string
		wantPassed []string
		wantAbsent []string
	}{
		{
			name: "an approval that accounts for nothing fails",
			result: AgentResult{
				Decision: "approved", Summary: "looks good to me",
			},
			wantFailed: []string{"review-verdict-has-evidence"},
		},
		{
			// FALSE NEGATIVE the previous version had: one arbitrary token cleared
			// the check, so a fabricated tests_run reached merge eligibility through
			// every guard.
			name: "a one-token tests_run is not evidence",
			result: AgentResult{
				Decision: "approved", Summary: "lgtm", TestsRun: []string{"."},
			},
			wantFailed: []string{"review-verdict-has-evidence"},
		},
		{
			// FALSE POSITIVE the previous version had: an honest approval of a change
			// with nothing to run was recorded as a contract violation. A guard that
			// fails correct behaviour is a guard that gets switched off.
			name: "an honest docs-only approval that explains itself passes",
			result: AgentResult{
				Decision: "approved",
				Summary:  "Read the full diff at head abc123; docs-only, wording is accurate, no code paths touched.",
			},
			wantPassed: []string{"review-verdict-has-evidence"},
		},
		{
			// A single token still counts when it names something real to go and look
			// at, which is what separates it from ".".
			name: "a single token that names a real path is evidence",
			result: AgentResult{
				Decision: "approved", Summary: "ok",
				TestsRun: []string{"internal/workflow/result_checks.go"},
			},
			wantPassed: []string{"review-verdict-has-evidence"},
		},
		{
			// The verbatim "Coordinator Result" from
			// skills/gitmoot/agent-templates/review-panel.md. The product's own
			// documented recipe must not record a contract violation on every run.
			name: "the shipped panel coordinator result is exempt",
			result: AgentResult{
				Decision:    "approved",
				Summary:     "Convening a three-reviewer panel on the PR with diverse lenses.",
				Delegations: panelDelegations,
			},
			wantAbsent: []string{"review-verdict-has-evidence"},
		},
		{
			// blocked/failed are self-describing non-answers that legitimately carry
			// no findings or tests. Nudging them would turn an honest "I could not
			// review this" into a recorded violation.
			name: "a blocked review is not nudged",
			result: AgentResult{
				Decision: "blocked", Summary: "checkout unavailable",
				Needs: []string{"a working checkout"},
			},
			wantAbsent: []string{"review-verdict-has-evidence"},
		},
		{
			// A fan-out that requests changes still owes findings, because
			// review-evidence-present is about an actionable rejection rather than
			// about who produced the verdict.
			name: "a changes-requested fan-out still owes findings",
			result: AgentResult{
				Decision: "changes_requested", Summary: "panel will report",
				Delegations: panelDelegations,
			},
			wantFailed: []string{"review-evidence-present"},
			wantAbsent: []string{"review-verdict-has-evidence"},
		},
		{
			// ACCEPTANCE: the real g7-review verdict shape from #1690 — findings[]
			// empty beside a POPULATED tests_run. A guard that rejects this has made
			// honest approvals impossible, which is the failure that gets guards
			// switched off.
			name: "a real evidence-bearing approval passes every check",
			result: AgentResult{
				Decision: "approved", Summary: "verified at exact head",
				TestsRun: []string{"go build ./... -> ok", "go test ./internal/cli -> ok, 12 tests"},
			},
			wantPassed: []string{"review-verdict-has-evidence"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran := checkIDs(ResultCheckInput{Action: "review", Result: tc.result})
			failed := failedIDs(ResultCheckInput{Action: "review", Result: tc.result})
			for _, id := range tc.wantFailed {
				if _, ok := failed[id]; !ok {
					t.Fatalf("%s did not fail; failed = %v", id, keys(failed))
				}
			}
			for _, id := range tc.wantPassed {
				if _, ok := ran[id]; !ok {
					t.Fatalf("%s did not run, so it cannot have passed; ran = %v", id, keys(ran))
				}
				if c, ok := failed[id]; ok {
					t.Fatalf("%s must not fail here: %s", id, c.Explanation)
				}
			}
			for _, id := range tc.wantAbsent {
				if _, ok := ran[id]; ok {
					t.Fatalf("%s must not run for this result; ran = %v", id, keys(ran))
				}
			}
		})
	}
}

// The nudge is scoped to review jobs. An implement or ask job legitimately
// returns a terminal decision with an empty evidence set, and must not be held
// to a review-slot obligation. Asserted by ABSENCE: a check that never runs
// would satisfy a failure-only assertion no matter how the scope changed.
func TestRunResultChecksVerdictNudgeIsScopedToReviewJobs(t *testing.T) {
	for _, action := range []string{"implement", "ask"} {
		ran := checkIDs(ResultCheckInput{
			Action: action,
			Result: AgentResult{
				Decision: "approved", Summary: "fanning out the work",
				Delegations: []Delegation{{ID: "child", Agent: "a", Action: "implement"}},
			},
		})
		if _, ok := ran["review-verdict-has-evidence"]; ok {
			t.Fatalf("action %q must not run the review verdict nudge; ran = %v", action, keys(ran))
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

// TestExecutedClaimNeedsARunnableTarget pins the round-3 finding that entries
// stating the OPPOSITE of execution were accepted as proof of it: "none run"
// and "could not run" are two words each, so the two-word arm let them through.
func TestExecutedClaimNeedsARunnableTarget(t *testing.T) {
	hollow := []string{"none run", "could not run", "nothing to run", "ok", "n/a"}
	for _, entry := range hollow {
		result := AgentResult{
			Decision: "approved",
			Summary:  "a summary long enough to satisfy the accounting floor on its own merits",
			TestsRun: []string{entry},
			Evidence: EvidenceExecuted,
		}
		failed := failedIDs(ResultCheckInput{Action: "review", Result: result})
		if _, ok := failed["review-executed-claim-is-substantiated"]; !ok {
			t.Errorf("tests_run %q substantiated an EXECUTED claim; failed=%v", entry, keys(failed))
		}
	}

	real := []string{"go test ./... -> ok", "go build ./internal/workflow/ rc=0", "gofmt -l internal/ -> clean"}
	for _, entry := range real {
		result := AgentResult{
			Decision: "approved",
			Summary:  "a summary long enough to satisfy the accounting floor on its own merits",
			TestsRun: []string{entry},
			Evidence: EvidenceExecuted,
		}
		if failed := failedIDs(ResultCheckInput{Action: "review", Result: result}); len(failed) != 0 {
			t.Errorf("tests_run %q is a real command and must pass; failed=%v", entry, keys(failed))
		}
	}

	// An honest static-only verdict still passes with the SAME hollow entries,
	// because it claims nothing.
	honest := AgentResult{
		Decision: "approved",
		Summary:  "read the diff and prior verdicts; the toolchain was unreachable so nothing could be run",
		TestsRun: []string{"none run"},
		Evidence: EvidenceStaticOnly,
	}
	if failed := failedIDs(ResultCheckInput{Action: "review", Result: honest}); len(failed) != 0 {
		t.Fatalf("an honest static-only review failed %v; under result_checks=block that KILLS the job", keys(failed))
	}
}
