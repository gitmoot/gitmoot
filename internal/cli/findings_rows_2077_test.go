package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
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
