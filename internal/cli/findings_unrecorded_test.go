package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// tandem#2: the ledger skipped the review's only finding, and this command then
// said "no findings recorded", which reads as "the review found nothing". It
// must say how many reported findings it is not listing, and where to look.
func TestFindingsSaysHowManyFindingsWereNotRecorded(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	payload, err := json.Marshal(map[string]any{"repo": "jerryfane/tandem", "pull_request": 2})
	if err != nil {
		t.Fatal(err)
	}
	const jobID = "local-review-review-router-18e5a038be33e82e-1"
	if err := store.CreateJobWithEvent(ctx,
		db.Job{ID: jobID, Agent: "review-router", Type: "review", State: "succeeded", Payload: string(payload)},
		db.JobEvent{Kind: "findings_ledger_skipped", Message: "finding[0] not recorded to the #1822 ledger: finding is a JSON number, neither an object nor a string"},
	); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"findings", "--home", home, "--repo", "jerryfane/tandem", "--pr", "2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("findings exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"no findings recorded for jerryfane/tandem#2", "1 reported finding(s) were NOT recorded", jobID} {
		if !strings.Contains(out, want) {
			t.Fatalf("output omitted %q:\n%s", want, out)
		}
	}

	// --json keeps stdout the row array a consumer parses; the count goes to stderr.
	var jsonOut, jsonErr bytes.Buffer
	if code := Run([]string{"findings", "--home", home, "--repo", "jerryfane/tandem", "--pr", "2", "--json"}, &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("findings --json exit = %d, stderr=%s", code, jsonErr.String())
	}
	var rows []db.ReviewFindingObservation
	if err := json.Unmarshal(jsonOut.Bytes(), &rows); err != nil {
		t.Fatalf("--json stdout is not the row array: %v\n%s", err, jsonOut.String())
	}
	if !strings.Contains(jsonErr.String(), "1 reported finding(s) were NOT recorded") {
		t.Fatalf("--json stderr omitted the unrecorded count: %q", jsonErr.String())
	}

	// A PR with no skipped findings prints no such line.
	var cleanOut, cleanErr bytes.Buffer
	if code := Run([]string{"findings", "--home", home, "--repo", "jerryfane/tandem", "--pr", "3"}, &cleanOut, &cleanErr); code != 0 {
		t.Fatalf("findings --pr 3 exit = %d", code)
	}
	if strings.Contains(cleanOut.String(), "NOT recorded") {
		t.Fatalf("a PR with nothing skipped reported unrecorded findings:\n%s", cleanOut.String())
	}
}
