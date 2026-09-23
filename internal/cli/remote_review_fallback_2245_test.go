//go:build e2e

package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// requeueToNextGeneration moves the job queued -> running -> queued, which is
// the exact state walk a review model fallback performs, and returns the row as
// the store now holds it (lifecycle_generation bumped by one).
func requeueToNextGeneration(t *testing.T, f *remoteReviewAdmissionFixture, fallbackMarker bool) db.Job {
	t.Helper()
	if _, err := f.store.TransitionJobStateWithEvent(f.ctx, f.job.ID, string(workflow.JobQueued), string(workflow.JobRunning),
		db.JobEvent{JobID: f.job.ID, Kind: string(workflow.JobRunning), Message: "seed first run"}); err != nil {
		t.Fatal(err)
	}
	message := "runtime_quota on anthropic/claude-opus-5; falling back to review model devin/swe-2 on runtime omp (attempt 1/3): seeded"
	if fallbackMarker {
		message = "runtime_quota on anthropic/claude-opus-5; falling back to review model devin/swe-2 on runtime omp (attempt 1/3) lifecycle_generation=1: seeded"
	}
	if _, err := f.store.TransitionJobStateWithEvent(f.ctx, f.job.ID, string(workflow.JobRunning), string(workflow.JobQueued),
		db.JobEvent{JobID: f.job.ID, Kind: reviewModelFallbackEventKind, Message: message}); err != nil {
		t.Fatal(err)
	}
	current, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.LifecycleGeneration != f.job.LifecycleGeneration+1 {
		t.Fatalf("fixture generation = %d, want %d after one requeue", current.LifecycleGeneration, f.job.LifecycleGeneration+1)
	}
	return current
}

func reserveFirstAttempt(t *testing.T, f *remoteReviewAdmissionFixture) {
	t.Helper()
	if err := f.store.ReserveExecBackendAttempt(f.ctx, db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: f.job.ID, Attempt: 1, LifecycleGeneration: f.job.LifecycleGeneration},
		Provider:              "e2b", DaemonFencingToken: "test-fence", BootID: "test-boot",
		TTLExpiresAt: time.Now().UTC().Add(time.Minute), CostReservedUSD: 0.5,
	}, db.ExecBackendCostCap{Configured: true, MaxReservedUSD: 25, PerAttemptUSD: 0.5}); err != nil {
		t.Fatal(err)
	}
}

// #2245, the behaviour half. A review model fallback re-queues a remote review
// onto a different model precisely so that model can run. Before the fix the
// remote retry admission recognised only retry_queued and
// remote_review_provider_retryable markers, so the fallback's new lifecycle was
// refused before provisioning: a remote review could NEVER fall through its pool.
func TestRemoteReviewModelFallbackEarnsTheNewModelACloudAttempt(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	reserveFirstAttempt(t, f)
	f.job = requeueToNextGeneration(t, f, true)

	f.run(t)

	if f.factoryCalls != 1 || f.backend.provisionCalls != 1 {
		t.Fatalf("backend factory/provider calls = %d/%d, want 1/1: the fallback model must get its cloud attempt", f.factoryCalls, f.backend.provisionCalls)
	}
}

// #2245, the spin half. The worker hands admission a job snapshot that can
// predate a re-queue. Before the fix, admission refused "cloud attempt consumed
// for lifecycle_generation=0" against a row already at 1, the refusal's
// generation-anchored CAS matched nothing, and the job stayed queued to be
// picked up and refused again, forever: 152 refusals and 2420 runtime_override
// events on local-review-review-router-18d71359f53e810a-1 until an operator
// cancelled it. The refusal must reach a terminal state even from a stale
// snapshot, and it must do so before any provider is touched.
func TestRemoteReviewRefusalFromStaleSnapshotReachesTerminal(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	stale := f.job
	reserveFirstAttempt(t, f)
	// No marker: the new lifecycle is NOT a sanctioned retry, so admission must
	// refuse it — this test is about what happens to the refusal.
	requeueToNextGeneration(t, f, false)
	f.job = stale

	f.run(t)

	if f.factoryCalls != 0 || f.backend.provisionCalls != 0 {
		t.Fatalf("backend factory/provider calls = %d/%d, want 0/0 for a refused retry", f.factoryCalls, f.backend.provisionCalls)
	}
	after, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != string(workflow.JobFailed) {
		t.Fatalf("job state after refusal = %q, want %q: a refused remote review left queued is re-admitted and refused forever",
			after.State, workflow.JobFailed)
	}
}

// A stale worker must not claim the fallback generation using the old runtime:
// provisioning would then spend the new model's only remote attempt on the old
// adapter. The next scheduler poll gets a fresh row and can use the new model.
func TestRemoteReviewStaleWorkerLeavesFallbackForFreshPoll(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	stale := f.job
	reserveFirstAttempt(t, f)
	current := requeueToNextGeneration(t, f, true)
	payload, err := daemonJobPayload(current)
	if err != nil {
		t.Fatal(err)
	}
	payload.Model = "devin/swe-2"
	payload.RuntimeOverride = "omp"
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateJobPayload(f.ctx, f.job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	f.job = stale
	f.run(t)
	if f.factoryCalls != 0 || f.backend.provisionCalls != 0 {
		t.Fatalf("stale worker spent fallback attempt: factory/provider calls = %d/%d", f.factoryCalls, f.backend.provisionCalls)
	}
	current, err = f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != string(workflow.JobQueued) {
		t.Fatalf("stale worker state = %s, want queued for a fresh poll", current.State)
	}
}
