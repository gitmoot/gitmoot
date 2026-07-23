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
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/sandbox"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type jobWorker struct {
	Store  *db.Store
	Stdout io.Writer
	// ConfigHome is ALWAYS the RAW --home (never the resolved <home>/.gitmoot
	// root) — INVARIANT (#459). The read-only policy loaders below
	// (orchestratePolicy/parallelSessionPolicy/admissionPolicy via configPaths())
	// resolve it through pathsFromFlag -> PathsForHome, which appends ".gitmoot"
	// exactly once. Passing the already-resolved root here would append it a SECOND
	// time and read a phantom <home>/.gitmoot/.gitmoot/config.toml. workflowHome()
	// likewise resolves ConfigHome once to the engine's resolved root. The loaders
	// are side-effect-free (no config.Initialize), so even a mistaken resolved-root
	// ConfigHome can never MkdirAll the phantom — but every construction site must
	// still pass the raw --home so the config is actually found.
	ConfigHome         string
	ConfigHomeExplicit bool
	AdapterFactory     func(runtime.Agent, string) (workflow.DeliveryAdapter, error)
	// OutputAdapterFactory rebuilds a production runtime adapter around the one
	// shared live-output writer used by pipeline progress and cockpit. Tests that
	// inject an opaque fake AdapterFactory may leave this nil and still exercise
	// elapsed-only progress without replacing their fake.
	OutputAdapterFactory func(runtime.Agent, string, io.Writer) (workflow.DeliveryAdapter, error)
	StartAdapterFactory  func(string, string) (runtime.Adapter, error)
	CheckoutValidator    func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error)
	WorkflowFactory      func(string) workflow.Engine
	CommenterFactory     func(string) github.Client
	// UsePool selects the opt-in continuous worker-pool scheduler (#394,
	// --scheduler=pool) over the default per-tick wg.Wait() barrier.
	UsePool bool
	// Admission is the opt-in, host-global memory-aware concurrency budget (#365)
	// the scheduler consults before dispatching each session job. nil means the
	// feature is OFF (no [admission] config) ⇒ scheduling is byte-identical to a
	// build without admission accounting. The supervisors attach it at startup;
	// it is a shared pointer across all per-repo dispatch passes so the cap is
	// process-global (host-global for the normal single-daemon deployment).
	Admission *admissionBudget
	// EventSinkOverride lets a test inject a recording events.Sink (#446) without
	// a config file / webhook. When nil (production), eventSink() resolves the
	// shared process-global webhook sink from [events] config instead.
	EventSinkOverride events.Sink
	// RelayServer is the daemon's #732 chat relay. When non-nil AND a job payload is
	// a `gitmoot moot` seat (payload.MootSeat), run() mints a per-seat token on it and
	// injects GITMOOT_CHAT_RELAY[_AUTH] into the seat's runtime subprocess so the
	// sandboxed seat's `gitmoot chat send/wait` routes through the (unsandboxed)
	// daemon. nil (foreground CLI, and every non-daemon construction) means no relay
	// injection — the job's adapter is byte-identical to pre-#732.
	RelayServer *chatRelayServer
	// AuthProbe is the injected doctor-style live credential probe (#532 slice B).
	// It gates re-dispatch of a runtime_auth deferral: once the coarse hold elapses
	// the scheduler only releases the job when the probe reports the credential is
	// VALID again (an Invalid verdict extends the hold WITHOUT burning a retry
	// attempt; an Unknown/transient verdict falls back to the coarse cadence). When
	// nil (foreground CLI, and every construction that does not opt in) the gate is
	// byte-identical to slice A: the coarse cadence alone governs re-dispatch. Tests
	// inject a fake verdict; the daemon wires defaultAuthProbe (a bounded
	// runtime.ClaudeLiveCheck for claude agents, Unknown for other runtimes).
	AuthProbe func(context.Context, db.Job, workflow.JobPayload) authProbeVerdict
	// SandboxProbe is the cached host capability check used only for Claude/Kimi
	// produce stages. nil selects sandbox.SandboxProbe; tests inject deterministic
	// supported/unsupported results without depending on the test binary's argv.
	SandboxProbe func() sandbox.ProbeResult
	// Progress timing seams keep unit/E2E tests deterministic and short. Zero/nil
	// values select the package defaults and real timer implementation.
	PipelineProgressThreshold time.Duration
	PipelineProgressInterval  time.Duration
	ProgressTickSource        func(context.Context, time.Duration, time.Duration) <-chan time.Time
}

// eventSink resolves the best-effort outbound event Sink (#446) for the
// worker's home, or nil when [events] is OFF (the default). It is the seam
// finishQueuedJob / handleRunJobError use to emit the DAEMON-owned terminal
// cases (pre-flight queued->failed/blocked and permission-blocked
// running->blocked) that never pass through the engine's Mailbox chokepoint. The
// underlying webhook sink is a process-global singleton, so this is a cheap
// cache hit on the hot path. A test override short-circuits config resolution.
func (w jobWorker) eventSink() events.Sink {
	if w.EventSinkOverride != nil {
		return w.EventSinkOverride
	}
	return daemonEventSink(w.Store, w.workflowHome())
}

type tempWorkerEligibility struct {
	Eligible bool
	Reason   string
}

func defaultJobWorker(store *db.Store, stdout io.Writer, home ...string) jobWorker {
	configHome := ""
	configHomeExplicit := false
	if len(home) > 0 {
		configHome = home[0]
		configHomeExplicit = true
	}
	worker := jobWorker{Store: store, Stdout: stdout, ConfigHome: configHome, ConfigHomeExplicit: configHomeExplicit}
	worker.RelayServer = activeChatRelayServer()
	worker.AdapterFactory = worker.defaultAdapter
	worker.OutputAdapterFactory = worker.outputAdapter
	worker.StartAdapterFactory = worker.defaultStartAdapter
	worker.CheckoutValidator = worker.defaultCheckout
	worker.WorkflowFactory = worker.defaultWorkflow
	worker.AuthProbe = worker.defaultAuthProbe
	return worker
}

func (w jobWorker) run(ctx context.Context, job db.Job) error {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err)
	}
	// An ephemeral child carries an inline worker spec instead of a
	// pre-registered agent. Materialize a throwaway agent + runtime session
	// from the spec before the normal flow runs (which assumes the agent
	// already exists via GetAgent below), and register a cleanup defer so the
	// worker is auto-disposed on every exit path — success, failure, or block.
	if payload.Ephemeral != nil {
		if err := w.startEphemeralWorker(ctx, job, payload); err != nil {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "ephemeral_worker_failed", Message: err.Error()}); eventErr != nil {
				return eventErr
			}
			if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
			return nil
		}
		// Idempotent removal of the agent row + instance regardless of how run
		// returns; uses a background context so cleanup survives ctx cancel.
		defer w.cleanupTempWorker(context.Background(), job.Agent)
	}
	dbAgent, err := w.Store.GetAgent(ctx, job.Agent)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
		return nil
	}
	agent := runtimeAgent(dbAgent)
	// An ephemeral worker's runtime session exists solely for this job — it was
	// started by startEphemeralWorker above and disposed by the cleanup defer.
	// Mark it single-use so adapters whose CLIs report session-cumulative usage
	// (codex, #658) can attribute that usage to the job: the whole session is
	// this job's cost. In-memory only — GetAgent never returns the flag.
	if payload.Ephemeral != nil {
		agent.SingleUseSession = true
	}
	// Per-job runtime override (#531): the payload carries the override, so a
	// background/daemon job honors it identically to a foreground dispatch. The
	// effective agent swaps in the override runtime + the job's own session ref;
	// the stored agent row (and its default-runtime session) is never touched.
	defaultRuntime := agent.Runtime
	overridden := strings.TrimSpace(payload.RuntimeOverride) != ""
	if overridden {
		agent = applyJobRuntimeOverride(agent, payload)
		if err := runtime.ValidateAgent(agent); err != nil {
			if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
			return nil
		}
	}
	if !overridden {
		agent = scopeRegisteredFreshRefForJob(agent, job.ID)
	}
	if err := w.produceDispatchError(job.Type, agent); err != nil {
		w.recordProduceSandboxDiagnostic(ctx, job.ID, job.Type, agent)
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobBlocked, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
		return nil
	}
	if readOnlyImplementationBlocked(job.Type, agent) {
		transitioned, err := markJobPermissionBlocked(ctx, w.Store, job.ID)
		if err != nil {
			return err
		}
		if !transitioned {
			return nil
		}
		if err := blockTaskForPermissionBlockedJob(ctx, w.Store, job); err != nil {
			return err
		}
		// Best-effort outbound emit (#446): this PRE-FLIGHT queued->blocked
		// permission transition is daemon-owned (it never reaches the engine's
		// Mailbox chokepoint), exactly like the MID-RUN permission block in
		// handleRunJobError which already emits job.blocked. Emit here too so both
		// halves of the permission-blocked terminal case are covered; gated on the
		// genuine transition above, nil-safe when [events] is OFF. The following
		// finalizePreflightDelegationChild only attaches a synthetic result
		// (savePayload, no transition), so it never re-emits.
		emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, job.ID, events.EventJobBlocked, string(workflow.JobBlocked), agentPermissionBlockedMessage, "permission_guard")
		_ = w.postJobResultComment(ctx, job.ID, agent, "", errors.New(agentPermissionBlockedMessage))
		writeLine(w.Stdout, "job %s blocked: %s", job.ID, agentPermissionBlockedMessage)
		// A read-only implement DELEGATION child short-circuits to blocked here,
		// BEFORE finishQueuedJob, via markJobPermissionBlocked (a direct transition)
		// — and blockTaskForPermissionBlockedJob only blocks the task, it never
		// advances the parent DAG. So without this the parent strands exactly like
		// #409. Route the delegation child through the SAME finalize helper so its
		// failure_policy fires. Gated strictly on a delegation child (ParentJobID set,
		// Result nil), so a NON-delegation permission-blocked job is byte-identical.
		if err := w.finalizePreflightDelegationChild(ctx, job.ID, errors.New(agentPermissionBlockedMessage)); err != nil {
			return err
		}
		return nil
	}
	checkout, err := w.CheckoutValidator(ctx, job, payload, agent)
	if err != nil {
		if resumedCheckout, resumedPayload, ok := w.resumeSelfDirtyWorktree(ctx, job, payload, agent, err); ok {
			checkout, payload, err = resumedCheckout, resumedPayload, nil
		} else {
			// Checkout-contention deferral (#532 slice C): a NON-delegation job whose
			// daemon pre-flight checkout failed on a classified contention string (a
			// branch-lock conflict that self-heals, or a dirty/wrong-head checkout that
			// needs a human) is HELD with a backoff instead of terminally failing —
			// pre-terminal, so no job.failed precedes the additive job.deferred. Every
			// other checkout error (and every delegation child) falls through to the
			// existing terminal path byte-identically.
			if deferred, deferErr := w.deferCheckoutContention(ctx, job, payload, err); deferErr != nil {
				writeLine(w.Stdout, "job %s checkout-contention deferral failed: %v", job.ID, deferErr)
			} else if deferred {
				writeLine(w.Stdout, "job %s deferred on checkout contention: %v", job.ID, err)
				return nil
			}
			if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
			return nil
		}
	}
	// #732 moot-seat relay injection: a `gitmoot moot` SEAT (payload.MootSeat) must
	// converse via `gitmoot chat send/wait` mid-run, but its runtime sandbox makes
	// the home read-only. buildSeatAwareAdapter mints a per-seat token bound to
	// (agent, thread), builds the adapter with an env-injecting runner so the seat's
	// runtime subprocess inherits GITMOOT_CHAT_RELAY[_AUTH] and routes those writes
	// to this daemon, AND — only when it actually injects that relay env — elevates
	// the agent to ChatSeat (a codex seat then gets workspace-write+network to reach
	// the socket; the home stays read-only, so the relay does the write). Coupling
	// the elevation to real injection is deliberate: a seat is NEVER left with the
	// extra codex privilege but no working relay (the pre-#732-review bug). Gating on
	// MootSeat — not ThreadID — keeps chat-task promotions and ThreadID-carrying
	// continuations/children byte-identical (unelevated, no relay env). The token is
	// released on every exit path so it cannot be replayed after the seat ends.
	var progressTracker *pipelineProgressLineTracker
	if payload.Sender == workflow.PipelineJobSender {
		progressTracker = &pipelineProgressLineTracker{}
	}
	var adapter workflow.DeliveryAdapter
	var relayToken string
	if progressTracker != nil {
		adapter, relayToken, err = w.buildSeatAwareAdapter(&agent, checkout, payload, progressTracker)
	} else {
		adapter, relayToken, err = w.buildSeatAwareAdapter(&agent, checkout, payload)
	}
	if relayToken != "" {
		defer w.RelayServer.ReleaseSeat(relayToken)
	}
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	managed, err := w.managedJobConfig(ctx, agent.Name)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	jobTimeout := effectiveJobTimeout(payload, managed)
	// Size the runtime-session lease to jobTimeout PLUS a teardown grace so the
	// lease strictly OUTLIVES the run-context deadline (armed at exactly jobTimeout
	// below) and the terminal worktree teardown that runs while this lock is still
	// held. A normally-terminating worker therefore releases its lease before it
	// expires; without the grace the lease would expire in the live-worker teardown
	// window and the expired-lock reaper would requeue the still-'running' owner
	// onto its dirty worktree — the #536 clobber. See runtimeLeaseTeardownGrace.
	lockTTL := defaultDaemonRunningJobStaleAfter
	if jobTimeout > 0 {
		lockTTL = jobTimeout + runtimeLeaseTeardownGrace
	}
	// SESSION SAFETY (#531): the lock is taken on the EFFECTIVE agent, so an
	// overridden job locks the OVERRIDE runtime's session key and can never
	// collide with (or occupy) the agent's default-runtime session lock.
	var (
		releaseLock func(context.Context) error
		acquired    bool
		lockKey     string
		ownerToken  string
	)
	if key, ok := isolatedShellStageRuntimeSessionKey(payload, job.ID); ok {
		// #1034: an isolated shell stage acquires the SAME job-scoped shell key the
		// selector (queuedJobRuntimeResourceKey) gates on, so identical-command
		// isolated forks neither serialize at the gate nor collide at acquisition.
		// acquireRuntimeSessionLockWithKey is the shared low-level acquirer that
		// acquireJobRuntimeSessionLock itself delegates to.
		releaseLock, acquired, lockKey, ownerToken, err = acquireRuntimeSessionLockWithKey(ctx, w.Store, job.ID, key, true, time.Now().UTC(), lockTTL)
	} else {
		releaseLock, acquired, lockKey, ownerToken, err = acquireJobRuntimeSessionLock(ctx, w.Store, job.ID, agent, overridden, time.Now().UTC(), lockTTL)
	}
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	if !acquired {
		message := fmt.Sprintf("runtime session %s is busy", lockKey)
		policy, policyErr := w.parallelSessionPolicy()
		if policyErr != nil {
			if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, policyErr); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, checkout, policyErr)
			return nil
		}
		eligibility := tempWorkerEligible(ctx, w.Store, job, payload, agent, policy, time.Now().UTC())
		if eligibility.Eligible {
			eligibleMessage := fmt.Sprintf("%s; temp worker eligible", message)
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "temp_worker_eligible", Message: eligibleMessage}); eventErr != nil {
				return eventErr
			}
			return w.runWithTempWorker(ctx, job, payload, agent, checkout, policy, eligibleMessage)
		} else if strings.TrimSpace(eligibility.Reason) != "" {
			message = fmt.Sprintf("%s; temp worker ineligible: %s", message, eligibility.Reason)
		}
		// Dedup the runtime_lock_wait row + flood log to once per wait episode
		// (#598): a permanently-contended job otherwise wrote one row per dispatch
		// pass. The busy error is returned UNCONDITIONALLY (outside the episode
		// gate) so the pool dispatcher still sees the bounce and holds the job back.
		if !runtimeLockWaitEpisodeOpen(job.ID) {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_lock_wait", Message: message}); eventErr != nil {
				return eventErr
			}
			markRuntimeLockWaitEpisode(job.ID)
			writeLine(w.Stdout, "job %s waiting: %s", job.ID, message)
		}
		return fmt.Errorf("%w: %s", errRuntimeSessionBusy, message)
	}
	// Acquired the runtime lock: close any open wait episode so a future contention
	// is recorded as a fresh episode.
	endRuntimeLockWaitEpisode(job.ID)
	defer func() {
		if err := releaseLock(context.Background()); err != nil {
			writeLine(w.Stdout, "job %s runtime lock release failed: %v", job.ID, err)
		}
	}()
	stopRuntimeLockHeartbeat := startRuntimeSessionLockHeartbeat(ctx, w.Store, lockKey, ownerToken, lockTTL)
	defer stopRuntimeLockHeartbeat()
	// Thread the owner token into the context so the terminal worktree cleanup
	// (which runs inside RunJob -> AdvanceJob while THIS lock is still held — it is
	// released only by the defer above, after RunJob returns) recognizes the run's
	// OWN lock and does not refuse the healthy-path cleanup as if a foreign live
	// owner held it (#536 / #478). Covers RunJob and the handleRunJobError finalize
	// path below, both of which derive from this ctx.
	ctx = workflow.WithRuntimeSelfOwnerToken(ctx, ownerToken)
	// Expose the effective runtime (and the session lock it runs under) in job
	// history so an overridden background job is observable (#531).
	if overridden {
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_override", Message: jobRuntimeOverrideEventMessage(defaultRuntime, agent, lockKey)}); eventErr != nil {
			writeLine(w.Stdout, "job %s runtime_override event failed: %v", job.ID, eventErr)
		}
	}
	// This is the last filesystem authorization check before adapter delivery.
	// It runs after runtime-session admission so a symlink retargeted while the job
	// waited cannot inherit stale grants. The adapter is then rebuilt in-place with
	// sandbox-exec as the innermost runner for Claude/Kimi produce only.
	if err := applyProduceRuntimeGrants(ctx, w.Store, w.ConfigHome, job, payload, &agent); err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	// Shared tool-cache grant (#1113 lever 1): disk hygiene, not a security
	// precondition, so any failure is fail-open (logged, job proceeds without the
	// cache redirect) rather than failing the job like the produce grants above.
	// MUST run after applyProduceRuntimeGrants: that call overwrites
	// agent.WritablePaths for produce jobs, so appending earlier would be lost.
	var toolCacheEnv []string
	if cachePaths, cacheErr := w.configPaths(); cacheErr != nil {
		writeLine(w.Stdout, "job %s tool cache config load failed: %v", job.ID, cacheErr)
	} else if env, grantErr := applyIsolatedToolCacheGrants(cachePaths, payload, &agent); grantErr != nil {
		writeLine(w.Stdout, "job %s tool cache grant failed: %v", job.ID, grantErr)
	} else {
		toolCacheEnv = env
	}
	adapter, err = wrapProduceSandboxAdapter(job.Type, agent, adapter)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	if len(toolCacheEnv) > 0 {
		if envAdapter, envErr := injectDeliveryAdapterEnv(adapter, toolCacheEnv); envErr != nil {
			writeLine(w.Stdout, "job %s tool cache env inject failed: %v", job.ID, envErr)
		} else {
			adapter = envAdapter
		}
	}
	// Opt-in retained capture is attached to the already-composed adapter so
	// relay env, credential curation, gateway leases, Landlock, and pipeline
	// progress all survive. Any open/composition failure is fail-open.
	retainedLogPath, retainedLogFile, retainedLogErr := openRetainedTranscriptLog(w.ConfigHome, job.ID)
	if retainedLogErr != nil {
		writeLine(w.Stdout, "job %s transcript log open failed: %v", job.ID, retainedLogErr)
	}
	if retainedLogFile != nil {
		teeAdapter, teeErr := appendDeliveryAdapterOutput(adapter, retainedLogFile)
		if teeErr != nil {
			_ = retainedLogFile.Close()
			retainedLogFile = nil
			writeLine(w.Stdout, "job %s transcript tee build failed: %v", job.ID, teeErr)
		} else {
			adapter = teeAdapter
			defer func() {
				if err := retainedLogFile.Close(); err != nil {
					writeLine(w.Stdout, "job %s transcript log close failed: %v", job.ID, err)
				}
			}()
		}
	}
	// Cockpit wrapping happens AFTER the runtime-session lock + checkout
	// resolution so at most one live pane exists per held runtime session and the
	// pane's CWD is the resolved worktree. It is strictly opt-in and best-effort:
	// when --cockpit is off (or herdr is unavailable) the adapter is unchanged and
	// behavior is byte-identical to today. A policy load failure degrades to no
	// cockpit rather than failing the job.
	if payload.Cockpit {
		policy, policyErr := w.orchestratePolicy()
		// A policy LOAD error is not the same as the user opting out (mode off): the
		// user asked for a cockpit, so degrade to cockpit-unavailable (run unwrapped
		// AND emit the single cockpit_unavailable event) rather than silently
		// dropping the pane. Only an explicit mode-off opts out without an event.
		userOptedOff := policyErr == nil && policy.CockpitMode == config.CockpitModeOff
		var cp *cockpit.Cockpit
		if policyErr == nil && !userOptedOff {
			cp = w.newCockpit(policy)
		}
		meta := cockpitJobMeta(job, payload, agent, checkout, policy.CockpitPaneKey)
		seatMode := policy.CockpitPaneKey == config.CockpitPaneKeySeat
		// Only when the cockpit will actually wrap (herdr available) do we tee the
		// child's live output into a log the pane tails (Task 6). The tee rebuilds
		// the inner adapter with a group-kill-preserving TeeRunner and sets
		// meta.LogPath; on any log-setup failure it falls back to no LogPath (the P0
		// `job watch` pane). The non-cockpit / unavailable paths never create a log
		// file or tee — they stay byte-identical.
		//
		// Job mode uses a per-job truncate log removed when the job finishes. Seat
		// mode (Task 7) uses a STABLE per-seat append log so the one seat pane tails
		// one file that accumulates the seat's history across delegation rounds — it
		// is opened O_APPEND and is NOT removed per job (it persists for the root's
		// life and is torn down by FinalizeRoot).
		if maybeWrapCockpitAvailable(cp, payload.Cockpit, userOptedOff) {
			if retainedLogFile != nil && !seatMode {
				// Job-mode cockpit tails the canonical retained file. Presence alone
				// never creates a pane; LogPath is set only inside this cockpit gate.
				meta.LogPath = retainedLogPath
			} else if retainedLogFile != nil && seatMode {
				// Seat logs remain transient. Add the seat writer to the existing
				// retained/progress runner chain without rebuilding the adapter.
				seatPath, seatFile := w.cockpitSeatLogFile(cp, job.ID, meta.RootJobID, meta.PaneKey)
				if seatFile != nil {
					seatAdapter, seatErr := appendDeliveryAdapterOutput(adapter, seatFile)
					if seatErr != nil {
						_ = seatFile.Close()
						writeLine(w.Stdout, "job %s cockpit seat tee build failed: %v", job.ID, seatErr)
					} else {
						adapter = seatAdapter
						meta.LogPath = seatPath
						defer func() { _ = seatFile.Close() }()
					}
				}
			} else {
				var teeAdapter workflow.DeliveryAdapter
				var logPath string
				var logFile *os.File
				if progressTracker != nil {
					teeAdapter, logPath, logFile = w.cockpitLogAdapter(cp, agent, checkout, job.ID, meta.RootJobID, meta.PaneKey, seatMode, progressTracker)
				} else {
					teeAdapter, logPath, logFile = w.cockpitLogAdapter(cp, agent, checkout, job.ID, meta.RootJobID, meta.PaneKey, seatMode)
				}
				if logFile != nil {
					defer func() {
						if err := logFile.Close(); err != nil {
							writeLine(w.Stdout, "job %s cockpit log close failed: %v", job.ID, err)
						}
						// Job mode: the per-job log only backs a per-job pane torn down with
						// the job, so remove it. Seat mode: keep the append log — it backs the
						// persisted seat pane and is removed on root finalize.
						if !seatMode {
							_ = os.Remove(logPath)
						}
					}()
					adapter = teeAdapter
					meta.LogPath = logPath
				}
			}
		}
		var unavailable bool
		adapter, unavailable = maybeWrapCockpit(cp, payload.Cockpit, userOptedOff, adapter, meta)
		if unavailable {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "cockpit_unavailable", Message: "cockpit requested but herdr is unavailable; running without a pane"}); eventErr != nil {
				writeLine(w.Stdout, "job %s cockpit_unavailable event failed: %v", job.ID, eventErr)
			}
		}
		// On the job's return, check whether the root coordination tree has now
		// terminated and, if so, tear its panes / workspace / seat logs down once and
		// surface the reconvene view (Task 7/8). This runs in BOTH modes: seat mode
		// closes the persisted seat panes + workspace here, and job mode (whose panes
		// already close per-Deliver) still needs the per-root WORKSPACE closed at
		// root-terminal — the cockpit_workspaces registry is the only remaining handle
		// once the pane rows are gone. finalizeCockpitRootIfDone's cheap guard
		// short-circuits when there is neither a pane row nor a registered workspace,
		// so a non-cockpit tree makes no extra herdr calls.
		if cp != nil && !userOptedOff {
			defer w.finalizeCockpitRootIfDone(cp, job, payload, meta.RootJobID)
		}
	}
	if managed.OK {
		if err := w.Store.MarkAgentInstanceRunning(ctx, agent.Name, time.Now().UTC(), managed.JobTimeout); err != nil {
			if finishErr := w.finishQueuedJob(ctx, job.ID, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
			return nil
		}
		defer func() {
			if err := w.Store.TouchAgentInstance(context.Background(), agent.Name, time.Now().UTC(), managed.IdleTimeout); err != nil {
				writeLine(w.Stdout, "job %s managed agent state update failed: %v", job.ID, err)
			}
		}()
	}
	writeLine(w.Stdout, "running job %s for %s in %s", job.ID, agent.Name, payload.Repo)
	adapter = pipeline.WrapPipelineEnvDeliveryAdapter(w.Store, w.ConfigHome, payload, adapter)
	engine := w.WorkflowFactory(checkout)
	// Wire the PRE-TERMINAL operational-blocker deferrer (#532 slice E) on the LIVE
	// worker (not the WorkflowFactory-captured copy) so it observes this worker's
	// EventSink for the first-class job.deferred emit. When a delivery-seam failure
	// classifies as a retryable operational blocker the mailbox re-queues the job
	// BEFORE the terminal transition, so no job.failed reaches the [events] sink.
	engine.BlockerDeferrer = w.deferOperationalBlockerPreTerminal
	runCtx, stopRun := w.runningJobContext(ctx, job.ID)
	defer stopRun()
	if jobTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, jobTimeout)
		defer cancel()
	}
	stopProgress := func() {}
	if progressTracker != nil {
		progressCtx, cancelProgress := context.WithCancel(runCtx)
		done := make(chan struct{})
		threshold := w.PipelineProgressThreshold
		if threshold <= 0 {
			threshold = pipelineProgressThreshold
		}
		interval := w.PipelineProgressInterval
		if interval <= 0 {
			interval = pipelineProgressInterval
		}
		tickSource := w.ProgressTickSource
		if tickSource == nil {
			tickSource = pipelineProgressTicks
		}
		startedAt := time.Now().UTC()
		go func() {
			defer close(done)
			emitPipelineProgress(progressCtx, w.Store, w.Stdout, job.ID, startedAt, progressTracker, tickSource(progressCtx, threshold, interval))
		}()
		stopProgress = func() {
			cancelProgress()
			<-done
		}
	}
	_, err = engine.RunJob(runCtx, job.ID, agent, adapter)
	stopProgress()
	if err != nil {
		// Operational-blocker deferral (#532 slice E): a run whose delivery failed on
		// a classified OPERATIONAL blocker (runtime auth rejected, rate limit/quota,
		// network/GitHub outage) is re-queued PRE-terminally by the mailbox's injected
		// BlockerDeferrer — running→queued with a hold + a first-class job.deferred,
		// and NO job.failed. RunJob reports ErrJobDeferred; short-circuit the entire
		// terminal path (no handleRunJobError, no failure comment) since the run
		// already resolved to a deferral. Every other failure takes the path below
		// byte-identically.
		if errors.Is(err, workflow.ErrJobDeferred) {
			writeLine(w.Stdout, "job %s deferred on operational blocker (pre-terminal): %v", job.ID, err)
			return nil
		}
		if markErr := w.handleRunJobError(ctx, job.ID, err); markErr != nil {
			return markErr
		}
		if reconcileErr := engine.ReconcileTerminalDrivingJob(ctx, job.ID); reconcileErr != nil {
			return reconcileErr
		}
		commentErr := err
		if job.Type == "implement" && runtimePermissionFailure(err) {
			latest, latestErr := w.Store.GetJob(ctx, job.ID)
			if latestErr == nil && latest.State == string(workflow.JobBlocked) {
				commentErr = errors.New(agentPermissionBlockedMessage)
			}
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, commentErr)
		writeLine(w.Stdout, "job %s failed: %v", job.ID, err)
		return nil
	}
	if err := engine.ReconcileTerminalDrivingJob(ctx, job.ID); err != nil {
		return err
	}
	_ = w.postJobResultComment(ctx, job.ID, agent, checkout, nil)
	writeLine(w.Stdout, "job %s completed", job.ID)
	return nil
}

// applyProduceRuntimeGrants performs the final delivery-time path check and only
// then copies produce-only grants onto the in-memory runtime agent. Non-produce
// jobs remain byte-identical and can never inherit persisted produce fields.
func applyProduceRuntimeGrants(ctx context.Context, store *db.Store, home string, job db.Job, payload workflow.JobPayload, agent *runtime.Agent) error {
	if strings.TrimSpace(job.Type) != "produce" {
		return nil
	}
	if agent == nil {
		return errors.New("produce runtime agent is required")
	}
	subject := fmt.Sprintf("job %q", job.ID)
	writable, err := pipeline.CanonicalizePipelineProducePaths(ctx, store, home, subject, payload.WritablePaths)
	if err != nil {
		return fmt.Errorf("produce writable path preflight failed: %w", err)
	}
	envFile := ""
	if len(payload.ReadablePaths) > 0 {
		if strings.TrimSpace(payload.PipelineName) == "" {
			return errors.New("produce readable path preflight failed: pipeline name is required")
		}
		record, found, err := store.GetPipeline(ctx, payload.PipelineName)
		if err != nil {
			return fmt.Errorf("produce readable path preflight failed: load pipeline: %w", err)
		}
		if !found {
			return fmt.Errorf("produce readable path preflight failed: pipeline %q is unavailable", payload.PipelineName)
		}
		spec, err := pipeline.Load([]byte(record.SpecYAML))
		if err != nil {
			return fmt.Errorf("produce readable path preflight failed: load pipeline spec: %w", err)
		}
		envFile = spec.EnvFile
	}
	readable, err := pipeline.CanonicalizePipelineProduceReadPaths(ctx, store, home, subject, payload.ReadablePaths, writable, envFile)
	if err != nil {
		return fmt.Errorf("produce readable path preflight failed: %w", err)
	}
	var readableFiles []string
	if len(payload.ReadablePaths) > 0 && agent.Runtime == runtime.ClaudeRuntime {
		var warnings []runtime.ClaudeHookWarning
		readable, readableFiles, warnings, err = claudeProduceRuntimeReadAccess(ctx, store, home, envFile, readable)
		recordClaudeProduceHookWarnings(ctx, store, job.ID, warnings)
		if err != nil {
			return fmt.Errorf("produce Claude runtime resource preflight failed: %w", err)
		}
	}
	agent.WritablePaths = writable
	agent.ReadablePaths = readable
	agent.ReadableFiles = readableFiles
	agent.ProduceNetwork = payload.Network
	return nil
}

func claudeProduceRuntimeReadAccess(ctx context.Context, store *db.Store, homeFlag, envFile string, declared []string) ([]string, []string, []runtime.ClaudeHookWarning, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve Claude operator home: %w", err)
	}
	home = filepath.Clean(home)
	configDir := realClaudeConfigDir()
	if strings.TrimSpace(configDir) == "" {
		configDir = filepath.Join(home, ".claude")
	}
	configDir, err = pipeline.ResolveProduceSafetyPath(configDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve Claude config directory: %w", err)
	}
	protected, err := pipeline.ResolveProduceReadProtectedPaths(ctx, store, homeFlag, envFile)
	if err != nil {
		return nil, nil, nil, err
	}

	resources, warnings := runtime.DiscoverClaudeHookResources(home, configDir)
	readable := compactCleanPaths(declared)
	readableFiles := []string{}
	addDir := func(path, resource string) error {
		resolved, resolveErr := pipeline.ResolveProduceSafetyPath(path)
		if resolveErr != nil {
			return fmt.Errorf("resolve Claude runtime resource %q: %w", resource, resolveErr)
		}
		if label, excluded := protected.Exclusion(resolved); excluded {
			return fmt.Errorf("Claude runtime resource %q cannot be read because its parent %q overlaps %s; move it outside protected state, then add reads: [%q] if needed", resource, resolved, label, resolved)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("inspect Claude runtime resource directory %q: %w", resolved, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("Claude runtime resource parent %q is not a directory", resolved)
		}
		readable = compactCleanPaths(append(readable, resolved))
		return nil
	}
	if info, statErr := os.Stat(configDir); statErr == nil && info.IsDir() {
		if err := addDir(configDir, configDir); err != nil {
			return nil, nil, warnings, err
		}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, nil, warnings, fmt.Errorf("inspect Claude config directory %q: %w", configDir, statErr)
	}

	userState := filepath.Join(home, ".claude.json")
	if _, statErr := os.Stat(userState); statErr == nil {
		resolved, err := pipeline.ResolveProduceSafetyPath(userState)
		if err != nil {
			return nil, nil, warnings, fmt.Errorf("resolve Claude user settings %q: %w", userState, err)
		}
		if label, excluded := protected.Exclusion(resolved); excluded {
			return nil, nil, warnings, fmt.Errorf("Claude user settings %q cannot be read because it overlaps %s", userState, label)
		}
		readableFiles = compactCleanPaths(append(readableFiles, resolved))
	} else if !os.IsNotExist(statErr) {
		return nil, nil, warnings, fmt.Errorf("inspect Claude user settings %q: %w", userState, statErr)
	}

	for _, resource := range resources {
		resolved, err := pipeline.ResolveProduceSafetyPath(resource.Path)
		if err != nil {
			return nil, nil, warnings, fmt.Errorf("resolve Claude hook path %q: %w", resource.Path, err)
		}
		parent := filepath.Dir(resolved)
		if err := addDir(parent, resource.Path); err != nil {
			return nil, nil, warnings, err
		}
		if info, statErr := os.Stat(resolved); statErr == nil {
			if info.IsDir() {
				return nil, nil, warnings, fmt.Errorf("Claude hook path %q is a directory, not a readable script", resource.Path)
			}
			file, openErr := os.Open(resolved)
			if openErr != nil {
				return nil, nil, warnings, fmt.Errorf("Claude hook path %q is not readable: %w", resource.Path, openErr)
			}
			_ = file.Close()
		} else if os.IsNotExist(statErr) {
			return nil, nil, warnings, fmt.Errorf("Claude hook path %q does not exist; fix the hook or add a readable absolute script", resource.Path)
		} else {
			return nil, nil, warnings, fmt.Errorf("inspect Claude hook path %q: %w", resource.Path, statErr)
		}
		if !pathCoveredByRuntimeReads(resolved, readable, readableFiles) {
			return nil, nil, warnings, fmt.Errorf("Claude hook path %q is outside the final read allowlist; add reads: [%q]", resource.Path, parent)
		}
	}
	return readable, readableFiles, warnings, nil
}

func pathCoveredByRuntimeReads(path string, dirs, files []string) bool {
	for _, dir := range dirs {
		if pipeline.PathWithin(path, dir) {
			return true
		}
	}
	for _, file := range files {
		if path == file {
			return true
		}
	}
	return false
}

func recordClaudeProduceHookWarnings(ctx context.Context, store *db.Store, jobID string, warnings []runtime.ClaudeHookWarning) {
	if store == nil || strings.TrimSpace(jobID) == "" {
		return
	}
	for _, warning := range warnings {
		origin := warning.SettingsPath
		if warning.Event != "" {
			origin += " (" + warning.Event + ")"
		}
		_ = store.AddJobEvent(ctx, db.JobEvent{
			JobID:   jobID,
			Kind:    "produce_runtime_resource_warning",
			Message: fmt.Sprintf("Claude hook settings %s: %s", origin, warning.Reason),
		})
	}
}

func (w jobWorker) produceDispatchError(action string, agent runtime.Agent) error {
	if err := runtime.ProduceDispatchError(action, agent); err != nil {
		return err
	}
	if strings.TrimSpace(action) != "produce" || agent.Runtime == runtime.CodexRuntime {
		return nil
	}
	if agent.Runtime != runtime.ClaudeRuntime && agent.Runtime != runtime.KimiRuntime {
		return nil
	}
	result, _ := w.produceSandboxProbe(action, agent)
	if result.Supported {
		return nil
	}
	return fmt.Errorf("produce stages require the codex runtime; agent %q uses runtime %q", agent.Name, agent.Runtime)
}

func (w jobWorker) produceSandboxProbe(action string, agent runtime.Agent) (sandbox.ProbeResult, bool) {
	if strings.TrimSpace(action) != "produce" || (agent.Runtime != runtime.ClaudeRuntime && agent.Runtime != runtime.KimiRuntime) {
		return sandbox.ProbeResult{}, false
	}
	probe := w.SandboxProbe
	if probe == nil {
		probe = sandbox.SandboxProbe
	}
	return probe(), true
}

func (w jobWorker) recordProduceSandboxDiagnostic(ctx context.Context, jobID, action string, agent runtime.Agent) {
	// Only annotate the probe-gated refusal. Capability/policy/runtime validation
	// errors from the legacy preflight keep their existing event surface.
	if err := runtime.ProduceDispatchError(action, agent); err != nil {
		return
	}
	result, applicable := w.produceSandboxProbe(action, agent)
	if !applicable || result.Supported || w.Store == nil {
		return
	}
	detail := "Landlock enforcement self-test failed"
	if result.Err != nil {
		detail = result.Err.Error()
	}
	if result.ABI > 0 {
		detail = fmt.Sprintf("Landlock ABI v%d: %s", result.ABI, detail)
	}
	message := fmt.Sprintf("Gitmoot Landlock sandbox unavailable for %s produce: %s; run gitmoot sandbox probe", agent.Runtime, detail)
	if err := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "produce_sandbox_unsupported", Message: message}); err != nil {
		writeLine(w.Stdout, "job %s produce_sandbox_unsupported event failed: %v", jobID, err)
	}
}

// wrapProduceSandboxAdapter rewrites only Claude/Kimi produce adapters. Codex
// keeps its existing native sandbox and every non-produce adapter is returned
// byte-for-byte unchanged.
func wrapProduceSandboxAdapter(action string, agent runtime.Agent, adapter workflow.DeliveryAdapter) (workflow.DeliveryAdapter, error) {
	if strings.TrimSpace(action) != "produce" || agent.Runtime == runtime.CodexRuntime {
		return adapter, nil
	}
	if agent.Runtime != runtime.ClaudeRuntime && agent.Runtime != runtime.KimiRuntime {
		return adapter, nil
	}
	reads, readFiles, writes, env, err := produceRuntimeSandboxGrants(agent.Runtime, agent.ReadablePaths, agent.ReadableFiles, agent.WritablePaths)
	if err != nil {
		return nil, err
	}
	switch a := adapter.(type) {
	case modelGatewayRuntimeAdapter:
		wrapped, err := wrapProduceSandboxAdapter(action, agent, a.Adapter)
		if err != nil {
			return nil, err
		}
		runtimeAdapter, ok := wrapped.(runtime.Adapter)
		if !ok {
			return nil, fmt.Errorf("produce Landlock sandbox returned incompatible %T adapter", wrapped)
		}
		a.Adapter = runtimeAdapter
		return a, nil
	case runtime.ClaudeAdapter:
		a.Runner = landlockProduceRunner(a.Runner, reads, readFiles, writes, env)
		return a, nil
	case *runtime.ClaudeAdapter:
		a.Runner = landlockProduceRunner(a.Runner, reads, readFiles, writes, env)
		return a, nil
	case runtime.KimiAdapter:
		a.Runner = landlockProduceRunner(a.Runner, reads, readFiles, writes, env)
		return a, nil
	case *runtime.KimiAdapter:
		a.Runner = landlockProduceRunner(a.Runner, reads, readFiles, writes, env)
		return a, nil
	default:
		return nil, fmt.Errorf("produce Landlock sandbox cannot wrap %s adapter %T", agent.Runtime, adapter)
	}
}

func produceRuntimeSandboxGrants(runtimeName string, readable, readFiles, writable []string) ([]string, []string, []string, []string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("resolve runtime state home: %w", err)
	}
	home = filepath.Clean(home)
	var statePaths []string
	var env []string
	switch runtimeName {
	case runtime.ClaudeRuntime:
		stateDir := filepath.Join(home, ".claude")
		cacheRoot, err := os.UserCacheDir()
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("resolve Claude cache root: %w", err)
		}
		cacheDir := filepath.Join(cacheRoot, "claude-cli-nodejs")
		statePaths = []string{stateDir, cacheDir}
		env = []string{"CLAUDE_CONFIG_DIR=" + stateDir}
	case runtime.KimiRuntime:
		statePaths = []string{filepath.Join(home, ".kimi-code")}
	default:
		return compactCleanPaths(readable), compactCleanPaths(readFiles), compactCleanPaths(writable), nil, nil
	}
	for _, path := range statePaths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("create %s runtime state directory %q: %w", runtimeName, path, err)
		}
	}
	reads := compactCleanPaths(readable)
	files := compactCleanPaths(readFiles)
	writes := compactCleanPaths(append(append([]string(nil), writable...), statePaths...))
	return reads, files, writes, env, nil
}

func compactCleanPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

func landlockProduceRunner(runner subprocess.Runner, reads, readFiles, writes, env []string) subprocess.Runner {
	readable := append([]string(nil), reads...)
	files := append([]string(nil), readFiles...)
	writable := append([]string(nil), writes...)
	runtimeEnv := append([]string(nil), env...)
	if tee, ok := runner.(subprocess.TeeRunner); ok {
		inner := tee.Inner
		if inner == nil {
			inner = subprocess.GroupRunner{}
		}
		if _, wrapped := inner.(subprocess.WrappingRunner); !wrapped {
			tee.Inner = subprocess.WrappingRunner{Inner: inner, ReadablePaths: readable, ReadableFiles: files, WritablePaths: writable, Env: runtimeEnv}
		}
		return tee
	}
	if tee, ok := runner.(*subprocess.TeeRunner); ok {
		inner := tee.Inner
		if inner == nil {
			inner = subprocess.GroupRunner{}
		}
		if _, wrapped := inner.(subprocess.WrappingRunner); !wrapped {
			tee.Inner = subprocess.WrappingRunner{Inner: inner, ReadablePaths: readable, ReadableFiles: files, WritablePaths: writable, Env: runtimeEnv}
		}
		return tee
	}
	if runner == nil {
		runner = subprocess.GroupRunner{}
	}
	if _, wrapped := runner.(subprocess.WrappingRunner); wrapped {
		return runner
	}
	return subprocess.WrappingRunner{Inner: runner, ReadablePaths: readable, ReadableFiles: files, WritablePaths: writable, Env: runtimeEnv}
}

// configPaths resolves this worker's config.Paths for READ-ONLY policy loading
// WITHOUT calling config.Initialize (#459). ConfigHome is the raw --home invariant
// (see the struct field doc), so pathsFromFlag resolves it exactly once to the
// real <home>/.gitmoot, which withStore/withStoreAndPaths already initialized
// upstream. Using pathsFromFlag instead of initializedPaths here is the durable
// guard: even if a caller mistakenly passes the already-resolved root, this never
// MkdirAll-s the phantom <home>/.gitmoot/.gitmoot — it just reads (and degrades to
// an error the best-effort callers absorb). Initialize is the only dir-creator and
// the policy loaders only need to READ.
func (w jobWorker) configPaths() (config.Paths, error) {
	return pathsFromFlag(w.ConfigHome)
}

func (w jobWorker) parallelSessionPolicy() (config.ParallelSessionPolicy, error) {
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return config.DefaultParallelSessionPolicy(), nil
	}
	paths, err := w.configPaths()
	if err != nil {
		return config.ParallelSessionPolicy{}, err
	}
	return config.LoadParallelSessionPolicy(paths)
}

// repoConcurrency loads the per-repo [repos."owner/repo"] scheduler overrides
// (#576), mirroring parallelSessionPolicy: an implicit/empty config home has no
// overrides (nil ⇒ every repo uses the global default), and an explicit home
// loads them from the config file. Errors are surfaced to the caller, which
// fails safe to the global default.
func (w jobWorker) repoConcurrency() ([]config.RepoConcurrency, error) {
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return nil, nil
	}
	paths, err := w.configPaths()
	if err != nil {
		return nil, err
	}
	return config.LoadRepoConcurrency(paths)
}

// resolveRepoScheduler resolves the effective worker limit and pool toggle for a
// repo's queued-job run (#576). It is behavior-preserving by default: with no
// repoFilter, no [repos."owner/repo"] section, an implicit config home, or a
// config-load error, it returns (globalLimit, w.UsePool) unchanged. A configured
// max_parallel>0 caps THAT repo's concurrency; max_parallel<=0/missing keeps the
// global default (never zero ⇒ never a stalled repo). A configured scheduler
// ("pool"/"barrier") overrides the pool toggle for that repo only.
func (w jobWorker) resolveRepoScheduler(repoFilter string, globalLimit int) (int, bool) {
	limit := globalLimit
	usePool := w.UsePool
	repo := strings.TrimSpace(repoFilter)
	if repo == "" {
		return limit, usePool
	}
	configs, err := w.repoConcurrency()
	if err != nil || len(configs) == 0 {
		return limit, usePool
	}
	entry, ok := config.RepoConcurrencyFor(configs, repo)
	if !ok {
		return limit, usePool
	}
	if entry.MaxParallel > 0 {
		limit = entry.MaxParallel
	}
	switch entry.Scheduler {
	case "pool":
		usePool = true
	case "barrier":
		usePool = false
	}
	return limit, usePool
}

// admissionPolicy loads the host-level [admission] budget config, mirroring
// parallelSessionPolicy: an implicit/empty config home uses the defaults
// (both caps 0 ⇒ off), and an explicit home loads from the config file.
func (w jobWorker) admissionPolicy() (config.AdmissionPolicy, error) {
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return config.DefaultAdmissionPolicy(), nil
	}
	paths, err := w.configPaths()
	if err != nil {
		return config.AdmissionPolicy{}, err
	}
	return config.LoadAdmissionPolicy(paths)
}

// loadAdmissionBudget builds the opt-in *admissionBudget from the [admission]
// config, returning nil when the feature is off (both caps 0/unset) or the
// config cannot be loaded — nil keeps scheduling byte-identical to today. The
// supervisors call this once at startup and share the returned pointer across all
// per-repo dispatch passes so the cap is process-global.
func (w jobWorker) loadAdmissionBudget() *admissionBudget {
	policy, err := w.admissionPolicy()
	if err != nil {
		return nil
	}
	return newAdmissionBudget(policy)
}

// perJobAdmissionEstimate maps a queued job's runtime to its admission cost
// (#365): whether it holds a resumable runtime session (so it counts against
// max_concurrent_sessions) and its configured RAM estimate (GB). A job whose
// runtime has no resumable session key — exactly the runtimes already exempt from
// the runtime session lock (queuedJobRuntimeResourceKey returns "") — is "not
// session-counted" and contributes 0 RAM, per the frozen goal. Otherwise the job
// is session-counted and its RAM is the per-runtime prior, falling back to
// default_memory_gb for a session runtime not explicitly mapped.
func perJobAdmissionEstimate(ctx context.Context, store *db.Store, job db.Job, policy config.AdmissionPolicy) admissionEstimate {
	if queuedJobRuntimeResourceKey(ctx, store, job) == "" {
		return admissionEstimate{session: false, memGB: 0}
	}
	if store == nil {
		return admissionEstimate{session: true, memGB: policy.DefaultMemoryGB}
	}
	agent, err := store.GetAgent(ctx, job.Agent)
	if err != nil {
		return admissionEstimate{session: true, memGB: policy.DefaultMemoryGB}
	}
	switch strings.TrimSpace(runtimeAgent(agent).Runtime) {
	case runtime.CodexRuntime:
		return admissionEstimate{session: true, memGB: policy.CodexMemoryGB}
	case runtime.ClaudeRuntime:
		return admissionEstimate{session: true, memGB: policy.ClaudeMemoryGB}
	case runtime.KimiRuntime, runtime.KimiCLIRuntime:
		return admissionEstimate{session: true, memGB: policy.KimiMemoryGB}
	default:
		return admissionEstimate{session: true, memGB: policy.DefaultMemoryGB}
	}
}

// admissionEstimate resolves the per-job admission cost (session-ness + RAM) for
// THIS worker's configured admission policy. It is the thunk handed to
// admissionBudget.Reserve at the dispatch reserve points: Reserve invokes it ONLY
// when the budget is active (non-nil) and the job is not already in flight, so on
// the default (no [admission] config) off path it is never called and the
// dispatch loop does ZERO extra config-file I/O or DB lookups — keeping that path
// byte-identical. A load error degrades to the default policy so a transient
// config read never silently disables a gate.
func (w jobWorker) admissionEstimate(ctx context.Context, job db.Job) admissionEstimate {
	policy, err := w.admissionPolicy()
	if err != nil {
		policy = config.DefaultAdmissionPolicy()
	}
	return perJobAdmissionEstimate(ctx, w.Store, job, policy)
}

// orchestratePolicy loads the host-level [orchestrate] cockpit policy, mirroring
// parallelSessionPolicy: an implicit/empty config home uses the defaults, and an
// explicit home loads from the config file. It is best-effort at the call site —
// a load error degrades to no cockpit (the job runs unwrapped).
func (w jobWorker) orchestratePolicy() (config.OrchestratePolicy, error) {
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return config.DefaultOrchestratePolicy(), nil
	}
	paths, err := w.configPaths()
	if err != nil {
		return config.OrchestratePolicy{}, err
	}
	return config.LoadOrchestratePolicy(paths)
}

// newCockpit constructs a *cockpit.Cockpit from the orchestrate policy, backed by
// the db store via the cockpitPaneStore shim. When the policy disables cockpit
// panes (mode "off") it returns nil so the caller skips wrapping entirely. The
// herdr binary is taken from HERDR_BIN (falling back to "herdr").
func (w jobWorker) newCockpit(policy config.OrchestratePolicy) *cockpit.Cockpit {
	if policy.CockpitMode == config.CockpitModeOff {
		return nil
	}
	return cockpit.New(cockpit.Options{
		HerdrBin:    firstNonEmpty(os.Getenv("HERDR_BIN"), "herdr"),
		MaxPanes:    policy.CockpitMaxPanes,
		PaneKeyMode: policy.CockpitPaneKey,
	}, cockpitPaneStore{store: w.Store})
}

// cockpitJobMeta builds the cockpit.JobMeta for a delegation job from the decoded
// payload, the runtime agent, and the resolved checkout dir. The pane key follows
// the policy pane-key mode: "seat" keys by agent (one pane per logical seat),
// otherwise the job id (one pane per job, the P0 default).
func cockpitJobMeta(job db.Job, payload workflow.JobPayload, agent runtime.Agent, checkout string, paneKeyMode string) cockpit.JobMeta {
	paneKey := job.ID
	if paneKeyMode == config.CockpitPaneKeySeat {
		paneKey = agent.Name
	}
	// A root coordinator job has an empty payload.RootJobID; its own id IS the
	// root (mirrors Engine.rootJobID). Without this every root collides into one
	// herdr workspace keyed by "".
	root := payload.RootJobID
	if strings.TrimSpace(root) == "" {
		root = job.ID
	}
	return cockpit.JobMeta{
		JobID:     job.ID,
		RootJobID: root,
		Agent:     agent.Name,
		Runtime:   agent.Runtime,
		Action:    job.Type,
		Branch:    payload.Branch,
		Worktree:  checkout,
		PaneKey:   paneKey,
		Depth:     payload.DelegationDepth,
	}
}

// cockpitTeeAdapter creates the per-job log the cockpit pane tails and rebuilds
// the runtime adapter to tee the child's live stdout/stderr into it. It is called
// ONLY on the wrapping path (herdr available), so non-cockpit and cockpit-off
// jobs never create a log file or tee and stay byte-identical. The log lives at
// <home>/logs/jobs/<jobid>.log and is created+truncated so each run starts fresh.
// The tee uses a TeeRunner whose inner is GroupRunner{}, so process-group kill is
// preserved and the buffered Result the adapter consumes is unchanged.
//
// It is fail-open: any failure (paths unresolved, mkdir, create, or an
// unsupported runtime) returns a nil *os.File so the caller skips teeing and the
// pane falls back to the P0 `job watch` command. The returned *os.File is the
// caller's to Close after the job runs; when nil the adapter/path are ignored.
func (w jobWorker) cockpitTeeAdapter(agent runtime.Agent, checkout string, jobID string, additionalOutput ...io.Writer) (workflow.DeliveryAdapter, string, *os.File) {
	paths, err := pathsFromFlag(w.ConfigHome)
	if err != nil {
		writeLine(w.Stdout, "job %s cockpit log path resolve failed: %v", jobID, err)
		return nil, "", nil
	}
	dir := filepath.Join(paths.Logs, "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeLine(w.Stdout, "job %s cockpit log dir create failed: %v", jobID, err)
		return nil, "", nil
	}
	// Sanitize the job id into a flat, path-safe filename: delegation/continuation
	// job ids contain '/' (e.g. "root/delegation/haiku-ocean", ".../continuation"),
	// which would nest the log into dirs that are never created and fail os.Create →
	// the live tail silently falls back to the P0 pane. A flat slug keeps it one
	// file in this dir (no deep per-job dir trees).
	logPath := filepath.Join(dir, cockpit.SafeLogName(jobID)+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		writeLine(w.Stdout, "job %s cockpit log create failed: %v", jobID, err)
		return nil, "", nil
	}
	if err := logFile.Chmod(0o600); err != nil {
		_ = logFile.Close()
		writeLine(w.Stdout, "job %s cockpit log chmod failed: %v", jobID, err)
		return nil, "", nil
	}
	return w.cockpitTeeOnFile(agent, checkout, jobID, logPath, logFile, additionalOutput...)
}

// cockpitTeeOnFile rebuilds the runtime adapter to tee the child's live
// stdout/stderr into an already-open log file, shared by the per-job (truncate)
// and per-seat (append) log paths. It is fail-open: an unsupported runtime closes
// the file and returns nils so the caller falls back to the P0 pane.
func (w jobWorker) cockpitTeeOnFile(agent runtime.Agent, checkout, jobID, logPath string, logFile *os.File, additionalOutput ...io.Writer) (workflow.DeliveryAdapter, string, *os.File) {
	outputs := append([]io.Writer{logFile}, additionalOutput...)
	adapter, err := buildRuntimeAdapter(w.ConfigHome, agent, checkout, subprocess.TeeRunner{Inner: subprocess.GroupRunner{}, Out: runtimeOutputWriter(outputs...)})
	if err != nil {
		// Unsupported runtime: this should never happen (AdapterFactory already
		// built one above), but stay fail-open rather than leak the open file.
		_ = logFile.Close()
		writeLine(w.Stdout, "job %s cockpit tee adapter build failed: %v", jobID, err)
		return nil, "", nil
	}
	return adapter, logPath, logFile
}

// cockpitLogAdapter picks the live-output log per PaneKeyMode (Task 7): seat mode
// uses the stable per-seat append log so the one seat pane tails one accumulating
// file across rounds; job mode keeps the per-job truncate log (byte-identical to
// P1). It is called only on the wrapping path (herdr available); a nil *os.File
// means fall back to the P0 pane.
func (w jobWorker) cockpitLogAdapter(cp *cockpit.Cockpit, agent runtime.Agent, checkout, jobID, rootJobID, paneKey string, seatMode bool, additionalOutput ...io.Writer) (workflow.DeliveryAdapter, string, *os.File) {
	if seatMode {
		return w.cockpitSeatLogAdapter(cp, agent, checkout, jobID, rootJobID, paneKey, additionalOutput...)
	}
	return w.cockpitTeeAdapter(agent, checkout, jobID, additionalOutput...)
}

// cockpitSeatLogAdapter opens the stable per-seat append log the seat's one pane
// tails across delegation rounds (Task 7) and tees the child's stdout/stderr into
// it. The path is <home>/logs/seats/<rootShort>/<seatSlug>.log, opened O_APPEND so
// each round's output accumulates rather than truncating the prior round's — no
// tail re-pointing needed. The log is NOT removed per job; it persists for the
// root's life and is removed by FinalizeRoot. It is fail-open: any failure
// (unresolved path, mkdir, create, unsupported runtime) returns nils so the caller
// falls back to the P0 pane.
func (w jobWorker) cockpitSeatLogAdapter(cp *cockpit.Cockpit, agent runtime.Agent, checkout, jobID, rootJobID, paneKey string, additionalOutput ...io.Writer) (workflow.DeliveryAdapter, string, *os.File) {
	logPath, logFile := w.cockpitSeatLogFile(cp, jobID, rootJobID, paneKey)
	if logFile == nil {
		return nil, "", nil
	}
	return w.cockpitTeeOnFile(agent, checkout, jobID, logPath, logFile, additionalOutput...)
}

func (w jobWorker) cockpitSeatLogFile(cp *cockpit.Cockpit, jobID, rootJobID, paneKey string) (string, *os.File) {
	logPath := cp.SeatLogPath(rootJobID, paneKey)
	if logPath == "" {
		// Home unset (cockpit could not resolve GITMOOT_HOME): fall back to the P0
		// pane rather than an unstable seat log.
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		writeLine(w.Stdout, "job %s cockpit seat log dir create failed: %v", jobID, err)
		return "", nil
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		writeLine(w.Stdout, "job %s cockpit seat log open failed: %v", jobID, err)
		return "", nil
	}
	if err := logFile.Chmod(0o600); err != nil {
		_ = logFile.Close()
		writeLine(w.Stdout, "job %s cockpit seat log chmod failed: %v", jobID, err)
		return "", nil
	}
	return logPath, logFile
}

// finalizeCockpitRootIfDone tears the root's cockpit down once the coordination
// tree it belongs to has terminated (Task 7/8, seat mode only). It runs on a
// wrapped seat-mode job's return: if every job sharing the root is terminal, it
// calls FinalizeRoot (close panes / workspace, delete rows, remove seat logs) and,
// when this job is the terminal coordinator continuation, FocusRoot to surface the
// reconvene view. Everything is best-effort: it is deferred on a detached context
// so a cockpit/herdr problem never affects the job. Job mode never reaches here, so
// its per-Deliver teardown stays byte-identical.
func (w jobWorker) finalizeCockpitRootIfDone(cp *cockpit.Cockpit, job db.Job, payload workflow.JobPayload, rootJobID string) {
	ctx := context.Background()
	// Cheap scoped guard before the full job-table scan: short-circuit only when the
	// root has NEITHER a live pane row NOR a registered workspace (none opened, or
	// already finalized) — there is then nothing to tear down, so the redundant
	// rootTreeTerminal scans on every in-tree job's completion are skipped. Job mode
	// deletes pane rows per-Deliver, so by root-terminal the pane list is empty while
	// a cockpit_workspaces row still needs closing; gating on the pane list alone
	// would skip that workspace teardown (the leftover-workspace bug). Any store error
	// falls through to the (idempotent, best-effort) finalize rather than skipping.
	if panes, perr := w.Store.ListCockpitPanesByRoot(ctx, rootJobID); perr == nil && len(panes) == 0 {
		if _, found, wsErr := w.Store.GetWorkspaceForRoot(ctx, rootJobID); wsErr == nil && !found {
			return
		}
	}
	done, err := w.rootTreeTerminal(ctx, rootJobID)
	if err != nil {
		writeLine(w.Stdout, "job %s cockpit root-finalize check failed: %v", job.ID, err)
		return
	}
	if !done {
		return
	}
	// A terminal continuation that absorbed the children (a finalize continuation,
	// or a coordinator continuation that returned no further delegations) is the
	// reconvene point: surface the root workspace so the synthesized verdict —
	// which lands in the coordinator's own pane (its continuation shares the
	// coordinator seat in seat mode) — is brought forward.
	if w.isReconveneContinuation(ctx, job, payload) {
		cp.FocusRoot(ctx, rootJobID)
	}
	cp.FinalizeRoot(ctx, rootJobID)
}

// rootTreeTerminal reports whether every job in the coordination tree rooted at
// rootJobID is terminal (succeeded/failed/cancelled) — i.e. nothing is still
// queued, running, or blocked (a blocked job can resume, so it is not terminal). It lists jobs and matches the root id against each
// job's own id (the root coordinator) or its payload RootJobID (children +
// continuations), mirroring the engine's per-root reasoning. It fails closed
// (returns false) on any unparseable payload so a transient hiccup never triggers
// a premature teardown.
func (w jobWorker) rootTreeTerminal(ctx context.Context, rootJobID string) (bool, error) {
	rootJobID = strings.TrimSpace(rootJobID)
	if rootJobID == "" {
		return false, nil
	}
	jobs, err := w.Store.ListJobs(ctx)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		inTree := j.ID == rootJobID
		if !inTree {
			p, perr := daemonJobPayload(j)
			if perr != nil {
				// An unparseable job payload could belong to the tree; do not finalize
				// while its membership/state is unknown.
				return false, nil
			}
			inTree = strings.TrimSpace(p.RootJobID) == rootJobID
		}
		if !inTree {
			continue
		}
		// Root-tree finalization uses FINAL (resumability) semantics, NOT settled:
		// a blocked job is deliberately non-final (it can resume via RetryJob), so
		// the tree is not terminal while any in-tree job is blocked. Finalizing then
		// would tear down a pane + seat log the job still needs. The engine's
		// graceful-finalize continuation provides the real terminal signal for a
		// stuck tree. See #632 (IsFinalJobState vs IsSettledJobState).
		if !workflow.IsFinalJobState(j.State) {
			return false, nil
		}
	}
	// Every in-tree job (if any) is terminal — the tree is done. An already-pruned
	// root (no jobs found) is also terminal: a late finalize is a harmless no-op.
	return true, nil
}

// isReconveneContinuation reports whether this job is the coordinator's terminal
// reconvene point: a finalize continuation, or any coordinator continuation that
// returned no further delegations (so the tree stops here). It is the signal to
// refocus the root workspace on the synthesized verdict (Task 8).
func (w jobWorker) isReconveneContinuation(ctx context.Context, job db.Job, payload workflow.JobPayload) bool {
	if payload.DelegationFinalize {
		return true
	}
	// A continuation job carries a parent (the prior coordinator job in the chain).
	// When such a continuation returns no delegations, the coordination tree has
	// reconvened on it.
	if strings.TrimSpace(payload.ParentJobID) == "" {
		// The root coordinator itself: a reconvene point only if it spawned no
		// children (it ran to completion without delegating).
		children, err := w.Store.ListJobsByParent(ctx, job.ID)
		if err != nil {
			return false
		}
		return len(children) == 0
	}
	if payload.Result != nil && len(payload.Result.Delegations) > 0 {
		return false
	}
	return true
}

// maybeWrapCockpit decides whether a job's delivery is wrapped in a herdr pane.
// It is a pure helper (no daemon state) so the wrap-vs-passthrough decision is
// directly unit-testable. The returned unavailable flag is true exactly when the
// caller should emit a single cockpit_unavailable job event:
//   - not requested (payload.Cockpit false): inner unchanged, no event.
//   - requested but the policy mode is off: skip entirely, inner unchanged, no
//     event (an off host opted out, so there is nothing to warn about).
//   - requested, mode not off, but the cockpit is nil or herdr is not available:
//     inner unchanged, unavailable=true so the caller emits the event.
//   - requested and available: the wrapped adapter, no event.
//
// Cockpit construction/Available failures are fail-open by contract: cp.Wrap
// already returns inner untouched when Available is false.
func maybeWrapCockpit(cp *cockpit.Cockpit, requested bool, modeOff bool, inner workflow.DeliveryAdapter, meta cockpit.JobMeta) (workflow.DeliveryAdapter, bool) {
	if !requested || modeOff {
		return inner, false
	}
	if !maybeWrapCockpitAvailable(cp, requested, modeOff) {
		return inner, true
	}
	return cp.Wrap(inner, meta), false
}

// maybeWrapCockpitAvailable reports whether the cockpit will actually wrap this
// job's delivery in a pane: requested, the host did not opt out (mode off), and
// herdr is reachable. It is the single source of truth the daemon uses BOTH to
// decide whether to set up the per-job tee log (so logs/tees are created only on
// the wrapping path) and inside maybeWrapCockpit's final decision, so the two can
// never drift. Availability is cached (availableTTL) so the extra call is cheap.
func maybeWrapCockpitAvailable(cp *cockpit.Cockpit, requested bool, modeOff bool) bool {
	if !requested || modeOff || cp == nil {
		return false
	}
	return cp.Available(context.Background())
}

func tempWorkerEligible(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload, agent runtime.Agent, policy config.ParallelSessionPolicy, now time.Time) tempWorkerEligibility {
	if payload.Ephemeral != nil {
		// An ephemeral job already runs directly on its own throwaway worker;
		// forking it into a second temp worker would double-spawn.
		return tempWorkerEligibility{Reason: "ephemeral worker runs directly"}
	}
	if strings.TrimSpace(payload.RuntimeOverride) != "" {
		// An override job already runs on its own per-job session (a fresh ref or
		// an explicit --session); forking a temp worker from the effective agent
		// would re-derive a second session for the same one-shot job.
		return tempWorkerEligibility{Reason: "runtime override runs on its own session"}
	}
	if payload.DelegationReason == "temp_worker_merge_back" {
		return tempWorkerEligibility{Reason: "merge-back waits for original runtime session"}
	}
	if payload.DelegationReason == "runtime_session_busy" {
		return tempWorkerEligibility{Reason: "delegated temp worker waits for assigned runtime session"}
	}
	if policy.SameSession != config.ParallelSessionForkTempSession {
		return tempWorkerEligibility{Reason: "parallel_sessions.same_session is queue"}
	}
	switch agent.Runtime {
	case runtime.CodexRuntime, runtime.ClaudeRuntime, runtime.KimiRuntime, runtime.KimiCLIRuntime:
	default:
		return tempWorkerEligibility{Reason: fmt.Sprintf("runtime %s does not support temp workers", agent.Runtime)}
	}
	if !parallelSessionActionAllowed(job.Type, policy.EligibleActions) {
		return tempWorkerEligibility{Reason: fmt.Sprintf("action %s is not eligible", job.Type)}
	}
	if readOnlyImplementationBlocked(job.Type, agent) {
		return tempWorkerEligibility{Reason: "implementation requires writable agent policy"}
	}
	if strings.TrimSpace(job.Type) == "implement" {
		path, ok := queuedJobTaskWorktreePath(ctx, store, payload)
		if !ok {
			return tempWorkerEligibility{Reason: "implementation requires task worktree"}
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return tempWorkerEligibility{Reason: "implementation task worktree is missing"}
		}
	}
	if store != nil {
		count, err := store.CountActiveAgentInstances(ctx, tempWorkerAgentType(agent.Name), agent.AutonomyPolicy, now)
		if err != nil {
			return tempWorkerEligibility{Reason: fmt.Sprintf("count active temp workers: %v", err)}
		}
		if count >= policy.MaxTempSessionsPerAgent {
			return tempWorkerEligibility{Reason: fmt.Sprintf("max temp workers reached for %s", agent.Name)}
		}
	}
	return tempWorkerEligibility{Eligible: true}
}

func parallelSessionActionAllowed(action string, eligibleActions []string) bool {
	action = strings.TrimSpace(action)
	for _, candidate := range eligibleActions {
		if strings.TrimSpace(candidate) == action {
			return true
		}
	}
	return false
}

func tempWorkerAgentType(agentName string) string {
	return "temp:" + strings.TrimSpace(agentName)
}

type tempWorkerStartResult struct {
	Agent       runtime.Agent
	IdleTimeout time.Duration
	JobTimeout  time.Duration
}

func (w jobWorker) runWithTempWorker(ctx context.Context, job db.Job, payload workflow.JobPayload, original runtime.Agent, checkout string, policy config.ParallelSessionPolicy, reason string) error {
	started, err := w.startTempWorker(ctx, job, payload, original, checkout)
	if err != nil {
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "temp_worker_failed", Message: err.Error()}); eventErr != nil {
			return eventErr
		}
		waitMessage := fmt.Sprintf("%s; temp worker start failed: %v", reason, err)
		// Once per wait episode (#598); busy error returned unconditionally so the
		// pool dispatcher observes the bounce.
		if !runtimeLockWaitEpisodeOpen(job.ID) {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_lock_wait", Message: waitMessage}); eventErr != nil {
				return eventErr
			}
			markRuntimeLockWaitEpisode(job.ID)
			writeLine(w.Stdout, "job %s waiting: %s", job.ID, waitMessage)
		}
		return fmt.Errorf("%w: %s", errRuntimeSessionBusy, waitMessage)
	}
	// A per-delegation timeout on the payload overrides the agent-type job
	// timeout for both the lock TTL and the run deadline below.
	if d, perr := time.ParseDuration(strings.TrimSpace(payload.JobTimeout)); perr == nil && d > 0 {
		started.JobTimeout = d
	}
	payload.OriginalAgent = original.Name
	payload.DelegatedAgent = started.Agent.Name
	payload.DelegationReason = "runtime_session_busy"
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	delegated, err := w.Store.DelegateQueuedJob(ctx, job.ID, original.Name, started.Agent.Name, string(encoded), db.JobEvent{
		JobID:   job.ID,
		Kind:    "temp_worker_delegated",
		Message: fmt.Sprintf("delegated from %s to %s: %s", original.Name, started.Agent.Name, reason),
	})
	if err != nil {
		w.cleanupTempWorker(context.Background(), started.Agent.Name)
		return err
	}
	if !delegated {
		w.cleanupTempWorker(context.Background(), started.Agent.Name)
		return nil
	}
	delegatedJob, err := w.Store.GetJob(ctx, job.ID)
	if err != nil {
		return err
	}
	adapter, err := w.AdapterFactory(started.Agent, checkout)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	writeLine(w.Stdout, "running job %s for temporary worker %s in %s", job.ID, started.Agent.Name, payload.Repo)
	// Same lease-outlives-context invariant as run(): the temp-worker run context is
	// armed at started.JobTimeout below, so the lease must be jobTimeout+grace to
	// cover teardown and avoid the #536 live-worker reap+requeue window.
	tempLockTTL := defaultDaemonRunningJobStaleAfter
	if started.JobTimeout > 0 {
		tempLockTTL = started.JobTimeout + runtimeLeaseTeardownGrace
	}
	releaseLock, acquired, lockKey, ownerToken, err := acquireRuntimeSessionLock(ctx, w.Store, delegatedJob.ID, started.Agent, time.Now().UTC(), tempLockTTL)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	if !acquired {
		message := fmt.Sprintf("runtime session %s is busy", lockKey)
		// Once per wait episode (#598); busy error returned unconditionally so the
		// pool dispatcher observes the bounce.
		if !runtimeLockWaitEpisodeOpen(delegatedJob.ID) {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: delegatedJob.ID, Kind: "runtime_lock_wait", Message: message}); eventErr != nil {
				return eventErr
			}
			markRuntimeLockWaitEpisode(delegatedJob.ID)
			writeLine(w.Stdout, "job %s waiting: %s", delegatedJob.ID, message)
		}
		return fmt.Errorf("%w: %s", errRuntimeSessionBusy, message)
	}
	// Acquired the temp worker's runtime lock (delegatedJob.ID == job.ID; the
	// delegation keeps the same job id): close any open wait episode.
	endRuntimeLockWaitEpisode(delegatedJob.ID)
	defer func() {
		if err := releaseLock(context.Background()); err != nil {
			writeLine(w.Stdout, "job %s temp runtime lock release failed: %v", delegatedJob.ID, err)
		}
	}()
	stopRuntimeLockHeartbeat := startRuntimeSessionLockHeartbeat(ctx, w.Store, lockKey, ownerToken, tempLockTTL)
	defer stopRuntimeLockHeartbeat()
	// See runQueuedJob: thread the owner token so terminal cleanup recognizes this
	// run's own still-held lock and does not refuse the healthy-path cleanup (#536).
	ctx = workflow.WithRuntimeSelfOwnerToken(ctx, ownerToken)
	// Produce temp workers use the same post-admission filesystem authorization
	// and Landlock adapter wrapping as the primary worker path. Without this seam,
	// runtime-session contention could route Claude/Kimi around the launch sandbox.
	if err := applyProduceRuntimeGrants(ctx, w.Store, w.ConfigHome, delegatedJob, payload, &started.Agent); err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	// Shared tool-cache grant (#1113 lever 1): mirrors the primary worker path so
	// runtime-session contention rerouting through the temp worker doesn't
	// silently lose the cache redirect. Fail-open (disk hygiene, not security).
	var tempToolCacheEnv []string
	if cachePaths, cacheErr := w.configPaths(); cacheErr != nil {
		writeLine(w.Stdout, "job %s tool cache config load failed: %v", delegatedJob.ID, cacheErr)
	} else if env, grantErr := applyIsolatedToolCacheGrants(cachePaths, payload, &started.Agent); grantErr != nil {
		writeLine(w.Stdout, "job %s tool cache grant failed: %v", delegatedJob.ID, grantErr)
	} else {
		tempToolCacheEnv = env
	}
	adapter, err = wrapProduceSandboxAdapter(delegatedJob.Type, started.Agent, adapter)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	if len(tempToolCacheEnv) > 0 {
		if envAdapter, envErr := injectDeliveryAdapterEnv(adapter, tempToolCacheEnv); envErr != nil {
			writeLine(w.Stdout, "job %s tool cache env inject failed: %v", delegatedJob.ID, envErr)
		} else {
			adapter = envAdapter
		}
	}
	adapter = pipeline.WrapPipelineEnvDeliveryAdapter(w.Store, w.ConfigHome, payload, adapter)
	// Temp-session delivery is a separate early-return path; attach the same
	// append-only capture here or it would be absent from the trajectory corpus.
	_, retainedLogFile, retainedLogErr := openRetainedTranscriptLog(w.ConfigHome, delegatedJob.ID)
	if retainedLogErr != nil {
		writeLine(w.Stdout, "job %s transcript log open failed: %v", delegatedJob.ID, retainedLogErr)
	}
	if retainedLogFile != nil {
		teeAdapter, teeErr := appendDeliveryAdapterOutput(adapter, retainedLogFile)
		if teeErr != nil {
			_ = retainedLogFile.Close()
			writeLine(w.Stdout, "job %s transcript tee build failed: %v", delegatedJob.ID, teeErr)
		} else {
			adapter = teeAdapter
			defer func() { _ = retainedLogFile.Close() }()
		}
	}
	if err := w.Store.MarkAgentInstanceRunning(ctx, started.Agent.Name, time.Now().UTC(), started.JobTimeout); err != nil {
		if finishErr := w.finishQueuedJob(ctx, delegatedJob.ID, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	defer func() {
		if err := w.Store.TouchAgentInstance(context.Background(), started.Agent.Name, time.Now().UTC(), started.IdleTimeout); err != nil {
			writeLine(w.Stdout, "job %s temp worker state update failed: %v", delegatedJob.ID, err)
		}
	}()
	runCtx, stopRun := w.runningJobContext(ctx, job.ID)
	defer stopRun()
	if started.JobTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, started.JobTimeout)
		defer cancel()
	}
	engine := w.WorkflowFactory(checkout)
	_, err = engine.RunJob(runCtx, delegatedJob.ID, started.Agent, adapter)
	if err != nil {
		if markErr := w.handleRunJobError(ctx, delegatedJob.ID, err); markErr != nil {
			return markErr
		}
		if reconcileErr := engine.ReconcileTerminalDrivingJob(ctx, delegatedJob.ID); reconcileErr != nil {
			return reconcileErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		writeLine(w.Stdout, "job %s failed: %v", delegatedJob.ID, err)
		return nil
	}
	if policy.MergeBack == config.ParallelSessionMergeBackSummary {
		if err := w.queueTempWorkerMergeBack(ctx, delegatedJob.ID, original, started.Agent); err != nil {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: delegatedJob.ID, Kind: "temp_worker_merge_back_failed", Message: err.Error()}); eventErr != nil {
				return eventErr
			}
			return err
		}
	}
	if err := engine.ReconcileTerminalDrivingJob(ctx, delegatedJob.ID); err != nil {
		return err
	}
	_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, nil)
	writeLine(w.Stdout, "job %s completed by temporary worker %s", delegatedJob.ID, started.Agent.Name)
	return nil
}

func (w jobWorker) queueTempWorkerMergeBack(ctx context.Context, completedJobID string, original runtime.Agent, tempAgent runtime.Agent) error {
	completedJob, err := w.Store.GetJob(ctx, completedJobID)
	if err != nil {
		return err
	}
	payload, err := daemonJobPayload(completedJob)
	if err != nil {
		return err
	}
	if payload.Result == nil {
		return fmt.Errorf("completed temp-worker job %s has no result", completedJob.ID)
	}
	mergeBackID := completedJob.ID + "-merge-back"
	if _, err := w.Store.GetJob(ctx, mergeBackID); err == nil {
		return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: completedJob.ID, Kind: "temp_worker_merge_back_existing", Message: fmt.Sprintf("summary merge-back job %s already exists", mergeBackID)})
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	request := workflow.JobRequest{
		ID:               mergeBackID,
		Agent:            original.Name,
		Action:           "ask",
		Model:            payload.Model,
		Effort:           payload.Effort,
		Repo:             payload.Repo,
		Branch:           payload.Branch,
		GoalID:           payload.GoalID,
		TaskID:           payload.TaskID,
		TaskTitle:        payload.TaskTitle,
		LeadAgent:        payload.LeadAgent,
		Reviewers:        payload.Reviewers,
		ReviewRound:      payload.ReviewRound,
		Sender:           tempAgent.Name,
		Instructions:     tempWorkerMergeBackInstructions(completedJob, payload, tempAgent.Name),
		OriginalAgent:    original.Name,
		DelegatedAgent:   tempAgent.Name,
		DelegationReason: "temp_worker_merge_back",
		Constraints: []string{
			"This is a temp-worker merge-back summary only.",
			"Do not edit files, create commits, open pull requests, or dispatch more agents unless the summary explicitly requires follow-up.",
		},
	}
	if _, err := (workflow.Mailbox{Store: w.Store, CanaryEnabled: canaryRoutingEnabled(w.workflowHome()), RuntimeDefaultModel: runtimeDefaultModelResolver(w.workflowHome()), RequireWorkflowPolicy: requireWorkflowPolicyResolver(w.workflowHome()), OrgPolicy: orgPolicyResolver(w.workflowHome())}).Enqueue(ctx, request); err != nil {
		return err
	}
	return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: completedJob.ID, Kind: "temp_worker_merge_back_queued", Message: fmt.Sprintf("queued summary merge-back job %s for %s", mergeBackID, original.Name)})
}

func tempWorkerMergeBackInstructions(job db.Job, payload workflow.JobPayload, tempAgentName string) string {
	result := payload.Result
	var builder strings.Builder
	fmt.Fprintf(&builder, "Temporary worker %s completed job %s.\n", tempAgentName, job.ID)
	fmt.Fprintf(&builder, "Repo: %s\n", payload.Repo)
	if strings.TrimSpace(payload.Branch) != "" {
		fmt.Fprintf(&builder, "Branch: %s\n", payload.Branch)
	}
	if payload.PullRequest > 0 {
		fmt.Fprintf(&builder, "Pull request: #%d\n", payload.PullRequest)
	}
	if strings.TrimSpace(payload.HeadSHA) != "" {
		fmt.Fprintf(&builder, "Head SHA: %s\n", payload.HeadSHA)
	}
	fmt.Fprintf(&builder, "Decision: %s\n", result.Decision)
	if strings.TrimSpace(result.Summary) != "" {
		fmt.Fprintf(&builder, "Summary: %s\n", result.Summary)
	}
	appendMergeBackList(&builder, "Changes made", result.ChangesMade)
	appendMergeBackList(&builder, "Tests run", result.TestsRun)
	appendMergeBackList(&builder, "Needs", result.Needs)
	builder.WriteString("\nAcknowledge the summary and keep any follow-up concise.")
	return builder.String()
}

func appendMergeBackList(builder *strings.Builder, label string, values []string) {
	values = compactMergeBackStrings(values)
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(builder, "%s:\n", label)
	for _, value := range values {
		fmt.Fprintf(builder, "- %s\n", value)
	}
}

func compactMergeBackStrings(values []string) []string {
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (w jobWorker) startTempWorker(ctx context.Context, job db.Job, payload workflow.JobPayload, original runtime.Agent, checkout string) (tempWorkerStartResult, error) {
	idleTimeout := 20 * time.Minute
	jobTimeout := defaultDaemonRunningJobStaleAfter
	if managed, err := w.managedJobConfig(ctx, original.Name); err == nil && managed.OK {
		idleTimeout = managed.IdleTimeout
		jobTimeout = managed.JobTimeout
	} else if err != nil {
		return tempWorkerStartResult{}, err
	}
	tempAgent := original
	tempAgent.Name = tempWorkerInstanceName(original.Name, job.ID)
	tempAgent.RuntimeRef = ""
	// A temp worker's session is started for this one job and disposed after it
	// — single-use, so session-cumulative usage (codex, #658) is the job's cost.
	tempAgent.SingleUseSession = true
	var cachedTemplate db.AgentTemplate
	if tempAgent.TemplateID != "" {
		var err error
		cachedTemplate, err = loadInstalledTemplate(ctx, w.Store, tempAgent.TemplateID)
		if err != nil {
			return tempWorkerStartResult{}, err
		}
	}
	now := time.Now().UTC()
	reserved := db.AgentInstance{
		Name:           tempAgent.Name,
		Type:           tempWorkerAgentType(original.Name),
		Runtime:        tempAgent.Runtime,
		RuntimeRef:     "starting:" + tempAgent.Name,
		RepoFullName:   payload.Repo,
		Role:           tempAgent.Role,
		TemplateID:     tempAgent.TemplateID,
		Model:          tempAgent.Model,
		Effort:         tempAgent.Effort,
		Capabilities:   tempAgent.Capabilities,
		AutonomyPolicy: tempAgent.AutonomyPolicy,
		State:          "starting",
		CreatedAt:      formatManagedAgentTime(now),
		LastUsedAt:     formatManagedAgentTime(now),
		ExpiresAt:      formatManagedAgentTime(now.Add(jobTimeout)),
	}
	if err := w.Store.UpsertAgentInstance(ctx, reserved); err != nil {
		return tempWorkerStartResult{}, err
	}
	adapter, err := w.StartAdapterFactory(tempAgent.Runtime, checkout)
	if err != nil {
		_ = w.Store.DeleteAgentInstance(context.Background(), reserved.Name)
		return tempWorkerStartResult{}, err
	}
	started, err := adapter.Start(ctx, runtime.StartRequest{Agent: tempAgent, Prompt: agentStartupPrompt(tempAgent, cachedTemplate)})
	if err != nil {
		_ = w.Store.DeleteAgentInstance(context.Background(), reserved.Name)
		return tempWorkerStartResult{}, err
	}
	tempAgent.RuntimeRef = strings.TrimSpace(started.RuntimeRef)
	if err := runtime.ValidateAgent(tempAgent); err != nil {
		_ = w.Store.DeleteAgentInstance(context.Background(), reserved.Name)
		return tempWorkerStartResult{}, err
	}
	instance := reserved
	instance.RuntimeRef = tempAgent.RuntimeRef
	instance.State = "idle"
	if err := w.Store.UpsertAgentInstance(ctx, instance); err != nil {
		_ = w.Store.DeleteAgentInstance(context.Background(), reserved.Name)
		return tempWorkerStartResult{}, err
	}
	return tempWorkerStartResult{Agent: tempAgent, IdleTimeout: idleTimeout, JobTimeout: jobTimeout}, nil
}

// startEphemeralWorker materializes a throwaway agent for a job whose payload
// carries an inline worker spec, generalizing the temp-worker machinery from
// "fork an existing agent" to "spawn from a spec". It persists the agent (so the
// rest of run's flow — GetAgent, the engine's executor checks — finds it),
// associates payload.Repo via the agent's RepoScope, and reserves + starts a
// runtime session (mirroring startTempWorker). The agent name on the job is
// already the engine-assigned "-ephemeral-" name; callers register a deferred
// cleanupTempWorker to auto-dispose the worker on every exit path. The worker
// runs read-only unless the spec opts into a writable autonomy policy.
func (w jobWorker) startEphemeralWorker(ctx context.Context, job db.Job, payload workflow.JobPayload) (err error) {
	spec := payload.Ephemeral
	if spec == nil {
		return errors.New("ephemeral worker requires a spec")
	}
	capabilities := spec.Capabilities
	if len(capabilities) == 0 {
		capabilities = []string{job.Type}
	}
	// Least privilege: default read-only, except an implement must be able to
	// write. The spec may still opt into a different (validated) policy. Note an
	// EMPTY-policy implement spec is already refused upstream by
	// validateEphemeralSpec (#452), so this implement default is now defense in
	// depth for any path that reaches here without that validation.
	defaultPolicy := runtime.AutonomyPolicyReadOnly
	if job.Type == "implement" {
		defaultPolicy = runtime.AutonomyPolicyWorkspaceWrite
	}
	policy := firstNonEmpty(strings.TrimSpace(spec.AutonomyPolicy), defaultPolicy)
	// Role is required by runtime.ValidateAgent but optional on the spec; fall
	// back to the job action (e.g. "review"/"implement"), then a generic role.
	role := firstNonEmpty(strings.TrimSpace(spec.Role), strings.TrimSpace(job.Type), "worker")
	ephemeralAgent := runtime.Agent{
		Name:           job.Agent,
		Role:           role,
		Runtime:        spec.Runtime,
		Model:          spec.Model,
		Effort:         spec.Effort,
		TemplateID:     spec.Template,
		Capabilities:   capabilities,
		AutonomyPolicy: policy,
		RepoScope:      payload.Repo,
	}
	// Persisting with RepoScope set associates the worker with payload.Repo
	// (agent_repos), mirroring how a normal agent gains repo access.
	if err := w.Store.UpsertAgent(ctx, dbAgent(ephemeralAgent)); err != nil {
		return err
	}
	// The agent row (and, below, its instance + a live runtime session) now
	// exist. Dispose them if any later bring-up step fails so a partial
	// materialization cannot leak an agent/instance/session — mirroring
	// startTempWorker's cleanup-on-error. (The named return err is set by the
	// `return err` paths below.)
	defer func() {
		if err != nil {
			w.cleanupTempWorker(context.Background(), ephemeralAgent.Name)
		}
	}()
	// Normalize the stored policy back onto the in-memory agent so the runtime
	// session is started with the same sandbox the rest of run will use.
	ephemeralAgent.AutonomyPolicy = runtime.NormalizeStoredAutonomyPolicy(ephemeralAgent.AutonomyPolicy)
	checkout, err := w.CheckoutValidator(ctx, job, payload, ephemeralAgent)
	if err != nil {
		return err
	}
	var cachedTemplate db.AgentTemplate
	if ephemeralAgent.TemplateID != "" {
		cachedTemplate, err = loadInstalledTemplate(ctx, w.Store, ephemeralAgent.TemplateID)
		if err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	reserved := db.AgentInstance{
		Name:    ephemeralAgent.Name,
		Type:    tempWorkerAgentType(ephemeralWorkerInstanceOrigin),
		Runtime: ephemeralAgent.Runtime,
		// "starting:" placeholder ref keeps the reserved row valid before the
		// adapter returns the real runtime ref.
		RuntimeRef:     "starting:" + ephemeralAgent.Name,
		RepoFullName:   payload.Repo,
		Role:           ephemeralAgent.Role,
		TemplateID:     ephemeralAgent.TemplateID,
		Model:          ephemeralAgent.Model,
		Effort:         ephemeralAgent.Effort,
		Capabilities:   ephemeralAgent.Capabilities,
		AutonomyPolicy: ephemeralAgent.AutonomyPolicy,
		State:          "starting",
		CreatedAt:      formatManagedAgentTime(now),
		LastUsedAt:     formatManagedAgentTime(now),
		ExpiresAt:      formatManagedAgentTime(now.Add(defaultDaemonRunningJobStaleAfter)),
	}
	if err := w.Store.UpsertAgentInstance(ctx, reserved); err != nil {
		return err
	}
	adapter, err := w.StartAdapterFactory(ephemeralAgent.Runtime, checkout)
	if err != nil {
		return err
	}
	started, err := adapter.Start(ctx, runtime.StartRequest{Agent: ephemeralAgent, Prompt: agentStartupPrompt(ephemeralAgent, cachedTemplate)})
	if err != nil {
		return err
	}
	ephemeralAgent.RuntimeRef = strings.TrimSpace(started.RuntimeRef)
	if err := runtime.ValidateAgent(ephemeralAgent); err != nil {
		return err
	}
	// Persist the live runtime_ref on both the agent row (so GetAgent below
	// resolves a runnable session) and the instance.
	if err := w.Store.UpsertAgent(ctx, dbAgent(ephemeralAgent)); err != nil {
		return err
	}
	instance := reserved
	instance.RuntimeRef = ephemeralAgent.RuntimeRef
	instance.State = "idle"
	if err := w.Store.UpsertAgentInstance(ctx, instance); err != nil {
		return err
	}
	if err := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "ephemeral_worker_started", Message: fmt.Sprintf("materialized %s worker %s", ephemeralAgent.Runtime, ephemeralAgent.Name)}); err != nil {
		return err
	}
	writeLine(w.Stdout, "materialized ephemeral worker %s (%s) for job %s in %s", ephemeralAgent.Name, ephemeralAgent.Runtime, job.ID, payload.Repo)
	return nil
}

// ephemeralWorkerInstanceOrigin is the synthetic "original" agent name used in an
// ephemeral worker's instance type. It has no registered instance, so
// managedJobConfig treats the worker as unmanaged (no agent-type config), which
// is correct for a spec-spawned worker that does not belong to a managed pool.
const ephemeralWorkerInstanceOrigin = "gitmoot-ephemeral-spec"

func (w jobWorker) cleanupTempWorker(ctx context.Context, agentName string) {
	if err := w.Store.DeleteAgentInstance(ctx, agentName); err != nil {
		writeLine(w.Stdout, "temp worker %s instance cleanup failed: %v", agentName, err)
	}
	if removed, err := w.Store.RemoveAgent(ctx, agentName); err != nil {
		writeLine(w.Stdout, "temp worker %s agent cleanup failed: %v", agentName, err)
	} else if removed {
		writeLine(w.Stdout, "temp worker %s agent cleanup removed regular agent row", agentName)
	}
}

func tempWorkerInstanceName(agentName string, jobID string) string {
	base := strings.Trim(strings.ToLower(agentName), "-_ ")
	if base == "" {
		base = "agent"
	}
	job := strings.Trim(strings.ToLower(jobID), "-_ ")
	if job == "" {
		job = strconv.FormatInt(time.Now().UTC().UnixNano(), 16)
	}
	name := base + "-temp-" + job
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-' || r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-_")
}

type managedJobRuntimeConfig struct {
	OK          bool
	JobTimeout  time.Duration
	IdleTimeout time.Duration
}

func (w jobWorker) managedJobConfig(ctx context.Context, agentName string) (managedJobRuntimeConfig, error) {
	instance, err := w.Store.GetAgentInstance(ctx, agentName)
	if errors.Is(err, sql.ErrNoRows) {
		return managedJobRuntimeConfig{}, nil
	}
	if err != nil {
		return managedJobRuntimeConfig{}, err
	}
	configType := instance.Type
	if original := originalAgentForTempWorkerType(instance.Type); original != "" {
		originalInstance, err := w.Store.GetAgentInstance(ctx, original)
		if errors.Is(err, sql.ErrNoRows) {
			return managedJobRuntimeConfig{}, nil
		}
		if err != nil {
			return managedJobRuntimeConfig{}, err
		}
		configType = originalInstance.Type
	}
	types, err := loadAgentTypeConfig(w.ConfigHome)
	if err != nil {
		return managedJobRuntimeConfig{}, err
	}
	agentType, ok := types[configType]
	if !ok {
		return managedJobRuntimeConfig{}, fmt.Errorf("agent type %q not found for managed instance %s", configType, agentName)
	}
	jobTimeout, err := time.ParseDuration(agentType.JobTimeout)
	if err != nil {
		return managedJobRuntimeConfig{}, fmt.Errorf("agent type %s job_timeout: %w", configType, err)
	}
	if jobTimeout <= 0 {
		return managedJobRuntimeConfig{}, fmt.Errorf("agent type %s job_timeout must be positive", configType)
	}
	idleTimeout, err := time.ParseDuration(agentType.IdleTimeout)
	if err != nil {
		return managedJobRuntimeConfig{}, fmt.Errorf("agent type %s idle_timeout: %w", configType, err)
	}
	if idleTimeout <= 0 {
		return managedJobRuntimeConfig{}, fmt.Errorf("agent type %s idle_timeout must be positive", configType)
	}
	return managedJobRuntimeConfig{OK: true, JobTimeout: jobTimeout, IdleTimeout: idleTimeout}, nil
}

// effectiveJobTimeout returns the timeout to enforce for a job: the
// per-delegation payload.JobTimeout when it parses to a positive duration,
// otherwise the agent-type managed.JobTimeout, otherwise the daemon stale window
// for unmanaged jobs (so an unmanaged job still has a watchdog, #555). This value
// drives the run context deadline directly; the runtime-session lock TTL is this
// value PLUS runtimeLeaseTeardownGrace (#536/#560) so the lease strictly outlives
// the deadline and the terminal teardown — the lock cannot expire while the
// worker is still finishing, which would otherwise let the reaper requeue a live
// job onto its dirty worktree.
func effectiveJobTimeout(payload workflow.JobPayload, managed managedJobRuntimeConfig) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(payload.JobTimeout)); err == nil && d > 0 {
		return d
	}
	if managed.JobTimeout > 0 {
		return managed.JobTimeout
	}
	return daemonRunningJobStaleAfter
}

func originalAgentForTempWorkerType(typ string) string {
	original, ok := strings.CutPrefix(strings.TrimSpace(typ), "temp:")
	if !ok {
		return ""
	}
	return strings.TrimSpace(original)
}

func (w jobWorker) runningJobContext(ctx context.Context, jobID string) (context.Context, func()) {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(daemonJobCancelPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				job, err := w.Store.GetJob(ctx, jobID)
				if err == nil && job.State == string(workflow.JobCancelled) {
					cancel()
					return
				}
			}
		}
	}()
	return runCtx, func() {
		cancel()
		<-done
	}
}

func blockTaskForPermissionBlockedJob(ctx context.Context, store *db.Store, job db.Job) error {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return err
	}
	if strings.TrimSpace(payload.TaskID) == "" {
		return nil
	}
	task := db.Task{
		ID:           payload.TaskID,
		RepoFullName: payload.Repo,
		GoalID:       payload.GoalID,
		Title:        payload.TaskTitle,
		State:        string(workflow.TaskBlocked),
		Branch:       payload.Branch,
	}
	existing, err := store.GetTask(ctx, payload.TaskID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if existing.State == string(workflow.TaskDismissed) {
			return fmt.Errorf("task %s is dismissed; permission-blocked job cannot resurrect it", existing.ID)
		}
		if task.RepoFullName == "" {
			task.RepoFullName = existing.RepoFullName
		}
		if task.GoalID == "" {
			task.GoalID = existing.GoalID
		}
		if task.Title == "" {
			task.Title = existing.Title
		}
		if task.Branch == "" {
			task.Branch = existing.Branch
		}
	}
	return store.UpsertTask(ctx, task)
}

func (w jobWorker) jobNeedsAdvanceRetry(ctx context.Context, jobID string) (bool, error) {
	events, err := w.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	needsRetry := false
	for _, event := range events {
		switch event.Kind {
		case "advance_started", "advance_retry":
			needsRetry = true
		case "advance_completed", "advance_retried", "advance_blocked", "advance_retry_skipped":
			needsRetry = false
		case "retry_queued":
			needsRetry = false
		}
	}
	return needsRetry, nil
}

// recordAdvanceRetryOnce appends an advance_retry marker UNLESS the job is already
// sitting on one. A terminal job whose post-delivery advancement keeps failing is
// re-attempted on every ~1s tick; appending a fresh advance_retry each time grew
// job_events without bound (a real install reached ~1.8M rows — 96% of the table —
// and the per-tick JobIDsWithPendingAdvanceRetry GROUP BY plus jobNeedsAdvanceRetry's
// per-job ListJobEvents pinned a core with zero jobs in flight). Only the latest
// marker per job is ever consulted (last-one-wins), so a job already on advance_retry
// stays a candidate and keeps retrying with no new row; any other latest marker
// (advance_started, or a prior terminal resolution before a re-trigger) still records
// the transition to advance_retry. jobNeedsAdvanceRetry and JobIDsWithPendingAdvanceRetry
// see an identical candidate set either way.
func (w jobWorker) recordAdvanceRetryOnce(ctx context.Context, jobID, message string) error {
	latest, err := w.Store.LatestAdvancementMarker(ctx, jobID)
	if err != nil {
		return err
	}
	if latest == "advance_retry" {
		// Keep the single-row bound but refresh the surviving row so the why-stuck
		// surface (#552) reports the current failure, not the first one.
		_, err := w.Store.RefreshLatestAdvanceRetry(ctx, jobID, message)
		return err
	}
	return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "advance_retry", Message: message})
}

func (w jobWorker) advanceJob(ctx context.Context, job db.Job) error {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_retry_skipped", Message: err.Error()})
	}
	dbAgent, err := w.Store.GetAgent(ctx, job.Agent)
	if err != nil {
		return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_retry_skipped", Message: err.Error()})
	}
	agent := runtimeAgent(dbAgent)
	if refreshed, ok, err := w.refreshImplementedPayloadForRetry(ctx, job, payload); err != nil {
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry refresh failed: "+err.Error())
	} else if ok {
		payload = refreshed
	}
	checkout, err := w.CheckoutValidator(ctx, job, payload, agent)
	if err != nil {
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry preflight failed: "+err.Error())
	}
	engine := w.WorkflowFactory(checkout)
	if err := engine.AdvanceJob(ctx, job.ID); err != nil {
		var awaiting workflow.AwaitingHumanError
		if errors.As(err, &awaiting) {
			return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_awaiting_human", Message: err.Error()})
		}
		var blocked workflow.BlockedError
		if errors.As(err, &blocked) {
			return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_blocked", Message: err.Error()})
		}
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry failed: "+err.Error())
	}
	writeLine(w.Stdout, "job %s advancement retried", job.ID)
	if err := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_retried", Message: "post-delivery workflow retry completed"}); err != nil {
		return err
	}
	return engine.ReconcileTerminalDrivingJob(ctx, job.ID)
}

func (w jobWorker) refreshImplementedPayloadForRetry(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, bool, error) {
	if job.Type != "implement" || payload.Result == nil || payload.Result.Decision != "implemented" {
		return payload, false, nil
	}
	checkout, err := w.resolveJobCheckout(ctx, job, payload)
	if err != nil {
		return workflow.JobPayload{}, false, err
	}
	payload, err = refreshDaemonJobPayload(ctx, w.Store, checkout, job, payload)
	if err != nil {
		return workflow.JobPayload{}, false, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return workflow.JobPayload{}, false, err
	}
	if err := w.Store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		return workflow.JobPayload{}, false, err
	}
	return payload, true, nil
}

func (w jobWorker) defaultAdapter(agent runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
	return buildRuntimeAdapter(w.ConfigHome, agent, checkout, nil)
}

func (w jobWorker) outputAdapter(agent runtime.Agent, checkout string, out io.Writer) (workflow.DeliveryAdapter, error) {
	return buildRuntimeAdapter(w.ConfigHome, agent, checkout, subprocess.TeeRunner{Inner: subprocess.GroupRunner{}, Out: runtimeOutputWriter(out)})
}

// buildSeatAwareAdapter builds the job's runtime adapter, injecting the #732 chat
// relay env for a `gitmoot moot` SEAT (payload.MootSeat) when a relay is running.
// For a seat that gets a working relay it mints a per-seat token bound to (agent,
// thread), returns an adapter whose runner appends GITMOOT_CHAT_RELAY[_AUTH] to the
// runtime subprocess env, and — only then — sets agent.ChatSeat so a codex seat is
// elevated to workspace-write+network to reach the socket. It takes *agent so this
// elevation propagates to the agent that RunJob delivers. Elevation is coupled to
// injection on PURPOSE: a seat is never granted the extra codex privilege without a
// working relay to use it (a no-relay / mint-failure seat stays unelevated and
// degrades to a job_result conclusion, exactly like a non-seat). For every non-seat
// job (or when no relay is running) it returns the byte-identical AdapterFactory
// adapter, no elevation, and an empty token.
//
// NOTE: moot seats are dispatched WITHOUT cockpit (runMoot sets no Cockpit flag),
// so the cockpit adapter-rebuild path in run() — which would replace this adapter
// and drop the env runner — never fires for a seat. If that ever changes, thread
// the relay env into the cockpit rebuild too.
func (w jobWorker) buildSeatAwareAdapter(agent *runtime.Agent, checkout string, payload workflow.JobPayload, output ...io.Writer) (workflow.DeliveryAdapter, string, error) {
	if w.RelayServer == nil || !payload.MootSeat || strings.TrimSpace(payload.ThreadID) == "" {
		if len(output) > 0 && output[0] != nil && w.OutputAdapterFactory != nil {
			adapter, err := w.OutputAdapterFactory(*agent, checkout, output[0])
			return adapter, "", err
		}
		adapter, err := w.AdapterFactory(*agent, checkout)
		return adapter, "", err
	}
	token, err := w.RelayServer.RegisterSeat(agent.Name, payload.ThreadID)
	if err != nil {
		// Fail-open: without a token the seat cannot relay, but a normal adapter
		// still lets the seat run (and degrade to a job_result conclusion). Leave the
		// agent UNELEVATED — elevation without a relay buys nothing and would leave a
		// codex seat with write+network it cannot use. Do not fail the job over a mint
		// error.
		writeLine(w.Stdout, "job seat %s relay token mint failed: %v", agent.Name, err)
		adapter, aerr := w.AdapterFactory(*agent, checkout)
		return adapter, "", aerr
	}
	relayEnv := []string{
		chatRelayEnvSocket + "=" + w.RelayServer.SocketPath(),
		chatRelayEnvToken + "=" + token,
	}
	// Elevate ONLY now that the seat will get a working relay env (see the coupling
	// rationale above). Mutates the caller's agent so RunJob delivers with ChatSeat.
	agent.ChatSeat = true
	adapter, err := buildRuntimeAdapter(w.ConfigHome, *agent, checkout, subprocess.EnvInjectingRunner{Env: relayEnv})
	if err != nil {
		agent.ChatSeat = false
		w.RelayServer.ReleaseSeat(token)
		return nil, "", err
	}
	return adapter, token, nil
}

// buildRuntimeAdapter constructs the concrete runtime adapter for a job. With
// credential curation off, a nil runner remains nil and the adapter falls through
// to GroupRunner exactly as before. With curation on, runtimeJobRunner installs
// the curated process-group base beneath any tee, relay, or Landlock wrapper. The
// wrappers still append their environment last and preserve result capture,
// cancellation, and live output.
func buildRuntimeAdapter(home string, agent runtime.Agent, checkout string, runner subprocess.Runner) (workflow.DeliveryAdapter, error) {
	var err error
	runner, err = runtimeJobRunner(home, agent.Runtime, runner)
	if err != nil {
		return nil, err
	}
	gatewayRunner, _ := runner.(*credgw.Runner)
	if (len(agent.WritablePaths) > 0 || len(agent.ReadablePaths) > 0 || len(agent.ReadableFiles) > 0) && (agent.Runtime == runtime.ClaudeRuntime || agent.Runtime == runtime.KimiRuntime) {
		reads, readFiles, writes, env, err := produceRuntimeSandboxGrants(agent.Runtime, agent.ReadablePaths, agent.ReadableFiles, agent.WritablePaths)
		if err != nil {
			return nil, err
		}
		runner = landlockProduceRunner(runner, reads, readFiles, writes, env)
	}
	var adapter runtime.Adapter
	switch agent.Runtime {
	case runtime.CodexRuntime:
		adapter = runtime.CodexAdapter{Dir: checkout, Runner: runner}
	case runtime.ClaudeRuntime:
		adapter = runtime.ClaudeAdapter{Dir: checkout, Runner: runner}
	case runtime.KimiRuntime:
		adapter = runtime.KimiAdapter{Dir: checkout, Runner: runner}
	case runtime.KimiCLIRuntime:
		adapter = runtime.KimiCLIAdapter{Dir: checkout, Runner: runner}
	case runtime.ShellRuntime:
		adapter = runtime.ShellAdapter{Dir: checkout, Runner: runner}
	default:
		return nil, fmt.Errorf("unsupported runtime: %s", agent.Runtime)
	}
	return wrapModelGatewayAdapter(adapter, gatewayRunner), nil
}

func (w jobWorker) defaultStartAdapter(runtimeName string, checkout string) (runtime.Adapter, error) {
	return runtimeAdapterFor(w.ConfigHome, runtimeName, checkout)
}

func (w jobWorker) defaultWorkflow(checkout string) workflow.Engine {
	engine := daemonWorkflowEngine(w.Store, github.NewClient(checkout), checkout, w.workflowHome())
	w.applyOrchestratePolicy(&engine)
	return engine
}

// applyOrchestratePolicy sets the engine's opt-in [orchestrate] fields — the
// artifact-body inlining knobs, the upstream-dep-context injection toggle (#419),
// the per-root delegation token (#338 Part B) and dollar-cost (#380) budgets, the
// result-aware non-progress streak threshold (#339), and the verify→replan
// attempt cap (#439) — from the host policy. It is fail-safe: any load error
// leaves the engine with its defaults (inlining off, upstream-dep injection off,
// both budgets 0 = unlimited, streak threshold and verify cap 0 = engine default)
// rather than failing engine construction.
func (w jobWorker) applyOrchestratePolicy(engine *workflow.Engine) {
	policy, err := w.orchestratePolicy()
	if err != nil {
		return
	}
	engine.InlineArtifactBodies = policy.InlineArtifactBodies
	engine.MaxInlineArtifactBytes = policy.InlineArtifactMaxBytes
	engine.InjectUpstreamDepContext = policy.InjectUpstreamDepContext
	engine.MaxDelegationTokenBudget = policy.MaxDelegationTokenBudget
	engine.MaxDelegationCostUSD = policy.MaxDelegationCostUSD
	engine.MaxDelegationNonProgressStreak = policy.MaxDelegationNonProgressStreak
	engine.MaxVerifyReplanAttempts = policy.MaxVerifyReplanAttempts
	engine.DelegationTimeoutDefaults = workflow.DelegationTimeoutDefaults{
		Default:   policy.DefaultDelegationTimeout,
		Plan:      policy.DefaultPlanTimeout,
		Implement: policy.DefaultImplementTimeout,
		Review:    policy.DefaultReviewTimeout,
		Gate:      policy.DefaultGateTimeout,
		Repair:    policy.DefaultRepairTimeout,
	}
	if notifier, ok := engine.EscalationNotifier.(*daemonEscalationNotifier); ok && notifier != nil {
		notifier.Handle = policy.EscalationHandle
	}
}

// workflowHome resolves the GITMOOT_HOME root used to place per-delegation
// worktrees, mirroring how the daemon resolves paths elsewhere. It returns an
// empty string when resolution fails so the engine falls back to legacy
// shared-checkout dispatch rather than failing the job.
func (w jobWorker) workflowHome() string {
	paths, err := pathsFromFlag(w.ConfigHome)
	if err != nil {
		return ""
	}
	return paths.Home
}

func (w jobWorker) defaultCommenter(_ string) github.Client {
	return github.NewClient("")
}
