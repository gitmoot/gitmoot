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
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/permissionpolicy"
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
	AgentLookup        func(context.Context, string) (db.Agent, error)
	AdapterFactory     func(runtime.Agent, string) (workflow.DeliveryAdapter, error)
	// OutputAdapterFactory rebuilds a production runtime adapter around the one
	// shared live-output writer used by pipeline progress and cockpit. Tests that
	// inject an opaque fake AdapterFactory may leave this nil and still exercise
	// elapsed-only progress without replacing their fake.
	OutputAdapterFactory func(runtime.Agent, string, io.Writer) (workflow.DeliveryAdapter, error)
	StartAdapterFactory  func(execbackend.Backend, string, string) (runtime.Adapter, error)
	// ExecutionBackendFactory is nil on hand-built/test workers that intentionally
	// retain the pre-P2b host-only path. executionBackendJobWorker wires it for
	// real daemon jobs, where one lifecycle instance is acquired after runtime
	// admission and kept alive through every Mailbox delivery attempt. The factory
	// must consume the same config snapshot used for preflight.
	ExecutionBackendFactory func(execbackend.Backend, config.RemoteExecConfig) (execbackend.ExecutionBackend, error)
	// GITMOOT-IMPL: RemoteEnvdEndpointResolver is an offline-test seam. Production leaves it
	// nil and resolves envd from [remote_exec].e2b_domain or the provider response.
	RemoteEnvdEndpointResolver func(sandboxID string, port int) string
	// executionRunner is run-scoped state on jobWorker's value receiver. Cockpit
	// adapter rebuilds reuse it so enabling live logs cannot escape the backend.
	executionRunner subprocess.Runner
	// CheckoutValidator and WorkflowFactory are test overrides. Production leaves
	// them nil so checkoutForJob/workflowForJob must consume the resolved runner.
	CheckoutValidator func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error)
	WorkflowFactory   func(string) workflow.Engine
	// ReclaimJobLookup is a test seam for candidate-local GetJob failures. The
	// production path is always Store.GetJob.
	ReclaimJobLookup func(context.Context, string) (db.Job, error)
	// beforeDeliveryDiagnosticsWrite fires inside recordDeliveryFailureDiagnostics
	// between the lifecycle read and the generation-conditioned payload write —
	// i.e. inside the window the compare-and-swap exists to close. It is the only
	// way to drive that window deterministically, since the two operations are
	// otherwise adjacent. Production leaves it nil.
	beforeDeliveryDiagnosticsWrite func()
	CommenterFactory               func(string) github.Client
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
	// QuotaWake is the existing Herdr agent-prompt client used for the direct,
	// one-shot parent-role escalation when a Claude job makes its acting org role
	// unavailable. nil keeps non-daemon/test workers wake-free.
	QuotaWake eventWakeClient
	// SandboxProbe is the cached host capability check used only for Claude/Kimi
	// produce stages. nil selects sandbox.SandboxProbe; tests inject deterministic
	// supported/unsupported results without depending on the test binary's argv.
	SandboxProbe func() sandbox.ProbeResult
	// RuntimePreflight evaluates the installed CLI contract immediately before
	// checkout/adapter construction. nil keeps hand-built test workers unchanged;
	// defaultJobWorker wires the process-wide identity cache.
	RuntimePreflight func(context.Context, runtime.Agent, runtime.RuntimeContractRequest) runtime.RuntimeContractResult
	// Progress timing seams keep unit/E2E tests deterministic and short. Zero/nil
	// values select the package defaults and real timer implementation.
	PipelineProgressThreshold time.Duration
	PipelineProgressInterval  time.Duration
	ProgressTickSource        func(context.Context, time.Duration, time.Duration) <-chan time.Time
	// PermissionPolicyEffectGit injects the existing git observation primitives.
	// It is consulted only for a claimed not-applied observation; nil selects the
	// checkout-bound production client.
	PermissionPolicyEffectGit func(string) permissionpolicy.EffectGit
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

// replyWakeDelivery resolves one authoritative, batch-scoped rules snapshot and
// the sink built from it. The outbox invokes this immediately before each claim
// and passes the same snapshot through delivery, so authorization cannot outlive
// its batch and routing cannot become unreadable after claim.
func (w jobWorker) replyWakeDelivery(ctx context.Context) (replyWakeDelivery, error) {
	if w.Store == nil {
		return replyWakeDelivery{}, errors.New("wake outbox store is required")
	}
	rules, err := w.Store.ListEventRules(ctx)
	if err != nil {
		return replyWakeDelivery{}, fmt.Errorf("list event rules: %w", err)
	}
	if w.EventSinkOverride != nil {
		return replyWakeDelivery{sink: w.EventSinkOverride, rules: rules}, nil
	}
	return replyWakeDelivery{
		sink:  resolveDaemonEventSinkWithRules(w.Store, w.workflowHome(), rules),
		rules: rules,
	}, nil
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
	worker := jobWorker{Store: store, Stdout: serializeWrites(stdout), ConfigHome: configHome, ConfigHomeExplicit: configHomeExplicit}
	worker.AdapterFactory = worker.defaultAdapter
	worker.OutputAdapterFactory = worker.outputAdapter
	worker.StartAdapterFactory = worker.defaultStartAdapter
	worker.AuthProbe = worker.defaultAuthProbe
	worker.RuntimePreflight = runtime.DefaultRuntimeContractChecker().CheckRequest
	worker.QuotaWake = newQuotaRoleUnavailableWakeClient()
	recoverKillPendingAtWorkerStartup.Do(func() {
		if err := recoverKillPendingJobs(context.Background(), store, worker.Stdout); err != nil {
			writeLine(worker.Stdout, "job kill-pending recovery failed: %v", err)
		}
	})
	return worker
}

// executionBackendJobWorker is the production daemon constructor. Keeping the
// lifecycle opt-in at this boundary preserves defaultJobWorker as the existing
// unit-test/foreground seam: a hand-built worker has no backend instance and
// therefore can never accidentally receive a live ChangeSet importer.
func executionBackendJobWorker(store *db.Store, stdout io.Writer, home string) jobWorker {
	worker := defaultJobWorker(store, stdout, home)
	worker.ExecutionBackendFactory = worker.defaultExecutionBackend
	// GITMOOT-IMPL: always reconcile local crash leftovers, including the bounded
	// set stranded by a switch to remote, then reconcile the configured provider.
	// The attempts are independent so a local disk failure cannot suppress remote
	// account reconciliation (or vice versa).
	cfg, err := worker.executionBackendConfig()
	if err != nil {
		writeLine(stdout, "execution backend startup config failed: %v", err)
		return worker
	}
	startupBackends := []execbackend.Backend{execbackend.Local}
	if configured, err := execbackend.ParseImplemented(cfg.Backend); err != nil {
		writeLine(stdout, "execution backend startup config failed: %v", err)
		return worker
	} else if configured != execbackend.Local {
		startupBackends = append(startupBackends, configured)
	}
	for _, backend := range startupBackends {
		if _, err := worker.ExecutionBackendFactory(backend, cfg); err != nil {
			writeLine(stdout, "execution backend startup reap failed for %s: %v", backend, err)
		}
	}
	return worker
}

var recoverKillPendingAtWorkerStartup sync.Once

func (w jobWorker) run(ctx context.Context, job db.Job) error {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return w.finishQueuedJob(ctx, job, workflow.JobFailed, err)
	}
	// Resolve WHERE the runtime executes before any path can construct or start
	// an adapter. In particular, ephemeral jobs materialize and start a host
	// runtime below, so delaying this decision until after agent lookup would let
	// them bypass the backend boundary entirely.
	jobExecBackend, jobExecBackendPresent := payload.ExecBackendOverride()
	execBackend, execConfig, err := daemonJobExecBackendFor(w, jobExecBackend, jobExecBackendPresent)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
		return nil
	}
	if execBackend != execbackend.Local && job.Type != "implement" {
		err := fmt.Errorf("%s jobs are not supported on the %s execution backend; only implement jobs transport changes back to the host", job.Type, execBackend)
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
		return nil
	}
	jobRunner, err := jobSubprocessRunnerForBackend(execBackend)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
		return nil
	}
	// Review-to-fix jobs are the implementation jobs created by workflow
	// advancement. They are explicitly marked FixWorktree; ordinary task runs,
	// local implement dispatches, and delegation legs may legitimately be PR-less
	// and must not inherit this delivery gate. Check the same durable target the
	// finalizer will use before agent lookup, checkout setup, or adapter delivery.
	if job.Type == "implement" && payload.FixWorktree {
		if _, err := implementationFinalizationTargetForRunner(ctx, w.Store, job, payload, implementationFinalizationBeforeRun, jobRunner); err != nil {
			err = fmt.Errorf("validate implementation target before model run: %w", err)
			if !resultDeliveryFailed(err) {
				return w.retryImplementationPreflight(ctx, job, payload, err)
			}
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobBlocked, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
			return nil
		}
	}
	// An ephemeral child carries an inline worker spec instead of a
	// pre-registered agent. Materialize a throwaway agent + runtime session
	// from the spec before the normal flow runs (which assumes the agent
	// already exists via GetAgent below), and register a cleanup defer so the
	// worker is auto-disposed on every exit path — success, failure, or block.
	if payload.Ephemeral != nil {
		if err := w.startEphemeralWorker(ctx, job, payload, execBackend, jobRunner); err != nil {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "ephemeral_worker_failed", Message: err.Error()}); eventErr != nil {
				return eventErr
			}
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
			return nil
		}
		// Idempotent removal of the agent row + instance regardless of how run
		// returns; uses a background context so cleanup survives ctx cancel.
		defer w.cleanupTempWorker(context.Background(), job.Agent)
	}
	dbAgent, err := w.lookupAgent(ctx, job.Agent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			warned, warningErr := permissionpolicy.RecordWarning(ctx, w.Store, job, runtime.Agent{Name: job.Agent}, permissionpolicy.StaticProvider{Property: runtime.PermissionPolicyUnresolved}, time.Now())
			if warningErr != nil {
				writeLine(w.Stdout, "job %s permission-policy observation failed: %v", job.ID, warningErr)
			}
			if warned {
				defer w.capturePermissionPolicyEffects(context.Background(), job.ID, strings.TrimSpace(payload.WorktreePath))
			}
		} else {
			writeLine(w.Stdout, "job %s permission-policy observation skipped: agent lookup failed: %v", job.ID, err)
		}
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
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
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
			return nil
		}
	}
	if !overridden {
		agent = scopeRegisteredFreshRefForJob(agent, job.ID)
	}
	// Stamp the already-resolved decision on the in-memory job agent so every
	// secondary adapter rebuild consumes the same backend selection.
	agent.ExecBackend = string(execBackend)
	readOnlySeat := payload.ReadOnlySeat
	runtimeConfigDir := strings.TrimSpace(payload.RuntimeConfigDir)
	if readOnlySeat && runtimeConfigDir == "" {
		runtimeConfigDir = selectedRuntimeConfigDir(agent.Runtime)
	}
	applyReadOnlySeat(readOnlySeat, runtimeConfigDir, &agent)
	// A seat stages a credential SNAPSHOT, so check the expiry it is about to
	// copy BEFORE launching a runtime that can only fail on it. Provably
	// unusable (expired, no refresh token) fails here; expired-but-refreshable
	// records the fact so a later auth failure is not read as a fresh OAuth
	// problem. See readonly_seat_credential.go for the measurement.
	if credentialDiagnosis, credentialErr := readOnlySeatCredentialPreflight(agent, runtimeConfigDir, time.Now().UTC()); credentialErr != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, credentialErr); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, "", credentialErr)
		return nil
	} else if credentialDiagnosis != "" {
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "readonly_seat_credential_expired", Message: credentialDiagnosis}); eventErr != nil {
			writeLine(w.Stdout, "job %s readonly_seat_credential_expired event failed: %v", job.ID, eventErr)
		}
	}
	preflightRequest := runtime.RuntimeContractRequest{Plan: payload.Plan}
	if result, checked, preflightErr := w.runtimeContractPreflight(ctx, execBackend, execConfig, agent, preflightRequest); preflightErr != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, preflightErr); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, "", preflightErr)
		return nil
	} else if checked {
		if err := runtime.RuntimeContractDispatchError(agent, result); err != nil {
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobBlocked, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
			return nil
		}
		if result.State == runtime.RuntimeContractUnknown {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_contract_unknown", Message: runtime.RuntimeContractEventMessage(job.ID, agent, result)}); eventErr != nil {
				writeLine(w.Stdout, "job %s runtime_contract_unknown event failed: %v", job.ID, eventErr)
			}
		}
	}
	if err := w.produceDispatchError(job.Type, agent); err != nil {
		w.recordProduceSandboxDiagnostic(ctx, job.ID, job.Type, agent)
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobBlocked, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
		return nil
	}
	if readOnlyImplementationBlocked(job.Type, agent) {
		transitioned, err := markJobPermissionBlocked(ctx, w.Store, job)
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
		emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, job.ID, daemonTerminalPermissionGuard, string(workflow.JobBlocked), agentPermissionBlockedMessage)
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
	nativeReviewDeliveryStarted := false
	payload, err = w.prepareNativeReviewWorktreeForRunner(ctx, job, payload, jobRunner)
	if err != nil {
		// An exact-head allocation that spent its checkout-mutation-lock budget is
		// TRANSIENT: the holder is another worker's short shared-.git op. Terminally
		// failing the leg here BURNED the verdict — the payload is left unmutated, so
		// the next poll's re-enqueue matches it and is a silent no-op, and
		// FindRepeatedReviewers only re-enlists SUCCEEDED verdicts, so nothing ever
		// re-attempts it. Hold the still-queued leg for re-dispatch instead, exactly
		// like the checkout-preflight site below. Every other allocation failure (a
		// missing commit object, an unwritable path) is unclassified and keeps the
		// terminal path, and the hold itself is bounded by maxOperationalBlockerRetries.
		//
		// This site uses deferPreDeliveryAllocationContention rather than the general
		// helper because a high-risk LENS CHILD reaches this same path-less fallback
		// (TestNativeReviewWorktreePreparationCoversHighRiskLensChild), and the general
		// helper's delegation-child exclusion would route it to
		// finishQueuedJob(JobFailed) → finalizePreflightDelegationChild, advancing the
		// delegation DAG with a synthetic `failed` verdict on a lock another worker
		// holds for a sub-second op. The narrowing is typed and pre-delivery only; see
		// the helper's own comment for why that is the safe boundary.
		if deferred, deferErr := w.deferPreDeliveryAllocationContention(ctx, job, payload, err); deferErr != nil {
			writeLine(w.Stdout, "job %s review-worktree contention deferral failed: %v", job.ID, deferErr)
		} else if deferred {
			writeLine(w.Stdout, "job %s deferred on review-worktree contention: %v", job.ID, err)
			return nil
		}
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
		return nil
	}
	nativeReviewWorktreeOwned := payload.ReadOnlyWorktree && strings.TrimSpace(payload.WorktreePath) != ""
	if nativeReviewWorktreeOwned {
		defer func() {
			if nativeReviewDeliveryStarted {
				return
			}
			w.cleanupUndeliveredNativeReviewWorktree(context.WithoutCancel(ctx), job.ID, payload, jobRunner)
		}()
	}
	checkout, err := w.checkoutForJob(ctx, job, payload, agent, jobRunner)
	if err != nil {
		if resumedCheckout, resumedPayload, ok := w.resumeSelfDirtyWorktreeForRunner(ctx, job, payload, agent, err, jobRunner); ok {
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
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
			return nil
		}
	}
	deliveryCheckout := checkout
	var progressTracker *pipelineProgressLineTracker
	if payload.Sender == workflow.PipelineJobSender {
		progressTracker = &pipelineProgressLineTracker{}
	}
	var adapter workflow.DeliveryAdapter
	if progressTracker != nil {
		adapter, err = w.buildJobAdapterForBackend(execBackend, agent, checkout, progressTracker)
	} else {
		adapter, err = w.buildJobAdapterForBackend(execBackend, agent, checkout)
	}
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	warningRecorded, warningErr := permissionpolicy.RecordWarning(ctx, w.Store, job, agent, adapter, time.Now())
	if warningErr != nil {
		writeLine(w.Stdout, "job %s permission-policy observation failed: %v", job.ID, warningErr)
	}
	if warningRecorded {
		defer w.capturePermissionPolicyEffects(context.Background(), job.ID, checkout)
	}
	managed, err := w.managedJobConfig(ctx, agent.Name)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	timeoutResolution := resolveEffectiveJobTimeout(payload, managed)
	jobTimeout := timeoutResolution.Timeout
	if timeoutResolution.Clamped {
		message := fmt.Sprintf("%s job_timeout %s exceeds [daemon].job_timeout_max %s; clamped to %s",
			timeoutResolution.Source, timeoutResolution.Requested, timeoutResolution.Max, timeoutResolution.Timeout)
		if eventErr := w.Store.AddJobEventIfAbsent(ctx, db.JobEvent{JobID: job.ID, Kind: "job_timeout_clamped", Message: message}); eventErr != nil {
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, eventErr); finishErr != nil {
				return finishErr
			}
			return nil
		}
	}
	// Size the runtime-session lease to jobTimeout PLUS a teardown grace so the
	// lease strictly OUTLIVES the run-context deadline (armed at exactly jobTimeout
	// below) and the terminal worktree teardown that runs while this lock is still
	// held. A normally-terminating worker therefore releases its lease before it
	// expires; without the grace the lease would expire in the live-worker teardown
	// window and the expired-lock reaper would requeue the still-'running' owner
	// onto its dirty worktree — the #536 clobber. See runtimeLeaseTeardownGrace.
	lockTTL := jobTimeout + runtimeLeaseTeardownGrace
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
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	if !acquired {
		message := fmt.Sprintf("runtime session %s is busy", lockKey)
		policy, policyErr := w.parallelSessionPolicy()
		if policyErr != nil {
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, policyErr); finishErr != nil {
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
			return w.runWithTempWorker(ctx, job, payload, execBackend, agent, checkout, policy, eligibleMessage, warningRecorded)
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
	if persistErr := persistJobEffectiveRuntime(ctx, w.Store, job.ID, agent.Runtime); persistErr != nil {
		err := fmt.Errorf("persist effective runtime before execution: %w", persistErr)
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	// Expose the effective runtime (and the session lock it runs under) in job
	// history for every job. Only an actual override uses runtime_override;
	// default selection is recorded as effective_runtime.
	if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: jobRuntimeEventKind(overridden), Message: jobRuntimeOverrideEventMessage(defaultRuntime, agent, lockKey)}); eventErr != nil {
		writeLine(w.Stdout, "job %s effective-runtime event failed: %v", job.ID, eventErr)
	}
	// This is the last filesystem authorization check before adapter delivery.
	// It runs after runtime-session admission so a symlink retargeted while the job
	// waited cannot inherit stale grants. The adapter is then rebuilt in-place with
	// sandbox-exec as the innermost runner for Claude/Kimi produce only.
	if err := applyProduceRuntimeGrants(ctx, w.Store, w.ConfigHome, job, payload, &agent); err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	// Acquire the execution-backend lifecycle only after checkout validation and
	// runtime-session admission. The instance then survives every Mailbox repair
	// delivery and is destroyed synchronously on every return path. Host checkout,
	// git, observation, and finalization remain on checkout/jobRunner; only runtime
	// delivery executes in the distinct backend workspace.
	lifecycle, instance, credentialLease, credentialEnv, lifecycleErr := w.provisionExecutionBackend(ctx, execBackend, execConfig, agent.Runtime, job, jobTimeout+runtimeLeaseTeardownGrace, checkout)
	if instance != nil {
		defer w.destroyExecutionBackend(job.ID, lifecycle, instance)
	}
	if credentialLease != nil {
		defer credentialLease.Revoke()
	}
	if lifecycleErr != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, lifecycleErr); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, lifecycleErr)
		return nil
	}
	if lifecycle != nil && instance != nil {
		deliveryCheckout = instance.Workspace
		w.executionRunner = execbackend.InstanceRunner{Backend: lifecycle, Instance: instance}
		if len(credentialEnv) > 0 {
			w.executionRunner = subprocess.EnvInjectingRunner{Inner: w.executionRunner, Env: credentialEnv}
		}
		if progressTracker != nil {
			adapter, err = w.executionDeliveryAdapter(agent, deliveryCheckout, progressTracker)
		} else {
			adapter, err = w.executionDeliveryAdapter(agent, deliveryCheckout)
		}
		if err != nil {
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
			return nil
		}
	}
	// Cache grants are keyed to the checkout the runtime actually receives. A
	// local execution backend may replace the source worktree with an instance
	// workspace, so constructing this grant before provisioning creates two
	// different hard-seat cache roots.
	var toolCacheEnv []string
	var toolCacheErr error
	if execBackend == execbackend.Local {
		if cachePaths, cacheErr := w.configPaths(); cacheErr != nil {
			toolCacheErr = cacheErr
		} else {
			cachePayload := payload
			cachePayload.WorktreePath = deliveryCheckout
			toolCacheEnv, toolCacheErr = applyIsolatedToolCacheGrants(cachePaths, cachePayload, &agent)
		}
	}
	if toolCacheErr != nil {
		if agent.ReadOnlySeat {
			err := fmt.Errorf("prepare read-only tool cache: %w", toolCacheErr)
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
			return nil
		}
		writeLine(w.Stdout, "job %s tool cache grant failed: %v", job.ID, toolCacheErr)
	}
	adapter, err = wrapProduceSandboxAdapter(job.Type, agent, adapter)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	adapter, err = wrapReadOnlySandboxAdapter(w.ConfigHome, agent, deliveryCheckout, adapter)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	var readOnlyState *readOnlyRuntimeAdapter
	readOnlyStateCleaned := false
	if stateAdapter, ok := adapter.(readOnlyRuntimeAdapter); ok {
		readOnlyState = &stateAdapter
		defer func() {
			if !readOnlyStateCleaned {
				if cleanupErr := readOnlyState.cleanup(); cleanupErr != nil {
					writeLine(w.Stdout, "job %s read-only runtime state cleanup failed: %v", job.ID, cleanupErr)
				}
			}
		}()
	}
	if execBackend == execbackend.Local && len(toolCacheEnv) > 0 {
		if envAdapter, envErr := injectDeliveryAdapterEnv(adapter, toolCacheEnv); envErr != nil {
			writeLine(w.Stdout, "job %s tool cache env inject failed: %v", job.ID, envErr)
		} else {
			adapter = envAdapter
		}
	}
	// Default-on retained capture is attached to the already-composed adapter so
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
	cockpitRequested := payload.Cockpit
	if cockpitRequested && execBackend != execbackend.Local {
		cockpitRequested = false
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "cockpit_unavailable",
			Message: fmt.Sprintf("cockpit requested but the %s execution backend has no host worktree pane; running without a pane", execBackend),
		}); eventErr != nil {
			writeLine(w.Stdout, "job %s cockpit_unavailable event failed: %v", job.ID, eventErr)
		}
	}
	if cockpitRequested {
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
		meta := cockpitJobMeta(job, payload, agent, deliveryCheckout, policy.CockpitPaneKey)
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
					teeAdapter, logPath, logFile = w.cockpitLogAdapter(cp, agent, deliveryCheckout, job.ID, meta.RootJobID, meta.PaneKey, seatMode, progressTracker)
				} else {
					teeAdapter, logPath, logFile = w.cockpitLogAdapter(cp, agent, deliveryCheckout, job.ID, meta.RootJobID, meta.PaneKey, seatMode)
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
	if managed.Instance {
		if err := w.Store.MarkAgentInstanceRunning(ctx, agent.Name, time.Now().UTC(), jobTimeout); err != nil {
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
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
	adapter = wrapManagedWorktreeRuntimeEnv(payload, adapter)
	if warningRecorded {
		adapter = w.observePermissionPolicyEffects(adapter, job.ID, deliveryCheckout)
	}
	engine := w.workflowForJob(checkout, jobRunner)
	engine.CollectChangeSet = executionChangeSetCollector(lifecycle, instance, execBackend, job.ID)
	// Wire the PRE-TERMINAL operational-blocker deferrer (#532 slice E) on the LIVE
	// worker (not the WorkflowFactory-captured copy) so it observes this worker's
	// EventSink for the first-class job.deferred emit. When a delivery-seam failure
	// classifies as a retryable operational blocker the mailbox re-queues the job
	// BEFORE the terminal transition, so no job.failed reaches the [events] sink.
	engine.BlockerDeferrer = w.deferOperationalBlockerPreTerminal
	runCtx, stopRun := w.runningJobContext(ctx, job.ID)
	defer stopRun()
	runStartedAt := time.Now().UTC()
	var cancel context.CancelFunc
	runCtx, cancel = context.WithTimeout(runCtx, jobTimeout)
	defer cancel()
	runDeadline, hasRunDeadline := runCtx.Deadline()
	stopKillPending := func() {}
	if hasRunDeadline {
		stopKillPending = armJobKillPending(w.Store, job.ID, runDeadline)
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
	nativeReviewDeliveryStarted = true
	_, err = engine.RunJob(runCtx, job.ID, agent, adapter)
	stopKillPending()
	stopProgress()
	if readOnlyState != nil {
		err = errors.Join(err, readOnlyState.cleanup())
		readOnlyStateCleaned = true
	}
	if err != nil {
		if quotaErr := w.quotaRoleUnavailableHooks().recordRuntimeOutcome(ctx, job, payload, agent, err, time.Now().UTC()); quotaErr != nil {
			writeLine(w.Stdout, "job %s org-role quota unavailability capture failed: %v", job.ID, quotaErr)
		}
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
		var markErr error
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			markErr = w.handleRunJobError(ctx, job.ID, observedJobLifecycle(job), context.DeadlineExceeded, jobTimeoutEvidence{Deadline: runDeadline, Started: runStartedAt})
		} else {
			markErr = w.handleRunJobError(ctx, job.ID, observedJobLifecycle(job), err)
		}
		if markErr != nil {
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
		// Record the SAME err the journal line below prints, so the job row can
		// distinguish faults the row otherwise collapses into one exit_code: 1
		// (#1620). commentErr is deliberately not used here: it can be swapped for
		// the permission-blocked operator message, and this field means "what the
		// engine actually failed with".
		w.recordDeliveryFailureDiagnostics(ctx, job.ID, job.LifecycleGeneration, err)
		writeLine(w.Stdout, "job %s failed: %v", job.ID, err)
		return nil
	}
	if clearErr := w.quotaRoleUnavailableHooks().recordRuntimeOutcome(ctx, job, payload, agent, nil, time.Now().UTC()); clearErr != nil {
		writeLine(w.Stdout, "job %s org-role quota unavailability clear failed: %v", job.ID, clearErr)
	}
	if err := engine.ReconcileTerminalDrivingJob(ctx, job.ID); err != nil {
		return err
	}
	_ = w.postJobResultComment(ctx, job.ID, agent, checkout, nil)
	writeLine(w.Stdout, "job %s completed", job.ID)
	return nil
}

func (w jobWorker) cleanupUndeliveredNativeReviewWorktree(ctx context.Context, jobID string, payload workflow.JobPayload, runner subprocess.Runner) {
	job, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		writeLine(w.Stdout, "job %s native review pre-delivery cleanup lookup failed: %v", jobID, err)
		return
	}
	switch workflow.JobState(job.State) {
	case workflow.JobFailed, workflow.JobCancelled:
	default:
		return
	}
	engine := w.workflowForJob(strings.TrimSpace(payload.WorktreePath), runner)
	if err := engine.ReclaimTerminalDelegationWorktree(ctx, jobID); err != nil {
		writeLine(w.Stdout, "job %s native review pre-delivery cleanup failed: %v", jobID, err)
	}
}

const implementationPreflightAttemptLimit = 3
const implementationPreflightRetryDelay = time.Second

func (w jobWorker) retryImplementationPreflight(ctx context.Context, job db.Job, payload workflow.JobPayload, cause error) error {
	const class = "implementation_preflight"
	alreadyExhausted := payload.BlockerClass == class && payload.BlockerAttempts >= implementationPreflightAttemptLimit
	attempt := payload.BlockerAttempts + 1
	if attempt > implementationPreflightAttemptLimit {
		attempt = implementationPreflightAttemptLimit
	}
	payload.BlockerClass = class
	payload.BlockerAttempts = attempt
	payload.BlockerPreDelivery = true
	payload.BlockerRetryAt = ""
	payload.BlockerSuggestedAction = ""
	if attempt < implementationPreflightAttemptLimit {
		payload.BlockerRetryAt = time.Now().UTC().Add(implementationPreflightRetryDelay).Format(time.RFC3339Nano)
	}
	encoded, err := payloadWithImplementationPreflightRetry(job.Payload, payload)
	if err != nil {
		return err
	}
	message := fmt.Sprintf("implementation preflight attempt %d/%d failed: %v", attempt, implementationPreflightAttemptLimit, cause)
	if attempt < implementationPreflightAttemptLimit {
		transitioned, err := w.Store.TransitionJobStatePayloadWithEventAtGeneration(ctx, job.ID, string(workflow.JobQueued), job.LifecycleGeneration, string(workflow.JobQueued), encoded, db.JobEvent{JobID: job.ID, Kind: blockerDeferredEventKind, Message: message})
		if err != nil || !transitioned {
			return err
		}
		emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, job.ID, daemonTerminalDeferred, string(workflow.JobQueued), message)
		return nil
	}
	exhausted := fmt.Errorf("automatic implementation preflight retries exhausted after %d attempts for task %s in worktree %q: %w; inspect and repair the worktree, then run `gitmoot job retry %s`; if the dispatch metadata is stale, dispatch a fresh fix job against pull request #%d's current head", implementationPreflightAttemptLimit, payload.TaskID, payload.WorktreePath, cause, job.ID, payload.PullRequest)
	firstEvent := db.JobEvent{JobID: job.ID, Kind: blockerExhaustedEventKind, Message: exhausted.Error()}
	additionalEvents := []db.JobEvent{
		{JobID: job.ID, Kind: string(workflow.JobFailed), Message: exhausted.Error()},
	}
	if alreadyExhausted {
		firstEvent = additionalEvents[0]
		additionalEvents = nil
	}
	transitioned, err := w.Store.TransitionJobStatePayloadWithEventAtGeneration(ctx, job.ID, string(workflow.JobQueued), job.LifecycleGeneration, string(workflow.JobFailed), encoded, firstEvent, additionalEvents...)
	if err != nil {
		return err
	}
	if !transitioned {
		return nil
	}
	if err := w.afterQueuedJobTransition(ctx, job.ID, workflow.JobFailed, exhausted, true); err != nil {
		return err
	}
	_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, strings.TrimSpace(payload.WorktreePath), exhausted)
	return nil
}

// payloadWithImplementationPreflightRetry updates only the retry fields this
// preflight owns; the stored payload may contain newer or legacy evidence.
func payloadWithImplementationPreflightRetry(raw string, payload workflow.JobPayload) (string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return "", err
	}
	owned := map[string]any{
		"blocker_class":        payload.BlockerClass,
		"blocker_attempts":     payload.BlockerAttempts,
		"blocker_pre_delivery": payload.BlockerPreDelivery,
	}
	for key, value := range owned {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		envelope[key] = encoded
	}
	if payload.BlockerRetryAt == "" {
		delete(envelope, "blocker_retry_at")
	} else {
		encoded, err := json.Marshal(payload.BlockerRetryAt)
		if err != nil {
			return "", err
		}
		envelope["blocker_retry_at"] = encoded
	}
	delete(envelope, "blocker_suggested_action")
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func runtimeContractPreflightForBackend(backend execbackend.Backend, local func() runtime.RuntimeContractResult) (runtime.RuntimeContractResult, bool, error) {
	type result struct {
		contract runtime.RuntimeContractResult
		checked  bool
	}
	consumed, err := execbackend.Consume(backend, func() (result, error) {
		return result{contract: local(), checked: true}, nil
	}, func() (result, error) {
		// This preflight runs before provisioning, so a host probe would answer
		// about the wrong machine. The attached instance is checked at delivery.
		return result{}, nil
	})
	return consumed.contract, consumed.checked, err
}

func (w jobWorker) runtimeContractPreflight(ctx context.Context, backend execbackend.Backend, cfg config.RemoteExecConfig, agent runtime.Agent, request runtime.RuntimeContractRequest) (runtime.RuntimeContractResult, bool, error) {
	if w.RuntimePreflight == nil {
		return runtime.RuntimeContractResult{}, false, nil
	}
	if backend == execbackend.Local && w.ExecutionBackendFactory != nil {
		if identity := cfg.LocalIdentity(); identity != nil {
			request.EffectiveUID = int(identity.UID)
			request.EffectiveUIDKnown = true
		}
	}
	result, checked, err := runtimeContractPreflightForBackend(backend, func() runtime.RuntimeContractResult {
		return w.RuntimePreflight(ctx, agent, request)
	})
	return result, checked, err
}

func (w jobWorker) lookupAgent(ctx context.Context, name string) (db.Agent, error) {
	if w.AgentLookup != nil {
		return w.AgentLookup(ctx, name)
	}
	return w.Store.GetAgent(ctx, name)
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

const maxReadOnlyRuntimeStateFileBytes = 1 << 20

type readOnlySandboxGrants struct {
	reads     []string
	readFiles []string
	writes    []string
	env       []string
	cacheRoot string
	stateDir  string
}

type readOnlyRuntimeAdapter struct {
	runtime.Adapter
	cleanupRoot string
}

func (a readOnlyRuntimeAdapter) PermissionPolicyApplication(agent runtime.Agent) runtime.PermissionPolicyApplication {
	return runtime.ResolvePermissionPolicyApplication(a.Adapter, agent)
}

func (a readOnlyRuntimeAdapter) Deliver(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	// Mailbox repair turns reuse this adapter. Keep its isolated credentials and
	// session state until RunJob returns; the worker owns job-boundary cleanup.
	return a.Adapter.Deliver(ctx, agent, job)
}

func (a readOnlyRuntimeAdapter) cleanup() error {
	// The prompt can move or rewrite anything under the job-private writable
	// cache. Delete that entire root so renamed credentials cannot escape cleanup;
	// never synchronize untrusted state back to the shared runtime profile.
	if a.cleanupRoot == "" {
		return nil
	}
	if err := os.RemoveAll(a.cleanupRoot); err != nil {
		return fmt.Errorf("remove read-only seat cache: %w", err)
	}
	return nil
}

func wrapReadOnlySandboxAdapter(home string, agent runtime.Agent, checkout string, adapter workflow.DeliveryAdapter) (workflow.DeliveryAdapter, error) {
	if !agent.ReadOnlySeat {
		return adapter, nil
	}
	_, gatewayMode := adapter.(modelGatewayRuntimeAdapter)
	grants, err := readOnlyRuntimeSandboxGrants(home, agent, checkout, gatewayMode)
	if err != nil {
		return nil, err
	}
	// A seat's env is rebuilt from scratch, which DROPPED the runtime auth the
	// non-seat path injects (runtimeJobRunnerWithAuth appends
	// runtimeAuthInjectionEnv onto the curated BaseEnv). The seat was therefore
	// left with nothing but its staged credential snapshot, and once that
	// snapshot expired every claude review failed with "OAuth session expired and
	// could not be refreshed" while `gitmoot auth probe claude` stayed green,
	// because the probe reads the resolved auth the seat never received. Inject
	// the same overlay here so a seat authenticates exactly like every other job.
	// Gateway mode is excluded: there the gateway holds the credential and the
	// seat is deliberately given none.
	seatAuthEnv, err := readOnlySeatRuntimeAuthEnv(home, agent.Runtime, gatewayMode)
	if err != nil {
		return nil, err
	}
	wrap := func(runner subprocess.Runner) subprocess.Runner {
		baseEnv := readOnlyRuntimeBaseEnv(agent.Runtime, os.Environ(), filepath.Join(grants.cacheRoot, "gh"))
		curated := graftRuntimeBaseRunner(runner, subprocess.CuratedGroupRunner{
			BaseEnv: append(baseEnv, seatAuthEnv...),
		})
		return landlockReadOnlyRunner(curated, grants.reads, grants.readFiles, grants.writes, grants.env)
	}
	wrapped, err := wrapReadOnlyAdapterRunner(agent.Runtime, adapter, grants.stateDir, wrap)
	if err != nil {
		return nil, err
	}
	if grants.stateDir == "" {
		return wrapped, nil
	}
	runtimeAdapter, ok := wrapped.(runtime.Adapter)
	if !ok {
		return nil, fmt.Errorf("read-only Landlock sandbox returned incompatible %T adapter", wrapped)
	}
	return readOnlyRuntimeAdapter{
		Adapter:     runtimeAdapter,
		cleanupRoot: grants.cacheRoot,
	}, nil
}

func wrapReadOnlyAdapterRunner(runtimeName string, adapter workflow.DeliveryAdapter, stateDir string, wrap func(subprocess.Runner) subprocess.Runner) (workflow.DeliveryAdapter, error) {
	switch a := adapter.(type) {
	case modelGatewayRuntimeAdapter:
		a.runner.ChildConfigDir = stateDir
		wrapped, err := wrapReadOnlyAdapterRunner(runtimeName, a.Adapter, stateDir, wrap)
		if err != nil {
			return nil, err
		}
		runtimeAdapter, ok := wrapped.(runtime.Adapter)
		if !ok {
			return nil, fmt.Errorf("read-only Landlock sandbox returned incompatible %T adapter", wrapped)
		}
		a.Adapter = runtimeAdapter
		return a, nil
	case runtime.ClaudeAdapter:
		a.Runner = wrap(a.Runner)
		return a, nil
	case *runtime.ClaudeAdapter:
		a.Runner = wrap(a.Runner)
		return a, nil
	case runtime.CodexAdapter:
		a.Runner = wrap(a.Runner)
		return a, nil
	case *runtime.CodexAdapter:
		a.Runner = wrap(a.Runner)
		return a, nil
	case runtime.KimiAdapter:
		a.Runner = wrap(a.Runner)
		return a, nil
	case *runtime.KimiAdapter:
		a.Runner = wrap(a.Runner)
		return a, nil
	case runtime.OmpAdapter, *runtime.OmpAdapter:
		return nil, errors.New("read-only seats cannot use omp without an isolated credential broker")
	case runtime.ShellAdapter:
		a.Runner = wrap(a.Runner)
		return a, nil
	case *runtime.ShellAdapter:
		a.Runner = wrap(a.Runner)
		return a, nil
	default:
		return nil, fmt.Errorf("read-only Landlock sandbox cannot wrap %s adapter %T", runtimeName, adapter)
	}
}

func applyReadOnlySeat(readOnlySeat bool, configDir string, agent *runtime.Agent) {
	if agent == nil || !readOnlySeat {
		return
	}
	agent.ReadOnlySeat = true
	agent.RuntimeConfigDir = strings.TrimSpace(configDir)
	agent.WritablePaths = nil
	agent.ReadablePaths = nil
	agent.ReadableFiles = nil
}

func selectedRuntimeConfigDir(runtimeName string) string {
	switch runtimeName {
	case runtime.ClaudeRuntime:
		return strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	case runtime.CodexRuntime:
		return strings.TrimSpace(os.Getenv("CODEX_HOME"))
	default:
		return ""
	}
}

func readOnlyRuntimeSandboxGrants(home string, agent runtime.Agent, checkout string, gatewayMode bool) (readOnlySandboxGrants, error) {
	var grants readOnlySandboxGrants
	paths, err := pathsFromFlag(home)
	if err != nil {
		return grants, fmt.Errorf("resolve read-only sandbox config paths: %w", err)
	}
	toolEnv, err := applyIsolatedToolCacheGrants(paths, workflow.JobPayload{WorktreePath: checkout}, &agent)
	if err != nil {
		return grants, err
	}
	if len(agent.WritablePaths) != 1 {
		return grants, fmt.Errorf("read-only seat requires exactly one isolated cache grant, got %d: %q", len(agent.WritablePaths), agent.WritablePaths)
	}
	grants.cacheRoot = agent.WritablePaths[0]
	grants.writes = []string{grants.cacheRoot}
	grants.env = append(grants.env, toolEnv...)

	grants.reads = append(grants.reads, checkout)
	metadata, err := reviewGitMetadataPaths(checkout)
	if err != nil {
		return grants, err
	}
	grants.reads = append(grants.reads, metadata...)

	stateDir, stateEnv, err := prepareReadOnlyRuntimeState(agent, grants.cacheRoot, gatewayMode)
	if err != nil {
		return grants, err
	}
	grants.stateDir = stateDir
	grants.env = append(grants.env, stateEnv...)

	tempDir := filepath.Join(grants.cacheRoot, "tmp")
	grants.env = append(grants.env,
		"HOME="+filepath.Join(grants.cacheRoot, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(grants.cacheRoot, "xdg-config"),
		"XDG_CACHE_HOME="+filepath.Join(grants.cacheRoot, "xdg-cache"),
		"XDG_DATA_HOME="+filepath.Join(grants.cacheRoot, "xdg-data"),
		"XDG_STATE_HOME="+filepath.Join(grants.cacheRoot, "xdg-state"),
		"GOPATH="+filepath.Join(grants.cacheRoot, "go"),
		"GH_CONFIG_DIR="+filepath.Join(grants.cacheRoot, "gh"),
		"GH_PROMPT_DISABLED=1",
		"TMPDIR="+tempDir,
		"TMP="+tempDir,
		"TEMP="+tempDir,
	)
	if err := validateReadOnlyWritablePaths(checkout, grants.writes); err != nil {
		return grants, err
	}
	for _, path := range append([]string{tempDir, filepath.Join(grants.cacheRoot, "home")}, grants.writes...) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return grants, fmt.Errorf("create read-only sandbox write directory %q: %w", path, err)
		}
	}
	grants.reads = compactCleanPaths(grants.reads)
	grants.readFiles = compactCleanPaths(grants.readFiles)
	grants.writes = compactCleanPaths(grants.writes)
	return grants, nil
}

func prepareReadOnlyRuntimeState(agent runtime.Agent, cacheRoot string, gatewayMode bool) (string, []string, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", nil, fmt.Errorf("resolve read-only runtime state home: %w", err)
	}
	sourceDir := strings.TrimSpace(agent.RuntimeConfigDir)
	var relativeState, credentialFile, credentialSection string
	stateRoot := filepath.Join(cacheRoot, "runtime-state")
	switch agent.Runtime {
	case runtime.ClaudeRuntime:
		if sourceDir == "" {
			sourceDir = filepath.Join(userHome, ".claude")
		}
		relativeState = ".claude"
		if !gatewayMode {
			credentialFile = ".credentials.json"
			credentialSection = "claudeAiOauth"
		}
	case runtime.CodexRuntime:
		if sourceDir == "" {
			sourceDir = filepath.Join(userHome, ".codex")
		}
		relativeState = ".codex"
		credentialFile = "auth.json"
	case runtime.KimiRuntime:
		if sourceDir == "" {
			sourceDir = filepath.Join(userHome, ".kimi-code")
		}
		// Kimi reads HOME/.kimi-code. The sandbox supplies HOME=cacheRoot/home,
		// so stage its isolated profile at that exact path.
		stateRoot = cacheRoot
		relativeState = filepath.Join("home", ".kimi-code")
		credentialFile = filepath.Join("credentials", "kimi-code.json")
	case runtime.ShellRuntime:
		return "", nil, nil
	case runtime.OmpRuntime:
		return "", nil, errors.New("read-only seats cannot use omp without an isolated credential broker")
	default:
		return "", nil, fmt.Errorf("read-only seat runtime %q has no isolated state policy", agent.Runtime)
	}
	sourceDir, err = filepath.Abs(sourceDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve runtime state directory %q: %w", sourceDir, err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(sourceDir); resolveErr == nil {
		sourceDir = resolved
	}
	stateDir := filepath.Join(stateRoot, relativeState)
	if err := os.RemoveAll(stateDir); err != nil {
		return "", nil, fmt.Errorf("reset isolated runtime state %q: %w", stateDir, err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create isolated runtime state %q: %w", stateDir, err)
	}
	if credentialFile != "" {
		if err := stageReadOnlyRuntimeCredential(
			filepath.Join(sourceDir, credentialFile),
			filepath.Join(stateDir, credentialFile),
			credentialSection,
		); err != nil {
			return "", nil, err
		}
	}
	var stateEnv []string
	switch agent.Runtime {
	case runtime.ClaudeRuntime:
		stateEnv = append(stateEnv, "CLAUDE_CONFIG_DIR="+stateDir)
	case runtime.CodexRuntime:
		stateEnv = append(stateEnv, "CODEX_HOME="+stateDir)
	}
	return stateDir, stateEnv, nil
}

func stageReadOnlyRuntimeCredential(source, destination, section string) error {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect runtime state file %q: %w", source, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxReadOnlyRuntimeStateFileBytes {
		return fmt.Errorf("runtime state file %q must be a regular file no larger than %d bytes", source, maxReadOnlyRuntimeStateFileBytes)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read runtime state file %q: %w", source, err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("credential must be a JSON object")
		}
		return fmt.Errorf("validate runtime credential %q: %w", source, err)
	}
	if section != "" {
		value, ok := object[section]
		if !ok {
			return fmt.Errorf("runtime credential %q lacks required %q section", source, section)
		}
		data, err = json.Marshal(map[string]json.RawMessage{section: value})
		if err != nil {
			return fmt.Errorf("isolate runtime credential %q: %w", source, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create isolated runtime state directory: %w", err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		return fmt.Errorf("stage runtime state file %q: %w", destination, err)
	}
	return nil
}

func readOnlyRuntimeBaseEnv(runtimeName string, environ []string, githubDir string) []string {
	allowed := make(map[string]struct{}, len(curatedBaseEnvNames)+2)
	for _, name := range curatedBaseEnvNames {
		allowed[name] = struct{}{}
	}
	switch runtimeName {
	case runtime.ClaudeRuntime:
		allowed["CLAUDE_CONFIG_DIR"] = struct{}{}
	case runtime.CodexRuntime:
		allowed["CODEX_HOME"] = struct{}{}
	}
	base := make([]string, 0, len(environ)+2)
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(name, "GH_") || strings.HasPrefix(name, "GITHUB_") {
			continue
		}
		if _, ok := allowed[name]; ok || strings.HasPrefix(name, "LC_") {
			base = append(base, entry)
		}
	}
	return append(base, "GH_CONFIG_DIR="+githubDir, "GH_PROMPT_DISABLED=1")
}

func validateReadOnlyWritablePaths(checkout string, writes []string) error {
	type protectedRoot struct {
		kind string
		path string
	}
	protected := []protectedRoot{{kind: "read-only worktree", path: checkout}}
	metadata, err := reviewGitMetadataPaths(checkout)
	if err != nil {
		return err
	}
	for _, path := range metadata {
		protected = append(protected, protectedRoot{kind: "protected git metadata", path: path})
	}
	for i := range protected {
		resolved, err := resolvePathForContainment(protected[i].path)
		if err != nil {
			return fmt.Errorf("resolve %s %q: %w", protected[i].kind, protected[i].path, err)
		}
		protected[i].path = resolved
	}
	for _, path := range compactCleanPaths(writes) {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("read-only sandbox write path %q must be absolute", path)
		}
		resolvedWrite, err := resolvePathForContainment(path)
		if err != nil {
			return fmt.Errorf("resolve read-only sandbox write path %q: %w", path, err)
		}
		for _, root := range protected {
			if pathsOverlap(root.path, resolvedWrite) {
				return fmt.Errorf("read-only sandbox write path %q overlaps %s %q", path, root.kind, root.path)
			}
		}
	}
	return nil
}

func reviewGitMetadataPaths(checkout string) ([]string, error) {
	dotGit := filepath.Join(checkout, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return nil, fmt.Errorf("resolve review git metadata %q: %w", dotGit, err)
	}
	if info.IsDir() {
		return []string{dotGit}, nil
	}
	content, err := os.ReadFile(dotGit)
	if err != nil {
		return nil, fmt.Errorf("read review gitdir file %q: %w", dotGit, err)
	}
	gitDirText := strings.TrimSpace(string(content))
	const prefix = "gitdir:"
	if !strings.HasPrefix(gitDirText, prefix) {
		return nil, fmt.Errorf("review gitdir file %q has invalid contents", dotGit)
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(gitDirText, prefix))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(checkout, gitDir)
	}
	gitDir = filepath.Clean(gitDir)
	commonDir := gitDir
	commonContent, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err == nil {
		commonDir = strings.TrimSpace(string(commonContent))
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		}
		commonDir = filepath.Clean(commonDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read review common-dir file for %q: %w", gitDir, err)
	}
	return compactCleanPaths([]string{gitDir, commonDir}), nil
}

func resolvePathForContainment(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	current := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func pathsOverlap(left, right string) bool {
	contains := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
	}
	return contains(left, right) || contains(right, left)
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
		// An operator-set CLAUDE_CONFIG_DIR selects the ACCOUNT the engine runs
		// as. Hard-coding $HOME/.claude here silently replaced it, so a daemon
		// configured onto a live account still pointed produce jobs at a dead
		// one: on this host /root/.claude has carried expiresAt 0 with no refresh
		// token since 2026-08-31, which yields exactly "OAuth session expired and
		// could not be refreshed". Honor the configured dir and fall back only
		// when none is set.
		stateDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
		if stateDir == "" {
			stateDir = filepath.Join(home, ".claude")
		}
		stateDir = filepath.Clean(stateDir)
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
	return landlockRuntimeRunner(runner, reads, readFiles, writes, env, false)
}

func landlockReadOnlyRunner(runner subprocess.Runner, reads, readFiles, writes, env []string) subprocess.Runner {
	return landlockRuntimeRunner(runner, reads, readFiles, writes, env, true)
}

func landlockRuntimeRunner(runner subprocess.Runner, reads, readFiles, writes, env []string, readOnlyWorkdir bool) subprocess.Runner {
	readable := append([]string(nil), reads...)
	files := append([]string(nil), readFiles...)
	writable := append([]string(nil), writes...)
	runtimeEnv := append([]string(nil), env...)
	wrap := func(inner subprocess.Runner) subprocess.WrappingRunner {
		return subprocess.WrappingRunner{Inner: inner, ReadablePaths: readable, ReadableFiles: files, WritablePaths: writable, ReadOnlyWorkdir: readOnlyWorkdir, Env: runtimeEnv}
	}
	if tee, ok := runner.(subprocess.TeeRunner); ok {
		inner := tee.Inner
		if inner == nil {
			inner = subprocess.GroupRunner{}
		}
		if _, wrapped := inner.(subprocess.WrappingRunner); !wrapped {
			tee.Inner = wrap(inner)
		}
		return tee
	}
	if tee, ok := runner.(*subprocess.TeeRunner); ok {
		inner := tee.Inner
		if inner == nil {
			inner = subprocess.GroupRunner{}
		}
		if _, wrapped := inner.(subprocess.WrappingRunner); !wrapped {
			tee.Inner = wrap(inner)
		}
		return tee
	}
	if runner == nil {
		runner = subprocess.GroupRunner{}
	}
	if _, wrapped := runner.(subprocess.WrappingRunner); wrapped {
		return runner
	}
	return wrap(runner)
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

// resolveExecBackend resolves the effective execution backend for one job
// (#1536 P1): the optional [remote_exec] section (default "local") overridden
// by the payload's exec_backend field. It is read per dispatch — like
// [credentials] — so a config edit takes effect without daemon reload
// plumbing. A missing config file or section resolves to local; an unknown
// value (config or override) is a hard error naming the value and the allowed
// set — the fail-loud contract, never a silent fallback.
func (w jobWorker) resolveExecBackend(jobOverride string, jobOverridePresent bool) (execbackend.Backend, config.RemoteExecConfig, error) {
	cfg, err := w.executionBackendConfig()
	if err != nil {
		return "", config.RemoteExecConfig{}, err
	}
	if jobOverridePresent {
		backend, err := execbackend.Resolve(cfg.Backend, &jobOverride)
		return backend, cfg, err
	}
	backend, err := execbackend.Resolve(cfg.Backend, nil)
	return backend, cfg, err
}

var daemonJobExecBackendFor = func(w jobWorker, jobOverride string, jobOverridePresent bool) (execbackend.Backend, config.RemoteExecConfig, error) {
	return w.resolveExecBackend(jobOverride, jobOverridePresent)
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
	case runtime.KimiRuntime:
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
	inner := subprocess.StreamRunner(subprocess.GroupRunner{})
	if w.executionRunner != nil {
		stream, ok := w.executionRunner.(subprocess.StreamRunner)
		if !ok {
			_ = logFile.Close()
			writeLine(w.Stdout, "job %s cockpit tee backend runner lacks streaming", jobID)
			return nil, "", nil
		}
		inner = stream
	}
	adapter, err := buildRuntimeAdapter(w.ConfigHome, agent, checkout, subprocess.TeeRunner{Inner: inner, Out: runtimeOutputWriter(outputs...)})
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
	case runtime.CodexRuntime, runtime.ClaudeRuntime, runtime.KimiRuntime:
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

func (w jobWorker) runWithTempWorker(ctx context.Context, job db.Job, payload workflow.JobPayload, backend execbackend.Backend, original runtime.Agent, checkout string, policy config.ParallelSessionPolicy, reason string, observePermissionPolicy bool) error {
	jobRunner, err := jobSubprocessRunnerForBackend(backend)
	if err != nil {
		return err
	}
	started, err := w.startTempWorker(ctx, job, payload, backend, original, checkout)
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
	// delegatedJob carries the temp agent and rewritten payload. It is not an
	// admission token: this read can observe a cancel/retry generation that raced
	// after job was admitted. Every pre-flight terminal write below therefore
	// remains anchored to job, while runtime work uses delegatedJob's metadata.
	adapter, err := w.deliveryAdapterForBackend(backend, started.Agent, checkout)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		return nil
	}
	writeLine(w.Stdout, "running job %s for temporary worker %s in %s", job.ID, started.Agent.Name, payload.Repo)
	// Same lease-outlives-context invariant as run(): the temp-worker run context is
	// armed at started.JobTimeout below, so the lease must be jobTimeout+grace to
	// cover teardown and avoid the #536 live-worker reap+requeue window.
	tempLockTTL := started.JobTimeout + runtimeLeaseTeardownGrace
	releaseLock, acquired, lockKey, ownerToken, err := acquireRuntimeSessionLock(ctx, w.Store, delegatedJob.ID, started.Agent, time.Now().UTC(), tempLockTTL)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
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
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
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
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
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
	adapter = wrapManagedWorktreeRuntimeEnv(payload, adapter)
	if err := w.Store.MarkAgentInstanceRunning(ctx, started.Agent.Name, time.Now().UTC(), started.JobTimeout); err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
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
	runStartedAt := time.Now().UTC()
	var cancel context.CancelFunc
	runCtx, cancel = context.WithTimeout(runCtx, started.JobTimeout)
	defer cancel()
	runDeadline, hasRunDeadline := runCtx.Deadline()
	stopKillPending := func() {}
	if hasRunDeadline {
		stopKillPending = armJobKillPending(w.Store, delegatedJob.ID, runDeadline)
	}
	if observePermissionPolicy {
		adapter = w.observePermissionPolicyEffects(adapter, delegatedJob.ID, checkout)
	}
	engine := w.workflowForJob(checkout, jobRunner)
	_, err = engine.RunJob(runCtx, delegatedJob.ID, started.Agent, adapter)
	stopKillPending()
	if err != nil {
		if quotaErr := w.quotaRoleUnavailableHooks().recordRuntimeOutcome(ctx, delegatedJob, payload, started.Agent, err, time.Now().UTC()); quotaErr != nil {
			writeLine(w.Stdout, "job %s org-role quota unavailability capture failed: %v", delegatedJob.ID, quotaErr)
		}
		var markErr error
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			markErr = w.handleRunJobError(ctx, delegatedJob.ID, observedJobLifecycle(delegatedJob), context.DeadlineExceeded, jobTimeoutEvidence{Deadline: runDeadline, Started: runStartedAt})
		} else {
			markErr = w.handleRunJobError(ctx, delegatedJob.ID, observedJobLifecycle(delegatedJob), err)
		}
		if markErr != nil {
			return markErr
		}
		if reconcileErr := engine.ReconcileTerminalDrivingJob(ctx, delegatedJob.ID); reconcileErr != nil {
			return reconcileErr
		}
		_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
		// Same record as the ordinary path (#1620) — a delegated run's runtime
		// error was journal-only too, and the temp-worker row is exactly where an
		// operator looks first.
		w.recordDeliveryFailureDiagnostics(ctx, delegatedJob.ID, delegatedJob.LifecycleGeneration, err)
		writeLine(w.Stdout, "job %s failed: %v", delegatedJob.ID, err)
		return nil
	}
	if clearErr := w.quotaRoleUnavailableHooks().recordRuntimeOutcome(ctx, delegatedJob, payload, started.Agent, nil, time.Now().UTC()); clearErr != nil {
		writeLine(w.Stdout, "job %s org-role quota unavailability clear failed: %v", delegatedJob.ID, clearErr)
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
	mailbox := workflow.NewMailbox(w.Store, workflow.UnavailableDeliveryWorktreeResolver("temporary worker merge-back enqueue"))
	mailbox.RuntimeDefaultModel = runtimeDefaultModelResolver(w.workflowHome())
	mailbox.RequireWorkflowPolicy = requireWorkflowPolicyResolver(w.workflowHome())
	mailbox.OrgPolicy = orgPolicyResolver(w.workflowHome())
	if _, err := mailbox.Enqueue(ctx, request); err != nil {
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

func (w jobWorker) startTempWorker(ctx context.Context, job db.Job, payload workflow.JobPayload, backend execbackend.Backend, original runtime.Agent, checkout string) (tempWorkerStartResult, error) {
	idleTimeout := 20 * time.Minute
	managed, err := w.managedJobConfig(ctx, original.Name)
	if err != nil {
		return tempWorkerStartResult{}, err
	}
	if managed.OK {
		idleTimeout = managed.IdleTimeout
	}
	// Resolve the same authority as the primary dispatch path so runtime-session
	// contention cannot route a payload around the hard ceiling.
	jobTimeout := effectiveJobTimeout(payload, managed)
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
	adapter, err := w.StartAdapterFactory(backend, tempAgent.Runtime, checkout)
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
func (w jobWorker) startEphemeralWorker(ctx context.Context, job db.Job, payload workflow.JobPayload, backend execbackend.Backend, jobRunner subprocess.Runner) (err error) {
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
		ExecBackend:    string(backend),
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
	checkout, err := w.checkoutForJob(ctx, job, payload, ephemeralAgent, jobRunner)
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
	adapter, err := w.StartAdapterFactory(backend, ephemeralAgent.Runtime, checkout)
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
	OK                bool
	Instance          bool
	JobTimeout        time.Duration
	IdleTimeout       time.Duration
	JobTimeoutDefault time.Duration
	JobTimeoutMax     time.Duration
}

func (w jobWorker) managedJobConfig(ctx context.Context, agentName string) (managedJobRuntimeConfig, error) {
	runtimeConfig := managedJobRuntimeConfig{
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
	}
	var paths config.Paths
	configFilePresent := false
	if w.ConfigHomeExplicit || strings.TrimSpace(w.ConfigHome) != "" {
		var err error
		paths, err = w.configPaths()
		if err != nil {
			return managedJobRuntimeConfig{}, err
		}
		daemonConfig, err := config.LoadDaemonRuntimeConfig(paths)
		// ConfigHome also enables checkout isolation in tests and embedders; it
		// does not prove that a config file or agent-type registry exists. Absence
		// is the normal unmanaged case and falls through to the daemon defaults.
		// A present-but-malformed file still returns its parse error below.
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return managedJobRuntimeConfig{}, err
		}
		if err == nil {
			configFilePresent = true
			runtimeConfig.JobTimeoutDefault, runtimeConfig.JobTimeoutMax = daemonConfig.JobTimeoutPolicy()
		}
	}

	registered, err := w.Store.GetAgent(ctx, agentName)
	if errors.Is(err, sql.ErrNoRows) {
		return runtimeConfig, nil
	}
	if err != nil {
		return managedJobRuntimeConfig{}, err
	}
	// A normal registered agent's stable name is also its [agents.<type>]
	// enrollment key. A live instance row, when present, is an explicit override
	// layer: managed/temp workers retain the type that created them even when their
	// generated instance name differs.
	configType := registered.Name
	instance, err := w.Store.GetAgentInstance(ctx, agentName)
	if err == nil {
		runtimeConfig.Instance = true
		configType = instance.Type
	} else if !errors.Is(err, sql.ErrNoRows) {
		return managedJobRuntimeConfig{}, err
	}
	if original := originalAgentForTempWorkerType(configType); original != "" {
		// Ephemeral specs have no registered parent type by design; they inherit the
		// daemon default while retaining their instance lifecycle row.
		if original == ephemeralWorkerInstanceOrigin {
			return runtimeConfig, nil
		}
		originalAgent, err := w.Store.GetAgent(ctx, original)
		if errors.Is(err, sql.ErrNoRows) {
			return runtimeConfig, nil
		}
		if err != nil {
			return managedJobRuntimeConfig{}, err
		}
		configType = originalAgent.Name
		if originalInstance, instanceErr := w.Store.GetAgentInstance(ctx, original); instanceErr == nil {
			configType = originalInstance.Type
		} else if !errors.Is(instanceErr, sql.ErrNoRows) {
			return managedJobRuntimeConfig{}, instanceErr
		}
	}
	if !configFilePresent {
		return runtimeConfig, nil
	}
	types, err := config.LoadAgentTypes(paths)
	if err != nil {
		return managedJobRuntimeConfig{}, err
	}
	agentType, ok := types[configType]
	if !ok {
		// Enrollment is optional for both stable agents and live instances. Keep
		// Instance above for lifecycle updates, but use the daemon timeout default.
		return runtimeConfig, nil
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
	runtimeConfig.OK = true
	runtimeConfig.JobTimeout = jobTimeout
	runtimeConfig.IdleTimeout = idleTimeout
	return runtimeConfig, nil
}

type jobTimeoutResolution struct {
	Timeout   time.Duration
	Requested time.Duration
	Max       time.Duration
	Source    string
	Clamped   bool
}

// effectiveJobTimeout returns the kill deadline selected by the independent
// timeout authority. The stale-running threshold is deliberately absent: it is
// only a crash/staleness predicate and can never become a run-context deadline.
func effectiveJobTimeout(payload workflow.JobPayload, managed managedJobRuntimeConfig) time.Duration {
	return resolveEffectiveJobTimeout(payload, managed).Timeout
}

func resolveEffectiveJobTimeout(payload workflow.JobPayload, managed managedJobRuntimeConfig) jobTimeoutResolution {
	jobTimeoutDefault := managed.JobTimeoutDefault
	if jobTimeoutDefault <= 0 {
		jobTimeoutDefault = config.DefaultDaemonJobTimeoutDefault
	}
	jobTimeoutMax := managed.JobTimeoutMax
	if jobTimeoutMax <= 0 {
		jobTimeoutMax = config.DefaultDaemonJobTimeoutMax
	}
	requested := jobTimeoutDefault
	source := "daemon default"
	if managed.JobTimeout > 0 {
		requested = managed.JobTimeout
		source = "agent type"
	}
	if d, err := time.ParseDuration(strings.TrimSpace(payload.JobTimeout)); err == nil && d > 0 {
		requested = d
		source = "payload"
	}
	resolution := jobTimeoutResolution{Timeout: requested, Requested: requested, Max: jobTimeoutMax, Source: source}
	if requested > jobTimeoutMax {
		resolution.Timeout = jobTimeoutMax
		resolution.Clamped = true
	}
	return resolution
}

const jobKillPendingLead = 5 * time.Second

// armJobKillPending records intent before the run context expires. The callback
// deliberately uses Background: the witness must survive the deadline it
// predicts. A completed run stops the timer before terminal handling.
func armJobKillPending(store *db.Store, jobID string, deadline time.Time) func() {
	delay := time.Until(deadline) - jobKillPendingLead
	if delay < 0 {
		delay = 0
	}
	timer := time.AfterFunc(delay, func() {
		message := fmt.Sprintf("deadline=%s", deadline.UTC().Format(time.RFC3339Nano))
		_ = store.AddJobEvent(context.Background(), db.JobEvent{
			JobID:   jobID,
			Kind:    "job_kill_pending",
			Message: message,
		})
	})
	return func() { timer.Stop() }
}

// recoverKillPendingJobs is the startup anti-immortality pass. A running job
// with a pre-kill witness but no terminal event consumed its deadline and the
// daemon died during the kill unwind; it must be failed, never silently requeued.
func recoverKillPendingJobs(ctx context.Context, store *db.Store, stdout io.Writer) error {
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.State != string(workflow.JobRunning) || isSessionRecordedJob(job) {
			continue
		}
		events, err := store.ListJobEvents(ctx, job.ID)
		if err != nil {
			return err
		}
		pending, terminalAfterPending := false, false
		for _, event := range events {
			switch event.Kind {
			case "job_kill_pending":
				pending = true
				terminalAfterPending = false
			case string(workflow.JobSucceeded), string(workflow.JobFailed), string(workflow.JobBlocked), string(workflow.JobCancelled), "job_timeout":
				if pending {
					terminalAfterPending = true
				}
			}
		}
		if !pending || terminalAfterPending {
			continue
		}
		settled, err := failRecoveredRunningJob(ctx, store, stdout, time.Now().UTC(), job,
			"daemon died mid-kill after job_kill_pending witness (killed-by-deadline-unwitnessed)")
		if err != nil {
			return err
		}
		if settled {
			writeLine(stdout, "failed deadline-killed job %s after unwitnessed daemon shutdown", job.ID)
		}
	}
	return nil
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
	fromState := ""
	existing, err := store.GetTask(ctx, payload.TaskID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if existing.State == string(workflow.TaskDismissed) {
			return fmt.Errorf("task %s is dismissed; permission-blocked job cannot resurrect it", existing.ID)
		}
		fromState = existing.State
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
	reason := fmt.Sprintf("permission-blocked job %s", job.ID)
	if payload.Result != nil && strings.TrimSpace(payload.Result.Summary) != "" {
		reason = strings.TrimSpace(payload.Result.Summary)
	}
	blocked, err := store.BlockTaskWithEvent(ctx, task, db.TaskEvent{
		Kind:      "permission_job_blocked",
		FromState: fromState,
		Reason:    reason,
	})
	if err != nil {
		if !blocked {
			return err
		}
		return errors.Join(workflow.BlockedError{Reason: reason},
			fmt.Errorf("record permission block for task %s: %w", task.ID, err))
	}
	return nil
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
		case "advance_completed", "advance_retried", "advance_blocked", "advance_retry_skipped", workflow.ReviewLoopDetectedEventKind:
			// The defect is not that advance_blocked stops retries; stopping retries is
			// all this classification does. The blocked outcome is settled separately
			// and must never be converted back into a retry.
			//
			// advance_blocked_superseded is deliberately ABSENT from this arm (#1407): a
			// settlement describing a PREVIOUS run must not cancel the current run's
			// pending advancement. TestSupersededSettlementDoesNotClearPendingAdvanceRetry
			// pins that, and it is the reason adding a kind here is never a mechanical
			// edit -- each entry silences a retry.
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

// closedPullRequestSupersededChild reports whether this job is EXACTLY the shape the
// closed-PR sweep produces: a delegation child terminalized because its pull request is
// no longer open, carrying the synthetic result the parent's advanceDelegations consumes
// (#1673).
//
// IT READS THE RESULT, NOT THE EVENT LOG. An event scan cannot be made safe here:
// job_events has no lifecycle_generation column, and RetryJob starts a new run while
// PRESERVING prior events, so a historical supersession would keep authorizing the
// bypass for a job that has since been retried and failed normally - and that run still
// owes the full advance, deferred teardown and implement bookkeeping included. Carrying
// the fact in the result means RetryJob clears it along with Result, so the stale case
// cannot arise rather than being detected.
//
// Rejected alternatives: stamping the generation on the supersession event at write time
// (follows the exec-backend-attempts precedent, but adds a write-side schema change and
// still leaves a scan whose correctness depends on the filter being right everywhere it
// is read); and comparing against a recorded generation boundary (same objection, plus a
// second durable fact to keep in agreement - the defect class this campaign kept hitting).
func closedPullRequestSupersededChild(job db.Job, payload workflow.JobPayload) bool {
	if strings.TrimSpace(payload.ParentJobID) == "" || payload.Result == nil {
		return false
	}
	if job.State != string(workflow.JobFailed) {
		return false
	}
	return payload.Result.SupersededPullRequestClosed
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
	jobBackend, jobBackendPresent := payload.ExecBackendOverride()
	backend, _, err := daemonJobExecBackendFor(w, jobBackend, jobBackendPresent)
	if err != nil {
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry backend resolution failed: "+err.Error())
	}
	jobRunner, err := jobSubprocessRunnerForBackend(backend)
	if err != nil {
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry backend consumption failed: "+err.Error())
	}
	if refreshed, ok, err := w.refreshImplementedPayloadForRetry(ctx, job, payload, jobRunner); err != nil {
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry refresh failed: "+err.Error())
	} else if ok {
		payload = refreshed
	}
	checkout, err := w.checkoutForJob(ctx, job, payload, agent, jobRunner)
	if err != nil {
		// THE STRUCTURAL ROUTE (#1673). A child terminalized by the closed-PR sweep can
		// never satisfy this preflight - the shared checkout is never on a dead PR's head
		// - and gating its parent advancement on that stranded the parent forever. But
		// letting it through the FULL advance with an empty checkout was too broad:
		// AdvanceJob also normalizes high-risk lens verdicts, continues into the review
		// merge gate and registers worktree teardown, so a preflight failure would become
		// database and remote action under an unvalidated checkout. (It can dispatch a
		// child's own delegations too, but not for one whose decision is "failed" - the
		// parent-side block short-circuits first, which review measured. The point is what
		// the narrow operation EXCLUDES, not that each excluded step was reachable here.)
		//
		// So this shape is routed to an operation that CANNOT reach any of that, rather
		// than to the full advance with a narrower predicate in front of it. A predicate
		// is a list the next edit widens; an operation that cannot execute the child's own
		// advancement cannot be widened by accident.
		if closedPullRequestSupersededChild(job, payload) {
			parentOnly := w.workflowForJob("", jobRunner)
			if advErr := parentOnly.AdvanceParentDAGForTerminalChild(ctx, job.ID); advErr != nil {
				var blocked workflow.BlockedError
				if errors.As(advErr, &blocked) {
					return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_blocked", Message: advErr.Error()})
				}
				return w.recordAdvanceRetryOnce(ctx, job.ID, "parent-only advancement failed: "+advErr.Error())
			}
			return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_completed",
				Message: "parent advancement completed without a delivery checkout (closed pull request)"})
		}
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry preflight failed: "+err.Error())
	}
	engine := w.workflowForJob(checkout, jobRunner)
	if err := engine.AdvanceJob(ctx, job.ID); err != nil {
		var awaiting workflow.AwaitingHumanError
		if errors.As(err, &awaiting) {
			return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_awaiting_human", Message: err.Error()})
		}
		var blocked workflow.BlockedError
		if errors.As(err, &blocked) {
			return w.recordBlockedAdvancement(ctx, job.ID, observedJobLifecycle(job), err, blocked)
		}
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry failed: "+err.Error())
	}
	writeLine(w.Stdout, "job %s advancement retried", job.ID)
	if err := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_retried", Message: "post-delivery workflow retry completed"}); err != nil {
		return err
	}
	return engine.ReconcileTerminalDrivingJob(ctx, job.ID)
}

func (w jobWorker) refreshImplementedPayloadForRetry(ctx context.Context, job db.Job, payload workflow.JobPayload, runner subprocess.Runner) (workflow.JobPayload, bool, error) {
	if job.Type != "implement" || payload.Result == nil || payload.Result.Decision != "implemented" {
		return payload, false, nil
	}
	checkout, err := w.resolveJobCheckoutForRunner(ctx, job, payload, runner)
	if err != nil {
		return workflow.JobPayload{}, false, err
	}
	payload, err = refreshDaemonJobPayloadForRunner(ctx, w.Store, checkout, job, payload, runner)
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

// buildJobAdapterForBackend consumes the already-resolved backend at the
// daemon's adapter boundary. Adding a name to the selector's implemented set is
// insufficient: a new backend must extend execbackend.Consume and supply its
// builder at every compiler-identified call site before it can execute.
func (w jobWorker) buildJobAdapterForBackend(backend execbackend.Backend, agent runtime.Agent, checkout string, output ...io.Writer) (workflow.DeliveryAdapter, error) {
	return execbackend.Consume(backend, func() (workflow.DeliveryAdapter, error) {
		if len(output) > 0 && output[0] != nil && w.OutputAdapterFactory != nil {
			return w.OutputAdapterFactory(agent, checkout, output[0])
		}
		return w.AdapterFactory(agent, checkout)
	}, func() (workflow.DeliveryAdapter, error) {
		return unprovisionedRemoteDeliveryAdapter{}, nil
	})
}

// unprovisionedRemoteDeliveryAdapter prevents a configured remote job from
// falling back to a host adapter before its lifecycle instance is attached.
type unprovisionedRemoteDeliveryAdapter struct{}

func (unprovisionedRemoteDeliveryAdapter) Deliver(context.Context, runtime.Agent, runtime.Job) (runtime.Result, error) {
	return runtime.Result{}, errors.New("remote execution backend is not provisioned")
}

func (w jobWorker) deliveryAdapterForBackend(backend execbackend.Backend, agent runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
	return execbackend.Consume(backend, func() (workflow.DeliveryAdapter, error) {
		return w.AdapterFactory(agent, checkout)
	}, func() (workflow.DeliveryAdapter, error) {
		// The temporary-worker loop never provisions an execution instance.
		return nil, errors.New("temporary workers do not support the remote execution backend")
	})
}

// buildRuntimeAdapter constructs the concrete runtime adapter for a job. With
// credential curation off, a nil runner remains nil and the adapter falls through
// to GroupRunner exactly as before. With curation on, runtimeJobRunner installs
// the curated process-group base beneath any tee, relay, or Landlock wrapper. The
// wrappers still append their environment last and preserve result capture,
// cancellation, and live output.
func buildRuntimeAdapter(home string, agent runtime.Agent, checkout string, runner subprocess.Runner) (workflow.DeliveryAdapter, error) {
	backend := execbackend.Local
	if agent.ExecBackend != "" {
		resolved, err := execbackend.ParseImplemented(agent.ExecBackend)
		if err != nil {
			return nil, err
		}
		backend = resolved
	}
	// Consume the positive backend implementation after parsing. A future name
	// accepted by ParseImplemented still cannot reach Local's composition unless
	// P2 explicitly extends execbackend.Consume and every compiler-identified
	// call site supplies that backend's builder.
	return execbackend.Consume(backend, func() (workflow.DeliveryAdapter, error) {
		return buildLocalRuntimeAdapter(home, agent, checkout, runner)
	}, func() (workflow.DeliveryAdapter, error) {
		return buildRemoteRuntimeAdapter(agent, checkout, runner)
	})
}

func buildRemoteRuntimeAdapter(agent runtime.Agent, checkout string, runner subprocess.Runner) (workflow.DeliveryAdapter, error) {
	if !runnerReachesExecutionInstance(runner) {
		return nil, errors.New("remote execution backend runtime runner is not attached to an instance")
	}
	if agent.Runtime != runtime.ShellRuntime {
		return nil, fmt.Errorf("runtime %q is not supported on the remote execution backend", agent.Runtime)
	}
	if len(agent.WritablePaths) > 0 || len(agent.ReadablePaths) > 0 || len(agent.ReadableFiles) > 0 {
		return nil, errors.New("runtime path grants are not supported on the remote execution backend")
	}
	return runtime.ShellAdapter{Dir: checkout, Runner: runner}, nil
}

func runnerReachesExecutionInstance(runner subprocess.Runner) bool {
	switch runner := runner.(type) {
	case execbackend.InstanceRunner:
		return runner.Backend != nil && runner.Instance != nil
	case *execbackend.InstanceRunner:
		return runner != nil && runner.Backend != nil && runner.Instance != nil
	case subprocess.TeeRunner:
		return runnerReachesExecutionInstance(runner.Inner)
	case *subprocess.TeeRunner:
		return runner != nil && runnerReachesExecutionInstance(runner.Inner)
	case subprocess.EnvInjectingRunner:
		return runnerReachesExecutionInstance(runner.Inner)
	case *subprocess.EnvInjectingRunner:
		return runner != nil && runnerReachesExecutionInstance(runner.Inner)
	case subprocess.WrappingRunner:
		return runnerReachesExecutionInstance(runner.Inner)
	case *subprocess.WrappingRunner:
		return runner != nil && runnerReachesExecutionInstance(runner.Inner)
	default:
		return false
	}
}

// buildLocalRuntimeAdapter is the pre-#1536 runner-composition pipeline. Keep
// its ordering unchanged: runtimeJobRunner/credgw, optional Landlock produce
// wrapper, concrete runtime adapter, then model-gateway wrapping.
func buildLocalRuntimeAdapter(home string, agent runtime.Agent, checkout string, runner subprocess.Runner) (workflow.DeliveryAdapter, error) {
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
	case runtime.OmpRuntime:
		adapter = runtime.OmpAdapter{Dir: checkout, Runner: runner}
	case runtime.ShellRuntime:
		adapter = runtime.ShellAdapter{Dir: checkout, Runner: runner}
	default:
		return nil, fmt.Errorf("unsupported runtime: %s", agent.Runtime)
	}
	return wrapModelGatewayAdapter(adapter, gatewayRunner), nil
}

func startRuntimeAdapterForBackend(backend execbackend.Backend, home string, runtimeName string, checkout string) (runtime.Adapter, error) {
	return execbackend.Consume(backend, func() (runtime.Adapter, error) {
		return runtimeAdapterFor(home, runtimeName, checkout)
	}, func() (runtime.Adapter, error) {
		// Agent-start sessions are host-owned and have no lifecycle instance.
		return nil, errors.New("agent start sessions do not support the remote execution backend")
	})
}

func (w jobWorker) defaultStartAdapter(backend execbackend.Backend, runtimeName string, checkout string) (runtime.Adapter, error) {
	return startRuntimeAdapterForBackend(backend, w.ConfigHome, runtimeName, checkout)
}

func (w jobWorker) defaultWorkflow(checkout string) workflow.Engine {
	return w.defaultWorkflowForRunner(checkout, subprocess.ExecRunner{})
}
func (w jobWorker) workflowForHost(checkout string) workflow.Engine {
	if w.WorkflowFactory != nil {
		return w.WorkflowFactory(checkout)
	}
	return w.defaultWorkflow(checkout)
}

func (w jobWorker) defaultWorkflowForRunner(checkout string, runner subprocess.Runner) workflow.Engine {
	engine := daemonWorkflowEngineForRunner(w.Store, github.NewClient(checkout), checkout, w.workflowHome(), runner, nil)
	w.applyOrchestratePolicy(&engine)
	return engine
}

func (w jobWorker) checkoutForJob(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent, runner subprocess.Runner) (string, error) {
	if w.CheckoutValidator != nil {
		return w.CheckoutValidator(ctx, job, payload, agent)
	}
	return w.defaultCheckoutForRunner(ctx, job, payload, agent, runner)
}

func (w jobWorker) workflowForJob(checkout string, runner subprocess.Runner) workflow.Engine {
	if w.WorkflowFactory != nil {
		return w.WorkflowFactory(checkout)
	}
	return w.defaultWorkflowForRunner(checkout, runner)
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
