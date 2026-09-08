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
// pull_request, file and observer_job. Nothing read it that way.
//
// THE ROUND IS THE OBSERVING JOB, NOT THE REVIEWER'S LABEL (#2066 review). The
// first version of this keyed on RoundLabel, which review_findings.go:20-26
// forbids in terms - reviewers restart numbering at 1 each round, so the label
// "is NEVER used for matching by any consumer". Measured on 675 rows it was
// wrong in both directions: 52 of 239 (pr, file, label) groups spanned more than
// one observing job, and one file on #1930 showed 16 labels across 6 real jobs
// because a single lens run emitted 14 of them.

func obs(file, job, round string) db.ReviewFindingObservation {
	return db.ReviewFindingObservation{File: file, ObserverJob: job, RoundLabel: round}
}

// THE UNIT IS A DISTINCT OBSERVING JOB, NOT A FINDING COUNT. Several findings in
// one round is a thorough review; one finding in each of three rounds is a
// defect that keeps coming back. Collapsing those would report a careful
// reviewer as a relocation problem, which is the false positive that would get
// this ignored on sight.
func TestRelocationCountCountsRoundsNotFindings(t *testing.T) {
	thorough := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-1", "F2"),
		obs("internal/cli/a.go", "job-1", "F3"),
		obs("internal/cli/a.go", "job-1", "F4"),
	}
	if got := ledgerRelocationBrief(thorough); got != "" {
		t.Fatalf("four findings in ONE round were reported as relocations:\n%s", got)
	}

	relocating := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F1"),
		obs("internal/cli/a.go", "job-3", "F1"),
	}
	got := ledgerRelocationBrief(relocating)
	if got == "" {
		t.Fatal("three rounds that all labelled their finding F1 were not counted as three")
	}
	for _, want := range []string{"DEFECT RELOCATION COUNT", "internal/cli/a.go", "rounds=3"} {
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

// THE REGRESSION FOR THE #2066 FINDING, both directions in one test.
//
// Deflation: a reviewer restarting at F1 each round is the DOCUMENTED norm, so a
// label-keyed count reads three rounds as one and stays silent on exactly the
// case the brief exists to surface. Inflation: one round emitting many labels -
// a lens run - must stay one round.
func TestRelocationCountIgnoresLabelsWhenCountingRounds(t *testing.T) {
	repeatedLabel := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F1"),
		obs("internal/cli/a.go", "job-3", "F1"),
	}
	got := ledgerRelocationBrief(repeatedLabel)
	if !strings.Contains(got, "rounds=3") {
		t.Fatalf("one label reused across three jobs must count as three rounds:\n%s", got)
	}

	oneLensRun := []db.ReviewFindingObservation{
		obs("internal/cli/b.go", "job-9", "L01"),
		obs("internal/cli/b.go", "job-9", "L03"),
		obs("internal/cli/b.go", "job-9", "L05"),
		obs("internal/cli/b.go", "job-9", "L07"),
		obs("internal/cli/b.go", "job-9", "F1"),
	}
	if got := ledgerRelocationBrief(oneLensRun); got != "" {
		t.Fatalf("five labels from ONE job were counted as five rounds:\n%s", got)
	}
}

// The labels are still shown, because a human needs to recognise which rounds
// are meant - review_findings.go reserves the label for exactly that. Shown and
// counted must stay different things: here three rounds share one label.
func TestRelocationCountShowsLabelsWithoutCountingThem(t *testing.T) {
	got := ledgerRelocationBrief([]db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F1"),
		obs("internal/cli/a.go", "job-3", "F2"),
	})
	if !strings.Contains(got, "rounds=3") {
		t.Fatalf("count must come from jobs:\n%s", got)
	}
	if !strings.Contains(got, "labels F1 F2") {
		t.Fatalf("labels are not displayed for recognition:\n%s", got)
	}
	// A reader must not be able to infer the count from the label list.
	if !strings.Contains(got, "they are not what") {
		t.Fatalf("the brief does not tell the reader labels are not the count:\n%s", got)
	}
}

// Rounds with no label at all are real rounds. The store records an absent label
// as empty, and three unlabelled jobs are three relocations.
func TestRelocationCountCountsUnlabelledRounds(t *testing.T) {
	got := ledgerRelocationBrief([]db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", ""),
		obs("internal/cli/a.go", "job-2", ""),
		obs("internal/cli/a.go", "job-3", ""),
	})
	if !strings.Contains(got, "rounds=3") {
		t.Fatalf("three unlabelled jobs must count as three rounds:\n%s", got)
	}
	if !strings.Contains(got, "labels not recorded") {
		t.Fatalf("an empty label list must be named, not rendered as an empty parenthesis:\n%s", got)
	}
}

// Below the threshold nothing is said. A count that warned at two would fire on
// almost every PR in this store and be ignored; three is the number the incident
// in #1419 had already agreed and never enforced.
func TestRelocationCountStaysSilentBelowTheThreshold(t *testing.T) {
	two := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F2"),
	}
	if got := ledgerRelocationBrief(two); got != "" {
		t.Fatalf("two rounds triggered a relocation warning:\n%s", got)
	}
}

// Findings are attributed PER FILE. A defect that moves between files is a
// different phenomenon from one relocating inside a single vessel, and the issue
// is about the second: rounds 4, 5 and 6 each arrived inside the previous fix.
func TestRelocationCountAttributesPerFileAndSkipsUnattributableRows(t *testing.T) {
	spread := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/b.go", "job-2", "F1"),
		obs("internal/cli/c.go", "job-3", "F1"),
	}
	if got := ledgerRelocationBrief(spread); got != "" {
		t.Fatalf("three rounds across three DIFFERENT files were reported as relocation in one:\n%s", got)
	}

	// A finding with no file cannot be attributed to a vessel, so it cannot
	// evidence relocation within one. Counting it would inflate every file.
	fileless := []db.ReviewFindingObservation{
		obs("", "job-1", "F1"), obs("", "job-2", "F1"), obs("", "job-3", "F1"),
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F2"),
	}
	if got := ledgerRelocationBrief(fileless); got != "" {
		t.Fatalf("fileless rows pushed a two-round file over the threshold:\n%s", got)
	}

	// A row with no observing job has no attributable round. THE DISCRIMINATING
	// SHAPE IS A JOBLESS ROW BESIDE REAL ONES: skipping it leaves two rounds and
	// silence, while admitting it adds a third bucket and crosses the threshold.
	// Three jobless rows alone would not discriminate - they share one empty key
	// either way, which is a mutant my first version of this test let survive.
	joblessBesideReal := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F2"),
		obs("internal/cli/a.go", "", "F3"),
	}
	if got := ledgerRelocationBrief(joblessBesideReal); got != "" {
		t.Fatalf("a row with no observing job pushed two real rounds over the threshold:\n%s", got)
	}
}
