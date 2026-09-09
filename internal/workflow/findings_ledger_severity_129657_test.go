package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// OWNER DECISION, workflow note 129657: the merge gate blocks on P1 and P2 only.
// P3 is REPORTED and does not hold the merge.
//
// The three properties that decide whether this change is safe, one test each:
// a P1 still holds, a P3 alone does not, and a P3 that stops blocking does not
// also stop being visible. The visibility half is not decoration - it is the
// reason the change is safe at all. A signal that costs a review round and buys
// nothing stops being filed, and two real P3s in this campaign (an orphaned
// comment and an unpinned map ordering) were only caught because someone filed
// one.

func seedFinding(t *testing.T, store *db.Store, pr int64, head, uid, severity string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: pr, HeadSHA: head, ObserverJob: "review-1",
		State: db.FindingOpen, Severity: severity, RoundLabel: uid, Title: "a finding",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test -> FAIL"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("seed %s/%s: %v", uid, severity, err)
	}
}

// A P1 AT THE QUEUED HEAD STILL HOLDS. This is the property the whole gate
// exists for and the one phobos made a constraint: criterion 6 reads the LEDGER,
// not the verdict, so a P1 recorded at this head blocks even when a review says
// approved. If the severity split ever widens to swallow this, the gate is gone.
func TestP1AtTheHeadStillBlocksAfterTheSeverityChange(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	oldHead, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	seedFinding(t, store, 7001, oldHead, "F-1", "P1")

	err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 7001, head, LedgerScope{TaskID: "task-7001"})
	if err == nil {
		t.Fatal("a P1 outstanding at the evaluated head did not block; the gate has no subject left")
	}
	if !strings.Contains(err.Error(), "P1") {
		t.Fatalf("refusal does not name the severity that caused it: %v", err)
	}
}

// A P3 ALONE DOES NOT HOLD. This is the change.
func TestP3AloneDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	oldHead, head := strings.Repeat("c", 40), strings.Repeat("d", 40)
	seedFinding(t, store, 7002, oldHead, "F-1", "P3")

	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 7002, head, LedgerScope{TaskID: "task-7002"}); err != nil {
		t.Fatalf("a P3 alone held the merge, which note 129657 removed: %v", err)
	}
}

// AND A P3 BESIDE A P1 DOES NOT DILUTE THE P1. The mixed case is where a naive
// "if any non-blocking, permit" would show up.
func TestP3BesideAP1StillBlocksOnTheP1(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	oldHead, head := strings.Repeat("e", 40), strings.Repeat("f", 40)
	seedFinding(t, store, 7003, oldHead, "F-1", "P3")
	seedFinding(t, store, 7003, oldHead, "F-2", "P1")

	err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 7003, head, LedgerScope{TaskID: "task-7003"})
	if err == nil {
		t.Fatal("a P3 in the same set suppressed a P1 refusal")
	}
	if strings.Contains(err.Error(), "F-1") {
		t.Fatalf("the refusal names the non-blocking P3 as a cause: %v", err)
	}
	if !strings.Contains(err.Error(), "F-2") {
		t.Fatalf("the refusal does not name the P1 that caused it: %v", err)
	}
}

// A BLANK OR UNRECOGNISED SEVERITY IS NOT A BYPASS.
//
// THE WRITER CANNOT PRODUCE ONE TODAY and the reader must still handle it:
// RecordReviewFindingObservation rejects anything outside P0/P1/P2/P3, so this
// is asserted against the PREDICATE rather than end to end. That is not a
// weaker test, it is the only reachable one - and it is not hypothetical, because
// 44 LEGACY ROWS with an empty severity predate that validator and 43 of them are
// open in the live store right now. The gate reads rows, not the writer's rules.
//
// Reading "P1 and P2 only" as "nothing else blocks" would have silently
// unblocked every one of those - a wider change than the one ruled, in the
// direction that loses obligations.
func TestAnUnrecognisedSeverityStillBlocks(t *testing.T) {
	for _, severity := range []string{"", "   ", "p4", "minor", "NIT", "critical"} {
		if !obligationSeverityBlocks(severity) {
			t.Errorf("severity %q does not block; an unrecognised label is a merge bypass", severity)
		}
	}
}

// P0 BLOCKS, AND IT IS IN THE STORE'S VOCABULARY EVEN THOUGH THE CENSUS HAS NONE.
// RecordReviewFindingObservation accepts P0/P1/P2/P3; the corpus today is
// P1 318, P2 414, P3 239 and 44 blank, so P0 is writable and unused. A severity
// change that only reasoned about the labels PRESENT would leave the most severe
// writable label unconsidered.
func TestP0BlocksEndToEnd(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	oldHead, head := strings.Repeat("9", 40), strings.Repeat("0", 40)
	seedFinding(t, store, 7008, oldHead, "F-1", "P0")

	err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 7008, head, LedgerScope{TaskID: "task-7008"})
	if err == nil {
		t.Fatal("a P0 did not block the merge")
	}
}

// P3 MATCHING TOLERATES CASE AND SPACE at the predicate, for the same
// legacy-row reason: the writer normalises today, the reader must not depend on
// it having always done so.
func TestP3ExemptionToleratesCaseAndSpace(t *testing.T) {
	for _, severity := range []string{"P3", "p3", " P3 ", "\tp3\n"} {
		if obligationSeverityBlocks(severity) {
			t.Errorf("severity %q blocked; note 129657 exempts P3 however it is spelled", severity)
		}
	}
}

// REPORTED, NOT SILENT - ASSERTED IN THE PRODUCTION SHAPE, ON THE PRODUCTION
// CHANNEL.
//
// THE FIRST VERSION OF THIS TEST SUPPLIED ITS OWN scope.Degraded SINK AND
// ASSERTED ON THAT. Production never does: scope.Degraded starts nil and is
// auto-wired to a findings_ledger_scope_degraded task event, and the merge-gate
// path never supplies a sink. So the test asserted a channel the production
// caller does not use, and the #2102 reviewer proved the gap by DELETING the
// dedicated AddTaskEvent call - all nine tests still passed.
//
// It now runs with TaskID set and Degraded nil, exactly as the gate does, and
// asserts the durable record an operator would actually find.
func TestANonBlockingP3IsStillReported(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	oldHead, head := strings.Repeat("5", 40), strings.Repeat("6", 40)
	seedFinding(t, store, 7006, oldHead, "F-1", "P3")

	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 7006, head, LedgerScope{TaskID: "task-7006"}); err != nil {
		t.Fatalf("P3 blocked: %v", err)
	}

	events, err := store.ListTaskEvents(ctx, "task-7006")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	var reported, degraded int
	var reason string
	for _, event := range events {
		switch event.Kind {
		case "findings_ledger_reported_not_blocking":
			reported++
			reason = event.Reason
		case "findings_ledger_scope_degraded":
			degraded++
		}
	}
	if reported != 1 {
		t.Fatalf("got %d findings_ledger_reported_not_blocking event(s), want exactly 1; a P3 that stops blocking must not stop being recorded", reported)
	}
	if !strings.Contains(reason, "F-1") {
		t.Fatalf("the recorded report does not name the finding: %q", reason)
	}
	// AND NOT ON THE DEGRADATION CHANNEL. A routine classification labelled as a
	// resolver failure corrupts the signal that tells an operator the ledger
	// could not trust its own answer.
	if degraded != 0 {
		t.Fatalf("got %d findings_ledger_scope_degraded event(s) for a successful classification, want 0", degraded)
	}
}

// A CALLER-SUPPLIED DEGRADED SINK IS NOT A REPORTING CHANNEL EITHER.
//
// THE ATTRIBUTION HERE HAS BEEN WRONG IN BOTH DIRECTIONS IN ONE AFTERNOON, so
// it is dated rather than asserted.
//
// I first wrote that "the CLI supplies one". The #2102 reviewer checked main and
// found no production caller assigning Degraded - the only LedgerScope
// constructor was LedgerResolvers.ScopeFor, which never does - so the claim
// described gm-findings' then-unmerged PR #2099 while reading as a statement
// about main. Correct finding.
//
// It stopped being correct at 15:39:11Z, when #2099 merged as 9c892237, which
// added a production caller assigning scope.Degraded for
// `gitmoot findings --at-head`. So the caller this test defends against is real
// and on main.
//
// CITED BY COMMIT, NOT BY LINE NUMBER (#2102 f4). An earlier version named
// internal/cli/findings.go:365 - a location this branch's tree does not contain,
// so a reviewer in a single-branch checkout cannot check it and a later edit
// silently invalidates it. A commit SHA is checkable from anywhere and cannot
// drift.
//
// THE TEST DID NOT CHANGE, and that is the point worth keeping: it pinned the
// contract while the caller was still someone else's unmerged branch, and the
// contract held when the caller arrived. A non-blocking P3 must not reach that
// sink and be rendered as "degraded:", because it is not a degradation.
func TestANonBlockingP3DoesNotReachACallerDegradedSink(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	oldHead, head := strings.Repeat("a", 40), strings.Repeat("d", 40)
	seedFinding(t, store, 7010, oldHead, "F-1", "P3")

	var notes []string
	scope := LedgerScope{TaskID: "task-7010", Degraded: func(note string) { notes = append(notes, note) }}
	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 7010, head, scope); err != nil {
		t.Fatalf("P3 blocked: %v", err)
	}
	for _, note := range notes {
		if strings.Contains(note, "F-1") {
			t.Fatalf("a non-blocking P3 was reported as a degradation: %q", note)
		}
	}
}

// AND THE OBLIGATION SET ITSELF IS UNCHANGED. The severity split lives at the
// BLOCKING boundary, not in the predicate, so `gitmoot findings` and every other
// reader still sees P3 rows as outstanding. Filtering the predicate would have
// deleted them from the ledger's answer to "what is outstanding", which is a
// different and worse change than the one ruled.
func TestTheObligationSetStillContainsNonBlockingFindings(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	oldHead, head := strings.Repeat("7", 40), strings.Repeat("8", 40)
	seedFinding(t, store, 7007, oldHead, "F-1", "P3")

	rows, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 7007)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	obligations := LedgerObligationsAtHead(ctx, rows, head, LedgerScope{})
	if len(obligations) != 1 {
		t.Fatalf("the predicate dropped the non-blocking finding: got %d obligations, want 1", len(obligations))
	}
	if obligations[0].Severity != "P3" {
		t.Fatalf("obligation severity = %q, want P3", obligations[0].Severity)
	}
}

// AND A LEGACY BLANK ROW BLOCKS END TO END, NOT JUST AT THE PREDICATE.
//
// The predicate test above proves CLASSIFICATION. It does not prove that
// EnsureLedgerObligationsObserved receives such a row from the store and
// refuses on it, which is the property that actually protects the 43 open
// blank-severity obligations in the live ledger.
//
// The gap was real and I first documented it as unreachable, because
// RecordReviewFindingObservation rejects a blank severity. It is reachable:
// db.Store.ExecForTest inserts the row the way the pre-validator writer did.
// A limitation worth documenting is worth one more look for a way around it.
func TestALegacyBlankSeverityRowBlocksThroughTheStore(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	oldHead, head := strings.Repeat("b", 40), strings.Repeat("c", 40)

	if err := store.ExecForTest(ctx,
		`INSERT INTO review_finding_observations
		   (finding_uid, repo, pull_request, head_sha, observer_job, state, severity, round_label, title, evidence_kind)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"gitmoot/gitmoot#7009-f1", "gitmoot/gitmoot", 7009, oldHead, "review-legacy",
		string(db.FindingOpen), "", "F-1", "a finding written before the severity validator", string(db.EvidenceExecuted),
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// The premise: this row could not be written through the current writer.
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 7009, HeadSHA: oldHead, ObserverJob: "review-2",
		State: db.FindingOpen, Severity: "", RoundLabel: "F-2", Title: "rejected",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"x"}, ExecutedCount: 1,
	}); err == nil {
		t.Fatal("premise broken: the writer now accepts a blank severity, so this fixture no longer represents legacy data")
	}

	err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 7009, head, LedgerScope{TaskID: "task-7009"})
	if err == nil {
		t.Fatal("a stored blank-severity obligation did not block; 43 live rows would have been silently retired")
	}
	if !strings.Contains(err.Error(), "F-1") {
		t.Fatalf("the refusal does not name the legacy obligation: %v", err)
	}
}

// A BLOCKING-ONLY REFUSAL REPORTS NOTHING. The guard is `if len(reported) > 0`,
// and the #2102 reviewer showed that replacing it with `if true` compiles and
// passes the ENTIRE internal/workflow suite - no test asserted the absence of a
// spurious event. Production was correct; the coverage was not, and a future
// regression would have emitted "0 finding(s) ... do not hold the merge" on
// every P1/P2 refusal.
//
// That is this campaign's own class one more time: a record asserting something
// it cannot evidence, here a report of an empty set.
func TestABlockingOnlyRefusalEmitsNoReportedEvent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	oldHead, head := strings.Repeat("7", 40), strings.Repeat("e", 40)
	seedFinding(t, store, 7011, oldHead, "F-1", "P1")
	seedFinding(t, store, 7011, oldHead, "F-2", "P2")

	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 7011, head, LedgerScope{TaskID: "task-7011"}); err == nil {
		t.Fatal("premise broken: a P1 and a P2 did not block")
	}

	events, err := store.ListTaskEvents(ctx, "task-7011")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == "findings_ledger_reported_not_blocking" {
			t.Fatalf("a blocking-only refusal emitted a non-blocking report: %q", event.Reason)
		}
	}
}

// THE BRIEF MUST STATE THE RULE THE GATE ENFORCES, NOT A STRICTER ONE.
//
// ledgerObligationBrief is read BEFORE a reviewer writes, so it is the most
// consequential statement of the gate's rule anywhere. It said the gate refuses
// "until every one carries an observation", which stopped being true the moment
// P1/P2 became the blocking set - and NO TEST PINNED THAT SENTENCE, so it would
// have drifted out of truth silently and stayed there.
//
// A brief that overstates the gate teaches reviewers a rule the gate does not
// enforce; the first reviewer to notice learns the brief cannot be trusted,
// which costs more than the sentence saved.
func TestTheObligationBriefStatesTheSeverityRule(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	oldHead, head := strings.Repeat("c", 40), strings.Repeat("f", 40)
	seedFinding(t, store, 7012, oldHead, "F-1", "P3")

	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 7012, head, "task-7012")
	if strings.TrimSpace(brief) == "" {
		t.Fatal("no brief produced, so this test cannot check what it says")
	}
	if !strings.Contains(brief, "every P1 and P2") {
		t.Fatalf("the brief does not name the blocking set, so a reviewer cannot know what holds the merge:\n%s", brief)
	}
	if !strings.Contains(brief, "does not hold the merge") {
		t.Fatalf("the brief does not say a P3 cannot block, which is the rule the gate now enforces:\n%s", brief)
	}
	// AND IT MUST NOT TELL THEM A P3 IS FREE TO IGNORE.
	if !strings.Contains(brief, "still worth") {
		t.Fatalf("the brief stops asking for P3 answers entirely; reported-not-blocking is not the same as retired:\n%s", brief)
	}
}
