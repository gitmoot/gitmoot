package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/gitmoot/gitmoot/internal/workflow"
	"io"
	"path/filepath"
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
func startWorktreeReclaimLoop(ctx context.Context, store *db.Store, worker jobWorker, tracker *inflightJobTracker, rootFilter string, repos reclaimLoopRepos, stdout io.Writer) (stop func()) {
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

// reclaimLoopRepos lists the repos one loop pass visits, in order. cand is the
// pass's candidate carrier, so a lister can read the pass's shared candidate
// queries instead of running them again.
type reclaimLoopRepos func(ctx context.Context, store *db.Store, cand *tickCandidates, now time.Time) ([]string, error)

// fixedReclaimRepos visits the named repos, a single-repo daemon's set.
func fixedReclaimRepos(names ...string) reclaimLoopRepos {
	return func(context.Context, *db.Store, *tickCandidates, time.Time) ([]string, error) {
		return names, nil
	}
}

// runWorktreeReclaimLoopPass visits every repo once. agedAt is when the aged
// pass last attempted anything; it runs again after worktreeReclaimInterval,
// the cadence it had in the tick. The updated value is returned. A panic is
// logged, never propagated: a reclaim fault must not take the daemon down.
func runWorktreeReclaimLoopPass(ctx context.Context, store *db.Store, worker jobWorker, tracker *inflightJobTracker, rootFilter string, repos reclaimLoopRepos, stdout io.Writer, now time.Time, agedAt time.Time) (next time.Time) {
	next = agedAt
	defer func() {
		if r := recover(); r != nil {
			writeLine(stdout, "worktree reclaim loop pass panicked: %v\n%s", r, debug.Stack())
		}
	}()
	sweepCleanupTrash(ctx, store, worker, tracker, stdout)
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
	names, err := repos(ctx, store, cand, now)
	if err != nil {
		writeLine(stdout, "worktree reclaim loop: list repos: %v", err)
		return next
	}
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

// fleetReclaimRepos lists the enabled registered repos, the set the fleet
// worker sweep visits, then the repos owning reclaim candidates that are not
// registered at all: removed, or renamed (a job keeps the repo name it ran
// under). No repo sweep visits those, so their obligations were never
// attempted (#2531). They come last, under the same per-kind budgets and
// resume positions. A disabled repo is still registered and stays unvisited.
func fleetReclaimRepos(ctx context.Context, store *db.Store, cand *tickCandidates, now time.Time) ([]string, error) {
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	names := make([]string, 0, len(repos))
	registered := make(map[string]bool, len(repos))
	for _, repo := range repos {
		registered[repo.FullName()] = true
		if repo.Enabled {
			names = append(names, repo.FullName())
		}
	}
	// An unreadable TTL leaves the aged candidates out here; the aged pass
	// logs it.
	ttl, err := cand.config.delegationWorktreeTTL(filepath.Dir(store.DatabasePath()))
	if err != nil {
		ttl = 0
	}
	owners, err := cand.reclaimCandidateRepos(ctx, now, ttl)
	if err != nil {
		// The registered repos still get their pass, which reads the same
		// queries again.
		return names, nil
	}
	for _, repo := range owners {
		if !registered[repo] {
			names = append(names, repo)
		}
	}
	return names, nil
}

// cleanupTrashSweepLimit bounds how many leftover trash directories one loop
// pass tries to delete (each can take tens of seconds on the worktree mount);
// cleanupTrashSweepWindow bounds how many pending records it looks at, so busy
// or failing entries cannot hold back the rest (#2526 review). An entry whose
// disposal could not finish is skipped for cleanupTrashRetryAfter.
const (
	cleanupTrashSweepLimit  = 2
	cleanupTrashSweepWindow = 32
	cleanupTrashRetryAfter  = 10 * time.Minute
)

// sweepCleanupTrash deletes trash directories whose deletion did not finish
// right after their cleanup (#2524): a crash between the move and the
// deletion, or a failed deletion. The worktree's own reclaim already
// completed; this only frees the space. Like an attempt, it reserves the
// repo's registered checkout, whose worktree metadata the deletion rewrites.
func sweepCleanupTrash(ctx context.Context, store *db.Store, worker jobWorker, tracker *inflightJobTracker, stdout io.Writer) {
	pending, err := store.PendingCleanupTrashes(ctx, cleanupTrashSweepWindow, time.Now().Add(-cleanupTrashRetryAfter))
	if err != nil {
		writeLine(stdout, "worktree reclaim loop: list cleanup trash: %v", err)
		return
	}
	attempts := 0
	for _, item := range pending {
		if ctx.Err() != nil || attempts >= cleanupTrashSweepLimit {
			return
		}
		if disposeCleanupTrashOnce(ctx, store, worker, tracker, stdout, item) {
			attempts++
		}
	}
}

// disposeCleanupTrashOnce makes one disposal attempt; it reports whether it
// reached the deletion step. The reservation is released however the attempt
// ends, a panic included.
func disposeCleanupTrashOnce(ctx context.Context, store *db.Store, worker jobWorker, tracker *inflightJobTracker, stdout io.Writer, item db.PendingCleanupTrash) (attempted bool) {
	job, err := store.GetJob(ctx, item.JobID)
	if err != nil {
		writeLine(stdout, "cleanup trash %s: load job %s: %v", item.Path, item.JobID, err)
		return false
	}
	payload, _ := daemonJobPayload(job)
	retry := func() {
		_ = store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{JobID: item.JobID, Kind: workflow.CleanupTrashRetryEvent, Message: item.Path})
	}
	checkout, keys, err := cleanupTrashCheckout(ctx, store, payload.Repo, item)
	if err != nil {
		writeLine(stdout, "cleanup trash %s: %v", item.Path, err)
		retry()
		return false
	}
	if strings.TrimSpace(checkout) == "" {
		writeLine(stdout, "cleanup trash %s: repo %s has no registered checkout", item.Path, payload.Repo)
		retry()
		return false
	}
	release, ok := tracker.tryReserveCheckouts(keys...)
	if !ok {
		// Busy now; skip it for a while so busy entries cannot fill the
		// window on every pass (#2526 review).
		retry()
		return false
	}
	defer release()
	runner, err := worker.subprocessRunnerForJob(job)
	if err == nil {
		err = worker.workflowForJob(checkout, runner).DisposeCleanupTrash(ctx, item)
	}
	if err != nil {
		writeLine(stdout, "cleanup trash %s: %v", item.Path, err)
		retry()
	}
	return true
}

// cleanupTrashCheckout is the checkout a trash disposal runs through, and the
// checkout keys it reserves: the repo's and its registered checkout's. The
// trash of a repo no longer registered (#2531) goes through the checkout
// owning it, from its recorded git administrative directory, as the disposal
// itself locks it (#2526); the registered repos whose checkout that is are
// reserved too.
func cleanupTrashCheckout(ctx context.Context, store *db.Store, repo string, item db.PendingCleanupTrash) (string, []string, error) {
	repo = strings.TrimSpace(repo)
	keys := []string{"repo:" + repo}
	record, err := store.GetRepo(ctx, repo)
	if err == nil {
		checkout := strings.TrimSpace(record.CheckoutPath)
		return checkout, append(keys, registeredCheckoutWorktreeKey(checkout)), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}
	owner, err := workflow.CleanupTrashOwnerCheckout(item)
	if err != nil {
		return "", nil, fmt.Errorf("repo %s is not registered and the checkout owning the trash is unknown: %w", repo, err)
	}
	owners, err := checkoutRepoKeys(ctx, store, owner)
	if err != nil {
		return "", nil, err
	}
	return owner, append(append(keys, registeredCheckoutWorktreeKey(owner)), owners...), nil
}
