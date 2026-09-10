package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/github/githubtest"
)

type mergedUnresolvedClient struct {
	githubtest.NoopClient
	byNumber map[int64]github.PullRequest
}

func (c mergedUnresolvedClient) GetPullRequest(_ context.Context, _ github.Repository, number int64) (github.PullRequest, error) {
	return c.byNumber[number], nil
}

func installMergedUnresolvedClient(t *testing.T, prs map[int64]github.PullRequest) {
	t.Helper()
	previous := newFindingsGitHubClient
	newFindingsGitHubClient = func(string) github.Client { return mergedUnresolvedClient{byNumber: prs} }
	t.Cleanup(func() { newFindingsGitHubClient = previous })
}

// #1971. THE REPORT MUST KEY ON THE BRANCH HEAD, NEVER THE MERGE COMMIT.
//
// Every merge in this repository is a SQUASH, so the merge commit is a commit no
// reviewer ever observed: measured across 28 merged pull requests carrying
// findings, the ledger holds 19 observations at branch heads and ZERO at merge
// commits, and the two SHAs are never equal.
//
// The fixture therefore gives the pull request a merge commit that appears
// nowhere in the ledger. A report keyed on it finds nothing and prints a clean
// list, which is the worst available failure because empty reads as good news.
func TestMergedUnresolvedKeysOnTheBranchHeadNotTheMergeCommit(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	// THE THREE SHAS ARE THE POINT. A finding is recorded at the head a reviewer
	// looked at; the branch then moves and merges. An obligation is a prior
	// finding not re-observed at the head being judged, so the fixture must put
	// the finding at an EARLIER head than the one that merged - which is the real
	// shape, and the reason the live report surfaces #1879 and #1926.
	reviewedHead := strings.Repeat("a", 40)
	branchHead := strings.Repeat("e", 40)
	mergeCommit := strings.Repeat("b", 40)

	store := openCLIJobStore(t, home)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 11, HeadSHA: reviewedHead,
		ObserverJob: "review-1", State: db.FindingOpen, Severity: "P1",
		RoundLabel: "f1", Title: "shipped unresolved", Detail: "a real concern",
		File: "internal/run.go", Line: 1,
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}
	// PRECONDITION: the merge commit must appear nowhere in the ledger, or the
	// fixture cannot tell the two keys apart.
	rows, err := store.ListReviewFindingObservations(ctx, "owner/repo", 11)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	for _, row := range rows {
		if strings.EqualFold(row.HeadSHA, mergeCommit) {
			t.Fatalf("fixture records an observation at the merge commit, so it cannot discriminate the two keys")
		}
	}

	installMergedUnresolvedClient(t, map[int64]github.PullRequest{
		11: {Number: 11, Merged: true, MergedAt: "2026-09-01T00:00:00Z", HeadSHA: branchHead, MergeSHA: mergeCommit},
	})

	var stdout, stderr bytes.Buffer
	if code := runFindings([]string{"--home", home, "--merged-unresolved", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings --merged-unresolved exited %d: %s", code, stderr.String())
	}
	var report findingsMergedUnresolvedReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\nraw: %s", err, stdout.String())
	}
	if len(report.Unresolved) != 1 {
		t.Fatalf("a merged pull request carrying an open obligation was not reported: %+v", report)
	}
	if report.Unresolved[0].HeadSHA != branchHead {
		t.Fatalf("report keyed on %q, want the branch head %q: the merge commit was never observed by any reviewer",
			report.Unresolved[0].HeadSHA, branchHead)
	}
}

// AN UNMERGED PULL REQUEST IS NOT THIS REPORT'S SUBJECT. Without this, a report
// that simply listed every pull request with obligations would pass the test
// above while answering a different question.
func TestMergedUnresolvedIgnoresPullRequestsThatDidNotMerge(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	head := strings.Repeat("c", 40)

	store := openCLIJobStore(t, home)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 12, HeadSHA: head,
		ObserverJob: "review-1", State: db.FindingOpen, Severity: "P1",
		RoundLabel: "f1", Title: "still open on an open PR", Detail: "a real concern",
		File: "internal/run.go", Line: 2,
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}
	installMergedUnresolvedClient(t, map[int64]github.PullRequest{
		12: {Number: 12, State: "open", HeadSHA: head},
	})

	var stdout, stderr bytes.Buffer
	if code := runFindings([]string{"--home", home, "--merged-unresolved", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exited %d: %s", code, stderr.String())
	}
	var report findingsMergedUnresolvedReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if len(report.Unresolved) != 0 {
		t.Fatalf("an OPEN pull request was reported as merged-unresolved: %+v", report.Unresolved)
	}
	// SCANNED IS PART OF THE ANSWER. An empty list from a scan of one pull
	// request and an empty list from a scan of zero read identically otherwise,
	// and the second is an instrument failure wearing a success message.
	if report.ScannedPullRequests != 1 || report.ScannedRepos != 1 {
		t.Fatalf("the report does not say what it scanned, so its empty result is unfalsifiable: %+v", report)
	}
}

// A FORGE ERROR IS UNKNOWN, NOT CLEAN. Skipping silently would let an outage
// render every merged pull request as resolved.
func TestMergedUnresolvedTreatsAForgeErrorAsUnknown(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	store := openCLIJobStore(t, home)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 13, HeadSHA: strings.Repeat("d", 40),
		ObserverJob: "review-1", State: db.FindingOpen, Severity: "P1",
		RoundLabel: "f1", Title: "unknown", Detail: "a real concern",
		File: "internal/run.go", Line: 3,
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}
	previous := newFindingsGitHubClient
	newFindingsGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newFindingsGitHubClient = previous })

	var stdout, stderr bytes.Buffer
	if code := runFindings([]string{"--home", home, "--merged-unresolved"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "degraded:") {
		t.Fatalf("a pull request the forge could not answer for was silently cleared:\n%s", stdout.String())
	}
}

// #2106 f1. THE MOST DAMNING CASE WAS THE ONE THE REPORT COULD NOT SEE.
//
// LedgerObligationsAtHead's dischargedAtHead step removes any finding observed
// AT the head being judged, because the gate asks "what must a NEW review at
// this head still observe" and a row recorded there has been observed. Correct
// for the gate; wrong for this report, which asks what was unresolved WHEN THE
// PR MERGED. A P1 recorded at the exact head that merged produced Merged:1 and
// Unresolved:[] - the report was blindest precisely where the evidence was
// strongest.
//
// The earlier fixture could not catch it: I had put the finding at an EARLIER
// head after my first attempt failed, which made the test pass and encoded the
// gate's semantics as though they were this report's.
func TestMergedUnresolvedSeesAFindingOpenAtTheMergedHead(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	mergedHead := strings.Repeat("7", 40)

	store := openCLIJobStore(t, home)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 21, HeadSHA: mergedHead,
		ObserverJob: "review-1", State: db.FindingOpen, Severity: "P1",
		RoundLabel: "f1", Title: "shipped with this open", Detail: "a real concern",
		File: "internal/run.go", Line: 1,
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}
	// PRECONDITION: the finding must sit at the very head that merged, or this
	// fixture is the earlier one again and proves nothing.
	rows, err := store.ListReviewFindingObservations(ctx, "owner/repo", 21)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(rows) != 1 || !strings.EqualFold(rows[0].HeadSHA, mergedHead) {
		t.Fatalf("fixture does not record the finding at the merged head: %+v", rows)
	}

	installMergedUnresolvedClient(t, map[int64]github.PullRequest{
		21: {Number: 21, Merged: true, MergedAt: "2026-09-01T00:00:00Z", HeadSHA: mergedHead},
	})

	var stdout, stderr bytes.Buffer
	if code := runFindings([]string{"--home", home, "--merged-unresolved", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exited %d: %s", code, stderr.String())
	}
	var report findingsMergedUnresolvedReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\nraw: %s", err, stdout.String())
	}
	if len(report.Unresolved) != 1 || len(report.Unresolved[0].Obligations) != 1 {
		t.Fatalf("a P1 open AT THE MERGED HEAD is invisible to the report: %+v", report)
	}
	if got := report.Unresolved[0].Obligations[0].Reason; !strings.Contains(got, "still open at the merged head") {
		t.Fatalf("reason = %q, want it to distinguish this from a gate obligation", got)
	}
}
