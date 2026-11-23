package cli

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// jobScopeVerifier says whether this process may let the jobs table's repo and
// root_id columns EXCLUDE rows from a scheduling pass (#2735). Every payload
// writer keeps them equal to the payload, but rows an older version wrote may
// not be, and only Store.ReprojectLiveJobScope proves otherwise. Until that
// repair has succeeded in this process, listPendingQueuedJobs lists the whole
// queue and scopes it by payload, as before #2735; a failed repair therefore
// costs decode time, never a hidden job. A nil verifier (a hand-built worker,
// or a one-shot command that runs no repair) is never verified.
type jobScopeVerifier struct {
	verified atomic.Bool
}

func (v *jobScopeVerifier) trusted() bool {
	return v != nil && v.verified.Load()
}

// jobScopeRepairBackoff is the first and the largest wait between repair
// attempts; each failure doubles the wait up to the largest. A var so a test
// can shorten it.
var jobScopeRepairBackoff = [2]time.Duration{time.Second, time.Minute}

// jobScopeStopTimeout bounds how long stop waits for the repair to return.
// A var so a test can shorten it.
var jobScopeStopTimeout = daemonShutdownDrainTimeout

// stopOverrunDiagnostics receives the line a background loop's stop reports
// when the loop outlives its stop timeout. It is not the worker's output: the
// loops write there through the serialized writer (output.go), and a write
// stalled under that writer's mutex is one way a loop outlives the timeout, so
// a report through the same sink could block the shutdown it bounds (#2739).
// A var so a test can capture it.
var stopOverrunDiagnostics io.Writer = os.Stderr

// reportStopOverrun reports that a background loop did not stop within
// timeout. It writes from its own goroutine, so the stop that calls it returns
// on time even when stopOverrunDiagnostics stalls too.
func reportStopOverrun(loop string, timeout time.Duration) {
	go writeLine(stopOverrunDiagnostics, "gitmoot: %s did not stop within %s", loop, timeout)
}

// startJobScopeVerifier runs the repair in the background until it succeeds
// or ctx ends, logging each failure and the success, and returns the verifier
// the daemon's workers share and a stop that cancels the repair and waits for
// it, as the worktree reclaim loop's does: the supervisor calls it before it
// returns and its store is closed.
func startJobScopeVerifier(ctx context.Context, store *db.Store, stdout io.Writer) (*jobScopeVerifier, func()) {
	v := &jobScopeVerifier{}
	repairCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.repairUntilVerified(repairCtx, store, stdout)
	}()
	return v, func() {
		cancel()
		timer := time.NewTimer(jobScopeStopTimeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			reportStopOverrun("job scope repair", jobScopeStopTimeout)
		}
	}
}

func (v *jobScopeVerifier) repairUntilVerified(ctx context.Context, store *db.Store, stdout io.Writer) {
	delay := jobScopeRepairBackoff[0]
	for attempt := 1; ; attempt++ {
		changed, err := store.ReprojectLiveJobScope(ctx)
		if err == nil {
			// The flag is published first, so whoever reads this line can rely
			// on the passes listing by column.
			v.verified.Store(true)
			writeLine(stdout, "job scope verified: re-projected repo/session columns of %d live job(s); scheduling passes now list by column", changed)
			return
		}
		if ctx.Err() != nil {
			return
		}
		writeLine(stdout, "job scope repair attempt %d failed, retrying in %s; scheduling passes list the whole queue meanwhile: %v", attempt, delay, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, jobScopeRepairBackoff[1])
	}
}
