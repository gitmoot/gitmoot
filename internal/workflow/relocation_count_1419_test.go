package workflow

import (
	"context"
	"fmt"
	"sort"
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
	if got := ledgerRelocationBrief(thorough, nil); got != "" {
		t.Fatalf("four findings in ONE round were reported as relocations:\n%s", got)
	}

	relocating := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F1"),
		obs("internal/cli/a.go", "job-3", "F1"),
	}
	got := ledgerRelocationBrief(relocating, nil)
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
	got := ledgerRelocationBrief(repeatedLabel, nil)
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
	if got := ledgerRelocationBrief(oneLensRun, nil); got != "" {
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
	}, nil)
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
	}, nil)
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
	if got := ledgerRelocationBrief(two, nil); got != "" {
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
	if got := ledgerRelocationBrief(spread, nil); got != "" {
		t.Fatalf("three rounds across three DIFFERENT files were reported as relocation in one:\n%s", got)
	}

	// A finding with no file cannot be attributed to a vessel, so it cannot
	// evidence relocation within one. Counting it would inflate every file.
	fileless := []db.ReviewFindingObservation{
		obs("", "job-1", "F1"), obs("", "job-2", "F1"), obs("", "job-3", "F1"),
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F2"),
	}
	if got := ledgerRelocationBrief(fileless, nil); got != "" {
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
	if got := ledgerRelocationBrief(joblessBesideReal, nil); got != "" {
		t.Fatalf("a row with no observing job pushed two real rounds over the threshold:\n%s", got)
	}
}

// #2066 round two. A REVIEW ROUND THAT FANS OUT IS ONE ROUND. Routine dispatch to
// several reviewers, and a high-risk lens splitting into children, give every job
// the SAME ReviewRound. Counting jobs would render rounds=3 for a single round.
func TestRelocationCountCollapsesFanOutWithinOneReviewRound(t *testing.T) {
	fanOut := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "lens-1", "F1"),
		obs("internal/cli/a.go", "lens-2", "F2"),
		obs("internal/cli/a.go", "lens-3", "F3"),
	}
	rounds := map[string]string{"lens-1": "review-1", "lens-2": "review-1", "lens-3": "review-1"}
	if got := ledgerRelocationBrief(fanOut, rounds); got != "" {
		t.Fatalf("three fan-out jobs in ONE review round were counted as three rounds:\n%s", got)
	}

	// Three ROUNDS, each fanned out to two jobs, is still three relocations: the
	// collapse must be per round, not a blanket de-duplication.
	threeRounds := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "j1a", "F1"), obs("internal/cli/a.go", "j1b", "F2"),
		obs("internal/cli/a.go", "j2a", "F1"), obs("internal/cli/a.go", "j2b", "F2"),
		obs("internal/cli/a.go", "j3a", "F1"), obs("internal/cli/a.go", "j3b", "F2"),
	}
	spread := map[string]string{
		"j1a": "review-1", "j1b": "review-1",
		"j2a": "review-2", "j2b": "review-2",
		"j3a": "review-3", "j3b": "review-3",
	}
	got := ledgerRelocationBrief(threeRounds, spread)
	if !strings.Contains(got, "rounds=3") {
		t.Fatalf("three fanned-out rounds must count as three:\n%s", got)
	}
}

// THE FALLBACK MUST NOT COLLAPSE UNKNOWN ROUNDS TOGETHER. Measured on this store,
// zero of 198 observing jobs carry a ReviewRound, so keying on the round alone
// would put every observation in one empty bucket and hide every relocation.
func TestRelocationCountFallsBackToTheJobWhenTheRoundIsUnknown(t *testing.T) {
	unknown := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F1"),
		obs("internal/cli/a.go", "job-3", "F1"),
	}
	for _, rounds := range []map[string]string{
		nil,
		{"job-1": "", "job-2": "", "job-3": ""},
		{"job-1": "   ", "job-2": "", "job-3": ""},
	} {
		got := ledgerRelocationBrief(unknown, rounds)
		if !strings.Contains(got, "rounds=3") {
			t.Fatalf("an unknown round collapsed three separate rounds into one (rounds=%v):\n%s", rounds, got)
		}
	}

	// A MIXED population is the real store: some jobs carry a round, some do not.
	// The known ones collapse, the parentless unknown ones stay distinct, total 2.
	mixedPop := []db.ReviewFindingObservation{
		obs("internal/cli/b.go", "lens-1", "F1"),
		obs("internal/cli/b.go", "lens-2", "F2"),
		obs("internal/cli/b.go", "solo", "F1"),
	}
	if got := ledgerRelocationBrief(mixedPop, map[string]string{"lens-1": "review-9", "lens-2": "review-9"}); got != "" {
		t.Fatalf("one fanned-out round plus one unknown-round job is two rounds, not three:\n%s", got)
	}
}

// #2066 ROUND THREE. THE ROUND STRING ALONE IS NOT A PR-WIDE IDENTITY.
// nextReviewRound mints review-1, review-2 and so on after filtering by
// sameTask, so the numbering RESTARTS PER TASK. Keying on the bare string
// collapsed task-A/review-1 and task-B/review-1 - two genuinely separate rounds -
// which is the deflation defect this predicate has now produced from two
// different keys.
func TestRelocationCountKeepsTwoTasksRoundsApart(t *testing.T) {
	sameNumberDifferentTasks := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-a", "F1"),
		obs("internal/cli/a.go", "job-b", "F1"),
		obs("internal/cli/a.go", "job-c", "F1"),
	}
	// What the resolver now returns: the task and the round, composed.
	perTask := map[string]string{
		"job-a": "task-A\x00review-1",
		"job-b": "task-B\x00review-1",
		"job-c": "task-C\x00review-1",
	}
	got := ledgerRelocationBrief(sameNumberDifferentTasks, perTask)
	if !strings.Contains(got, "rounds=3") {
		t.Fatalf("three tasks each at review-1 are three rounds, not one:\n%s", got)
	}

	// And within ONE task the collapse must still happen, or the fix has simply
	// disabled the previous round's correction.
	oneTaskFanOut := map[string]string{
		"job-a": "task-A\x00review-1",
		"job-b": "task-A\x00review-1",
		"job-c": "task-A\x00review-1",
	}
	if got := ledgerRelocationBrief(sameNumberDifferentTasks, oneTaskFanOut); got != "" {
		t.Fatalf("one task's fan-out at review-1 must still count once:\n%s", got)
	}
}

// The separator must not be forgeable. A task id or round label containing the
// composition character would otherwise let one pair impersonate another.
func TestRelocationCountRoundIdentityIsNotForgeable(t *testing.T) {
	rows := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-a", "F1"),
		obs("internal/cli/a.go", "job-b", "F1"),
		obs("internal/cli/a.go", "job-c", "F1"),
	}
	// "task-A" + "\x00review-1" must not equal "task" + "\x00" + "A\x00review-1"
	// after composition; distinct pairs stay distinct.
	crafted := map[string]string{
		"job-a": "task-A\x00review-1",
		"job-b": "task\x00A" + "\x00review-1",
		"job-c": "task-A\x00review-2",
	}
	if got := ledgerRelocationBrief(rows, crafted); !strings.Contains(got, "rounds=3") {
		t.Fatalf("crafted identities collapsed into fewer rounds:\n%s", got)
	}
}

// The resolver's own behaviour, which the brief cannot show: what it composes
// and that it refuses to scan without limit (#2066 round three).
func TestReviewRoundResolutionComposesTaskAndRoundAndIsBounded(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store}

	seed := func(id, task, round string) db.ReviewFindingObservation {
		insertCompletedJob(t, store, db.Job{ID: id, Agent: "reviewer", Type: "review"}, JobPayload{
			Repo: "gitmoot/gitmoot", PullRequest: 2066, TaskID: task, ReviewRound: round,
			Result: &AgentResult{Decision: "approved", Summary: "ok"},
		})
		return obs("internal/cli/a.go", id, "F1")
	}

	// Same round number, different tasks: the composed values must differ, which
	// is the whole point of the pair.
	rows := []db.ReviewFindingObservation{
		seed("job-task-a", "task-A", "review-1"),
		seed("job-task-b", "task-B", "review-1"),
	}
	resolved := engine.reviewRoundsForObservations(ctx, rows)
	if resolved["job-task-a"] == resolved["job-task-b"] {
		t.Fatalf("two tasks at review-1 resolved to the same identity %q", resolved["job-task-a"])
	}
	if !strings.Contains(resolved["job-task-a"], "task-A") || !strings.Contains(resolved["job-task-a"], "review-1") {
		t.Fatalf("resolved identity must carry both task and round, got %q", resolved["job-task-a"])
	}

	// A job carrying NO round yields no entry, so the caller falls back to job
	// keying rather than folding it in with every other roundless job.
	rows = append(rows, seed("job-no-round", "task-C", ""))
	resolved = engine.reviewRoundsForObservations(ctx, rows)
	if _, present := resolved["job-no-round"]; present {
		t.Fatal("a job with no review round produced an entry, which would fold roundless jobs together")
	}

	// THE SCAN IS BOUNDED. The previous version had no maximum and justified it
	// with a measurement of one store. Past the cap the count degrades to job
	// keying, which over-counts a fan-out rather than hiding a relocation.
	// OBSERVATIONS ARRIVE IN DESCENDING ID ORDER ON PURPOSE. jobs is built by
	// walking the observation SLICE, so a fixture that already supplies sorted
	// ids cannot tell a sorted prefix from an unsorted one - and it let the
	// dropped-sort mutant survive twice. Reversed input makes the two answers
	// disjoint: unsorted takes the HIGHEST ids, sorted takes the lowest.
	var many []db.ReviewFindingObservation
	for i := ledgerRoundResolutionCap + 11; i >= 0; i-- {
		many = append(many, seed(fmt.Sprintf("job-bulk-%03d", i), "task-bulk", fmt.Sprintf("review-%d", i)))
	}
	bounded := engine.reviewRoundsForObservations(ctx, many)
	if len(bounded) > ledgerRoundResolutionCap {
		t.Fatalf("resolution returned %d entries, above the cap of %d", len(bounded), ledgerRoundResolutionCap)
	}
	// THE SUBSET IS THE SORTED PREFIX, ASSERTED AS A SET. Checking only that one
	// early id is present passed roughly five times in six by luck under map
	// iteration - flaky rather than merely weak - and it let an unsorted mutant
	// survive. The exact expected set is the only assertion that pins
	// determinism.
	var ids []string
	for _, row := range many {
		ids = append(ids, row.ObserverJob)
	}
	sort.Strings(ids)
	want := map[string]struct{}{}
	for _, id := range ids[:ledgerRoundResolutionCap] {
		want[id] = struct{}{}
	}
	for id := range bounded {
		if _, expected := want[id]; !expected {
			t.Fatalf("resolved %q, which is outside the sorted prefix: the subset is not deterministic", id)
		}
	}
	for id := range want {
		if _, resolved := bounded[id]; !resolved {
			t.Fatalf("sorted-prefix job %q was not resolved: the subset is not the prefix", id)
		}
	}
}

// #2066 ROUND FOUR, P1. A ROUNDLESS FAN-OUT IS STILL ONE ROUND. Local
// agent-review coordinators persist no ReviewRound and delegationRequest passes
// that blank value to every review child, so a supported review-panel fan-out
// arrives roundless - and the previous fallback counted its children as separate
// rounds, which is the inflation the round before had asked me to fix.
//
// I had measured ReviewRound as empty on all 198 observing jobs and concluded
// the fan-out hazard was theoretical. Empty is EXACTLY when the fallback fires,
// so the measurement that justified the fallback was the same fact that made it
// wrong.
func TestRelocationCountCollapsesARoundlessFanOutByItsCoordinator(t *testing.T) {
	fanOut := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "panel-1", "F1"),
		obs("internal/cli/a.go", "panel-2", "F2"),
		obs("internal/cli/a.go", "panel-3", "F3"),
	}
	// What the resolver returns for a roundless fan-out: the shared coordinator.
	sharedParent := map[string]string{
		"panel-1": "parent\x00coordinator-9",
		"panel-2": "parent\x00coordinator-9",
		"panel-3": "parent\x00coordinator-9",
	}
	if got := ledgerRelocationBrief(fanOut, sharedParent); got != "" {
		t.Fatalf("three roundless children of ONE coordinator counted as three rounds:\n%s", got)
	}

	// Three separate coordinators are three genuine rounds.
	distinctParents := map[string]string{
		"panel-1": "parent\x00coordinator-1",
		"panel-2": "parent\x00coordinator-2",
		"panel-3": "parent\x00coordinator-3",
	}
	if got := ledgerRelocationBrief(fanOut, distinctParents); !strings.Contains(got, "rounds=3") {
		t.Fatalf("three separate coordinators must count as three rounds:\n%s", got)
	}

	// A round identity and a parent identity must never collide, or a coordinator
	// id equal to a task id would fold two different groupings together.
	mixed := map[string]string{
		"panel-1": "round\x00task-A\x00review-1",
		"panel-2": "parent\x00task-A",
		"panel-3": "parent\x00coordinator-9",
	}
	if got := ledgerRelocationBrief(fanOut, mixed); !strings.Contains(got, "rounds=3") {
		t.Fatalf("round-keyed and parent-keyed identities collided:\n%s", got)
	}
}

// #2066 ROUND FOUR, P2. The rendered text must not name a unit the code does not
// use: it said "counted by observing job" while the implementation collapses a
// fan-out through a logical identity, so a three-job fan-out reading as one round
// contradicted the brief a reviewer was reading.
func TestRelocationBriefDescribesTheUnitItActuallyCounts(t *testing.T) {
	got := ledgerRelocationBrief([]db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F1"),
		obs("internal/cli/a.go", "job-3", "F1"),
	}, nil)
	if strings.Contains(got, "counted\nby observing job") || strings.Contains(got, "counted by observing job") {
		t.Fatalf("the brief still claims the count is by observing job:\n%s", got)
	}
	for _, want := range []string{"review round when one is recorded", "coordinator that", "only otherwise by the individual reviewing job"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the brief does not describe the identity ladder (%q):\n%s", want, got)
		}
	}
}

// The resolver's ladder, end to end against a real store (#2066 round four).
func TestReviewRoundResolutionFallsBackToTheCoordinator(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store}

	seedChild := func(id, parent, round string) db.ReviewFindingObservation {
		insertCompletedJob(t, store, db.Job{ID: id, Agent: "reviewer", Type: "review"}, JobPayload{
			Repo: "gitmoot/gitmoot", PullRequest: 2066, ParentJobID: parent, ReviewRound: round,
			Result: &AgentResult{Decision: "approved", Summary: "ok"},
		})
		return obs("internal/cli/a.go", id, "F1")
	}

	// A roundless fan-out: no ReviewRound anywhere, one shared coordinator.
	rows := []db.ReviewFindingObservation{
		seedChild("panel-a", "coordinator-9", ""),
		seedChild("panel-b", "coordinator-9", ""),
	}
	resolved := engine.reviewRoundsForObservations(ctx, rows)
	if resolved["panel-a"] == "" || resolved["panel-a"] != resolved["panel-b"] {
		t.Fatalf("roundless siblings must share one identity, got %q and %q", resolved["panel-a"], resolved["panel-b"])
	}
	if !strings.Contains(resolved["panel-a"], "coordinator-9") {
		t.Fatalf("the fallback identity must name the coordinator, got %q", resolved["panel-a"])
	}

	// A recorded round WINS over the parent, because it is the logical identity
	// and a coordinator can dispatch more than one round.
	rows = append(rows, seedChild("panel-c", "coordinator-9", "review-2"))
	resolved = engine.reviewRoundsForObservations(ctx, rows)
	if resolved["panel-c"] == resolved["panel-a"] {
		t.Fatal("a child with its own review round was folded into its coordinator's roundless group")
	}

	// Neither round nor parent: no entry, so the caller counts by job id.
	rows = append(rows, seedChild("solo", "", ""))
	resolved = engine.reviewRoundsForObservations(ctx, rows)
	if _, present := resolved["solo"]; present {
		t.Fatal("a job with neither round nor parent produced an identity, which would fold unrelated jobs together")
	}
}

// #2066 ROUND FIVE, P1. A LINE-QUALIFIED LOCATOR MUST NOT SPLIT ONE FILE INTO
// SEPARATE BUCKETS. The ledger accepts the repo's documented path:line
// convention and preserves it in File, so three rounds recorded against
// a.go:10, a.go:20 and a.go:30 were three one-round buckets emitting no
// warning - the deflation defect from a fourth direction: not the wrong unit,
// the wrong SUBJECT.
//
// Every relocation test before this one used bare paths exclusively, so the
// focused and full suites could not exercise the production-supported shape.
// #2076's class, fifth instance, and the fourth of them in my own tests.
func TestRelocationCountGroupsLineQualifiedLocatorsByFile(t *testing.T) {
	// The store writes File AND Line together, so a fixture that sets only File
	// models a row the writer does not produce - and the recorded Line is exactly
	// what makes an ambiguous locator decidable.
	at := func(file string, line int64, job string) db.ReviewFindingObservation {
		return db.ReviewFindingObservation{File: file, Line: line, ObserverJob: job, RoundLabel: "F1"}
	}

	lineQualified := []db.ReviewFindingObservation{
		at("internal/workflow/a.go:10", 10, "job-1"),
		at("internal/workflow/a.go:20", 20, "job-2"),
		at("internal/workflow/a.go:30", 30, "job-3"),
	}
	got := ledgerRelocationBrief(lineQualified, nil)
	if !strings.Contains(got, "rounds=3") {
		t.Fatalf("line-qualified locators split the file into separate buckets:\n%s", got)
	}
	if !strings.Contains(got, "internal/workflow/a.go  rounds=3") {
		t.Fatalf("the brief must name the canonical path:\n%s", got)
	}

	// MIXED bare and qualified locators are one file: the convention is a
	// reviewer's choice per finding, so the real store holds both.
	mixed := []db.ReviewFindingObservation{
		at("internal/workflow/a.go", 0, "job-1"),
		at("internal/workflow/a.go:20", 20, "job-2"),
		at(" internal/workflow/a.go: 30 ", 30, "job-3"),
	}
	if got := ledgerRelocationBrief(mixed, nil); !strings.Contains(got, "rounds=3") {
		t.Fatalf("bare, qualified and whitespace-qualified spellings were counted separately:\n%s", got)
	}

	// #2066 ROUND SIX: A COLON-BEARING PATH WITH A LINE QUALIFIER. splitLocator
	// cut at the FIRST colon, so "pkg:a.go:10" failed Atoi on "a.go:10" and kept
	// the raw locator - three rounds, three buckets, no warning.
	colonWithLine := []db.ReviewFindingObservation{
		at("pkg:a.go:10", 10, "job-1"),
		at("pkg:a.go:20", 20, "job-2"),
		at("pkg:a.go:30", 30, "job-3"),
	}
	if got := ledgerRelocationBrief(colonWithLine, nil); !strings.Contains(got, "pkg:a.go  rounds=3") {
		t.Fatalf("a colon-bearing path with a line qualifier split across buckets:\n%s", got)
	}

	// AND THE OPPOSITE DIRECTION, which cutting at the last colon gets wrong on
	// text alone: "pkg:10" is a real FILENAME here, and its row records no line.
	// Folding it to "pkg" would merge two different files and invent a
	// relocation.
	filenameLooksLikeLine := []db.ReviewFindingObservation{
		at("pkg:10", 0, "job-1"),
		at("pkg:20", 0, "job-2"),
		at("pkg:30", 0, "job-3"),
	}
	if got := ledgerRelocationBrief(filenameLooksLikeLine, nil); got != "" {
		t.Fatalf("three different files whose names end in a number were folded into one:\n%s", got)
	}

	// A trailing number that does NOT match the recorded line is part of the name.
	mismatched := []db.ReviewFindingObservation{
		at("pkg:10", 99, "job-1"),
		at("pkg:20", 99, "job-2"),
		at("pkg:30", 99, "job-3"),
	}
	if got := ledgerRelocationBrief(mismatched, nil); got != "" {
		t.Fatalf("a suffix that does not match the recorded line was stripped anyway:\n%s", got)
	}

	// DIFFERENT files must stay different, or the canonicalisation has collapsed
	// everything.
	distinct := []db.ReviewFindingObservation{
		at("internal/workflow/a.go:10", 10, "job-1"),
		at("internal/workflow/b.go:10", 10, "job-2"),
		at("internal/workflow/c.go:10", 10, "job-3"),
	}
	if got := ledgerRelocationBrief(distinct, nil); got != "" {
		t.Fatalf("three different files were folded into one bucket:\n%s", got)
	}
}

// #2066 ROUND SIX, P1. TWO CHILDREN OF ONE COORDINATOR THAT REVIEWED DIFFERENT
// HEADS ARE TWO ROUNDS. Both identity rungs omitted the reviewed head, so a
// coordinator's dependent review legs - deferred legs are enqueued after their
// dependencies settle, and delegationHeadSHA resolves the then-current PR mirror
// - collapsed into one round if a push landed between their dispatches. That
// undercounts genuine review/fix cycles, which is the defect this brief exists to
// surface.
func TestReviewRoundResolutionSeparatesDifferentReviewedHeads(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store}

	seed := func(id, parent, round, head string) db.ReviewFindingObservation {
		insertCompletedJob(t, store, db.Job{ID: id, Agent: "reviewer", Type: "review"}, JobPayload{
			Repo: "gitmoot/gitmoot", PullRequest: 2066, ParentJobID: parent, ReviewRound: round, HeadSHA: head,
			Result: &AgentResult{Decision: "approved", Summary: "ok"},
		})
		return db.ReviewFindingObservation{File: "internal/cli/a.go", ObserverJob: id, RoundLabel: "F1"}
	}

	// Roundless siblings of ONE coordinator at DIFFERENT heads: two rounds.
	differentHeads := []db.ReviewFindingObservation{
		seed("leg-h1", "coordinator-9", "", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		seed("leg-h2", "coordinator-9", "", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}
	resolved := engine.reviewRoundsForObservations(ctx, differentHeads)
	if resolved["leg-h1"] == resolved["leg-h2"] {
		t.Fatalf("two legs that reviewed different heads share one identity %q", resolved["leg-h1"])
	}

	// SAME head still collapses - that is the fan-out case the previous round
	// fixed, and separating heads must not undo it.
	sameHead := []db.ReviewFindingObservation{
		seed("panel-a", "coordinator-7", "", "cccccccccccccccccccccccccccccccccccccccc"),
		seed("panel-b", "coordinator-7", "", "cccccccccccccccccccccccccccccccccccccccc"),
	}
	resolved = engine.reviewRoundsForObservations(ctx, sameHead)
	if resolved["panel-a"] != resolved["panel-b"] || resolved["panel-a"] == "" {
		t.Fatalf("same-head siblings no longer collapse: %q vs %q", resolved["panel-a"], resolved["panel-b"])
	}

	// A RECORDED ROUND is separated by head too: fan-out members can be resynced
	// across a push, which is a new round by any reading.
	roundAcrossHeads := []db.ReviewFindingObservation{
		seed("r1-h1", "coordinator-5", "review-1", "dddddddddddddddddddddddddddddddddddddddd"),
		seed("r1-h2", "coordinator-5", "review-1", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"),
	}
	resolved = engine.reviewRoundsForObservations(ctx, roundAcrossHeads)
	if resolved["r1-h1"] == resolved["r1-h2"] {
		t.Fatalf("one recorded round across two heads shares an identity %q", resolved["r1-h1"])
	}
}
