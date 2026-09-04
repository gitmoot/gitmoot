package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1850 REMEDIATION GUARDS. One per finding the two verdicts raised. Each is
// written so that reverting its fix makes THIS test fail by name, and each
// refusal is asserted alongside its PASSING case, because every bound added to
// this repo lately has had a version that rejected valid input.

func seedOpenFinding(t *testing.T, store *db.Store, head string) string {
	t.Helper()
	uid, err := store.RecordReviewFindingObservation(context.Background(), db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: head, ObserverJob: "review-1",
		State: db.FindingOpen, Severity: "P1", RoundLabel: "F-1", Title: "the defect",
		File: "internal/run.go", EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test ./internal/ -> FAIL"}, ExecutedCount: 1,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return uid
}

// F3, P1, raised by both verdicts. A QUOTED row establishes nothing, so it must
// never become the folded state of a finding that has a real observation and it
// must never clear an obligation. Before the fix one evidence-free QUOTED
// continuation silenced an OPEN finding at every later head.
func TestLedgerQuotedObservationCannotSilenceAnOpenFinding(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	headA, headB, headC := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	uid := seedOpenFinding(t, store, headA)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: headB, ObserverJob: "review-2",
		ContinuesUID: uid, State: db.FindingOpen, Title: "the defect", File: "internal/run.go",
		EvidenceKind: db.EvidenceQuoted, SourceJob: "review-1",
	}); err != nil {
		t.Fatalf("a quoted row must stay recordable for context: %v", err)
	}
	for _, head := range []string{headB, headC} {
		err := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, head, LedgerScope{})
		if err == nil {
			t.Fatalf("a QUOTED observation discharged an open finding at %s", head[:6])
		}
		if !strings.Contains(err.Error(), uid) {
			t.Fatalf("refusal at %s does not name %s: %v", head[:6], uid, err)
		}
	}
}

// The passing half of F3: real evidence at the head DOES discharge.
func TestLedgerExecutedObservationDischargesAtThatHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	headA, headB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	uid := seedOpenFinding(t, store, headA)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: headB, ObserverJob: "review-2",
		ContinuesUID: uid, State: db.FindingAnswered, Title: "the defect", File: "internal/run.go",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./internal/ -> ok"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("record answer: %v", err)
	}
	if err := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, LedgerScope{}); err != nil {
		t.Fatalf("an EXECUTED observation at the head must discharge: %v", err)
	}
}

// F2, P1, raised by both verdicts. The mint was a COUNT followed by a separate
// INSERT, so concurrent observations of DISTINCT defects were issued one uid and
// 8 of 12 real findings became permanently unobservable.
func TestLedgerConcurrentMintsIssueDistinctUIDs(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	const n = 12
	uids := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			// Heads must be 40 HEX characters; 'a'+11 is 'l', which the store
			// correctly refuses. Indexing the hex alphabet keeps 12 distinct heads.
			head := strings.Repeat("0123456789abcdef"[i:i+1], 40)
			uid, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
				Repo: "owner/repo", PullRequest: 7, HeadSHA: head, ObserverJob: "review-x",
				State: db.FindingOpen, Severity: "P2", Title: "distinct defect", File: "internal/run.go",
				EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"probe"}, ExecutedCount: 1,
			})
			if err != nil {
				errs <- err
				return
			}
			uids <- uid
		}(i)
	}
	seen := map[string]bool{}
	minted := 0
	for i := 0; i < n; i++ {
		select {
		case uid := <-uids:
			if seen[uid] {
				t.Fatalf("uid %q was minted twice: two distinct defects share one identity", uid)
			}
			seen[uid] = true
			minted++
		case err := <-errs:
			// A LOUD collision refusal is acceptable; a silent merge is not.
			if !strings.Contains(err.Error(), "mint collision") && !strings.Contains(err.Error(), "locked") {
				t.Fatalf("concurrent mint failed for an unexpected reason: %v", err)
			}
		}
	}
	if minted == 0 {
		t.Fatal("no uid was minted, so this test proved nothing")
	}
}

// F5, P2. A STATIC discharge citing a path that no longer exists at the head is
// not an answer any more. The structural check lives at the store boundary,
// which has no tree; this is the existence half that was absent entirely.
func TestLedgerStaticDischargeIsReArmedWhenItsLocatorIsGone(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	headA, headB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	uid := seedOpenFinding(t, store, headA)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: headA, ObserverJob: "review-2",
		ContinuesUID: uid, State: db.FindingAnswered, Title: "the defect", File: "internal/run.go",
		EvidenceKind: db.EvidenceStatic, EvidenceLocator: "internal/gone.go:9999",
		Rationale: "the guard now lives here",
	}); err != nil {
		t.Fatalf("record static answer: %v", err)
	}
	gone := LedgerScope{PathExistsAtHead: func(context.Context, string, string) (bool, error) { return false, nil }}
	err := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, gone)
	if err == nil {
		t.Fatal("a STATIC answer citing a deleted path still counted as an answer")
	}
	if !strings.Contains(err.Error(), "does not exist at this head") {
		t.Fatalf("refusal does not name the missing locator: %v", err)
	}
	present := LedgerScope{PathExistsAtHead: func(context.Context, string, string) (bool, error) { return true, nil }}
	if err := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, present); err != nil {
		t.Fatalf("an answer whose locator still exists must stand: %v", err)
	}
}

// F7, P2, both verdicts. Matching is anchored: exact path or directory prefix.
// The unanchored Contains arm made key "db" fire on "docs/adb/unrelated.md".
func TestLedgerRelevanceMatchingIsAnchored(t *testing.T) {
	if key, touched := relevanceTouched([]string{"db"}, []string{"docs/adb/unrelated.md"}); touched {
		t.Fatalf("unanchored substring match survived: key %q matched an unrelated path", key)
	}
	if _, touched := relevanceTouched([]string{"internal/db"}, []string{"internal/db/review_findings.go"}); !touched {
		t.Fatal("a directory-prefix key must still match a file inside it")
	}
	if _, touched := relevanceTouched([]string{"internal/run.go"}, []string{"internal/run.go"}); !touched {
		t.Fatal("an exact path key must match itself")
	}
}

// F7's write half: a symbol key can never match a path, so storing one gives a
// reviewer no protection AND no warning. Refusal is the loud version.
func TestLedgerRefusesASymbolRelevanceKey(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	_, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: strings.Repeat("a", 40), ObserverJob: "review-1",
		State: db.FindingOpen, Title: "t", File: "internal/run.go",
		RelevanceKeys: []string{"EnsureLedgerObligationsObserved()"},
		EvidenceKind:  db.EvidenceExecuted, ExecutedCommands: []string{"probe"}, ExecutedCount: 1,
	})
	if !errors.Is(err, db.ErrFindingRelevanceKey) {
		t.Fatalf("a key the matcher can never match was accepted: %v", err)
	}
}

// Second verdict B, P3. Withdrawal is the only state relevance never re-arms,
// which makes it the cheapest permanent exit from the invariant. Design
// revision 116549 required a recorded reason and the store never checked it.
func TestLedgerRefusesAReasonlessWithdrawal(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	_, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: strings.Repeat("a", 40), ObserverJob: "review-1",
		State: db.FindingWithdrawn, Title: "t", File: "internal/run.go",
		EvidenceKind: db.EvidenceStatic, EvidenceLocator: "a", Rationale: "n",
	})
	if !errors.Is(err, db.ErrFindingWithdrawReason) {
		t.Fatalf("a reasonless withdrawal cleared the ledger: %v", err)
	}
}

// Second verdict C, P3. observed_at decided the fold and was unvalidated, so a
// round could pin its own row as latest forever with a future timestamp. The
// fold now orders by rowid, which only the database assigns.
func TestLedgerFoldIgnoresACallerSuppliedFutureTimestamp(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	headA, headB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	uid := seedOpenFinding(t, store, headA)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: headA, ObserverJob: "review-2",
		ContinuesUID: uid, State: db.FindingAnswered, Title: "t", File: "internal/run.go",
		ObservedAt:   "9999-01-01T00:00:00Z",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test -> ok"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("a valid RFC3339 stamp must be accepted: %v", err)
	}
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: headA, ObserverJob: "review-3",
		ContinuesUID: uid, State: db.FindingOpen, Title: "t", File: "internal/run.go",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test -> FAIL"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("record reopen: %v", err)
	}
	if err := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, LedgerScope{}); err == nil {
		t.Fatal("the future-stamped answer won the fold, so a reopened finding read as answered")
	}
	_, bad := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: headB, ObserverJob: "review-4",
		State: db.FindingOpen, Title: "t", File: "internal/run.go", ObservedAt: "not-a-time",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"probe"}, ExecutedCount: 1,
	})
	if !errors.Is(bad, db.ErrFindingObservedAt) {
		t.Fatalf("a junk timestamp was accepted: %v", bad)
	}
}

// F8, P2. Two DIFFERENT reviewers observing one finding at one head is
// legitimate, and this very PR had three reviewer seats dispatched at one head.
// The old key dropped the dissenting second opinion with a raw driver string.
func TestLedgerAdmitsASecondReviewerAtTheSameHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("a", 40)
	uid := seedOpenFinding(t, store, head)
	second := db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: head, ObserverJob: "reviewer-b",
		ContinuesUID: uid, State: db.FindingOpen, Title: "still broken", File: "internal/run.go",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test -> FAIL"}, ExecutedCount: 1,
	}
	if _, err := store.RecordReviewFindingObservation(ctx, second); err != nil {
		t.Fatalf("a second reviewer at the same head was rejected: %v", err)
	}
	if _, err := store.RecordReviewFindingObservation(ctx, second); !errors.Is(err, db.ErrFindingDuplicateObservation) {
		t.Fatalf("a repeat by ONE observer returned %v, want the named sentinel", err)
	}
}

// M3's pin. The fold guard's real job is NOT the open case (a quoted row is
// forced to state open, so an open finding stays mandatory either way) - it is
// stopping a QUOTED mention from OVERWRITING AN ANSWERED finding's state and
// silently re-arming it. That direction is a valid-input rejection: a reviewer
// quoting a prior answered finding for context must not thereby reopen it.
// Without the guard the fold takes the quoted row, reads "still open", and the
// answered finding becomes mandatory again for no reason.
func TestLedgerQuotedMentionDoesNotReopenAnAnsweredFinding(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	headA, headB, headC := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	uid := seedOpenFinding(t, store, headA)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: headA, ObserverJob: "review-2",
		ContinuesUID: uid, State: db.FindingAnswered, Title: "the defect", File: "internal/run.go",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test -> ok"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("answer the finding: %v", err)
	}
	// A later round merely QUOTES it for context. Nothing was re-checked and
	// nothing was re-broken.
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: headB, ObserverJob: "review-3",
		ContinuesUID: uid, State: db.FindingOpen, Title: "the defect", File: "internal/run.go",
		EvidenceKind: db.EvidenceQuoted, SourceJob: "review-2",
	}); err != nil {
		t.Fatalf("quote for context: %v", err)
	}
	// No diff is supplied, so relevance cannot re-arm it either: the answered
	// state must survive the quote and the verdict at a fresh head must pass.
	if err := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headC, LedgerScope{}); err != nil {
		t.Fatalf("a quoted mention reopened an answered finding: %v", err)
	}
}

// #1850 round 2 F6, ADV-6, WITH ITS FIVE-SYMBOL CONTROL SET rather than the one
// parenthesised fixture. My previous guard's pattern accepted every bare Go
// identifier: the test passed because its fixture carried "()" and died on
// punctuation, not on symbol-ness. The reviewer built that as the fourteenth
// mutant. This is the same test re-fixtured on the shape it names.
func TestLedgerRefusesSymbolKeysAndAcceptsRealPaths(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("a", 40)
	record := func(key string) error {
		_, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
			Repo: "owner/repo", PullRequest: 7, HeadSHA: head, ObserverJob: "review-" + key,
			State: db.FindingOpen, Title: "t", File: "internal/run.go",
			RelevanceKeys: []string{key},
			EvidenceKind:  db.EvidenceExecuted, ExecutedCommands: []string{"probe"}, ExecutedCount: 1,
		})
		return err
	}
	// THE FIVE SYMBOLS FROM ADV-6, none parenthesised.
	for _, symbol := range []string{
		"EnsureLedgerObligationsObserved",
		"LedgerScope",
		"AdvanceJob",
		"RecordReviewFindingObservation",
		"relevanceTouched",
	} {
		if err := record(symbol); !errors.Is(err, db.ErrFindingRelevanceKey) {
			t.Fatalf("bare symbol key %q was accepted (err=%v); it can never match a changed path", symbol, err)
		}
	}
	// A Go package path can never head a repo-relative diff path either.
	if err := record("github.com/gitmoot/gitmoot/internal/db"); !errors.Is(err, db.ErrFindingRelevanceKey) {
		t.Fatalf("a Go package path was accepted: %v", err)
	}
	// AND THE PASSING CONTROL SET, so the guard cannot be satisfied by refusing
	// everything: each of these is a real repo path shape.
	for _, path := range []string{"internal/run.go", "internal/db", "AGENTS.md", "docs/"} {
		if err := record(path); err != nil {
			t.Fatalf("legitimate path key %q was refused: %v", path, err)
		}
	}
}

// #1850 round 2 F5, ADV-3. The repo's documented per-finding convention is
// "path:line", and a reviewer using it in the `file` field had its ENTIRE
// observation refused, dropping a P1 from the ledger. A derived key is
// sanitised; a key the REVIEWER asserted is still refused.
func TestLedgerAcceptsTheDocumentedFileLineConvention(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("b", 40)
	uid, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: head, ObserverJob: "review-1",
		State: db.FindingOpen, Severity: "P1", Title: "the dropped P1",
		File:         "internal/workflow/merge_gate.go:800",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"probe"}, ExecutedCount: 1,
	})
	if err != nil {
		t.Fatalf("the documented path:line convention was refused: %v", err)
	}
	// The derived key must be the PATH, so relevance can actually match a diff.
	observations, err := store.ListReviewFindingObservations(ctx, "owner/repo", 7)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var keys []string
	for _, obs := range observations {
		if obs.FindingUID == uid {
			keys = obs.RelevanceKeys
		}
	}
	if _, touched := relevanceTouched(keys, []string{"internal/workflow/merge_gate.go"}); !touched {
		t.Fatalf("relevance keys %v do not match the finding's own file, so the colon was seeded verbatim", keys)
	}
}
