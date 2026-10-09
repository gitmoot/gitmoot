//go:build e2e

package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// diskRoutedAdmissionFixture is a remote review exactly as the disk guard
// leaves it: remote, green-CI admission required, and marked as switched.
func diskRoutedAdmissionFixture(t *testing.T) *remoteReviewAdmissionFixture {
	t.Helper()
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	payload, err := daemonJobPayload(f.job)
	if err != nil {
		t.Fatal(err)
	}
	payload.PolicyRoutedReview = true
	payload.DiskGuardRouted = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateJobPayload(f.ctx, f.job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	f.job.Payload = string(encoded)
	f.github.checks = []github.PullRequestCheck{{Name: "build", Bucket: "pass"}}
	return f
}

func (f *remoteReviewAdmissionFixture) assertWaitingLocallyAgain(t *testing.T) {
	t.Helper()
	job, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state %s, want queued: a refused disk-guard route must wait, not fail", job.State)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := payload.ExecBackendOverride(); present || payload.DiskGuardRouted || payload.PolicyRoutedReview || !payload.DiskGuardRouteDeclined {
		t.Fatalf("payload after undo %+v: want no backend, not policy-routed, declined", payload)
	}
	events, err := f.store.ListJobEvents(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEventKind(events, diskGuardRouteUndoneEventKind) {
		t.Fatalf("no %s event", diskGuardRouteUndoneEventKind)
	}
}

func TestADiskRoutedReviewWhoseRemoteRunIsRefusedWaitsInsteadOfFailing(t *testing.T) {
	t.Run("red CI", func(t *testing.T) {
		f := diskRoutedAdmissionFixture(t)
		f.github.checks = []github.PullRequestCheck{{Name: "build", Bucket: "fail"}}
		f.run(t)
		if f.backend.provisionCalls != 0 {
			t.Fatal("a red-CI review reached the provider")
		}
		f.assertWaitingLocallyAgain(t)
	})
	t.Run("moved head", func(t *testing.T) {
		f := diskRoutedAdmissionFixture(t)
		f.github.pull.HeadSHA = strings.Repeat("a", 40)
		f.run(t)
		f.assertWaitingLocallyAgain(t)
	})
	t.Run("provider refused after admission", func(t *testing.T) {
		f := diskRoutedAdmissionFixture(t)
		f.run(t)
		if f.backend.provisionCalls != 1 {
			t.Fatalf("provision calls = %d, want 1 (the probe refuses it)", f.backend.provisionCalls)
		}
		f.assertWaitingLocallyAgain(t)
	})
}

// The macserve#13 shape: the guard routes a fresh PR moments after its push,
// while CI is still pending. That refusal must not burn the one switch: the
// review is held briefly, then switched again once CI is green and runs.
func TestADiskRoutedReviewRefusedForPendingCIIsRoutedAgainOnceGreen(t *testing.T) {
	f := diskRoutedAdmissionFixture(t)
	f.github.checks = []github.PullRequestCheck{
		{Name: "Go (macos-latest)", Bucket: "pass"},
		{Name: "Go (ubuntu-latest)", Bucket: "pending"},
	}
	f.run(t)
	if f.backend.provisionCalls != 0 {
		t.Fatal("a pending-CI review reached the provider")
	}
	f.assertRefusedBeforeProvision(t, "ci_pending", 0)
	job, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state %s, want queued", job.State)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := payload.ExecBackendOverride(); present || payload.DiskGuardRouted || payload.PolicyRoutedReview || payload.DiskGuardRouteDeclined {
		t.Fatalf("payload after pending-CI undo %+v: want local, not declined", payload)
	}
	if column := diskGuardBlockerRetryAtColumn(t, f, job); column == "" || column != payload.BlockerRetryAt {
		t.Fatalf("blocker_retry_at column %q payload %q: want the hold persisted in both", column, payload.BlockerRetryAt)
	}

	// Inside the hold the guard does not switch it again.
	if routed := diskGuardRemoteReviews(f.ctx, f.worker, []db.Job{job}, "low disk"); len(routed) != 0 {
		t.Fatalf("held review switched again inside its hold: %+v", routed)
	}

	// The hold expires and CI turns green: the next pass switches it and it runs.
	payload.BlockerRetryAt = time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateJobPayload(f.ctx, job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if job, err = f.store.GetJob(f.ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	f.github.checks = []github.PullRequestCheck{
		{Name: "Go (macos-latest)", Bucket: "pass"},
		{Name: "Go (ubuntu-latest)", Bucket: "pass"},
	}
	routed := diskGuardRemoteReviews(f.ctx, f.worker, []db.Job{job}, "low disk")
	if len(routed) != 1 {
		t.Fatalf("expired-hold green-CI review was not switched again: %d routed", len(routed))
	}
	rerouted, err := daemonJobPayload(routed[0])
	if err != nil {
		t.Fatal(err)
	}
	if !rerouted.DiskGuardRouted || !rerouted.PolicyRoutedReview || rerouted.BlockerRetryAt != "" {
		t.Fatalf("re-routed payload %+v: want routed, policy-routed, hold cleared", rerouted)
	}
	if f.job, err = f.store.GetJob(f.ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	if f.backend.provisionCalls != 1 {
		t.Fatalf("provision calls = %d, want 1: the green re-routed review must reach the provider", f.backend.provisionCalls)
	}
}

// Failing CI and no reported checks are not pending: the route stays declined.
func TestADiskRoutedReviewRefusedForRedOrMissingCIStaysDeclined(t *testing.T) {
	for name, checks := range map[string][]github.PullRequestCheck{
		"failing beside pending": {{Name: "build", Bucket: "fail"}, {Name: "lint", Bucket: "pending"}},
		"none reported":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := diskRoutedAdmissionFixture(t)
			f.github.checks = checks
			f.run(t)
			f.assertRefusedBeforeProvision(t, remoteReviewAvoidedRedCI, 0)
			f.assertWaitingLocallyAgain(t)
			job, err := f.store.GetJob(f.ctx, f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if column := diskGuardBlockerRetryAtColumn(t, f, job); column != "" {
				t.Fatalf("declined review got a retry hold %q", column)
			}
			if routed := diskGuardRemoteReviews(f.ctx, f.worker, []db.Job{job}, "low disk"); len(routed) != 0 {
				t.Fatal("declined review switched again")
			}
		})
	}
}

// The undo is only for the disk guard's own switch: a review a requester sent
// remote keeps today's behavior and fails on a refusal.
func TestARequestedRemoteReviewStillFailsOnRefusal(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	f.github.pull.HeadSHA = strings.Repeat("a", 40)
	f.run(t)
	job, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("requested remote review state %s, want failed", job.State)
	}
}

// diskGuardBlockerRetryAtColumn reads the jobs.blocker_retry_at column (the one
// the stuck/queue projections use), which GetJob does not load.
func diskGuardBlockerRetryAtColumn(t *testing.T, f *remoteReviewAdmissionFixture, job db.Job) string {
	t.Helper()
	raw, err := sql.Open("sqlite", f.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var column string
	if err := raw.QueryRowContext(f.ctx, `SELECT blocker_retry_at FROM jobs WHERE id = ?`, job.ID).Scan(&column); err != nil {
		t.Fatal(err)
	}
	return column
}
