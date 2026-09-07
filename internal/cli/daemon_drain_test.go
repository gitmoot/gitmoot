package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TARGET SPEC FOR #1726's DRAIN SLICE. These tests FAIL on this commit, on
// purpose: they describe the behaviour the fix must produce, and they are
// preserved here rather than re-derived tomorrow.
//
// #1726, and the point of these tests is the PARENTAGE, not the wait.
//
// drain() has existed, been wired at both supervisor entry points, been
// deferred and been bounded for a long time. It did not work, because runCtx
// descended from the supervisor's SIGNAL context: SIGTERM cancelled every
// in-flight job's context before drain was entered, so the wait returned at
// once. Measured on an isolated home - the daemon exited in 0.6s and its child
// died with it. A drain waiting for corpses.
//
// Reordering drain's cancel would not have fixed it and that is asserted below,
// because it is the first thing a reader of drain() would try.
//
// THE CONSTRAINT THAT KILLED THE THREE-LINE FIX, and it must be honoured by
// whatever ships: jobContext has TWO production consumers with OPPOSITE
// cancellation semantics. daemon_dispatch_tracked.go:783 hands runCtx to the
// per-job goroutines, which MUST survive the signal so a job can finish;
// :732 hands the same runCtx to runQueuedJobsForRepoPoolTracked, the pool
// dispatch and requery loop, which MUST die with the signal or the daemon keeps
// hunting for work while shutting down. One context cannot serve both, so the
// pool path needs two threaded through it. Two existing tests hold that line
// and must pass UNCHANGED:
//   - daemon_dispatch_tracked_test.go:199 an abandoned job observes cancellation
//   - pool_requery_bound_test.go:411      a draining daemon stops re-querying

// TestJobContextSurvivesTheShutdownSignal is the fix, stated as the one
// property everything else depends on.
func TestJobContextSurvivesTheShutdownSignal(t *testing.T) {
	supervisor, cancelSupervisor := context.WithCancel(context.Background())
	tracker := newInflightJobTracker(supervisor)
	jobCtx := tracker.jobContext(context.Background())

	// The shutdown signal. Before the fix this cancelled jobCtx immediately.
	cancelSupervisor()

	select {
	case <-jobCtx.Done():
		t.Fatal("the job context was cancelled by the shutdown signal; drain can then only wait for work that is already dying")
	case <-time.After(50 * time.Millisecond):
	}

	// And it IS still cancellable, by the one thing that should do it.
	tracker.drain(&bytes.Buffer{}, time.Millisecond)
	select {
	case <-jobCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not cancel the job context; an in-flight job would outlive the daemon and orphan its runtime child")
	}
}

// TestDrainWaitsForInFlightWorkBeforeCancelling is the behaviour an operator
// gets: work in flight when the signal arrives is allowed to finish.
//
// The fake job observes its own context, so it fails if the context dies while
// it is still working - which is exactly what the 0.6s measurement was.
func TestDrainWaitsForInFlightWorkBeforeCancelling(t *testing.T) {
	supervisor, cancelSupervisor := context.WithCancel(context.Background())
	tracker := newInflightJobTracker(supervisor)

	if !tracker.beginWithin(0, "job-slow", "owner/repo", "checkout", "runtime") {
		t.Fatal("beginWithin refused a first job")
	}
	jobCtx := tracker.jobContext(context.Background())

	var mu sync.Mutex
	var cancelledEarly bool
	finished := make(chan struct{})
	tracker.wg.Add(1)
	go func() {
		defer tracker.wg.Done()
		defer tracker.end("job-slow")
		defer close(finished)
		// Slow BY CONSTRUCTION, like the /tmp probe's `sleep 300`, not slow by
		// luck. 300ms is long enough that a cancel-first drain would be observed.
		select {
		case <-time.After(300 * time.Millisecond):
		case <-jobCtx.Done():
			mu.Lock()
			cancelledEarly = true
			mu.Unlock()
		}
	}()

	// The signal arrives while the job is in flight.
	cancelSupervisor()

	var out bytes.Buffer
	tracker.drain(&out, 10*time.Second)

	<-finished
	mu.Lock()
	early := cancelledEarly
	mu.Unlock()
	if early {
		t.Fatal("the in-flight job was cancelled before it finished; the drain is still waiting for corpses")
	}
	if strings.Contains(out.String(), "abandoned") {
		t.Fatalf("drain reported abandoning work it should have waited for: %q", out.String())
	}
}

// TestDrainAbandonsPastItsBudgetAndSaysSo is the bound. A ctx-deaf subprocess
// must not block daemon stop forever, and what it leaves behind must be a
// KILLED row rather than a silent one - the trailing cancel kills the
// subprocess, the delivery fails on the signal, and #1726's classifier
// (merged as 246caff8) records it so `job list --killed` finds exactly what
// was abandoned.
//
// It also pins the orphan risk I found in my own first fix: a DEFERRED trailing
// cancel lets drain return before the cancellation has propagated, so this
// asserts the context is done by the time drain returns.
func TestDrainAbandonsPastItsBudgetAndSaysSo(t *testing.T) {
	tracker := newInflightJobTracker(context.Background())
	if !tracker.beginWithin(0, "job-deaf", "owner/repo", "checkout", "runtime") {
		t.Fatal("beginWithin refused a first job")
	}
	release := make(chan struct{})
	tracker.wg.Add(1)
	go func() {
		defer tracker.wg.Done()
		<-release // deaf to cancellation, on purpose
	}()

	var out bytes.Buffer
	start := time.Now()
	tracker.drain(&out, 50*time.Millisecond)
	elapsed := time.Since(start)
	close(release)

	if elapsed > 5*time.Second {
		t.Fatalf("drain took %s against a 50ms budget; the bound is not bounding", elapsed)
	}
	if !strings.Contains(out.String(), "abandoned 1 in-flight job") {
		t.Fatalf("drain did not report what it abandoned: %q", out.String())
	}
	// And it still cancelled on the way out, or the abandoned job would keep
	// running past daemon death.
	select {
	case <-tracker.jobContext(context.Background()).Done():
	case <-time.After(2 * time.Second):
		t.Fatal("drain abandoned work WITHOUT cancelling; that orphans a runtime child holding a worktree")
	}
}

// TestDrainRefusesNewWorkOnceStarted is what makes the wait finite. Without
// it a worker tick racing the shutdown could spawn a job the drain never sees,
// and the bound would be a lie.
func TestDrainRefusesNewWorkOnceStarted(t *testing.T) {
	tracker := newInflightJobTracker(context.Background())
	tracker.drain(&bytes.Buffer{}, time.Millisecond)
	if tracker.beginWithin(0, "job-late", "owner/repo", "checkout", "runtime") {
		t.Fatal("a job was admitted after drain started; the drain's wait would never cover it")
	}
}
