package workflow

import (
	"context"
	"errors"
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
	if got := ledgerRelocationBrief(thorough, nil, relocationTestTree); got != "" {
		t.Fatalf("four findings in ONE round were reported as relocations:\n%s", got)
	}

	relocating := []db.ReviewFindingObservation{
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F1"),
		obs("internal/cli/a.go", "job-3", "F1"),
	}
	got := ledgerRelocationBrief(relocating, nil, relocationTestTree)
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
	got := ledgerRelocationBrief(repeatedLabel, nil, relocationTestTree)
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
	if got := ledgerRelocationBrief(oneLensRun, nil, relocationTestTree); got != "" {
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
	}, nil, relocationTestTree)
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
	}, nil, relocationTestTree)
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
	if got := ledgerRelocationBrief(two, nil, relocationTestTree); got != "" {
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
	if got := ledgerRelocationBrief(spread, nil, relocationTestTree); got != "" {
		t.Fatalf("three rounds across three DIFFERENT files were reported as relocation in one:\n%s", got)
	}

	// A finding with no file cannot be attributed to a vessel, so it cannot
	// evidence relocation within one. Counting it would inflate every file.
	fileless := []db.ReviewFindingObservation{
		obs("", "job-1", "F1"), obs("", "job-2", "F1"), obs("", "job-3", "F1"),
		obs("internal/cli/a.go", "job-1", "F1"),
		obs("internal/cli/a.go", "job-2", "F2"),
	}
	if got := ledgerRelocationBrief(fileless, nil, relocationTestTree); got != "" {
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
	if got := ledgerRelocationBrief(joblessBesideReal, nil, relocationTestTree); got != "" {
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
	if got := ledgerRelocationBrief(fanOut, rounds, relocationTestTree); got != "" {
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
	got := ledgerRelocationBrief(threeRounds, spread, relocationTestTree)
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
		got := ledgerRelocationBrief(unknown, rounds, relocationTestTree)
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
	if got := ledgerRelocationBrief(mixedPop, map[string]string{"lens-1": "review-9", "lens-2": "review-9"}, relocationTestTree); got != "" {
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
	got := ledgerRelocationBrief(sameNumberDifferentTasks, perTask, relocationTestTree)
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
	if got := ledgerRelocationBrief(sameNumberDifferentTasks, oneTaskFanOut, relocationTestTree); got != "" {
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
	if got := ledgerRelocationBrief(rows, crafted, relocationTestTree); !strings.Contains(got, "rounds=3") {
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
	if got := ledgerRelocationBrief(fanOut, sharedParent, relocationTestTree); got != "" {
		t.Fatalf("three roundless children of ONE coordinator counted as three rounds:\n%s", got)
	}

	// Three separate coordinators are three genuine rounds.
	distinctParents := map[string]string{
		"panel-1": "parent\x00coordinator-1",
		"panel-2": "parent\x00coordinator-2",
		"panel-3": "parent\x00coordinator-3",
	}
	if got := ledgerRelocationBrief(fanOut, distinctParents, relocationTestTree); !strings.Contains(got, "rounds=3") {
		t.Fatalf("three separate coordinators must count as three rounds:\n%s", got)
	}

	// A round identity and a parent identity must never collide, or a coordinator
	// id equal to a task id would fold two different groupings together.
	mixed := map[string]string{
		"panel-1": "round\x00task-A\x00review-1",
		"panel-2": "parent\x00task-A",
		"panel-3": "parent\x00coordinator-9",
	}
	if got := ledgerRelocationBrief(fanOut, mixed, relocationTestTree); !strings.Contains(got, "rounds=3") {
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
	}, nil, relocationTestTree)
	if strings.Contains(got, "counted\nby observing job") || strings.Contains(got, "counted by observing job") {
		t.Fatalf("the brief still claims the count is by observing job:\n%s", got)
	}
	// #2066 round seven, P2: the head is part of every rung, so the brief must
	// say so - a reader who believes one coordinator counts once cannot interpret
	// a count that separated its legs by head.
	for _, want := range []string{
		"review round when one is recorded", "coordinator that",
		"only otherwise by the individual reviewing job",
		"INCLUDING the job fallback", "the exact head that was reviewed",
		"a fan-out at ONE head is one",
	} {
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
	got := ledgerRelocationBrief(lineQualified, nil, relocationTestTree)
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
	if got := ledgerRelocationBrief(mixed, nil, relocationTestTree); !strings.Contains(got, "rounds=3") {
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
	if got := ledgerRelocationBrief(colonWithLine, nil, relocationTestTree); !strings.Contains(got, "pkg:a.go  rounds=3") {
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
	if got := ledgerRelocationBrief(filenameLooksLikeLine, nil, relocationTestTree); got != "" {
		t.Fatalf("three different files whose names end in a number were folded into one:\n%s", got)
	}

	// A trailing number that does NOT match the recorded line is part of the name.
	mismatched := []db.ReviewFindingObservation{
		at("pkg:10", 99, "job-1"),
		at("pkg:20", 99, "job-2"),
		at("pkg:30", 99, "job-3"),
	}
	if got := ledgerRelocationBrief(mismatched, nil, relocationTestTree); got != "" {
		t.Fatalf("a suffix that does not match the recorded line was stripped anyway:\n%s", got)
	}

	// DIFFERENT files must stay different, or the canonicalisation has collapsed
	// everything.
	distinct := []db.ReviewFindingObservation{
		at("internal/workflow/a.go:10", 10, "job-1"),
		at("internal/workflow/b.go:10", 10, "job-2"),
		at("internal/workflow/c.go:10", 10, "job-3"),
	}
	if got := ledgerRelocationBrief(distinct, nil, relocationTestTree); got != "" {
		t.Fatalf("three different files were folded into one bucket:\n%s", got)
	}
}

// #2066 ROUND SIX, P1. TWO CHILDREN OF ONE COORDINATOR THAT REVIEWED DIFFERENT
// HEADS ARE TWO ROUNDS. A coordinator's dependent review legs - deferred legs
// are enqueued after their dependencies settle, and delegationHeadSHA resolves
// the then-current PR mirror - collapsed into one round if a push landed between
// their dispatches. That undercounts genuine review/fix cycles, which is the
// defect this brief exists to surface.
//
// ROUND SEVEN MOVED WHERE THIS IS DECIDED, so the assertion moved with it. The
// head is no longer part of the job's resolved identity, because one job can
// record observations at several heads; it is appended per OBSERVATION. The
// resolver can therefore no longer separate these legs, and asserting that it
// does would pin the defect the round-seven reviewer found. The separation is
// asserted where it now happens: the rendered count.
func TestRelocationCountSeparatesDifferentReviewedHeads(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store}

	seed := func(id, parent, round, head string) db.ReviewFindingObservation {
		insertCompletedJob(t, store, db.Job{ID: id, Agent: "reviewer", Type: "review"}, JobPayload{
			Repo: "gitmoot/gitmoot", PullRequest: 2066, ParentJobID: parent, ReviewRound: round, HeadSHA: head,
			Result: &AgentResult{Decision: "approved", Summary: "ok"},
		})
		// The store stamps every observation with its own 40-character head and
		// REFUSES one without it, so a fixture that omits it models a row the
		// writer cannot produce.
		return db.ReviewFindingObservation{File: "internal/cli/a.go", ObserverJob: id, RoundLabel: "F1", HeadSHA: head}
	}

	brief := func(rows []db.ReviewFindingObservation) string {
		return ledgerRelocationBrief(rows, engine.reviewRoundsForObservations(ctx, rows), relocationTestTree)
	}

	// Roundless siblings of ONE coordinator at THREE different heads: three
	// rounds, which is what the warning exists to say.
	differentHeads := []db.ReviewFindingObservation{
		seed("leg-h1", "coordinator-9", "", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		seed("leg-h2", "coordinator-9", "", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		seed("leg-h3", "coordinator-9", "", "ffffffffffffffffffffffffffffffffffffffff"),
	}
	if got := brief(differentHeads); !strings.Contains(got, "rounds=3") {
		t.Fatalf("three legs that reviewed different heads collapsed:\n%s", got)
	}

	// SAME head still collapses - that is the fan-out case round five fixed, and
	// separating heads must not undo it.
	sameHead := []db.ReviewFindingObservation{
		seed("panel-a", "coordinator-7", "", "cccccccccccccccccccccccccccccccccccccccc"),
		seed("panel-b", "coordinator-7", "", "cccccccccccccccccccccccccccccccccccccccc"),
		seed("panel-c", "coordinator-7", "", "cccccccccccccccccccccccccccccccccccccccc"),
	}
	if got := brief(sameHead); got != "" {
		t.Fatalf("a three-member fan-out at ONE head was counted as three rounds:\n%s", got)
	}

	// A RECORDED ROUND is separated by head too: fan-out members can be resynced
	// across a push, which is a new round by any reading.
	roundAcrossHeads := []db.ReviewFindingObservation{
		seed("r1-h1", "coordinator-5", "review-1", "dddddddddddddddddddddddddddddddddddddddd"),
		seed("r1-h2", "coordinator-5", "review-1", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"),
		seed("r1-h3", "coordinator-5", "review-1", "1111111111111111111111111111111111111111"),
	}
	if got := brief(roundAcrossHeads); !strings.Contains(got, "rounds=3") {
		t.Fatalf("one recorded round across three heads collapsed:\n%s", got)
	}
}

// #2066 ROUND SEVEN, P1. ONE OBSERVER JOB CAN CARRY OBSERVATIONS AT SEVERAL
// HEADS. RetryJob reuses the job ID, clears the result and may retarget the
// head, and findings are recorded before advancement returns, so a retried
// reviewer records at H1 and then at H2 under ONE job id. The payload holds only
// the job's CURRENT head, so qualifying the identity from the payload stamped
// every observation with the latest head and collapsed genuine rounds.
//
// The prior round's test could not see this: it used DISTINCT job ids and left
// each observation's HeadSHA empty, which is exactly the shape the defect hides
// behind.
func TestRelocationCountSeparatesOneJobsObservationsByRecordedHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store}

	// ONE job, whose payload names only its latest head.
	insertCompletedJob(t, store, db.Job{ID: "retried", Agent: "reviewer", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 2066, ReviewRound: "review-1", TaskID: "task-1",
		HeadSHA: "cccccccccccccccccccccccccccccccccccccccc",
		Result:  &AgentResult{Decision: "changes_requested", Summary: "ok"},
	})
	at := func(head string) db.ReviewFindingObservation {
		return db.ReviewFindingObservation{
			File: "internal/cli/a.go", ObserverJob: "retried", RoundLabel: "F1", HeadSHA: head,
		}
	}
	brief := func(rows []db.ReviewFindingObservation) string {
		return ledgerRelocationBrief(rows, engine.reviewRoundsForObservations(ctx, rows), relocationTestTree)
	}

	rows := []db.ReviewFindingObservation{
		at("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		at("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		at("cccccccccccccccccccccccccccccccccccccccc"),
	}
	if got := brief(rows); !strings.Contains(got, "rounds=3") {
		t.Fatalf("three recorded heads under one retried job collapsed:\n%s", got)
	}

	// The SAME head under one job is still ONE round: a reviewer recording three
	// findings in a single pass has not relocated anything.
	same := []db.ReviewFindingObservation{
		at("cccccccccccccccccccccccccccccccccccccccc"),
		at("cccccccccccccccccccccccccccccccccccccccc"),
		at("cccccccccccccccccccccccccccccccccccccccc"),
	}
	if got := brief(same); got != "" {
		t.Fatalf("three findings in one pass at one head were counted as three rounds:\n%s", got)
	}
}

// #2066 ROUND SEVEN, P1, second half: THE DECIDING FIELD WAS EMPTY IN EVERY REAL
// ROW. Round six stripped a ":<n>" suffix only when it matched the row's
// recorded Line. Measured against the live store: 9 of 907 observation rows
// carry a colon in File and ALL 9 record NO Line, so that rule never fired where
// the ambiguity actually occurs. One real PR (1910) holds three spellings of one
// test file at lines 32, 53 and 248, which sat in three separate buckets.
func TestRelocationCountGroupsQualifiedLocatorsWithNoRecordedLine(t *testing.T) {
	at := func(file string, job string) db.ReviewFindingObservation {
		return db.ReviewFindingObservation{File: file, ObserverJob: job, RoundLabel: "F1"}
	}

	// The measured shape from PR 1910, Line unset on every row.
	measured := []db.ReviewFindingObservation{
		at("internal/daemon/resolver_refusal_log_test.go:32", "job-1"),
		at("internal/daemon/resolver_refusal_log_test.go:53", "job-2"),
		at("internal/daemon/resolver_refusal_log_test.go:248", "job-3"),
	}
	if got := ledgerRelocationBrief(measured, nil, relocationTestTree); !strings.Contains(got, "internal/daemon/resolver_refusal_log_test.go  rounds=3") {
		t.Fatalf("three line-qualified spellings with no recorded Line stayed in separate buckets:\n%s", got)
	}

	// THIS CASE HAS NOW BEEN DECIDED THREE WAYS, and the third is the only one
	// with evidence behind it. Round seven folded it because the prefix held a
	// separator. Round eight refused to, because a separator says nothing about
	// the NAME. Round nine folds it again - but for a different reason: the
	// TREE says "apps/web/public/_landing" exists and the full locator does not.
	// Shape was a guess in both directions; existence is an observation.
	separatorOnly := []db.ReviewFindingObservation{
		at("apps/web/public/_landing:372", "job-1"),
		at("apps/web/public/_landing:373", "job-2"),
		at("apps/web/public/_landing:374", "job-3"),
	}
	if got := ledgerRelocationBrief(separatorOnly, nil, relocationTestTree); !strings.Contains(got, "apps/web/public/_landing  rounds=3") {
		t.Fatalf("an extensionless path the tree confirms was not grouped:\n%s", got)
	}

	// #2066 ROUND EIGHT, P1: THE COUNTER-EXAMPLE THAT KILLED PATH SHAPE. The
	// store accepts these locators and nothing in the text says the suffix is a
	// line, so folding them would invent rounds=3 from three different files.
	pathShapedButNamed := []db.ReviewFindingObservation{
		at("dir/pkg:10", "job-1"),
		at("dir/pkg:20", "job-2"),
		at("dir/pkg:30", "job-3"),
	}
	if got := ledgerRelocationBrief(pathShapedButNamed, nil, relocationTestTree); got != "" {
		t.Fatalf("three separator-bearing names with numeric suffixes were folded:\n%s", got)
	}

	// An extension on the last segment IS about the name, so a dotted root file
	// still groups: "go.mod:3" is a line-qualified locator by any convention.
	dottedRoot := []db.ReviewFindingObservation{
		at("go.mod:3", "job-1"),
		at("go.mod:9", "job-2"),
		at("go.mod:14", "job-3"),
	}
	if got := ledgerRelocationBrief(dottedRoot, nil, relocationTestTree); !strings.Contains(got, "go.mod  rounds=3") {
		t.Fatalf("an extension-bearing root file did not group:\n%s", got)
	}

	// A LEADING DOT IS A PREFIX, NOT AN EXTENSION. ".env" is the whole name, so
	// ".env:10" is declined and stays a floor. Deciding it the other way would
	// fold any dotfile whose name ends in a number.
	dotfile := []db.ReviewFindingObservation{
		at(".env:10", "job-1"),
		at(".env:20", "job-2"),
		at(".env:30", "job-3"),
	}
	if got := ledgerRelocationBrief(dotfile, nil, relocationTestTree); got != "" {
		t.Fatalf("a leading dot was read as an extension separator:\n%s", got)
	}

	// A TRAILING DOT IS NOT AN EXTENSION EITHER: there is nothing after it to be
	// one.
	trailingDot := []db.ReviewFindingObservation{
		at("pkg.:10", "job-1"),
		at("pkg.:20", "job-2"),
		at("pkg.:30", "job-3"),
	}
	if got := ledgerRelocationBrief(trailingDot, nil, relocationTestTree); got != "" {
		t.Fatalf("a trailing dot was accepted as an extension:\n%s", got)
	}

	// A DOT IN A PARENT DIRECTORY IS NOT AN EXTENSION ON THE FILE. Only the last
	// segment is inspected, so "v1.2/pkg:10" keeps its whole value.
	dottedDirectory := []db.ReviewFindingObservation{
		at("v1.2/pkg:10", "job-1"),
		at("v1.2/pkg:20", "job-2"),
		at("v1.2/pkg:30", "job-3"),
	}
	if got := ledgerRelocationBrief(dottedDirectory, nil, relocationTestTree); got != "" {
		t.Fatalf("a dot in a PARENT directory was read as the file's extension:\n%s", got)
	}

	// AND THE FALSE FOLD THE OLD RULE TOOK: a real filename "pkg:10" whose
	// finding happens to be AT line 10 was stripped to "pkg", merging different
	// files and inventing a relocation. Not path-shaped, so it keeps its name.
	coincidence := []db.ReviewFindingObservation{
		{File: "pkg:10", Line: 10, ObserverJob: "job-1", RoundLabel: "F1"},
		{File: "pkg:20", Line: 20, ObserverJob: "job-2", RoundLabel: "F1"},
		{File: "pkg:30", Line: 30, ObserverJob: "job-3", RoundLabel: "F1"},
	}
	if got := ledgerRelocationBrief(coincidence, nil, relocationTestTree); got != "" {
		t.Fatalf("three files whose names end in a matching number were folded into one:\n%s", got)
	}
}

// #2066 ROUND EIGHT, P1. THE JOB FALLBACK NEEDS THE HEAD TOO. Round seven
// appended the observation's head only inside the resolved-base branch, so a
// retried job with NO ReviewRound and NO ParentJobID keyed its observations by
// the reused job ID alone and reported one round. That is the ORDINARY shape for
// a local review enqueue on a top-level job, not a corner case.
//
// Round seven's own regression seeded ReviewRound="review-1", so it never
// reached this path: the fix and its test agreed with each other and not with
// production. This one seeds neither field.
func TestRelocationCountSeparatesHeadsOnTheJobFallback(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store}

	// A top-level job: no ReviewRound, no ParentJobID, so the resolver yields
	// nothing and the identity falls back to the job id.
	insertCompletedJob(t, store, db.Job{ID: "solo-retried", Agent: "reviewer", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 2066,
		HeadSHA: "cccccccccccccccccccccccccccccccccccccccc",
		Result:  &AgentResult{Decision: "changes_requested", Summary: "ok"},
	})
	at := func(head string) db.ReviewFindingObservation {
		return db.ReviewFindingObservation{
			File: "internal/cli/a.go", ObserverJob: "solo-retried", RoundLabel: "F1", HeadSHA: head,
		}
	}
	resolved := engine.reviewRoundsForObservations(ctx, []db.ReviewFindingObservation{at("x")})
	if base := resolved["solo-retried"]; base != "" {
		t.Fatalf("this fixture must exercise the FALLBACK, but the resolver returned %q", base)
	}

	rows := []db.ReviewFindingObservation{
		at("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		at("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		at("cccccccccccccccccccccccccccccccccccccccc"),
	}
	got := ledgerRelocationBrief(rows, engine.reviewRoundsForObservations(ctx, rows), relocationTestTree)
	if !strings.Contains(got, "rounds=3") {
		t.Fatalf("three heads under one unattributed retried job collapsed:\n%s", got)
	}

	// One head is still one round on the fallback path too.
	same := []db.ReviewFindingObservation{
		at("cccccccccccccccccccccccccccccccccccccccc"),
		at("cccccccccccccccccccccccccccccccccccccccc"),
		at("cccccccccccccccccccccccccccccccccccccccc"),
	}
	if got := ledgerRelocationBrief(same, engine.reviewRoundsForObservations(ctx, same), relocationTestTree); got != "" {
		t.Fatalf("three findings at one head on the fallback path were counted as three rounds:\n%s", got)
	}

	// AND AN UNATTRIBUTABLE ROW MUST STAY UNATTRIBUTABLE. A row with no observer
	// job is skipped; it must not become countable by acquiring a head.
	headless := []db.ReviewFindingObservation{
		{File: "internal/cli/b.go", RoundLabel: "F1", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{File: "internal/cli/b.go", RoundLabel: "F1", HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{File: "internal/cli/b.go", RoundLabel: "F1", HeadSHA: "cccccccccccccccccccccccccccccccccccccccc"},
	}
	if got := ledgerRelocationBrief(headless, nil, relocationTestTree); got != "" {
		t.Fatalf("rows with no observing job manufactured rounds from their heads:\n%s", got)
	}
}

// relocationTestTree stands in for PathExistsAtHead. It answers for the paths
// the fixtures name, and it is deliberately EXPLICIT rather than permissive: a
// stub returning true for everything would keep every locator whole and make
// the grouping assertions vacuous, while one returning false for everything
// would fold nothing. Each entry states which spelling the tree really holds.
func relocationTestTree(path string) bool {
	switch path {
	// Real files, so a line-qualified locator naming them must GROUP.
	case "internal/cli/a.go", "internal/cli/b.go", "internal/workflow/a.go",
		"internal/workflow/b.go", "internal/workflow/c.go",
		"internal/daemon/resolver_refusal_log_test.go", "pkg:a.go",
		"apps/web/public/_landing", "go.mod", "pkg", "v1.2/pkg", "dir":
		return true
	// Files whose NAMES end in a number, so their locators must stay WHOLE.
	case "pkg:10", "pkg:20", "pkg:30",
		"dir/pkg:10", "dir/pkg:20", "dir/pkg:30",
		"dir/pkg.go:10", "dir/pkg.go:20", "dir/pkg.go:30",
		"v1.2/pkg:10", "v1.2/pkg:20", "v1.2/pkg:30",
		".env:10", ".env:20", ".env:30",
		"pkg.:10", "pkg.:20", "pkg.:30":
		return true
	default:
		return false
	}
}

// #2066 ROUND NINE, P1: THE REVIEWER'S COUNTER-EXAMPLE, WHICH THE EXTENSION RULE
// FOLDED. Three tracked files whose names end in ":<n>" and which happen to
// carry a final-segment extension. The tree confirms each full name exists, so
// each keeps its whole locator and no relocation is manufactured.
func TestRelocationCountKeepsExtensionBearingNamesThatEndInNumbers(t *testing.T) {
	at := func(file, job string) db.ReviewFindingObservation {
		return db.ReviewFindingObservation{File: file, ObserverJob: job, RoundLabel: "F1"}
	}
	named := []db.ReviewFindingObservation{
		at("dir/pkg.go:10", "job-1"),
		at("dir/pkg.go:20", "job-2"),
		at("dir/pkg.go:30", "job-3"),
	}
	if got := ledgerRelocationBrief(named, nil, relocationTestTree); got != "" {
		t.Fatalf("three tracked files whose names end in a number were folded:\n%s", got)
	}

	// AND THE SAME SPELLING GROUPS when the tree says the PREFIX is the file and
	// the full locator is not. Identical text, opposite answer, decided by
	// evidence rather than by the string: this is the pair that shows the key is
	// no longer guessing.
	qualified := []db.ReviewFindingObservation{
		at("internal/workflow/a.go:10", "job-1"),
		at("internal/workflow/a.go:20", "job-2"),
		at("internal/workflow/a.go:30", "job-3"),
	}
	if got := ledgerRelocationBrief(qualified, nil, relocationTestTree); !strings.Contains(got, "internal/workflow/a.go  rounds=3") {
		t.Fatalf("a line-qualified locator the tree resolves was not grouped:\n%s", got)
	}
}

// WITH NO RESOLVER THE KEY MUST NOT GUESS. Every caller outside the daemon has
// no tree, and the brief must then under-report rather than fold on text. This
// is the floor the brief documents, and it is also what makes the nil case safe
// to leave wired in production paths that have no checkout.
func TestRelocationCountWithoutATreeKeepsRawLocators(t *testing.T) {
	at := func(file, job string) db.ReviewFindingObservation {
		return db.ReviewFindingObservation{File: file, ObserverJob: job, RoundLabel: "F1"}
	}
	rows := []db.ReviewFindingObservation{
		at("internal/workflow/a.go:10", "job-1"),
		at("internal/workflow/a.go:20", "job-2"),
		at("internal/workflow/a.go:30", "job-3"),
	}
	if got := ledgerRelocationBrief(rows, nil, nil); got != "" {
		t.Fatalf("with no tree resolver the key folded on text anyway:\n%s", got)
	}
}

// NEITHER SPELLING TRACKED MEANS KEEP THE RAW LOCATOR. A finding can cite a path
// that no longer exists at this head - a deleted file, a typo, a path from
// another repository - and then the tree cannot decide anything. Stripping on a
// prefix nobody confirmed is round seven's guess with extra steps, so the key
// must under-report instead.
func TestRelocationCountKeepsLocatorsTheTreeCannotResolve(t *testing.T) {
	at := func(file, job string) db.ReviewFindingObservation {
		return db.ReviewFindingObservation{File: file, ObserverJob: job, RoundLabel: "F1"}
	}
	unknown := []db.ReviewFindingObservation{
		at("deleted/gone.go:10", "job-1"),
		at("deleted/gone.go:20", "job-2"),
		at("deleted/gone.go:30", "job-3"),
	}
	if got := ledgerRelocationBrief(unknown, nil, relocationTestTree); got != "" {
		t.Fatalf("locators the tree cannot resolve were folded on their prefix:\n%s", got)
	}
}

// A RESOLVER ERROR IS NOT AN ANSWER, and it must not read as existence. An
// errored lookup on a PREFIX would otherwise strip the suffix and fold two
// files on a failed git call - a defect that would appear only when the
// checkout was unavailable, which is exactly when nobody is watching.
func TestRelocationPathCheckerTreatsAnErrorAsUnresolved(t *testing.T) {
	engine := Engine{LedgerResolvers: LedgerResolvers{
		PathExistsAtHead: func(_ context.Context, _ string, _ string) (bool, error) {
			return true, errors.New("git cat-file failed")
		},
	}}
	checker := engine.relocationPathChecker(context.Background(), "cafebabecafebabecafebabecafebabecafebabe")
	if checker == nil {
		t.Fatal("a wired resolver produced no checker")
	}
	if checker("internal/workflow/a.go") {
		t.Fatal("an errored lookup reported the path as existing, so a failed git call folds files")
	}

	// AND NO RESOLVER YIELDS NO CHECKER, so the caller keeps raw locators rather
	// than receiving a stub that answers.
	if (Engine{}).relocationPathChecker(context.Background(), "cafebabecafebabecafebabecafebabecafebabe") != nil {
		t.Fatal("an unwired resolver produced a checker, which would answer questions it cannot")
	}
}
