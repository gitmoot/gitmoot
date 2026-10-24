package cli

import (
	"context"
	"io"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// Recheck pending work even without inserts: a recipient becoming idle releases
// its held batch. Empty outboxes return before any runtime probes.
const replyWakeDrainInterval = 5 * time.Second

// replyWakeDrainLoop is the daemon's ONLY wake outbox drainer. The drain used to
// run at the head of every repository sweep, so a wake waited for the whole
// fleet sweep (one to two minutes on this fleet) before it could be claimed.
// It now runs on its own goroutine: one per daemon, ticks never overlap, and
// ClaimWakeOutbox's compare-and-set still guards against any other process.
type replyWakeDrainLoop struct {
	store   *db.Store
	worker  jobWorker
	tracker *inflightJobTracker
	stdout  io.Writer
}

func newReplyWakeDrainLoop(store *db.Store, worker jobWorker, tracker *inflightJobTracker, stdout io.Writer) *replyWakeDrainLoop {
	return &replyWakeDrainLoop{
		store:   store,
		worker:  worker,
		tracker: tracker,
		stdout:  stdout,
	}
}

// tick observes recipient activity on every tick, not just after a new row.
func (l *replyWakeDrainLoop) tick(ctx context.Context, now time.Time) {
	runReplyWakeOutboxDrainOnce(ctx, l.store, l.worker, l.stdout, now, l.tracker)
}

// startReplyWakeDrainLoop starts the single drain goroutine. It drains once
// immediately so startup does not wait an interval.
//
// The returned stop function is the supervisor's shutdown: it cancels the loop
// and waits for an in-flight drain to record its outcome, so the supervisor
// cannot close the store under a claimed batch (post-claim delivery runs
// without cancellation). The wait is bounded by daemonShutdownDrainTimeout,
// the same bound the in-flight job tracker uses.
func startReplyWakeDrainLoop(ctx context.Context, store *db.Store, worker jobWorker, tracker *inflightJobTracker, stdout io.Writer) (stop func()) {
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	loop := newReplyWakeDrainLoop(store, worker, tracker, stdout)
	go func() {
		defer close(done)
		loop.tick(loopCtx, time.Now().UTC())
		ticker := time.NewTicker(replyWakeDrainInterval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				loop.tick(loopCtx, time.Now().UTC())
			}
		}
	}()
	return func() {
		cancel()
		timer := time.NewTimer(daemonShutdownDrainTimeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			writeLine(stdout, "reply wake outbox drain did not stop within %s", daemonShutdownDrainTimeout)
		}
	}
}

// runReplyWakeOutboxDrainOnce drains the store-global wake outbox and logs its
// health. A drain failure is logged, never returned: it must not stop delivery
// on the next tick or any repository work.
func runReplyWakeOutboxDrainOnce(ctx context.Context, store *db.Store, worker jobWorker, stdout io.Writer, now time.Time, tracker *inflightJobTracker) {
	health, err := drainFleetReplyWakeOutbox(ctx, store, worker, now)
	switch {
	case err != nil:
		if health.blocked > 0 {
			if !tracker.replyWakeOutboxHealthChanged(health) {
				break
			}
		} else {
			tracker.forgetReplyWakeOutboxHealth()
		}
		writeLine(stdout, "reply wake outbox drain unhealthy: %v", err)
	case health.inert > 0:
		// Log on CHANGE only (#1758): inert obligations persist until an
		// operator adds a matching rule, so the unchanged line is pure noise.
		if tracker.replyWakeOutboxHealthChanged(health) {
			writeLine(stdout, "reply wake outbox drain health: %s", health)
		}
	default:
		tracker.forgetReplyWakeOutboxHealth()
	}
}
