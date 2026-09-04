package workflow

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1822 guards. Every one of these drives EnsureLedgerObligationsObserved, which
// is the function PolicyMergeGate.ensureFinalReviewCaptured calls, NOT the
// LedgerObligationsAtHead helper underneath it. That distinction is the point:
// a test that pins a helper while every real caller routes elsewhere is not a
// test of the path, and a routing mutant would leave such a test green.

const (
	headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func ledgerStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func executedObservation(state db.FindingState, head, label string) db.ReviewFindingObservation {
	return db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: head, State: state,
		Severity: "P2", RoundLabel: label, Title: "the guard is inert",
		File: "internal/workflow/run.go", EvidenceKind: db.EvidenceExecuted,
		ExecutedCommands: []string{"go test ./internal/workflow/"}, ExecutedCount: 12,
	}
}

// G1: a prior finding with no observation at the NEW head blocks acceptance, and
// the refusal names the finding. DESIRED: rejected. WITHOUT THE GUARD the round
// would be accepted having never looked at the prior finding.
func TestLedgerRefusesAVerdictThatSkipsAPriorFindingAtANewHead(t *testing.T) {
	ctx := context.Background()
	store := ledgerStore(t)
	uid, err := store.RecordReviewFindingObservation(ctx, executedObservation(db.FindingOpen, headA, "F-1"))
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	err = EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, nil)
	if err == nil {
		t.Fatal("a verdict at a new head with no observation of the open finding was ACCEPTED; the ledger let the round skip verification")
	}
	if !strings.Contains(err.Error(), uid) {
		t.Fatalf("refusal does not name the unobserved finding %q: %v", uid, err)
	}

	// POSITIVE CONTROL: the same round WITH an observation at head B is accepted.
	// A guard that cannot accept a correct round rejects valid input.
	obs := executedObservation(db.FindingAnswered, headB, "F-1")
	obs.ContinuesUID = uid
	if _, err := store.RecordReviewFindingObservation(ctx, obs); err != nil {
		t.Fatalf("record continuation: %v", err)
	}
	if err := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, nil); err != nil {
		t.Fatalf("a round that observed every mandatory finding at head B was refused: %v", err)
	}
}

// G2, THE COLLISION GUARD (#1822 review 116538). Reviewers number findings PER
// ROUND: on #1783, F-1 named four different defects across four rounds. A round
// that reuses the label for an UNRELATED defect must not discharge the prior
// finding. DESIRED: the prior finding is still counted unobserved and the
// verdict is refused. WITHOUT THE FIX the invariant is satisfied by a
// coincidence of naming and this path stays green forever.
func TestLedgerLabelCollisionDoesNotDischargeThePriorFinding(t *testing.T) {
	ctx := context.Background()
	store := ledgerStore(t)
	priorUID, err := store.RecordReviewFindingObservation(ctx, executedObservation(db.FindingOpen, headA, "F-1"))
	if err != nil {
		t.Fatalf("record prior: %v", err)
	}

	// Round 2 at a NEW head, same label F-1, DIFFERENT defect, and crucially no
	// ContinuesUID: the reviewer is naming, not referencing.
	collision := executedObservation(db.FindingOpen, headB, "F-1")
	collision.Title = "an entirely different defect that happens to be numbered F-1"
	collision.File = "internal/cli/daemon_scheduler.go"
	newUID, err := store.RecordReviewFindingObservation(ctx, collision)
	if err != nil {
		t.Fatalf("record collision: %v", err)
	}
	if newUID == priorUID {
		t.Fatalf("a renumbered label reused the prior finding's identity: both are %q", newUID)
	}

	err = EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, nil)
	if err == nil {
		t.Fatal("a label collision DISCHARGED the prior finding: the obligation was satisfied by a coincidence of naming")
	}
	if !strings.Contains(err.Error(), priorUID) {
		t.Fatalf("refusal does not name the still-unobserved prior finding %q: %v", priorUID, err)
	}
}

// G3: citing a continues_uid that does not exist is refused rather than silently
// minting, because minting there turns a typo into a new finding and leaves the
// intended one unobserved.
func TestLedgerRefusesAnUnknownContinuesUID(t *testing.T) {
	ctx := context.Background()
	store := ledgerStore(t)
	obs := executedObservation(db.FindingAnswered, headA, "F-1")
	obs.ContinuesUID = "owner/repo#7-f99"
	if _, err := store.RecordReviewFindingObservation(ctx, obs); !errors.Is(err, db.ErrFindingUnknownContinues) {
		t.Fatalf("unknown continues_uid error = %v, want ErrFindingUnknownContinues", err)
	}
}

// G4: an EXECUTED claim with nothing executed is refused AT THE STORE BOUNDARY.
// This is the direct analogue of the #1824 verdict that reported 15 mutants and
// ran zero, except that a persisted wrong number is read as fact by every later
// round.
func TestLedgerRefusesExecutedEvidenceThatExecutedNothing(t *testing.T) {
	ctx := context.Background()
	store := ledgerStore(t)
	for name, mutate := range map[string]func(*db.ReviewFindingObservation){
		"zero count":  func(o *db.ReviewFindingObservation) { o.ExecutedCount = 0 },
		"no commands": func(o *db.ReviewFindingObservation) { o.ExecutedCommands = nil },
	} {
		obs := executedObservation(db.FindingOpen, headA, "F-1")
		mutate(&obs)
		if _, err := store.RecordReviewFindingObservation(ctx, obs); !errors.Is(err, db.ErrFindingEvidence) {
			t.Fatalf("%s: error = %v, want ErrFindingEvidence", name, err)
		}
	}
}

// G5 (#1822 review 116564 condition 2): a DISCHARGING observation must carry
// evidence. Without this a round facing 27 obligations submits 27 evidence-free
// observations and is accepted, which is delta sign-off arriving as cheap
// discharge rather than as skipping.
func TestLedgerRefusesAnEvidenceFreeDischarge(t *testing.T) {
	ctx := context.Background()
	store := ledgerStore(t)

	staticNoRationale := db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: headA, State: db.FindingOpen,
		EvidenceKind: db.EvidenceStatic, EvidenceLocator: "internal/workflow/run.go:1977",
	}
	if _, err := store.RecordReviewFindingObservation(ctx, staticNoRationale); !errors.Is(err, db.ErrFindingDischarge) {
		t.Fatalf("STATIC with no rationale = %v, want ErrFindingDischarge", err)
	}
	staticNoLocator := staticNoRationale
	staticNoLocator.EvidenceLocator = ""
	staticNoLocator.Rationale = "read it"
	if _, err := store.RecordReviewFindingObservation(ctx, staticNoLocator); !errors.Is(err, db.ErrFindingDischarge) {
		t.Fatalf("STATIC with no locator = %v, want ErrFindingDischarge", err)
	}

	// QUOTED establishes nothing, so it cannot discharge.
	quoted := db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: headA, State: db.FindingAnswered,
		EvidenceKind: db.EvidenceQuoted, SourceJob: "local-implement-1",
	}
	if _, err := store.RecordReviewFindingObservation(ctx, quoted); !errors.Is(err, db.ErrFindingQuotedDischarge) {
		t.Fatalf("QUOTED discharge = %v, want ErrFindingQuotedDischarge", err)
	}

	// POSITIVE CONTROLS: a STATIC discharge WITH locator and rationale is
	// accepted, and so is an EXECUTED one. Every bound added in this lane
	// recently had a version that rejected valid input.
	good := staticNoRationale
	good.Rationale = "re-read the guard at the current head and the property holds"
	if _, err := store.RecordReviewFindingObservation(ctx, good); err != nil {
		t.Fatalf("a STATIC discharge with a locator and a rationale was refused: %v", err)
	}
	if _, err := store.RecordReviewFindingObservation(ctx, executedObservation(db.FindingOpen, headB, "F-2")); err != nil {
		t.Fatalf("an EXECUTED discharge with a real command and a non-zero count was refused: %v", err)
	}
}

// The remaining positive controls for the acceptance function itself, so no
// guard can pass by refusing everything.
func TestLedgerAcceptsWhenItHasNothingToSay(t *testing.T) {
	ctx := context.Background()
	store := ledgerStore(t)

	if err := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, nil); err != nil {
		t.Fatalf("an EMPTY ledger refused a verdict: %v", err)
	}
	if err := EnsureLedgerObligationsObserved(ctx, nil, "owner/repo", 7, headB, nil); err != nil {
		t.Fatalf("a nil store refused a verdict: %v", err)
	}

	// A withdrawn finding is never mandatory.
	withdrawn := executedObservation(db.FindingWithdrawn, headA, "F-9")
	withdrawn.WithdrawReason = "not a defect: the premise was wrong"
	if _, err := store.RecordReviewFindingObservation(ctx, withdrawn); err != nil {
		t.Fatalf("record withdrawn: %v", err)
	}
	if err := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, nil); err != nil {
		t.Fatalf("a withdrawn finding created an obligation: %v", err)
	}

	// An ANSWERED finding whose relevance keys the diff does NOT touch is
	// advisory, so it does not block. This is the bound that stops a late round
	// inheriting dozens of mandatory observations for defects nobody touched.
	answered := executedObservation(db.FindingAnswered, headA, "F-8")
	answered.RelevanceKeys = []string{"internal/workflow/run.go", "AGENTS.md"}
	if _, err := store.RecordReviewFindingObservation(ctx, answered); err != nil {
		t.Fatalf("record answered: %v", err)
	}
	if err := EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, []string{"internal/cli/unrelated.go"}); err != nil {
		t.Fatalf("an answered finding untouched by the diff blocked acceptance: %v", err)
	}
}

// RELEVANCE IS WIDER THAN THE NAMED FILE (#1822 review 116564 condition 1). On
// #1783 a finding in a run.go comment was answered by a change that landed
// partly in AGENTS.md, so a file-scoped bound would call it advisory when a
// later AGENTS.md change re-breaks it. DESIRED: a diff touching a relevance key
// the finding named, but NOT its own file, makes it mandatory again.
func TestLedgerRelevanceReachesBeyondTheNamedFile(t *testing.T) {
	ctx := context.Background()
	store := ledgerStore(t)
	answered := executedObservation(db.FindingAnswered, headA, "F-1")
	answered.File = "internal/workflow/run.go"
	answered.RelevanceKeys = []string{"AGENTS.md"}
	uid, err := store.RecordReviewFindingObservation(ctx, answered)
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	// The diff touches AGENTS.md and NOT run.go.
	err = EnsureLedgerObligationsObserved(ctx, store, "owner/repo", 7, headB, []string{"AGENTS.md"})
	if err == nil {
		t.Fatal("a diff touching a declared relevance key left the answered finding advisory: relevance is still file-scoped")
	}
	if !strings.Contains(err.Error(), uid) {
		t.Fatalf("refusal does not name %q: %v", uid, err)
	}
}
