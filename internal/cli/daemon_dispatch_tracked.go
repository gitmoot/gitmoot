package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// This file is the #562 fix: job execution runs OFF the supervisor worker-tick /
// checkout-lock critical path, so one hung or multi-hour job can no longer wedge
// the whole daemon (queued jobs unclaimed, recovery scans stalled) while `daemon
// status` still reports running.
//
// The tick now CLAIMS runnable jobs and dispatches them to tracked goroutines,
// returning promptly so the loop keeps ticking. Correctness that inline
// execution used to provide by accident is preserved EXPLICITLY:
//
//   - same-repo serialization: an in-flight job's checkout/runtime keys are held
//     in the tracker and seeded into selectRunnableQueuedJobsSeeded, so a
//     same-repo no-worktree job is never dispatched beside the job occupying the
//     shared checkout (the barrier used to get this from one wg.Wait()).
//   - concurrency caps: dispatch slots are limit minus the repo's tracked
//     in-flight count, so --workers / per-repo max_parallel (#576) still bound
//     TOTAL in-flight jobs, not just per-batch ones — AND the shared tracker's
//     host-global count clamps the sum across repos, so a multi-repo daemon
//     never runs more than max(--workers, the repo's explicit override)
//     jobs host-wide (main's inline sweep gave this for free by blocking).
//   - poller exclusion: the poller only runs a full PollOnce when the tracker is
//     idle for the repo, so PollOnce never mutates a checkout under a running job
//     (previously guaranteed by the tick holding the checkout lock for the whole
//     run).
//   - maintenance exclusion: comment retries never touch a checkout and run
//     every tick (main's cadence); checkout-mutating maintenance (advancement
//     retries, worktree reclaims) is gated per candidate on the checkout key it
//     would mutate being free of in-flight holders — NOT on whole-repo
//     idleness, which a steady staggered backlog could prevent indefinitely,
//     starving merge retries forever. DB-only recovery scans keep running
//     every tick but skip jobs this process is itself running.
//   - #570 escalation: an async infra error (job-level failures already resolve
//     to nil inside worker.run) is recorded per repo and surfaced as the NEXT
//     tick's error, so a persistent store fault still trips the ceiling.
//   - graceful shutdown: every dispatched job runs on the tracker's runCtx;
//     drain() cancels it and waits (bounded) for in-flight jobs to finish.

// daemonShutdownDrainTimeout bounds how long the daemon waits for in-flight
// dispatched jobs to observe cancellation on shutdown before abandoning them
// (a truly ctx-deaf subprocess must not block daemon stop forever).
const daemonShutdownDrainTimeout = 15 * time.Second

// heldBackLogInterval throttles the per-job "held back" observability lines
// (#562 point 5): a job that stays excluded for the same reason re-logs at most
// once per interval instead of every ~1s tick.
const heldBackLogInterval = 5 * time.Minute

type inflightDispatch struct {
	repo        string
	checkoutKey string
	runtimeKey  string
}

// inflightJobTracker owns the cross-tick in-flight job accounting for a
// supervisor. All methods are nil-receiver safe (a nil tracker means "untracked
// / legacy inline path" and behaves as permanently idle).
type inflightJobTracker struct {
	mu        sync.Mutex
	wg        sync.WaitGroup
	runCtx    context.Context
	cancelRun context.CancelFunc
	draining  bool // set once drain() starts: no new work may begin
	jobs      map[string]inflightDispatch
	checkouts map[string]int // key -> holder count (counted to stay robust)
	runtimes  map[string]int
	perRepo   map[string]int
	poolRuns  map[string]bool // repos with a background pool pass in flight
	errs      map[string][]error
}

func newInflightJobTracker(ctx context.Context) *inflightJobTracker {
	runCtx, cancel := context.WithCancel(ctx)
	return &inflightJobTracker{
		runCtx:    runCtx,
		cancelRun: cancel,
		jobs:      map[string]inflightDispatch{},
		checkouts: map[string]int{},
		runtimes:  map[string]int{},
		perRepo:   map[string]int{},
		poolRuns:  map[string]bool{},
		errs:      map[string][]error{},
	}
}

// jobContext returns the context dispatched jobs must run on: cancelled when
// the supervisor context is cancelled OR drain() begins. A nil tracker falls
// back to the caller's context.
func (t *inflightJobTracker) jobContext(fallback context.Context) context.Context {
	if t == nil {
		return fallback
	}
	return t.runCtx
}

// beginWithin is begin with an ATOMIC host-global admission check: when
// hostCap > 0 it refuses (registering nothing) if the tracker already has
// hostCap jobs in flight across all repos. The check must live under the same
// mutex as the registration: concurrent per-repo pool passes each compute
// their slot budgets from a snapshot, so two passes could both see one slot of
// host headroom and both dispatch past the cap if the check were external.
// hostCap <= 0 means unbounded (plain begin).
func (t *inflightJobTracker) beginWithin(hostCap int, jobID, repo, checkoutKey, runtimeKey string) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return false
	}
	if _, exists := t.jobs[jobID]; exists {
		return false
	}
	if hostCap > 0 {
		total := 0
		for _, count := range t.perRepo {
			total += count
		}
		if total >= hostCap {
			return false
		}
	}
	t.jobs[jobID] = inflightDispatch{repo: repo, checkoutKey: checkoutKey, runtimeKey: runtimeKey}
	if checkoutKey != "" {
		t.checkouts[checkoutKey]++
	}
	if runtimeKey != "" {
		t.runtimes[runtimeKey]++
	}
	t.perRepo[repo]++
	t.wg.Add(1)
	return true
}

// end releases everything begin registered for jobID. Idempotent.
func (t *inflightJobTracker) end(jobID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	dispatch, exists := t.jobs[jobID]
	if !exists {
		return
	}
	delete(t.jobs, jobID)
	decrementCount(t.checkouts, dispatch.checkoutKey)
	decrementCount(t.runtimes, dispatch.runtimeKey)
	decrementCount(t.perRepo, dispatch.repo)
	t.wg.Done()
}

// unionStringSets returns a fresh set containing every true key of a and b;
// neither input is mutated.
func unionStringSets(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for key, held := range a {
		if held {
			out[key] = true
		}
	}
	for key, held := range b {
		if held {
			out[key] = true
		}
	}
	return out
}

// sortedStringSetKeys returns the set's keys sorted (nil for an empty set), for
// deterministic SQL exclusion lists and log lines.
func sortedStringSetKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func decrementCount(m map[string]int, key string) {
	if key == "" {
		return
	}
	if m[key] <= 1 {
		delete(m, key)
		return
	}
	m[key]--
}

func (t *inflightJobTracker) inflightJob(jobID string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, exists := t.jobs[jobID]
	return exists
}

// inflightIDs snapshots the in-flight job IDs (jobs THIS process is running),
// used to keep the tick's recovery scans away from live work.
func (t *inflightJobTracker) inflightIDs() map[string]bool {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.jobs) == 0 {
		return nil
	}
	ids := make(map[string]bool, len(t.jobs))
	for id := range t.jobs {
		ids[id] = true
	}
	return ids
}

func (t *inflightJobTracker) inflight(repo string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.perRepo[repo]
}

// total reports the in-flight dispatched jobs across ALL repos — the host-global
// counterpart of inflight(repo). The registered-repo supervisor shares one
// tracker across every enabled repo, and clamping dispatch by this keeps
// --workers a HOST-wide bound: without it, per-repo async dispatch would
// multiply the cap by the number of enabled repos (main's inline sweep ran one
// repo's batch at a time, so at most `workers` jobs ever ran host-wide).
func (t *inflightJobTracker) total() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, count := range t.perRepo {
		n += count
	}
	return n
}

// checkoutHeld reports whether an in-flight dispatched job currently holds the
// given checkout key. Nil-receiver safe (a nil tracker never holds anything),
// so a method value taken from a nil tracker is a valid always-false predicate.
func (t *inflightJobTracker) checkoutHeld(key string) bool {
	if t == nil || key == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.checkouts[key] > 0
}

// busy reports whether the repo has any in-flight dispatched job or a live
// background pool pass — the gate for checkout-mutating maintenance and for the
// poller's full PollOnce.
func (t *inflightJobTracker) busy(repo string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.perRepo[repo] > 0 || t.poolRuns[repo]
}

// poolRunning reports whether a background pool pass is live for repo.
func (t *inflightJobTracker) poolRunning(repo string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.poolRuns[repo]
}

// holderOf returns the job ID currently holding checkoutKey, if any.
func (t *inflightJobTracker) holderOf(checkoutKey string) string {
	if t == nil || checkoutKey == "" {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, dispatch := range t.jobs {
		if dispatch.checkoutKey == checkoutKey {
			return id
		}
	}
	return ""
}

// seeds snapshots the in-flight checkout/runtime key sets in the shape
// selectRunnableQueuedJobsSeeded consumes.
func (t *inflightJobTracker) seeds() (map[string]bool, map[string]bool) {
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	checkouts := make(map[string]bool, len(t.checkouts))
	for key := range t.checkouts {
		checkouts[key] = true
	}
	runtimes := make(map[string]bool, len(t.runtimes))
	for key := range t.runtimes {
		runtimes[key] = true
	}
	return checkouts, runtimes
}

// tryBeginPool marks a background pool pass in flight for repo. Single-flight:
// it refuses while a pass is already running, and also while tracked non-pool
// jobs are still in flight (a scheduler flip mid-run must not start a pool pass
// whose fresh accounting could double-dispatch beside them).
func (t *inflightJobTracker) tryBeginPool(repo string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining || t.poolRuns[repo] || t.perRepo[repo] > 0 {
		return false
	}
	t.poolRuns[repo] = true
	t.wg.Add(1)
	return true
}

func (t *inflightJobTracker) endPool(repo string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.poolRuns[repo] {
		return
	}
	delete(t.poolRuns, repo)
	t.wg.Done()
}

// recordErr stores an async infra error for the repo so the NEXT tick can
// surface it to the recovering supervisor (#570 escalation still sees it).
func (t *inflightJobTracker) recordErr(repo string, err error) {
	if t == nil || err == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.errs[repo] = append(t.errs[repo], err)
}

// takeErr drains one recorded async error for the repo (oldest first).
func (t *inflightJobTracker) takeErr(repo string) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	pending := t.errs[repo]
	if len(pending) == 0 {
		return nil
	}
	err := pending[0]
	if len(pending) == 1 {
		delete(t.errs, repo)
	} else {
		t.errs[repo] = pending[1:]
	}
	return err
}

// drain cancels every dispatched job's context and waits (bounded) for the
// in-flight jobs to finish, logging what it had to abandon. Called on
// supervisor exit so daemon stop cancels + drains in-flight work. From the
// moment drain starts, begin/tryBeginPool refuse new work, so a worker tick
// racing the shutdown cannot spawn a job the drain would never see.
func (t *inflightJobTracker) drain(stdout io.Writer, timeout time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.draining = true
	t.mu.Unlock()
	t.cancelRun()
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.mu.Lock()
		remaining := len(t.jobs)
		t.mu.Unlock()
		writeLine(stdout, "daemon shutdown: abandoned %d in-flight job(s) still running after %s drain", remaining, timeout)
	}
}

// heldBackWarnState throttles the per-job held-back observability lines. Keyed
// by job ID; an unchanged reason stays quiet for heldBackLogInterval.
type heldBackWarnState struct {
	reason string
	at     time.Time
}

var (
	heldBackWarnMu    sync.Mutex
	heldBackWarnByJob = map[string]heldBackWarnState{}
)

// warnJobHeldBack emits ONE throttled line explaining why a queued job was not
// dispatched this tick (#562 point 5: these exclusions used to be silent). The
// wording reuses the #552 why-stuck vocabulary where it applies.
func warnJobHeldBack(stdout io.Writer, jobID string, reason string) {
	if strings.TrimSpace(reason) == "" {
		return
	}
	now := time.Now()
	heldBackWarnMu.Lock()
	prev, seen := heldBackWarnByJob[jobID]
	if seen && prev.reason == reason && now.Sub(prev.at) < heldBackLogInterval {
		heldBackWarnMu.Unlock()
		return
	}
	// Bound the throttle map for a long-lived daemon: entries for jobs that
	// stopped being held back (ran or were cancelled) are dead weight, so once
	// the map grows past a small cap, sweep everything past its re-log window.
	if len(heldBackWarnByJob) > 1024 {
		for id, state := range heldBackWarnByJob {
			if now.Sub(state.at) >= heldBackLogInterval {
				delete(heldBackWarnByJob, id)
			}
		}
	}
	heldBackWarnByJob[jobID] = heldBackWarnState{reason: reason, at: now}
	heldBackWarnMu.Unlock()
	writeLine(stdout, "job %s held back: %s", jobID, reason)
}

// admissionSkipReason describes an admission-budget refusal (#365) for the
// held-back log, distinguishing a transient "does not fit right now" from a
// configuration never-fit (the job alone exceeds the configured cap, so waiting
// can never admit it).
func admissionSkipReason(budget *admissionBudget, est admissionEstimate) string {
	if budget == nil {
		return ""
	}
	reservedCount, reservedMemGB, maxSessions, maxMemoryGB := budget.snapshot()
	if maxMemoryGB > 0 && est.memGB > maxMemoryGB {
		return fmt.Sprintf("admission budget can NEVER fit it (estimated %.1f GB > max_memory_gb %.1f); raise [admission].max_memory_gb or lower the runtime's estimate", est.memGB, maxMemoryGB)
	}
	return fmt.Sprintf("waiting on admission budget (%d/%d sessions, %.1f/%.1f GB reserved)", reservedCount, maxSessions, reservedMemGB, maxMemoryGB)
}

// explainHeldBackJobs logs (throttled) why each still-queued job was excluded
// from this dispatch pass: a checkout key held by an in-flight job, or a held /
// stranded runtime-session resource lock. Best-effort observability only.
func explainHeldBackJobs(ctx context.Context, worker jobWorker, tracker *inflightJobTracker, remaining []db.Job) {
	for _, job := range remaining {
		checkoutKey := queuedJobCheckoutKey(ctx, worker.Store, job)
		if holder := tracker.holderOf(checkoutKey); holder != "" && holder != job.ID {
			warnJobHeldBack(worker.Stdout, job.ID, fmt.Sprintf("waiting on checkout %s (held by in-flight job %s)", checkoutKey, holder))
			continue
		}
		runtimeKey := queuedJobRuntimeResourceKey(ctx, worker.Store, job)
		if runtimeKey == "" {
			continue
		}
		if lock, err := worker.Store.GetResourceLock(ctx, runtimeKey); err == nil {
			reason := fmt.Sprintf("waiting on runtime session lock %s", runtimeKey)
			if owner := strings.TrimSpace(lock.OwnerJobID); owner != "" && owner != job.ID {
				reason += fmt.Sprintf(" (held by job %s)", owner)
			}
			if expires := strings.TrimSpace(lock.ExpiresAt); expires != "" {
				reason += fmt.Sprintf(", lease expires %s", expires)
			}
			warnJobHeldBack(worker.Stdout, job.ID, reason)
		}
	}
}

// dispatchQueuedJobsTracked is the daemon's non-blocking dispatch pass (#562).
// It claims runnable queued jobs and hands them to tracked goroutines, then
// returns promptly (surfacing any async error a previously dispatched job
// recorded) so the supervisor worker loop keeps ticking while jobs run.
//
// limit is the repo's resolved concurrency (global --workers or a per-repo
// max_parallel override, #576); globalLimit is the daemon-wide --workers count.
// Dispatch is clamped by BOTH the per-repo limit and the host-global remainder
// max(globalLimit, limit) − tracker.total(): the registered-repo supervisor
// runs this pass for every enabled repo with a shared tracker, and without the
// global clamp each repo would independently fill `limit` slots — up to
// workers × repos in-flight runtime subprocesses where main's inline sweep
// never exceeded `workers` host-wide. Taking the max keeps an EXPLICIT
// per-repo override able to raise its own repo's share (main's inline batch
// honored it); with no override the host cap is exactly `workers`.
//
// Pool mode starts runQueuedJobsForRepoPool as a SINGLE-FLIGHT background pass
// per repo: the pool already re-queries continuously and does its own in-flight
// accounting (which it mirrors into the tracker for poller/maintenance gating),
// so one live pass is both necessary and sufficient.
func dispatchQueuedJobsTracked(ctx context.Context, worker jobWorker, limit int, globalLimit int, repoFilter string, rootFilter string, tracker *inflightJobTracker) error {
	if obs := dispatchLimitObserver; obs != nil {
		obs(limit)
	}
	if limit <= 0 {
		return tracker.takeErr(repoFilter)
	}
	hostCap := globalLimit
	if limit > hostCap {
		hostCap = limit
	}
	if serializingConfig(worker.UsePool, limit) {
		warnSerializedParallelJobs(ctx, worker, limit, repoFilter, rootFilter)
	}
	if worker.UsePool {
		if tracker.tryBeginPool(repoFilter) {
			runCtx := tracker.jobContext(ctx)
			go func() {
				defer tracker.endPool(repoFilter)
				err := runQueuedJobsForRepoPoolTracked(runCtx, worker, limit, hostCap, repoFilter, rootFilter, tracker)
				if err != nil && !errors.Is(err, context.Canceled) {
					tracker.recordErr(repoFilter, err)
				}
			}()
		}
		return tracker.takeErr(repoFilter)
	}

	// Single-selector invariant: a still-live background pool pass (a warm
	// pool->barrier scheduler flip mid-run, #577) keeps OWNING dispatch for this
	// repo until it drains. Two concurrent selectors could each snapshot seeds
	// before the other's begin() and dispatch two jobs onto one checkout key,
	// breaking same-repo serialization. Symmetric to tryBeginPool refusing while
	// tracked non-pool jobs are in flight; queued jobs simply wait a tick.
	if tracker.poolRunning(repoFilter) {
		return tracker.takeErr(repoFilter)
	}

	pending, err := listPendingQueuedJobs(ctx, worker, repoFilter, rootFilter, true)
	if err != nil {
		return err
	}
	// Drop jobs this process is already running: a freshly dispatched job stays
	// 'queued' in the DB until engine.RunJob claims it, and that window must not
	// double-dispatch it.
	eligible := pending[:0]
	for _, job := range pending {
		if !tracker.inflightJob(job.ID) {
			eligible = append(eligible, job)
		}
	}
	slots := limit - tracker.inflight(repoFilter)
	// Host-global clamp: never dispatch past the daemon-wide in-flight budget,
	// whatever this repo's own remainder is (see the function comment).
	if hostSlots := hostCap - tracker.total(); hostSlots < slots {
		slots = hostSlots
	}
	if slots <= 0 || len(eligible) == 0 {
		return tracker.takeErr(repoFilter)
	}
	policy, err := worker.parallelSessionPolicy()
	if err != nil {
		policy = config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue}
	}
	seedCheckouts, seedRuntimes := tracker.seeds()
	queued, remaining := selectRunnableQueuedJobsSeeded(ctx, worker.Store, eligible, slots, policy, seedCheckouts, seedRuntimes)
	explainHeldBackJobs(ctx, worker, tracker, remaining)
	runCtx := tracker.jobContext(ctx)
	for _, job := range queued {
		job := job
		if !worker.Admission.Reserve(job.ID, func() admissionEstimate { return worker.admissionEstimate(ctx, job) }) {
			warnJobHeldBack(worker.Stdout, job.ID, admissionSkipReason(worker.Admission, worker.admissionEstimate(ctx, job)))
			continue
		}
		checkoutKey := queuedJobCheckoutKey(ctx, worker.Store, job)
		runtimeKey := queuedJobRuntimeResourceKey(ctx, worker.Store, job)
		// beginWithin re-checks the host-global cap atomically with registration:
		// a concurrent pool pass for another repo may have consumed the headroom
		// this pass's slot computation saw.
		if !tracker.beginWithin(hostCap, job.ID, repoFilter, checkoutKey, runtimeKey) {
			worker.Admission.Release(job.ID)
			continue
		}
		go func() {
			defer tracker.end(job.ID)
			defer worker.Admission.Release(job.ID)
			err := runPoolJobRecovered(runCtx, worker, job)
			if err != nil && !errors.Is(err, errRuntimeSessionBusy) && !errors.Is(err, context.Canceled) {
				tracker.recordErr(repoFilter, err)
			}
		}()
	}
	return tracker.takeErr(repoFilter)
}
