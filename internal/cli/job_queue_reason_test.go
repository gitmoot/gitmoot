package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedQueuedJob inserts a queued job carrying the given payload and NO
// reason-bearing event, which is exactly the shape #1887 is about: the deferral
// reason already sits in the payload while the row renders as bare `queued`.
//
// The absent reason event is deliberate and is the whole point. A job whose
// blocker_deferred event is still in its event list already renders a WHY line
// today, so seeding one would make these tests pass against the unfixed code and
// prove nothing.
func seedQueuedJob(t *testing.T, store *db.Store, id string, payload workflow.JobPayload) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJob(context.Background(), db.Job{
		ID:      id,
		Agent:   "lead",
		Type:    "review",
		State:   string(workflow.JobQueued),
		Payload: string(encoded),
		Repo:    payload.Repo,
	}); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

// jobListLine drives the real CLI entry point and returns the row for one job.
// The proof requirement is a test through the render entry point rather than a
// formatting helper, so every assertion below reads what an operator would see.
func jobListLine(t *testing.T, home, jobID string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exit = %d, stderr=%s", code, stderr.String())
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, jobID+"\t") {
			return line
		}
	}
	t.Fatalf("job %s absent from job list output:\n%s", jobID, stdout.String())
	return ""
}

func jobListJSONEntry(t *testing.T, home, jobID string) jobListEntry {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --json exit = %d, stderr=%s", code, stderr.String())
	}
	var entries []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("decode job list --json: %v (stdout=%s)", err, stdout.String())
	}
	for _, entry := range entries {
		if entry.ID == jobID {
			return entry
		}
	}
	t.Fatalf("job %s absent from --json entries", jobID)
	return jobListEntry{}
}

// TestJobListSurfacesDeferralReasonWithRetryTime is #1887's primary case: a
// quota-deferred job must not render as bare `queued` when its payload already
// names the class and the earliest retry the queue gate honors.
func TestJobListSurfacesDeferralReasonWithRetryTime(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "deferred-with-retry", workflow.JobPayload{
		Repo:           "owner/repo",
		PullRequest:    7,
		BlockerClass:   "runtime_quota",
		BlockerRetryAt: "2026-09-06T02:55:00Z",
	})
	store.Close()

	line := jobListLine(t, home, "deferred-with-retry")
	if !strings.Contains(line, "runtime_quota") {
		t.Errorf("job list line = %q, want the blocker class surfaced so a deferred job is distinguishable from plain queued", line)
	}
	if !strings.Contains(line, "2026-09-06T02:55:00Z") {
		t.Errorf("job list line = %q, want the recorded retry time surfaced", line)
	}

	entry := jobListJSONEntry(t, home, "deferred-with-retry")
	if !strings.Contains(entry.WhyStuck, "runtime_quota") {
		t.Errorf("--json why_stuck = %q, want the blocker class; non-human consumers read this surface", entry.WhyStuck)
	}
	if entry.NextRetryAt != "2026-09-06T02:55:00Z" {
		t.Errorf("--json next_retry_at = %q, want the recorded retry time", entry.NextRetryAt)
	}
}

// TestJobListDeferralWithoutRetryTimeClaimsNoTime pins the constraint the issue
// flags as easy to get wrong: 42 of 55 succeeded checkout_contention rows carry a
// null retry_at, so absence is the COMMON case and must render as unknown rather
// than as a zero time.
func TestJobListDeferralWithoutRetryTimeClaimsNoTime(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "deferred-no-retry", workflow.JobPayload{
		Repo:         "owner/repo",
		PullRequest:  8,
		BlockerClass: "checkout_contention",
	})
	store.Close()

	line := jobListLine(t, home, "deferred-no-retry")
	if !strings.Contains(line, "checkout_contention") {
		t.Errorf("job list line = %q, want the blocker class surfaced", line)
	}
	if !strings.Contains(line, "unknown") {
		t.Errorf("job list line = %q, want the missing retry time rendered as unknown", line)
	}
	for _, forbidden := range []string{"0001-01-01", "1970-01-01", "next retry )"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("job list line = %q, must not invent a retry time (%q)", line, forbidden)
		}
	}

	entry := jobListJSONEntry(t, home, "deferred-no-retry")
	if entry.NextRetryAt != "" {
		t.Errorf("--json next_retry_at = %q, want EMPTY rather than a fabricated time", entry.NextRetryAt)
	}
	if !strings.Contains(entry.WhyStuck, "checkout_contention") {
		t.Errorf("--json why_stuck = %q, want the blocker class", entry.WhyStuck)
	}
}

func TestJobListOrdinaryQueuedJobRenderingIsUnchanged(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "plain-queued", workflow.JobPayload{
		Repo:        "owner/repo",
		PullRequest: 10,
	})
	store.Close()

	line := jobListLine(t, home, "plain-queued")
	if strings.Contains(line, "WHY:") {
		t.Errorf("job list line = %q, want NO why-stuck annotation on an ordinary queued job", line)
	}
	for _, forbidden := range []string{"deferred", "withheld", "unknown"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("job list line = %q, must not describe a hold that does not exist (%q)", line, forbidden)
		}
	}

	entry := jobListJSONEntry(t, home, "plain-queued")
	if entry.WhyStuck != "" || entry.NextRetryAt != "" || entry.SuggestedAction != "" {
		t.Errorf("--json entry = %+v, want the why-stuck fields empty for an ordinary queued job", entry)
	}
}

// TestJobListSurfacesTheCausalCheckoutContentionHold is #1553's half, kept but
// moved onto the signal that actually proves it. The daemon pre-flight emits
// "branch %s is locked by %s" (internal/cli/workflow.go:1283),
// job_blocker_checkout.go classifies it as checkout_contention and persists both
// the payload class and a blocker_deferred event. This asserts the withheld job
// no longer renders as a bare `queued` row - the original #1887/#1553 ask - and
// that the holder it names comes from the daemon's own message rather than from a
// lane row this renderer guessed at.
func TestJobListSurfacesTheCausalCheckoutContentionHold(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "causal-hold", workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "task-9",
		PullRequest:  9,
		BlockerClass: "checkout_contention",
	})
	store.Close()

	line := jobListLine(t, home, "causal-hold")
	if strings.Contains(line, "queued\t\t") || !strings.Contains(line, "checkout_contention") {
		t.Fatalf("job list line = %q, want the causal checkout_contention hold, not a bare queued row", line)
	}
	// The absent retry time must still read as unknown, never as a zero time.
	if !strings.Contains(line, "retry time unknown") {
		t.Fatalf("job list line = %q, want the absent retry rendered unknown", line)
	}
}
