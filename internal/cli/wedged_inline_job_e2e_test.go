package cli

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// wedgeBlockingAdapter is a delivery adapter whose Deliver BLOCKS for the job
// named in blockJob until release is closed (or the job's ctx is cancelled),
// standing in for a hung/very-long runtime subprocess. Every other job returns a
// successful ask result immediately. It records delivery order and lets the test
// observe (a) that the blocking job is genuinely in flight and (b) whether a
// second job was delivered WHILE the first one was still blocked.
type wedgeBlockingAdapter struct {
	mu        sync.Mutex
	blockJob  string
	release   chan struct{}
	delivered []string
	blocked   bool
	output    string
}

func newWedgeBlockingAdapter(blockJob string) *wedgeBlockingAdapter {
	return &wedgeBlockingAdapter{
		blockJob: blockJob,
		release:  make(chan struct{}),
		output:   poolSchedulerAskResult,
	}
}

func (a *wedgeBlockingAdapter) Name() string { return "wedge-fake" }

func (a *wedgeBlockingAdapter) Start(context.Context, runtime.StartRequest) (runtime.StartResult, error) {
	return runtime.StartResult{RuntimeRef: "550e8400-e29b-41d4-a716-446655440000"}, nil
}

func (a *wedgeBlockingAdapter) Validate(context.Context, runtime.Agent) error { return nil }

func (a *wedgeBlockingAdapter) Deliver(ctx context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	a.mu.Lock()
	a.delivered = append(a.delivered, job.ID)
	blocking := job.ID == a.blockJob
	if blocking {
		a.blocked = true
	}
	a.mu.Unlock()
	if blocking {
		select {
		case <-a.release:
		case <-ctx.Done():
			a.mu.Lock()
			a.blocked = false
			a.mu.Unlock()
			return runtime.Result{}, ctx.Err()
		}
		a.mu.Lock()
		a.blocked = false
		a.mu.Unlock()
	}
	return runtime.Result{Raw: a.output}, nil
}

func (a *wedgeBlockingAdapter) Health(context.Context, runtime.Agent) error { return nil }

func (a *wedgeBlockingAdapter) Capabilities(context.Context) ([]string, error) { return nil, nil }

func (a *wedgeBlockingAdapter) stillBlocked() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.blocked
}

func (a *wedgeBlockingAdapter) deliveredJobs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.delivered))
	copy(out, a.delivered)
	return out
}

// waitForJobState polls the store until jobID reaches wantState or the deadline
// elapses, returning the last observed state.
func waitForJobState(t *testing.T, store *db.Store, jobID string, wantState string, deadline time.Duration) string {
	t.Helper()
	stop := time.Now().Add(deadline)
	last := ""
	for time.Now().Before(stop) {
		job, err := store.GetJob(context.Background(), jobID)
		if err == nil {
			last = job.State
			if last == wantState {
				return last
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return last
}

// waitForCondition polls fn until it returns true or the deadline elapses.
func waitForCondition(t *testing.T, deadline time.Duration, fn func() bool) bool {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fn()
}

// TestWedgedInlineJobE2E is the #562 end-to-end reproduction and fix proof.
//
// Scenario: a single-repo daemon worker loop (the REAL production wiring:
// startSingleRepoWorkerLoop -> startSupervisorWorkerLoopRecovering ->
// runDaemonWorkerTickTracked, exactly what runSingleRepoSupervisor starts) runs
// with 2 workers against a real store. Job A (no worktree, so it occupies the
// shared checkout key) starts and HANGS inside its runtime adapter — a stand-in
// for a hung subprocess or a legitimate multi-hour delegation. While A is still
// in flight, job B — a parallelizable same-repo job with its own worktree, so
// nothing but the scheduler can serialize it — is enqueued.
//
// DESIRED behavior (this test): B is claimed and completes within a bounded
// wait while A is STILL blocked, because job execution runs off the worker-tick
// critical path and later ticks keep claiming queued work.
//
// FAILED ON UNMODIFIED MAIN (the #562 wedge, reproduced before the fix): the
// tick ran jobs INLINE while holding the supervisor checkout lock —
// runQueuedJobsForRepo blocked on wg.Wait() until A returned — so B was never
// claimed (state stayed "queued" for the whole bounded wait) even though the
// daemon stayed alive and polling. Mutation proof: forcing the tracker to nil in
// startSingleRepoWorkerLoop (re-enabling inline execution) turns this test red.
func TestWedgedInlineJobE2E(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const repo = "owner/repo"
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, repo)

	adapter := newWedgeBlockingAdapter("job-a")
	worker := poolSchedulerWorker(t, store, adapter, false)

	// Job A occupies the shared repo checkout (no worktree -> "repo:owner/repo").
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1})

	// Drive the REAL single-repo supervisor worker loop with a fast interval:
	// same closure, same checkout lock discipline, same tick function that
	// runSingleRepoSupervisor wires in production.
	live := newDaemonReloadableConfig(30*time.Second, 2, false)
	var checkoutLock sync.Mutex
	tracker := newInflightJobTracker(ctx)
	defer tracker.drain(io.Discard, 5*time.Second)
	errCh := startSingleRepoWorkerLoop(ctx, 5*time.Millisecond, store, worker, live, &checkoutLock, tracker, repo, "", io.Discard)

	// Wait until A is genuinely in flight (blocked inside its adapter).
	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering; delivered=%v", adapter.deliveredJobs())
	}

	// NOW enqueue B: same repo, distinct worktree -> a distinct checkout key, so
	// only the (wedged) scheduler could keep it from running.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: 2, WorktreePath: filepath.Join(t.TempDir(), "wt-job-b")})

	// THE #562 ASSERTION: B completes within a bounded wait while A still hangs.
	if got := waitForJobState(t, store, "job-b", string(workflow.JobSucceeded), 10*time.Second); got != string(workflow.JobSucceeded) {
		t.Fatalf("job-b state = %q after bounded wait while job-a hangs, want succeeded (the #562 wedge: a stuck inline job starves all queued work)", got)
	}
	if !adapter.stillBlocked() {
		t.Fatalf("job-a unexpectedly finished before job-b was claimed; the scenario did not exercise the wedge")
	}

	// The hung job is still recorded running, not clobbered by B's dispatch.
	if got := waitForJobState(t, store, "job-a", string(workflow.JobRunning), 2*time.Second); got != string(workflow.JobRunning) {
		t.Fatalf("job-a state = %q while blocked, want running", got)
	}

	// Release A; it must complete normally (the fix never abandons the slow job).
	close(adapter.release)
	if got := waitForJobState(t, store, "job-a", string(workflow.JobSucceeded), 10*time.Second); got != string(workflow.JobSucceeded) {
		t.Fatalf("job-a state = %q after release, want succeeded", got)
	}

	// Graceful shutdown: cancel the supervisor ctx; the loop channel must close.
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("worker loop did not stop after context cancellation")
	}
}
