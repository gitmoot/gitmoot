package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const daemonRunningJobStaleAfter = 30 * time.Minute

const defaultDaemonRunningJobStaleAfter = daemonRunningJobStaleAfter

// daemonRunningJobStaleFloor is the smallest GITMOOT_STALE_RUNNING_AFTER we
// honor. The stale-running window is a CRASH BACKSTOP, not a job timeout: it is
// how long a job may sit in 'running' with no lease progress before the daemon
// assumes the worker died and requeues it. A tiny value (e.g. 1s) turns that
// backstop into an aggressive killer — especially for NON-resumable runtimes
// (shell/no runtime-session lock) where runtimeOwnerLeaseHeld is always false,
// so there is no lease to protect a live worker and the coarse threshold is the
// ONLY gate. Sub-floor values are rejected in favor of the default (#560).
const daemonRunningJobStaleFloor = 1 * time.Minute

// runtimeLeaseTeardownGrace is added to a job's timeout when sizing its
// runtime-session lock lease so the lease strictly OUTLIVES the run-context
// deadline plus worst-case terminal teardown (runtime subprocess kill + worktree
// force-clean). run() arms the run context at exactly jobTimeout but holds the
// lease through that teardown, which happens AFTER the deadline fires. Without
// this margin the lease would expire in the window [t0+jobTimeout,
// t0+jobTimeout+teardown] while the worker is STILL ALIVE finishing — and
// recoverExpiredRuntimeSessionLocks + DeleteExpiredResourceLocks (runtime:%
// bypasses the not-running guard) would reap that live worker's lock and requeue
// its still-'running' owner, starting a SECOND worker on the dirty in-flight
// worktree: the exact #536 clobber. With the grace a NORMALLY-terminating worker
// always releases its lease before it expires; only a genuinely stuck/crashed
// worker past jobTimeout+grace is reaped and requeued.
const runtimeLeaseTeardownGrace = 2 * time.Minute

const daemonJobCancelPollInterval = 250 * time.Millisecond

const daemonWorkerLoopInterval = 1 * time.Second

// daemonPollTimeout bounds a single repo's PollOnce / PollRecoveryCommandsOnce.
// The poll runs while HOLDING that repo's checkout lock, and both supervisors
// take each repo's lock SEQUENTIALLY, so a wedged (ctx-respecting-but-slow)
// poll on one repo freezes that repo's — and, in the multi-repo sweep, every
// later repo's — worker ticks until it returns (#555 / #536). It is therefore a
// hard STALL bound, not the expected poll duration: reusing
// daemonRunningJobStaleAfter (30 min) here left the sweep frozen for up to half
// an hour, largely defeating #555's anti-stall goal, so it is deliberately much
// tighter. A healthy poll finishes well inside this; exceeding it means the poll
// is wedged and cancelling it (the deferred checkout Unlock still runs, so no
// lock leak) is the correct recovery.
const daemonPollTimeout = 2 * time.Minute

// cockpitReconcileInterval is the low-frequency cadence of the cockpit reconcile
// GC sweep (Task 7): it drops cockpit_pane rows whose herdr pane is gone and whose
// owning root is terminal. It runs rarely because it is a backstop for the
// per-Deliver / root-finalize teardown plus report-metadata --ttl-ms self-expiry.
const cockpitReconcileInterval = 5 * time.Minute

var errRuntimeSessionBusy = errors.New("runtime session is busy")

// runtimeLockWaitEpisodes dedups the runtime_lock_wait job_event: a job that
// bounces busy every dispatcher pass records ONE event per wait EPISODE — the
// first busy since it last acquired its runtime lock, or since daemon start — not
// one per attempt. Before #598 a permanently-contended job wrote a runtime_lock_wait
// row on EVERY dispatch pass (~76k rows / 56% of the whole job_events table), which
// then bloated every per-job ListJobEvents scan the retry/recovery passes run.
//
// The map records, per job id, WHEN that job's episode event was last EMITTED. An
// episode is "open" (suppress further writes) while an entry exists AND is younger
// than runtimeLockWaitEpisodeTTL; the id is cleared outright when the job acquires
// its runtime lock, so the next wait re-emits immediately. For a job that stays
// contended longer than the TTL the episode re-opens and re-emits at most one event
// per TTL — a deliberate liveness signal that the wait is still ongoing, not a
// per-pass flood. Entries also expire: a job that terminates WITHOUT ever acquiring
// (so endRuntimeLockWaitEpisode never runs for it) leaves a stale entry that ages
// past the "open" window and is pruned once the map grows beyond
// runtimeLockWaitEpisodeMax, so terminal-without-acquire jobs can no longer grow the
// map unboundedly. In-memory ⇒ resets on daemon start (matching the "since daemon
// start" episode boundary). Mirrors the preflightWarnByRepo/preflightWarnMu throttle
// style above.
const (
	// runtimeLockWaitEpisodeTTL re-opens a still-contended job's wait episode after
	// this long, so a very long wait re-emits one liveness event per TTL rather than
	// staying silent forever.
	runtimeLockWaitEpisodeTTL = 15 * time.Minute
	// runtimeLockWaitEpisodeMax bounds the episode map: once it exceeds this many
	// entries, markRuntimeLockWaitEpisode prunes every entry older than the TTL
	// (terminal-without-acquire leftovers that endRuntimeLockWaitEpisode will never
	// clear).
	runtimeLockWaitEpisodeMax = 512
)

var (
	runtimeLockWaitMu       sync.Mutex
	runtimeLockWaitEpisodes = map[string]time.Time{}
)

// runtimeLockWaitEpisodeOpen reports whether jobID currently has an open, already-
// emitted wait episode: an entry exists AND its event was emitted within the last
// runtimeLockWaitEpisodeTTL. It is READ-ONLY — it never mutates the map — so a
// failed event write (which skips markRuntimeLockWaitEpisode) leaves the episode
// closed and the next bounce re-attempts the write. Call it BEFORE writing; write
// the event iff it returns false.
func runtimeLockWaitEpisodeOpen(jobID string) bool {
	runtimeLockWaitMu.Lock()
	defer runtimeLockWaitMu.Unlock()
	emitted, ok := runtimeLockWaitEpisodes[jobID]
	if !ok {
		return false
	}
	return time.Since(emitted) < runtimeLockWaitEpisodeTTL
}

// markRuntimeLockWaitEpisode records that a runtime_lock_wait event was just emitted
// for jobID, opening (or refreshing) its wait episode. Call it ONLY AFTER AddJobEvent
// succeeds, so a failed write is retried on the next bounce instead of being
// suppressed. It also opportunistically bounds the map: once it exceeds
// runtimeLockWaitEpisodeMax entries, every entry older than the TTL is dropped (these
// are terminal-without-acquire leftovers past their liveness window — a live episode
// is refreshed on each re-emit and so never ages out here).
func markRuntimeLockWaitEpisode(jobID string) {
	runtimeLockWaitMu.Lock()
	defer runtimeLockWaitMu.Unlock()
	runtimeLockWaitEpisodes[jobID] = time.Now()
	if len(runtimeLockWaitEpisodes) > runtimeLockWaitEpisodeMax {
		for id, emitted := range runtimeLockWaitEpisodes {
			if time.Since(emitted) >= runtimeLockWaitEpisodeTTL {
				delete(runtimeLockWaitEpisodes, id)
			}
		}
	}
}

// endRuntimeLockWaitEpisode clears jobID's wait episode once it acquires its
// runtime lock, so a later wait is recorded as a fresh episode.
func endRuntimeLockWaitEpisode(jobID string) {
	runtimeLockWaitMu.Lock()
	defer runtimeLockWaitMu.Unlock()
	delete(runtimeLockWaitEpisodes, jobID)
}

// staleFloorWarnOnce keeps the sub-floor warning from flooding the log: the
// recovery path that reads this runs once per worker-loop tick (~1s), so we warn
// at most once per daemon process.
var staleFloorWarnOnce sync.Once

// configuredDaemonRunningJobStaleAfter resolves the crash-backstop window from
// GITMOOT_STALE_RUNNING_AFTER, falling back to the default when unset, malformed,
// non-positive, OR below daemonRunningJobStaleFloor. This is a CRASH BACKSTOP,
// not a timeout: it bounds how long a job may sit 'running' with no lease
// progress before the daemon assumes the worker crashed and requeues it. A
// sub-floor value (e.g. 1s) would let the backstop requeue live workers — most
// dangerously for non-resumable runtimes that hold no lease — so it is rejected
// with a one-time warning rather than honored (#560).
func configuredDaemonRunningJobStaleAfter(stdout io.Writer) time.Duration {
	raw := strings.TrimSpace(os.Getenv("GITMOOT_STALE_RUNNING_AFTER"))
	if raw == "" {
		return defaultDaemonRunningJobStaleAfter
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultDaemonRunningJobStaleAfter
	}
	if d < daemonRunningJobStaleFloor {
		staleFloorWarnOnce.Do(func() {
			writeLine(stdout, "GITMOOT_STALE_RUNNING_AFTER=%s is below the %s crash-backstop floor; using default %s", raw, daemonRunningJobStaleFloor, defaultDaemonRunningJobStaleAfter)
		})
		return defaultDaemonRunningJobStaleAfter
	}
	return d
}

func recoverExpiredRuntimeSessionLocks(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time) error {
	return recoverExpiredRuntimeSessionLocksSkipping(ctx, store, stdout, now, nil)
}

// recoverForeignBootRunners is the #651 cross-boot recovery pass, run at daemon
// startup AND every worker tick. When this host's boot id differs from the boot id
// recorded on a running job / held runtime-session lock, that owner was claimed on
// a PREVIOUS boot and its in-process worker died when the host rebooted — so it is
// recovered IMMEDIATELY, regardless of any runtime-session lease (which survives a
// reboot in the DB and would otherwise keep the job "held" until it expired: the
// AC2 gap #536's lease gate cannot close by itself).
//
// It requeues the foreign-boot running jobs (this covers non-resumable/shell jobs
// too, which hold no lease at all) and reclaims the foreign-boot runtime-session
// locks so a requeued resumable owner can re-acquire its session on re-dispatch.
// It is a STRICT no-op off Linux (BootID()=="") — preserving today's age/lease
// behavior — and never touches a SAME-boot owner, so a live in-process worker is
// never double-run (the #536 protection is untouched). Cheap and idempotent: after
// the first pass reclaims them there are no foreign-boot rows left, so re-running
// it per repo per tick is a near-empty indexed scan.
func recoverForeignBootRunners(ctx context.Context, store *db.Store, stdout io.Writer) error {
	bootID := db.BootID()
	if bootID == "" {
		return nil
	}
	requeued, err := store.RequeueRunningJobsFromForeignBoot(ctx, bootID)
	if err != nil {
		return err
	}
	for _, id := range requeued {
		writeLine(stdout, "requeued running job %s claimed on a previous boot (host rebooted)", id)
	}
	released, err := store.ReleaseRuntimeSessionLocksFromForeignBoot(ctx, bootID)
	if err != nil {
		return err
	}
	if released > 0 {
		writeLine(stdout, "reclaimed %d runtime session lock(s) held on a previous boot", released)
	}
	return nil
}

// recoverExpiredRuntimeSessionLocksSkipping is recoverExpiredRuntimeSessionLocks
// with an in-flight owner exclusion (#562): a lock whose owner job is currently
// being run BY THIS PROCESS is neither requeued nor reaped even if its lease has
// expired — the owning goroutine is still alive (a ctx-deaf runtime overrunning
// its timeout), and releasing its lock would let a second run of the same
// session start beside it (the #536 hazard). A nil skip set is byte-identical
// to the unskipped recovery.
func recoverExpiredRuntimeSessionLocksSkipping(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, skipOwners map[string]bool) error {
	expiredRuntimeLocks, err := store.ListExpiredRuntimeSessionLocks(ctx, now)
	if err != nil {
		return err
	}
	// Requeue owners BEFORE reaping the lock rows. An expired runtime lease means
	// the job's real timeout + teardown grace has elapsed, so a still-'running'
	// owner is genuinely stale (a normally-terminating worker releases its lock
	// before the grace-padded lease expires — see runtimeLeaseTeardownGrace).
	// Ordering requeue-then-delete keeps the two durable: if a mid-loop DB error
	// aborts the sweep, the un-processed locks are still expired and get retried
	// next tick, instead of being deleted up front and losing the requeue signal
	// (which would strand those owners as 'running' until the coarse 30m window).
	// TransitionJobStateWithEvent is a no-op unless the owner is still 'running'.
	for _, lock := range expiredRuntimeLocks {
		if strings.TrimSpace(lock.OwnerJobID) == "" || skipOwners[lock.OwnerJobID] {
			continue
		}
		recovered, err := store.TransitionJobStateWithEvent(ctx, lock.OwnerJobID, string(workflow.JobRunning), string(workflow.JobQueued), db.JobEvent{
			JobID:   lock.OwnerJobID,
			Kind:    string(workflow.JobQueued),
			Message: "recovered running job after runtime session lock expired",
		})
		if err != nil {
			return err
		}
		if recovered {
			writeLine(stdout, "requeued running job %s after runtime session lock expired", lock.OwnerJobID)
		}
	}
	deleted, err := store.DeleteExpiredResourceLocksExcludingOwners(ctx, now, sortedStringSetKeys(skipOwners))
	if err != nil {
		return err
	}
	if deleted > 0 {
		writeLine(stdout, "recovered %d expired runtime session locks", deleted)
	}
	return nil
}

// jobEventBlockedTTLExpired is the job_event kind the blocked_ttl sweep appends
// after it dismisses a blocked job (#631). It is DISTINCT from the bare
// "cancelled" event workflow.CancelJob writes so a job's history tells a TTL
// auto-expiry apart from an operator's explicit `job cancel`.
const jobEventBlockedTTLExpired = "blocked_ttl_expired"

// sweepExpiredBlockedJobs is the opt-in blocked-job TTL reaper (#631), mirroring
// recoverExpiredRuntimeSessionLocks's tick cadence. With ttl <= 0 — the DEFAULT,
// [orchestrate].blocked_ttl unset — it is an immediate no-op: a blocked job is
// paused awaiting a human, so it is NEVER auto-dismissed unless the operator opted
// in with a positive duration (so the default path is byte-identical).
//
// Otherwise it dismisses every blocked job whose last transition — updated_at,
// stamped by the blocked transition, falling back to created_at — is older than
// now-ttl. It routes each dismissal through workflow.CancelJob, the SAME single-row
// abandon verb an operator's `job cancel` uses, so the job's best-effort lock
// releases fire; it NEVER raw-writes the cancelled state, which would strand those
// locks. Each successful dismissal appends a distinct jobEventBlockedTTLExpired
// event naming the TTL.
//
// It is resilient: one job's cancel (or event-append) failure is logged and
// skipped so it can never abort the rest of the sweep. A job with no parseable
// timestamp is left alone rather than treated as infinitely old.
func sweepExpiredBlockedJobs(ctx context.Context, store *db.Store, ttl time.Duration, stdout io.Writer, now time.Time) error {
	if ttl <= 0 {
		return nil
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return err
	}
	cutoff := now.Add(-ttl).UnixMilli()
	swept := 0
	for _, job := range jobs {
		if job.State != string(workflow.JobBlocked) {
			continue
		}
		stamped := parseJobTimeMillis(job.UpdatedAt)
		if stamped == 0 {
			stamped = parseJobTimeMillis(job.CreatedAt)
		}
		if stamped == 0 || stamped >= cutoff {
			continue
		}
		if _, err := workflow.CancelJob(ctx, store, job.ID); err != nil {
			writeLine(stdout, "blocked_ttl sweep: cancel of blocked job %s failed: %v", job.ID, err)
			continue
		}
		// The cancel already succeeded; the history marker is best-effort (like
		// CancelJob's own lock-release events) so a failed append is logged but never
		// undoes the dismissal or aborts the rest of the sweep.
		if err := store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    jobEventBlockedTTLExpired,
			Message: fmt.Sprintf("dismissed after blocked_ttl %s elapsed", ttl),
		}); err != nil {
			writeLine(stdout, "blocked_ttl sweep: recording expiry event for job %s failed: %v", job.ID, err)
		}
		swept++
	}
	if swept > 0 {
		writeLine(stdout, "blocked_ttl sweep: dismissed %d blocked job(s) idle longer than %s", swept, ttl)
	}
	return nil
}

func runDaemonPollWithTimeout(ctx context.Context, timeout time.Duration, poll func(context.Context) error) error {
	if timeout <= 0 {
		return poll(ctx)
	}
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return poll(pollCtx)
}

func recoverRunningJobsForRepo(ctx context.Context, store *db.Store, stdout io.Writer, repoFilter string, rootFilter string) error {
	now := time.Now().UTC()
	return recoverRunningJobsBeforeForRepo(ctx, store, stdout, now, now.Add(-configuredDaemonRunningJobStaleAfter(stdout)), repoFilter, rootFilter)
}

func recoverCancelledRunningJobsForEnabledRepos(ctx context.Context, store *db.Store, rootFilter string, stdout io.Writer) error {
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if !repo.Enabled {
			continue
		}
		if err := recoverCancelledRunningJobsForRepo(ctx, store, stdout, repo.FullName(), rootFilter); err != nil {
			return err
		}
	}
	return nil
}

func recoverCancelledRunningJobsForRepo(ctx context.Context, store *db.Store, stdout io.Writer, repoFilter string, rootFilter string) error {
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.State != string(workflow.JobCancelled) || !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		settled, err := workflow.SettleCancelledRunningJob(ctx, store, job.ID, "cancelled job recovered after daemon restart")
		if err != nil {
			return err
		}
		if settled {
			writeLine(stdout, "settled cancelled running job %s", job.ID)
		}
	}
	return nil
}

func recoverRunningJobsBeforeForRepo(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, before time.Time, repoFilter string, rootFilter string) error {
	return recoverRunningJobsBeforeForRepoSkipping(ctx, store, stdout, now, before, repoFilter, rootFilter, nil)
}

// recoverRunningJobsBeforeForRepoSkipping is recoverRunningJobsBeforeForRepo
// with an in-flight exclusion (#562): a job THIS process is currently running is
// never treated as crashed-stale, even past the coarse 30m backstop with no
// runtime lease (e.g. a long shell-runtime job holds no lease at all). Inline
// execution used to guarantee this by never scanning while a job ran; the async
// dispatcher must guarantee it explicitly. A nil skip set is byte-identical.
func recoverRunningJobsBeforeForRepoSkipping(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, before time.Time, repoFilter string, rootFilter string, skipJobs map[string]bool) error {
	jobs, err := store.ListRunningJobsUpdatedBefore(ctx, before)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if skipJobs[job.ID] {
			continue
		}
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		if err := recoverRunningJobIfLeaseExpired(ctx, store, stdout, now, job); err != nil {
			return err
		}
	}
	return nil
}

func recoverRunningJobIfLeaseExpired(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, job db.Job) error {
	// Liveness gate (#536): the coarse `updated_at < before` threshold (30m) is a
	// crash backstop, NOT a timeout. A long-running job (e.g. a 4h delegation)
	// holds a runtime-session lock whose LEASE reflects its real job timeout. If
	// that lease has not elapsed the job's timeout has not elapsed, so leave it
	// running — requeuing it would start a second copy that fails on the dirty
	// in-flight worktree and then force-cleans it out from under the live worker.
	//
	// This keys on the lease, NOT on the lock's owner PID: the recorded PID is the
	// gitmoot DAEMON's, not the spawned runtime worker's, so on a daemon restart it
	// is the dead prior daemon even while the reparented worker keeps running — the
	// exact path this recovery is named for. Honoring the lease is correct across a
	// restart (the lease survives in the DB) and immune to PID reuse. The trade-off:
	// a genuinely-crashed worker whose daemon also died is recovered only once its
	// lease expires (recoverExpiredRuntimeSessionLocks reclaims it, then a later
	// tick requeues it) rather than at the 30m threshold — promptness traded for
	// never failing live work, the unattended-reliability goal of #536.
	leaseHeld, err := runtimeOwnerLeaseHeld(ctx, store, job.ID, now)
	if err != nil {
		return err
	}
	if leaseHeld {
		return nil
	}
	recovered, err := store.TransitionJobStateWithEvent(ctx, job.ID, string(workflow.JobRunning), string(workflow.JobQueued), db.JobEvent{
		JobID:   job.ID,
		Kind:    string(workflow.JobQueued),
		Message: "recovered stale running job on daemon startup",
	})
	if err != nil {
		return err
	}
	if recovered {
		writeLine(stdout, "requeued stale running job %s", job.ID)
	}
	return nil
}

// tickCandidates memoizes the three per-tick job-candidate GROUP BY queries
// (advance-retry / comment-retry / delegation-worktree-reclaim) so they run ONCE
// per supervisor tick instead of once per enabled repo (#619). Each query takes
// NO repo argument — they scan the whole job_events table and return the global
// candidate set — yet the retry passes ran them inside runDaemonWorkerTickTracked,
// which the multi-repo supervisor invokes once per enabled repo (18×/tick on the
// affected VPS). The most expensive of the three (JobIDsWithPendingAdvanceRetry)
// materialized ~23.67 MiB of row fetches per call, so re-running it per repo was
// the single largest source of the daemon's idle read volume. Hoisting it here
// keeps per-repo filtering exactly where it was (in Go, in the retry passes) while
// collapsing the shared query to one execution.
//
// Two memoization properties, both implemented once in candidateMemo.get:
//
//  1. SUCCESSES are computed once per tick and shared across every repo's pass, so
//     each query runs once per tick, not once per enabled repo. A job that begins
//     qualifying mid-sweep is therefore not observed until the next tick's fresh
//     carrier — a deliberate, bounded one-tick staleness that self-corrects on the
//     following tick. The carrier is created FRESH each tick (so a candidate that
//     stops qualifying next tick is re-evaluated) and MUST NOT be stored on the
//     long-lived tracker/worker.
//  2. ERRORS are NOT memoized. A failed query leaves the memo unset so the next
//     repo's pass RE-RUNS it. This preserves the per-repo fault isolation the
//     pre-#619 per-repo queries had: a transient store fault (e.g. a single
//     SQLITE_BUSY) fails only the repo that hit it and can self-heal for the rest
//     of the sweep, instead of being replayed to all 18 repos — which would make
//     failed==enabled, error the whole sweep, and feed the consecutive-tick daemon
//     self-exit streak #619 is closing.
//
// No mutex/sync.Once: it is consumed ONLY on the synchronous tick goroutine — the
// per-repo loop in runEnabledRepoWorkerTicksTracked is sequential, and dispatched
// jobs run on their own goroutines and never touch it.
//
// The store dependency is the narrow tickCandidateStore interface (satisfied by
// *db.Store) purely so a counting fake can pin the once-per-tick property in tests;
// production always threads the real *db.Store, so behavior is byte-identical.
type tickCandidateStore interface {
	JobIDsWithPendingAdvanceRetry(ctx context.Context) ([]string, error)
	JobIDsWithPendingCommentRetry(ctx context.Context) ([]string, error)
	JobIDsWithPendingDelegationWorktreeReclaim(ctx context.Context) ([]string, error)
	JobIDsWithAgedTerminalDelegationWorktree(ctx context.Context, cutoff time.Time) ([]string, error)
}

// candidateMemo lazily runs one per-tick candidate query and shares its RESULT
// across the tick's repos, memoizing ONLY a success: get caches the ids on the first
// successful fetch and returns them on every later call, but on a query error it
// returns the error and leaves the memo unset so the next call RE-RUNS fetch
// (retry-on-error — see tickCandidates for why per-repo fault isolation matters). It
// is consumed only on the synchronous tick goroutine, so it needs no synchronization.
type candidateMemo struct {
	done bool
	ids  []string
}

func (m *candidateMemo) get(fetch func() ([]string, error)) ([]string, error) {
	if m.done {
		return m.ids, nil
	}
	ids, err := fetch()
	if err != nil {
		return nil, err
	}
	m.ids = ids
	m.done = true
	return m.ids, nil
}

type tickCandidates struct {
	store       tickCandidateStore
	advance     candidateMemo
	comment     candidateMemo
	reclaim     candidateMemo
	agedReclaim candidateMemo
}

// newTickCandidates is a package var (not a plain func) only so the once-per-tick
// regression test can substitute a carrier backed by a counting store; production
// never reassigns it.
var newTickCandidates = func(store tickCandidateStore) *tickCandidates {
	return &tickCandidates{store: store}
}

func (c *tickCandidates) advanceRetryCandidates(ctx context.Context) ([]string, error) {
	return c.advance.get(func() ([]string, error) {
		return c.store.JobIDsWithPendingAdvanceRetry(ctx)
	})
}

func (c *tickCandidates) commentRetryCandidates(ctx context.Context) ([]string, error) {
	return c.comment.get(func() ([]string, error) {
		return c.store.JobIDsWithPendingCommentRetry(ctx)
	})
}

func (c *tickCandidates) delegationReclaimCandidates(ctx context.Context) ([]string, error) {
	return c.reclaim.get(func() ([]string, error) {
		return c.store.JobIDsWithPendingDelegationWorktreeReclaim(ctx)
	})
}

func (c *tickCandidates) agedDelegationReclaimCandidates(ctx context.Context, cutoff time.Time) ([]string, error) {
	return c.agedReclaim.get(func() ([]string, error) {
		return c.store.JobIDsWithAgedTerminalDelegationWorktree(ctx, cutoff)
	})
}

// retryPendingJobAdvancements re-fires the post-delivery advancement for any
// terminal job whose latest advancement event is still an unreconciled attempt
// marker (advance_started/advance_retry). It is BOUNDED, not a full-table scan
// (#598): rather than list EVERY job and re-read each terminal job's full event
// history (ListJobEvents) on every 1s worker tick — O(jobs × events), which burned
// a core once a few hundred terminal jobs had accumulated — it asks the store for
// ONLY the (small) set of jobs whose latest tracked advancement event is a pending
// marker, and GetJob's just those. Each candidate is then re-verified with the Go
// predicate jobNeedsAdvanceRetry, so behavior is identical to the old per-job walk;
// the state/repo/session filters and the checkoutHeld gate are preserved verbatim.
// The candidate set comes from the per-tick tickCandidates carrier (#619) so the
// underlying GROUP BY query runs once per tick, not once per enabled repo.
//
// checkoutHeld (nil ⇒ no gate, the legacy inline-tick behavior) reports whether an
// in-flight dispatched job currently holds a checkout key: a candidate whose own
// checkout key is held is skipped this tick (#562 review) — advancement mutates that
// checkout, and the live path only ever runs it under the job's own key — and
// retried on a later tick once the key frees, instead of gating ALL retries on
// whole-repo idleness (which a steady backlog can prevent indefinitely, freezing
// merge retries).
func retryPendingJobAdvancements(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, checkoutHeld func(string) bool, cand *tickCandidates) error {
	jobIDs, err := cand.advanceRetryCandidates(ctx)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		job, err := worker.Store.GetJob(ctx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			// A marker event with no surviving job row (e.g. a pruned job): nothing
			// to advance, and erroring here would abort the whole tick.
			continue
		}
		if err != nil {
			return err
		}
		if !jobStateCanRetryAdvancement(job.State) || !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		needsRetry, err := worker.jobNeedsAdvanceRetry(ctx, job.ID)
		if err != nil {
			return err
		}
		if !needsRetry {
			continue
		}
		if checkoutHeld != nil && checkoutHeld(queuedJobCheckoutKey(ctx, worker.Store, job)) {
			continue
		}
		if err := worker.advanceJob(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

// reclaimSkippedDelegationWorktrees re-fires the terminal worktree cleanup for any
// terminal delegation child whose cleanup was previously SKIPPED because a foreign
// runtime owner was still active (#536). The cleanup is idempotent and itself
// liveness-gated, so this is a no-op while the owner remains active; once the
// owner's lock releases or its lease expires (recoverExpiredRuntimeSessionLocks
// runs earlier in the tick), the preserved worktree+branch are reclaimed rather
// than leaked forever.
//
// It is BOUNDED, not a full-table scan (#549): rather than list every job and
// re-read each terminal job's full event history (ListJobEvents) on every 1s
// supervisor tick — O(jobs × events), which burned a core once a few hundred
// terminal jobs had accumulated — it asks the store for ONLY the (small) set of
// jobs whose latest cleanup outcome is still an unreconciled preserve marker, and
// reads just those. Correctness is unchanged: a worktree that genuinely needs
// reclaiming still carries that marker and is still reclaimed; once reclaimed it
// emits delegation_worktree_removed and drops out of the candidate set.
// checkoutHeld (nil ⇒ no gate, the legacy inline-tick behavior) skips a
// candidate while an in-flight job holds either the terminal child's own
// worktree key (someone is running in the worktree being reclaimed — e.g. a
// continuation reusing it) or the repo's shared checkout key (the reclaim's git
// commands run from the parent checkout). Skipped candidates keep their pending
// marker and are reclaimed on a later tick, so under a steady backlog preserved
// worktrees are still reclaimed instead of leaking until full idleness (#562
// review).
func reclaimSkippedDelegationWorktrees(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, checkoutHeld func(string) bool, cand *tickCandidates) error {
	jobIDs, err := cand.delegationReclaimCandidates(ctx)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		job, err := worker.Store.GetJob(ctx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			// A cleanup-marker event with no surviving job row (e.g. a pruned job):
			// nothing to reclaim, and erroring here would abort the whole tick.
			continue
		}
		if err != nil {
			return err
		}
		if !jobStateEligibleForWorktreeReclaim(job.State) || !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		if checkoutHeld != nil {
			if checkoutHeld(queuedJobCheckoutKey(ctx, worker.Store, job)) {
				continue
			}
			if repoFilter != "" && checkoutHeld("repo:"+repoFilter) {
				continue
			}
		}
		engine := worker.WorkflowFactory(worker.delegationParentCheckout(ctx, job))
		if err := engine.ReclaimTerminalDelegationWorktree(ctx, jobID); err != nil {
			return err
		}
	}
	return nil
}

// reclaimAgedTerminalDelegationWorktrees closes the cleanup crash window that
// has no _cleanup_skipped marker. The bounded store query selects only FINAL
// owners older than the configured TTL. Re-verification happens both here and
// in Engine.ReclaimAgedTerminalDelegationWorktree; blocked/queued/running jobs
// are never force-removed.
func reclaimAgedTerminalDelegationWorktrees(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, checkoutHeld func(string) bool, cand *tickCandidates, now time.Time, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	jobIDs, err := cand.agedDelegationReclaimCandidates(ctx, now.Add(-ttl))
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		job, err := worker.Store.GetJob(ctx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if !workflow.IsFinalJobState(job.State) || !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		if checkoutHeld != nil {
			if checkoutHeld(queuedJobCheckoutKey(ctx, worker.Store, job)) {
				continue
			}
			if repoFilter != "" && checkoutHeld("repo:"+repoFilter) {
				continue
			}
		}
		engine := worker.WorkflowFactory(worker.delegationParentCheckout(ctx, job))
		if err := engine.ReclaimAgedTerminalDelegationWorktree(ctx, jobID, now.Add(-ttl)); err != nil {
			return err
		}
	}
	return nil
}

// runDaemonWorkerTickTracked is the per-tick worker pass. With a nil tracker it
// follows the historical synchronous tick behavior: maintenance scans,
// then a BLOCKING runQueuedJobsForRepo dispatch. The supervisors pass a live
// tracker (#562), which changes the tick to claim-and-dispatch-async:
//
//   - recovery scans skip jobs THIS process is running (an in-flight >30m job
//     with no runtime lease — e.g. a shell-runtime job — must not be requeued
//     out from under its own live worker by its own daemon's tick);
//   - the expired runtime-lock reaper likewise skips locks owned by in-flight
//     jobs (their goroutine is alive; releasing the lock could double-run the
//     session);
//   - comment retries (no checkout touched) run every tick; checkout-mutating
//     maintenance (advancement retries, delegation worktree reclaims) skips any
//     candidate whose checkout key an in-flight job holds — so it never mutates
//     a checkout under a running job, without being starved forever by a repo
//     that always has SOMETHING in flight;
//   - dispatch goes through dispatchQueuedJobsTracked, which returns promptly
//     and bounds in-flight jobs by both the repo limit and the host-global
//     --workers cap.
func runDaemonWorkerTickTracked(ctx context.Context, store *db.Store, worker jobWorker, workers int, dryRun bool, repoFilter string, rootFilter string, stdout io.Writer, now time.Time, tracker *inflightJobTracker, cand *tickCandidates) error {
	if dryRun {
		return nil
	}
	// A nil carrier means this is a standalone tick (single-repo supervisor or
	// direct caller): compute the shared candidate sets once for THIS
	// tick. The multi-repo supervisor passes a carrier it created once per tick, so
	// the three GROUP BY queries run once per tick rather than once per enabled repo
	// (#619).
	if cand == nil {
		cand = newTickCandidates(worker.Store)
	}
	inflightIDs := tracker.inflightIDs()
	// Cross-boot recovery (#651): requeue jobs / reclaim runtime-session locks whose
	// recorded boot id proves a reboot happened, before the lease-gated recovery
	// below (which alone would leave a rebooted long-lease job "held"). It is
	// boot-scoped and repo-agnostic — no in-flight skip is needed because a foreign
	// boot id can never belong to a job this process is currently running.
	if err := recoverForeignBootRunners(ctx, store, stdout); err != nil {
		return err
	}
	if err := recoverRunningJobsBeforeForRepoSkipping(ctx, store, stdout, now, now.Add(-configuredDaemonRunningJobStaleAfter(stdout)), repoFilter, rootFilter, inflightIDs); err != nil {
		return err
	}
	if err := recoverExpiredRuntimeSessionLocksSkipping(ctx, store, stdout, now, inflightIDs); err != nil {
		return err
	}
	// Opt-in blocked-job TTL reaper (#631): dismiss blocked jobs (paused awaiting a
	// human) idle longer than [orchestrate].blocked_ttl. Disabled by default (ttl 0
	// ⇒ immediate no-op), so the default path is byte-identical. A sweep fault is
	// LOGGED, not returned: this optional housekeeping reaper must never abort the
	// tick's dispatch or escalate the daemon the way the store-fault recovery scans
	// above (deliberately) do. Resolved per tick, so the TTL is live-tunable like the
	// per-repo scheduler override below.
	if err := sweepExpiredBlockedJobs(ctx, store, resolveBlockedTTL(worker.workflowHome()), stdout, now); err != nil {
		writeLine(stdout, "blocked_ttl sweep failed: %v", err)
	}
	// Checkout-mutating maintenance (advancement/merge retries, delegation
	// worktree reclaims) is gated on the ACTUAL mutation hazard — each
	// candidate is skipped while an in-flight job holds the checkout key the
	// retry would touch — not on whole-repo idleness, which a steady staggered
	// backlog can prevent indefinitely (main's blocking barrier guaranteed an
	// idle point between batches; tracked dispatch does not). The per-key gate
	// mirrors the live path: a finishing job runs this same advancement inline
	// under its own key while other keys stay busy. It is race-free on the
	// barrier because begin() only ever runs on THIS goroutine (in the dispatch
	// below) and end() only frees keys; a live background POOL pass begins jobs
	// on its own goroutine, so it still defers the whole block (matching main,
	// where a live pool pass blocked the tick entirely).
	if !tracker.poolRunning(repoFilter) {
		if err := retryPendingJobAdvancements(ctx, worker, repoFilter, rootFilter, tracker.checkoutHeld, cand); err != nil {
			return err
		}
		if err := reclaimSkippedDelegationWorktrees(ctx, worker, repoFilter, rootFilter, tracker.checkoutHeld, cand); err != nil {
			return err
		}
		// The store path is already the resolved Gitmoot root. Using it keeps this
		// hot-read on the daemon's actual home and prevents the raw/resolved
		// double-resolution bug class; isolated tests likewise stay in /tmp.
		if ttl, err := resolveDelegationWorktreeTTL(filepath.Dir(store.DatabasePath())); err != nil {
			writeLine(stdout, "delegation_worktree_ttl reclaim skipped: %v", err)
		} else if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, repoFilter, rootFilter, tracker.checkoutHeld, cand, now, ttl); err != nil {
			return err
		}
	}
	// Comment retries only post PR comments through the commenter — they never
	// touch a checkout — so they run EVERY tick regardless of in-flight work,
	// exactly main's cadence (and main's advancements→reclaims→comments order).
	// Gating them on an idle repo would let one multi-hour in-flight job delay a
	// transiently-failed result comment (and any downstream automation waiting
	// on it) for the job's whole duration.
	if err := retryPendingJobComments(ctx, worker, repoFilter, rootFilter, cand); err != nil {
		return err
	}
	// Per-repo concurrency override (#576): a [repos."owner/repo"] section caps
	// THIS repo's in-flight concurrency (and may flip its scheduler) without a
	// global daemon restart. With no matching section this returns (workers,
	// worker.UsePool) unchanged, so the run path is byte-identical to today. The
	// override is re-read here every tick, which is precisely what makes it
	// tunable live. worker is passed by value, so the per-repo UsePool flip is
	// local to this tick's dispatch and never leaks to sibling repos.
	limit, usePool := worker.resolveRepoScheduler(repoFilter, workers)
	worker.UsePool = usePool
	if tracker == nil {
		return runQueuedJobsForRepo(ctx, worker, limit, repoFilter, rootFilter)
	}
	return dispatchQueuedJobsTracked(ctx, worker, limit, workers, repoFilter, rootFilter, tracker)
}

func runEnabledRepoWorkerTicksTracked(ctx context.Context, store *db.Store, worker jobWorker, workers int, rootFilter string, stdout io.Writer, now time.Time, locks *repoCheckoutLocks, tracker *inflightJobTracker) error {
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return err
	}
	// Compute the shared per-tick job-candidate sets ONCE for this whole sweep and
	// pass the carrier into every enabled repo's tick (#619). The three GROUP BY
	// candidate queries take no repo argument — they return the global candidate set
	// that each repo's retry pass then filters in Go — so running them once here
	// instead of once inside each runDaemonWorkerTickTracked collapses 18×/tick down
	// to 1×/tick on a multi-repo daemon. Fresh each sweep; never retained.
	cand := newTickCandidates(worker.Store)
	// Scope tick faults per repo (#555 follow-up): the recovering supervisor
	// treats a returned error as one fleet-wide failure unit and, after a bounded
	// streak, exits the WHOLE daemon. Returning on the first repo's error would
	// let a single repo-local fault (e.g. a broken/permission-denied checkout
	// dir) both starve every later repo in ListRepos order AND escalate/kill the
	// healthy repos' daemon with it. So log a single repo's tick error and keep
	// sweeping; only a fault hitting EVERY enabled repo — a shared/store-level
	// fault such as locked/corrupt SQLite or disk-full, the genuine global fault
	// #555's escalation targets — is returned so the supervisor can escalate.
	enabled := 0
	failed := 0
	var lastErr error
	for _, repo := range repos {
		if !repo.Enabled {
			continue
		}
		enabled++
		lock := locks.For(repo.FullName())
		if lock != nil {
			lock.Lock()
		}
		tickErr := runDaemonWorkerTickTracked(ctx, store, worker, workers, false, repo.FullName(), rootFilter, stdout, now, tracker, cand)
		if lock != nil {
			lock.Unlock()
		}
		if tickErr != nil {
			// A cancellation observed mid-sweep is a clean shutdown, not a repo
			// fault: propagate it immediately so the supervisor treats it as such
			// (and it never counts toward or masks the escalation streak).
			if errors.Is(tickErr, context.Canceled) || ctx.Err() != nil {
				return tickErr
			}
			failed++
			lastErr = tickErr
			writeLine(stdout, "%s: worker tick error: %v", repo.FullName(), tickErr)
		}
	}
	// Every enabled repo failing is the global-fault signal: return it so the
	// recovering supervisor's streak can trip and escalate. A single-repo daemon
	// (enabled==1) still escalates on its own persistent fault, matching the
	// single-repo supervisor.
	if enabled > 0 && failed == enabled {
		return lastErr
	}
	return nil
}

func jobStateCanRetryAdvancement(state string) bool {
	switch state {
	case string(workflow.JobSucceeded), string(workflow.JobFailed), string(workflow.JobBlocked):
		return true
	default:
		return false
	}
}

// jobStateEligibleForWorktreeReclaim gates the delegation/read-only worktree
// reclaim pass on FINAL states. Cancelled is included because an abort can leave
// a dispatch-allocated read-only worktree, while blocked is excluded because it
// is resumable and still owns its worktree. Cancelled remains intentionally out
// of jobStateCanRetryAdvancement: its resources may be reclaimed, but the job
// itself must never re-advance.
func jobStateEligibleForWorktreeReclaim(state string) bool {
	return workflow.IsFinalJobState(state)
}

// retryPendingJobComments re-posts the result comment for any terminal job whose
// latest comment event is comment_post_failed. Like retryPendingJobAdvancements it
// is BOUNDED (#598): it asks the store for ONLY the jobs whose latest comment event
// is a failure marker instead of listing EVERY job and re-reading each terminal
// job's full event history on every 1s worker tick. Each candidate is re-verified
// with the Go predicate jobNeedsCommentRetry, so behavior is identical. Comment
// retries never touch a checkout, so (unlike advancements) they take no checkoutHeld
// gate.
func retryPendingJobComments(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, cand *tickCandidates) error {
	jobIDs, err := cand.commentRetryCandidates(ctx)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		job, err := worker.Store.GetJob(ctx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			// A marker event with no surviving job row (e.g. a pruned job): skip
			// rather than abort the tick.
			continue
		}
		if err != nil {
			return err
		}
		if !jobStateCanRetryComment(job.State) || !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		needsRetry, err := worker.jobNeedsCommentRetry(ctx, job.ID)
		if err != nil {
			return err
		}
		if !needsRetry {
			continue
		}
		agent := runtime.Agent{Name: job.Agent}
		if dbAgent, err := worker.Store.GetAgent(ctx, job.Agent); err == nil {
			agent = runtimeAgent(dbAgent)
		}
		if err := worker.postJobResultComment(ctx, job.ID, agent, "", nil); err != nil {
			return err
		}
	}
	return nil
}

func jobStateCanRetryComment(state string) bool {
	switch state {
	case string(workflow.JobSucceeded), string(workflow.JobFailed), string(workflow.JobBlocked):
		return true
	default:
		return false
	}
}

// dispatchLimitObserver, when non-nil, is invoked with the concurrency limit that
// each repo dispatch pass actually uses, at the exact point production dispatch
// reads it. Test-only seam (#577): it lets a warm-reload E2E prove a SIGHUP change
// to the live worker count is what the RUNNING dispatch reads on its next pass,
// without a restart. It is nil in production, so the dispatch path is byte-identical.
var dispatchLimitObserver func(limit int)

func runQueuedJobsForRepo(ctx context.Context, worker jobWorker, limit int, repoFilter string, rootFilter string) error {
	if obs := dispatchLimitObserver; obs != nil {
		obs(limit)
	}
	if limit <= 0 {
		return nil
	}
	// Preflight (#444): if the config can't actually run same-repo jobs in
	// parallel (single worker, or the per-tick barrier scheduler) yet ≥2
	// parallelizable jobs are queued, surface the exact relaunch command instead
	// of silently serializing them. "Parallelizable" = same repo, dep-unblocked
	// (already true of queued jobs), and DISTINCT runtime sessions — same-session
	// jobs serialize on the runtime session lock even under pool, so counting raw
	// same-repo jobs would over-warn.
	if serializingConfig(worker.UsePool, limit) {
		warnSerializedParallelJobs(ctx, worker, limit, repoFilter, rootFilter)
	}
	if worker.UsePool {
		return runQueuedJobsForRepoPool(ctx, worker, limit, repoFilter, rootFilter)
	}
	pending, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter, true)
	if err != nil {
		return err
	}
	for len(pending) > 0 {
		policy, err := worker.parallelSessionPolicy()
		if err != nil {
			policy = config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue}
		}
		queued, remaining := selectRunnableQueuedJobsWithPolicy(ctx, worker.Store, pending, limit, policy)
		if len(queued) == 0 {
			return nil
		}
		pending = remaining

		// Host-global admission gate (#365): reserve a session slot + RAM estimate
		// for each selected job BEFORE dispatching it. A job that does not fit the
		// budget is left queued — defer it back to `pending` so it is retried on the
		// next loop iteration once this batch's reservations are released in the
		// goroutine defers (worker.Admission is nil ⇒ Reserve always admits, so the
		// default path is byte-identical). If nothing was admitted this pass we
		// return: the deferred jobs stay queued in the DB for the next daemon tick,
		// when a freed slot can admit them (avoids spinning on an unfittable job).
		admitted := make([]db.Job, 0, len(queued))
		for _, job := range queued {
			job := job
			if worker.Admission.Reserve(job.ID, func() admissionEstimate { return worker.admissionEstimate(ctx, job) }) {
				admitted = append(admitted, job)
				continue
			}
			pending = append([]db.Job{job}, pending...)
		}
		if len(admitted) == 0 {
			return nil
		}

		errs := make(chan error, len(admitted))
		var wg sync.WaitGroup
		for _, job := range admitted {
			job := job
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer worker.Admission.Release(job.ID)
				errs <- worker.run(ctx, job)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil && !errors.Is(err, errRuntimeSessionBusy) {
				return err
			}
		}
	}
	return nil
}

// serializingConfig reports whether the daemon's scheduler config cannot run
// same-repo jobs in parallel (#444): a single worker, or the per-tick barrier
// scheduler (which serializes same-repo jobs on one wg.Wait() + checkout lock).
func serializingConfig(usePool bool, limit int) bool {
	return limit <= 1 || !usePool
}

// parallelizableSerialJobs counts the queued jobs for this repo/session filter
// that could run concurrently but won't under a serializing config (#444):
// distinct runtime sessions among same-repo dep-unblocked queued jobs. Jobs with
// no resolvable runtime session key are counted individually (each is its own
// would-be parallel slot). The count is what the preflight warns on (≥2). The
// returned signature uniquely identifies the parallelizable session set so the
// preflight can de-duplicate an unchanged backlog across ticks.
func parallelizableSerialJobs(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string) (int, string) {
	// forDispatch=false: this is a preflight COUNT for the serialization warning, not
	// a dispatch. It must stay a pure read — no live auth probe (`claude -p`
	// subprocess) and no payload mutation — so the warning path keeps its documented
	// off-hot-path contract (#532).
	pending, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter, false)
	if err != nil {
		return 0, ""
	}
	// Cheap short-circuit: with fewer than 2 pending same-repo jobs there can
	// never be ≥2 parallelizable slots, so skip the per-job session lookups
	// (queuedJobRuntimeResourceKey → Store.GetAgent) entirely. This keeps the
	// common-case (default single-worker, empty/small backlog) off the DB hot
	// path beyond the single ListQueuedJobs the listing already performs.
	if len(pending) < 2 {
		return 0, ""
	}
	sessions := map[string]bool{}
	for _, job := range pending {
		key := queuedJobRuntimeResourceKey(ctx, worker.Store, job)
		if key == "" {
			// Each session-less job is its own parallel slot; key it by job ID
			// so the dedup signature still reflects backlog changes. The job-ID
			// key already makes it a distinct entry in `sessions`, so it must NOT
			// also be counted separately or the slot would be double-counted.
			sessions["job:"+job.ID] = true
			continue
		}
		sessions[key] = true
	}
	count := len(sessions)
	if count < 2 {
		return count, ""
	}
	keys := make([]string, 0, len(sessions))
	for k := range sessions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return count, strings.Join(keys, "\n")
}

// preflightWarnThrottle de-duplicates the serializing-config preflight warning
// (#444) across worker ticks. runQueuedJobsForRepo is called once per poll
// (default 30s), so a steady backlog under a serializing config would otherwise
// re-log the identical line every tick. We re-emit only when the parallelizable
// session set changes or a quiet interval has elapsed, keyed by repo filter.
type preflightWarnState struct {
	signature string
	at        time.Time
}

var (
	preflightWarnMu     sync.Mutex
	preflightWarnByRepo = map[string]preflightWarnState{}
	preflightWarnReWarn = 30 * time.Minute
)

// shouldEmitPreflightWarn reports whether the warning for this repo/signature
// should be emitted now, recording the decision so an unchanged backlog stays
// quiet until either the session set changes or preflightWarnReWarn elapses.
func shouldEmitPreflightWarn(repoKey string, signature string, now time.Time) bool {
	preflightWarnMu.Lock()
	defer preflightWarnMu.Unlock()
	prev, ok := preflightWarnByRepo[repoKey]
	if ok && prev.signature == signature && now.Sub(prev.at) < preflightWarnReWarn {
		return false
	}
	preflightWarnByRepo[repoKey] = preflightWarnState{signature: signature, at: now}
	return true
}

// warnSerializedParallelJobs emits an actionable preflight warning when ≥2
// parallelizable jobs are queued under a serializing config (#444), printing the
// exact relaunch command. It is best-effort and never blocks the tick, and is
// rate-limited so an unchanged backlog does not re-log every poll.
func warnSerializedParallelJobs(ctx context.Context, worker jobWorker, limit int, repoFilter string, rootFilter string) {
	count, signature := parallelizableSerialJobs(ctx, worker, repoFilter, rootFilter)
	if count < 2 {
		return
	}
	repo := strings.TrimSpace(repoFilter)
	target := "the daemon"
	repoKey := "*"
	if repo != "" {
		target = repo
		repoKey = repo
	}
	if !shouldEmitPreflightWarn(repoKey, signature, time.Now()) {
		return
	}
	workers := limit
	if workers < count {
		workers = count
	}
	writeLine(worker.Stdout, "warning: %d parallelizable jobs queued for %s will run serially under the current scheduler config; relaunch with: gitmoot daemon restart --parallel %d", count, target, workers)
	writeLine(worker.Stdout, "         %s", daemonRestartEnvCaveat)
}

// daemonRestartEnvCaveat is appended to the serialized-jobs relaunch hint.
// Runtime auth reloads per delivery; only scheduler state is restart-sensitive.
const daemonRestartEnvCaveat = "note: Claude runtime auth is read per delivery from runtime-auth.env and does not require a restart; a restart resets in-flight scheduler state."

// listPendingQueuedJobs returns the queued jobs eligible to run for this
// repo/session filter, dropping children of a killed root.
//
// Operator kill switch (#341): once a tree's root is killed, do not start any of
// its queued children. The coordinator's own continuation still runs so the
// engine can route through the graceful finalize path; in-flight children finish
// normally. Only children (payload.RootJobID points at another root) are skipped
// here — the root job itself is never skipped.
//
// forDispatch (#532 slice B) gates the LIVE runtime_auth credential probe: only a
// caller that is actually about to dispatch jobs runs it. The preflight
// serialization-warning path (parallelizableSerialJobs) passes false so counting
// queued jobs stays a pure read — no `claude -p` subprocess, no job-payload
// mutation — keeping that path off the DB/subprocess hot path as its contract
// promises. A within-pass cache dedupes the probe across auth-held jobs of the same
// runtime so one outage costs at most one live probe per pass.
func listPendingQueuedJobs(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, forDispatch bool) ([]db.Job, error) {
	jobs, err := worker.Store.ListQueuedJobs(ctx)
	if err != nil {
		return nil, err
	}
	var probeCache authProbeCache
	if forDispatch {
		probeCache = authProbeCache{}
	}
	pending := make([]db.Job, 0, len(jobs))
	for _, job := range jobs {
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		if queuedChildOfKilledRoot(ctx, worker.Store, job) {
			continue
		}
		// Operational-blocker hold (#532): a job deferred behind a classified
		// blocker is not eligible until its earliest-retry-at passes. Both
		// schedulers (barrier and pool) funnel through this listing, so the hold
		// is honored everywhere; jobs without the payload field are unaffected.
		if queuedJobBlockerHeld(job, time.Now().UTC()) {
			continue
		}
		// Auth-probe gate (#532 slice B): once a runtime_auth deferral's coarse hold
		// elapses, only re-dispatch when a live doctor-style probe says the credential
		// is VALID again — an Invalid verdict extends the hold (re-probe next cadence,
		// no attempt burned). Non-auth deferrals and jobs with no probe wired pass
		// straight through (coarse cadence only, byte-identical to slice A). The live
		// probe runs ONLY for a dispatching caller (forDispatch); the warning-count
		// path skips it so it never spawns a subprocess or mutates the payload.
		if forDispatch && !authProbeAllowsRedispatch(ctx, worker, job, time.Now().UTC(), probeCache) {
			continue
		}
		pending = append(pending, job)
	}
	return pending, nil
}

// runQueuedJobsForRepoPool is the opt-in (--scheduler=pool) continuous scheduler
// for #394. Unlike the per-tick barrier it never blocks the tick on a whole
// batch: it keeps up to `limit` workers busy and RE-QUERIES the queue as each
// worker frees, so a job queued *after* dispatch began (e.g. a running job that
// kicks off a follow-up same-repo job and polls it) is picked up without waiting
// for the in-flight batch to drain (layer 1).
//
// Working-tree safety is preserved by live in-flight checkout accounting: a job
// whose checkout key is already held by a running job is never dispatched
// concurrently (layer 2). Same-repo no-worktree jobs therefore still serialize;
// only distinct checkout keys (e.g. isolated worktrees) run in parallel — a
// follow-up PR makes the awaited follow-up carry one so the chain can complete.
//
// inflightCheckouts/inflightRuntimes/running/firstErr are owned solely by this
// dispatcher goroutine; worker goroutines communicate only via the done channel,
// so no lock is required.
func runQueuedJobsForRepoPool(ctx context.Context, worker jobWorker, limit int, repoFilter string, rootFilter string) error {
	return runQueuedJobsForRepoPoolTracked(ctx, worker, limit, limit, repoFilter, rootFilter, nil)
}

// poolDispatchSlots is a pool pass's dispatch budget: the repo's free slots,
// clamped by the host-global remainder (hostCap − tracked in-flight across ALL
// repos) so concurrent per-repo passes never exceed the daemon-wide cap. With a
// nil tracker total() is 0 and hostCap == limit, so the clamp is inert and the
// result is exactly the historical limit − running.
func poolDispatchSlots(limit, running, hostCap int, tracker *inflightJobTracker) int {
	slots := limit - running
	if hostSlots := hostCap - tracker.total(); hostSlots < slots {
		slots = hostSlots
	}
	return slots
}

// runQueuedJobsForRepoPoolTracked is the pool pass with the supervisor's
// in-flight tracker mirrored in (#562): each dispatched job is registered so the
// poller/maintenance gates and the shutdown drain see pool work, the tracker's
// keys are unioned into the selection seeds so a pool pass never dispatches
// beside a tracked non-pool job holding the same checkout/runtime key (a warm
// scheduler flip mid-run), and jobs already in flight are filtered out. hostCap
// additionally clamps each dispatch by the tracker's HOST-global in-flight
// count, so concurrent per-repo pool passes cannot multiply --workers by the
// number of enabled repos. A nil tracker (hostCap == limit, total() == 0) is
// byte-identical to the historical pool.
func runQueuedJobsForRepoPoolTracked(ctx context.Context, worker jobWorker, limit int, hostCap int, repoFilter string, rootFilter string, tracker *inflightJobTracker) error {
	if limit <= 0 {
		return nil
	}
	policy, perr := worker.parallelSessionPolicy()
	if perr != nil {
		policy = config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue}
	}

	type finished struct {
		jobID        string
		checkoutKey  string
		runtimeKey   string
		worktreePath string
		repoCheckout string
		// payloadBeforeIsolation is the job's payload as it was before an
		// isolation worktree was allocated and written into it; non-empty only
		// for isolation-dispatched jobs.
		payloadBeforeIsolation string
		err                    error
	}
	inflightCheckouts := map[string]bool{}
	inflightRuntimes := map[string]bool{}
	running := 0
	// bouncedBusy / bouncedBusyRuntimes track jobs (and their runtime-session keys)
	// that returned errRuntimeSessionBusy earlier in THIS pool invocation (#598).
	// A busy job re-queues immediately once reaped, so without this it was
	// re-selected and re-dispatched every pass in a tight spin (~36 attempts/s,
	// poisoning job_events with runtime_lock_wait rows). Dispatcher-goroutine-owned
	// (like inflightCheckouts), so no lock; reset each invocation, so a bounced job
	// is retried on a later worker tick.
	bouncedBusy := map[string]bool{}
	bouncedBusyRuntimes := map[string]bool{}
	// runtimeKeyMemo caches queuedJobRuntimeResourceKey per job id for the lifetime of
	// this dispatcher invocation (#615 review). excludeBouncedBusy re-derives the key
	// for every still-pending job on every dispatch pass, and each miss is a GetAgent
	// read, so a job that stays pending across N passes otherwise costs N GetAgent
	// reads. The key is stable while a job sits queued (it depends only on the job's
	// agent + payload runtime override), so caching it bounds the cost at one read per
	// job per invocation. Dispatcher-goroutine-owned like the sets above.
	runtimeKeyMemo := map[string]string{}
	done := make(chan finished, limit)
	var firstErr error

	reap := func(f finished) {
		delete(inflightCheckouts, f.checkoutKey)
		if f.runtimeKey != "" {
			delete(inflightRuntimes, f.runtimeKey)
		}
		// An isolation-dispatched job that bounced errRuntimeSessionBusy was never
		// claimed and stays queued — but its payload was rewritten to point at the
		// isolation worktree this reap is about to delete. Restore the
		// pre-isolation payload (best-effort, non-cancellable like the worktree
		// removal) so its next dispatch re-evaluates cleanly instead of
		// preflight-failing terminally on a reaped path. Done before tracker.end
		// so no other selector can re-dispatch it mid-restore.
		if f.payloadBeforeIsolation != "" && errors.Is(f.err, errRuntimeSessionBusy) {
			_ = worker.Store.UpdateJobPayload(context.WithoutCancel(ctx), f.jobID, f.payloadBeforeIsolation)
		} else if f.payloadBeforeIsolation != "" {
			// Operational-blocker deferral (#532) × pool isolation: a deferred job
			// is queued again but its payload still points at the isolation
			// worktree this reap is about to delete. Restore the pre-isolation
			// payload (carrying the blocker hold fields over) so its re-dispatch
			// after the hold re-evaluates cleanly instead of preflight-failing on
			// a reaped path. No-op for any job that is not queued with a blocker
			// hold, so terminal outcomes are byte-identical.
			restorePreIsolationPayloadForDeferredJob(context.WithoutCancel(ctx), worker.Store, f.jobID, f.payloadBeforeIsolation)
		}
		tracker.end(f.jobID)
		// Release the host-global admission reservation (#365) keyed by job ID,
		// alongside the checkout/runtime release. Release is idempotent and a nil
		// budget is a no-op, so this is safe on every reap path incl. panic
		// recovery and shutdown (mirrors the worktree cleanup discipline).
		worker.Admission.Release(f.jobID)
		running--
		// Dispose an auto-created isolation worktree (#394 part 2). Best-effort and
		// on a non-cancellable context so it still runs during daemon shutdown; both
		// the add (in allocatePoolIsolationWorktree) and this remove run on the
		// dispatcher goroutine under the tick's per-repo lock, so they never race.
		if f.worktreePath != "" && f.repoCheckout != "" {
			_ = gitutil.Client{Dir: f.repoCheckout}.RemoveWorktreeForce(context.WithoutCancel(ctx), f.worktreePath)
		}
		if f.err != nil && firstErr == nil && !errors.Is(f.err, errRuntimeSessionBusy) {
			firstErr = f.err
		}
		// A job that bounced busy must not be re-selected/re-dispatched again in
		// THIS invocation — by id, and by runtime-session key so every sibling
		// contending the same busy session is held back too (#598). It stays queued
		// and is retried on a later worker tick (a fresh invocation with fresh sets).
		if errors.Is(f.err, errRuntimeSessionBusy) {
			bouncedBusy[f.jobID] = true
			if f.runtimeKey != "" {
				bouncedBusyRuntimes[f.runtimeKey] = true
			}
		}
	}

	for {
		// Reap finished workers (non-blocking) so freed checkout keys and slots are
		// visible to this dispatch pass.
		for reaping := true; reaping; {
			select {
			case f := <-done:
				reap(f)
			default:
				reaping = false
			}
		}

		// Stop dispatching promptly on cancellation rather than relying on the next
		// store query to observe it; in-flight workers return as their own ctx is
		// cancelled (parity with the barrier's wg.Wait()), then we drain and exit.
		if firstErr == nil && ctx.Err() != nil {
			firstErr = ctx.Err()
		}

		dispatched := 0
		if firstErr == nil {
			pending, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter, true)
			if err != nil {
				firstErr = err
			} else if slots := poolDispatchSlots(limit, running, hostCap, tracker); slots > 0 {
				// Drop jobs that already bounced busy this invocation before ANY
				// selection (#598). pending is fresh each pass and feeds BOTH the
				// primary selection and the isolation `remaining` loop below, so
				// filtering it here excludes bounced jobs from both. A bounced job
				// removed from pending is never re-selected, so dispatched is not
				// re-incremented for it: once every remaining pending job is
				// busy-excluded the loop reaches dispatched==0 && running==0 and
				// returns, so "busy must not count as progress" holds structurally.
				pending = excludeBouncedBusy(ctx, worker, pending, bouncedBusy, bouncedBusyRuntimes, runtimeKeyMemo)
				// Union in the supervisor tracker's in-flight keys (#562): a tracked
				// non-pool job (e.g. dispatched before a warm scheduler flip) must
				// block same-key pool dispatch exactly like a pool-local one. Jobs
				// already in flight anywhere in this process are filtered by ID.
				// The union is a fresh per-pass COPY: the pool's own maps stay
				// reap-owned, so a foreign key never lingers after its job ends.
				seedCheckouts, seedRuntimes := inflightCheckouts, inflightRuntimes
				if tracker != nil {
					trackerCheckouts, trackerRuntimes := tracker.seeds()
					eligible := pending[:0]
					for _, job := range pending {
						if !tracker.inflightJob(job.ID) {
							eligible = append(eligible, job)
						}
					}
					pending = eligible
					seedCheckouts = unionStringSets(inflightCheckouts, trackerCheckouts)
					seedRuntimes = unionStringSets(inflightRuntimes, trackerRuntimes)
				}
				queued, remaining := selectRunnableQueuedJobsSeeded(ctx, worker.Store, pending, slots, policy, seedCheckouts, seedRuntimes)
				for _, job := range queued {
					job := job
					// Host-global admission gate (#365): reserve a session slot + RAM
					// estimate before claiming any checkout/runtime key or a worker slot.
					// A job that does not fit the budget is skipped (left queued) and the
					// pool re-queries on the next pass once a reap frees a slot — never
					// failed/dropped. A nil budget always admits ⇒ byte-identical default.
					if !worker.Admission.Reserve(job.ID, func() admissionEstimate { return worker.admissionEstimate(ctx, job) }) {
						if tracker != nil {
							warnJobHeldBack(worker.Stdout, job.ID, admissionSkipReason(worker.Admission, worker.admissionEstimate(ctx, job)))
						}
						continue
					}
					checkoutKey := queuedJobCheckoutKey(ctx, worker.Store, job)
					runtimeKey := queuedJobRuntimeResourceKey(ctx, worker.Store, job)
					// beginWithin re-checks the host-global cap atomically with
					// registration: a concurrent pass for another repo may have consumed
					// the headroom this pass's slot computation saw.
					if !tracker.beginWithin(hostCap, job.ID, repoFilter, checkoutKey, runtimeKey) {
						worker.Admission.Release(job.ID)
						continue
					}
					inflightCheckouts[checkoutKey] = true
					if runtimeKey != "" {
						inflightRuntimes[runtimeKey] = true
					}
					running++
					dispatched++
					go func() {
						done <- finished{jobID: job.ID, checkoutKey: checkoutKey, runtimeKey: runtimeKey, err: runPoolJobRecovered(ctx, worker, job)}
					}()
				}
				// #394 part 2: a read-only job left blocked ONLY by a contended same-repo
				// checkout (its repo:<repo> key is held by an in-flight job) can run beside
				// the holder in an auto-created detached worktree — the distinct
				// worktree:<path> key is safe to parallelize. This is what lets an awaited
				// same-repo follow-up (the #394 deadlock) make progress.
				// The checks below consult the LIVE inflightCheckouts/inflightRuntimes
				// maps as well as the seed unions: seedCheckouts/seedRuntimes are
				// per-pass COPIES when a tracker is present, so a job dispatched by the
				// loop above (which mutates only the live maps) would otherwise be
				// invisible here — letting a same-runtime-session job be
				// isolation-dispatched beside its just-started sibling. The loser of
				// that session-lock race would bounce busy AFTER its payload was
				// rewritten to the isolation worktree, which reap() then deletes,
				// leaving a queued job pointing at a reaped path that terminally fails
				// on its next run. With a nil tracker the seed and live maps are the
				// same object, so this is byte-identical to the historical pool.
				for _, job := range remaining {
					if running >= limit || tracker.total() >= hostCap {
						break
					}
					payload, perr := daemonJobPayload(job)
					if perr != nil || !poolIsolationEligible(job, payload) {
						continue
					}
					if queuedJobCheckoutKey(ctx, worker.Store, job) != "repo:"+payload.Repo ||
						!(inflightCheckouts["repo:"+payload.Repo] || seedCheckouts["repo:"+payload.Repo]) {
						continue // not blocked by a contended same-repo checkout
					}
					runtimeKey := queuedJobRuntimeResourceKey(ctx, worker.Store, job)
					if runtimeKey != "" && (inflightRuntimes[runtimeKey] || seedRuntimes[runtimeKey] || runtimeResourceLocked(ctx, worker.Store, runtimeKey)) {
						continue // also runtime-contended; leave it to the runtime/temp-worker path
					}
					// Host-global admission gate (#365): reserve before creating the
					// isolation worktree so a deferred job leaves no orphan worktree behind.
					if !worker.Admission.Reserve(job.ID, func() admissionEstimate { return worker.admissionEstimate(ctx, job) }) {
						if tracker != nil {
							warnJobHeldBack(worker.Stdout, job.ID, admissionSkipReason(worker.Admission, worker.admissionEstimate(ctx, job)))
						}
						continue
					}
					payloadBeforeIsolation := job.Payload
					iso, ok, allocErr := worker.allocatePoolIsolationWorktree(ctx, job, payload)
					if !ok {
						worker.Admission.Release(job.ID)
						// #739: the reactive isolation was silent on failure — the exact
						// reason #739 was hard to diagnose (a seat went queued→running with no
						// worktree event and serialized on the shared checkout). Emit a loud
						// skip event so a lost-parallelism serialize is observable. A nil
						// allocErr means the job was simply not isolable (no home/checkout) —
						// not a failure — so stay quiet there.
						if allocErr != nil {
							_ = worker.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "pool_isolation_skipped", Message: fmt.Sprintf("pool read-only isolation skipped (#739); job stays serialized in the shared checkout: %v", allocErr)})
						}
						continue
					}
					if !tracker.beginWithin(hostCap, iso.job.ID, repoFilter, iso.checkoutKey, iso.runtimeKey) {
						worker.Admission.Release(iso.job.ID)
						// Undo the allocation completely: the payload now points at the
						// isolation worktree being removed, and the job stays queued — a
						// host-cap or double-dispatch refusal must not strand it on a
						// reaped path.
						_ = worker.Store.UpdateJobPayload(context.WithoutCancel(ctx), iso.job.ID, payloadBeforeIsolation)
						_ = gitutil.Client{Dir: iso.repoCheckout}.RemoveWorktreeForce(context.WithoutCancel(ctx), iso.worktreePath)
						continue
					}
					inflightCheckouts[iso.checkoutKey] = true
					if iso.runtimeKey != "" {
						inflightRuntimes[iso.runtimeKey] = true
					}
					// #739: make the reactive isolation observable on SUCCESS too (it was
					// silent both ways). Emitted only past the host-cap/double-dispatch gate
					// above, so the event means the job is truly dispatched in its own
					// worktree:<path> key — running beside the same-repo checkout holder,
					// not serialized behind it.
					_ = worker.Store.AddJobEvent(ctx, db.JobEvent{JobID: iso.job.ID, Kind: "pool_isolation_worktree_allocated", Message: fmt.Sprintf("read-only pool-isolation worktree %s allocated (#739); job keyed %s to run beside the same-repo checkout holder", iso.worktreePath, iso.checkoutKey)})
					running++
					dispatched++
					go func() {
						done <- finished{jobID: iso.job.ID, checkoutKey: iso.checkoutKey, runtimeKey: iso.runtimeKey, worktreePath: iso.worktreePath, repoCheckout: iso.repoCheckout, payloadBeforeIsolation: payloadBeforeIsolation, err: runPoolJobRecovered(ctx, worker, iso.job)}
					}()
				}
			}
		}

		if running == 0 {
			// Nothing running: if we also dispatched nothing this pass the queue is
			// drained (or everything left is un-runnable for now) — return, surfacing
			// any worker error. On firstErr we reach here once inflight has drained.
			if dispatched == 0 {
				return firstErr
			}
			continue
		}
		if dispatched == 0 {
			// No progress is possible until a running worker frees a resource; block
			// for one, then re-query (which may now include newly-queued jobs).
			reap(<-done)
		}
	}
}

// runPoolJobRecovered runs a pool job and converts a panic into an error so the
// worker goroutine ALWAYS sends its result to the done channel. This keeps the
// pool's resource accounting and worktree cleanup (in reap) intact even on a
// panicking job, and prevents one bad job from crashing an unattended daemon.
func runPoolJobRecovered(ctx context.Context, worker jobWorker, job db.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pool worker panicked on job %s: %v", job.ID, r)
		}
	}()
	return worker.run(ctx, job)
}

// poolIsolationEligible reports whether a queued job blocked by a contended
// same-repo checkout key may be safely run in an ephemeral detached worktree
// (#394 part 2). Scope: read-only actions (ask/review) with no existing worktree.
// implement jobs are excluded — they either already carry a task worktree
// (already keyed) or must not run detached without the finalize/merge wiring.
func poolIsolationEligible(job db.Job, payload workflow.JobPayload) bool {
	switch strings.TrimSpace(job.Type) {
	case "ask", "review", "produce":
	default:
		return false
	}
	return strings.TrimSpace(payload.WorktreePath) == "" && strings.TrimSpace(payload.TaskID) == ""
}

type poolIsolatedDispatch struct {
	job          db.Job
	checkoutKey  string
	runtimeKey   string
	worktreePath string
	repoCheckout string
}

// allocatePoolIsolationWorktree creates a detached read-only worktree for a
// read-capable job otherwise blocked behind a contended same-repo checkout,
// rewrites the job's payload to run in it (so its checkout key becomes
// worktree:<path>), and returns the dispatch handle incl. cleanup info. ok=false
// means the job is not isolable or the worktree could not be created — the caller
// then leaves it queued to serialize as before (graceful, no deadlock-for-safety
// trade). Runs on the dispatcher goroutine under the tick's per-repo lock.
func (w jobWorker) allocatePoolIsolationWorktree(ctx context.Context, job db.Job, payload workflow.JobPayload) (poolIsolatedDispatch, bool, error) {
	if strings.TrimSpace(w.ConfigHome) == "" {
		return poolIsolatedDispatch{}, false, nil
	}
	repoRecord, err := w.Store.GetRepo(ctx, payload.Repo)
	if err != nil || strings.TrimSpace(repoRecord.CheckoutPath) == "" {
		return poolIsolatedDispatch{}, false, err
	}
	client := gitutil.Client{Dir: repoRecord.CheckoutPath}
	// #739: route through the shared read-only allocator so this reactive top-level
	// isolation path resolves the ref to HEAD (a committed tip that is always
	// resolvable — NOT the stale current branch the researchers flagged), holds the
	// checkout mutation lock, and returns errors LOUDLY. This keeps it behaviorally
	// aligned with the read-only delegation fan-out and the dispatch-time allocation,
	// and turns the previously-silent worktree-add failure into a returned error the
	// caller emits as a pool_isolation_skipped event. It runs SYNCHRONOUSLY on the
	// per-repo dispatch loop, so it passes the short ReadOnlyWorktreeDispatchLockWaitBudget
	// (not the 2-minute default) to fail open fast under merge-gate lock contention
	// rather than freezing this repo's dispatch+reap loop.
	path, err := workflow.AllocateReadOnlyWorktree(ctx, w.Store, w.ConfigHome, payload.Repo, repoRecord.CheckoutPath, job.ID, "pool-isolation", 0, "", workflow.ReadOnlyWorktreeDispatchLockWaitBudget, client)
	if err != nil {
		return poolIsolatedDispatch{}, false, err
	}
	if strings.TrimSpace(path) == "" {
		return poolIsolatedDispatch{}, false, nil
	}
	payload.WorktreePath = path
	// The detached worktree is the COMMITTED TIP of the base ref, so it omits
	// gitignored paths (e.g. vendored repos/**) and uncommitted working-tree
	// changes. Point the isolated read-only job at the canonical repo checkout so an
	// analysis task does not silently report working-tree state as missing (#654),
	// exactly as read-only delegation fan-out does (engine.go, #394 part 2). Append
	// to Instructions so the note is carried in the delivered prompt; the reap path
	// restores payloadBeforeIsolation on a bounce/defer, reverting this too. A blank
	// checkout path yields "" ⇒ byte-identical (no note).
	if note := workflow.ReadOnlyWorktreeContextNote(repoRecord.CheckoutPath); note != "" {
		payload.Instructions += note
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		_ = client.RemoveWorktreeForce(context.WithoutCancel(ctx), path)
		return poolIsolatedDispatch{}, false, err
	}
	if err := w.Store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		_ = client.RemoveWorktreeForce(context.WithoutCancel(ctx), path)
		return poolIsolatedDispatch{}, false, err
	}
	job.Payload = string(encoded)
	return poolIsolatedDispatch{
		job:          job,
		checkoutKey:  queuedJobCheckoutKey(ctx, w.Store, job),
		runtimeKey:   queuedJobRuntimeResourceKey(ctx, w.Store, job),
		worktreePath: path,
		repoCheckout: repoRecord.CheckoutPath,
	}, true, nil
}

type queuedJobResourceSelector struct {
	limit            int
	policy           config.ParallelSessionPolicy
	checkouts        map[string]bool
	runtimes         map[string]bool
	tempReservations map[string]int
}

func selectRunnableQueuedJobsWithPolicy(ctx context.Context, store *db.Store, pending []db.Job, limit int, policy config.ParallelSessionPolicy) ([]db.Job, []db.Job) {
	return selectRunnableQueuedJobsSeeded(ctx, store, pending, limit, policy, nil, nil)
}

// selectRunnableQueuedJobsSeeded is selectRunnableQueuedJobsWithPolicy with the
// checkout/runtime resource sets pre-seeded from already-running jobs. The
// barrier path passes nil seeds (empty, == the original behavior); the pool path
// (#394) seeds the live in-flight keys so a job whose checkout key is already
// held by a running job is not selected. The seed maps are copied, never mutated.
func selectRunnableQueuedJobsSeeded(ctx context.Context, store *db.Store, pending []db.Job, limit int, policy config.ParallelSessionPolicy, seedCheckouts map[string]bool, seedRuntimes map[string]bool) ([]db.Job, []db.Job) {
	if limit <= 0 {
		return nil, pending
	}
	selector := queuedJobResourceSelector{
		limit:            limit,
		policy:           policy,
		checkouts:        copyStringSet(seedCheckouts),
		runtimes:         copyStringSet(seedRuntimes),
		tempReservations: map[string]int{},
	}
	queued := make([]db.Job, 0, min(limit, len(pending)))
	remaining := make([]db.Job, 0, len(pending))
	for _, job := range pending {
		if selector.selects(ctx, store, job, len(queued)) {
			queued = append(queued, job)
			continue
		}
		remaining = append(remaining, job)
	}
	return queued, remaining
}

func copyStringSet(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	for k, v := range src {
		if v {
			dst[k] = true
		}
	}
	return dst
}

// excludeBouncedBusy drops queued jobs that already bounced errRuntimeSessionBusy
// earlier in THIS pool invocation — by job id, and by runtime-session key so every
// job contending the same busy session is held back too (#598). They stay queued
// and are retried on a later worker tick (a fresh invocation, fresh sets), instead
// of being re-selected every pass in a tight, event-table-poisoning spin. Empty
// sets ⇒ pending is returned unchanged, so the no-busy common case pays nothing.
//
// runtimeKeyMemo caches queuedJobRuntimeResourceKey across the invocation's dispatch
// passes so each still-pending job costs at most one GetAgent read per invocation
// (#615 review) rather than one per pass; a nil memo disables caching.
func excludeBouncedBusy(ctx context.Context, worker jobWorker, pending []db.Job, bouncedIDs, bouncedRuntimes map[string]bool, runtimeKeyMemo map[string]string) []db.Job {
	if len(bouncedIDs) == 0 && len(bouncedRuntimes) == 0 {
		return pending
	}
	kept := pending[:0]
	for _, job := range pending {
		if bouncedIDs[job.ID] {
			continue
		}
		if len(bouncedRuntimes) > 0 {
			if rk := memoizedRuntimeResourceKey(ctx, worker.Store, job, runtimeKeyMemo); rk != "" && bouncedRuntimes[rk] {
				continue
			}
		}
		kept = append(kept, job)
	}
	return kept
}

// memoizedRuntimeResourceKey returns queuedJobRuntimeResourceKey for job, caching the
// result in memo keyed by job id so repeated lookups for the same job across a
// dispatcher invocation's passes reuse the single GetAgent read. A nil memo bypasses
// the cache and calls through directly.
func memoizedRuntimeResourceKey(ctx context.Context, store *db.Store, job db.Job, memo map[string]string) string {
	if memo == nil {
		return queuedJobRuntimeResourceKey(ctx, store, job)
	}
	if key, ok := memo[job.ID]; ok {
		return key
	}
	key := queuedJobRuntimeResourceKey(ctx, store, job)
	memo[job.ID] = key
	return key
}

func (s queuedJobResourceSelector) selects(ctx context.Context, store *db.Store, job db.Job, selected int) bool {
	if selected >= s.limit {
		return false
	}
	checkoutKey := queuedJobCheckoutKey(ctx, store, job)
	runtimeKey := queuedJobRuntimeResourceKey(ctx, store, job)
	if s.checkouts[checkoutKey] {
		return false
	}
	runtimeAlreadySelected := runtimeKey != "" && s.runtimes[runtimeKey]
	runtimeAlreadyLocked := runtimeKey != "" && !runtimeAlreadySelected && runtimeResourceLocked(ctx, store, runtimeKey)
	if runtimeAlreadySelected || runtimeAlreadyLocked {
		if !s.canUseTempWorker(ctx, store, job) && runtimeAlreadySelected {
			return false
		}
	}
	s.checkouts[checkoutKey] = true
	if runtimeKey != "" {
		s.runtimes[runtimeKey] = true
	}
	return true
}

func runtimeResourceLocked(ctx context.Context, store *db.Store, runtimeKey string) bool {
	if store == nil || strings.TrimSpace(runtimeKey) == "" {
		return false
	}
	_, err := store.GetResourceLock(ctx, runtimeKey)
	return err == nil
}

func (s queuedJobResourceSelector) canUseTempWorker(ctx context.Context, store *db.Store, job db.Job) bool {
	if store == nil {
		return false
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		return false
	}
	dbAgent, err := store.GetAgent(ctx, job.Agent)
	if err != nil {
		return false
	}
	agent := runtimeAgent(dbAgent)
	typ := tempWorkerAgentType(agent.Name)
	count, err := store.CountActiveAgentInstances(ctx, typ, agent.AutonomyPolicy, time.Now().UTC())
	if err != nil {
		return false
	}
	if count+s.tempReservations[typ] >= s.policy.MaxTempSessionsPerAgent {
		return false
	}
	eligible := tempWorkerEligible(ctx, store, job, payload, agent, s.policy, time.Now().UTC())
	if !eligible.Eligible {
		return false
	}
	s.tempReservations[typ]++
	return true
}

func queuedJobMatchesRepo(job db.Job, repoFilter string) bool {
	repoFilter = strings.TrimSpace(repoFilter)
	if repoFilter == "" {
		return true
	}
	payload, err := daemonJobPayload(job)
	return err == nil && payload.Repo == repoFilter
}

// queuedJobMatchesSession reports whether a job belongs to the delegation tree
// rooted at rootFilter. An empty filter matches everything (the default daemon
// behavior). Otherwise a job matches iff it is the root coordinator job itself
// (job.ID == rootFilter) or carries the root id in its payload
// (payload.RootJobID == rootFilter); children and continuations inherit the
// root id via the payload.
func queuedJobMatchesSession(job db.Job, rootFilter string) bool {
	rootFilter = strings.TrimSpace(rootFilter)
	if rootFilter == "" {
		return true
	}
	if job.ID == rootFilter {
		return true
	}
	payload, err := daemonJobPayload(job)
	return err == nil && payload.RootJobID == rootFilter
}

// queuedChildOfKilledRoot reports whether a queued job is a delegation child leg
// of a tree whose root has been killed by an operator (#341). Only child legs are
// matched and skipped. Two classes are deliberately exempted so the graceful
// finalize can still run:
//   - the root coordinator itself (payload.RootJobID == "" or == job.ID); and
//   - any continuation (coordinator reconvene or the #305 graceful finalize),
//     which carries no DelegationID — it MUST run so the engine routes the killed
//     tree through enqueueFinalizeContinuation and emits a terminal result.
//
// Delegation child legs set DelegationID (delegationRequest), so a non-empty
// DelegationID is what marks a job as skippable work. A payload-parse miss or
// store error fails open (returns false) so a hiccup never silently strands a job.
//
// NOTE: the same child-leg classification invariant (RootJobID != "" &&
// RootJobID != job.ID && DelegationID != "") is re-implemented inline in
// workflow.KillDelegationTree (internal/workflow/job_kill.go, #480) to eagerly
// cancel queued child legs at kill time. The cli->workflow import direction
// prevents sharing one helper, so if the classification rules here change, update
// the workflow site too — the two MUST stay in lockstep.
func queuedChildOfKilledRoot(ctx context.Context, store *db.Store, job db.Job) bool {
	if store == nil {
		return false
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		return false
	}
	rootJobID := strings.TrimSpace(payload.RootJobID)
	if rootJobID == "" || rootJobID == job.ID {
		return false
	}
	// Continuations (DelegationID == "") reconvene the coordinator / finalize the
	// tree and must always run, even for a killed root. Only actual child legs are
	// skipped.
	if strings.TrimSpace(payload.DelegationID) == "" {
		return false
	}
	killed, err := store.IsRootJobKilled(ctx, rootJobID)
	return err == nil && killed
}

func queuedJobCheckoutKey(ctx context.Context, store *db.Store, job db.Job) string {
	payload, err := daemonJobPayload(job)
	if err != nil || strings.TrimSpace(payload.Repo) == "" {
		return "job:" + job.ID
	}
	if path, ok := queuedJobTaskWorktreePath(ctx, store, payload); ok {
		return "worktree:" + path
	}
	return "repo:" + payload.Repo
}

func queuedJobTaskWorktreePath(ctx context.Context, store *db.Store, payload workflow.JobPayload) (string, bool) {
	// Sibling delegations share a task id but run in distinct per-delegation
	// worktrees; key off the payload worktree path so they schedule as separate
	// checkout keys and can run in parallel.
	if delegationPath := strings.TrimSpace(payload.WorktreePath); delegationPath != "" {
		path, err := normalizeTaskWorktreePath(delegationPath)
		return path, err == nil && path != ""
	}
	if store == nil || strings.TrimSpace(payload.TaskID) == "" {
		return "", false
	}
	task, err := store.GetTask(ctx, payload.TaskID)
	if err != nil {
		return "", false
	}
	if strings.TrimSpace(task.RepoFullName) != "" && task.RepoFullName != payload.Repo {
		return "", false
	}
	path, err := normalizeTaskWorktreePath(task.WorktreePath)
	return path, err == nil && path != ""
}

func queuedJobRuntimeResourceKey(ctx context.Context, store *db.Store, job db.Job) string {
	if store == nil {
		return ""
	}
	// A per-job runtime override (#531) runs on ITS OWN session key, so schedule
	// it under that key (fully payload-derived — no GetAgent needed) rather than
	// the agent's default-runtime session it will never take.
	if payload, err := daemonJobPayload(job); err == nil && strings.TrimSpace(payload.RuntimeOverride) != "" {
		// An isolated shell stage keys by job id so identical-command isolated
		// forks don't serialize (#1034). The worker's lock acquisition uses the
		// SAME helper, so the gate and the lock can never disagree.
		if key, ok := isolatedShellStageRuntimeSessionKey(payload, job.ID); ok {
			return key
		}
		key, ok := overrideRuntimeSessionResourceKey(applyJobRuntimeOverride(runtime.Agent{}, payload))
		if !ok {
			return ""
		}
		return key
	}
	agent, err := store.GetAgent(ctx, job.Agent)
	if err != nil {
		return ""
	}
	key, ok := runtimeSessionResourceKey(runtimeAgent(agent))
	if !ok {
		return ""
	}
	return key
}
