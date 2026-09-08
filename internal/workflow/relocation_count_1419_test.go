package workflow

import (
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1419. "Gitmoot counts rounds of work. It does not count how many times the
// same defect came back." Every fix round looks identical to the first - a job,
// a review, a verdict, tests, green CI - so a lane chasing one defect around a
// file is indistinguishable from a lane converging on a fix.
//
// The data was already recorded: review_finding_observations carries repo,
// pull_request, file and round_label. Nothing read it that way. Measured on this
// store, 26 (repo, pr, file) groups carry findings across more than one round,
// and the worst is 16 distinct rounds on one file - the issue's own instance was
// six.

func obs(file, round string) db.ReviewFindingObservation {
	return db.ReviewFindingObservation{File: file, RoundLabel: round}
}

// THE UNIT IS A DISTINCT ROUND, NOT A FINDING COUNT. Three findings in one round
// is a thorough review; one finding in each of three rounds is a defect that
// keeps coming back. Collapsing those would report a careful reviewer as a
// relocation problem, which is the false positive that would get this ignored.
func TestRelocationCountCountsRoundsNotFindings(t *testing.T) {
	thorough := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "F1"),
		obs("internal/cli/a.go", "F1"),
		obs("internal/cli/a.go", "F1"),
		obs("internal/cli/a.go", "F1"),
	}
	if got := ledgerRelocationBrief(thorough); got != "" {
		t.Fatalf("four findings in ONE round were reported as relocations:\n%s", got)
	}

	relocating := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "F1"),
		obs("internal/cli/a.go", "F2"),
		obs("internal/cli/a.go", "F3"),
	}
	got := ledgerRelocationBrief(relocating)
	if got == "" {
		t.Fatal("one finding in each of three rounds was not reported as relocation")
	}
	for _, want := range []string{"DEFECT RELOCATION COUNT", "internal/cli/a.go", "rounds=3", "F1 F2 F3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("brief missing %q:\n%s", want, got)
		}
	}
	// It must say what to do with the number, or it is a statistic rather than a
	// prompt to make a design decision.
	for _, want := range []string{"STATING A CONTRACT", "unbounded set", "OPPOSITE direction", "not a block"} {
		if !strings.Contains(got, want) {
			t.Fatalf("brief reports the count without the judgement it informs (%q):\n%s", want, got)
		}
	}
}

// Below the threshold nothing is said. A count that warns at two would fire on
// almost every PR in this store and be ignored; three is the number the incident
// in #1419 had already agreed and never enforced.
func TestRelocationCountStaysSilentBelowTheThreshold(t *testing.T) {
	two := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "F1"),
		obs("internal/cli/a.go", "F2"),
	}
	if got := ledgerRelocationBrief(two); got != "" {
		t.Fatalf("two rounds triggered a relocation warning:\n%s", got)
	}
}

// Findings are attributed PER FILE. A defect that moves between files is a
// different phenomenon from one relocating inside a single vessel, and the issue
// is about the second: rounds 4, 5 and 6 each arrived inside the previous fix.
func TestRelocationCountAttributesPerFileAndIgnoresFilelessRows(t *testing.T) {
	spread := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "F1"),
		obs("internal/cli/b.go", "F2"),
		obs("internal/cli/c.go", "F3"),
	}
	if got := ledgerRelocationBrief(spread); got != "" {
		t.Fatalf("three rounds across three DIFFERENT files were reported as relocation in one:\n%s", got)
	}

	// A finding with no file cannot be attributed to a vessel, so it cannot
	// evidence relocation within one. Counting it would inflate every file.
	fileless := []db.ReviewFindingObservation{
		obs("", "F1"), obs("", "F2"), obs("", "F3"),
		obs("internal/cli/a.go", "F1"),
		obs("internal/cli/a.go", "F2"),
	}
	if got := ledgerRelocationBrief(fileless); got != "" {
		t.Fatalf("fileless rows pushed a two-round file over the threshold:\n%s", got)
	}
}

// An unlabelled round is a real round and must count, but every unlabelled row
// is ONE round rather than one each - otherwise a file with three legacy rows
// from a single pass reads as three relocations.
func TestRelocationCountTreatsUnlabelledRowsAsOneRound(t *testing.T) {
	legacy := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", ""),
		obs("internal/cli/a.go", ""),
		obs("internal/cli/a.go", ""),
	}
	if got := ledgerRelocationBrief(legacy); got != "" {
		t.Fatalf("three unlabelled rows from one pass were counted as three rounds:\n%s", got)
	}
	mixed := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", ""),
		obs("internal/cli/a.go", "F2"),
		obs("internal/cli/a.go", "F3"),
	}
	got := ledgerRelocationBrief(mixed)
	if got == "" {
		t.Fatal("an unlabelled round plus two labelled ones did not reach the threshold")
	}
	if !strings.Contains(got, "(unlabelled)") {
		t.Fatalf("the unlabelled round is not named, so a reader cannot tell what the third round was:\n%s", got)
	}
}
