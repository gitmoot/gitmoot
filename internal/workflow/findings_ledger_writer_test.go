package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1850 F1, P1, THE HEADLINE FINDING AND THE ONE THAT MATTERED MOST: the ledger
// had NO PRODUCTION WRITER, so the schema, the store's evidence bar and the
// gate's acceptance check were all present, all tested, and all unreachable.
// ListReviewFindingObservations always returned empty in production, so
// EnsureLedgerObligationsObserved always took its empty-ledger early return.
//
// THIS TEST ENTERS THROUGH AdvanceJob, the production advance path, not through
// the writer helper. That distinction is the whole lesson of F6: my previous
// guards pinned functions while the call sites stayed unpinned, so the routing
// mutant that mattered survived. Break the writer call in AdvanceJob and this
// fails by name.
func TestAdvanceJobRecordsReviewFindingsToTheLedger(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("d", 40)

	findings := []json.RawMessage{
		json.RawMessage(`{"id":"F1","severity":"P1","file":"internal/db/review_findings.go","line":115,"title":"no production writer","detail":"the ledger is inert"}`),
		json.RawMessage(`{"id":"F2","severity":"P2","file":"internal/workflow/merge_gate.go","title":"nil change set"}`),
	}
	insertCompletedJob(t, store, db.Job{ID: "review-1", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 1850, HeadSHA: head,
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "two defects",
			TestsRun: []string{"go build ./internal/... -> rc=0", "go test ./internal/workflow/ -> ok"},
			Findings: findings,
		},
	})

	if err := engine.AdvanceJob(ctx, "review-1"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1850)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(observations) != 2 {
		t.Fatalf("ledger holds %d observation(s) after a review reporting 2 findings; the production writer is not on the advance path", len(observations))
	}
	for _, obs := range observations {
		if obs.HeadSHA != head {
			t.Fatalf("observation recorded at %q, want the review's exact head %q", obs.HeadSHA, head)
		}
		if obs.State != db.FindingOpen {
			t.Fatalf("a REPORTED finding was recorded as %q; silence is never an answer and neither is a report", obs.State)
		}
		if obs.EvidenceKind != db.EvidenceExecuted {
			t.Fatalf("evidence kind = %q, want EXECUTED from the review's own tests_run", obs.EvidenceKind)
		}
		if obs.ExecutedCount != 2 {
			t.Fatalf("executed count = %d, want 2, the number of checks the verdict itself listed", obs.ExecutedCount)
		}
		if obs.ObserverJob != "review-1" {
			t.Fatalf("observer job = %q, want the reviewing job", obs.ObserverJob)
		}
	}
	// ROUND LABELS ARE PRESERVED FOR HUMANS AND NEVER USED AS IDENTITY: the uids
	// must be distinct even though the reviewer numbered its findings F1 and F2.
	if observations[0].FindingUID == observations[1].FindingUID {
		t.Fatal("two distinct findings share one uid")
	}
	if countJobEvents(t, store, "review-1", "findings_ledger_recorded") != 1 {
		t.Fatal("the writer recorded no audit event, so a silent skip would be indistinguishable from success")
	}
}

// The loop's other half. A written ledger with no disclosure would turn the
// inert feature into a guard that blocks legitimate merges: an obligation is
// dischargeable ONLY by citing a uid, and a uid is obtainable ONLY by being
// told it. So the brief must name every uid a round must observe.
func TestLedgerObligationBriefNamesEveryUIDAReviewerMustCite(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	headA, headB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	uid, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1850, HeadSHA: headA, ObserverJob: "review-1",
		State: db.FindingOpen, Severity: "P1", RoundLabel: "F-1", Title: "unfixed defect",
		File: "internal/run.go", EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test -> FAIL"}, ExecutedCount: 1,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 1850, headB)
	if !strings.Contains(brief, uid) {
		t.Fatalf("the brief does not name uid %q, so no reviewer can discharge it: %q", uid, brief)
	}
	if !strings.Contains(brief, "continues_uid") {
		t.Fatal("the brief does not tell the reviewer HOW to continue a prior finding")
	}
	// A PR with no ledger history must produce a byte-identical default brief.
	if empty := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 9999, headB); empty != "" {
		t.Fatalf("brief for a PR with no ledger = %q, want empty", empty)
	}
	// And once observed here, the brief stops asking.
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1850, HeadSHA: headB, ObserverJob: "review-2",
		ContinuesUID: uid, State: db.FindingAnswered, Title: "unfixed defect", File: "internal/run.go",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test -> ok"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if again := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 1850, headB); again != "" {
		t.Fatalf("brief still asks for an observed finding: %q", again)
	}
}
