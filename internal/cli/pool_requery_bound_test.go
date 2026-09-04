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
	_, errCh := startTrackedWedgeLoop(t, ctx, store, adapter, 2, true, io.Discard)

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

	// Stop the loop HERE rather than relying on cleanup ordering. Restoring
	// poolRequeryInterval while a pool pass is still parked on the shortened
	// timer is safe today only because t.Cleanup runs LIFO — this test's restore
	// was registered before startTrackedWedgeLoop's drain — and because
	// tryBeginPool/endPool bracket the pass in the tracker's WaitGroup so drain
	// genuinely waits for that goroutine. Both are true and neither is obvious,
	// so the ordering is made explicit instead of inherited: cancel, wait for the
	// loop to exit, and only then let the restore run.
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("worker loop did not stop after context cancellation")
	}
}

// TestPoolRequeryObserverIsSilentOnTheBarrierPath pins the DISCRIMINATOR the two
// tests above rely on, rather than trusting it.
//
// poolRequeryObserver is a production hook whose only caller in production is
// nil, so a reader is right to ask what it proves. It proves "the POOL pass's
// bounded wait fired" only if it stays silent when dispatch is NOT on the pool
// path — otherwise the assertions built on it in
// TestPoolPassSeesJobEnqueuedWhileAnotherRuns would pass on a barrier build and
// the pool-path claim would be unfounded again, which is exactly the round-1
// finding. Same scenario, barrier scheduler: the observer must never fire.
func TestPoolRequeryObserverIsSilentOnTheBarrierPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const repo = "owner/repo"
	var wakes int64
	restoreInterval, restoreObserver := poolRequeryInterval, poolRequeryObserver
	poolRequeryInterval = 20 * time.Millisecond
	poolRequeryObserver = func() { atomic.AddInt64(&wakes, 1) }
	t.Cleanup(func() { poolRequeryInterval, poolRequeryObserver = restoreInterval, restoreObserver })

	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, repo)

	adapter := newWedgeBlockingAdapter("job-a")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1})
	// usePool=false: the BARRIER scheduler, which re-queries on its own tick and
	// has no bounded wait to instrument.
	tracker, errCh := startTrackedWedgeLoop(t, ctx, store, adapter, 2, false, io.Discard)

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering; delivered=%v", adapter.deliveredJobs())
	}
	time.Sleep(300 * time.Millisecond)

	if tracker.poolRunning(repo) {
		t.Fatalf("a pool pass is live on the barrier scheduler; the control does not exercise the barrier path")
	}
	if got := atomic.LoadInt64(&wakes); got != 0 {
		t.Fatalf("pool re-query observer fired %d times on the BARRIER path, want 0; the discriminator the pool-path tests rely on does not discriminate", got)
	}

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

// TestPoolIsolationSkipEventIsThrottledAndRetriesBackOff is the production-path
// proof for the two P2s the round-4 review found in the code this PR added.
//
// Scenario, through the same real supervisor wiring as the tests above: job A
// hangs holding the shared repo checkout key, then job B — an ask job with no
// worktree, so isolation-ELIGIBLE — is enqueued behind it. The repo's checkout
// path is a plain directory rather than a git repo, so every
// allocatePoolIsolationWorktree call FAILS with an error, which is precisely the
// state both defects live in.
//
// DESIRED, and both halves are separate mutants:
//  1. exactly ONE pool_isolation_skipped row for B, however many retries happen.
//     FAILS WITHOUT THE FIX: isolationSkipLogged was read and never written, so
//     the "once per job per pass" throttle throttled nothing and the row was
//     written on every retry. Mutation proof: delete the
//     `isolationSkipLogged[job.ID] = true` line and this assertion fails with a
//     row count that tracks the retry count.
//  2. retries are paced by poolIsolationRetryBackoff, NOT by poolRequeryInterval.
//     FAILS WITHOUT THE FIX: each re-query re-ran an allocation that can spend
//     up to workflow.ReadOnlyWorktreeDispatchLockWaitBudget (5s in production)
//     on the checkout mutation lock, so a 2s interval and a 5s lock budget
//     composed into a permanent lock-wait spin. Mutation proof: delete the
//     backoff gate and attempts jump from <=5 to ~window/interval.
//
// The attempt count is read from poolIsolationAttemptObserver rather than from
// the event rows on purpose: the throttle makes the EVENT count 1 either way, so
// an event-based assertion cannot see a retry spin at all — it would be a check
// that passes on the code it is meant to reject.
func TestPoolIsolationSkipEventIsThrottledAndRetriesBackOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const repo = "owner/repo"
	const intervalMS, backoffMS, windowMS = 20, 200, 700
	var attempts int64
	restoreInterval, restoreAttempt := poolRequeryInterval, poolIsolationAttemptObserver
	restoreBackoff, restoreBackoffMax := poolIsolationRetryBackoff, poolIsolationRetryBackoffMax
	poolRequeryInterval = intervalMS * time.Millisecond
	poolIsolationRetryBackoff = backoffMS * time.Millisecond
	poolIsolationRetryBackoffMax = backoffMS * time.Millisecond
	poolIsolationAttemptObserver = func(jobID string) {
		if jobID == "job-b" {
			atomic.AddInt64(&attempts, 1)
		}
	}
	t.Cleanup(func() {
		poolRequeryInterval, poolIsolationAttemptObserver = restoreInterval, restoreAttempt
		poolIsolationRetryBackoff, poolIsolationRetryBackoffMax = restoreBackoff, restoreBackoffMax
	})

	store := daemonWorkerStore(t)
	// A plain directory, NOT a git repo: the read-only worktree allocation fails
	// with an error, which is the allocErr != nil branch under test. A nil
	// allocErr ("not isolable") is a different, quiet path.
	seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, repo)

	adapter := newWedgeBlockingAdapter("job-a")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1})
	// ConfigHome must be non-empty or the allocation short-circuits to "not
	// isolable" with a nil error and this test pins a path that never runs — which
	// is exactly what the first version of it did.
	_, errCh := startTrackedWedgeLoop(t, ctx, store, adapter, 2, true, io.Discard, func(w *jobWorker) {
		w.ConfigHome = t.TempDir()
	})

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering; delivered=%v", adapter.deliveredJobs())
	}
	// Enqueue B only once A genuinely holds the checkout, so B is blocked by
	// contention rather than by arrival order.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 2})
	if !waitForCondition(t, 5*time.Second, func() bool { return atomic.LoadInt64(&attempts) > 0 }) {
		t.Fatal("no isolation attempt was made for job-b; the scenario never reached the isolation path")
	}
	time.Sleep(windowMS * time.Millisecond)

	gotAttempts := atomic.LoadInt64(&attempts)
	// Paced by the backoff the window fits about 700/200 = 3.5, so ~4 with the
	// first attempt. Paced by the re-query interval it would be ~35. The cap is
	// generous enough that a slow box cannot fail it (a timer can only fire FEWER
	// times than expected) and still an order of magnitude below unpaced.
	if maxPaced := int64(windowMS/backoffMS + 2); gotAttempts > maxPaced {
		t.Fatalf("isolation retried %d times in %dms at a %dms backoff, want <= %d (retries are paced by the re-query interval, not by the backoff)", gotAttempts, windowMS, backoffMS, maxPaced)
	}
	events, err := store.ListJobEvents(ctx, "job-b")
	if err != nil {
		t.Fatalf("ListJobEvents(job-b): %v", err)
	}
	skips := 0
	for _, event := range events {
		if event.Kind == "pool_isolation_skipped" {
			skips++
		}
	}
	if skips == 0 {
		t.Fatalf("no pool_isolation_skipped event for job-b after %d isolation attempts; the lost-parallelism serialize is unobservable", gotAttempts)
	}
	if skips != 1 {
		t.Fatalf("pool_isolation_skipped rows for job-b = %d after %d attempts, want exactly 1; the throttle is not writing isolationSkipLogged", skips, gotAttempts)
	}

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

// TestPoolRequeryObserverIsSilentWhileDrainingAfterCancellation pins the third
// round-4 finding: the bounded wait also fired while the pass was DRAINING after
// cancellation, when the top of the loop skips dispatch entirely and no re-query
// follows.
//
// The blocking job is deliberately deaf to context cancellation (ignoreCtx), so
// the pass stays parked in its bounded wait with a worker still in flight long
// after ctx is cancelled — the drain window a ctx-obeying job does not leave.
//
// DESIRED: zero observer fires after cancellation.
//
// FAILS WITHOUT THE FIX: the timer branch called the observer unconditionally,
// so a draining pass reported ~window/interval re-queries it never performed —
// crediting the bound with work it did not do, the same class of false positive
// as the unwritten throttle above.
func TestPoolRequeryObserverIsSilentWhileDrainingAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const repo = "owner/repo"
	const intervalMS, drainMS = 20, 300
	var wakes int64
	restoreInterval, restoreObserver := poolRequeryInterval, poolRequeryObserver
	poolRequeryInterval = intervalMS * time.Millisecond
	poolRequeryObserver = func() { atomic.AddInt64(&wakes, 1) }
	t.Cleanup(func() { poolRequeryInterval, poolRequeryObserver = restoreInterval, restoreObserver })

	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, repo)

	adapter := newWedgeBlockingAdapter("job-a")
	adapter.ignoreCtx = true
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1})
	_, errCh := startTrackedWedgeLoop(t, ctx, store, adapter, 2, true, io.Discard)

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering; delivered=%v", adapter.deliveredJobs())
	}
	// Positive control FIRST: the observer must be firing while the pass is live,
	// otherwise a zero after cancellation proves nothing about the gate.
	if !waitForCondition(t, 5*time.Second, func() bool { return atomic.LoadInt64(&wakes) > 0 }) {
		t.Fatal("bound never fired while the pass was live; a post-cancellation zero would be an instrument failure, not a result")
	}

	cancel()
	// Let the pass observe cancellation before sampling, so the baseline is taken
	// from a pass that is already draining rather than one still dispatching.
	time.Sleep(4 * poolRequeryInterval)
	baseline := atomic.LoadInt64(&wakes)
	time.Sleep(drainMS * time.Millisecond)
	if drained := atomic.LoadInt64(&wakes) - baseline; drained != 0 {
		t.Fatalf("bound fired %d times in %dms while DRAINING after cancellation, want 0 (the wake re-queries nothing once firstErr is set)", drained, drainMS)
	}
	if !adapter.stillBlocked() {
		t.Fatal("job-a stopped blocking; the drain window was not exercised")
	}

	close(adapter.release)
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("worker loop did not stop after the blocking job was released")
	}
}

// TestPoolIsolationPenaltyIsDroppedWhenAJobLeavesTheQueue pins the prune block
// that the reviewer's M5 mutant SURVIVED against the whole package — the
// clearest kind of coverage gap, since disabling the prune broke nothing any
// test asserted.
//
// The prune's OBSERVABLE contract is not "the maps stay small": it is that a job
// which leaves the pending queue and comes back is a FRESH isolation attempt
// rather than a continuation of an old penalty. So this measures the TIMING of
// the retry, not the size of a map — a map-size assertion would have to reach
// inside the pass and would still pass on a build that pruned the wrong entries.
//
// Scenario: job B is blocked and fails isolation, arming a backoff far longer
// than the test's patience. B is then deleted from the queue and re-enqueued
// under the same id. With the prune the new B is attempted promptly; without it
// the stale isolationRetryNext entry suppresses attempts for the full backoff.
func TestPoolIsolationPenaltyIsDroppedWhenAJobLeavesTheQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const repo = "owner/repo"
	const intervalMS, backoffMS = 20, 60000
	var attempts int64
	restoreInterval, restoreAttempt := poolRequeryInterval, poolIsolationAttemptObserver
	restoreBackoff, restoreBackoffMax := poolIsolationRetryBackoff, poolIsolationRetryBackoffMax
	poolRequeryInterval = intervalMS * time.Millisecond
	// A backoff far beyond this test's own timeouts: any attempt observed after
	// the re-enqueue can ONLY come from the penalty having been dropped.
	poolIsolationRetryBackoff = backoffMS * time.Millisecond
	poolIsolationRetryBackoffMax = backoffMS * time.Millisecond
	poolIsolationAttemptObserver = func(jobID string) {
		if jobID == "job-b" {
			atomic.AddInt64(&attempts, 1)
		}
	}
	t.Cleanup(func() {
		poolRequeryInterval, poolIsolationAttemptObserver = restoreInterval, restoreAttempt
		poolIsolationRetryBackoff, poolIsolationRetryBackoffMax = restoreBackoff, restoreBackoffMax
	})

	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, repo)

	adapter := newWedgeBlockingAdapter("job-a")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1})
	_, errCh := startTrackedWedgeLoop(t, ctx, store, adapter, 2, true, io.Discard, func(w *jobWorker) {
		w.ConfigHome = t.TempDir()
	})

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering; delivered=%v", adapter.deliveredJobs())
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 2})
	if !waitForCondition(t, 5*time.Second, func() bool { return atomic.LoadInt64(&attempts) >= 1 }) {
		t.Fatal("no isolation attempt for job-b; the scenario never reached the isolation path")
	}
	// The penalty is now armed for a minute. Prove it BINDS before relying on its
	// release, or the release proves nothing.
	first := atomic.LoadInt64(&attempts)
	time.Sleep(200 * time.Millisecond)
	if again := atomic.LoadInt64(&attempts); again != first {
		t.Fatalf("isolation attempts went %d -> %d while the %dms backoff was armed; the penalty does not bind", first, again, backoffMS)
	}

	// B LEAVES the pending queue (a cancel is the cheapest real transition out of
	// it) and then RETURNS under the same id. The prune runs on a pass that sees
	// B absent, so the penalty must be gone when B comes back.
	if err := store.UpdateJobState(ctx, "job-b", string(workflow.JobCancelled)); err != nil {
		t.Fatalf("UpdateJobState(job-b, cancelled): %v", err)
	}
	// Give the pass at least one re-query with B absent - that is when the prune
	// runs.
	time.Sleep(6 * poolRequeryInterval)
	baseline := atomic.LoadInt64(&attempts)
	if err := store.UpdateJobState(ctx, "job-b", string(workflow.JobQueued)); err != nil {
		t.Fatalf("UpdateJobState(job-b, queued): %v", err)
	}

	if !waitForCondition(t, 5*time.Second, func() bool { return atomic.LoadInt64(&attempts) > baseline }) {
		t.Fatalf("job-b was re-enqueued and never re-attempted within 5s while its backoff was %dms; the stale isolation penalty survived the job leaving the queue, so the prune is not running", backoffMS)
	}

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

// TestPoolIsolationNotIsolablePathIsPacedToo pins the round-1 F-1 fix, and it
// exists because my own comment asserted the opposite. The first remediation
// armed the backoff only when allocErr != nil, on the stated grounds that the
// nil-error "not isolable" verdict was "cheap and stateless to re-test". The
// reviewer measured that claim and it was false: 19 attempts over 400ms, each
// reaching the allocation only after two or three store reads and an admission
// reserve/release pair.
//
// Here worker.ConfigHome is left EMPTY, which is exactly the shape that returns
// (false, nil) — not isolable, no error, no lock wait. The attempts must still
// be paced by the backoff rather than by the re-query interval.
//
// Mutation proof: restrict the arming switch to allocErr != nil again and this
// test fails with an attempt count that tracks the re-query interval.
func TestPoolIsolationNotIsolablePathIsPacedToo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const repo = "owner/repo"
	const intervalMS, backoffMS, windowMS = 20, 200, 700
	var attempts int64
	restoreInterval, restoreAttempt := poolRequeryInterval, poolIsolationAttemptObserver
	restoreBackoff, restoreBackoffMax := poolIsolationRetryBackoff, poolIsolationRetryBackoffMax
	poolRequeryInterval = intervalMS * time.Millisecond
	poolIsolationRetryBackoff = backoffMS * time.Millisecond
	poolIsolationRetryBackoffMax = backoffMS * time.Millisecond
	poolIsolationAttemptObserver = func(jobID string) {
		if jobID == "job-b" {
			atomic.AddInt64(&attempts, 1)
		}
	}
	t.Cleanup(func() {
		poolRequeryInterval, poolIsolationAttemptObserver = restoreInterval, restoreAttempt
		poolIsolationRetryBackoff, poolIsolationRetryBackoffMax = restoreBackoff, restoreBackoffMax
	})

	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, repo)

	adapter := newWedgeBlockingAdapter("job-a")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1})
	// NO ConfigHome override: the allocation short-circuits to "not isolable"
	// with a nil error, which is the branch under test.
	_, errCh := startTrackedWedgeLoop(t, ctx, store, adapter, 2, true, io.Discard)

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering; delivered=%v", adapter.deliveredJobs())
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 2})
	if !waitForCondition(t, 5*time.Second, func() bool { return atomic.LoadInt64(&attempts) > 0 }) {
		t.Fatal("no isolation attempt for job-b; the not-isolable branch was never reached")
	}
	time.Sleep(windowMS * time.Millisecond)

	gotAttempts := atomic.LoadInt64(&attempts)
	if maxPaced := int64(windowMS/backoffMS + 2); gotAttempts > maxPaced {
		t.Fatalf("not-isolable isolation retried %d times in %dms at a %dms backoff, want <= %d (the nil-allocErr branch is still paced by the re-query interval)", gotAttempts, windowMS, backoffMS, maxPaced)
	}

	// A not-isolable job must be QUIET: the skip event is for real failures.
	events, err := store.ListJobEvents(ctx, "job-b")
	if err != nil {
		t.Fatalf("ListJobEvents(job-b): %v", err)
	}
	for _, event := range events {
		if event.Kind == "pool_isolation_skipped" {
			t.Fatalf("a not-isolable job emitted pool_isolation_skipped; that event means a real allocation failure")
		}
	}

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
