package cli

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestE2E560LeaseGraceGuardsTeardownWindow is the load-bearing SAFETY E2E for the
// #536-safe layer on top of gaijinjoe's #560 (make stale-running recovery
// configurable + reap expired runtime-session locks). It drives the REAL
// production chain end to end:
//
//   - REAL jobWorker.run() with a codex (resumable) agent, so run() itself sizes
//     and acquires the runtime-session lease from lockTTL = jobTimeout +
//     runtimeLeaseTeardownGrace while arming the run context at exactly jobTimeout.
//   - a fake adapter that BLOCKS inside Deliver, standing in for a live worker that
//     has passed its run-context deadline and is now in terminal teardown (runtime
//     kill + worktree force-clean) while STILL holding its lease.
//   - the REAL recovery chain (runDaemonWorkerTickTracked ->
//     recoverExpiredRuntimeSessionLocks -> DeleteExpiredResourceLocks) against the
//     REAL db.Store, fired at a simulated time INSIDE the teardown grace window:
//     after t0+jobTimeout (context deadline) but before the lease expires.
//
// THE #536 GUARD (load-bearing): that recovery tick must NOT reap the live
// worker's lease and must NOT requeue its still-'running' owner. Reaping+requeuing
// here would start a SECOND worker on the dirty in-flight worktree — the exact
// #536 clobber. This flips RED under the grace=0 mutation (see the package const
// runtimeLeaseTeardownGrace): with no grace the lease expires at t0+jobTimeout,
// strictly before teardown completes, so the reaper deletes it and requeues the
// owner inside the window.
//
// Credit: builds on gaijinjoe's #560.
func TestE2E560LeaseGraceGuardsTeardownWindow(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	// codex is a resumable runtime, so run() acquires a runtime:codex:<ref> session
	// lease we can inspect mid-flight — the lease whose expiry drives #560 recovery.
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-a", []string{"ask"}, "owner/repo")

	const jobTimeout = 10 * time.Minute
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo",
		Branch: "main", PullRequest: 1, JobTimeout: jobTimeout.String(),
	})
	jobSnapshot, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}

	// onDeliver blocks the run inside Deliver (lease held, job 'running') until the
	// test releases it — this is the live-worker-in-teardown window.
	entered := make(chan struct{})
	release := make(chan struct{})
	adapter := &cliWorkerFakeAdapter{
		output: `{"gitmoot_result":{"decision":"approved","summary":"ask ran","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		onDeliver: func() {
			close(entered)
			<-release
		},
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}

	t0 := time.Now().UTC()
	runErr := make(chan error, 1)
	go func() { runErr <- worker.run(ctx, jobSnapshot) }()

	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("adapter Deliver was never entered; run() did not acquire the runtime lease")
	}
	// Guarantee the blocked run is unblocked and joined exactly once even if a later
	// assertion t.Fatalf's (which runs the defer), so the goroutine cannot leak and
	// we never double-close/double-drain.
	released := false
	joined := false
	defer func() {
		if !released {
			released = true
			close(release)
		}
		if !joined {
			joined = true
			<-runErr
		}
	}()

	const lockKey = "runtime:codex:session-a"
	lock, err := store.GetResourceLock(ctx, lockKey)
	if err != nil {
		t.Fatalf("runtime lease not held while run() is mid-Deliver: %v", err)
	}
	leaseExp, perr := time.Parse(time.RFC3339Nano, lock.ExpiresAt)
	if perr != nil {
		t.Fatalf("parse lease expiry %q: %v", lock.ExpiresAt, perr)
	}

	// LOAD-BEARING #536 GUARD (asserted FIRST so the mutation demonstrates the actual
	// requeue): a recovery tick DURING teardown — 30s past the run-context deadline
	// (t0+jobTimeout), i.e. a live worker still force-cleaning its worktree. With the
	// real grace this is well inside the lease (t0+jobTimeout+2m) so the worker is
	// left strictly alone. Under the grace=0 mutation the lease already expired at
	// t0+jobTimeout, so this same tick reaps the live lease and requeues the
	// still-'running' owner — the two assertions below FLIP RED, demonstrating the
	// reopened #536 clobber window (a SECOND worker on the dirty in-flight worktree).
	teardownNow := t0.Add(jobTimeout + 30*time.Second)
	recovery := defaultJobWorker(store, io.Discard)
	if err := runDaemonWorkerTickTracked(ctx, store, recovery, 0, false, "owner/repo", "", io.Discard, teardownNow, nil, nil); err != nil {
		t.Fatalf("recovery tick during teardown grace returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob after teardown tick returned error: %v", err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("job state during teardown grace = %q, want running; the live worker was requeued — the #536 clobber window reopened", job.State)
	}
	if _, err := store.GetResourceLock(ctx, lockKey); err != nil {
		t.Fatalf("live worker's runtime lease was reaped during the teardown grace window: %v", err)
	}

	// SECONDARY INVARIANT (also mutation-proven): the lease must strictly OUTLIVE the
	// run-context deadline (t0+jobTimeout) by a real teardown margin. The threshold
	// is a FIXED 30s (NOT jobTimeout+runtimeLeaseTeardownGrace — that would move with
	// the mutation and never catch it): the lease must exceed jobTimeout by clearly
	// more than the teardown probe above. gotTTL uses t0 as the base (run() acquires
	// a few ms after t0, so gotTTL slightly UNDERstates the real TTL). Under the
	// grace=0 mutation gotTTL collapses to ~jobTimeout (< jobTimeout+30s).
	gotTTL := leaseExp.Sub(t0)
	if gotTTL < jobTimeout+30*time.Second {
		t.Fatalf("runtime lease TTL = %s, want clearly > jobTimeout (%s); the lease does not outlive the teardown window (#536)", gotTTL, jobTimeout)
	}

	// Let the worker finish its (real) run cleanly and assert no error.
	released = true
	close(release)
	joined = true
	if err := <-runErr; err != nil {
		t.Fatalf("run() returned error: %v", err)
	}
	final, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob after run returned error: %v", err)
	}
	if final.State != string(workflow.JobSucceeded) {
		t.Fatalf("final job state = %q, want succeeded", final.State)
	}
}

// TestE2E560ExpiredLeaseRequeuesStuckWorker proves crash recovery still works
// after the grace is added: a genuinely stuck/crashed worker whose runtime lease
// (sized jobTimeout+grace, as run() sizes it) has FULLY elapsed is promptly
// requeued (running -> queued) and its expired lease reaped — well before the
// coarse 30m stale window. This is the #560 recovery path; without it a crashed
// worker's owner would strand in 'running' until the coarse backstop.
func TestE2E560ExpiredLeaseRequeuesStuckWorker(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-stuck", Agent: "audit", Action: "ask", Repo: "owner/repo",
		Branch: "main", PullRequest: 1, JobTimeout: "10m",
	})
	if err := store.UpdateJobState(ctx, "job-stuck", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	now := time.Now().UTC()
	// The lease was sized exactly as run() sizes it (jobTimeout+grace) but the worker
	// crashed and the whole lease has since elapsed — it expired a minute ago.
	leaseTTL := 10*time.Minute + runtimeLeaseTeardownGrace
	acquiredAt := now.Add(-(leaseTTL + time.Minute))
	const lockKey = "runtime:codex:session-stuck"
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey:   lockKey,
		OwnerJobID:    "job-stuck",
		OwnerToken:    "token-stuck",
		OwnerPID:      deadPID(t),
		OwnerHostname: thisHostname(t),
		ExpiresAt:     acquiredAt.Add(leaseTTL).Format(time.RFC3339Nano),
	}, acquiredAt)
	if err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}

	// A recovery tick "now" — far short of the 30m coarse window (the job went
	// running ~now) — must still recover the crashed worker via the expired-lease
	// reaper.
	recovery := defaultJobWorker(store, io.Discard)
	if err := runDaemonWorkerTickTracked(ctx, store, recovery, 0, false, "owner/repo", "", io.Discard, now, nil, nil); err != nil {
		t.Fatalf("recovery tick returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-stuck")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued; a crashed worker with a fully-expired lease must be requeued", job.State)
	}
	if _, err := store.GetResourceLock(ctx, lockKey); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetResourceLock after expired-lease reap = %v, want sql.ErrNoRows", err)
	}
}

// TestE2E560SubFloorStaleWindowRejected pins the env floor (#560 finding 3):
// GITMOOT_STALE_RUNNING_AFTER is a CRASH BACKSTOP, not a timeout, so a sub-floor
// value must be rejected in favor of the default window rather than turned into an
// aggressive killer. The danger is sharpest for NON-resumable runtimes (shell)
// that hold no lease — there the coarse window is the ONLY gate, so a 1s window
// would requeue a healthy worker on its next tick.
func TestE2E560SubFloorStaleWindowRejected(t *testing.T) {
	t.Setenv("GITMOOT_STALE_RUNNING_AFTER", "1s") // below the 1m floor

	seed := func(t *testing.T) (*db.Store, jobWorker, time.Time) {
		store := daemonWorkerStore(t)
		seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
		// shell runtime => no runtime-session lease => the coarse window is the sole gate.
		seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: "job-x", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
		})
		if err := store.UpdateJobState(context.Background(), "job-x", string(workflow.JobRunning)); err != nil {
			t.Fatalf("UpdateJobState returned error: %v", err)
		}
		return store, defaultJobWorker(store, io.Discard), time.Now().UTC()
	}

	// A 5m-old running job: OLDER than the sub-floor 1s (which would requeue it) but
	// well YOUNGER than the enforced 30m default (which leaves it running). Staying
	// 'running' proves the 1s was rejected.
	t.Run("sub_floor_rejected_uses_default_window", func(t *testing.T) {
		ctx := context.Background()
		store, worker, now := seed(t)
		if err := runDaemonWorkerTickTracked(ctx, store, worker, 0, false, "owner/repo", "", io.Discard, now.Add(5*time.Minute), nil, nil); err != nil {
			t.Fatalf("recovery tick returned error: %v", err)
		}
		job, err := store.GetJob(ctx, "job-x")
		if err != nil {
			t.Fatalf("GetJob returned error: %v", err)
		}
		if job.State != string(workflow.JobRunning) {
			t.Fatalf("job state at +5m = %q, want running; the sub-floor 1s window was honored (should fall back to the 30m default)", job.State)
		}
	})

	// Positive control: past the enforced 30m default the SAME job IS recovered, so
	// the floor falls back to a real finite window, not an infinite one.
	t.Run("default_window_still_recovers_when_truly_stale", func(t *testing.T) {
		ctx := context.Background()
		store, worker, now := seed(t)
		if err := runDaemonWorkerTickTracked(ctx, store, worker, 0, false, "owner/repo", "", io.Discard, now.Add(31*time.Minute), nil, nil); err != nil {
			t.Fatalf("recovery tick returned error: %v", err)
		}
		job, err := store.GetJob(ctx, "job-x")
		if err != nil {
			t.Fatalf("GetJob returned error: %v", err)
		}
		if job.State != string(workflow.JobQueued) {
			t.Fatalf("job state at +31m = %q, want queued; the enforced default 30m window must still recover a truly-stale job", job.State)
		}
	})
}
