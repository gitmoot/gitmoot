package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// syncBuffer is a mutex-guarded bytes.Buffer so loop goroutines can writeLine
// into it while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startTrackedWedgeLoop wires the common scaffolding for the tracked-loop
// regression tests: a real store, one repo + shell agent, the given adapter, and
// the REAL production single-repo worker loop at a fast interval.
func startTrackedWedgeLoop(t *testing.T, ctx context.Context, store *db.Store, adapter workflow.DeliveryAdapter, workers int, usePool bool, stdout io.Writer) (*inflightJobTracker, <-chan error) {
	t.Helper()
	worker := poolSchedulerWorker(t, store, adapter, usePool)
	live := newDaemonReloadableConfig(30*time.Second, workers, usePool)
	var checkoutLock sync.Mutex
	tracker := newInflightJobTracker(ctx)
	t.Cleanup(func() { tracker.drain(io.Discard, 5*time.Second) })
	errCh := startSingleRepoWorkerLoop(ctx, 5*time.Millisecond, store, worker, live, &checkoutLock, tracker, "owner/repo", "", stdout)
	return tracker, errCh
}

// TestTrackedDispatchPreservesSameRepoSerialization pins the #562 preserved
// invariant: two same-repo jobs WITHOUT worktrees share one checkout, so they
// must still never run concurrently — now enforced explicitly by the tracker's
// in-flight checkout-key seeding rather than by accident of inline execution.
// workers=2 proves the serialization comes from the shared key, not the limit.
func TestTrackedDispatchPreservesSameRepoSerialization(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")

	adapter := newWedgeBlockingAdapter("job-a")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	_, _ = startTrackedWedgeLoop(t, ctx, store, adapter, 2, false, io.Discard)

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering")
	}
	// job-b: SAME repo, NO worktree -> same "repo:owner/repo" checkout key.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})

	// Across many ticks, job-b must NOT be claimed while job-a occupies the
	// shared checkout: it stays queued and is never delivered.
	time.Sleep(300 * time.Millisecond)
	if delivered := adapter.deliveredJobs(); len(delivered) != 1 || delivered[0] != "job-a" {
		t.Fatalf("delivered = %v, want only job-a while it holds the shared checkout (same-repo serialization regressed)", delivered)
	}
	job, err := store.GetJob(ctx, "job-b")
	if err != nil {
		t.Fatalf("GetJob job-b: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job-b state = %q while job-a runs on the shared checkout, want queued", job.State)
	}

	// Once job-a releases the checkout, job-b runs to completion.
	close(adapter.release)
	for _, id := range []string{"job-a", "job-b"} {
		if got := waitForJobState(t, store, id, string(workflow.JobSucceeded), 10*time.Second); got != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q after release, want succeeded", id, got)
		}
	}
}

// trackedConcurrencyLimit runs `jobs` parallelizable (distinct-worktree) queued
// jobs through the REAL tracked loop at the given worker limit and scheduler,
// returning the peak concurrent deliveries observed.
func trackedConcurrencyLimit(t *testing.T, jobs, workers int, usePool bool) int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	concurrency := &poolConcurrencyTracker{}
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	adapter.onDeliver = concurrency.span
	ids := make([]string, 0, jobs)
	for i := 0; i < jobs; i++ {
		id := "job-" + strconv.Itoa(i+1)
		ids = append(ids, id)
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: i + 1, WorktreePath: filepath.Join(t.TempDir(), "wt-"+id)})
	}
	_, _ = startTrackedWedgeLoop(t, ctx, store, adapter, workers, usePool, io.Discard)
	for _, id := range ids {
		if got := waitForJobState(t, store, id, string(workflow.JobSucceeded), 15*time.Second); got != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded (usePool=%v workers=%d)", id, got, usePool, workers)
		}
	}
	return concurrency.peak()
}

// TestTrackedDispatchRespectsWorkerLimit pins the #562 preserved invariant that
// --workers still bounds TOTAL in-flight jobs across ticks (not merely one
// tick's batch), for both the tracked barrier and the tracked background pool.
func TestTrackedDispatchRespectsWorkerLimit(t *testing.T) {
	if peak := trackedConcurrencyLimit(t, 4, 2, false); peak > 2 {
		t.Fatalf("barrier: peak concurrency = %d, want <= 2 (workers cap must bound cross-tick in-flight jobs)", peak)
	}
	if peak := trackedConcurrencyLimit(t, 4, 2, true); peak > 2 {
		t.Fatalf("pool: peak concurrency = %d, want <= 2", peak)
	}
}

// TestTrackedLoopShutdownDrainsInFlightJobs pins the #562 graceful-shutdown
// contract: cancelling the supervisor context makes drain() cancel in-flight
// job contexts and wait for them to finish, so daemon stop is clean.
func TestTrackedLoopShutdownDrainsInFlightJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult, waitForContextCancel: true}
	// Cancel only once the job is INSIDE Deliver: tracker.busy flips at begin(),
	// before the worker reaches the adapter, and a cancel landing in that gap
	// aborts worker.run before Deliver so the job never observes cancellation
	// (flaked under -race). Once Deliver has been entered, even an
	// already-cancelled ctx still records the cancellation.
	deliverStarted := make(chan struct{})
	var deliverOnce sync.Once
	adapter.onDeliver = func() { deliverOnce.Do(func() { close(deliverStarted) }) }
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	tracker, errCh := startTrackedWedgeLoop(t, ctx, store, adapter, 1, false, io.Discard)

	select {
	case <-deliverStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("job-a never started delivering")
	}
	if !tracker.busy("owner/repo") {
		t.Fatalf("tracker not busy while job-a is inside Deliver")
	}
	cancel()
	done := make(chan struct{})
	go func() {
		tracker.drain(io.Discard, 10*time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(11 * time.Second):
		t.Fatal("drain did not return after context cancellation")
	}
	if !adapter.observedContextCancel() {
		t.Fatalf("in-flight job never observed cancellation during drain")
	}
	if tracker.busy("owner/repo") {
		t.Fatalf("tracker still busy after drain; in-flight accounting leaked")
	}
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("worker loop did not stop after context cancellation")
	}
}

// TestTrackedBarrierDefersToLivePoolPass pins the single-selector invariant: a
// still-live background pool pass (a warm pool->barrier scheduler flip mid-run,
// #577) owns dispatch for the repo, so the barrier path must refuse until it
// drains — two concurrent selectors could each snapshot the tracker's seeds
// before the other's begin() and put two jobs on one checkout key.
func TestTrackedBarrierDefersToLivePoolPass(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})

	worker := poolSchedulerWorker(t, store, &cliWorkerFakeAdapter{output: poolSchedulerAskResult}, false)
	tracker := newInflightJobTracker(ctx)
	if !tracker.tryBeginPool("owner/repo") {
		t.Fatalf("tryBeginPool refused on an idle tracker")
	}

	// While the pool pass is live, the barrier dispatch must claim nothing.
	if err := dispatchQueuedJobsTracked(ctx, worker, 2, "owner/repo", "", tracker); err != nil {
		t.Fatalf("dispatchQueuedJobsTracked (pool live): %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job-a state = %q while a pool pass is live, want queued (barrier dispatched beside a live pool selector)", job.State)
	}

	// Once the pool pass drains, the same barrier dispatch runs the job.
	tracker.endPool("owner/repo")
	if err := dispatchQueuedJobsTracked(ctx, worker, 2, "owner/repo", "", tracker); err != nil {
		t.Fatalf("dispatchQueuedJobsTracked (pool drained): %v", err)
	}
	if got := waitForJobState(t, store, "job-a", string(workflow.JobSucceeded), 10*time.Second); got != string(workflow.JobSucceeded) {
		t.Fatalf("job-a state = %q after the pool pass drained, want succeeded", got)
	}
	tracker.drain(io.Discard, 5*time.Second)
}

// TestRecoverRunningJobsSkipsInFlightJobs pins the #562 self-requeue guard: a
// running job past the coarse stale window with NO runtime lease (e.g. a long
// shell-runtime job) must NOT be requeued by its own daemon's recovery scan
// while this process is still running it. Inline execution made that impossible
// by construction; the tracked scan must skip in-flight IDs explicitly.
func TestRecoverRunningJobsSkipsInFlightJobs(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-live", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-live", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState: %v", err)
	}
	staleBy := time.Now().UTC().Add(time.Hour)

	// In flight in this process -> skipped, stays running.
	skip := map[string]bool{"job-live": true}
	if err := recoverRunningJobsBeforeForRepoSkipping(ctx, store, io.Discard, staleBy, staleBy, "owner/repo", "", skip); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepoSkipping: %v", err)
	}
	job, err := store.GetJob(ctx, "job-live")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("in-flight job state = %q after recovery scan, want running (scan requeued live work)", job.State)
	}

	// Same scan without the skip (a genuinely crashed worker) -> requeued.
	if err := recoverRunningJobsBeforeForRepoSkipping(ctx, store, io.Discard, staleBy, staleBy, "owner/repo", "", nil); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepoSkipping(nil): %v", err)
	}
	job, err = store.GetJob(ctx, "job-live")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("stale job state = %q, want queued (crash recovery regressed)", job.State)
	}
}

// TestExpiredRuntimeLockReaperSkipsInFlightOwners pins the companion guard: an
// EXPIRED runtime-session lock whose owner is in flight in this process is
// neither requeued nor reaped (its goroutine is alive; releasing the lock could
// double-run the session), while the same lock is recovered normally once the
// owner is no longer in flight.
func TestExpiredRuntimeLockReaperSkipsInFlightOwners(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-live", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	if err := store.UpdateJobState(ctx, "job-live", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState: %v", err)
	}
	now := time.Now().UTC()
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey:   "runtime:codex:session-live",
		OwnerJobID:    "job-live",
		OwnerToken:    "token-live",
		OwnerPID:      int64(os.Getpid()),
		OwnerHostname: "test-host",
		ExpiresAt:     now.Add(-time.Minute).Format(time.RFC3339Nano),
	}, now.Add(-time.Hour))
	if err != nil || !acquired {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", acquired, err)
	}

	skip := map[string]bool{"job-live": true}
	if err := recoverExpiredRuntimeSessionLocksSkipping(ctx, store, io.Discard, now, skip); err != nil {
		t.Fatalf("recoverExpiredRuntimeSessionLocksSkipping: %v", err)
	}
	if job, _ := store.GetJob(ctx, "job-live"); job.State != string(workflow.JobRunning) {
		t.Fatalf("in-flight owner state = %q, want running (reaper requeued live work)", job.State)
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:session-live"); err != nil {
		t.Fatalf("in-flight owner's lock was reaped: %v", err)
	}

	// Without the skip the normal recovery applies: owner requeued, lock reaped.
	if err := recoverExpiredRuntimeSessionLocksSkipping(ctx, store, io.Discard, now, nil); err != nil {
		t.Fatalf("recoverExpiredRuntimeSessionLocksSkipping(nil): %v", err)
	}
	if job, _ := store.GetJob(ctx, "job-live"); job.State != string(workflow.JobQueued) {
		t.Fatalf("stale owner state = %q, want queued", job.State)
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:session-live"); err == nil {
		t.Fatalf("expired lock still present, want reaped")
	}
}

// TestTrackedDispatchLogsHeldBackJobs pins the #562 observability slice: a
// queued job excluded because an in-flight job holds its checkout gets ONE
// throttled explanatory log line (reusing the #552 why-stuck vocabulary), not
// silence and not a line per 1s tick.
func TestTrackedDispatchLogsHeldBackJobs(t *testing.T) {
	resetHeldBackWarnThrottle()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")

	adapter := newWedgeBlockingAdapter("job-a")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	stdout := &syncBuffer{}
	worker := poolSchedulerWorker(t, store, adapter, false)
	worker.Stdout = stdout
	live := newDaemonReloadableConfig(30*time.Second, 2, false)
	var checkoutLock sync.Mutex
	tracker := newInflightJobTracker(ctx)
	t.Cleanup(func() { tracker.drain(io.Discard, 5*time.Second) })
	_ = startSingleRepoWorkerLoop(ctx, 5*time.Millisecond, store, worker, live, &checkoutLock, tracker, "owner/repo", "", io.Discard)

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering")
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})

	const wantLine = "job job-b held back: waiting on checkout repo:owner/repo (held by in-flight job job-a)"
	if !waitForCondition(t, 5*time.Second, func() bool { return strings.Contains(stdout.String(), wantLine) }) {
		t.Fatalf("held-back reason never logged; output=%q", stdout.String())
	}
	// Throttle: many more ticks must not repeat the identical line.
	time.Sleep(200 * time.Millisecond)
	if got := strings.Count(stdout.String(), wantLine); got != 1 {
		t.Fatalf("held-back line logged %d times, want 1 (throttle regressed)", got)
	}
	close(adapter.release)
	if got := waitForJobState(t, store, "job-b", string(workflow.JobSucceeded), 10*time.Second); got != string(workflow.JobSucceeded) {
		t.Fatalf("job-b state = %q after release, want succeeded", got)
	}
}

// TestTrackedDispatchLogsAdmissionNeverFit pins the admission half of the #562
// observability slice: a queued job whose RAM estimate alone exceeds the
// configured [admission] cap can NEVER be admitted — previously a silent
// skip-forever — and now logs a throttled NEVER-fit line naming the cap.
func TestTrackedDispatchLogsAdmissionNeverFit(t *testing.T) {
	resetHeldBackWarnThrottle()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	// A codex agent is session-counted with a 0.2 GB default estimate.
	seedDaemonWorkerAgent(t, store, "coder", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-big", Agent: "coder", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})

	stdout := &syncBuffer{}
	worker := poolSchedulerWorker(t, store, &cliWorkerFakeAdapter{output: poolSchedulerAskResult}, false)
	worker.Stdout = stdout
	// Cap below the smallest estimate: the job can never fit.
	worker.Admission = newAdmissionBudget(config.AdmissionPolicy{MaxMemoryGB: 0.1})
	tracker := newInflightJobTracker(ctx)

	if err := dispatchQueuedJobsTracked(ctx, worker, 2, "owner/repo", "", tracker); err != nil {
		t.Fatalf("dispatchQueuedJobsTracked: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "job job-big held back:") || !strings.Contains(out, "NEVER fit") {
		t.Fatalf("never-fit admission skip not logged; output=%q", out)
	}
	job, err := store.GetJob(ctx, "job-big")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job-big state = %q, want queued (admission skip must leave it queued)", job.State)
	}
}
