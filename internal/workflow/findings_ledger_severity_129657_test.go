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

// REPORTED, NOT SILENT. The half that makes the change safe: a P3 that no longer
// blocks must still be stated, or filing one becomes free to ignore and seats
// stop filing. Asserted through the scope's own degradation sink, which is the
// operator-facing channel the ledger already uses.
func TestANonBlockingP3IsStillReported(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	oldHead, head := strings.Repeat("5", 40), strings.Repeat("6", 40)
	seedFinding(t, store, 7006, oldHead, "F-1", "P3")

	var notes []string
	scope := LedgerScope{TaskID: "task-7006", Degraded: func(note string) { notes = append(notes, note) }}
	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 7006, head, scope); err != nil {
		t.Fatalf("P3 blocked: %v", err)
	}
	if len(notes) == 0 {
		t.Fatal("a P3 stopped blocking AND stopped being reported; the next reviewer learns not to file one")
	}
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "F-1") {
		t.Fatalf("the report does not name the finding it is reporting: %q", joined)
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
