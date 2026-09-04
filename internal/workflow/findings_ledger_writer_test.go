package workflow

import (
	"context"
	"database/sql"
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
			// DECLARES EXECUTION EXPLICITLY. This fixture predates main's Evidence
			// field and asserted EXECUTED from tests_run alone, which is exactly
			// the inference the merge-head P1 removed: an absent field is NOT a
			// declaration of execution (directive 117921 item 3). The subject of
			// this test is that the writer records findings at the exact head, so
			// it declares what it means rather than relying on the old inference.
			Evidence: EvidenceExecuted,
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

	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 1850, headB, "task-9")
	if !strings.Contains(brief, uid) {
		t.Fatalf("the brief does not name uid %q, so no reviewer can discharge it: %q", uid, brief)
	}
	if !strings.Contains(brief, "continues_uid") {
		t.Fatal("the brief does not tell the reviewer HOW to continue a prior finding")
	}
	// A PR with no ledger history must produce a byte-identical default brief.
	if empty := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 9999, headB, "task-9"); empty != "" {
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
	if again := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 1850, headB, "task-9"); again != "" {
		t.Fatalf("brief still asks for an observed finding: %q", again)
	}
}

// THE ARM I PUBLISHED AS UNTESTABLE, AND I WAS WRONG. I grepped for
// `type Store interface`, found zero, and concluded no injection point existed.
// The repo's actual idiom is a SQL TRIGGER, used in at least six places
// including merge_gate_test.go in this very package. I measured for a Go seam
// and reported the absence of one as the absence of any, which is the
// convenient-zero class this whole campaign has been about.
//
// THE PROPERTY: a failure to write the closing AUDIT event must NOT fail the
// review advance. Before the fix the writer returned that error and AdvanceJob
// propagated it, so a transient 'database is locked' on an audit row would have
// discarded a REAL VERDICT. SQLITE_BUSY is measured reality on this box
// (directive 117187). Reverting `_ =` to `return` there makes this fail.
func TestAdvanceJobSurvivesAFailedLedgerAuditEvent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("e", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-audit", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 1851, HeadSHA: head,
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "one defect",
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: []json.RawMessage{json.RawMessage(`{"id":"F1","severity":"P1","file":"internal/run.go","title":"a defect"}`)},
		},
	})

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer raw.Close()
	// Abort ONLY the ledger's audit event, so the observation write is untouched
	// and the failure is attributable to the audit path alone.
	if _, err := raw.ExecContext(ctx, `
CREATE TRIGGER fail_findings_ledger_audit_event
BEFORE INSERT ON job_events
WHEN NEW.kind = 'findings_ledger_recorded'
BEGIN
	SELECT RAISE(ABORT, 'forced findings ledger audit failure');
END`); err != nil {
		t.Fatalf("create audit failure trigger: %v", err)
	}

	// POSITIVE CONTROL: prove the trigger fires, or this test pins nothing.
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "review-audit", Kind: "findings_ledger_recorded", Message: "control"}); err == nil {
		t.Fatal("the trigger did not fire, so this test cannot discriminate")
	}

	if err := engine.AdvanceJob(ctx, "review-audit"); err != nil {
		t.Fatalf("a failed AUDIT event failed the whole review advance, discarding a real verdict: %v", err)
	}
	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1851)
	if err != nil {
		t.Fatalf("list observations: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("ledger holds %d observation(s); the finding was lost with the audit row", len(observations))
	}
	if observations[0].HeadSHA != head {
		t.Fatalf("observation head = %q, want %q", observations[0].HeadSHA, head)
	}
}

// #1850 round 2 F1, P1, THE WEDGE. The brief and the gate must compute the SAME
// obligation set. My first version passed LedgerScope{} to the brief while the
// gate passed a populated scope, so the gate demanded two classes the brief
// could not disclose - answered findings re-armed by relevance, and answered
// findings whose STATIC locator had gone - and neither was dischargeable,
// because a uid can only be cited by a reviewer that was told it. The reviewer
// reproduced it end to end through a full round-3 review (ADV-1, ADV-2).
//
// THIS TEST USES THE ANSWERED-PLUS-RELEVANCE CLASS DELIBERATELY, because the
// open case is the one class where the two agreed, which is exactly why my
// earlier brief test passed while the wedge was live.
func TestLedgerBriefSetEqualsGateSet(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	h1, h2, h3 := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	uid, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1850, HeadSHA: h1, ObserverJob: "review-1",
		State: db.FindingOpen, Severity: "P1", RoundLabel: "F-1", Title: "the defect",
		File: "internal/workflow/findings_ledger.go", RelevanceKeys: []string{"internal/workflow"},
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test -> FAIL"}, ExecutedCount: 1,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1850, HeadSHA: h2, ObserverJob: "review-2",
		ContinuesUID: uid, State: db.FindingAnswered, Severity: "P1", Title: "the defect",
		File: "internal/workflow/findings_ledger.go", RelevanceKeys: []string{"internal/workflow"},
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test -> ok"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("answer: %v", err)
	}

	// BOTH RESOLVER HALVES ON BOTH SIDES (#1850 round 3 item 4). The previous
	// version set neither PathExistsAtHead, so it passed while the two sides
	// diverged on exactly the locator-existence class that wedged round 3. Both
	// consumers now take the SAME LedgerResolvers value, so this constructs one.
	resolvers := LedgerResolvers{
		ChangedSince: func(context.Context, string, int, string, string) ([]string, error) {
			return []string{"internal/workflow/findings_ledger.go"}, nil
		},
		PathExistsAtHead: func(context.Context, string, string) (bool, error) { return true, nil },
	}
	engine := testEngine(store)
	engine.LedgerResolvers = resolvers
	gate := PolicyMergeGate{Store: store, LedgerResolvers: resolvers}

	gateErr := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 1850, h3,
		gate.ledgerScope(MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 1850, TaskID: "task-9"}))
	if gateErr == nil {
		t.Fatal("the gate accepted an answered finding whose relevance key the diff touches, so this test cannot discriminate")
	}
	if !strings.Contains(gateErr.Error(), uid) {
		t.Fatalf("gate refusal does not name %s: %v", uid, gateErr)
	}

	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 1850, h3, "task-9")
	if !strings.Contains(brief, uid) {
		t.Fatalf("THE WEDGE via relevance: the gate demands %s and the brief does not disclose it, so no reviewer can ever discharge it.\nbrief=%q", uid, brief)
	}

	// THE LOCATOR-EXISTENCE CLASS, which is how round 3 wedged after round 2 was
	// fixed. A STATIC answer whose cited path has vanished is mandatory again;
	// the brief must say so too.
	staticStore := openEngineStore(t)
	staticUID, err := staticStore.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1851, HeadSHA: h1, ObserverJob: "review-1",
		State: db.FindingOpen, Severity: "P1", RoundLabel: "F-1", Title: "static defect",
		File: "internal/gone.go", EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test -> FAIL"}, ExecutedCount: 1,
	})
	if err != nil {
		t.Fatalf("seed static: %v", err)
	}
	if _, err := staticStore.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1851, HeadSHA: h2, ObserverJob: "review-2",
		ContinuesUID: staticUID, State: db.FindingAnswered, Severity: "P1", Title: "static defect",
		File: "internal/gone.go", EvidenceKind: db.EvidenceStatic,
		EvidenceLocator: "internal/gone.go:9999", Rationale: "the guard lives here now",
	}); err != nil {
		t.Fatalf("answer static: %v", err)
	}
	gone := LedgerResolvers{
		ChangedSince:     func(context.Context, string, int, string, string) ([]string, error) { return nil, nil },
		PathExistsAtHead: func(context.Context, string, string) (bool, error) { return false, nil },
	}
	staticEngine := testEngine(staticStore)
	staticEngine.LedgerResolvers = gone
	staticGate := PolicyMergeGate{Store: staticStore, LedgerResolvers: gone}

	staticGateErr := EnsureLedgerObligationsObserved(ctx, staticStore, "gitmoot/gitmoot", 1851, h3,
		staticGate.ledgerScope(MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 1851, TaskID: "task-9"}))
	if staticGateErr == nil {
		t.Fatal("the gate accepted a STATIC answer whose locator is gone, so this arm cannot discriminate")
	}
	staticBrief := staticEngine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 1851, h3, "task-9")
	if !strings.Contains(staticBrief, staticUID) {
		t.Fatalf("THE WEDGE via locator existence: the gate demands %s and the brief does not disclose it.\nbrief=%q", staticUID, staticBrief)
	}
}

// #1850 round 2 F7, ADV-8. A reviewer whose round was superseded WHILE STILL
// RUNNING found a real defect at a real head. Dropping it was completely
// silent: no ledger row, no event of any kind, and the stale verdict is
// discarded too, so the finding was unrecoverable. This is the case where a
// later round most needs the reminder.
//
// THE SAFETY PROPERTY IS ASSERTED TOO: the stale observation is keyed by the
// STALE round's own head, so it must NOT discharge anything at the current head.
func TestAdvanceJobPreservesASupersededRoundsFindings(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	h1, h2 := strings.Repeat("7", 40), strings.Repeat("8", 40)

	// Round 2 lands first and is the latest round for this PR.
	insertCompletedJob(t, store, db.Job{ID: "review-round2", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 1860, HeadSHA: h2,
		TaskID: "task-9", ReviewRound: "review-2",
		Result: &AgentResult{Decision: "approved", Summary: "clean", TestsRun: []string{"go test -> ok"}},
	})
	// The slow round-1 reviewer then finishes at the OLDER head with a real P1.
	insertCompletedJob(t, store, db.Job{ID: "review-round1", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 1860, HeadSHA: h1,
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "data loss on merge",
			TestsRun: []string{"go test ./internal/ -> FAIL"},
			Findings: []json.RawMessage{json.RawMessage(`{"id":"F1","severity":"P1","file":"internal/run.go","title":"data loss on merge"}`)},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-round1"); err != nil {
		t.Fatalf("AdvanceJob on a superseded round: %v", err)
	}

	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1860)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("ledger holds %d observation(s); a superseded round's REAL P1 was dropped silently", len(observations))
	}
	if observations[0].HeadSHA != h1 {
		t.Fatalf("stale observation recorded at %q, want the stale round's own head %q", observations[0].HeadSHA, h1)
	}
	if countJobEvents(t, store, "review-round1", "findings_ledger_recorded") != 1 {
		t.Fatal("no durable accounting event for the superseded round's findings")
	}
	// SAFETY: keyed by the stale head, it cannot have discharged the current one,
	// so a verdict at h2 is still blocked by the open finding.
	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 1860, h2, LedgerScope{}); err == nil {
		t.Fatal("the stale round's observation discharged the obligation at the CURRENT head")
	}
}
