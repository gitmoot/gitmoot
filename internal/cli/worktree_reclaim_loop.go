package cli

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// worktreeReclaimLoopInterval is the pause between background reclaim passes.
// Each pass is bounded by reclaimTickBudget per pass kind, so this only sets
// how soon a finished job's worktree is looked at again.
const worktreeReclaimLoopInterval = 5 * time.Second

// startWorktreeReclaimLoop runs the skipped and aged delegation worktree
// reclaim passes on their own goroutine (#2517 step 3). They used to run in the
// worker tick, before dispatch, in the sequential repo sweep: preserving and
// removing a worktree on the sshfs mount takes tens of seconds, and every
// second of it delayed job dispatch for every repo.
//
// Safety does not come from running on the dispatch goroutine any more. Each
// attempt reserves its checkout keys in the tracker (tryReserveCheckouts):
// the candidate's own key and the repo's registered checkout, whose worktree
// metadata the removal rewrites. The reservation is refused while a job holds
// either key, and a job is refused while the reservation is held, both under
// the tracker lock. The engine's own fences (cleanup lifecycle lease, durable
// checkout mutation lock, generation and path revalidation) are unchanged;
// they already guard the same removal from `gitmoot job cleanup apply`, which
// runs in another process.
//
// One goroutine, one candidate at a time: no burst of parallel git processes
// on the mount. The terminal task pass stays in the tick (its worktrees are on
// the local disk). repos lists the repos to visit each pass. The returned stop
// cancels the loop and waits, bounded by daemonShutdownDrainTimeout, so the
// supervisor does not close the store under a running attempt.
func startWorktreeReclaimLoop(ctx context.Context, store *db.Store, worker jobWorker, tracker *inflightJobTracker, rootFilter string, repos func(context.Context) ([]string, error), stdout io.Writer) (stop func()) {
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	tracker.setReclaimLoopActive(true)
	go func() {
		defer close(done)
		var agedAt time.Time
		for {
			agedAt = runWorktreeReclaimLoopPass(loopCtx, store, worker, tracker, rootFilter, repos, stdout, time.Now().UTC(), agedAt)
			select {
			case <-loopCtx.Done():
				return
			case <-time.After(worktreeReclaimLoopInterval):
			}
		}
	}()
	return func() {
		cancel()
		timer := time.NewTimer(daemonShutdownDrainTimeout)
		defer timer.Stop()
		select {
		case <-done:
			tracker.setReclaimLoopActive(false)
		case <-timer.C:
			writeLine(stdout, "worktree reclaim loop did not stop within %s", daemonShutdownDrainTimeout)
		}
	}
}

// runWorktreeReclaimLoopPass visits every repo once. agedAt is when the aged
// pass last attempted anything; it runs again after worktreeReclaimInterval,
// the cadence it had in the tick. The updated value is returned. A panic is
// logged, never propagated: a reclaim fault must not take the daemon down.
func runWorktreeReclaimLoopPass(ctx context.Context, store *db.Store, worker jobWorker, tracker *inflightJobTracker, rootFilter string, repos func(context.Context) ([]string, error), stdout io.Writer, now time.Time, agedAt time.Time) (next time.Time) {
	next = agedAt
	defer func() {
		if r := recover(); r != nil {
			writeLine(stdout, "worktree reclaim loop pass panicked: %v\n%s", r, debug.Stack())
		}
	}()
	sweepCleanupTrash(ctx, store, worker, tracker, stdout)
	names, err := repos(ctx)
	if err != nil {
		writeLine(stdout, "worktree reclaim loop: list repos: %v", err)
		return next
	}
	cand := newTickCandidates(store)
	cand.reserve = tracker.tryReserveCheckouts
	cand.reclaimBudget.resume = tracker.reclaimResumeState()
	defer cand.finishReclaimTick()
	cand.skipAgedReclaim = !maintenancePassDue(agedAt, now, worktreeReclaimInterval)
	defer func() {
		if cand.agedReclaim.attempted {
			next = now
		}
	}()
	for _, repo := range names {
		if ctx.Err() != nil {
			return next
		}
		if err := runDelegationReclaimPasses(ctx, store, worker, repo, rootFilter, tracker.checkoutHeld, cand, stdout, now); err != nil {
			writeLine(stdout, "%s: worktree reclaim failed: %v", repo, err)
		}
	}
	return next
}

// enabledRepoNames lists the enabled registered repos, the set the fleet
// worker sweep visits.
func enabledRepoNames(store *db.Store) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		repos, err := store.ListRepos(ctx)
		if err != nil {
			return nil, fmt.Errorf("list repos: %w", err)
		}
		names := make([]string, 0, len(repos))
		for _, repo := range repos {
			if repo.Enabled {
				names = append(names, repo.FullName())
			}
		}
		return names, nil
	}
}

// cleanupTrashSweepLimit bounds how many leftover trash directories one loop
// pass deletes; each can take tens of seconds on the worktree mount.
const cleanupTrashSweepLimit = 2

// sweepCleanupTrash deletes trash directories whose deletion did not finish
// right after their cleanup (#2524): a crash between the move and the
// deletion, or a failed deletion. The worktree's own reclaim already
// completed; this only frees the space. Like an attempt, it reserves the
// repo's registered checkout, whose worktree metadata the deletion rewrites.
func sweepCleanupTrash(ctx context.Context, store *db.Store, worker jobWorker, tracker *inflightJobTracker, stdout io.Writer) {
	pending, err := store.PendingCleanupTrashes(ctx, cleanupTrashSweepLimit)
	if err != nil {
		writeLine(stdout, "worktree reclaim loop: list cleanup trash: %v", err)
		return
	}
	for _, item := range pending {
		if ctx.Err() != nil {
			return
		}
		job, err := store.GetJob(ctx, item.JobID)
		if err != nil {
			writeLine(stdout, "cleanup trash %s: load job %s: %v", item.Path, item.JobID, err)
			continue
		}
		checkout := worker.delegationParentCheckout(ctx, job)
		payload, _ := daemonJobPayload(job)
		release, ok := tracker.tryReserveCheckouts("repo:"+strings.TrimSpace(payload.Repo), registeredCheckoutWorktreeKey(checkout))
		if !ok {
			continue
		}
		runner, err := worker.subprocessRunnerForJob(job)
		if err == nil {
			err = worker.workflowForJob(checkout, runner).DisposeCleanupTrash(ctx, job.ID, item.Path)
		}
		release()
		if err != nil {
			writeLine(stdout, "cleanup trash %s: %v", item.Path, err)
		}
	}
}
