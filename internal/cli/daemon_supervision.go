package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func runRegisteredRepoSupervisor(ctx context.Context, home string, live *daemonReloadableConfig, dryRun bool, watchSkillOptReviews bool, watchIssues bool, rootFilter string, stdout io.Writer) error {
	return withStoreAndPaths(home, func(paths config.Paths, store *db.Store) error {
		schedule := registeredRepoSchedule{
			NextPoll:    map[string]time.Time{},
			ErrorStreak: map[string]int{},
			IdleStreak:  map[string]int{},
		}
		_, initialWorkers, _ := live.snapshot()
		// rawHome (the function's own param) feeds the read-only policy loaders;
		// paths.Home (the resolved <home>/.gitmoot root) feeds the engine wiring (#459).
		poller := defaultRegisteredRepoPoller(store, initialWorkers, dryRun, stdout, home, paths.Home)
		poller.WatchIssues = watchIssues
		blobStore := artifact.NewStore(paths.ArtifactBlobs)
		reviewGitHub := newSkillOptGitHubClient()
		worker := defaultJobWorker(store, stdout, home)
		worker.CommenterFactory = worker.defaultCommenter
		worker.Admission = worker.loadAdmissionBudget()
		checkoutLocks := &repoCheckoutLocks{}
		poller.CheckoutLocks = checkoutLocks
		var workerErr <-chan error
		if !dryRun {
			if err := recoverExpiredRuntimeSessionLocks(ctx, store, stdout, time.Now().UTC()); err != nil {
				return err
			}
			if err := recoverForeignBootRunners(ctx, store, stdout); err != nil {
				return err
			}
			if err := recoverCancelledRunningJobsForEnabledRepos(ctx, store, rootFilter, stdout); err != nil {
				return err
			}
			// The in-flight tracker (#562) keeps one hung/long job on any repo
			// from wedging the whole sweep: ticks claim + dispatch async and
			// return promptly. The poller consults it so a repo with in-flight
			// jobs degrades to the recovery-only poll instead of mutating the
			// checkout under a running job. The deferred drain cancels + waits
			// (bounded) for in-flight jobs on exit.
			tracker := newInflightJobTracker(ctx)
			defer tracker.drain(stdout, daemonShutdownDrainTimeout)
			poller.Inflight = tracker
			workerErr = startSupervisorWorkerLoopRecovering(ctx, daemonWorkerLoopInterval, stdout, func(now time.Time) error {
				// Read the warm-reloadable worker count + scheduler mode each tick
				// (#577). The worker is copied per tick so setting UsePool is race-free
				// with the SIGHUP reload goroutine, and the pool is re-dispatched each
				// tick so a resize applies live without disturbing in-flight jobs.
				_, workers, usePool := live.snapshot()
				w := worker
				w.UsePool = usePool
				return runEnabledRepoWorkerTicksTracked(ctx, store, w, workers, rootFilter, stdout, now, checkoutLocks, tracker)
			})
			// #884 home-scoped post-terminal insight harvest. This owns one
			// sequential classifier lane for the daemon process and is deliberately
			// outside daemonWorkflowEngine, which is rebuilt per repo/tick.
			startMemoryHarvestLoop(ctx, paths, home, store, stdout)
			startCockpitReconcileLoop(ctx, store, paths.Home, stdout)
			startBlockedRoleWakeLoop(ctx, store, paths.Home, stdout)
			startTranscriptRetentionLoop(ctx, paths, store, stdout)
		}
		// Heartbeat schedules (#533) reuse the normal job queue. Off-by-default: with
		// no heartbeat sections the scan returns before any store touch. Skip it under
		// --dry-run (no worker loop runs there, so an enqueued job would just sit).
		heartbeatEnqueue := newHeartbeatEnqueuer(store, home)
		// Pipeline schedules (#681) run the same way: the scan advances in-flight runs
		// and fires due interval schedules, reusing the normal job queue. Off-by-default
		// (no pipelines => an empty list before any state touch) and skipped under
		// --dry-run for the same reason.
		pipelineEnqueue := newPipelineStageEnqueuer(store, home)
		if !dryRun {
			pipeline.InstallDefaultMemoryPipelinesForDaemon(ctx, store, paths, home, stdout)
		}
		for {
			if err := receiveSupervisorWorkerError(workerErr); err != nil {
				return err
			}
			baseInterval := live.pollInterval()
			pollerForTick := poller
			pollerForTick.IdleGraceTicks, pollerForTick.IdleMaxMultiplier = live.idleCadence()
			wait, err := pollRegisteredReposWithPoller(ctx, pollerForTick, schedule, time.Now().UTC(), baseInterval)
			if err != nil {
				return err
			}
			// Per-repo idle decay gates GitHub calls only. Heartbeats, pipelines,
			// chat scans, and other supervisor maintenance still wake at base cadence.
			if wait > baseInterval {
				wait = baseInterval
			}
			if !dryRun {
				if err := runWorkflowAutoSettleOnce(ctx, paths, store, time.Now().UTC(), stdout); err != nil {
					writeLine(stdout, "workflow auto-settle error: %s", err)
				}
				if err := runHeartbeatScanOnce(ctx, paths, store, heartbeatEnqueue, time.Now().UTC()); err != nil {
					writeLine(stdout, "heartbeat scan error: %s", err)
				}
				if err := pipeline.RunPipelineScanOnce(ctx, store, pipelineEnqueue, time.Now().UTC()); err != nil {
					writeLine(stdout, "pipeline scan error: %s", err)
				}
				// Chat auto-respond sweep (#534 V1.5). Off-by-default: with
				// [chat].auto_respond unset (or no agent enrolled) it returns before any
				// chat-table query, so the tick hot path is byte-identical.
				if err := runChatAutoRespondScanOnce(ctx, paths, home, store, dispatchLocalAgentJob, time.Now().UTC()); err != nil {
					writeLine(stdout, "chat auto-respond scan error: %s", err)
				}
				// Decouple the pipeline-advance cadence from the repo-poll backoff
				// (#697): `wait` is the poller's cadence, which grows to minutes when
				// repo polling backs off, and it would otherwise throttle the pipeline
				// advancer to that same rate. While any run is in flight, cap the sleep
				// to the configured (non-backed-off) poll interval so settled stages
				// fold promptly. NOTE (#911): the idle-decay clamp above already caps
				// `wait` at the base interval on every pass, so this guard is now a
				// second, narrower ceiling; it still only reduces the sleep, never
				// extends it.
				if inFlight, err := pipeline.PipelineRunsInFlight(ctx, store); err != nil {
					writeLine(stdout, "pipeline in-flight check error: %s", err)
				} else {
					wait = pipeline.PipelineAdvanceWait(wait, live.pollInterval(), inFlight)
				}
			}
			if watchSkillOptReviews {
				if _, err := pollSkillOptReviewWatches(ctx, paths, store, blobStore, reviewGitHub, stdout, dryRun, home); err != nil {
					writeLine(stdout, "skillopt review watch poll error: %s", err)
				}
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case err := <-workerErr:
				timer.Stop()
				if err != nil {
					return err
				}
			case <-timer.C:
			}
		}
	})
}

func runSingleRepoSupervisor(ctx context.Context, home string, d daemon.Daemon, store *db.Store, live *daemonReloadableConfig, rootFilter string, stdout io.Writer) error {
	if err := recoverExpiredRuntimeSessionLocks(ctx, store, stdout, time.Now().UTC()); err != nil {
		return err
	}
	if err := recoverForeignBootRunners(ctx, store, stdout); err != nil {
		return err
	}
	if err := recoverRunningJobsForRepo(ctx, store, stdout, d.Repo.FullName(), rootFilter); err != nil {
		return err
	}
	if err := recoverCancelledRunningJobsForRepo(ctx, store, stdout, d.Repo.FullName(), rootFilter); err != nil {
		return err
	}
	worker := defaultJobWorker(store, stdout, home)
	worker.CommenterFactory = worker.defaultCommenter
	worker.Admission = worker.loadAdmissionBudget()
	var checkoutLock sync.Mutex
	// The in-flight tracker (#562) is what keeps ONE hung/long job from wedging
	// this whole loop: ticks claim + dispatch async and return, while the tracker
	// preserves same-repo serialization, concurrency caps, and poller exclusion.
	// The deferred drain cancels + waits (bounded) for in-flight jobs on exit.
	tracker := newInflightJobTracker(ctx)
	defer tracker.drain(stdout, daemonShutdownDrainTimeout)
	workerErr := startSingleRepoWorkerLoop(ctx, daemonWorkerLoopInterval, store, worker, live, &checkoutLock, tracker, d.Repo.FullName(), rootFilter, stdout)
	startCockpitReconcileLoop(ctx, store, home, stdout)
	startBlockedRoleWakeLoop(ctx, store, home, stdout)
	// Heartbeat schedules (#533) must also fire in the single-repo daemon, or a
	// single-repo daemon would silently never run them. Off-by-default: with no
	// heartbeat sections the scan returns before any store touch. A failure to
	// resolve paths only disables heartbeats (logged once); it never aborts the loop.
	heartbeatPaths, heartbeatPathsErr := pathsFromFlag(home)
	if heartbeatPathsErr != nil {
		writeLine(stdout, "heartbeat scan disabled: %s", heartbeatPathsErr)
	}
	heartbeatEnqueue := newHeartbeatEnqueuer(store, home)
	if heartbeatPathsErr == nil {
		// The single-repo daemon gets the same one-per-home sweep owner as the
		// registered-repo supervisor; it is not attached to the per-tick engine.
		startMemoryHarvestLoop(ctx, heartbeatPaths, home, store, stdout)
		startTranscriptRetentionLoop(ctx, heartbeatPaths, store, stdout)
	}
	// Pipeline schedules (#681) fire in the single-repo daemon too, or a single-repo
	// daemon would silently never advance/schedule pipelines. Off-by-default: with no
	// pipelines the scan returns an empty list before any state touch. Unlike the
	// heartbeat scan it needs no config paths (it reads the DB), so a paths failure
	// does not disable it.
	pipelineEnqueue := newPipelineStageEnqueuer(store, home)
	if heartbeatPathsErr == nil {
		pipeline.InstallDefaultMemoryPipelinesForDaemon(ctx, store, heartbeatPaths, home, stdout)
	} else {
		writeLine(stdout, "default memory pipeline install disabled: %s", heartbeatPathsErr)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := receiveSupervisorWorkerError(workerErr); err != nil {
			return err
		}
		// A full PollOnce may mutate the shared checkout (merge retries, task
		// reconciles), so it needs BOTH the checkout lock (excludes the tick, and
		// — because dispatch happens under that lock — excludes NEW jobs starting
		// mid-poll) AND an idle tracker (excludes already in-flight jobs, which
		// the tick no longer holds the lock for while they run, #562). Otherwise
		// fall back to the recovery-command-only poll, exactly as before.
		polledFull := false
		if checkoutLock.TryLock() {
			if !tracker.busy(d.Repo.FullName()) {
				_ = runDaemonPollWithTimeout(ctx, daemonPollTimeout, d.PollOnce)
				polledFull = true
			}
			checkoutLock.Unlock()
		}
		if !polledFull {
			_ = runDaemonPollWithTimeout(ctx, daemonPollTimeout, d.PollRecoveryCommandsOnce)
		}
		if heartbeatPathsErr == nil {
			if err := runWorkflowAutoSettleOnce(ctx, heartbeatPaths, store, time.Now().UTC(), stdout); err != nil {
				writeLine(stdout, "workflow auto-settle error: %s", err)
			}
			if err := runHeartbeatScanOnce(ctx, heartbeatPaths, store, heartbeatEnqueue, time.Now().UTC()); err != nil {
				writeLine(stdout, "heartbeat scan error: %s", err)
			}
			// Chat auto-respond sweep (#534 V1.5) needs the same resolved config paths
			// as the heartbeat scan, so gate it on the same paths resolution.
			// Off-by-default: returns before any chat-table query unless enabled.
			if err := runChatAutoRespondScanOnce(ctx, heartbeatPaths, home, store, dispatchLocalAgentJob, time.Now().UTC()); err != nil {
				writeLine(stdout, "chat auto-respond scan error: %s", err)
			}
		}
		if err := pipeline.RunPipelineScanOnce(ctx, store, pipelineEnqueue, time.Now().UTC()); err != nil {
			writeLine(stdout, "pipeline scan error: %s", err)
		}
		// Read the warm-reloadable poll interval each cycle (#577) so a SIGHUP
		// change takes effect on the next tick. Fall back to the historical 30s
		// default if it was somehow left non-positive.
		interval := live.pollInterval()
		if interval <= 0 {
			interval = 30 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-workerErr:
			timer.Stop()
			if err != nil {
				return err
			}
		case <-timer.C:
		}
	}
}

// heartbeatEnqueuer enqueues one heartbeat job. In production it wraps
// workflow.Mailbox.Enqueue; tests inject a fake to assert the request shape (and
// to fail loudly if an off-by-default scan ever enqueues).
type heartbeatEnqueuer func(ctx context.Context, request workflow.JobRequest) (db.Job, error)

// newHeartbeatEnqueuer builds the production enqueuer: a Mailbox bound to the
// store and the daemon's canary-routing policy, matching dispatchLocalAgentJob's
// construction so a heartbeat job is indistinguishable from a normal background
// job once enqueued.
func newHeartbeatEnqueuer(store *db.Store, home string) heartbeatEnqueuer {
	mailbox := workflow.Mailbox{Store: store, CanaryEnabled: canaryRoutingEnabled(home), RuntimeDefaultModel: runtimeDefaultModelResolver(home), RequireWorkflowPolicy: requireWorkflowPolicyResolver(home), OrgPolicy: orgPolicyResolver(home)}
	return func(ctx context.Context, request workflow.JobRequest) (db.Job, error) {
		return mailbox.Enqueue(ctx, request)
	}
}

func heartbeatFingerprint(agent, name string) string {
	return fmt.Sprintf("heartbeat:%s/%s", agent, name)
}

// heartbeatRepoManaged reports whether repo is one the daemon worker tick can
// actually run a job for: registered (a repos row exists), enabled, and with a
// non-empty checkout_path. A heartbeat for any other repo must be skipped (with
// next_due advanced) rather than enqueued into a job no worker will ever claim.
func heartbeatRepoManaged(ctx context.Context, store *db.Store, repo string) (bool, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return false, nil
	}
	record, err := store.GetRepo(ctx, repo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return record.Enabled && strings.TrimSpace(record.CheckoutPath) != "", nil
}

// heartbeatAgentHasCapability reports whether the named agent currently holds the
// given capability (e.g. "review"). A missing agent is NOT an error: it returns
// false so a review heartbeat for an unknown/unstarted agent is skipped rather
// than aborting the whole scan. A real store error propagates.
func heartbeatAgentHasCapability(ctx context.Context, store *db.Store, agent, capability string) (bool, error) {
	record, err := store.GetAgent(ctx, strings.TrimSpace(agent))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return agentHasCapability(record.Capabilities, capability), nil
}

// heartbeatImplementPermitted reports whether the named agent may run an implement
// heartbeat: it must hold the "implement" capability AND carry a write-granting
// autonomy policy (workspace-write / danger-full-access). A missing agent is NOT
// an error — it returns false so an implement heartbeat for an unknown/unstarted
// agent is skipped (and next_due advanced) rather than aborting the scan. It
// reuses the exact runtime predicate the direct-implement dispatch gate uses, so
// the two can never drift. A real store error propagates (#611).
func heartbeatImplementPermitted(ctx context.Context, store *db.Store, agent string) (bool, error) {
	record, err := store.GetAgent(ctx, strings.TrimSpace(agent))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !agentHasCapability(record.Capabilities, "implement") {
		return false, nil
	}
	return runtime.PolicyGrantsImplementWrite(record.AutonomyPolicy), nil
}

func heartbeatJobID(agent, name string, now time.Time) string {
	return fmt.Sprintf("heartbeat-%s-%s-%x", agent, name, now.UTC().UnixNano())
}

// heartbeatJitter returns a uniformly random delay in [0, jitter] so concurrent
// heartbeats with the same interval do not thunder. jitter<=0 returns 0, which
// keeps a no-jitter (the default) schedule deterministic.
func heartbeatJitter(jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(jitter) + 1))
}

// runHeartbeatScanOnce checks every configured heartbeat schedule once and
// enqueues a normal background job for each enabled+due entry (#533), reusing the
// standard job queue: the existing worker tick then runs the job (no new
// execution code).
//
// OFF BY DEFAULT: a config with no [agents.<agent>.heartbeats.<name>] sections
// makes LoadHeartbeats return an empty slice, so this returns nil BEFORE touching
// the store or the enqueuer. It is wired into BOTH supervisor loops; the caller
// logs a scan error and never aborts the loop. A per-heartbeat error is collected
// (first wins) but does not stop the remaining heartbeats.
func runHeartbeatScanOnce(ctx context.Context, paths config.Paths, store *db.Store, enqueue heartbeatEnqueuer, now time.Time) error {
	heartbeats, err := config.LoadHeartbeats(paths)
	if err != nil {
		return err
	}
	if len(heartbeats) == 0 {
		return nil
	}
	agentTypes, err := config.LoadAgentTypes(paths)
	if err != nil {
		return err
	}
	now = now.UTC()
	var firstErr error
	for _, heartbeat := range heartbeats {
		if err := runOneHeartbeat(ctx, store, enqueue, agentTypes, heartbeat, paths.Home, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func runOneHeartbeat(ctx context.Context, store *db.Store, enqueue heartbeatEnqueuer, agentTypes map[string]config.AgentType, heartbeat config.Heartbeat, home string, now time.Time) error {
	if !heartbeat.Enabled {
		return nil
	}
	interval, err := time.ParseDuration(heartbeat.Interval)
	if err != nil {
		return fmt.Errorf("heartbeat %s/%s interval: %w", heartbeat.Agent, heartbeat.Name, err)
	}
	jitter, err := time.ParseDuration(heartbeat.Jitter)
	if err != nil {
		return fmt.Errorf("heartbeat %s/%s jitter: %w", heartbeat.Agent, heartbeat.Name, err)
	}
	state, _, err := store.GetHeartbeatState(ctx, heartbeat.Agent, heartbeat.Name)
	if err != nil {
		return err
	}
	// Not yet due: a zero next_due (the first-ever scan) is in the past, so a fresh
	// heartbeat runs immediately; a future next_due is skipped without advancing.
	if !state.NextDueAt.IsZero() && now.Before(state.NextDueAt) {
		return nil
	}
	// Repo guard: the worker tick only claims jobs for a registered+enabled repo
	// with a checkout. A heartbeat targeting an unmanaged/disabled/uncheckout repo
	// would enqueue a job no worker ever claims, which would then permanently wedge
	// the heartbeat (the zombie 'queued' job trips the overlap guard every tick).
	// So skip the enqueue but ADVANCE next_due (record last_status) so no zombie is
	// created and the heartbeat self-recovers once the repo becomes managed.
	// dispatchLocalAgentJob does an equivalent resolve/upsert; this path bypasses it.
	managed, err := heartbeatRepoManaged(ctx, store, heartbeat.Repo)
	if err != nil {
		return err
	}
	if !managed {
		state.Agent = heartbeat.Agent
		state.Name = heartbeat.Name
		state.LastRunAt = now
		state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
		state.LastStatus = "repo_unmanaged"
		return store.UpsertHeartbeatState(ctx, state)
	}
	// Capability guard: a review heartbeat enqueues an Action="review" job, which
	// the worker only runs for an agent that HOLDS the review capability. Validate
	// here (the enqueue path bypasses ensureLocalAgentAccess) so we never enqueue a
	// review job the agent is not permitted to run. Skip but ADVANCE next_due with a
	// clear status so it self-recovers once the capability is granted (rather than
	// hot-looping or wedging). LoadHeartbeats already rejects any action other than
	// ask/review at config-load; this is the agent-aware half of that check.
	if heartbeat.Action == "review" {
		hasReview, err := heartbeatAgentHasCapability(ctx, store, heartbeat.Agent, "review")
		if err != nil {
			return err
		}
		if !hasReview {
			state.Agent = heartbeat.Agent
			state.Name = heartbeat.Name
			state.LastRunAt = now
			state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
			state.LastStatus = "capability_missing"
			return store.UpsertHeartbeatState(ctx, state)
		}
	}
	// Policy gate: an implement heartbeat enqueues a WRITE job. The worker only
	// produces files for an agent that holds the implement capability AND carries a
	// write-granting autonomy policy (workspace-write / danger-full-access); under
	// auto/read-only the job runs and produces nothing (and is separately blocked by
	// readOnlyImplementationBlocked at dispatch). Validate here — the enqueue path
	// bypasses ensureLocalAgentAccess — so an implement heartbeat for a read-only
	// agent NO-OPs rather than churning doomed jobs. Skip but ADVANCE next_due with a
	// clear status so it self-recovers once a write policy is granted. This is the
	// agent-aware half of the config-level action gate (#611).
	if heartbeat.Action == "implement" {
		permitted, err := heartbeatImplementPermitted(ctx, store, heartbeat.Agent)
		if err != nil {
			return err
		}
		if !permitted {
			state.Agent = heartbeat.Agent
			state.Name = heartbeat.Name
			state.LastRunAt = now
			state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
			state.LastStatus = "policy_readonly"
			return store.UpsertHeartbeatState(ctx, state)
		}
	}
	// Overlap protection: a still-active heartbeat job (>= max_concurrent) means the
	// previous run has not finished. Skip WITHOUT advancing so it is retried next
	// tick (this is also the restart-safe dedup: a restart sees the active job).
	fingerprint := heartbeatFingerprint(heartbeat.Agent, heartbeat.Name)
	active, err := store.CountActiveJobsByFingerprint(ctx, fingerprint)
	if err != nil {
		return err
	}
	if active >= heartbeat.MaxConcurrent {
		return nil
	}
	// Respect agent capacity: when the agent is already at its max_background, skip
	// this tick WITHOUT advancing so the heartbeat is retried once capacity frees up
	// rather than being silently dropped for a full interval.
	if agentType, ok := agentTypes[heartbeat.Agent]; ok && agentType.MaxBackground > 0 {
		busy, err := store.AgentActiveJobCount(ctx, heartbeat.Agent)
		if err != nil {
			return err
		}
		if busy >= agentType.MaxBackground {
			return nil
		}
	}
	// Per-heartbeat runtime override (#611): when the heartbeat names a runtime,
	// resolve it through the same seam the per-job --runtime override uses (#531) —
	// it validates the runtime and mints a FRESH session ref so the scheduled job
	// neither resumes nor writes the agent's default-runtime session. An empty
	// heartbeat runtime yields ("", "") and the job runs on the agent default,
	// byte-identical to a pre-#611 heartbeat.
	overrideRuntime, overrideRef, overrideErr := resolveJobRuntimeOverride(heartbeat.Runtime, "")
	if overrideErr != nil {
		// A bad runtime override is a config error; skip but ADVANCE next_due so a
		// broken heartbeat does not hot-loop, and self-recovers once corrected.
		state.Agent = heartbeat.Agent
		state.Name = heartbeat.Name
		state.LastRunAt = now
		state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
		state.LastStatus = "runtime_invalid"
		if err := store.UpsertHeartbeatState(ctx, state); err != nil {
			return err
		}
		return overrideErr
	}
	// Implement heartbeats need the SAME isolated task/branch/worktree the direct
	// `agent implement` path allocates (#611). Without it the enqueued job carries
	// Branch="",TaskID="",WorktreePath="" and the daemon worker fails its checkout
	// pre-flight ("checkout branch is main, not job branch ") on the shared checkout —
	// a false-green that never runs the agent, creates a branch, or opens a PR. Do the
	// allocation here (AFTER the overlap/capacity guards so a skipped tick allocates
	// nothing) so taskWorktreeCheckout resolves the on-branch worktree and
	// validateTargetCheckout passes, exactly like a foreground implement. Read-only
	// actions (ask/review) carry no branch identity and keep their bare-enqueue path.
	var implementFields heartbeatImplementFields
	if heartbeat.Action == "implement" {
		implementFields, err = allocateHeartbeatImplement(ctx, store, home, heartbeat)
		if err != nil {
			// Allocation failure (e.g. a dirty checkout or a taken branch) is handled
			// like an enqueue failure: skip but ADVANCE next_due with a clear status so a
			// broken implement heartbeat does not hot-loop, and self-recovers next tick.
			state.Agent = heartbeat.Agent
			state.Name = heartbeat.Name
			state.LastRunAt = now
			state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
			state.LastStatus = "implement_alloc_failed"
			if upsertErr := store.UpsertHeartbeatState(ctx, state); upsertErr != nil {
				return upsertErr
			}
			return err
		}
	}
	job, enqueueErr := enqueue(ctx, workflow.JobRequest{
		ID:                 heartbeatJobID(heartbeat.Agent, heartbeat.Name, now),
		Agent:              heartbeat.Agent,
		Action:             heartbeat.Action,
		Repo:               heartbeat.Repo,
		Branch:             implementFields.Branch,
		TaskID:             implementFields.TaskID,
		TaskTitle:          implementFields.TaskTitle,
		GoalID:             implementFields.GoalID,
		HeadSHA:            implementFields.HeadSHA,
		Sender:             "heartbeat",
		Instructions:       heartbeat.Prompt,
		Fingerprint:        fingerprint,
		RuntimeOverride:    overrideRuntime,
		RuntimeOverrideRef: overrideRef,
	})
	// Advance exactly one interval whether or not the enqueue succeeded. Anchoring
	// next_due to `now` (not the old due time) coalesces missed ticks into a single
	// run (no backlog replay), and advancing on failure (recording last_status)
	// stops a broken heartbeat from hot-looping every tick.
	state.Agent = heartbeat.Agent
	state.Name = heartbeat.Name
	state.LastRunAt = now
	state.NextDueAt = now.Add(interval + heartbeatJitter(jitter))
	if enqueueErr != nil {
		state.LastStatus = "enqueue_failed"
	} else {
		state.LastJobID = job.ID
		state.LastStatus = "enqueued"
	}
	if err := store.UpsertHeartbeatState(ctx, state); err != nil {
		return err
	}
	return enqueueErr
}

// heartbeatImplementFields is the task/branch/worktree identity an implement
// heartbeat job must carry so the daemon worker's checkout pre-flight
// (taskWorktreeCheckout + validateTargetCheckout) resolves the freshly allocated
// on-branch worktree and passes — the exact set the direct `agent implement` path
// stamps onto its JobRequest (#611).
type heartbeatImplementFields struct {
	Branch    string
	TaskID    string
	TaskTitle string
	GoalID    string
	HeadSHA   string
}

// allocateHeartbeatImplement performs the SAME task/branch/worktree allocation the
// direct `agent implement` dispatch does (prepareLocalImplementDispatchRequest →
// workflow.Engine.AllocateTaskWorktree): it upserts a fresh adhoc task on a
// gitmoot/<taskID> branch and adds an isolated git worktree checked out on that
// branch, returning the identity fields the enqueued job needs. It reuses the
// direct path verbatim so the scheduled and foreground implement flows can never
// drift. It uses the STORED repo record (whose DefaultBranch is the allocation
// base) rather than re-deriving the base from the possibly-off-branch shared
// checkout (#611).
func allocateHeartbeatImplement(ctx context.Context, store *db.Store, home string, heartbeat config.Heartbeat) (heartbeatImplementFields, error) {
	repo, err := daemon.ParseRepository(heartbeat.Repo)
	if err != nil {
		return heartbeatImplementFields{}, err
	}
	record, err := store.GetRepo(ctx, heartbeat.Repo)
	if err != nil {
		return heartbeatImplementFields{}, err
	}
	_, prepared, err := prepareLocalImplementDispatchRequest(ctx, store, record, repo, localAgentDispatchRequest{
		RepoFlag:     heartbeat.Repo,
		Agent:        heartbeat.Agent,
		Action:       "implement",
		Instructions: heartbeat.Prompt,
		Home:         home,
	})
	if err != nil {
		return heartbeatImplementFields{}, err
	}
	return heartbeatImplementFields{
		Branch:    prepared.Branch,
		TaskID:    prepared.TaskID,
		TaskTitle: prepared.TaskTitle,
		GoalID:    prepared.GoalID,
		HeadSHA:   prepared.HeadSHA,
	}, nil
}

// startSingleRepoWorkerLoop wires the single-repo supervisor's per-tick worker
// closure — the checkout-lock discipline, the warm-reloadable worker/scheduler
// snapshot (#577), and the tracked worker tick (#562) — and starts it on the
// recovering loop. It is the ONE production wiring runSingleRepoSupervisor
// uses; the #562 wedge E2E drives this exact function so the tested loop can
// never drift from the deployed one.
func startSingleRepoWorkerLoop(ctx context.Context, interval time.Duration, store *db.Store, worker jobWorker, live *daemonReloadableConfig, checkoutLock *sync.Mutex, tracker *inflightJobTracker, repo string, rootFilter string, stdout io.Writer) <-chan error {
	return startSupervisorWorkerLoopRecovering(ctx, interval, stdout, func(now time.Time) error {
		// The checkout lock now guards the TICK (maintenance + claim/dispatch),
		// not whole job runs: dispatched jobs execute on their own goroutines
		// after this returns, tracked by the in-flight tracker (#562). Holding it
		// across dispatch is still what makes the poller's full-PollOnce gate
		// race-free (no new checkout-occupying job can start mid-poll).
		checkoutLock.Lock()
		defer checkoutLock.Unlock()
		// Read the warm-reloadable worker count + scheduler mode each tick (#577);
		// the worker is copied per tick so UsePool is race-free with the SIGHUP
		// reload goroutine.
		_, workers, usePool := live.snapshot()
		w := worker
		w.UsePool = usePool
		// nil carrier: this single-repo supervisor tick self-computes the shared
		// candidate sets once for its own tick (#619).
		return runDaemonWorkerTickTracked(ctx, store, w, workers, false, repo, rootFilter, stdout, now, tracker, nil)
	})
}

func startSupervisorWorkerLoopRecovering(ctx context.Context, interval time.Duration, stdout io.Writer, run func(time.Time) error) <-chan error {
	return startSupervisorWorkerLoopInternal(ctx, interval, stdout, run, true)
}

// maxConsecutiveWorkerTickFailures bounds how many CONSECUTIVE recovering
// worker-tick failures the supervisor tolerates before it stops retrying and
// surfaces the error on its channel so the caller (the daemon) exits and
// systemd restarts/alerts. gaijinjoe's #555 made a single transient tick error
// survivable (the daemon no longer wedges), but retry-forever turned a
// PERMANENT infra fault — job-level failures already return nil, so what bubbles
// up here is disk-full / corrupt-or-locked SQLite / a failed migration — into a
// silent false-green daemon: status still "running", zero progress, ~86k log
// lines/day. The streak resets to 0 on any successful tick, so only a genuinely
// stuck daemon escalates. At the capped backoff below this spans a few minutes
// before escalating (1+2+4+8+16 then 30s each ≈ 4m for a 1s base interval).
const maxConsecutiveWorkerTickFailures = 12

// maxWorkerTickBackoff caps the exponential retry sleep applied to a persistent
// recovering-loop fault, so a permanent error backs off to the poll cadence
// instead of spinning at the 1s base interval and flooding the journal.
const maxWorkerTickBackoff = 30 * time.Second

func startSupervisorWorkerLoopInternal(ctx context.Context, interval time.Duration, stdout io.Writer, run func(time.Time) error, recoverErrors bool) <-chan error {
	errCh := make(chan error, 1)
	if interval <= 0 {
		interval = daemonWorkerLoopInterval
	}
	go func() {
		defer close(errCh)
		consecutiveFailures := 0
		for {
			if err := ctx.Err(); err != nil {
				return
			}
			if err := run(time.Now().UTC()); err != nil {
				// A cancellation observed mid-tick is a clean shutdown, never a
				// fault: return WITHOUT logging or counting it toward the
				// escalation streak, so graceful shutdown stays quiet and never
				// pushes context.Canceled onto errCh.
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					return
				}
				if !recoverErrors {
					errCh <- err
					return
				}
				consecutiveFailures++
				// A PERSISTENT fault (a bounded streak of consecutive tick
				// errors) is infra-level, not a transient blip: escalate it so
				// the daemon exits instead of spinning silently forever.
				if consecutiveFailures >= maxConsecutiveWorkerTickFailures {
					writeLine(stdout, "daemon worker tick error: %v; %d consecutive failures, escalating", err, consecutiveFailures)
					errCh <- err
					return
				}
				writeLine(stdout, "daemon worker tick error: %v; retrying (%d/%d)", err, consecutiveFailures, maxConsecutiveWorkerTickFailures)
				if sleepErr := sleepSupervisorWorkerLoop(ctx, workerTickBackoff(interval, consecutiveFailures)); sleepErr != nil {
					return
				}
				continue
			}
			// A successful tick clears the streak: only CONSECUTIVE failures
			// escalate, so one bad pass between good ones never trips the ceiling.
			consecutiveFailures = 0
			if err := sleepSupervisorWorkerLoop(ctx, interval); err != nil {
				return
			}
		}
	}()
	return errCh
}

// workerTickBackoff returns the retry sleep for the Nth consecutive recovering
// worker-tick failure: the base interval doubled per failure, capped at
// maxWorkerTickBackoff. consecutiveFailures==1 returns the base interval (his
// original single-transient-error cadence is preserved).
func workerTickBackoff(base time.Duration, consecutiveFailures int) time.Duration {
	if base <= 0 {
		base = daemonWorkerLoopInterval
	}
	backoff := base
	for i := 1; i < consecutiveFailures; i++ {
		if backoff >= maxWorkerTickBackoff {
			return maxWorkerTickBackoff
		}
		backoff *= 2
	}
	if backoff > maxWorkerTickBackoff {
		return maxWorkerTickBackoff
	}
	return backoff
}

func sleepSupervisorWorkerLoop(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func receiveSupervisorWorkerError(errCh <-chan error) error {
	if errCh == nil {
		return nil
	}
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// startCockpitReconcileLoop runs the low-frequency cockpit reconcile GC in the
// background until ctx is cancelled (Task 7). Each tick drops cockpit_pane rows
// whose herdr pane is gone AND whose owning root is terminal, complementing the
// per-Deliver / root-finalize teardown and report-metadata --ttl-ms self-expiry.
// It is entirely best-effort: it is gated on herdr availability (so a host without
// herdr never sweeps), uses the auto-policy cockpit, and swallows every error. It
// never blocks the supervisor's poll/worker loops. A policy load failure or a
// disabled cockpit simply skips the sweep.
func startCockpitReconcileLoop(ctx context.Context, store *db.Store, home string, stdout io.Writer) {
	worker := defaultJobWorker(store, stdout, home)
	go func() {
		ticker := time.NewTicker(cockpitReconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcileCockpitPanesOnce(ctx, worker)
			}
		}
	}()
}

// reconcileCockpitPanesOnce performs one best-effort cockpit reconcile sweep. It
// builds the cockpit from the host orchestrate policy, skips when the cockpit is
// disabled or herdr is unreachable, and otherwise asks cockpit.Reconcile to drop
// orphaned rows (pane gone + root terminal). All errors are swallowed.
func reconcileCockpitPanesOnce(ctx context.Context, worker jobWorker) {
	policy, err := worker.orchestratePolicy()
	if err != nil || policy.CockpitMode == config.CockpitModeOff {
		return
	}
	cp := worker.newCockpit(policy)
	if cp == nil || !cp.Available(ctx) {
		return
	}
	cp.Reconcile(ctx, func(rootJobID string) bool {
		terminal, terr := worker.rootTreeTerminal(ctx, rootJobID)
		return terr == nil && terminal
	})
}

type repoCheckoutLocks struct {
	locks sync.Map
}

func (l *repoCheckoutLocks) For(repo string) *sync.Mutex {
	if l == nil {
		return nil
	}
	value, _ := l.locks.LoadOrStore(repo, &sync.Mutex{})
	return value.(*sync.Mutex)
}

type registeredRepoSchedule struct {
	NextPoll    map[string]time.Time
	ErrorStreak map[string]int
	IdleStreak  map[string]int
}

func (s registeredRepoSchedule) ensure() registeredRepoSchedule {
	if s.NextPoll == nil {
		s.NextPoll = map[string]time.Time{}
	}
	if s.ErrorStreak == nil {
		s.ErrorStreak = map[string]int{}
	}
	if s.IdleStreak == nil {
		s.IdleStreak = map[string]int{}
	}
	return s
}

type registeredRepoPoller struct {
	Store                  *db.Store
	Workers                int
	DryRun                 bool
	Stdout                 io.Writer
	RecoveryOnly           bool
	WatchIssues            bool
	EscalationTTL          time.Duration
	RevertDetectionEnabled bool
	CheckoutLocks          *repoCheckoutLocks
	// Inflight is the supervisor's in-flight job tracker (#562). A repo with
	// dispatched jobs still running gets the recovery-command-only poll: a full
	// PollOnce may mutate the shared checkout, which used to be excluded by the
	// per-repo lock being held for entire job runs. nil (legacy/test callers)
	// behaves as always-idle.
	Inflight          *inflightJobTracker
	IdleGraceTicks    int
	IdleMaxMultiplier int
	GitHubClient      func(checkout string) github.Client
	WorkflowFactory   func(store *db.Store, gh github.Client, checkout string) *workflow.Engine
}

// defaultRegisteredRepoPoller wires the registered-repo supervisor's per-tick
// poller. It takes TWO home values with DISTINCT, documented shapes (#459):
//
//   - rawHome: the RAW --home (NOT <home>/.gitmoot). It feeds the READ-ONLY policy
//     loaders — resolveEscalationTTL and jobWorker.ConfigHome (orchestratePolicy) —
//     each of which resolves it to the config.toml exactly once. Passing a resolved
//     root here would re-append ".gitmoot" inside those loaders and create the
//     phantom <home>/.gitmoot/.gitmoot.
//   - resolvedRoot: the already-resolved <home>/.gitmoot root (config.Paths.Home).
//     It feeds the engine wiring — daemonWorkflowEngine's ArtifactRoot/Home and
//     daemonEventSink — which expect the resolved root and do NOT re-resolve.
//
// The direct pollRegisteredReposWithPoller caller passes "" for both, which is a
// no-op: resolveEscalationTTL("") returns the default and daemonWorkflowEngine("")
// leaves ArtifactRoot/Home/EventSink unset.
func defaultRegisteredRepoPoller(store *db.Store, workers int, dryRun bool, stdout io.Writer, rawHome, resolvedRoot string) registeredRepoPoller {
	return registeredRepoPoller{
		Store:                  store,
		Workers:                workers,
		DryRun:                 dryRun,
		Stdout:                 stdout,
		EscalationTTL:          resolveEscalationTTL(rawHome),
		RevertDetectionEnabled: resolveRevertDetectionEnabled(rawHome),
		IdleGraceTicks:         config.DefaultDaemonIdleGraceTicks,
		IdleMaxMultiplier:      config.DefaultDaemonIdleMaxMultiplier,
		GitHubClient:           func(checkout string) github.Client { return github.NewClient(checkout) },
		WorkflowFactory: func(store *db.Store, gh github.Client, checkout string) *workflow.Engine {
			engine := daemonWorkflowEngine(store, gh, checkout, resolvedRoot)
			// Apply only the escalate_human notifier handle from policy (#340),
			// keeping the budget/inlining knobs out of this path so its existing
			// behavior is unchanged. The notifier itself is already wired by
			// daemonWorkflowEngine; this just sets the configured @-handle.
			// orchestratePolicy reads via jobWorker.ConfigHome, which is ALWAYS the
			// RAW --home (#459) so it never re-resolves into a phantom doubled home.
			if notifier, ok := engine.EscalationNotifier.(*daemonEscalationNotifier); ok && notifier != nil {
				if policy, err := defaultJobWorker(store, stdout, rawHome).orchestratePolicy(); err == nil {
					notifier.Handle = policy.EscalationHandle
				}
			}
			return &engine
		},
	}
}

// resolveEscalationTTL reads the [orchestrate].escalation_ttl policy (#340),
// falling back to DefaultEscalationTTL when unset and to 0 (scan disabled) only
// on a hard parse failure, so the auto-finalize backstop is on by default.
//
// It is READ-ONLY and shape-tolerant (#459): it resolves the config.toml for
// EITHER a raw --home or an already-resolved <home>/.gitmoot root via
// resolveConfigFile, then LoadOrchestratePolicy (which only ReadFile-s, never
// MkdirAll-s). It MUST NOT call initializedPaths/config.Initialize: the real home
// is already initialized upstream by withStore/withStoreAndPaths, and Initializing
// here on a resolved root would re-append ".gitmoot" and create the phantom
// <home>/.gitmoot/.gitmoot. Being side-effect-free makes it phantom-free even if a
// caller hands it the resolved root by mistake — defense in depth.
func resolveEscalationTTL(home string) time.Duration {
	policy := config.DefaultOrchestratePolicy()
	if cfg := resolveConfigFile(home); cfg != "" {
		if loaded, err := config.LoadOrchestratePolicy(config.Paths{ConfigFile: cfg}); err == nil {
			policy = loaded
		}
	}
	raw := strings.TrimSpace(policy.EscalationTTL)
	if raw == "" {
		raw = config.DefaultEscalationTTL
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil || ttl <= 0 {
		return 0
	}
	return ttl
}

// resolveBlockedTTL reads the [orchestrate].blocked_ttl policy (#631) and returns
// the blocked-job sweep window, or 0 when the sweep is DISABLED. Unlike
// resolveEscalationTTL it has NO default fallback: an unset/empty (or zero, or
// unparseable) value resolves to 0 so the sweep stays OFF by default — a blocked
// job is a human-awaiting decision and is never auto-dismissed unless the operator
// opted in with a positive duration. It mirrors resolveEscalationTTL's read-only,
// shape-tolerant config resolution (resolveConfigFile + LoadOrchestratePolicy,
// never config.Initialize) so it is phantom-free for either a raw --home or an
// already-resolved <home>/.gitmoot root.
func resolveBlockedTTL(home string) time.Duration {
	policy := config.DefaultOrchestratePolicy()
	if cfg := resolveConfigFile(home); cfg != "" {
		if loaded, err := config.LoadOrchestratePolicy(config.Paths{ConfigFile: cfg}); err == nil {
			policy = loaded
		}
	}
	raw := strings.TrimSpace(policy.BlockedTTL)
	if raw == "" {
		return 0
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil || ttl <= 0 {
		return 0
	}
	return ttl
}

// resolveBlockedRoleWakeAfter reads the opt-in blocked-since threshold. It is
// read-only and shape-tolerant like resolveBlockedTTL: either a raw --home or an
// already-resolved <home>/.gitmoot root is accepted without initializing or
// re-resolving the home. Zero, missing, or invalid values keep both evaluators
// disabled.
func resolveBlockedRoleWakeAfter(home string) time.Duration {
	policy := config.DefaultOrchestratePolicy()
	if cfg := resolveConfigFile(home); cfg != "" {
		if loaded, err := config.LoadOrchestratePolicy(config.Paths{ConfigFile: cfg}); err == nil {
			policy = loaded
		}
	}
	if policy.BlockedRoleWakeAfter <= 0 {
		return 0
	}
	return policy.BlockedRoleWakeAfter
}

// resolveDelegationWorktreeTTL reads the default-on terminal worktree retention
// policy without initializing or re-resolving the daemon home. Parse errors are
// returned so the caller can surface the skipped housekeeping pass instead of
// silently falling back to destructive defaults.
func resolveDelegationWorktreeTTL(home string) (time.Duration, error) {
	return config.LoadDelegationWorktreeTTL(config.Paths{ConfigFile: resolveConfigFile(home)})
}

func pollRegisteredReposWithPoller(ctx context.Context, poller registeredRepoPoller, schedule registeredRepoSchedule, now time.Time, fallbackPoll time.Duration) (time.Duration, error) {
	schedule = schedule.ensure()
	repos, err := poller.Store.ListRepos(ctx)
	if err != nil {
		return fallbackPoll, err
	}
	enabled := 0
	polled := 0
	decayed := 0
	wait := fallbackPoll
	waitSet := false
	for _, repoRecord := range repos {
		if !repoRecord.Enabled {
			// Drop any idle streak so a disabled repo re-earns its grace ticks
			// when re-enabled instead of resuming a stale decayed cadence.
			delete(schedule.IdleStreak, repoRecord.FullName())
			continue
		}
		enabled++
		fullName := repoRecord.FullName()
		interval := repoPollInterval(repoRecord.PollInterval, fallbackPoll)
		// Local promotion is a ONE-SHOT exit from idle decay: it applies only
		// to a repo that actually decayed (IdleStreak > 0) and is not in error
		// backoff. Without the streak guard a busy repo would re-poll on every
		// supervisor tick instead of at its configured interval, and without
		// the error guard queued local jobs would override repoBackoffInterval
		// and hammer a failing API at base cadence.
		promoted := false
		if schedule.IdleStreak[fullName] > 0 && schedule.ErrorStreak[fullName] == 0 {
			queued, err := poller.Store.CountQueuedJobsForRepo(ctx, fullName)
			if err != nil {
				return wait, err
			}
			if queued > 0 || poller.Inflight.busy(fullName) {
				promoted = true
				delete(schedule.IdleStreak, fullName)
			}
		}
		dueAt := schedule.NextPoll[fullName]
		if !promoted && !dueAt.IsZero() && dueAt.After(now) {
			if repoIdleMultiplier(schedule.IdleStreak[fullName], poller.IdleGraceTicks, poller.IdleMaxMultiplier) > 1 {
				decayed++
			}
			wait = shorterWait(wait, dueAt.Sub(now), &waitSet)
			continue
		}
		polled++
		result, err := poller.pollRepo(ctx, repoRecord, now)
		if err != nil {
			return wait, err
		}
		nextInterval := interval
		if result.LastError != "" {
			schedule.ErrorStreak[fullName]++
			delete(schedule.IdleStreak, fullName)
			nextInterval = repoBackoffInterval(interval, schedule.ErrorStreak[fullName])
		} else {
			delete(schedule.ErrorStreak, fullName)
			queued, err := poller.Store.CountQueuedJobsForRepo(ctx, fullName)
			if err != nil {
				return wait, err
			}
			idle := result.Conditional.Calls > 0 && result.Conditional.Misses == 0 &&
				!poller.Inflight.busy(fullName) && queued == 0
			if idle {
				schedule.IdleStreak[fullName]++
			} else {
				delete(schedule.IdleStreak, fullName)
			}
			multiplier := repoIdleMultiplier(schedule.IdleStreak[fullName], poller.IdleGraceTicks, poller.IdleMaxMultiplier)
			if multiplier > 1 {
				decayed++
				nextInterval = interval * time.Duration(multiplier)
			}
		}
		schedule.NextPoll[fullName] = now.Add(nextInterval)
		wait = shorterWait(wait, nextInterval, &waitSet)
	}
	writeLine(poller.Stdout, "supervised %d enabled repos, polled %d, decayed %d", enabled, polled, decayed)
	if wait <= 0 {
		wait = fallbackPoll
	}
	return wait, nil
}

type registeredRepoPollResult struct {
	LastError   string
	Conditional github.ConditionalRequestStats
}

type conditionalRequestStatsProvider interface {
	ConditionalRequestStats() github.ConditionalRequestStats
}

func (p registeredRepoPoller) pollRepo(ctx context.Context, repoRecord db.Repo, now time.Time) (registeredRepoPollResult, error) {
	store := p.Store
	repo, err := daemon.ParseRepository(repoRecord.FullName())
	if err != nil {
		lastError := err.Error()
		writeLine(p.Stdout, "%s: %s", repoRecord.FullName(), lastError)
		return registeredRepoPollResult{LastError: lastError}, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), now.Format(time.RFC3339), lastError)
	}
	lastPollAt := now.Format(time.RFC3339)
	if strings.TrimSpace(repoRecord.CheckoutPath) == "" {
		message := "registered repo has no checkout path"
		writeLine(p.Stdout, "%s: %s", repoRecord.FullName(), message)
		return registeredRepoPollResult{LastError: message}, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, message)
	}
	writeLine(p.Stdout, "polling %s with %d workers dry_run=%t", repoRecord.FullName(), p.Workers, p.DryRun)
	if p.DryRun {
		return registeredRepoPollResult{}, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, "")
	}
	gh := p.GitHubClient(repoRecord.CheckoutPath)
	engine := p.WorkflowFactory(store, gh, repoRecord.CheckoutPath)
	recoveryOnly := p.RecoveryOnly
	if lock := p.CheckoutLocks.For(repoRecord.FullName()); lock != nil {
		if lock.TryLock() {
			defer lock.Unlock()
		} else {
			recoveryOnly = true
		}
	}
	// In-flight gate (#562): jobs no longer run under the per-repo lock, so a
	// held lock alone can't prove the checkout is quiet. While this repo has
	// dispatched jobs still running, degrade to the recovery-only poll — the
	// same exclusion inline execution used to give. Checked AFTER TryLock: new
	// checkout-occupying jobs dispatch only under the lock we now hold, so an
	// idle verdict cannot be raced by a fresh dispatch mid-poll.
	if p.Inflight.busy(repoRecord.FullName()) {
		recoveryOnly = true
	}
	d := daemon.Daemon{
		Repo:                   repo,
		Store:                  store,
		GitHub:                 gh,
		Workflow:               engine,
		WatchIssues:            p.WatchIssues,
		EscalationTTL:          p.EscalationTTL,
		RevertDetectionEnabled: p.RevertDetectionEnabled,
	}
	// Bound the poll the same way the single-repo supervisor does (#555 / #536):
	// this call runs while HOLDING the per-repo checkout lock (deferred Unlock
	// above), so a wedged PollOnce would hold that lock forever and freeze this
	// repo's worker ticks — the exact stall #555 targets. The timeout only
	// bounds the poll; the lock's per-repo checkout semantics are unchanged
	// because Unlock still runs via defer once the (now-bounded) poll returns.
	if recoveryOnly {
		err = runDaemonPollWithTimeout(ctx, daemonPollTimeout, d.PollRecoveryCommandsOnce)
	} else {
		err = runDaemonPollWithTimeout(ctx, daemonPollTimeout, d.PollOnce)
	}
	lastError := ""
	if err != nil {
		lastError = err.Error()
		writeLine(p.Stdout, "%s: %s", repoRecord.FullName(), lastError)
	}
	result := registeredRepoPollResult{LastError: lastError}
	if provider, ok := gh.(conditionalRequestStatsProvider); ok {
		result.Conditional = provider.ConditionalRequestStats()
	}
	return result, store.UpdateRepoPollResult(ctx, repoRecord.FullName(), lastPollAt, lastError)
}

func repoIdleMultiplier(streak, graceTicks, maxMultiplier int) int {
	if maxMultiplier <= 0 {
		maxMultiplier = config.DefaultDaemonIdleMaxMultiplier
	}
	if maxMultiplier <= 1 {
		return 1
	}
	if graceTicks <= 0 {
		graceTicks = config.DefaultDaemonIdleGraceTicks
	}
	if streak < graceTicks {
		return 1
	}
	if streak == graceTicks {
		if maxMultiplier < 2 {
			return maxMultiplier
		}
		return 2
	}
	return maxMultiplier
}

func repoBackoffInterval(base time.Duration, streak int) time.Duration {
	if streak <= 0 {
		return base
	}
	maxBackoff := base * 8
	if maxBackoff < 5*time.Minute {
		maxBackoff = 5 * time.Minute
	}
	backoff := base
	for i := 0; i < streak; i++ {
		if backoff >= maxBackoff/2 {
			return maxBackoff
		}
		backoff *= 2
	}
	if backoff > maxBackoff {
		return maxBackoff
	}
	return backoff
}

func repoPollInterval(value string, fallback time.Duration) time.Duration {
	interval, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || interval <= 0 {
		return fallback
	}
	return interval
}

func shorterWait(current time.Duration, candidate time.Duration, set *bool) time.Duration {
	if candidate <= 0 {
		return current
	}
	if !*set || candidate < current {
		*set = true
		return candidate
	}
	return current
}
