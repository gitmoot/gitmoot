package workflow

import (
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2086 f5: MULTI-UID OUTPUT ORDER WAS UNPINNED.
//
// The reviewer verified by reading that LatestObservationsInOrder never ranges
// the `latest` map for output order - it uses the map only for keyed lookups and
// emits in input-slice order, and ListReviewFindingObservations returns
// ORDER BY rowid. So the behaviour was already correct and deterministic; what
// was missing was a test that would notice if someone made it map-ordered.
//
// That is the whole risk this pins. Ranging a Go map is RANDOMISED per run, so a
// refactor to `for uid := range latest` would pass every existing test - both
// prior tests use a single UID, where order is unobservable - and produce a
// listing whose row order changed on every invocation.
func TestLatestObservationsInOrderKeepsFirstAppearanceOrderAcrossUIDs(t *testing.T) {
	// Interleaved on purpose: if the fold emitted grouped-by-UID or map order,
	// f2's later observation would not sit in f2's original position.
	input := []db.ReviewFindingObservation{
		{FindingUID: "u-f1", State: db.FindingOpen, EvidenceKind: db.EvidenceExecuted},
		{FindingUID: "u-f2", State: db.FindingOpen, EvidenceKind: db.EvidenceExecuted},
		{FindingUID: "u-f3", State: db.FindingOpen, EvidenceKind: db.EvidenceExecuted},
		{FindingUID: "u-f2", State: db.FindingAnswered, EvidenceKind: db.EvidenceExecuted},
		{FindingUID: "u-f4", State: db.FindingOpen, EvidenceKind: db.EvidenceExecuted},
		{FindingUID: "u-f1", State: db.FindingAnswered, EvidenceKind: db.EvidenceExecuted},
	}
	want := []string{"u-f1", "u-f2", "u-f3", "u-f4"}

	// Repeated because map-range order is randomised PER RANGE, not per process:
	// a single pass could match by luck, and one lucky pass is exactly how an
	// ordering regression survives a suite.
	for attempt := range 32 {
		folded := LatestObservationsInOrder(input)
		if len(folded) != len(want) {
			t.Fatalf("attempt %d: folded %d rows, want %d", attempt, len(folded), len(want))
		}
		for i, uid := range want {
			if folded[i].FindingUID != uid {
				t.Fatalf("attempt %d: row %d is %q, want %q; output order must follow first appearance "+
					"in the input, not map iteration", attempt, i, folded[i].FindingUID, uid)
			}
		}
	}

	// Each UID must still carry its LATEST state, so the ordering guarantee is
	// not bought by returning the first observation instead of the last.
	folded := LatestObservationsInOrder(input)
	if folded[0].State != db.FindingAnswered || folded[1].State != db.FindingAnswered {
		t.Fatalf("first-appearance ORDER must not cost latest-observation STATE: got %q and %q",
			folded[0].State, folded[1].State)
	}
	if folded[2].State != db.FindingOpen {
		t.Fatalf("a single-observation finding changed state: %q", folded[2].State)
	}
}
