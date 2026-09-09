package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

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
	if !strings.Contains(out, "degraded:") {
		t.Fatalf("an unresolvable instrument produced a clean-looking answer with no disclosure:\n%s", out)
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
