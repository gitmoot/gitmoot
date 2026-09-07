package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1990, found by gm-staged on their own verdict row.
//
// `gitmoot job record --type review --head-sha <H>` accepted the head, echoed it
// back in its own --json, and stored it nowhere in the payload, so the operator
// read their own input back as confirmation. That is why it survived: the CLI
// reported what it was GIVEN, not what it STORED.
//
// Measured on this host: externally-driven review jobs 41, payload head_sha set
// on 0, pull_request set on 41. Engine-dispatched succeeded reviews 3391, head
// set on 3387.
//
// THE ASSERTION IS THE ONE GM-STAGED SPECIFIED, and the distinction is the whole
// test: it compares the JSON against the row RE-READ FROM THE STORE. A test that
// asserted "head_sha is non-null" passes with the echo fully intact.
//
// The quarantine itself is NOT the defect and this test does not ask for it to
// be lifted. A supplied head is caller-asserted, and the codebase types that:
// evidence.SessionReviewGrade is GradeReported, stamped onto every session
// review payload at close time, because "session review inputs are
// caller-supplied, so this can never be stronger than reported". Persisting it
// into the payload would put reported-grade data in the field the merge gate
// reads as system-observed, and an operator could then claim an approval at any
// head. What this pins is that the CLI tells the truth about which plane the
// value landed on.
func TestJobRecordReportsTheStoredRowRatherThanItsOwnFlags(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	store.Close()
	seedConfiguredRole(t, home, "gm-staged")

	const claimedHead = "c1ca9824cf3ab0be2eb1a4d0a6e6cb2a5b40e0f1"
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"job", "record", "--home", home,
		"--acting-role", "gm-staged",
		"--repo", "owner/repo",
		"--type", "review",
		"--decision", "approved",
		"--summary", "exact-head review of the reviewed head",
		"--pr", "1950",
		"--head-sha", claimedHead,
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job record exit = %d, stderr=%s", code, stderr.String())
	}
	var out jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode session output: %v (stdout=%s)", err, stdout.String())
	}

	verify := openCLIJobStore(t, home)
	defer verify.Close()
	job, err := verify.GetJob(context.Background(), out.JobID)
	if err != nil {
		t.Fatalf("GetJob(%q): %v", out.JobID, err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}

	// THE PREMISE, asserted rather than assumed: the payload really does carry no
	// head. If this ever starts failing, the quarantine moved and the rest of this
	// test is reasoning about a world that no longer exists.
	if strings.TrimSpace(payload.HeadSHA) != "" {
		t.Fatalf("payload head_sha = %q, want empty: the quarantine this test is written around has moved", payload.HeadSHA)
	}

	// The reported head must not be presentable as merge evidence: it is reported
	// alongside the plane it actually lives on.
	if out.HeadSHA != claimedHead {
		t.Fatalf("reported head_sha = %q, want the caller's asserted head %q on the display plane", out.HeadSHA, claimedHead)
	}
	if out.HeadSHAPlane != "display" {
		t.Fatalf("reported head_sha_plane = %q, want %q; printing a head with no plane is what let an operator read a display value as a stored one",
			out.HeadSHAPlane, "display")
	}

	// EVERY OTHER FIELD MUST MATCH THE STORED ROW. The echo was wider than the
	// head: decision, severity and summary were all printed from the flags, so any
	// normalisation or refusal between the CLI and the store was invisible in
	// exactly the same way.
	//
	// WHAT THIS LOOP DOES NOT DO, MEASURED RATHER THAN ASSUMED. It is currently
	// NON-DISCRIMINATING: I mutated the production code to feed these three
	// fields from the flags again and the test still passed. Nothing between the
	// CLI and the store transforms them - validateSessionSeverity validates
	// without normalising, and the summary is trimmed before it is stored - so
	// flag-echo and store-read produce byte-identical strings today. It is kept
	// because it costs nothing and it fails the moment any normalisation,
	// defaulting or refusal is introduced, which is precisely when an echo would
	// start lying again. The assertions that DO kill mutants are the head plane
	// and the dispatcher below.
	if payload.Result == nil {
		t.Fatal("stored payload carries no result")
	}
	for _, field := range []struct {
		name     string
		reported string
		stored   string
	}{
		{"decision", out.Decision, payload.Result.Decision},
		{"severity", out.Severity, payload.Result.Severity},
		{"summary", out.Summary, payload.Result.Summary},
		{"state", out.State, job.State},
		{"type", out.Type, job.Type},
	} {
		if field.reported != field.stored {
			t.Errorf("--json %s = %q but the store holds %q; the CLI must report what it stored", field.name, field.reported, field.stored)
		}
	}
	if out.PullRequest != payload.PullRequest {
		t.Errorf("--json pull_request = %d but the store holds %d", out.PullRequest, payload.PullRequest)
	}

	// #1990's second half, and my own overclaim on #1967: OpenExternalJob is a
	// FOURTH insert path around prepareEnqueue, so the dispatcher resolution I
	// merged at the Enqueue chokepoint never reached a session row. Same payload
	// construction, same structural reason, two fields.
	if strings.TrimSpace(job.DispatchedBy) != "gm-staged" {
		t.Errorf("session row dispatched_by = %q, want %q: a session job bypasses the Enqueue chokepoint, so it needs the dispatcher resolved here too",
			job.DispatchedBy, "gm-staged")
	}
}

// A record with no --head-sha must not claim a plane for a value it does not
// have, or the field becomes noise on every session row in the fleet.
func TestJobRecordOmitsTheHeadPlaneWhenNoHeadWasSupplied(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	store.Close()
	seedConfiguredRole(t, home, "gm-staged")

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"job", "record", "--home", home,
		"--acting-role", "gm-staged",
		"--repo", "owner/repo",
		"--type", "review",
		"--decision", "approved",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job record exit = %d, stderr=%s", code, stderr.String())
	}
	var out jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode session output: %v (stdout=%s)", err, stdout.String())
	}
	if out.HeadSHA != "" || out.HeadSHAPlane != "" {
		t.Fatalf("head_sha=%q plane=%q, want both empty when no head was supplied", out.HeadSHA, out.HeadSHAPlane)
	}
}
