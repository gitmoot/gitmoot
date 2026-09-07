package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordReviewFindingObservationResolvesARepoUnqualifiedContinuesUID is
// #1965, reproduced through the production write path.
//
// MEASURED DEFECT: job local-review-g7-review-18d2fbd1b3e82f2c reviewed
// gitmoot/gitmoot#1950 at head 10701afae36347c3126c3a9b29048332cb267054 for
// 47m50s, returned changes_requested with 6 findings and 13 tests_run, and
// contributed NOTHING to the ledger. Its brief spelled the citations "#1950-f1"
// rather than "gitmoot/gitmoot#1950-f1", so the store refused all six and the
// engine recorded "recorded 0 of 6 ... (6 skipped)". The adjacent round, whose
// brief used the qualified form, recorded 6 of 6. Same reviewer, same agent,
// same repo, adjacent heads: the only variable was the spelling.
//
// The failure is also discovered AFTER the reviewer finishes, and the only
// signal is six findings_ledger_skipped job events, so a coordinator reading the
// verdict publishes an inherited-observation count against an empty record.
func TestRecordReviewFindingObservationResolvesARepoUnqualifiedContinuesUID(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	head := strings.Repeat("a", 40)
	observation := func(repo string, pr int64, observer string, continues string) ReviewFindingObservation {
		return ReviewFindingObservation{
			Repo: repo, PullRequest: pr, HeadSHA: head, ObserverJob: observer,
			State: FindingOpen, Severity: "P1", Title: "t", Detail: "d",
			File: "internal/workflow/merge_gate.go", EvidenceKind: EvidenceExecuted,
			ExecutedCommands: []string{"go test ./internal/workflow/"}, ExecutedCount: 1,
			ContinuesUID: continues,
		}
	}

	minted, err := store.RecordReviewFindingObservation(ctx, observation("gitmoot/gitmoot", 1950, "round-9", ""))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if minted != "gitmoot/gitmoot#1950-f1" {
		t.Fatalf("minted uid = %q; the abbreviation this test is about is defined relative to that shape", minted)
	}

	// THE REGRESSION: the round-10 spelling.
	got, err := store.RecordReviewFindingObservation(ctx, observation("gitmoot/gitmoot", 1950, "round-10", "#1950-f1"))
	if err != nil {
		t.Fatalf("continuing %q: %v", "#1950-f1", err)
	}
	if got != minted {
		t.Fatalf("short form resolved to %q, want the canonical %q; a continuation that mints a second uid leaves the first permanently unobserved", got, minted)
	}

	// The fully qualified form is unchanged.
	qualified, err := store.RecordReviewFindingObservation(ctx, observation("gitmoot/gitmoot", 1950, "round-11", minted))
	if err != nil {
		t.Fatalf("continuing the qualified uid: %v", err)
	}
	if qualified != minted {
		t.Fatalf("qualified continuation resolved to %q, want %q", qualified, minted)
	}

	// THE REFUSAL IS UNCHANGED, which is the property the issue explicitly asks
	// not to weaken: an abbreviation naming nothing is still refused, and the
	// message now names every spelling the store looked for so the corrective
	// form is in the error rather than in someone's memory.
	unknown, err := store.RecordReviewFindingObservation(ctx, observation("gitmoot/gitmoot", 1950, "round-12", "#1950-f9"))
	if !errors.Is(err, ErrFindingUnknownContinues) {
		t.Fatalf("an abbreviation naming nothing was accepted: uid=%q err=%v", unknown, err)
	}
	if !strings.Contains(err.Error(), `"gitmoot/gitmoot#1950-f9"`) {
		t.Fatalf("refusal does not name the qualified form it derived, so the caller cannot correct it: %v", err)
	}

	// THE SCOPE CONTROL, and the one a careless normalisation breaks: the
	// abbreviation is qualified against THIS observation's own repo, never
	// searched across repos. A row in another repo citing "#1950-f1" must not
	// silently continue gitmoot/gitmoot's finding.
	crossRepo, err := store.RecordReviewFindingObservation(ctx, observation("other/repo", 1950, "round-13", "#1950-f1"))
	if !errors.Is(err, ErrFindingUnknownContinues) {
		t.Fatalf("an abbreviation resolved across repositories: uid=%q err=%v", crossRepo, err)
	}
	if !strings.Contains(err.Error(), `"other/repo#1950-f1"`) {
		t.Fatalf("cross-repo refusal does not show which repo it qualified against: %v", err)
	}
}
