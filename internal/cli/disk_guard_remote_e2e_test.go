//go:build e2e

package cli

import (
	"encoding/json"
	"strings"
	"testing"

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
