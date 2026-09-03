package cli

import (
	"context"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestPoolPassSeesJobEnqueuedWhileAnotherRuns is the production-path proof for
// the bounded re-query.
//
// Scenario, driven through the REAL single-repo supervisor wiring
// (startTrackedWedgeLoop -> startSingleRepoWorkerLoop ->
// runDaemonWorkerTickTracked) with the POOL scheduler: job A occupies the shared
// repo checkout and hangs inside its adapter. Only AFTER A is genuinely in
// flight is job B enqueued, with its own worktree so its checkout key differs
// and nothing but the scheduler can hold it.
//
// DESIRED: B starts and completes while A still hangs.
//
// FAILS WITHOUT THE FIX: the live pool pass had dispatched nothing on its last
// look and parked on `reap(<-done)`, which only wakes on a COMPLETION, so B was
// invisible for A's remaining lifetime. A replacement pass cannot cover for it
// either — the parked pass still holds poolRuns[repo], so tryBeginPool refuses.
// Mutation proof: replace the select in runQueuedJobsForRepoPoolTracked with the
// old `reap(<-done)` and this test times out with job-b still queued.
//
// This test deliberately enters through the loop rather than calling the
// selector: a test that called selectRunnableQueuedJobsSeeded (or a pool helper)
// directly would pass on unfixed code, because the defect is not in selection —
// it is that nobody re-queries. The barrier scheduler already has this coverage
// in TestWedgedInlineJobE2E; the pool path had none.
func TestPoolPassSeesJobEnqueuedWhileAnotherRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const repo = "owner/repo"
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, repo)

	// Count bound-driven wakes. This is the POOL pass's discriminator: the
	// barrier scheduler has no such wait, so a mutant that routes dispatch away
	// from the pool leaves this at zero even though the barrier would re-query
	// on its own tick and satisfy a bare "job-b eventually ran" assertion.
	var wakes int64
	restoreObserver := poolRequeryObserver
	poolRequeryObserver = func() { atomic.AddInt64(&wakes, 1) }
	t.Cleanup(func() { poolRequeryObserver = restoreObserver })

	adapter := newWedgeBlockingAdapter("job-a")
	// Job A: no worktree -> the shared "repo:owner/repo" checkout key.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1})
	tracker, errCh := startTrackedWedgeLoop(t, ctx, store, adapter, 2, true, io.Discard)

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering; delivered=%v", adapter.deliveredJobs())
	}
	// A POOL pass must own dispatch for this repo. Without this the test passes
	// on a build where dispatch fell back to the barrier, which is a mutant a
	// reviewer killed in round 1 (`if worker.UsePool` -> `if false`).
	if !waitForCondition(t, 5*time.Second, func() bool { return tracker.poolRunning(repo) }) {
		t.Fatalf("no pool pass is live for %s; dispatch is not on the pool path this test claims to pin", repo)
	}
	// The pass has now dispatched A, found nothing else, and is waiting. Enqueue
	// B only at this point, so its arrival is strictly after the wait began.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 2, WorktreePath: filepath.Join(t.TempDir(), "wt-job-b")})

	// Bounded wait: comfortably past poolRequeryInterval, far short of "until A
	// finishes" (A never finishes on its own in this test).
	if got := waitForJobState(t, store, "job-b", string(workflow.JobSucceeded), 15*time.Second); got != string(workflow.JobSucceeded) {
		t.Fatalf("job-b state = %q while job-a hangs, want succeeded within the re-query bound (a live pool pass must not sleep through a new arrival)", got)
	}
	// ...and it was the POOL pass's bounded wait that noticed, not some other
	// selector: the bound must have fired at least once while job-a hung.
	if got := atomic.LoadInt64(&wakes); got == 0 {
		t.Fatalf("pool re-query bound never fired while job-a hung, so job-b was claimed by some other path; this test must pin the pool seam, not merely observe that something re-queried")
	}
	if !tracker.poolRunning(repo) {
		t.Fatalf("pool pass ended while job-a is still in flight; the dispatch under test did not come from a live pool pass")
	}
	if !adapter.stillBlocked() {
		t.Fatalf("job-a finished before job-b was claimed; the scenario did not exercise the blind wait")
	}
	if got := waitForJobState(t, store, "job-a", string(workflow.JobRunning), 2*time.Second); got != string(workflow.JobRunning) {
		t.Fatalf("job-a state = %q while blocked, want running", got)
	}

	// The slow job is never abandoned by the earlier wake.
	close(adapter.release)
	if got := waitForJobState(t, store, "job-a", string(workflow.JobSucceeded), 10*time.Second); got != string(workflow.JobSucceeded) {
		t.Fatalf("job-a state = %q after release, want succeeded", got)
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("worker loop did not stop after context cancellation")
	}
}

// TestPoolRequeryBoundIsPacedAndDispatchesNothingWhenIdle is the negative half:
// the bound must not become a spin, and waking early must not invent work.
//
// With one job hung and NOTHING else queued, the pass has no progress available.
// It must wake at the BOUND — not in a tight loop — and dispatch nothing. The
// observer count is the discriminator a wall-clock assertion cannot give: a busy
// loop and a paced one both "take 300ms". Measured kill strengths at this test's
// 20ms interval over a 300ms window (expected ~15 wakes): poolRequeryInterval/8
// produced 92 and poolRequeryInterval/64 — an effectively tight spin — produced
// only 226, NOT the "thousands" an earlier version of this comment claimed,
// because every wake still pays for a store query. A reviewer also measured that
// poolRequeryInterval/4 SURVIVED a 4x cap, so the cap here is 2x expected: a
// slow box can only fire FEWER times than expected, never more.
func TestPoolRequeryBoundIsPacedAndDispatchesNothingWhenIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const repo = "owner/repo"
	var wakes int64
	const intervalMS, windowMS = 20, 300
	restoreInterval, restoreObserver := poolRequeryInterval, poolRequeryObserver
	poolRequeryInterval = intervalMS * time.Millisecond
	poolRequeryObserver = func() { atomic.AddInt64(&wakes, 1) }
	t.Cleanup(func() { poolRequeryInterval, poolRequeryObserver = restoreInterval, restoreObserver })

	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, repo)

	adapter := newWedgeBlockingAdapter("job-a")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1})
	_, _ = startTrackedWedgeLoop(t, ctx, store, adapter, 2, true, io.Discard)

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering; delivered=%v", adapter.deliveredJobs())
	}
	time.Sleep(windowMS * time.Millisecond)

	got := atomic.LoadInt64(&wakes)
	if got == 0 {
		t.Fatalf("bound never fired while a job ran with an empty queue; the pass is still parked on a completion only")
	}
	// Expected wakes ≈ window/interval = 15. The cap is 2x that, tightened from
	// 4x after a reviewer measured that a 4x over-fire (poolRequeryInterval/4 —
	// 500ms in production) SURVIVED the looser bound, so the check could not see
	// a real pacing regression. 2x kills interval/4 (~60 wakes) while staying
	// safe on a slow box, where the timer can only fire FEWER times than
	// expected, never more.
	if maxPaced := int64(windowMS / intervalMS * 2); got > maxPaced {
		t.Fatalf("bound fired %d times in %dms at a %dms interval, want <= %d (re-query is over-firing, not paced)", got, windowMS, intervalMS, maxPaced)
	}
	// Waking early must not manufacture dispatches: A is the only job, and it is
	// still the only delivery.
	if delivered := adapter.deliveredJobs(); len(delivered) != 1 || delivered[0] != "job-a" {
		t.Fatalf("delivered = %v, want only job-a (an idle re-query must dispatch nothing)", delivered)
	}
	if !adapter.stillBlocked() {
		t.Fatalf("job-a finished on its own; the idle-wait scenario was not exercised")
	}

	close(adapter.release)
	if got := waitForJobState(t, store, "job-a", string(workflow.JobSucceeded), 10*time.Second); got != string(workflow.JobSucceeded) {
		t.Fatalf("job-a state = %q after release, want succeeded (the empty-queue path must be unchanged)", got)
	}
}
