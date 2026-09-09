package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1970 acceptance 3 and #1969's third criterion, answered by one report.
//
// Before this there was no findings command at all, so "271 open findings" was
// a number nobody could act on: it conflated a defect reported against the head
// under review with one nobody had looked at for twenty heads, and it hid a
// repository that had recorded 59 findings and answered none.
//
// THE PARTITION IS THE POINT, and so is what it refuses to do. #1970 proposed
// auto-superseding open findings when the head moves. Measured through
// LedgerObligationsAtHead, that drops a live obligation whenever the new push
// does not touch the finding's file: an open finding is an obligation
// unconditionally, while a superseded one is re-armed only by relevance. So
// this report splits the open count by head currency and leaves the state
// alone. Both buckets still block.
func TestFindingsReportPartitionsOpenFindingsByHeadCurrency(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)

	const currentHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const staleHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/consuming", Number: 7, URL: "u", HeadBranch: "b",
		BaseBranch: "main", HeadSHA: currentHead, State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	// A repository with a recorded PR whose head has moved on, plus one whose PR
	// row is absent entirely: the third bucket exists so an unknown head is not
	// silently counted as current.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/silent", Number: 29, URL: "u", HeadBranch: "b",
		BaseBranch: "main", HeadSHA: currentHead, State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}

	record := func(repo string, pr int64, head string, state db.FindingState, round string) {
		t.Helper()
		obs := db.ReviewFindingObservation{
			Repo: repo, PullRequest: pr, HeadSHA: head, ObserverJob: "local-review-" + round,
			State: state, Severity: "P2", RoundLabel: round,
			Title: "a defect worth reading", File: "internal/a.go",
			EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
		}
		if state == db.FindingWithdrawn {
			obs.WithdrawReason = "not a defect after all"
		}
		if _, err := store.RecordReviewFindingObservation(ctx, obs); err != nil {
			t.Fatalf("RecordReviewFindingObservation(%s/%s): %v", repo, round, err)
		}
	}

	// The consuming repository: one open at the current head, one open at a stale
	// head, one answered.
	record("owner/consuming", 7, currentHead, db.FindingOpen, "c1")
	record("owner/consuming", 7, staleHead, db.FindingOpen, "c2")
	record("owner/consuming", 7, currentHead, db.FindingAnswered, "c3")
	// The silent repository: findings recorded, nothing ever answered - the
	// #1969 shape the report has to make visible without a manual query.
	record("owner/silent", 29, staleHead, db.FindingOpen, "s1")
	record("owner/silent", 29, staleHead, db.FindingOpen, "s2")
	// A finding on a pull request with no locally recorded row at all.
	record("owner/silent", 999, staleHead, db.FindingOpen, "s3")
	store.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"findings", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings exit = %d, stderr=%s", code, stderr.String())
	}
	var rows []db.ReviewFindingConsumption
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode report: %v (stdout=%s)", err, stdout.String())
	}
	byRepo := map[string]db.ReviewFindingConsumption{}
	for _, row := range rows {
		byRepo[row.Repo] = row
	}

	consuming := byRepo["owner/consuming"]
	if consuming.Open != 2 || consuming.OpenAtCurrentHead != 1 || consuming.OpenAtEarlierHead != 1 {
		t.Errorf("owner/consuming open=%d at_head=%d at_earlier=%d, want 2/1/1; a single open total cannot tell a fresh defect from a twenty-head-old one",
			consuming.Open, consuming.OpenAtCurrentHead, consuming.OpenAtEarlierHead)
	}
	if consuming.Answered != 1 {
		t.Errorf("owner/consuming answered = %d, want 1", consuming.Answered)
	}

	silent := byRepo["owner/silent"]
	if silent.Answered != 0 || silent.Findings != 3 {
		t.Errorf("owner/silent findings=%d answered=%d, want 3/0: this is the shape #1969 exists to surface",
			silent.Findings, silent.Answered)
	}
	if silent.OpenAtEarlierHead != 2 {
		t.Errorf("owner/silent open_at_earlier_head = %d, want 2", silent.OpenAtEarlierHead)
	}
	// THE HONESTY BUCKET. An open finding whose pull request has no recorded head
	// must not be counted as current: that is how a report starts lying about the
	// one column a reader will act on.
	if silent.OpenHeadUnknown != 1 {
		t.Errorf("owner/silent open_head_unknown = %d, want 1; an unknown head folded into either bucket is a false claim",
			silent.OpenHeadUnknown)
	}
	if silent.OpenAtCurrentHead != 0 {
		t.Errorf("owner/silent open_at_current_head = %d, want 0", silent.OpenAtCurrentHead)
	}
}

// The plain-text form has to say that a stale-head finding still blocks, or a
// reader will treat the largest column in the table as expired backlog. That
// misreading is exactly the one that produced #1970's proposed fix.
func TestFindingsReportSaysStaleOpenFindingsStillBlock(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: strings.Repeat("c", 40),
		ObserverJob: "local-review-r1", State: db.FindingOpen, Severity: "P1",
		Title: "unfixed", File: "internal/a.go",
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"findings", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings exit = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "AT EARLIER HEAD") {
		t.Fatalf("report has no head-currency column:\n%s", output)
	}
	// THE DISCRIMINATING CLAUSE, NOT A SHARED FRAGMENT (#2102 f8 follow-up).
	//
	// This assertion used to read Contains("block a merge at every later head"),
	// which is the UNCHANGED MIDDLE of the sentence. When note 129657 inverted the
	// rule for P3 - from "all open findings block" to "a P3 is reported and does
	// not hold the merge" - THE TEST STAYED GREEN IN BOTH WORLDS, while its own
	// failure message claimed to defend the property it could not detect.
	//
	// That is why four rounds of fixing this rule's other copies walked past this
	// file: a green test beside a sentence is a reason not to read the sentence.
	//
	// It now asserts the part that DIFFERS between the two rules. Re-pinning the
	// new full sentence would reproduce the defect one wording later.
	if !strings.Contains(output, "OPEN P1 and P2 findings block a merge at every later head") {
		t.Fatalf("report does not say a stale open P1/P2 still blocks, so its largest column reads as expired:\n%s", output)
	}
	if !strings.Contains(output, "does not hold the merge") {
		t.Fatalf("report does not distinguish a non-blocking P3, so every row reads as blocking:\n%s", output)
	}
}
