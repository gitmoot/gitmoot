package cli

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// A review the pool has dispatched but its worker has not claimed yet still
// holds the other heads of its pull request (#2726), even when a later listing
// drops it: pinned-local A is in pre-claim preparation when the disk enters the
// review warning band, which takes A out of the listing, and E2B review B of
// the same pull request must still not start until A ends.
func TestReviewHoldCountsADispatchedUnclaimedReview(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 4)
	var warning atomic.Bool
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		if warning.Load() {
			return diskFilesystemUsage{TotalBytes: 100 << 30, FreeBytes: 15_000_000_000}, nil
		}
		return diskFilesystemUsage{TotalBytes: 100 << 30, FreeBytes: 50 << 30}, nil
	})
	queueDiskGuardJob(t, ctx, store, "review-a", "rev-omp", "review", `,"head_sha":"`+overlapHeadA+`","exec_backend":"local"`)
	queueDiskGuardJob(t, ctx, store, "review-b", "rev-claude", "review", `,"head_sha":"`+overlapHeadB+`","exec_backend":"remote"`)

	previousInterval, previousRunner := poolRequeryInterval, poolJobRunner
	t.Cleanup(func() { poolRequeryInterval, poolJobRunner = previousInterval, previousRunner })
	poolRequeryInterval = 20 * time.Millisecond
	started := make(chan string, 4)
	releaseA := make(chan struct{})
	poolJobRunner = func(ctx context.Context, worker jobWorker, job db.Job) error {
		started <- job.ID
		if job.ID == "review-a" {
			<-releaseA
		}
		return worker.Store.UpdateJobState(context.Background(), job.ID, string(workflow.JobSucceeded))
	}

	tracker := newInflightJobTracker(ctx)
	defer tracker.drain(io.Discard, 5*time.Second)
	tracker.setRemoteCap(2)
	if !tracker.tryBeginPool("owner/repo") {
		t.Fatal("could not begin the pool pass")
	}
	passDone := make(chan error, 1)
	go func() {
		err := runQueuedJobsForRepoPoolTracked(ctx, worker, 2, 2, "owner/repo", "", tracker)
		tracker.endPool("owner/repo")
		passDone <- err
	}()
	select {
	case id := <-started:
		if id != "review-a" {
			t.Fatalf("first dispatch = %s, want review-a", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("review-a was not dispatched")
	}

	warning.Store(true)
	select {
	case id := <-started:
		t.Fatalf("%s started while review-a of the same pull request was dispatched and not yet claimed", id)
	case <-time.After(500 * time.Millisecond):
	}

	warning.Store(false)
	close(releaseA)
	select {
	case id := <-started:
		if id != "review-b" {
			t.Fatalf("dispatch after review-a ended = %s, want review-b", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("review-b did not start after review-a ended")
	}
	select {
	case err := <-passDone:
		if err != nil {
			t.Fatalf("pool pass: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pool pass did not finish")
	}
}

// Two reviews of one pull request routed to remote in one listing, then one
// running and the other held: the held review no longer counts against the
// routing cap (#2726), so with a cap of two an unrelated review still routes.
func TestReviewHoldLeavesAHeldRoutedReviewOutOfTheRoutingCap(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 2)
	queueDiskGuardJob(t, ctx, store, "review-a", "rev-omp", "review", `,"head_sha":"`+overlapHeadA+`"`)
	queueDiskGuardJob(t, ctx, store, "review-b", "rev-omp", "review", `,"head_sha":"`+overlapHeadB+`"`)
	if got := strings.Join(pendingIDs(t, ctx, worker), ","); got != "review-a,review-b" {
		t.Fatalf("first warning-band listing = %q, want both reviews routed", got)
	}
	for _, id := range []string{"review-a", "review-b"} {
		if b := storedBackend(t, ctx, store, id); b != "remote" {
			t.Fatalf("%s backend = %q, want remote", id, b)
		}
	}
	if claimed, err := store.ClaimRunningJob(ctx, "review-a", string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, 1, "boot"); err != nil || !claimed {
		t.Fatalf("claim review-a: claimed=%v err=%v", claimed, err)
	}
	queueDiskGuardJob(t, ctx, store, "review-c", "rev-omp", "review", `,"pull_request":8,"head_sha":"`+overlapHeadC+`"`)

	if got := strings.Join(pendingIDs(t, ctx, worker), ","); got != "review-c" {
		t.Fatalf("listing with review-a running = %q, want review-c: held review-b must not keep the routing slot", got)
	}
	if c := storedBackend(t, ctx, store, "review-c"); c != "remote" {
		t.Fatalf("review-c backend = %q, want remote", c)
	}
}

// The routing cap covers the whole home, so a held routed review of another
// repository must not keep this repository's review from routing either.
func TestReviewHoldLeavesAnotherReposHeldReviewOutOfTheRoutingCap(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 2)
	for _, job := range []struct{ id, head string }{{"other-a", overlapHeadA}, {"other-b", overlapHeadB}} {
		payload := `{"repo":"other/repo","pull_request":3,"head_sha":"` + job.head + `","exec_backend":"remote"}`
		if err := store.CreateJobWithEvent(ctx, db.Job{ID: job.id, Agent: "rev-omp", Type: "review", State: string(workflow.JobQueued), Payload: payload},
			db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
			t.Fatalf("CreateJobWithEvent %s: %v", job.id, err)
		}
	}
	if claimed, err := store.ClaimRunningJob(ctx, "other-a", string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, 1, "boot"); err != nil || !claimed {
		t.Fatalf("claim other-a: claimed=%v err=%v", claimed, err)
	}
	queueDiskGuardJob(t, ctx, store, "review-c", "rev-omp", "review", `,"head_sha":"`+overlapHeadC+`"`)

	if got := strings.Join(pendingIDs(t, ctx, worker), ","); got != "review-c" {
		t.Fatalf("owner/repo listing = %q, want review-c: other/repo's held review must not keep the routing slot", got)
	}
}

// The serialization preflight counts reviews of one pull request at different
// heads as one parallel slot, as the selector admits one head per pull request.
func TestParallelizableSerialJobsCountsOneHeadPerPullRequest(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-b", "owner/repo", 12, overlapHeadB)
	h.enqueue("review-c", "owner/repo", 12, overlapHeadC)
	if got, _ := parallelizableSerialJobs(h.ctx, h.worker, "owner/repo", ""); got != 0 && got != 1 {
		t.Fatalf("parallelizableSerialJobs = %d with two heads of one pull request, want at most 1", got)
	}
	h.enqueue("review-other", "owner/repo", 13, overlapHeadB)
	if got, _ := parallelizableSerialJobs(h.ctx, h.worker, "owner/repo", ""); got != 2 {
		t.Fatalf("parallelizableSerialJobs = %d, want 2 (one head of PR 12, one of PR 13)", got)
	}
}
