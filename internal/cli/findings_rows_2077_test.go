package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2077 f3: THE OBLIGATION BRIEF POINTS AT THIS COMMAND AND IT COULD NOT ANSWER.
//
// ledgerObligationBrief is byte-bounded. Once the budget is exhausted it omits
// mandatory UIDs and tells the reviewer to consult the findings ledger - and
// this command reported per-repository COUNTS only, no rows and no UIDs, while a
// read-only review seat has no database access. The instruction was correct and
// the destination could not satisfy it.
//
// The finding survived three rounds of bounding work on the writer, because the
// missing half was never in the file being edited.

func seedRowsFixture(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)

	const head = "cccccccccccccccccccccccccccccccccccccccc"
	record := func(repo string, pr int64, uid string, state db.FindingState, title string) {
		t.Helper()
		obs := db.ReviewFindingObservation{
			FindingUID: uid, Repo: repo, PullRequest: pr, HeadSHA: head,
			ObserverJob: "local-review-r1", State: state, Severity: "P1", RoundLabel: "r1",
			Title: title, File: "internal/a.go",
			EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
		}
		if _, err := store.RecordReviewFindingObservation(ctx, obs); err != nil {
			t.Fatalf("RecordReviewFindingObservation(%s): %v", uid, err)
		}
	}
	record("owner/repo", 7, "owner/repo#7-f1", db.FindingOpen, "the boundary check is inverted")
	record("owner/repo", 7, "owner/repo#7-f2", db.FindingAnswered, "an earlier concern, since answered")
	record("owner/other", 9, "owner/other#9-f1", db.FindingOpen, "a defect in another repository")
	return home
}

// THE RETRIEVAL PATH. A reviewer sent here by a budgeted brief must be able to
// recover the UID, because continuing a finding requires citing it verbatim -
// typing its label mints a new one instead.
func TestFindingsListsRowsAndUIDsForARepository(t *testing.T) {
	home := seedRowsFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"findings", "--home", home, "--repo", "owner/repo", "--pr", "7"}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings --repo exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"owner/repo#7-f1", "owner/repo#7-f2", "the boundary check is inverted"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing omitted %q, so a reviewer cannot recover it:\n%s", want, out)
		}
	}
	// Scoping is real, not decorative: another repository's obligations must not
	// appear, or the listing cannot be trusted to be the set that blocks this PR.
	if strings.Contains(out, "owner/other#9-f1") {
		t.Fatalf("listing leaked another repository's finding:\n%s", out)
	}
}

// --pr narrows to one pull request, which is what a brief on a specific PR sends
// the reviewer to look up.
func TestFindingsScopesRowsToOnePullRequest(t *testing.T) {
	home := seedRowsFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"findings", "--home", home, "--repo", "owner/repo", "--pr", "7"}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings --pr exit = %d, stderr=%s", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "owner/repo#7-f1") {
		t.Fatalf("listing omitted the PR's own finding:\n%s", out)
	}

	var emptyOut, emptyErr bytes.Buffer
	if code := Run([]string{"findings", "--home", home, "--repo", "owner/repo", "--pr", "999"}, &emptyOut, &emptyErr); code != 0 {
		t.Fatalf("findings --pr 999 exit = %d, stderr=%s", code, emptyErr.String())
	}
	// AN EMPTY RESULT MUST SAY WHICH QUESTION IT ANSWERED. A reviewer who mistypes
	// the PR number and sees a bare "no findings" will conclude the ledger is
	// clear, which is the opposite of the truth.
	if out := emptyOut.String(); !strings.Contains(out, "owner/repo#999") {
		t.Fatalf("empty result did not name the repo and PR it searched:\n%s", out)
	}
}

// EITHER FLAG ALONE IS REFUSED, and the --repo-alone arm is the one that matters:
// the store's reader is keyed on (repo, pull_request), so a repo without a PR
// listed the pull_request=0 rows and printed "no findings recorded" for a
// repository full of them. An empty result for the wrong reason is precisely the
// failure this finding is about, so it is refused rather than rendered.
func TestFindingsRefusesEitherFlagAlone(t *testing.T) {
	home := seedRowsFixture(t)
	for _, args := range [][]string{
		{"findings", "--home", home, "--pr", "7"},
		{"findings", "--home", home, "--repo", "owner/repo"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code == 0 {
			t.Fatalf("%v exited 0; an unpaired flag cannot identify a finding set:\n%s", args, stdout.String())
		} else if !strings.Contains(stderr.String(), "must be given together") {
			t.Fatalf("%v refusal did not say what to supply: %s", args, stderr.String())
		}
	}
}

// #2086 f1: THE LISTING MUST SHOW CURRENT STATE, NOT THE APPEND-ONLY LOG.
//
// The store returns every observation and does not fold - its own doc comment
// says a caller folds. The first version printed one line per observation, so a
// finding appeared as "open" at an old head beside its own "answered" row at a
// newer one. A reviewer sent here by a budgeted brief is asking WHICH
// OBLIGATIONS ARE OPEN, and a stale open row answers that wrongly using the
// ledger's own data - worse than the gap the command was added to close.
func TestFindingsFoldsObservationsToCurrentState(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)

	// A SECOND OBSERVATION OF ONE FINDING NEEDS ContinuesUID. Supplying the same
	// FindingUID does NOT continue it - the store mints a fresh uid, which is how
	// the first version of this fixture silently created two DIFFERENT findings
	// and made the fold look broken when it was the fixture that was wrong.
	record := func(head string, state db.FindingState, kind db.EvidenceKind, continues string) {
		t.Helper()
		obs := db.ReviewFindingObservation{
			ContinuesUID: continues,
			FindingUID:   "owner/repo#7-f1", Repo: "owner/repo", PullRequest: 7, HeadSHA: head,
			ObserverJob: "local-review-" + head[:4], State: state, Severity: "P1",
			Title: "the boundary check is inverted", File: "internal/a.go",
			EvidenceKind: kind, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
		}
		if kind != db.EvidenceExecuted {
			obs.ExecutedCommands, obs.ExecutedCount = nil, 0
			obs.Rationale = "checked by reading"
		}
		if _, err := store.RecordReviewFindingObservation(ctx, obs); err != nil {
			t.Fatalf("RecordReviewFindingObservation(%s): %v", head, err)
		}
	}
	record(strings.Repeat("a", 40), db.FindingOpen, db.EvidenceExecuted, "")
	record(strings.Repeat("b", 40), db.FindingAnswered, db.EvidenceExecuted, "owner/repo#7-f1")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"findings", "--home", home, "--repo", "owner/repo", "--pr", "7"}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if n := strings.Count(out, "owner/repo#7-f1"); n != 1 {
		t.Fatalf("finding appears %d times, want 1; the raw observation log is not current state:\n%s", n, out)
	}
	if !strings.Contains(out, "answered") {
		t.Fatalf("listing shows a state other than the latest:\n%s", out)
	}
	// The stale OPEN row must be gone, not merely outnumbered: its presence is
	// what would send a reviewer to re-answer a discharged obligation.
	if strings.Contains(out, "open") {
		t.Fatalf("a superseded open observation survived the fold:\n%s", out)
	}
}

// THE QUOTED GUARD, which is why the fold reuses the engine's rule rather than
// "last row wins": a later QUOTED observation must not displace one that carried
// real executed evidence, or a listing would report an obligation as discharged
// on the strength of a row that discharges nothing.
func TestFindingsFoldKeepsExecutedEvidenceOverALaterQuotedRow(t *testing.T) {
	folded := workflow.LatestObservationsInOrder([]db.ReviewFindingObservation{
		{FindingUID: "u1", State: db.FindingAnswered, EvidenceKind: db.EvidenceExecuted, Title: "executed"},
		{FindingUID: "u1", State: db.FindingOpen, EvidenceKind: db.EvidenceQuoted, Title: "quoted"},
	})
	if len(folded) != 1 {
		t.Fatalf("fold produced %d rows, want 1", len(folded))
	}
	if folded[0].EvidenceKind != db.EvidenceExecuted {
		t.Fatalf("a later QUOTED row displaced EXECUTED evidence; the listing would disagree with the gate")
	}
}

// #2086 review: --json MUST fold too. The first fix folded after the JSON
// branch, so the machine-readable output - the one a dispatch path consumes -
// still returned the raw append-only log while the human table was correct.
func TestFindingsJSONOutputIsAlsoFolded(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	for i, spec := range []struct {
		head      string
		state     db.FindingState
		continues string
	}{{strings.Repeat("a", 40), db.FindingOpen, ""}, {strings.Repeat("b", 40), db.FindingAnswered, "owner/repo#7-f1"}} {
		obs := db.ReviewFindingObservation{
			ContinuesUID: spec.continues,
			FindingUID:   "owner/repo#7-f1", Repo: "owner/repo", PullRequest: 7, HeadSHA: spec.head,
			ObserverJob: "local-review-r" + strconv.Itoa(i), State: spec.state, Severity: "P1",
			Title: "the boundary check is inverted", File: "internal/a.go",
			EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
		}
		if _, err := store.RecordReviewFindingObservation(ctx, obs); err != nil {
			t.Fatalf("RecordReviewFindingObservation: %v", err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"findings", "--home", home, "--repo", "owner/repo", "--pr", "7", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings --json exit = %d, stderr=%s", code, stderr.String())
	}
	var rows []db.ReviewFindingObservation
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode json: %v\n%s", err, stdout.String())
	}
	if len(rows) != 1 {
		t.Fatalf("--json returned %d rows, want 1; the machine-readable path returned the raw log", len(rows))
	}
	if rows[0].State != db.FindingAnswered {
		t.Fatalf("--json row state = %q, want the latest observation", rows[0].State)
	}
}

// THE PRINTED RULE MUST MATCH THE GATE'S RULE (note 129657, #2102 f8).
//
// THE FIFTH COPY OF THE PRE-129657 RULE WAS THIS ONE, AND IT IS THE ONLY ONE A
// USER SEES. Four earlier copies were code comments; this is stdout. It said
// "OPEN findings block a merge at every later head" with no severity carve-out,
// so a reader following the tool's own explanation would have concluded a P3
// holds their merge.
//
// It had no test, which is why it survived four rounds of fixing the comments.
// A sentence a command PRINTS is a contract with its reader; this pins it.
func TestFindingsSummaryStatesTheSeverityRule(t *testing.T) {
	// The explanation prints beneath the table, so a repository with no findings
	// returns before it. Seed one.
	home := seedRowsFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"findings", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "OPEN P1 and P2 findings block a merge") {
		t.Fatalf("the summary does not name the blocking set:\n%s", out)
	}
	if !strings.Contains(out, "does not hold the merge") {
		t.Fatalf("the summary does not say a P3 cannot block:\n%s", out)
	}
	// AND IT MUST NOT READ AS "A P3 NEEDS NO ANSWER".
	if !strings.Contains(out, "still wants an answer") {
		t.Fatalf("the summary retires the P3 instead of reporting it:\n%s", out)
	}
}
