package cli

import (
	"context"
	"io"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

const (
	// replyWakeDrainInterval is how often the dedicated drain loop checks the
	// outbox for a new row. A review result skips the coalescing hold (owner
	// decision 2026-10-08), so this interval, not the fleet sweep, bounds how
	// long a seat waits for its verdict wake.
	replyWakeDrainInterval = 5 * time.Second
	// replyWakeDrainRecheckInterval bounds how long the loop goes without a full
	// drain when no new row arrived. Held rows become due, deferred recipients
	// become ready and attempted rows age without any insert, so they are picked
	// up on this cadence. The cheap new-row check keeps the loop from repeating
	// readiness probes (one Herdr snapshot per due group) every five seconds.
	replyWakeDrainRecheckInterval = 30 * time.Second
)

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
	recheck time.Duration

	lastRowID int64
	lastDrain time.Time
}

func newReplyWakeDrainLoop(store *db.Store, worker jobWorker, tracker *inflightJobTracker, stdout io.Writer) *replyWakeDrainLoop {
	return &replyWakeDrainLoop{
		store:   store,
		worker:  worker,
		tracker: tracker,
		stdout:  stdout,
		recheck: replyWakeDrainRecheckInterval,
	}
}

// tick runs a full drain when a row was inserted since the last drain or the
// recheck interval elapsed, and reports whether it drained. An unreadable
// watermark always drains, so the drain's own health line reports the fault.
func (l *replyWakeDrainLoop) tick(ctx context.Context, now time.Time) bool {
	latest, err := l.store.LatestWakeOutboxID(ctx)
	if err == nil && !l.lastDrain.IsZero() && latest == l.lastRowID && now.Sub(l.lastDrain) < l.recheck {
		return false
	}
	runReplyWakeOutboxDrainOnce(ctx, l.store, l.worker, l.stdout, now, l.tracker)
	l.lastDrain = now
	// The watermark was read BEFORE the drain, so a row inserted while it ran
	// still differs from lastRowID on the next tick.
	if err == nil {
		l.lastRowID = latest
	}
	return true
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
