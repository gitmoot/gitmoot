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
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const daemonRunningJobStaleAfter = 30 * time.Minute

const defaultDaemonRunningJobStaleAfter = daemonRunningJobStaleAfter

// daemonRunningJobStaleFloor is the smallest GITMOOT_STALE_RUNNING_AFTER we
// honor. The stale-running window is a CRASH BACKSTOP, not a job timeout: it is
// how long a job may sit in 'running' with no lease progress before the daemon
// considers the worker for the stricter same-boot liveness conjunction. A tiny
// value (e.g. 1s) would make the age leg meaningless, so sub-floor values are
// rejected in favor of the default (#560). Transcript progress and runtime
// process identity remain independent mandatory legs before any job is failed.
const daemonRunningJobStaleFloor = 1 * time.Minute

// runtimeLeaseTeardownGrace is added to a job's timeout when sizing its
// runtime-session lock lease so the lease strictly OUTLIVES the run-context
// deadline plus worst-case terminal teardown (runtime subprocess kill + worktree
// force-clean). run() arms the run context at exactly jobTimeout but holds the
// lease through that teardown, which happens AFTER the deadline fires. Without
// this margin the lease would expire in the window [t0+jobTimeout,
// t0+jobTimeout+teardown] while the worker is STILL ALIVE finishing — and
// recoverExpiredRuntimeSessionLocks + DeleteExpiredResourceLocks (runtime:%
// bypasses the not-running guard) would reap that live worker's lock and fail
// its still-'running' owner, starting a SECOND worker on the dirty in-flight
// worktree: the exact #536 clobber. With the grace a NORMALLY-terminating worker
// always releases its lease before it expires; only a genuinely stuck/crashed
// worker past jobTimeout+grace is reaped and failed with recovery evidence.
const runtimeLeaseTeardownGrace = 2 * time.Minute

const daemonJobCancelPollInterval = 250 * time.Millisecond

const daemonWorkerLoopInterval = 1 * time.Second

// foreignBootRecoveryInterval keeps the reboot fast-path comfortably inside
// the one-minute minimum crash-backstop window while avoiding a global DB scan
// on every supervisor iteration.
const foreignBootRecoveryInterval = 15 * time.Second

// worktreeReclaimInterval keeps terminal-task and aged-job candidate queries
// out of the daemon's one-second hot path.
const worktreeReclaimInterval = 5 * time.Minute

// Task lane locks are cheap to inspect but are not part of the one-second hot
// path. The age floor is the safety boundary: event-driven cancellation may
// release immediately, while the unattended sweeper never touches a lane until
// it has been unchanged for at least a day.
const (
	staleTaskLaneLockReclaimInterval = 5 * time.Minute
	staleTaskLaneLockAgeFloor        = 24 * time.Hour
)

const (
	delegationReclaimFailureLogLimit  = 3
	delegationReclaimFailureMaxPaths  = 4096
	delegationReclaimFailureRetention = 24 * time.Hour
	delegationCleanupRetryBudget      = 3
	delegationCleanupRetryDelay       = time.Minute
)

// delegationCleanupPassError stops the current reclaim pass after a selected
// candidate cannot be durably accounted. The failure is already logged through
// the bounded per-path suppression seam; the outer worker tick continues
// ordinary dispatch. Candidate-query and other global store errors remain
// unwrapped and still abort the tick.
type delegationCleanupPassError struct {
	err error
}

func (e *delegationCleanupPassError) Error() string { return e.err.Error() }
func (e *delegationCleanupPassError) Unwrap() error { return e.err }

func stopDelegationCleanupPass(err error) error {
	if err == nil {
		return nil
	}
	return &delegationCleanupPassError{err: err}
}

type delegationReclaimFailure struct {
	count    int
	lastSeen time.Time
}

var delegationReclaimAccounting = struct {
	sync.Mutex
	failures map[string]delegationReclaimFailure
}{
	failures: map[string]delegationReclaimFailure{},
}

func delegationReclaimPath(jobID string, payload string) string {
	if parsed, err := workflow.ParseJobPayload(payload); err == nil {
		return canonicalDelegationReclaimPath(jobID, parsed.WorktreePath)
	}
	return canonicalDelegationReclaimPath(jobID, "")
}

func canonicalDelegationReclaimPath(jobID string, path string) string {
	if path = strings.TrimSpace(path); path != "" {
		return filepath.Clean(path)
	}
	return "job:" + strings.TrimSpace(jobID)
}

func delegationReclaimCandidateJob(ctx context.Context, worker jobWorker, jobID string) (db.Job, error) {
	if worker.ReclaimJobLookup != nil {
		return worker.ReclaimJobLookup(ctx, jobID)
	}
	return worker.Store.GetJob(ctx, jobID)
}

// recordDelegationReclaimFailure counts failures by worktree path and returns
// whether this attempt should be logged. A persistent poison pill therefore
// remains visible for its first few attempts without flooding every daemon tick.
func recordDelegationReclaimFailure(path string, now time.Time) (count int, log bool) {
	delegationReclaimAccounting.Lock()
	defer delegationReclaimAccounting.Unlock()
	entry := delegationReclaimAccounting.failures[path]
	entry.count++
	entry.lastSeen = now
	delegationReclaimAccounting.failures[path] = entry
	if len(delegationReclaimAccounting.failures) > delegationReclaimFailureMaxPaths {
		cutoff := now.Add(-delegationReclaimFailureRetention)
		for candidatePath, candidate := range delegationReclaimAccounting.failures {
			if candidate.lastSeen.Before(cutoff) {
				delete(delegationReclaimAccounting.failures, candidatePath)
			}
		}
		if len(delegationReclaimAccounting.failures) > delegationReclaimFailureMaxPaths {
			oldestPath := ""
			oldestSeen := now
			for candidatePath, candidate := range delegationReclaimAccounting.failures {
				if oldestPath == "" || candidate.lastSeen.Before(oldestSeen) {
					oldestPath = candidatePath
					oldestSeen = candidate.lastSeen
				}
			}
			delete(delegationReclaimAccounting.failures, oldestPath)
		}
	}
	return entry.count, entry.count <= delegationReclaimFailureLogLimit
}

func clearDelegationReclaimFailure(path string) {
	delegationReclaimAccounting.Lock()
	defer delegationReclaimAccounting.Unlock()
	delete(delegationReclaimAccounting.failures, path)
}

func logDelegationReclaimFailure(stdout io.Writer, mode string, phase string, jobID string, path string, err error) {
	count, shouldLog := recordDelegationReclaimFailure(path, time.Now().UTC())
	if !shouldLog {
		return
	}
	writeLine(stdout, "job %s %s delegation worktree reclaim failed path=%s phase=%s attempt=%d: %v", jobID, mode, path, phase, count, err)
	if count == delegationReclaimFailureLogLimit {
		writeLine(stdout, "job %s delegation worktree reclaim path=%s reached %d failures; further identical-path failures are suppressed", jobID, path, count)
	}
}
func logTaskWorktreeReclaimFailure(stdout io.Writer, taskID string, path string, err error) {
	path = filepath.Clean(path)
	count, shouldLog := recordDelegationReclaimFailure(path, time.Now().UTC())
	if !shouldLog {
		return
	}
	writeLine(stdout, "terminal task worktree reclaim failed task=%s path=%s attempt=%d: %v", taskID, path, count, err)
	if count == delegationReclaimFailureLogLimit {
		writeLine(stdout, "terminal task worktree reclaim task=%s path=%s reached %d failures; further identical-path failures are suppressed", taskID, path, count)
	}
}

func logTaskWorktreeRetention(stdout io.Writer, taskID string, path string, classification workflow.TaskWorktreeReclaimClassification, malformedJobID string) {
	if classification == "" {
		return
	}
	path = filepath.Clean(path)
	key := path + "|retained|" + string(classification)
	count, shouldLog := recordDelegationReclaimFailure(key, time.Now().UTC())
	if !shouldLog {
		return
	}
	detail := ""
	if classification == workflow.TaskWorktreeReclaimActiveOwner && strings.TrimSpace(malformedJobID) != "" {
		detail = " malformed_non_final_job=" + strings.TrimSpace(malformedJobID)
	}
	writeLine(stdout, "terminal task worktree retained task=%s path=%s classification=%s observation=%d%s", taskID, path, classification, count, detail)
	if count == delegationReclaimFailureLogLimit {
		writeLine(stdout, "terminal task worktree retention task=%s path=%s classification=%s reached %d observations; further identical retention messages are suppressed", taskID, path, classification, count)
	}
}

func recordDelegationCleanupFailure(ctx context.Context, worker jobWorker, mode, phase, jobID, path string, err error, now time.Time) (db.CleanupObligation, error) {
	reason := db.ClassifyCleanupObligationFailure(phase, err)
	obligation, persistErr := worker.Store.RecordCleanupObligationFailure(
		context.WithoutCancel(ctx), jobID, path, reason, err, now,
		now.Add(delegationCleanupRetryDelay), delegationCleanupRetryBudget,
	)
	if persistErr != nil {
		logDelegationReclaimFailure(worker.Stdout, mode, phase, jobID, path, fmt.Errorf("%w (persist cleanup obligation: %v)", err, persistErr))
		return db.CleanupObligation{}, fmt.Errorf("persist cleanup obligation for %s: %w", jobID, persistErr)
	}
	logDelegationReclaimFailure(worker.Stdout, mode, phase, jobID, path, err)
	if obligation.State == db.CleanupObligationQuarantined && obligation.AttemptCount == delegationCleanupRetryBudget {
		_ = worker.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{
			JobID: jobID,
			Kind:  "delegation_worktree_cleanup_quarantined",
			Message: fmt.Sprintf("cleanup obligation %s quarantined after %d attempts: reason=%s path=%s",
				obligation.ResourceID, obligation.AttemptCount, obligation.Reason, obligation.ExpectedPath),
		})
	}
	return obligation, nil
}

// deferDelegationCleanupContention reschedules a candidate whose only problem was
// a busy checkout lock. Contention is not failure: counting it spends the
// three-attempt retry budget and quarantines an obligation that would have
// succeeded as soon as the lock cleared.
func deferDelegationCleanupContention(ctx context.Context, worker jobWorker, mode, jobID, path string, err error, now time.Time) error {
	if _, deferErr := worker.Store.DeferCleanupObligation(
		context.WithoutCancel(ctx), jobID, path, db.CleanupReasonCheckoutLock, now, now.Add(delegationCleanupRetryDelay),
	); deferErr != nil {
		logDelegationReclaimFailure(worker.Stdout, mode, "lock", jobID, path, fmt.Errorf("%w (persist cleanup obligation: %v)", err, deferErr))
		return fmt.Errorf("defer contended cleanup obligation for %s: %w", jobID, deferErr)
	}
	logDelegationReclaimFailure(worker.Stdout, mode, "lock", jobID, path, err)
	return nil
}

// deferDelegationCleanupSkip moves a selected-but-unattempted row behind the
// bounded host window. The durable next_attempt_at is the fairness cursor: repo
// filters, session filters, non-final state, and held checkouts must not leave the
// same 256 rows monopolizing every tick after a daemon restart.
func deferDelegationCleanupSkip(ctx context.Context, worker jobWorker, jobID, path string, reason db.CleanupObligationReason, now time.Time) error {
	_, err := worker.Store.DeferCleanupObligation(
		context.WithoutCancel(ctx), jobID, path, reason, now, now.Add(delegationCleanupRetryDelay),
	)
	return err
}

func delegationCleanupContended(err error) bool {
	var blocked workflow.BlockedError
	return errors.As(err, &blocked)
}

func delegationCleanupTargetContained(worker jobWorker, job db.Job, obligation db.CleanupObligation) error {
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		return fmt.Errorf("parse cleanup owner payload: %w", err)
	}
	path := filepath.Clean(strings.TrimSpace(payload.WorktreePath))
	if path == "." || path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("cleanup path %q is not absolute", payload.WorktreePath)
	}
	if path != filepath.Clean(obligation.ExpectedPath) {
		return fmt.Errorf("cleanup path %q does not match obligation %q", path, obligation.ExpectedPath)
	}
	return workflow.ValidateDelegationCleanupTarget(worker.workflowHome(), job.ID, job.Type, payload)
}

func prepareDelegationCleanup(ctx context.Context, worker jobWorker, mode string, job db.Job, path string, now time.Time) (db.CleanupObligation, bool, error) {
	obligation, err := worker.Store.EnsureCleanupObligation(context.WithoutCancel(ctx), job.ID, path, now)
	if err != nil {
		logDelegationReclaimFailure(worker.Stdout, mode, "obligation", job.ID, path, err)
		return db.CleanupObligation{}, false, err
	}
	if obligation.State == db.CleanupObligationQuarantined || obligation.State == db.CleanupObligationRemoved {
		return obligation, false, nil
	}
	if obligation.State != db.CleanupObligationPending && obligation.State != db.CleanupObligationRetryable {
		return obligation, false, fmt.Errorf("cleanup obligation %s has unsupported state %q", obligation.ResourceID, obligation.State)
	}
	if err := delegationCleanupTargetContained(worker, job, obligation); err != nil {
		if _, persistErr := recordDelegationCleanupFailure(ctx, worker, mode, "identity", job.ID, path, err, now); persistErr != nil {
			return obligation, false, persistErr
		}
		return obligation, false, nil
	}
	return obligation, true, nil
}

func finishDelegationCleanupAttempt(ctx context.Context, worker jobWorker, jobID, path string, reclaimed bool, now time.Time) error {
	if reclaimed {
		if _, err := worker.Store.MarkCleanupObligationRemoved(context.WithoutCancel(ctx), jobID, path, now); err != nil {
			logDelegationReclaimFailure(worker.Stdout, "state", "removed", jobID, path, err)
			return err
		}
		clearDelegationReclaimFailure(path)
		return nil
	}
	if _, err := worker.Store.DeferCleanupObligation(context.WithoutCancel(ctx), jobID, path, db.CleanupReasonTerminalDeferred, now, now.Add(delegationCleanupRetryDelay)); err != nil {
		logDelegationReclaimFailure(worker.Stdout, "state", "deferred", jobID, path, err)
		return err
	}
	return nil
}

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
// progress before the daemon checks whether the worker crashed and fails it. A
// sub-floor value (e.g. 1s) would let the backstop fail live workers — most
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

// recoverForeignBootRunners is the #651 cross-boot recovery pass, run eagerly at
// daemon startup and then on a throttled, once-per-supervisor-sweep cadence. When
// this host's boot id differs from the boot id recorded on a running job / held
// runtime-session lock, that owner was claimed on a PREVIOUS boot and its
// in-process worker died when the host rebooted — so it is recovered promptly,
// regardless of any runtime-session lease (which survives a reboot in the DB and
// would otherwise keep the job "held" until it expired: the AC2 gap #536's lease
// gate cannot close by itself).
//
// It fails the foreign-boot running jobs (this covers non-resumable/shell jobs
// too, which hold no lease at all) and reclaims their foreign-boot runtime-session
// locks. Work consumed before a daemon restart is never silently run twice; an
// operator can inspect the durable evidence and explicitly retry.
// It is a STRICT no-op off Linux (BootID()=="") — preserving today's age/lease
// behavior — and never touches a SAME-boot owner, so a live in-process worker is
// never double-run (the #536 protection is untouched).
func recoverForeignBootRunners(ctx context.Context, store *db.Store, stdout io.Writer) error {
	return recoverForeignBootRunnersWithGet(ctx, store, stdout, store.GetJob)
}

func recoverForeignBootRunnersWithGet(ctx context.Context, store *db.Store, stdout io.Writer, getJob func(context.Context, string) (db.Job, error)) error {
	bootID := db.BootID()
	if bootID == "" {
		return nil
	}
	ids, err := store.ListRunningJobIDsFromForeignBoot(ctx, bootID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		job, err := getJob(ctx, id)
		if err != nil {
			writeLine(stdout, "foreign-boot recovery: skipping candidate %s after row lookup failed: %v", id, err)
			continue
		}
		if _, err := failRecoveredRunningJob(ctx, store, stdout, time.Now().UTC(), job, "daemon restart: runner belonged to a previous host boot"); err != nil {
			return err
		}
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

// recoverForeignBootRunnersForSweep is a test seam for pinning the
// supervisor-level call frequency. Production never reassigns it.
var recoverForeignBootRunnersForSweep = recoverForeignBootRunners

func runForeignBootRecoveryOnce(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, tracker *inflightJobTracker) error {
	if !tracker.foreignBootRecoveryDue(now) {
		return nil
	}
	if err := recoverForeignBootRunnersForSweep(ctx, store, stdout); err != nil {
		return err
	}
	tracker.markForeignBootRecoverySuccessful(now)
	return nil
}

// recoverExpiredRuntimeSessionLocksSkipping is recoverExpiredRuntimeSessionLocks
// with an in-flight owner exclusion (#562): a lock whose owner job is currently
// being run BY THIS PROCESS is neither failed nor reaped even if its lease has
// expired — the owning goroutine is still alive (a ctx-deaf runtime overrunning
// its timeout), and releasing its lock would let a second run of the same
// session start beside it (the #536 hazard). A nil skip set is byte-identical
// to the unskipped recovery.
func recoverExpiredRuntimeSessionLocksSkipping(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, skipOwners map[string]bool) error {
	expiredRuntimeLocks, err := store.ListExpiredRuntimeSessionLocks(ctx, now)
	if err != nil {
		return err
	}
	// Fail owners BEFORE reaping the lock rows. An expired runtime lease means
	// the job's real timeout + teardown grace has elapsed, so a still-'running'
	// owner is genuinely stale (a normally-terminating worker releases its lock
	// before the grace-padded lease expires — see runtimeLeaseTeardownGrace).
	// Ordering fail-then-delete keeps the two durable: if a mid-loop DB error
	// aborts the sweep, the un-processed locks are still expired and get retried
	// next tick, instead of being deleted up front and losing the recovery signal
	// (which would strand those owners as 'running' until the coarse 30m window).
	preserveOwners := make(map[string]bool, len(skipOwners))
	for owner, skip := range skipOwners {
		if skip {
			preserveOwners[owner] = true
		}
	}
	for _, lock := range expiredRuntimeLocks {
		owner := strings.TrimSpace(lock.OwnerJobID)
		if owner == "" || skipOwners[owner] {
			if owner != "" {
				preserveOwners[owner] = true
			}
			continue
		}
		job, err := store.GetJob(ctx, owner)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if isSessionRecordedJob(job) && job.State == string(workflow.JobRunning) {
			preserveOwners[owner] = true
			continue
		}
		if _, err := failRecoveredRunningJob(ctx, store, stdout, now, job, "stale recovery: runtime session lock expired with no in-process worker"); err != nil {
			return err
		}
	}
	deleted, err := store.DeleteExpiredResourceLocksExcludingOwners(ctx, now, sortedStringSetKeys(preserveOwners))
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
	jobs, err := store.ListJobsByState(ctx, string(workflow.JobBlocked))
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
	return recoverRunningJobsBeforeForRepoSkippingWithGet(ctx, store, stdout, now, before, repoFilter, rootFilter, skipJobs, store.GetJob)
}

func recoverRunningJobsBeforeForRepoSkippingWithGet(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, before time.Time, repoFilter string, rootFilter string, skipJobs map[string]bool, getJob func(context.Context, string) (db.Job, error)) error {
	jobs, err := store.ListRunningJobsUpdatedBefore(ctx, before)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if skipJobs[job.ID] || isSessionRecordedJob(job) {
			continue
		}
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			continue
		}
		fresh, err := getJob(ctx, job.ID)
		if err != nil {
			writeLine(stdout, "stale-running recovery: skipping candidate %s after row lookup failed: %v", job.ID, err)
			continue
		}
		if err := recoverRunningJobIfLeaseExpired(ctx, store, stdout, now, fresh); err != nil {
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
	// tick fails it) rather than at the 30m threshold — promptness traded for
	// never failing live work, the unattended-reliability goal of #536.
	leaseHeld, err := runtimeOwnerLeaseHeld(ctx, store, job.ID, now)
	if err != nil {
		return err
	}
	if leaseHeld {
		return nil
	}
	_, err = failRecoveredRunningJob(ctx, store, stdout, now, job, "daemon restart/stale recovery found no live runtime lease")
	return err
}

const jobRecoveryFailedEvent = "job_recovery_failed"

func isSessionRecordedJob(job db.Job) bool {
	return job.ExternallyDriven || strings.HasPrefix(strings.TrimSpace(job.ID), "session-")
}

// failRecoveredRunningJob is the single terminal-write seam for daemon-owned
// crash recovery. It preserves a bounded, redacted transcript tail and elapsed
// time in both the payload and loud audit event, clears stale process identity,
// releases locks, and refuses to touch externally recorded session jobs.
func failRecoveredRunningJob(ctx context.Context, store *db.Store, stdout io.Writer, now time.Time, job db.Job, cause string) (bool, error) {
	if job.State != string(workflow.JobRunning) || isSessionRecordedJob(job) {
		return false, nil
	}
	tail := recoveredJobLogTail(store, job.ID)
	elapsed := recoveredJobElapsed(now, job)
	cause = strings.TrimSpace(cause)
	message := fmt.Sprintf("daemon recovery failed running job: cause=%s; elapsed=%s", cause, elapsed)
	if tail != "" {
		message += "\nlast log tail:\n" + tail
	}

	encoded := job.Payload
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err == nil {
		payload.RuntimePID = 0
		payload.RuntimePIDStartTime = ""
		payload.RuntimePGID = 0
		payload.FailureDiagnostics = &workflow.FailureDiagnostics{Phase: workflow.FailurePhaseRecovery, StderrTail: tail}
		payload.Result = &workflow.AgentResult{
			Decision: "failed", Summary: fmt.Sprintf("daemon recovery: %s (elapsed %s)", cause, elapsed),
			Findings: []json.RawMessage{}, ChangesMade: []string{}, TestsRun: []string{}, Needs: []string{}, Delegations: []workflow.Delegation{},
		}
		if b, err := json.Marshal(payload); err == nil {
			encoded = string(b)
		}
	}
	transitioned, err := store.TransitionJobStatePayloadWithEvent(ctx, job.ID, string(workflow.JobRunning), string(workflow.JobFailed), encoded, db.JobEvent{
		JobID: job.ID, Kind: jobRecoveryFailedEvent, Message: message,
	})
	if err != nil {
		return false, err
	}
	if transitioned {
		_, _ = store.DeleteResourceLocksByOwnerIfNotRunning(ctx, job.ID, time.Now().UTC())
		writeLine(stdout, "failed recovered running job %s: %s", job.ID, cause)
	}
	return transitioned, nil
}

func recoveredJobElapsed(now time.Time, job db.Job) string {
	// updated_at is the last durable row write, not an immutable claim timestamp.
	// Report a conservative recovery age from that evidence rather than presenting
	// it as total wall-clock execution time; created_at is only a legacy fallback.
	started := parseJobTimeMillis(job.UpdatedAt)
	if started == 0 {
		if parsed, err := time.Parse("2006-01-02 15:04:05 -0700 MST", strings.TrimSpace(job.UpdatedAt)); err == nil {
			started = parsed.UnixMilli()
		}
	}
	if started == 0 {
		started = parseJobTimeMillis(job.CreatedAt)
	}
	if started == 0 {
		return "unknown"
	}
	d := now.Sub(time.UnixMilli(started))
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

func recoveredJobLogTail(store *db.Store, jobID string) string {
	logsDir := filepath.Join(filepath.Dir(store.DatabasePath()), config.LogsDir)
	f, err := os.Open(transcript.ResolveJobLogPath(logsDir, jobID))
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	const scanBytes = 64 * 1024
	start := info.Size() - scanBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, info.Size()-start)
	n, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	redacted := workflow.RedactCommentText(strings.TrimSpace(string(buf[:n])))
	if len(redacted) <= workflow.MaxStderrTailBytes {
		return redacted
	}
	cut := redacted[len(redacted)-workflow.MaxStderrTailBytes:]
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return cut
}

// tickCandidates memoizes the five per-tick global candidate queries
// (advance retry, comment retry, delegation cleanup, aged job cleanup, and
// terminal task cleanup) so they run once per supervisor tick instead of once
// per enabled repo (#619). Each query takes no repo argument and returns a
// global candidate set that the per-repo pass filters in Go. Before the hoist,
// the multi-repo supervisor invoked each query once per enabled repo. The most
// expensive query materialized about 23.67 MiB of row fetches per call.
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
	TaskIDsWithTerminalWorktree(ctx context.Context) ([]string, error)
	FirstMalformedNonFinalJob(ctx context.Context) (string, error)
	MaxJobEventID(ctx context.Context) (int64, error)
}

// candidateMemo lazily runs one per-tick candidate query and shares its RESULT
// across the tick's repos, memoizing ONLY a success: get caches the ids on the first
// successful fetch and returns them on every later call, but on a query error it
// returns the error and leaves the memo unset so the next call RE-RUNS fetch
// (retry-on-error — see tickCandidates for why per-repo fault isolation matters). It
// is consumed only on the synchronous tick goroutine, so it needs no synchronization.
type candidateMemo struct {
	attempted bool
	done      bool
	ids       []string
}

func (m *candidateMemo) get(fetch func() ([]string, error)) ([]string, error) {
	m.attempted = true
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
	store tickCandidateStore
	// config carries the tick's config.toml memo (#1758). It rides the candidate
	// carrier for the same reason the candidate sets do: the carrier is created
	// once per tick and consumed by every repo in the sweep, so a value resolved
	// here is resolved exactly once per tick instead of once per repo.
	config             tickConfigCache
	advance            candidateMemo
	comment            candidateMemo
	reclaim            candidateMemo
	agedReclaim        candidateMemo
	taskReclaim        candidateMemo
	malformedOwnerDone bool
	malformedOwnerID   string
	skipAgedReclaim    bool
	// events is the supervisor's CROSS-tick job_events cursor cache (#1758). It
	// outlives the carrier (it hangs off the tracker), which is exactly why it is
	// a pointer while config is a value: the config memo must die with the tick,
	// the cursor must survive it. nil disables cursor gating entirely.
	events *jobEventCandidateCache
}

// newTickCandidates is a package var (not a plain func) only so the once-per-tick
// regression test can substitute a carrier backed by a counting store; production
// never reassigns it.
var newTickCandidates = func(store tickCandidateStore) *tickCandidates {
	return &tickCandidates{store: store}
}

// jobEventCandidateKind indexes the candidate sets whose value is a pure function
// of job_events and can therefore be gated on that table's change cursor.
type jobEventCandidateKind int

const (
	jobEventCandidateAdvance jobEventCandidateKind = iota
	jobEventCandidateComment
	jobEventCandidateKindCount
)

// jobEventCandidateCache carries the advance- and comment-retry candidate sets
// ACROSS ticks, keyed by db.MaxJobEventID. Both queries GROUP BY job_id over the
// whole of job_events (92K rows on the affected host and growing without bound)
// and ran every ~2.5s whether or not a single event had been written (#1758).
//
// The cursor is only ever a permission to SKIP a query whose answer provably
// cannot have changed — never a substitute for one. Two properties make that
// safe, and the second is the one worth guarding:
//
//  1. Soundness of the key: see db.MaxJobEventID. Both gated queries read only
//     (id, kind, job_id), which no production write mutates in place, and rows are
//     otherwise append-only — so an unmoved maximum means an unchanged result.
//  2. Ordering: the cursor is read BEFORE the query it guards. An event written
//     between the two is then either seen by the query (fine) or not — but either
//     way the RECORDED cursor pre-dates it, so the next tick sees a moved cursor
//     and re-runs. Recording the cursor after the query would let exactly that
//     event be skipped forever. The worst case is therefore a candidate deferred
//     by one tick, never dropped.
//
// It is guarded by a mutex rather than relying on the tick goroutine: the carrier
// is per-tick and single-threaded, but this cache is shared by every carrier the
// supervisor makes, and the single-repo and fleet loops both reach it.
type jobEventCandidateCache struct {
	mu      sync.Mutex
	entries [jobEventCandidateKindCount]jobEventCandidateEntry
}

type jobEventCandidateEntry struct {
	cursor int64
	valid  bool
	ids    []string
}

func (c *jobEventCandidateCache) lookup(kind jobEventCandidateKind, cursor int64) ([]string, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[kind]
	if !entry.valid || entry.cursor != cursor {
		return nil, false
	}
	return entry.ids, true
}

func (c *jobEventCandidateCache) remember(kind jobEventCandidateKind, cursor int64, ids []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[kind] = jobEventCandidateEntry{cursor: cursor, valid: true, ids: ids}
}

// jobEventCandidates runs one job_events-derived candidate query, skipping it when
// the change cursor has not moved since the last tick that ran it. Any cursor read
// failure falls through to the query: the cache may only ever remove redundant
// work, so a broken cursor must degrade to today's behavior, not to a skip.
func (c *tickCandidates) jobEventCandidates(ctx context.Context, kind jobEventCandidateKind, fetch func() ([]string, error)) ([]string, error) {
	if c.events == nil {
		return fetch()
	}
	cursor, err := c.store.MaxJobEventID(ctx)
	if err != nil {
		return fetch()
	}
	if ids, ok := c.events.lookup(kind, cursor); ok {
		return ids, nil
	}
	ids, err := fetch()
	if err != nil {
		return nil, err
	}
	c.events.remember(kind, cursor, ids)
	return ids, nil
}

func (c *tickCandidates) advanceRetryCandidates(ctx context.Context) ([]string, error) {
	return c.advance.get(func() ([]string, error) {
		return c.jobEventCandidates(ctx, jobEventCandidateAdvance, func() ([]string, error) {
			return c.store.JobIDsWithPendingAdvanceRetry(ctx)
		})
	})
}

func (c *tickCandidates) commentRetryCandidates(ctx context.Context) ([]string, error) {
	return c.comment.get(func() ([]string, error) {
		return c.jobEventCandidates(ctx, jobEventCandidateComment, func() ([]string, error) {
			return c.store.JobIDsWithPendingCommentRetry(ctx)
		})
	})
}

func (c *tickCandidates) delegationReclaimCandidates(ctx context.Context) ([]string, error) {
	return c.reclaim.get(func() ([]string, error) {
		return c.store.JobIDsWithPendingDelegationWorktreeReclaim(ctx)
	})
}

func (c *tickCandidates) agedDelegationReclaimCandidates(ctx context.Context, cutoff time.Time) ([]string, error) {
	if c.skipAgedReclaim {
		return nil, nil
	}
	return c.agedReclaim.get(func() ([]string, error) {
		return c.store.JobIDsWithAgedTerminalDelegationWorktree(ctx, cutoff)
	})
}

func (c *tickCandidates) terminalTaskWorktreeCandidates(ctx context.Context) ([]string, error) {
	if c.skipAgedReclaim {
		return nil, nil
	}
	return c.taskReclaim.get(func() ([]string, error) {
		return c.store.TaskIDsWithTerminalWorktree(ctx)
	})
}

func (c *tickCandidates) firstMalformedNonFinalJob(ctx context.Context) (string, error) {
	if c.malformedOwnerDone {
		return c.malformedOwnerID, nil
	}
	id, err := c.store.FirstMalformedNonFinalJob(ctx)
	if err != nil {
		return "", err
	}
	c.malformedOwnerID = id
	c.malformedOwnerDone = true
	return id, nil
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
			writeLine(worker.Stdout, "job %s pending advancement retry failed: %v", job.ID, err)
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
func reclaimSkippedDelegationWorktrees(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, checkoutHeld func(string) bool, cand *tickCandidates, now time.Time) error {
	jobIDs, err := cand.delegationReclaimCandidates(ctx)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		job, err := delegationReclaimCandidateJob(ctx, worker, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			// A cleanup-marker event with no surviving job row (e.g. a pruned job):
			// nothing to reclaim, and erroring here would abort the whole tick.
			continue
		}
		if err != nil {
			path, pathErr := worker.Store.JobWorktreePath(ctx, jobID)
			if errors.Is(pathErr, sql.ErrNoRows) {
				continue
			}
			if pathErr != nil {
				return fmt.Errorf("get skipped reclaim candidate %s: %w (path lookup also failed: %v)", jobID, err, pathErr)
			}
			path = canonicalDelegationReclaimPath(jobID, path)
			if _, persistErr := recordDelegationCleanupFailure(ctx, worker, "skipped", "get_job", jobID, path, err, now); persistErr != nil {
				return stopDelegationCleanupPass(persistErr)
			}
			continue
		}
		path := delegationReclaimPath(job.ID, job.Payload)
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			if err := deferDelegationCleanupSkip(ctx, worker, job.ID, path, db.CleanupReasonTerminalDeferred, now); err != nil {
				return stopDelegationCleanupPass(err)
			}
			continue
		}
		if !jobStateEligibleForWorktreeReclaim(job.State) {
			if err := deferDelegationCleanupSkip(ctx, worker, job.ID, path, db.CleanupReasonTerminalDeferred, now); err != nil {
				return stopDelegationCleanupPass(err)
			}
			continue
		}
		if checkoutHeld != nil && (checkoutHeld(queuedJobCheckoutKey(ctx, worker.Store, job)) ||
			(repoFilter != "" && checkoutHeld("repo:"+repoFilter))) {
			if err := deferDelegationCleanupSkip(ctx, worker, job.ID, path, db.CleanupReasonCheckoutLock, now); err != nil {
				return stopDelegationCleanupPass(err)
			}
			continue
		}
		_, ok, err := prepareDelegationCleanup(ctx, worker, "skipped", job, path, now)
		if err != nil {
			return stopDelegationCleanupPass(err)
		}
		if !ok {
			continue
		}
		runner, err := worker.subprocessRunnerForJob(job)
		if err != nil {
			if _, persistErr := recordDelegationCleanupFailure(ctx, worker, "skipped", "runner", job.ID, path, err, now); persistErr != nil {
				return stopDelegationCleanupPass(persistErr)
			}
			continue
		}
		engine := worker.workflowForJob(worker.delegationParentCheckout(ctx, job), runner)
		reclaimed, err := engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, jobID)
		if err != nil {
			if _, persistErr := recordDelegationCleanupFailure(ctx, worker, "skipped", "reclaim", job.ID, path, err, now); persistErr != nil {
				return stopDelegationCleanupPass(persistErr)
			}
			continue
		}
		if err := finishDelegationCleanupAttempt(ctx, worker, job.ID, path, reclaimed, now); err != nil {
			return stopDelegationCleanupPass(err)
		}
	}
	return nil
}

// reclaimAgedTerminalDelegationWorktrees closes the cleanup crash window that
// has no _cleanup_skipped marker. The bounded store query selects only FINAL
// owners older than the configured TTL. Re-verification happens both here and
// in Engine.ReclaimAgedTerminalDelegationWorktreeOutcome; blocked/queued/running jobs
// are never force-removed.
func reclaimAgedTerminalDelegationWorktrees(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, checkoutHeld func(string) bool, cand *tickCandidates, now time.Time, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	jobIDs, err := cand.agedDelegationReclaimCandidates(ctx, now.Add(-ttl))
	if err != nil {
		return err
	}
	attempts := 0
	for _, jobID := range jobIDs {
		// Each attempt can run a remote fetch under a two-minute deadline and a
		// full-clone removal, so the pass is bounded per tick. Every attempted or
		// skipped row persists a later next_attempt_at; the ordered SQL window
		// therefore advances without an in-memory rotation cursor.
		if attempts >= terminalTaskWorktreeReclaimPassBudget {
			break
		}
		job, err := delegationReclaimCandidateJob(ctx, worker, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			path, pathErr := worker.Store.JobWorktreePath(ctx, jobID)
			if errors.Is(pathErr, sql.ErrNoRows) {
				continue
			}
			if pathErr != nil {
				return fmt.Errorf("get aged reclaim candidate %s: %w (path lookup also failed: %v)", jobID, err, pathErr)
			}
			path = canonicalDelegationReclaimPath(jobID, path)
			if _, persistErr := recordDelegationCleanupFailure(ctx, worker, "aged", "get_job", jobID, path, err, now); persistErr != nil {
				return stopDelegationCleanupPass(persistErr)
			}
			continue
		}
		path := delegationReclaimPath(job.ID, job.Payload)
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			if err := deferDelegationCleanupSkip(ctx, worker, job.ID, path, db.CleanupReasonTerminalDeferred, now); err != nil {
				return stopDelegationCleanupPass(err)
			}
			continue
		}
		if !workflow.IsFinalJobState(job.State) {
			if err := deferDelegationCleanupSkip(ctx, worker, job.ID, path, db.CleanupReasonTerminalDeferred, now); err != nil {
				return stopDelegationCleanupPass(err)
			}
			continue
		}
		if checkoutHeld != nil && (checkoutHeld(queuedJobCheckoutKey(ctx, worker.Store, job)) ||
			(repoFilter != "" && checkoutHeld("repo:"+repoFilter))) {
			if err := deferDelegationCleanupSkip(ctx, worker, job.ID, path, db.CleanupReasonCheckoutLock, now); err != nil {
				return stopDelegationCleanupPass(err)
			}
			continue
		}
		_, ok, err := prepareDelegationCleanup(ctx, worker, "aged", job, path, now)
		if err != nil {
			return stopDelegationCleanupPass(err)
		}
		if !ok {
			continue
		}
		runner, err := worker.subprocessRunnerForJob(job)
		if err != nil {
			if _, persistErr := recordDelegationCleanupFailure(ctx, worker, "aged", "runner", job.ID, path, err, now); persistErr != nil {
				return stopDelegationCleanupPass(persistErr)
			}
			continue
		}
		engine := worker.workflowForJob(worker.delegationParentCheckout(ctx, job), runner)
		attempts++
		reclaimed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, now.Add(-ttl))
		if err != nil {
			if delegationCleanupContended(err) {
				if deferErr := deferDelegationCleanupContention(ctx, worker, "aged", job.ID, path, err, now); deferErr != nil {
					return stopDelegationCleanupPass(deferErr)
				}
				continue
			}
			if _, persistErr := recordDelegationCleanupFailure(ctx, worker, "aged", "reclaim", job.ID, path, err, now); persistErr != nil {
				return stopDelegationCleanupPass(persistErr)
			}
			continue
		}
		if err := finishDelegationCleanupAttempt(ctx, worker, job.ID, path, reclaimed, now); err != nil {
			return stopDelegationCleanupPass(err)
		}
	}
	return nil
}

// terminalTaskWorktreeReclaimPassBudget caps how many candidates enter the
// engine's safety proof per tick. Each one takes the checkout mutation lock with
// the package's two-minute wait budget and runs two `git status --ignored`
// scans, and the candidate list has no age filter, so a host that accumulates
// permanently retained worktrees would otherwise spend the whole tick here
// ahead of dispatch.
const terminalTaskWorktreeReclaimPassBudget = 8

// terminalTaskWorktreeReclaimResume rotates the bounded window so a candidate
// that can never be reclaimed cannot starve the ones behind it: the pass resumes
// at the first id at or after the last pass's unreached candidate.
// The marker is keyed by repo filter. The candidate list is host-wide and every
// repo's pass walks all of it, skipping other repos' candidates for free, so one
// shared marker let a small repo's completed pass reset a large repo's window to
// the start on every tick and starve its tail forever.
var terminalTaskWorktreeReclaimResume = struct {
	sync.Mutex
	taskIDByRepo map[string]string
}{taskIDByRepo: map[string]string{}}

func rotateTerminalTaskWorktreeCandidates(repoFilter string, ids []string) []string {
	terminalTaskWorktreeReclaimResume.Lock()
	resume := terminalTaskWorktreeReclaimResume.taskIDByRepo[repoFilter]
	terminalTaskWorktreeReclaimResume.Unlock()
	if resume == "" || len(ids) == 0 {
		return ids
	}
	for i, id := range ids {
		if id >= resume {
			if i == 0 {
				return ids
			}
			rotated := make([]string, 0, len(ids))
			return append(append(rotated, ids[i:]...), ids[:i]...)
		}
	}
	return ids
}

func setTerminalTaskWorktreeReclaimResume(repoFilter string, taskID string) {
	terminalTaskWorktreeReclaimResume.Lock()
	defer terminalTaskWorktreeReclaimResume.Unlock()
	if taskID == "" {
		delete(terminalTaskWorktreeReclaimResume.taskIDByRepo, repoFilter)
		return
	}
	terminalTaskWorktreeReclaimResume.taskIDByRepo[repoFilter] = taskID
}

// reclaimTerminalTaskWorktrees removes task-owned worktrees based on terminal
// lifecycle state, never age. Each candidate is independently safety-checked by
// the workflow engine; item failures are logged and left for the next bounded
// pass so maintenance cannot suppress dispatch.
func reclaimTerminalTaskWorktrees(ctx context.Context, worker jobWorker, repoFilter string, rootFilter string, checkoutHeld func(string) bool, cand *tickCandidates, stdout io.Writer) error {
	if strings.TrimSpace(rootFilter) != "" {
		return nil
	}
	taskIDs, err := cand.terminalTaskWorktreeCandidates(ctx)
	if err != nil {
		return err
	}
	if len(taskIDs) == 0 {
		return nil
	}
	home := worker.workflowHome()
	if strings.TrimSpace(home) == "" {
		return errors.New("resolve Gitmoot home for terminal task worktree reclaim")
	}
	malformedJobID, malformedErr := cand.firstMalformedNonFinalJob(ctx)
	if malformedErr != nil {
		writeLine(stdout, "terminal task worktree reclaim could not identify malformed non-final owner: %v", malformedErr)
	}
	rotated := rotateTerminalTaskWorktreeCandidates(repoFilter, taskIDs)
	attempts := 0
	resume := ""
	for i, taskID := range rotated {
		if attempts >= terminalTaskWorktreeReclaimPassBudget {
			resume = rotated[i]
			break
		}
		task, err := worker.Store.GetTask(ctx, taskID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			writeLine(stdout, "terminal task worktree reclaim candidate %s skipped: %v", taskID, err)
			continue
		}
		if repoFilter != "" && task.RepoFullName != repoFilter {
			continue
		}
		path := strings.TrimSpace(task.WorktreePath)
		if checkoutHeld != nil && (checkoutHeld("worktree:"+filepath.Clean(path)) || checkoutHeld("repo:"+task.RepoFullName)) {
			continue
		}
		repo, err := worker.Store.GetRepo(ctx, task.RepoFullName)
		if err != nil {
			writeLine(stdout, "terminal task worktree reclaim candidate %s has no registered repo checkout: %v", task.ID, err)
			continue
		}
		checkout := strings.TrimSpace(repo.CheckoutPath)
		if checkout == "" {
			writeLine(stdout, "terminal task worktree reclaim candidate %s has an empty registered repo checkout", task.ID)
			continue
		}
		engine := worker.workflowForHost(checkout)
		manager, ok := engine.DelegationWorktrees.(workflow.WritableWorktreeLineageManager)
		if !ok || manager == nil {
			writeLine(stdout, "terminal task worktree reclaim candidate %s has no writable worktree manager", task.ID)
			continue
		}
		attempts++
		outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, task.ID, manager)
		if err != nil {
			logTaskWorktreeReclaimFailure(stdout, task.ID, path, err)
			continue
		}
		clearDelegationReclaimFailure(filepath.Clean(path))
		switch {
		case outcome.Reclaimed:
			writeLine(stdout, "terminal task worktree reclaimed: task=%s path=%s classification=%s", task.ID, outcome.Path, outcome.Classification)
		case outcome.Classification == workflow.TaskWorktreeReclaimUnremovable || outcome.Classification == workflow.TaskWorktreeReclaimPathMismatch:
			writeLine(stdout, "terminal task worktree classified: task=%s path=%s classification=%s", task.ID, outcome.Path, outcome.Classification)
		default:
			logTaskWorktreeRetention(stdout, task.ID, outcome.Path, outcome.Classification, malformedJobID)
		}
	}
	setTerminalTaskWorktreeReclaimResume(repoFilter, resume)
	return nil
}

// reclaimStaleTaskLaneLocks releases only aged task lanes whose repo+branch has
// no non-terminal task or job. ReleaseBranchLockIfInactiveWithEvent rechecks all
// predicates atomically with the DELETE, so a concurrent resume or dispatch wins
// safely and keeps its lane. Candidate-local failures are logged and skipped;
// this optional housekeeping must never prevent queued-job dispatch.
func reclaimStaleTaskLaneLocks(ctx context.Context, store *db.Store, repoFilter string, stdout io.Writer, now time.Time) error {
	locks, err := store.ListBranchLocks(ctx, repoFilter)
	if err != nil {
		return err
	}
	cutoff := now.Add(-staleTaskLaneLockAgeFloor)
	for _, lock := range locks {
		released, err := store.ReleaseBranchLockIfInactiveWithEvent(ctx, lock, "", cutoff, db.BranchLockEvent{
			Kind: "released", Message: "released stale task lane with no non-terminal branch work (#1565)",
		})
		if err != nil {
			writeLine(stdout, "task lane lock reclaim failed for %s %s: %v", lock.RepoFullName, lock.Branch, err)
			continue
		}
		if released {
			writeLine(stdout, "released stale task lane lock %s %s", lock.RepoFullName, lock.Branch)
		}
	}
	return nil
}

// runDaemonWorkerTickTracked is the per-tick worker pass. With a nil tracker it
// follows the historical synchronous tick behavior: maintenance scans,
// then a BLOCKING runQueuedJobsForRepo dispatch. The supervisors pass a live
// tracker (#562), which changes the tick to claim-and-dispatch-async:
//
//   - same-boot stale-job detection requires age, two frozen transcript samples,
//     and dead runtime identity; a recorded live PID absolutely vetoes settlement,
//     including for an in-flight >30m shell-runtime job with no runtime lease;
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
	ownsCandidates := cand == nil
	// A nil carrier means this is a standalone tick (single-repo supervisor or
	// direct caller): compute the shared candidate sets once for this tick. The
	// multi-repo supervisor passes a carrier it created once per tick, so the
	// five global candidate queries run once rather than once per enabled repo.
	if cand == nil {
		cand = newTickCandidates(worker.Store)
		cand.events = tracker.jobEventCandidateCache()
		runWorktreeReclaim := tracker.worktreeReclaimDue(now)
		cand.skipAgedReclaim = !runWorktreeReclaim
		if runWorktreeReclaim {
			defer func() {
				if cand.taskReclaim.attempted || cand.agedReclaim.attempted {
					tracker.markWorktreeReclaimAttempted(now)
				}
			}()
		}
	}
	if ownsCandidates && tracker.staleTaskLaneLockReclaimDue(now) {
		if err := reclaimStaleTaskLaneLocks(ctx, store, repoFilter, stdout, now); err != nil {
			writeLine(stdout, "task lane lock reclaim failed: %v", err)
		} else {
			tracker.markStaleTaskLaneLockReclaimSuccessful(now)
		}
	}
	inflightIDs := tracker.inflightIDs()
	paths, pathsErr := worker.configPaths()
	if pathsErr != nil {
		return pathsErr
	}
	staleAfter := configuredDaemonRunningJobStaleAfter(stdout)
	quietAfter := configuredDaemonQuietKillAfter(worker.ConfigHome, stdout)
	liveness := newDaemonLivenessSweep()
	if tracker != nil && tracker.liveness != nil {
		liveness = tracker.liveness
	}
	if err := liveness.sweepRunningJobLiveness(ctx, store, stdout, paths, now, staleAfter, quietAfter, repoFilter, rootFilter); err != nil {
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
	workflowHome := worker.workflowHome()
	if err := sweepExpiredBlockedJobs(ctx, store, cand.config.blockedJobTTL(workflowHome), stdout, now); err != nil {
		writeLine(stdout, "blocked_ttl sweep failed: %v", err)
	}
	// Opt-in blocked-since source (#1060): stale BLOCKED tasks synthesize one
	// blocked event per continuous episode for the existing event-rule engine.
	// Like blocked_ttl, every failure is logged and swallowed so this optional
	// evaluator can never fail the repo tick.
	if err := sweepBlockedTaskWakeEvents(ctx, store, workflowHome, repoFilter, cand.config.blockedRoleWakeAfter(workflowHome), stdout, now); err != nil {
		writeLine(stdout, "blocked_since task sweep failed: %v", err)
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
		// Boundary: candidate scans and store/global faults still return through
		// these helpers and feed #555 escalation. Once a candidate is selected,
		// its optional housekeeping operation logs and continues so one bad item
		// can never prevent the dispatch below.
		if err := retryPendingJobAdvancements(ctx, worker, repoFilter, rootFilter, tracker.checkoutHeld, cand); err != nil {
			return err
		}
		if err := reclaimSkippedDelegationWorktrees(ctx, worker, repoFilter, rootFilter, tracker.checkoutHeld, cand, now); err != nil {
			var passErr *delegationCleanupPassError
			if !errors.As(err, &passErr) {
				return err
			}
		}
		// The store path is already the resolved Gitmoot root. Using it keeps this
		// hot-read on the daemon's actual home and prevents the raw/resolved
		// double-resolution bug class.
		if ttl, err := cand.config.delegationWorktreeTTL(filepath.Dir(store.DatabasePath())); err != nil {
			writeLine(stdout, "delegation_worktree_ttl reclaim skipped: %v", err)
		} else if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, repoFilter, rootFilter, tracker.checkoutHeld, cand, now, ttl); err != nil {
			writeLine(stdout, "delegation_worktree_ttl reclaim failed: %v", err)
		}
		if err := reclaimTerminalTaskWorktrees(ctx, worker, repoFilter, rootFilter, tracker.checkoutHeld, cand, stdout); err != nil {
			writeLine(stdout, "terminal task worktree reclaim failed: %v", err)
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
	var err error
	if tracker == nil {
		err = runQueuedJobsForRepo(ctx, worker, limit, repoFilter, rootFilter)
	} else {
		err = dispatchQueuedJobsTracked(ctx, worker, limit, workers, repoFilter, rootFilter, tracker)
	}
	if err != nil {
		return err
	}
	return nil
}

func runEnabledRepoWorkerTicksTracked(ctx context.Context, store *db.Store, worker jobWorker, workers int, rootFilter string, stdout io.Writer, now time.Time, locks *repoCheckoutLocks, tracker *inflightJobTracker) error {
	// #1200/#1201 durable addressed-note wakes belong to the shared store, not
	// any repository. Drain before listing repos so zero enabled repos cannot
	// suppress delivery or hide an unreadable outbox behind a healthy fleet tick.
	health, err := drainFleetReplyWakeOutbox(ctx, store, worker, now)
	switch {
	case err != nil:
		writeLine(stdout, "reply wake outbox drain unhealthy: %v", err)
		tracker.forgetReplyWakeOutboxHealth()
	case health.inert > 0:
		// Log on CHANGE only (#1758): inert obligations persist until an
		// operator adds a matching rule, so the unchanged line is pure noise.
		if tracker.replyWakeOutboxHealthChanged(health) {
			writeLine(stdout, "reply wake outbox drain health: %s", health)
		}
	default:
		tracker.forgetReplyWakeOutboxHealth()
	}
	if tracker.staleTaskLaneLockReclaimDue(now) {
		if err := reclaimStaleTaskLaneLocks(ctx, store, "", stdout, now); err != nil {
			writeLine(stdout, "task lane lock reclaim failed: %v", err)
		} else {
			tracker.markStaleTaskLaneLockReclaimSuccessful(now)
		}
	}
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return err
	}
	// Compute the shared per-tick candidate sets once for this whole sweep and
	// pass the carrier into every enabled repo's tick (#619). The five global
	// queries take no repo argument. Each repo's retry pass filters in Go, so
	// hoisting them here collapses 18 calls per query to one on the affected
	// multi-repo daemon. Fresh each sweep; never retained.
	cand := newTickCandidates(worker.Store)
	// The cursor cache is the one piece of candidate state that DOES span ticks:
	// it hangs off the tracker, and the fresh carrier borrows it for this sweep.
	cand.events = tracker.jobEventCandidateCache()
	runAgedReclaim := tracker.worktreeReclaimDue(now)
	cand.skipAgedReclaim = !runAgedReclaim
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
	if runAgedReclaim && (cand.taskReclaim.attempted || cand.agedReclaim.attempted) {
		tracker.markWorktreeReclaimAttempted(now)
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

func drainFleetReplyWakeOutbox(ctx context.Context, store *db.Store, worker jobWorker, now time.Time) (replyWakeOutboxHealth, error) {
	health, err := drainReplyWakeOutboxWithHealth(ctx, store, now, worker.replyWakeDelivery)
	if err != nil {
		return health, fmt.Errorf("reply wake outbox drain failed: %w", err)
	}
	return health, nil
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
			writeLine(worker.Stdout, "job %s pending result comment retry failed: %v", job.ID, err)
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
	// Both barrier and pool schedulers (including every continuous-pool re-query)
	// pass through this exact forDispatch path immediately before selecting work.
	// Returning an empty eligible set pauses dispatch without changing job state,
	// so low disk is retriable and queued work resumes automatically once healthy.
	if forDispatch && !diskGuardAllowsQueuedDispatch(ctx, worker, jobs, repoFilter, rootFilter) {
		return nil, nil
	}
	unavailableRows, err := worker.Store.ListActiveOrgRolesUnavailable(ctx, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	unavailableRoles := make(map[string]db.OrgRoleUnavailable, len(unavailableRows))
	for _, row := range unavailableRows {
		unavailableRoles[row.Role] = row
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
		// Provider-declared role unavailability (#1136): all queued work attributed
		// to that role is held until the reset boundary. ListActive... excludes
		// expired rows (the one-minute sweep removes them), so stale incidents
		// never suppress dispatch.
		if payload, payloadErr := daemonJobPayload(job); payloadErr == nil {
			if _, unavailable := unavailableRoles[strings.ToLower(strings.TrimSpace(payload.ActingOrgRole))]; unavailable {
				continue
			}
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

// poolRequeryInterval bounds how long a LIVE pool pass may wait on a job
// completion before looking at the queue again. It exists because a pass that
// only wakes on completions cannot see work enqueued after it parked, and the
// single-flight guard forbids a replacement pass while it lives.
//
// Two seconds: a queued job's worst-case invisibility becomes a bound the
// operator can state instead of "however long the longest running job takes"
// (measured starvation before the bound: 22m42s and 27m40s).
//
// Cost, stated precisely because the first two versions of this comment were
// wrong: per interval, per repo that has a job IN FLIGHT and nothing
// dispatchable, the pass re-pays the WHOLE top of the loop, not just one query.
// That is one listPendingQueuedJobs store scan, the admission estimate for every
// job the selector considers, AND one allocatePoolIsolationWorktree attempt per
// isolation-eligible blocked job — each of which can spend up to
// workflow.ReadOnlyWorktreeDispatchLockWaitBudget waiting on the checkout
// mutation lock. The isolation retries are therefore paced by
// poolIsolationRetryBackoff rather than by this interval; without that backoff a
// 2s interval and a 5s lock budget compose into a permanent lock-wait spin on a
// job that can never isolate. An EMPTY QUEUE does reach this seam whenever
// something is running — that is exactly what
// TestPoolRequeryBoundIsPacedAndDispatchesNothingWhenIdle constructs. What
// returns above is `running == 0 && dispatched == 0`, i.e. nothing running AND
// nothing dispatchable, so a fully idle daemon never re-queries. The store scan's
// cost grows with the queue, not an O(1) probe, and it is skipped entirely when
// the repo has no dispatch slots; the bound caps its FREQUENCY, not its size.
//
// A var, not a const, so tests can shorten it; production never reassigns it.
var poolRequeryInterval = 2 * time.Second

// poolIsolationRetryBackoff / poolIsolationRetryBackoffMax pace a FAILING
// allocatePoolIsolationWorktree retry inside one live pass. The retry cost is
// not the 2s re-query it rides on: each attempt can spend up to
// workflow.ReadOnlyWorktreeDispatchLockWaitBudget (5s) waiting on the checkout
// mutation lock, on the dispatcher goroutine, so an unpaced retry both exceeds
// the interval it is supposed to obey and keeps a lock the merge gate wants
// under permanent contention. The delay doubles from the first value to the max
// per consecutive failure of the SAME job, and is dropped as soon as that job
// leaves the pending set (allocated, dispatched, reaped, or gone), so a
// transient failure recovers on the next re-query while a job that can never
// isolate settles at one attempt per max interval.
//
// Vars, not consts, so tests can shorten them; production never reassigns them.
var (
	poolIsolationRetryBackoff    = 2 * time.Second
	poolIsolationRetryBackoffMax = 30 * time.Second
)

// poolRequeryObserver is a test hook fired when the bound (rather than a job
// completion) wakes the pass. nil in production. It lets a test distinguish
// "re-queries on the bound" from "re-queries in a tight loop", which a
// wall-clock assertion cannot.
var poolRequeryObserver func()

// poolIsolationAttemptObserver is a test hook fired immediately before each
// allocatePoolIsolationWorktree ATTEMPT. nil in production. It exists because
// the retry cost this backoff bounds is otherwise unobservable: the skip EVENT
// is throttled to one row per job, so an event count cannot distinguish "retried
// once" from "retried every re-query" — the two behaviours the backoff separates.
var poolIsolationAttemptObserver func(jobID string)

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
		runner       subprocess.Runner
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
	// isolationSkipLogged bounds the pool_isolation_skipped event to once per job
	// per invocation. The skip branch is reached on every re-query, and re-queries
	// are now paced by poolRequeryInterval instead of by completions, so writing it
	// unthrottled turned one row per completion into one row every two seconds for
	// as long as a job stayed blocked. Dispatcher-goroutine-owned like the sets
	// above, so no lock; reset each invocation, so a later pass reports the skip
	// again for a job that is still serialized.
	isolationSkipLogged := map[string]bool{}
	// isolationRetryNext / isolationRetryDelay pace a FAILING isolation allocation
	// for one job across the re-queries of THIS pass (see poolIsolationRetryBackoff).
	// Both are keyed by job id, pruned to the live pending set on every pass so a
	// long-lived pass cannot accumulate entries for jobs that have left the queue,
	// and dispatcher-goroutine-owned like the sets above, so no lock.
	isolationRetryNext := map[string]time.Time{}
	isolationRetryDelay := map[string]time.Duration{}
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
			_ = jobGitClient(f.repoCheckout, f.runner).RemoveWorktreeForce(context.WithoutCancel(ctx), f.worktreePath)
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
			// Ask for the budget BEFORE the scan. A saturated repo (every slot taken,
			// or the host-global remainder exhausted) cannot dispatch anything this
			// pass, and the old order paid a full listPendingQueuedJobs store scan
			// every poolRequeryInterval only to discard the result at the `slots > 0`
			// test. The wake still happens — the pass must stay alive to reap — it
			// just no longer re-scans a queue it is not allowed to draw from.
			slots := poolDispatchSlots(limit, running, hostCap, tracker)
			pending := []db.Job(nil)
			if slots > 0 {
				scanned, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter, true)
				if err != nil {
					firstErr = err
				} else {
					pending = scanned
				}
			}
			if firstErr == nil && slots > 0 {
				// Prune the isolation retry/skip state to the jobs still queued, BEFORE
				// excludeBouncedBusy and against the freshly scanned queue. This pass can
				// outlive many jobs (it holds poolRuns[repo] for as long as anything
				// runs), so without pruning the three maps would grow with every job id
				// the pass ever saw. Dropping a job also drops its backoff, which is the
				// intended semantics: a job that leaves the QUEUE and returns is a fresh
				// isolation attempt, not a continuation of an old penalty.
				//
				// The ORDER is load-bearing and the first version had it backwards. A job
				// that bounced runtime-busy this pass is REMOVED from pending, so pruning
				// against the post-exclusion list deleted that job's isolation penalty
				// even though it never waited the penalty out — and it would then re-pay
				// the 5s-lock-budget allocation immediately on its next appearance, which
				// is the spin the backoff exists to stop. A busy bounce and an isolation
				// failure are independent facts about a job; neither may clear the other.
				live := make(map[string]bool, len(pending))
				for _, job := range pending {
					live[job.ID] = true
				}
				for id := range isolationRetryNext {
					if !live[id] {
						delete(isolationRetryNext, id)
						delete(isolationRetryDelay, id)
					}
				}
				for id := range isolationSkipLogged {
					if !live[id] {
						delete(isolationSkipLogged, id)
					}
				}
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
					// Backoff gate FIRST, before anything that reads the store or
					// reserves admission. The reviewer measured the earlier placement:
					// 19 attempts over 400ms at a 20ms interval, each one still paying
					// queuedJobCheckoutKey, queuedJobRuntimeResourceKey and an admission
					// estimate before being turned away — so "backed off" cost almost as
					// much as attempting. daemonJobPayload and poolIsolationEligible are
					// pure functions of the job row already in hand, so they stay above
					// the gate; everything below it touches the store.
					if next, ok := isolationRetryNext[job.ID]; ok && time.Now().Before(next) {
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
					if poolIsolationAttemptObserver != nil {
						poolIsolationAttemptObserver(job.ID)
					}
					iso, ok, allocErr := worker.allocatePoolIsolationWorktree(ctx, job, payload)
					if !ok {
						worker.Admission.Release(job.ID)
						// #739: the reactive isolation was silent on failure — the exact
						// reason #739 was hard to diagnose (a seat went queued→running with no
						// worktree event and serialized on the shared checkout). Emit a loud
						// skip event so a lost-parallelism serialize is observable. A nil
						// allocErr means the job was simply not isolable (no home/checkout) —
						// not a failure — so stay quiet there.
						// Throttled to once per job per pass invocation: this branch is
						// reached on every retry, so an unthrottled write turned one event
						// per completion into one row per retry for as long as the job
						// stayed blocked. The flag MUST be set here — reading it without
						// ever writing it is a throttle that throttles nothing, which is
						// what the first version of this branch shipped.
						if allocErr != nil && !isolationSkipLogged[job.ID] {
							isolationSkipLogged[job.ID] = true
							_ = worker.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "pool_isolation_skipped", Message: fmt.Sprintf("pool read-only isolation skipped (#739); job stays serialized in the shared checkout: %v", allocErr)})
						}
						// Arm the backoff on EITHER outcome, and my earlier claim here was
						// wrong: I wrote that allocErr == nil ("not isolable") was "cheap
						// and stateless to re-test", and the reviewer measured otherwise —
						// 19 attempts over 400ms, each reaching this point only after two
						// or three store reads and an admission reserve/release pair.
						// Nothing about that is cheap, and the not-isolable verdict is a
						// stable property of the job and its repo, so re-testing it every
						// re-query buys nothing. A real FAILURE additionally paid the lock
						// wait, so it keeps the doubling curve; a not-isolable job is held
						// flat at the first interval, which stops the spin without
						// delaying a job whose repo config might legitimately change.
						delay := isolationRetryDelay[job.ID]
						switch {
						case allocErr == nil:
							delay = poolIsolationRetryBackoff
						case delay <= 0:
							delay = poolIsolationRetryBackoff
						case delay < poolIsolationRetryBackoffMax:
							delay *= 2
							if delay > poolIsolationRetryBackoffMax {
								delay = poolIsolationRetryBackoffMax
							}
						}
						isolationRetryDelay[job.ID] = delay
						isolationRetryNext[job.ID] = time.Now().Add(delay)
						continue
					}
					// Allocation succeeded: drop this job's backoff and skip-throttle so a
					// later blocked spell starts from a clean slate rather than inheriting
					// the penalty of an earlier one.
					delete(isolationRetryNext, job.ID)
					delete(isolationRetryDelay, job.ID)
					delete(isolationSkipLogged, job.ID)
					if !tracker.beginWithin(hostCap, iso.job.ID, repoFilter, iso.checkoutKey, iso.runtimeKey) {
						worker.Admission.Release(iso.job.ID)
						// Undo the allocation completely: the payload now points at the
						// isolation worktree being removed, and the job stays queued — a
						// host-cap or double-dispatch refusal must not strand it on a
						// reaped path.
						_ = worker.Store.UpdateJobPayload(context.WithoutCancel(ctx), iso.job.ID, payloadBeforeIsolation)
						_ = jobGitClient(iso.repoCheckout, iso.runner).RemoveWorktreeForce(context.WithoutCancel(ctx), iso.worktreePath)
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
						done <- finished{jobID: iso.job.ID, checkoutKey: iso.checkoutKey, runtimeKey: iso.runtimeKey, worktreePath: iso.worktreePath, repoCheckout: iso.repoCheckout, runner: iso.runner, payloadBeforeIsolation: payloadBeforeIsolation, err: runPoolJobRecovered(ctx, worker, iso.job)}
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
			// A job newly enqueued for this repo is progress that IS possible, and
			// a pass parked on `done` cannot see it: the old blind receive only
			// woke on a COMPLETION. That starved every later arrival for the
			// running job's remaining lifetime (measured: 22m42s and 27m40s waits
			// for a 3-second stage), and no replacement pass can cover for it
			// because this one still holds poolRuns[repo] via tryBeginPool while
			// the `running == 0` return above keeps it alive. Worse, it made the
			// owner-configured per-agent max_background unreachable for a second
			// same-repo job. So wake on a completion OR the re-query bound,
			// whichever comes first, and let the top of the loop re-query.
			//
			// Single-flight is untouched: the same dispatcher goroutine simply
			// looks again sooner, so no second selector exists and no two jobs
			// can be dispatched onto one checkout key.
			timer := time.NewTimer(poolRequeryInterval)
			select {
			case f := <-done:
				timer.Stop()
				reap(f)
			case <-timer.C:
				// Fire the observer ONLY when a re-query will actually follow. Once
				// firstErr is set (including a cancelled ctx) the top of the loop skips
				// the whole dispatch block and the pass is just draining in-flight
				// workers, so a wake here re-queries nothing. A test that counts these
				// wakes as re-queries would otherwise credit the bound with work it did
				// not do — the same class of false positive as an unwritten throttle.
				if firstErr == nil && ctx.Err() == nil && poolRequeryObserver != nil {
					poolRequeryObserver()
				}
			}
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
	runner       subprocess.Runner
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
	runner, err := w.subprocessRunnerForJob(job)
	if err != nil {
		return poolIsolatedDispatch{}, false, err
	}
	client := jobGitClient(repoRecord.CheckoutPath, runner)
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
	switch strings.TrimSpace(job.Type) {
	case "ask", "review":
		payload.ReadOnlySeat = true
	}
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
		runner:       runner,
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
	// A PR review's TaskID names the owning implementation task. Its task-table
	// worktree may therefore be the registered implementation checkout, never the
	// exact review head. Normal native and local review dispatches are born with an
	// owned payload WorktreePath; a legacy or misconfigured pathless review gets a
	// job-local scheduler key until the worker allocates that exact-head worktree.
	// It must not inherit either the task checkout key or repo:<repo>.
	if job.Type == "review" && payload.PullRequest > 0 && strings.TrimSpace(payload.HeadSHA) != "" {
		if path := strings.TrimSpace(payload.WorktreePath); path != "" {
			normalized, normalizeErr := normalizeTaskWorktreePath(path)
			if normalizeErr == nil && normalized != "" {
				return "worktree:" + normalized
			}
		}
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
