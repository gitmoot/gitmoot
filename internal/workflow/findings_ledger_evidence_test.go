package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1850 MERGE-HEAD P1, RULED ON IN DIRECTIVE 117886. THE PERMANENT REGRESSION.
//
// I derived the ledger's evidence kind from len(tests_run) > 0. That is the
// classic instrument error: a field that CAN be filled by something that did
// not execute, used as proof that something executed. Main's merge brought the
// real discriminator, AgentResult.Evidence, which result.go DEFAULTS to
// static_only with the stated reason that nothing may read "we could not run
// the gate" as "the gate passed" - and my writer never consulted it.
//
// MEASURED BEFORE THE FIX, with a throwaway probe at the merge head: a verdict
// declaring Evidence=static_only whose tests_run listed three items, ONE OF
// THEM "go build -> COULD NOT RUN: permission denied", was recorded as EXECUTED
// with count 3; and as a continuation it DISCHARGED a prior open P1, with the
// gate returning nil. That is evidence-free discharge, the exact class this
// ledger exists to prevent, and the more scrupulous the static reviewer the
// stronger its false discharge, because the acceptance rule asks static
// reviewers to state what they examined.
//
// BOTH TESTS BELOW ENTER THROUGH AdvanceJob, the production advance path, not
// through the writer helper. A routing mutant that leaves real verdicts flowing
// through another path must fail them.

// The refusing half: a static_only verdict must not record a discharging kind,
// and must not clear a prior obligation.
func TestAdvanceJobRefusesToRecordStaticOnlyAsExecuted(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("c", 40)
	// #1524: the objection arm now refuses a verdict whose head cannot be compared
	// to the pull request's observed head. This fixture is about static-only evidence recording, not about
	// heads, so it seeds the observation explicitly and keeps its own subject.
	if err := store.UpsertPullRequest(context.Background(), db.PullRequest{
		RepoFullName: "gitmoot/gitmoot", Number: 1900, HeadBranch: "task-9", BaseBranch: "main",
		HeadSHA: head, State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	prior := strings.Repeat("d", 40)

	uid, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1900, HeadSHA: prior, ObserverJob: "review-0",
		State: db.FindingOpen, Severity: "P1", RoundLabel: "F-1", Title: "prior defect",
		File: "internal/run.go", EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test -> FAIL"}, ExecutedCount: 1,
	})
	if err != nil {
		t.Fatalf("seed prior finding: %v", err)
	}

	// A static-only reviewer that HONESTLY lists what it examined, including an
	// instrument that could not run. This is the shape the acceptance rule asks
	// for, which is what made the defect so dangerous.
	insertCompletedJob(t, store, db.Job{ID: "review-static", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 1900, HeadSHA: head,
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "static-only continuation",
			Evidence: EvidenceStaticOnly,
			TestsRun: []string{
				"read internal/run.go in full",
				"grep -rn Foo internal/ -> 3 hits",
				"go build -> COULD NOT RUN: permission denied",
			},
			Findings: []json.RawMessage{json.RawMessage(`{"id":"F1","severity":"P1","file":"internal/run.go","title":"prior defect","continues_uid":"` + uid + `","state":"answered"}`)},
		},
	})
	if err := engine.AdvanceJob(ctx, "review-static"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}

	rows, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1900)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var atHead *db.ReviewFindingObservation
	for i := range rows {
		if rows[i].HeadSHA == head {
			atHead = &rows[i]
		}
	}
	if atHead == nil {
		t.Fatal("the static-only verdict recorded nothing at its head; the observation must be kept, only not as execution")
	}
	if atHead.EvidenceKind == db.EvidenceExecuted {
		t.Fatalf("a static_only verdict was recorded as EXECUTED with count %d; that is a MANUFACTURED EXECUTION COUNT", atHead.ExecutedCount)
	}
	// THE PRECISE DEFECT WAS THE COUNT, so pin it: whatever kind is recorded,
	// nothing may read as "this many checks ran" when the verdict declared it
	// ran nothing.
	if atHead.ExecutedCount != 0 {
		t.Fatalf("executed count = %d for a verdict declaring static_only; it must be zero", atHead.ExecutedCount)
	}
	// The examined-list is still PRESERVED, because it is real and useful; it is
	// simply not an execution claim.
	if len(atHead.ExecutedCommands) != 3 {
		t.Fatalf("the reviewer's examined-list was dropped (%d entries); it is worth keeping, just not as execution", len(atHead.ExecutedCommands))
	}
	// AND THE PROTECTION THAT MAPPING THIS TO QUOTED WOULD HAVE LOST. Only
	// EvidenceStatic is re-checked by the locator-existence guard, so a quoted
	// row would have evaded the round-2 F5 rule that a discharge whose cited
	// path has vanished stops counting as an answer. With the STATIC fallback
	// the guard applies: a resolver reporting the locator gone must re-arm the
	// obligation.
	//
	// THE HEAD MATTERS AND MY FIRST VERSION OF THIS ASSERTION GOT IT WRONG. An
	// observation recorded AT head H satisfies the obligation at H by
	// definition, so dischargedAtHead short-circuits before any locator is
	// consulted. The re-arm is a LATER-head property, so it must be queried at a
	// later head. I am recording the correction rather than quietly moving the
	// argument, because the first version failed and the failure was mine.
	later := strings.Repeat("9", 40)
	gone := LedgerScope{PathExistsAtHead: func(context.Context, string, string) (bool, error) { return false, nil }}
	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 1900, later, gone); err == nil {
		t.Fatal("a static_only answer whose cited locator no longer exists still discharged the obligation at a later head; the locator-existence guard is not reaching this row")
	}
	// PASSING CASE FOR THE SAME ROW, so the guard is not satisfied by refusing
	// everything: with the locator present the answer legitimately stands. This
	// is the 58-percent-of-reviews case, and blocking it would rebuild the merge
	// wedge that rounds 2 and 3 closed.
	present := LedgerScope{PathExistsAtHead: func(context.Context, string, string) (bool, error) { return true, nil }}
	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 1900, later, present); err != nil {
		t.Fatalf("a static review that cited an existing locator was refused at a later head: %v", err)
	}
	// AND AT ITS OWN HEAD it discharges regardless, which is what makes the
	// 58-percent case work: the overwhelming majority of real reviewers omit the
	// evidence field, and they must still be able to answer a finding.
	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 1900, head, LedgerScope{}); err != nil {
		t.Fatalf("a static review was refused at its OWN head: %v", err)
	}
}

// The passing half, written because every bound added to this repo lately has
// had a version that rejected valid input: a genuinely EXECUTED verdict must
// still record EXECUTED and must still discharge.
func TestAdvanceJobStillRecordsExecutedForAnExecutedVerdict(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("e", 40)
	prior := strings.Repeat("f", 40)

	uid, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1901, HeadSHA: prior, ObserverJob: "review-0",
		State: db.FindingOpen, Severity: "P1", RoundLabel: "F-1", Title: "prior defect",
		File: "internal/run.go", EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test -> FAIL"}, ExecutedCount: 1,
	})
	if err != nil {
		t.Fatalf("seed prior finding: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-exec", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 1901, HeadSHA: head,
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "executed re-review",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/ -> ok", "go vet ./internal/... -> clean"},
			Findings: []json.RawMessage{json.RawMessage(`{"id":"F1","severity":"P1","file":"internal/run.go","title":"prior defect","continues_uid":"` + uid + `","state":"answered"}`)},
		},
	})
	if err := engine.AdvanceJob(ctx, "review-exec"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}

	rows, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1901)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var atHead *db.ReviewFindingObservation
	for i := range rows {
		if rows[i].HeadSHA == head {
			atHead = &rows[i]
		}
	}
	if atHead == nil {
		t.Fatal("an executed verdict recorded nothing at its head")
	}
	if atHead.EvidenceKind != db.EvidenceExecuted {
		t.Fatalf("an EXECUTED verdict recorded %q; the fix must not refuse legitimate execution", atHead.EvidenceKind)
	}
	if atHead.ExecutedCount != 2 {
		t.Fatalf("executed count = %d, want 2", atHead.ExecutedCount)
	}
	// AND IT MUST STILL DISCHARGE, which is the property a stricter rule is most
	// likely to break.
	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 1901, head, LedgerScope{}); err != nil {
		t.Fatalf("a genuinely EXECUTED answer failed to discharge its obligation: %v", err)
	}
}

// DIRECTIVE 117921's MANDATORY 58-PERCENT COHORT TEST. Measured on one day's
// 59 succeeded review jobs: 7 declared executed, 2 declared static_only, and
// FIFTY omitted the field entirely, of which 33 listed a non-empty tests_run.
// result.go defaults absence to static_only, so this cohort is the great
// majority of real reviewers.
//
// THEY MUST STILL BE ABLE TO ANSWER A FINDING. If an absent-field reviewer that
// supplies an EXISTING locator plus a rationale cannot discharge, the merge
// wedges for almost every PR carrying a prior finding, which is the failure
// rounds 2 and 3 closed and which my first version of this fix would have
// rebuilt from the other side. Pinning it here so a future tightening cannot
// re-create the wedge silently.
func TestAdvanceJobLetsAnAbsentEvidenceReviewerDischargeViaAnExistingLocator(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("2", 40)
	prior := strings.Repeat("3", 40)

	uid, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1902, HeadSHA: prior, ObserverJob: "review-0",
		State: db.FindingOpen, Severity: "P1", RoundLabel: "F-1", Title: "prior defect",
		File: "internal/run.go", EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test -> FAIL"}, ExecutedCount: 1,
	})
	if err != nil {
		t.Fatalf("seed prior finding: %v", err)
	}

	// NO Evidence FIELD AT ALL, which is the 50-of-59 shape, answering the prior
	// finding and citing where it looked.
	insertCompletedJob(t, store, db.Job{ID: "review-absent", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 1902, HeadSHA: head,
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "read the fix and it is correct",
			TestsRun: []string{"read internal/run.go", "grep -rn guard internal/ -> 2 hits"},
			Findings: []json.RawMessage{json.RawMessage(`{"id":"F1","severity":"P1","file":"internal/run.go","title":"prior defect","continues_uid":"` + uid + `","state":"answered","rationale":"the guard now covers the case"}`)},
		},
	})
	if err := engine.AdvanceJob(ctx, "review-absent"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}

	rows, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1902)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var atHead *db.ReviewFindingObservation
	for i := range rows {
		if rows[i].HeadSHA == head {
			atHead = &rows[i]
		}
	}
	if atHead == nil {
		t.Fatal("an absent-evidence reviewer recorded nothing at its head")
	}
	if atHead.EvidenceKind == db.EvidenceExecuted {
		t.Fatalf("an absent Evidence field was read as a declaration of execution (count %d); item 3 of the ruling forbids that", atHead.ExecutedCount)
	}
	if atHead.EvidenceKind != db.EvidenceStatic {
		t.Fatalf("evidence kind = %q, want STATIC so the observation lives or dies on its locator", atHead.EvidenceKind)
	}
	// THE PROPERTY THAT MATTERS: with the cited locator present, it discharges.
	present := LedgerScope{PathExistsAtHead: func(context.Context, string, string) (bool, error) { return true, nil }}
	later := strings.Repeat("4", 40)
	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 1902, later, present); err != nil {
		t.Fatalf("THE WEDGE REBUILT: the 58-percent cohort could not discharge a finding it answered with an existing locator: %v", err)
	}
	// And the negative control on the same row, so the pass is not vacuous: with
	// the locator gone it must re-arm.
	gone := LedgerScope{PathExistsAtHead: func(context.Context, string, string) (bool, error) { return false, nil }}
	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 1902, later, gone); err == nil {
		t.Fatal("a STATIC answer whose cited path is gone still discharged; the locator is not load-bearing")
	}
}
