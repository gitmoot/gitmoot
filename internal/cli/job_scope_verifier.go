package cli

import (
	"context"
	"io"
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

// startJobScopeVerifier runs the repair in the background until it succeeds
// or ctx ends, logging each failure and the success, and returns the verifier
// the daemon's workers share.
func startJobScopeVerifier(ctx context.Context, store *db.Store, stdout io.Writer) *jobScopeVerifier {
	v := &jobScopeVerifier{}
	go v.repairUntilVerified(ctx, store, stdout)
	return v
}

func (v *jobScopeVerifier) repairUntilVerified(ctx context.Context, store *db.Store, stdout io.Writer) {
	delay := jobScopeRepairBackoff[0]
	for attempt := 1; ; attempt++ {
		changed, err := store.ReprojectLiveJobScope(ctx)
		if err == nil {
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
