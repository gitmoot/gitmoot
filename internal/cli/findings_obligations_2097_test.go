package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// #2097. STATE IS NOT "DOES THIS STILL BLOCK", AND THE GAP IS NOT COSMETIC.
//
// The existing row listing prints STATE, the last recorded observation. The
// question a merge asks is what the gate still demands AT A HEAD, and the two
// disagree in both directions: an OPEN finding recorded at an earlier head is
// still an obligation at a later one, and an ANSWERED finding becomes mandatory
// again when the diff since the answer touches its relevance keys.
//
// This pins the first direction end to end through the command, because it is
// the one a state filter gets WRONG SILENTLY: a reader filtering rows to
// state=open at this head sees nothing and concludes the PR is clean.
func TestFindingsAtHeadReportsObligationsAStateFilterWouldMiss(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	earlier := strings.Repeat("a", 40)
	later := strings.Repeat("b", 40)

	store := openCLIJobStore(t, home)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 7, HeadSHA: earlier,
		ObserverJob: "review-1", State: db.FindingOpen, Severity: "P1",
		RoundLabel: "f1", Title: "a real defect", Detail: "with a concern",
		File: "internal/run.go", Line: 1,
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}

	// PRECONDITION: no observation exists at the later head. Without this the
	// fixture could pass while proving nothing about the head being a parameter.
	rows, err := store.ListReviewFindingObservations(ctx, "owner/repo", 7)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	for _, row := range rows {
		if strings.EqualFold(row.HeadSHA, later) {
			t.Fatalf("fixture records an observation at the later head, so it cannot exercise the gap")
		}
	}

	var stdout, stderr bytes.Buffer
	if code := runFindings([]string{"--home", home, "--repo", "owner/repo", "--pr", "7", "--at-head", later, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings --at-head exited %d: %s", code, stderr.String())
	}
	var report findingsObligationsReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\nraw: %s", err, stdout.String())
	}
	if len(report.Obligations) != 1 {
		t.Fatalf("obligations at the later head = %+v, want the one recorded at the earlier head: a finding does not stop blocking because the branch moved",
			report.Obligations)
	}
	if report.Obligations[0].Severity != "P1" || !strings.Contains(report.Obligations[0].Reason, "still open") {
		t.Fatalf("obligation = %+v, want the P1 with a reason a reader can act on", report.Obligations[0])
	}
}

// A DEGRADATION MUST REACH THE CALLER (#2086 f3's rule, applied to this mode).
//
// LedgerScope degrades rather than failing: with no changed-file resolver,
// answered findings stay advisory and simply do not appear. That UNDER-reports,
// and an empty list from a working instrument is indistinguishable from an empty
// list from an instrument that could not look - the empty one reading as good
// news. The engine records these as task events; a CLI has no task, so they must
// be printed or they are lost.
func TestFindingsAtHeadDisclosesWhatItCouldNotResolve(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	earlier := strings.Repeat("c", 40)
	later := strings.Repeat("d", 40)

	store := openCLIJobStore(t, home)
	// An ANSWERED finding is the row whose disposition depends on a diff the CLI
	// cannot resolve without a checkout, so it is the row that goes silent.
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 8, HeadSHA: earlier,
		ObserverJob: "review-1", State: db.FindingAnswered, Severity: "P2",
		RoundLabel: "f1", Title: "answered once", Detail: "and possibly re-armed",
		File: "internal/run.go", Line: 2, RelevanceKeys: []string{"internal/run.go"},
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runFindings([]string{"--home", home, "--repo", "owner/repo", "--pr", "8", "--at-head", later}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings --at-head exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	// #2099 f2: ASSERT THE SPECIFIC NOTE, NOT THAT SOME NOTE EXISTS.
	//
	// The first version checked only for the substring "degraded:", so ANY note
	// satisfied it - including the eager "no checkout" note, which was emitted
	// unconditionally and whose claim about answered findings was often FALSE
	// because github.NewClient("") still installs the compare-API resolver. A
	// test that accepts any disclosure cannot tell a correct disclosure from a
	// wrong one, which is the same defect as accepting any non-empty result.
	// EITHER accurate disclosure is acceptable and the choice is not this test's
	// business: the resolver may be absent, or present and unable to answer. What
	// is NOT acceptable is silence, or a note asserting a consequence it cannot
	// know. Measured on this fixture the scope emits the second form - "changed
	// files between X and Y unavailable" - because github.NewClient("") installs
	// a compare-API resolver that then 404s, which is exactly the case #2099 f2
	// said the old eager note described wrongly.
	if !strings.Contains(out, "no changed-file resolver") && !strings.Contains(out, "changed files between") {
		t.Fatalf("the answered finding was omitted without saying WHY it could not be evaluated:\n%s", out)
	}
	// And the note must not claim more than it knows: the checkout note states
	// the missing checkout, never the consequence it used to assume.
	if strings.Contains(out, "no registered checkout") && strings.Contains(out, "answered findings are advisory") {
		t.Fatalf("the checkout note still asserts a resolver consequence it cannot know:\n%s", out)
	}
}

// #2099 f1. AN ADVISORY REPOSITORY IS NOT BLOCKED BY ITS OWN WAIVED OBLIGATIONS.
//
// The merge gate resolves findings_consumption onto the LedgerScope, and
// EnsureLedgerObligationsObserved then returns nil for exactly these pending
// obligations. Printing them without the declaration reports an advisory
// repository as blocked by rows its own gate waives - the CLI answering
// differently from the thing that blocks, which is the one property this command
// exists to guarantee.
//
// The obligations stay LISTED, because advisory means recorded and not held,
// never invisible (#1969). What the declaration changes is the sentence.
func TestFindingsAtHeadReportsAnAdvisoryRepositoryAsWaived(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	earlier := strings.Repeat("1", 40)
	later := strings.Repeat("2", 40)

	writeFindingsAdvisoryConfig(t, home, "owner/repo")
	store := openCLIJobStore(t, home)
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "owner/repo", PullRequest: 9, HeadSHA: earlier,
		ObserverJob: "review-1", State: db.FindingOpen, Severity: "P1",
		RoundLabel: "f1", Title: "a real defect", Detail: "with a concern",
		File: "internal/run.go", Line: 1,
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./..."}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runFindings([]string{"--home", home, "--repo", "owner/repo", "--pr", "9", "--at-head", later, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings --at-head exited %d: %s", code, stderr.String())
	}
	var report findingsObligationsReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\nraw: %s", err, stdout.String())
	}
	if !report.Advisory {
		t.Fatalf("the repository declares findings_consumption = advisory and the report does not say so: %+v", report)
	}
	// STILL LISTED. Advisory waives the hold, not the record.
	if len(report.Obligations) != 1 {
		t.Fatalf("advisory dropped the obligation instead of waiving it: %+v", report.Obligations)
	}
}

// The refusal must NAME THE ACCEPTED COMBINATION. Four flags now form a validity
// matrix, and a refusal saying only that the input is invalid leaves the caller
// to guess which flag to move (#2086 f3).
func TestFindingsAtHeadRefusalNamesTheAcceptedCombination(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runFindings([]string{"--home", t.TempDir(), "--at-head", strings.Repeat("e", 40)}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	message := stderr.String()
	for _, want := range []string{"--at-head", "--repo", "--pr"} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal does not name %q, so the caller cannot tell what to supply: %s", want, message)
		}
	}
}

// writeFindingsAdvisoryConfig declares the repository advisory in the home's
// config, the same file loadReviewConfig reads in production.
func writeFindingsAdvisoryConfig(t *testing.T, home, repo string) {
	t.Helper()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := "[review]\nblocking_severity = \"P2\"\n\n[repos.\"" + repo + "\".review]\nfindings_consumption = \"advisory\"\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// PRECONDITION: the fixture must actually declare advisory through the same
	// loader production uses, or the test passes for the wrong reason.
	if !loadReviewConfig(home).For(repo).FindingsAreAdvisory() {
		t.Fatalf("fixture config does not read back as advisory for %s, so this test cannot exercise the waiver", repo)
	}
}
