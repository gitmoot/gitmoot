package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// TestJobListSurfacesWithheldByLockHolder is #1553's half, handled on this same
// pass per the issue: a job withheld because another job holds its repo resource
// also renders as bare `queued`, and surfacing only the quota half would look to
// the next reader like the gap had closed.
func TestJobListSurfacesWithheldByLockHolder(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	seedQueuedJob(t, store, "withheld-by-lock", workflow.JobPayload{
		Repo:        "owner/repo",
		PullRequest: 9,
	})
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: "checkout:owner/repo",
		OwnerJobID:  "implement-holding-repo",
		OwnerToken:  "implement-holding-repo-token",
		ExpiresAt:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("AcquireResourceLock: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock reported not acquired; the withheld arm would prove nothing")
	}
	store.Close()

	line := jobListLine(t, home, "withheld-by-lock")
	if !strings.Contains(line, "implement-holding-repo") {
		t.Errorf("job list line = %q, want the holding job named; #1553's cause must not stay invisible", line)
	}

	entry := jobListJSONEntry(t, home, "withheld-by-lock")
	if !strings.Contains(entry.WhyStuck, "implement-holding-repo") {
		t.Errorf("--json why_stuck = %q, want the holding job named", entry.WhyStuck)
	}
}

// TestJobListOrdinaryQueuedJobRenderingIsUnchanged is the control, and without it
// the three tests above would be satisfied by annotating every queued row. A
// queued job with no blocker payload and no lock must keep rendering exactly as
// it does today: the zero value of stuckReason means "no derivable reason" and
// healthy output has to stay byte-stable.
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
