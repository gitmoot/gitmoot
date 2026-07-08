package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/jerryfane/gitmoot/internal/github"
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
	if err := dispatchQueuedJobsTracked(ctx, worker, 2, 2, "owner/repo", "", tracker); err != nil {
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
	if err := dispatchQueuedJobsTracked(ctx, worker, 2, 2, "owner/repo", "", tracker); err != nil {
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

	if err := dispatchQueuedJobsTracked(ctx, worker, 2, 2, "owner/repo", "", tracker); err != nil {
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

// TestTrackedDispatchBoundsHostTotalAcrossRepos pins the #562 review fix for
// registered-repo mode: every enabled repo shares ONE tracker, and dispatch is
// clamped by the HOST-global in-flight count, so --workers bounds total
// in-flight jobs across repos. Main's inline sweep gave this for free (one
// repo's blocking batch at a time); per-repo slots alone would allow
// workers × repos concurrent runtime subprocesses. Four parallelizable
// (distinct-worktree) jobs across two repos at workers=2 must never exceed 2
// concurrent deliveries — for the tracked barrier and the tracked pool alike.
func TestTrackedDispatchBoundsHostTotalAcrossRepos(t *testing.T) {
	for _, usePool := range []bool{false, true} {
		name := "Barrier"
		if usePool {
			name = "Pool"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := daemonWorkerStore(t)
			seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
			seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
			seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
			if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
				t.Fatalf("AllowAgentRepo: %v", err)
			}
			concurrency := &poolConcurrencyTracker{}
			adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
			adapter.onDeliver = concurrency.span
			ids := make([]string, 0, 4)
			for i, repo := range []string{"owner/repo-a", "owner/repo-a", "owner/repo-b", "owner/repo-b"} {
				id := fmt.Sprintf("job-%d", i+1)
				ids = append(ids, id)
				enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: i + 1, WorktreePath: filepath.Join(t.TempDir(), "wt-"+id)})
			}
			worker := poolSchedulerWorker(t, store, adapter, usePool)
			tracker := newInflightJobTracker(ctx)
			t.Cleanup(func() { tracker.drain(io.Discard, 5*time.Second) })
			locks := &repoCheckoutLocks{}
			// The REAL registered-repo per-tick sweep, at a fast interval.
			_ = startSupervisorWorkerLoopRecovering(ctx, 2*time.Millisecond, io.Discard, func(now time.Time) error {
				return runEnabledRepoWorkerTicksTracked(ctx, store, worker, 2, "", io.Discard, now, locks, tracker)
			})
			for _, id := range ids {
				if got := waitForJobState(t, store, id, string(workflow.JobSucceeded), 15*time.Second); got != string(workflow.JobSucceeded) {
					t.Fatalf("%s state = %q, want succeeded (usePool=%v)", id, got, usePool)
				}
			}
			if peak := concurrency.peak(); peak > 2 {
				t.Fatalf("host-wide peak concurrency = %d, want <= 2 (--workers must bound TOTAL in-flight jobs across repos, not per repo)", peak)
			}
		})
	}
}

// TestTrackedPoolIsolationHonorsSamePassRuntimeSibling pins the #562 review fix
// for the pool's isolation loop: the loop must consult the LIVE in-flight maps,
// not just the per-pass seed-union copies. A same-runtime-session sibling
// dispatched moments earlier IN THE SAME PASS is invisible in the stale copies
// (and its DB session lock may not be acquired yet), so before the fix the
// contended no-worktree job could be isolation-dispatched beside it — the loser
// of the session-lock race then bounced busy with its payload rewritten to an
// isolation worktree that reap() deletes. Here: while job-wt (session sess-1)
// is in flight, job-iso (same session, blocked on the externally-held shared
// checkout) must stay queued with an untouched payload; once job-wt finishes,
// job-iso must run — via an isolation worktree, since the shared checkout stays
// held — proving the fix holds it back without starving it.
func TestTrackedPoolIsolationHonorsSamePassRuntimeSibling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	// ONE codex agent with a session ref: both jobs share runtime key
	// "runtime:codex:sess-1", so only one may run at a time.
	seedDaemonWorkerAgent(t, store, "coder", runtime.CodexRuntime, "sess-1", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-wt", Agent: "coder", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1, WorktreePath: filepath.Join(t.TempDir(), "wt-job-wt")})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-iso", Agent: "coder", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})

	adapter := newWedgeBlockingAdapter("job-wt")
	worker := poolSchedulerWorker(t, store, adapter, true)
	worker.ConfigHome = home // enable pool isolation worktrees
	tracker := newInflightJobTracker(ctx)
	// An external in-flight job holds the shared checkout for the whole test, so
	// job-iso is always checkout-contended (isolation is its only way to run).
	if !tracker.begin("external-hold", "owner/repo", "repo:owner/repo", "") {
		t.Fatalf("begin(external-hold) refused on a fresh tracker")
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runQueuedJobsForRepoPoolTracked(ctx, worker, 2, 8, "owner/repo", "", tracker) }()

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-wt never started delivering; delivered=%v", adapter.deliveredJobs())
	}
	// While the same-session sibling is in flight, job-iso must NOT be
	// isolation-dispatched: still queued, payload untouched, never delivered.
	time.Sleep(250 * time.Millisecond)
	job, err := store.GetJob(ctx, "job-iso")
	if err != nil {
		t.Fatalf("GetJob job-iso: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job-iso state = %q while its same-session sibling runs, want queued", job.State)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("payload job-iso: %v", err)
	}
	if payload.WorktreePath != "" {
		t.Fatalf("job-iso payload was rewritten to isolation worktree %q beside a live same-session sibling (stale-seed isolation dispatch)", payload.WorktreePath)
	}
	if delivered := adapter.deliveredJobs(); len(delivered) != 1 || delivered[0] != "job-wt" {
		t.Fatalf("delivered = %v, want only job-wt while it holds runtime session sess-1", delivered)
	}

	// Sibling finishes -> the session frees -> job-iso must now run via an
	// isolation worktree (the shared checkout is STILL externally held).
	close(adapter.release)
	if got := waitForJobState(t, store, "job-wt", string(workflow.JobSucceeded), 10*time.Second); got != string(workflow.JobSucceeded) {
		t.Fatalf("job-wt state = %q after release, want succeeded", got)
	}
	if got := waitForJobState(t, store, "job-iso", string(workflow.JobSucceeded), 10*time.Second); got != string(workflow.JobSucceeded) {
		t.Fatalf("job-iso state = %q after the session freed, want succeeded (held back forever instead of isolated)", got)
	}
	job, err = store.GetJob(ctx, "job-iso")
	if err != nil {
		t.Fatalf("GetJob job-iso (final): %v", err)
	}
	if payload, err = daemonJobPayload(job); err != nil || payload.WorktreePath == "" {
		t.Fatalf("job-iso final payload worktree = %q err=%v, want an isolation worktree (must have run beside the held shared checkout)", payload.WorktreePath, err)
	}
	// #739: the reactive isolation success is now OBSERVABLE — a
	// pool_isolation_worktree_allocated event was emitted when job-iso was
	// dispatched into its worktree (it used to be silent both ways).
	if n := countCLIJobEvents(t, store, "job-iso", "pool_isolation_worktree_allocated"); n != 1 {
		t.Fatalf("pool_isolation_worktree_allocated event count = %d, want 1 (#739 observability)", n)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("pool pass returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pool pass did not return after the queue drained")
	}
	tracker.end("external-hold")
	tracker.drain(io.Discard, 5*time.Second)
}

// TestTrackedTickMaintenanceNotStarvedByInFlightJobs pins the #562 review fix
// for tick maintenance: it must not be gated on WHOLE-repo idleness, which a
// steady staggered backlog can prevent indefinitely (main's blocking barrier
// guaranteed an idle point between batches). While an in-flight job holds the
// shared repo checkout:
//   - a transiently-failed result comment IS retried (comments never touch a
//     checkout),
//   - a pending advancement whose own checkout key is FREE (its worktree) IS
//     retried,
//   - a pending advancement that would mutate the HELD shared checkout is
//     skipped (the safety half) — and retried once the holder finishes.
func TestTrackedTickMaintenanceNotStarvedByInFlightJobs(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")

	mustCreateTerminalJob := func(id string, state string, payload workflow.JobPayload, extraEvents ...db.JobEvent) {
		t.Helper()
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("Marshal %s: %v", id, err)
		}
		if err := store.CreateJobWithEvent(ctx, db.Job{ID: id, Agent: "reviewer", Type: "review", State: state, Payload: string(encoded)}, db.JobEvent{JobID: id, Kind: state, Message: "seed"}); err != nil {
			t.Fatalf("CreateJobWithEvent %s: %v", id, err)
		}
		for _, event := range extraEvents {
			event.JobID = id
			if err := store.AddJobEvent(ctx, event); err != nil {
				t.Fatalf("AddJobEvent %s: %v", id, err)
			}
		}
	}
	approved := &workflow.AgentResult{Decision: "approved", Summary: "approved"}
	// Failed result comment awaiting retry (posting is GitHub-API-only).
	mustCreateTerminalJob("job-comment", string(workflow.JobFailed),
		workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 7},
		db.JobEvent{Kind: "comment_post_failed", Message: "temporary github error"})
	// Pending advancement keyed on its OWN worktree (free while job-live runs).
	mustCreateTerminalJob("job-adv-wt", string(workflow.JobSucceeded),
		workflow.JobPayload{Repo: "owner/repo", Branch: "task-wt", PullRequest: 1, TaskID: "task-wt", WorktreePath: filepath.Join(t.TempDir(), "wt-adv"), Result: approved},
		db.JobEvent{Kind: "advance_started", Message: "workflow advancement started"})
	// Pending advancement keyed on the SHARED repo checkout (held by job-live).
	mustCreateTerminalJob("job-adv-repo", string(workflow.JobSucceeded),
		workflow.JobPayload{Repo: "owner/repo", Branch: "task-repo", PullRequest: 2, TaskID: "task-repo", Result: approved},
		db.JobEvent{Kind: "advance_started", Message: "workflow advancement started"})

	comments := &cliPollFakeGitHub{}
	gate := &cliWorkerFakeMergeGate{decision: workflow.MergeDecision{Ready: true}}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, MergeGate: gate}
	}
	worker.CommenterFactory = func(string) github.Client {
		return comments
	}

	tracker := newInflightJobTracker(ctx)
	// A long-running in-flight job occupying the shared checkout — on the old
	// whole-repo busy gate this starved ALL maintenance below.
	if !tracker.begin("job-live", "owner/repo", "repo:owner/repo", "") {
		t.Fatalf("begin(job-live) refused on a fresh tracker")
	}

	hasEvent := func(jobID, kind string) bool {
		events, err := store.ListJobEvents(ctx, jobID)
		if err != nil {
			t.Fatalf("ListJobEvents %s: %v", jobID, err)
		}
		return daemonWorkerHasEvent(events, kind)
	}

	if err := runDaemonWorkerTickTracked(ctx, store, worker, 2, false, "owner/repo", "", io.Discard, time.Now().UTC(), tracker, nil); err != nil {
		t.Fatalf("tick (busy repo): %v", err)
	}
	if len(comments.posted) != 1 {
		t.Fatalf("posted comments = %d, want 1 (comment retry starved by an in-flight job)", len(comments.posted))
	}
	if !hasEvent("job-adv-wt", "advance_retried") {
		t.Fatalf("job-adv-wt not advanced while its own worktree key is free (advancement starved by an in-flight job)")
	}
	if hasEvent("job-adv-repo", "advance_retried") {
		t.Fatalf("job-adv-repo advanced while an in-flight job holds the shared checkout it would mutate")
	}

	// Holder finishes -> the shared-checkout advancement is retried next tick.
	tracker.end("job-live")
	if err := runDaemonWorkerTickTracked(ctx, store, worker, 2, false, "owner/repo", "", io.Discard, time.Now().UTC(), tracker, nil); err != nil {
		t.Fatalf("tick (idle repo): %v", err)
	}
	if !hasEvent("job-adv-repo", "advance_retried") {
		t.Fatalf("job-adv-repo still not advanced after the shared checkout freed")
	}
}

// countCLIJobEvents returns how many events of the given kind exist for jobID.
func countCLIJobEvents(t *testing.T, store *db.Store, jobID, kind string) int {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}
