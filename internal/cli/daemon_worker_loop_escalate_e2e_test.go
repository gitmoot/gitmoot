package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

// This file is the end-to-end proof for the daemon worker-loop hardening built
// on top of gaijinjoe's PR #555 ("fix(daemon): keep wedged jobs from stalling
// workers"). It drives the REAL supervisor worker loop the daemon wires at
// runSingleRepoSupervisor / runRegisteredRepoSupervisor
// (startSupervisorWorkerLoopRecovering) — no mocks, no hand-rolled fake loop.
//
// #555 made a SINGLE transient worker-tick error survivable so the daemon no
// longer wedges (property A below — his win, preserved). But retry-forever left
// a "silent-forever" gap (#536-class safety): job-level failures already return
// nil, so what bubbles up here is INFRA (disk-full / corrupt-or-locked SQLite /
// failed migration). A PERMANENT such fault would spin silently at ~1s forever
// — a false-green daemon (status still "running"), zero progress, ~86k log
// lines/day — instead of surfacing so the process exits and systemd restarts.
// The fix tracks CONSECUTIVE tick failures, resets on any success, applies
// capped backoff, and after maxConsecutiveWorkerTickFailures escalates on errCh
// and returns. Cancellation mid-tick is a clean shutdown, never a fault.
//
// MUTATION PROOF for the escalation property (B): remove the escalation ceiling
// so the recovering loop retries forever, e.g. in
// startSupervisorWorkerLoopInternal delete the block:
//
//	if consecutiveFailures >= maxConsecutiveWorkerTickFailures {
//	    writeLine(stdout, "...escalating", err, consecutiveFailures)
//	    errCh <- err
//	    return
//	}
//
// Then TestRecoveringWorkerLoopEscalatesPersistentFault_555_E2E goes RED: errCh
// never receives, the loop never surfaces the persistent fault, and the test
// times out. Restore the block -> green.

// TestWedgedWorkerLoopRecoveringProcessesLaterQueuedWork_555_E2E is property (A):
// a SINGLE transient tick error is survived and a later queued job is still
// processed. This is gaijinjoe's #555 win; before it the daemon wedged on the
// first tick error and never drained subsequently-queued work.
func TestWedgedWorkerLoopRecoveringProcessesLaterQueuedWork_555_E2E(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("db.Open returned error: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Real "later work": a job queued in the real store. It must survive a
	// transient worker-tick error and still get processed by a subsequent tick.
	const laterJobID = "job-later-queued-555"
	if err := store.CreateJob(ctx, db.Job{
		ID:      laterJobID,
		Agent:   "worker-e2e",
		Type:    "delegation",
		State:   "queued",
		Payload: `{"prompt":"later queued work that must not be stranded"}`,
	}); err != nil {
		t.Fatalf("CreateJob(queued later work) returned error: %v", err)
	}

	loopCtx, cancelLoop := context.WithCancel(ctx)
	defer cancelLoop()

	var ticks int32

	errCh := startSupervisorWorkerLoopRecovering(loopCtx, 2*time.Millisecond, io.Discard, func(now time.Time) error {
		n := atomic.AddInt32(&ticks, 1)
		if n == 1 {
			return errors.New("transient worker tick failure (e2e #555)")
		}
		claimed, err := store.TransitionJobState(ctx, laterJobID, "queued", "running")
		if err != nil {
			return err
		}
		if claimed {
			if err := store.UpdateJobState(ctx, laterJobID, "done"); err != nil {
				return err
			}
		}
		job, err := store.GetJob(ctx, laterJobID)
		if err != nil {
			return err
		}
		if job.State == "done" {
			cancelLoop()
		}
		return nil
	})

	select {
	case err, ok := <-errCh:
		if ok && err != nil {
			t.Fatalf("recovering worker loop reported fatal error %v; a single transient tick error must be retried, not surfaced", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovering worker loop never terminated (deadlock / no retry)")
	}

	if got := atomic.LoadInt32(&ticks); got < 2 {
		t.Fatalf("worker loop ran %d tick(s); the transient tick #1 error wedged the loop so no SUBSEQUENT tick ran (pre-#555 bug)", got)
	}

	job, err := store.GetJob(ctx, laterJobID)
	if err != nil {
		t.Fatalf("GetJob(later work) returned error: %v", err)
	}
	if job.State != "done" {
		t.Fatalf("later queued job state = %q, want %q; the worker loop wedged on the transient error", job.State, "done")
	}
}

// TestRecoveringWorkerLoopEscalatesPersistentFault_555_E2E is property (B): a
// PERMANENT tick fault (every tick errors) is retried a bounded number of times
// and then ESCALATED — the error is surfaced on errCh and the loop returns — so
// systemd can restart the daemon instead of it spinning silently forever. This
// closes the "silent-forever" gap left by #555's retry-forever behavior.
//
// This is the mutation target: delete the escalation ceiling and this goes RED
// (errCh never receives, this times out).
func TestRecoveringWorkerLoopEscalatesPersistentFault_555_E2E(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ticks int32
	permanent := errors.New("persistent infra fault (corrupt sqlite) (e2e #555)")

	// Small base interval so the capped exponential backoff stays sub-second
	// across the whole streak; the production const is unchanged.
	errCh := startSupervisorWorkerLoopRecovering(ctx, 100*time.Microsecond, io.Discard, func(now time.Time) error {
		atomic.AddInt32(&ticks, 1)
		return permanent
	})

	select {
	case err, ok := <-errCh:
		if !ok {
			t.Fatal("recovering worker loop closed WITHOUT surfacing the persistent fault; a permanent tick error must escalate so systemd can restart")
		}
		if !errors.Is(err, permanent) {
			t.Fatalf("recovering worker loop surfaced %v, want the persistent fault %v", err, permanent)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovering worker loop NEVER surfaced the persistent fault (silent-forever gap); it must escalate after maxConsecutiveWorkerTickFailures")
	}

	// It escalated at (not before, not long after) the ceiling: exactly
	// maxConsecutiveWorkerTickFailures ticks ran before the loop returned.
	if got := int(atomic.LoadInt32(&ticks)); got != maxConsecutiveWorkerTickFailures {
		t.Fatalf("worker loop ran %d ticks before escalating, want %d (maxConsecutiveWorkerTickFailures)", got, maxConsecutiveWorkerTickFailures)
	}
}

// TestRecoveringWorkerLoopResetsStreakOnSuccess_555_E2E is property (C): a
// SUCCESS between failures resets the consecutive-failure streak, so (max-1)
// failures, then a success, then (max-1) more failures does NOT prematurely
// escalate — even though the TOTAL failure count far exceeds the ceiling. Only
// CONSECUTIVE failures escalate.
func TestRecoveringWorkerLoopResetsStreakOnSuccess_555_E2E(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two bursts of (max-1) failures separated by a single success. The success
	// resets the streak, so neither burst reaches the ceiling. Total failures
	// (2*(max-1)) is well above max, proving it is CONSECUTIVE failures — not a
	// lifetime count — that escalate.
	burst := maxConsecutiveWorkerTickFailures - 1
	transient := errors.New("transient tick error (e2e #555 reset)")

	var mu sync.Mutex
	var n int
	errCh := startSupervisorWorkerLoopRecovering(ctx, 100*time.Microsecond, io.Discard, func(now time.Time) error {
		mu.Lock()
		defer mu.Unlock()
		n++
		switch {
		case n <= burst:
			return transient // first burst: streak climbs to max-1
		case n == burst+1:
			return nil // success: streak resets to 0
		case n <= 2*burst+1:
			return transient // second burst: streak climbs to max-1 again
		default:
			cancel() // clean stop; we never hit the ceiling
			return nil
		}
	})

	select {
	case err, ok := <-errCh:
		if ok && err != nil {
			t.Fatalf("recovering worker loop escalated %v after a reset; %d total failures never reached %d CONSECUTIVE, so it must NOT escalate", err, 2*burst, maxConsecutiveWorkerTickFailures)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovering worker loop never terminated after the reset scenario")
	}
}

// TestRecoveringWorkerLoopCleanShutdownOnCancel_555_E2E is property (D): a ctx
// cancellation observed mid-tick is a clean shutdown — the loop returns WITHOUT
// surfacing an error on errCh and WITHOUT emitting a spurious "tick error;
// retrying" log line, and the cancellation does NOT count toward the escalation
// streak.
func TestRecoveringWorkerLoopCleanShutdownOnCancel_555_E2E(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logs bytes.Buffer
	var logMu sync.Mutex
	safeLog := &lockedWriter{w: &logs, mu: &logMu}

	errCh := startSupervisorWorkerLoopRecovering(ctx, time.Millisecond, safeLog, func(now time.Time) error {
		// Simulate a tick that is interrupted by graceful shutdown: cancel the
		// context and return the context error the way a real tick would when
		// its own ctx is cancelled mid-work.
		cancel()
		return context.Canceled
	})

	select {
	case err, ok := <-errCh:
		if ok && err != nil {
			t.Fatalf("clean shutdown surfaced %v on errCh; a mid-tick cancellation must be silent, never a fault", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovering worker loop did not shut down cleanly on cancellation")
	}

	logMu.Lock()
	got := logs.String()
	logMu.Unlock()
	if strings.Contains(got, "retrying") || strings.Contains(got, "escalating") || strings.Contains(got, "context canceled") {
		t.Fatalf("clean shutdown emitted a spurious log line %q; a mid-tick cancellation must not log or count as a failure", got)
	}
}

// TestRunDaemonPollWithTimeoutBoundsWedgedPoll_555_E2E is the poll half of #555:
// polling runs under the checkout lock, so a poll that hangs forever holds that
// lock forever and freezes the daemon. runDaemonPollWithTimeout bounds it. This
// path is now ALSO wired into the multi-repo supervisor's pollRepo (the real
// deployment), not just single-repo.
func TestRunDaemonPollWithTimeoutBoundsWedgedPoll_555_E2E(t *testing.T) {
	start := time.Now()

	poll := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	err := runDaemonPollWithTimeout(context.Background(), 20*time.Millisecond, poll)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runDaemonPollWithTimeout error = %v, want context.DeadlineExceeded (a hung poll must be bounded)", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("runDaemonPollWithTimeout took %v; a wedged poll was NOT bounded", elapsed)
	}
}

// TestWorkerTickBackoffCapsAtMax verifies the retry sleep grows from the base
// interval and never exceeds maxWorkerTickBackoff (so a persistent fault backs
// off to the poll cadence instead of spinning at the base interval).
func TestWorkerTickBackoffCapsAtMax(t *testing.T) {
	base := time.Second
	if got := workerTickBackoff(base, 1); got != base {
		t.Fatalf("workerTickBackoff(1) = %v, want base %v (his single-transient cadence preserved)", got, base)
	}
	if got := workerTickBackoff(base, 2); got != 2*base {
		t.Fatalf("workerTickBackoff(2) = %v, want %v", got, 2*base)
	}
	prev := time.Duration(0)
	for f := 1; f <= maxConsecutiveWorkerTickFailures; f++ {
		got := workerTickBackoff(base, f)
		if got > maxWorkerTickBackoff {
			t.Fatalf("workerTickBackoff(%d) = %v exceeds cap %v", f, got, maxWorkerTickBackoff)
		}
		if got < prev {
			t.Fatalf("workerTickBackoff(%d) = %v decreased below previous %v (backoff must be monotonic up to the cap)", f, got, prev)
		}
		prev = got
	}
	if got := workerTickBackoff(base, 1000); got != maxWorkerTickBackoff {
		t.Fatalf("workerTickBackoff(1000) = %v, want cap %v", got, maxWorkerTickBackoff)
	}
}

// lockedWriter serializes writes to an underlying buffer so the loop goroutine
// and the test goroutine can share it without a data race.
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
