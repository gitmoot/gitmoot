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
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/sandbox"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/toolchain"
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
	ConfigHome string

	// DrainSentinelPath is the RESOLVED absolute path of the #1207 drain
	// sentinel, computed once at construction. Empty means resolution FAILED and
	// was announced at start; it never means "no home", because an empty
	// ConfigHome resolves to the default home like any other.
	DrainSentinelPath  string
	ConfigHomeExplicit bool
	AgentLookup        func(context.Context, string) (db.Agent, error)
	AdapterFactory     func(runtime.Agent, string) (workflow.DeliveryAdapter, error)
	// adapterExecsDeclaredBinary is the daemon half of the #1817 discriminator
	// (ledger row gitmoot/gitmoot#1926-f5): it states that AdapterFactory builds a
	// REAL adapter which will exec the runtime's declared CLI binary.
	//
	// UNEXPORTED AND SET ONLY BY setRealAdapterFactory, and both properties are
	// load-bearing. A test that injects a fake assigns AdapterFactory directly, as
	// ~22 of them already do, so the flag keeps its zero value and the dispatch
	// stays exempt - which is why the 24-test over-block regression stays green
	// without a single test edit. Production goes through the setter, where the
	// factory and the claim about it cannot drift apart.
	//
	// Opt-in, because the unsafe direction is refusal: a production path that
	// forgot the setter degrades to the pre-#1817 late failure rather than
	// refusing work that would have run.
	adapterExecsDeclaredBinary bool
	// declaredRealAdapterPtr is the code pointer of the factory the claim above was
	// made about, so replacing AdapterFactory withdraws the claim automatically.
	declaredRealAdapterPtr uintptr
	StartAdapterFactory    func(execbackend.Backend, string, string) (runtime.Adapter, error)
	// ExecutionBackendFactory is nil on hand-built/test workers that intentionally
	// retain the pre-P2b host-only path. executionBackendJobWorker wires it for
	// real daemon jobs, where one lifecycle instance is acquired after runtime
	// admission and kept alive through every Mailbox delivery attempt. The factory
	// must consume the same config snapshot used for preflight.
	ExecutionBackendFactory func(execbackend.Backend, config.RemoteExecConfig) (execbackend.ExecutionBackend, error)
	// GITMOOT-IMPL: RemoteEnvdEndpointResolver is an offline-test seam. Production leaves it
	// nil and resolves envd from [remote_exec].e2b_domain or the provider response.
	RemoteEnvdEndpointResolver func(sandboxID string, port int) string
	// executionRunner is run-scoped state on jobWorker's value receiver. Runtime
	// adapter rebuilds reuse it so live logs cannot escape the backend.
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
	// ReviewAdmissionGitHubFactory is the current-head/check reader at the one
	// remote-review pre-provision boundary. Production leaves it nil.
	ReviewAdmissionGitHubFactory func(string) remoteReviewAdmissionGitHub
	// StaticReviewCIGitHubFactory reads exact-head check runs and job logs for
	// a no-execution review's staged CI evidence (#2364). Production leaves it
	// nil.
	StaticReviewCIGitHubFactory func(string) staticReviewCIGitHub
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
	// EventSinkOverride lets a test inject a recording events.Sink (#446). When
	// nil (production), eventSink() resolves the shared event-rule sink.
	EventSinkOverride events.Sink
	// RuntimePreflight evaluates the installed CLI contract immediately before
	// checkout/adapter construction. nil keeps hand-built test workers unchanged;
	// defaultJobWorker wires the process-wide identity cache.
	RuntimePreflight func(context.Context, runtime.Agent, runtime.RuntimeContractRequest) runtime.RuntimeContractResult
	// OwnerGram delivers wakes addressed to the owner role as Herdr Grams. Only
	// the daemon supervisors set it, on the worker they hand the wake drain loop
	// (sendOwnerGramViaHerdr); nil keeps owner wakes on the ordinary rule/pane
	// path, so tests never send a real Gram.
	OwnerGram ownerGramSender
}

// eventSink resolves the best-effort outbound event Sink (#446) for the
// worker's home, or nil when there is no store. It is the seam
// finishQueuedJob / handleRunJobError use to emit the DAEMON-owned terminal
// cases (pre-flight queued->failed/blocked and permission-blocked
// running->blocked) that never pass through the engine's Mailbox chokepoint. The
// underlying event-rule sink is cached per home, so this is a cheap cache hit
// on the hot path. A test override short-circuits resolution.
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
		return replyWakeDelivery{sink: w.EventSinkOverride, rules: rules, ownerGram: w.OwnerGram}, nil
	}
	return replyWakeDelivery{
		sink:      resolveDaemonEventSinkWithRules(w.Store, w.workflowHome(), rules),
		rules:     rules,
		ready:     messageRecipientReadiness(w.workflowHome()),
		loadOrg:   messageOrgLoader(w.workflowHome()),
		ownerGram: w.OwnerGram,
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
	// #1207: the scheduler's drain guard stats a sentinel under this home.
	//
	// THE PATH IS RESOLVED ONCE, HERE, AND CARRIED. The first version resolved it
	// on EVERY eligibility call, which put os.UserHomeDir() on the dispatch hot
	// path: a resolution error would abort listPendingQueuedJobs and stall EVERY
	// job on the box. That is a strictly larger blast radius than the bug it
	// replaced, and it is the same argument that chose a file sentinel over a
	// store row - a guard must not depend on the thing it is protecting.
	// Owner-ruled (#2096 round 3): resolve at start or degrade non-fatally.
	worker := jobWorker{Store: store, Stdout: serializeWrites(stdout), ConfigHome: configHome, ConfigHomeExplicit: configHomeExplicit}
	if sentinel, err := daemonDrainSentinelPath(configHome); err != nil {
		// NON-FATAL AND ANNOUNCED. A daemon that cannot resolve its own home is
		// already broken, but refusing all dispatch here would be the larger
		// failure. Say so once, loudly, at start.
		fmt.Fprintf(worker.Stdout, "warning: drain guard unavailable (%v); this daemon will keep claiming through a drain\n", err)
	} else {
		worker.DrainSentinelPath = sentinel
	}
	// PRODUCTION assigns through the setter so the factory and the claim that it
	// execs the declared binary are written together (#1926-f5).
	worker.setRealAdapterFactory(worker.defaultAdapter)
	worker.StartAdapterFactory = worker.defaultStartAdapter
	worker.RuntimePreflight = runtime.DefaultRuntimeContractChecker().CheckRequest
	recoverKillPendingAtWorkerStartup.Do(func() {
		if err := recoverKillPendingJobs(context.Background(), store, worker.Stdout); err != nil {
			writeLine(worker.Stdout, "job kill-pending recovery failed: %v", err)
		}
	})
	return worker
}

// executionBackendJobWorker is the production daemon constructor. Keeping the
// lifecycle opt-in at this boundary preserves defaultJobWorker as the existing
// unit-test/foreground seam: a hand-built worker has no backend instance.
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

// remoteExecutionJobTypes is the allowlist of job types a non-local execution
// backend will dispatch. Engine implement delivery is gone (#2586), so review
// is the only type a non-local backend runs. A review transports FINDINGS in
// its result envelope, which is the ordinary adapter return path.
//
// "ask" stays out deliberately. It has no worktree contract and no acceptance
// defined in #1529, so admitting it would be scope rather than capability.
var remoteExecutionJobTypes = []string{"review"}

func remoteExecutionSupportsJobType(jobType string) bool {
	jobType = strings.TrimSpace(jobType)
	for _, supported := range remoteExecutionJobTypes {
		if strings.EqualFold(jobType, supported) {
			return true
		}
	}
	return false
}

// resolveExecutionBackendForJob is the single job-aware normalization boundary
// for every backend consumer. Review dispatch is job-scoped: an older or
// manually-created review with no selector stays local even while process config
// names a remote backend; only an explicit payload override may move it off-host
// (#2234). run gives that override to a review of a repository with checks
// routing (routeReviewChecks, #2316) before calling this, whichever producer
// enqueued it. A remote job's provider is job-scoped too: the payload's
// exec_provider selects it, and absent means cloud E2B. The returned config is
// that provider's view, so admission, provisioning, envd routing, keepalive and
// teardown all use the provider the job was requested on.
func (w jobWorker) resolveExecutionBackendForJob(job db.Job, payload workflow.JobPayload) (execbackend.Backend, config.RemoteExecConfig, error) {
	name, present := payload.ExecBackendOverride()
	if strings.EqualFold(strings.TrimSpace(job.Type), "review") && !present {
		name = string(execbackend.Local)
		present = true
	}
	backend, cfg, err := daemonJobExecBackendFor(w, name, present)
	if err != nil {
		return backend, cfg, err
	}
	if backend != execbackend.Remote {
		if provider := strings.TrimSpace(payload.ExecProvider); provider != "" {
			return "", config.RemoteExecConfig{}, fmt.Errorf("exec_provider %q requires the remote execution backend, but job %s resolved to %s", provider, job.ID, backend)
		}
		if template := strings.TrimSpace(payload.ExecTemplate); template != "" {
			return "", config.RemoteExecConfig{}, fmt.Errorf("exec_template %q requires the remote execution backend, but job %s resolved to %s", template, job.ID, backend)
		}
		return backend, cfg, nil
	}
	cfg, err = cfg.ForProvider(payload.ExecProvider)
	if err != nil {
		return "", config.RemoteExecConfig{}, err
	}
	// A repository's checks_template (#2316) replaces this provider's
	// templates for this job only; every runtime uses it, because it is the
	// image that carries the repository's toolchain.
	if template := strings.TrimSpace(payload.ExecTemplate); template != "" {
		cfg.E2BTemplate = template
		cfg.E2BOMPTemplate = template
	}
	return backend, cfg, nil
}

// Refresh before preparation and again before admission. Runtime persistence
// changes payload JSON during preparation, so compare delivery identity instead.
func (w jobWorker) currentRemoteReviewDelivery(ctx context.Context, job db.Job, payload workflow.JobPayload) (db.Job, workflow.JobPayload, bool, error) {
	current, err := w.Store.GetJob(ctx, job.ID)
	if err != nil {
		return job, payload, false, err
	}
	if current.State != string(workflow.JobQueued) {
		return job, payload, false, nil
	}
	fresh, err := daemonJobPayload(current)
	if err != nil {
		return job, payload, false, err
	}
	backend, overridden := payload.ExecBackendOverride()
	freshBackend, freshOverridden := fresh.ExecBackendOverride()
	if current.Agent != job.Agent || fresh.Model != payload.Model ||
		fresh.RuntimeOverride != payload.RuntimeOverride ||
		fresh.RuntimeOverrideRef != payload.RuntimeOverrideRef ||
		fresh.HeadSHA != payload.HeadSHA ||
		freshBackend != backend || freshOverridden != overridden ||
		fresh.ExecProvider != payload.ExecProvider || fresh.ExecTemplate != payload.ExecTemplate {
		return job, payload, false, nil
	}
	return current, fresh, true, nil
}

func (w jobWorker) run(ctx context.Context, job db.Job) (returnErr error) {
	// Registered first so it runs last: after the capacity monitor stops and
	// every cleanup defer, a run this worker claimed must not stay running.
	exitGuard := beginWorkerExitGuard(job.ID)
	settleCtx := ctx
	// Every engine claim made under this frame (RunJob, the temp worker's
	// RunJob) reports its exact generation to the guard.
	ctx = workflow.WithClaimObserver(ctx, exitGuard.claimed)
	defer func() {
		panicked := recover()
		w.settleWorkerExit(settleCtx, exitGuard, returnErr, panicked)
		if panicked != nil {
			panic(panicked)
		}
	}()
	payload, err := daemonJobPayload(job)
	if err != nil {
		return w.finishQueuedJob(ctx, job, workflow.JobFailed, err)
	}
	job, payload, owned, err := w.routeReviewChecks(ctx, job, payload)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
		return nil
	}
	if !owned {
		return nil
	}
	// Resolve WHERE the runtime executes before any path can construct or start
	// an adapter. In particular, ephemeral jobs materialize and start a host
	// runtime below, so delaying this decision until after agent lookup would let
	// them bypass the backend boundary entirely.
	execBackend, execConfig, err := w.resolveExecutionBackendForJob(job, payload)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
		return nil
	}
	if execBackend != execbackend.Local && !remoteExecutionSupportsJobType(job.Type) {
		err := fmt.Errorf("%s jobs are not supported on the %s execution backend; supported types are %s", job.Type, execBackend, strings.Join(remoteExecutionJobTypes, ", "))
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
	sourceOnly := job.Type == "review" && execBackend == execbackend.Remote && !config.IsSandboxdProvider(execConfig.Provider)
	if job.Type == "review" && execBackend == execbackend.Remote {
		current, fresh, owned, refreshErr := w.currentRemoteReviewDelivery(ctx, job, payload)
		if refreshErr != nil {
			return refreshErr
		}
		if !owned {
			return nil
		}
		job, payload = current, fresh
	}
	if job.Type == "review" {
		paths, pathsErr := diskGuardPaths(w)
		if pathsErr != nil {
			return pathsErr
		}
		runCtx, stop, admissionErr := startReviewCapacityMonitor(ctx, w.Store, paths, payload.Repo, payload.HeadSHA, job.ID, sourceOnly, &job.LifecycleGeneration, payload.WorktreePath)
		if admissionErr != nil {
			return w.Store.AddJobEventIfAbsent(ctx, db.JobEvent{
				JobID: job.ID, Kind: janitorPressureHeldEventKind,
				Message: "held queued before worker preparation: " + admissionErr.Error(),
			})
		}
		ctx = runCtx
		// Registered before preparation's cleanup defers, so monitoring covers
		// the whole delivery and its provider/worktree cleanup.
		defer func() { returnErr = errors.Join(returnErr, stop()) }()
	}
	if sourceOnly && !payload.ReadOnlySeat {
		payload.ReadOnlySeat = true
		encoded, encodeErr := json.Marshal(payload)
		if encodeErr != nil {
			return encodeErr
		}
		updated, updateErr := w.Store.UpdateJobPayloadAtGeneration(ctx, job.ID, string(encoded), job.LifecycleGeneration)
		if updateErr != nil {
			return updateErr
		}
		if !updated {
			return nil
		}
		job.Payload = string(encoded)
	}
	dbAgent, err := w.lookupAgent(ctx, job.Agent)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, runtime.Agent{Name: job.Agent}, "", err)
		return nil
	}
	agent := runtimeAgent(dbAgent)
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
	runtimeConfigDir := ""
	if readOnlySeat {
		runtimeConfigDir = strings.TrimSpace(payload.RuntimeConfigDir)
	}
	// The runtime-session lock keeps naming the agent's REGISTERED session, so
	// #684's serialization is unchanged for a REGISTERED seat: it still queues
	// behind a busy reviewer session, and the scheduler gate
	// (queuedJobRuntimeResourceKey, which reads the stored agent for that case)
	// computes the same key the acquisition below does. Only DELIVERY moves to
	// the seat's isolated fresh session.
	//
	// #1952: this does NOT hold for an EPHEMERAL seat, and the unqualified
	// version of this sentence was false. Such a job has no registered session:
	// the gate keys it synthetically by job id and the worker locks the
	// materialized live session, so gate and acquisition deliberately differ.
	// The full explanation is on readOnlySeatRuntimeRef below.
	sessionLockAgent := agent
	if err := applyReadOnlySeat(readOnlySeat, runtimeConfigDir, job.ID, &agent); err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
		return nil
	}
	noExecution := false
	if job.Type == "review" || payload.ReviewNoExecution {
		// #2364: a no-execution review is requested on the job or declared by
		// the repository. The repository declaration is read HERE, on every
		// review whatever path queued it, so no dispatch route can skip it.
		// Applied before any adapter or sandbox runner is built from this
		// agent, and refused outright for a non-seat, an unsupported runtime,
		// backend or host rather than run with a weaker contract.
		var err error
		if paths, pathsErr := w.configPaths(); pathsErr != nil {
			err = pathsErr
		} else {
			noExecution, err = reviewRequiresNoExecution(paths, payload.Repo, payload.ReviewNoExecution)
		}
		if err == nil && noExecution {
			err = applyNoExecution(w.ConfigHome, readOnlySeat, &agent)
		}
		if err == nil && noExecution && execBackend != execbackend.Local {
			// The no-execution sandbox is the local Landlock wrapper; a remote
			// instance runs the runtime outside it.
			err = fmt.Errorf("no-execution review is not supported on the %s execution backend; it runs only on the local backend", execBackend)
		}
		if err != nil {
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
			return nil
		}
	}
	var absentBinaryRefusal error
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
		// #1926-f5: CAPTURE the absent-executable refusal here, where the contract
		// result is in scope, but ACT on it after the policy guards below. Ordering
		// is behavioural, not cosmetic: refusing here pre-empted the read-only
		// implement permission block, and CI caught it -
		// TestPreflightReadOnlyImplementEmitsJobBlocked got my capability message
		// where it required the permission one. A policy refusal is true whether or
		// not the binary exists, so it must win the diagnosis.
		absentBinaryRefusal = runtime.RuntimeContractAbsentBinaryError(agent, result)
		if noExecution && absentBinaryRefusal == nil {
			if err := runtime.OmpNoExecutionVersionError(result.Version); err != nil {
				if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
					return finishErr
				}
				_ = w.postJobResultComment(ctx, job.ID, agent, "", err)
				return nil
			}
		}
	} else if noExecution {
		// #2364: the omp build a no-execution seat runs must be a verified one,
		// and nothing else can say which build that is.
		err := errors.New("no-execution review refused: the runtime contract probe did not run, so the omp version cannot be verified")
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
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
		// genuine transition above, nil-safe when no event rule is enabled.
		emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, job.ID, daemonTerminalPermissionGuard, string(workflow.JobBlocked), agentPermissionBlockedMessage)
		_ = w.postJobResultComment(ctx, job.ID, agent, "", errors.New(agentPermissionBlockedMessage))
		writeLine(w.Stdout, "job %s blocked: %s", job.ID, agentPermissionBlockedMessage)
		return nil
	}
	// #1926-f5: THE SECOND SEAM, ACTED ON HERE - after the policy guards above, and
	// BEFORE review-worktree allocation on the next line and adapter construction
	// below. That is the ordering the ruling asks for: a delegated or
	// daemon-created job whose declared CLI is absent is refused before it spends
	// a worktree, rather than dying at exec.
	//
	// Only when this worker's factory was declared REAL through
	// setRealAdapterFactory, so an injected fake stays exempt by construction, and
	// only for a local backend, since the preflight that produced this result was
	// itself routed by execbackend.Consume. The `unknown` classification and its
	// event are untouched.
	if absentBinaryRefusal != nil && w.adapterIsDeclaredReal() {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobBlocked, absentBinaryRefusal); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, "", absentBinaryRefusal)
		return nil
	}
	nativeReviewDeliveryStarted := false
	if !sourceOnly {
		payload, err = w.prepareNativeReviewWorktreeForRunner(ctx, job, payload, jobRunner)
	}
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
		// The narrowing is typed and pre-delivery only; see
		// deferPreDeliveryAllocationContention's own comment for why that is the
		// safe boundary.
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
	var checkout string
	if !sourceOnly {
		checkout, err = w.checkoutForJob(ctx, job, payload, agent, jobRunner)
	}
	if err != nil {
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
	deliveryCheckout := checkout
	adapter, err := w.buildJobAdapterForBackend(execBackend, agent, checkout)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	managed, err := w.managedJobConfig(ctx, agent.Name)
	if err != nil {
		if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	timeoutResolution := resolveEffectiveJobTimeout(payload, managed, job.Type)
	jobTimeout := timeoutResolution.Timeout
	// The class floor fell back to its built-in because the operator's
	// [review_router] could not be read. Reported on the REVIEW it affects, so
	// a deliberately shorter class deadline silently becoming longer is
	// visible where the consequence lands (#2192 review).
	if reason := strings.TrimSpace(managed.ReviewClassFallbackReason); reason != "" && isReviewJob(payload, job.Type) {
		message := fmt.Sprintf("[review_router] unreadable (%s); using the built-in review class deadline %s", reason, managed.ReviewJobTimeout)
		// CLAIM, not if-absent: AddJobEventIfAbsent dedups on (job_id, kind), so a
		// retry whose config failed differently would disclose NOTHING - the
		// first reason spoken and every later distinct reason silent. Claiming on
		// (job_id, kind, message) still collapses an identical repeat while
		// keeping every DISTINCT value visible (#2192 review, P3). Same defect
		// this advisory was added to fix, one level up at the disclosure layer.
		if _, eventErr := w.Store.ClaimJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "review_class_deadline_default", Message: message}); eventErr != nil {
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, eventErr); finishErr != nil {
				return finishErr
			}
			return nil
		}
	}
	if invalid := strings.TrimSpace(timeoutResolution.InvalidPayload); invalid != "" {
		message := fmt.Sprintf("job payload job_timeout %q is not a positive duration; using %s from %s instead",
			invalid, timeoutResolution.Timeout, timeoutResolution.Source)
		// Claimed on the MESSAGE, which carries the offending value, so a retry
		// carrying a different invalid timeout is not swallowed (#2192 review).
		if _, eventErr := w.Store.ClaimJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "job_timeout_payload_invalid", Message: message}); eventErr != nil {
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobFailed, eventErr); finishErr != nil {
				return finishErr
			}
			return nil
		}
	}
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
		releaseLock, acquired, lockKey, ownerToken, err = acquireJobRuntimeSessionLock(ctx, w.Store, job.ID, sessionLockAgent, overridden, time.Now().UTC(), lockTTL)
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
			return w.runWithTempWorker(ctx, job, payload, execBackend, agent, checkout, policy, eligibleMessage)
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
	// Runtime carried STRUCTURALLY beside the prose (#1534): see the sibling
	// write in agent_dispatch.go. Both run sites must set it or the append-only
	// tier is only half installed, which is the "correct where installed, absent
	// at the next call site" shape #1531 warned about.
	if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: jobRuntimeEventKind(overridden), Message: jobRuntimeOverrideEventMessage(defaultRuntime, agent, lockKey), Runtime: agent.Runtime}); eventErr != nil {
		writeLine(w.Stdout, "job %s effective-runtime event failed: %v", job.ID, eventErr)
	}
	// #2245: remote review admission must reason about the job's CURRENT
	// lifecycle generation, and its refusal must close that same generation.
	// The snapshot this function was handed can predate a re-queue: a review
	// model fallback moves running -> queued, which bumps lifecycle_generation,
	// and a worker holding the pre-bump row then refused "cloud attempt consumed
	// for lifecycle_generation=0" against a row already at 1. The refusal's
	// generation-anchored CAS matched nothing, afterQueuedJobTransition treated
	// transitioned=false as success, and the job re-entered the queue forever:
	// measured on local-review-review-router-18d71359f53e810a-1, 2420
	// runtime_override and 152 admission-refusal events, no terminal event, until
	// an operator cancelled it an hour later. Re-reading here makes admission and
	// its refusal agree on one generation.
	if execBackend == execbackend.Remote && strings.EqualFold(strings.TrimSpace(job.Type), "review") {
		current, freshPayload, owned, err := w.currentRemoteReviewDelivery(ctx, job, payload)
		if err != nil {
			return err
		}
		if !owned || current.LifecycleGeneration != job.LifecycleGeneration {
			// A requeue during preparation belongs to a fresh worker, not the
			// generation whose preparation and cancellation we currently own.
			return nil
		}
		payload = freshPayload
		job = current
	}
	remoteReviewAdmitted, admissionErr := w.admitRemoteReview(ctx, job, payload, agent, execBackend, checkout, jobRunner)
	if admissionErr != nil {
		if errors.Is(admissionErr, errRemoteReviewAdmissionCancelled) {
			return nil
		}
		return w.recordRemoteReviewAdmissionRefusal(ctx, job, admissionErr)
	}
	runCtx := ctx
	stopRun := func() {}
	if remoteReviewAdmitted {
		// Admission claimed exactly this generation (ClaimRunningJobAtGeneration).
		exitGuard.claimed(job.ID, job.LifecycleGeneration)
		// Admission owns the queued->running claim, so cancellation observation
		// must begin before the cost ledger or provider can see this job.
		runCtx, stopRun = w.runningJobContext(ctx, job.ID)
		defer stopRun()
	}
	// Acquire the execution-backend lifecycle only after checkout validation and
	// runtime-session admission. The instance then survives every Mailbox repair
	// delivery and is destroyed synchronously on every return path. Host checkout,
	// git, observation, and finalization remain on checkout/jobRunner; only runtime
	// delivery executes in the distinct backend workspace.
	provisionStartedAt := time.Now()
	lifecycle, instance, credentialLease, credentialEnv, lifecycleErr := w.provisionExecutionBackend(runCtx, execBackend, execConfig, agent.Runtime, job, jobTimeout+runtimeLeaseTeardownGrace, checkout)
	if instance != nil {
		defer w.destroyExecutionBackend(job.ID, lifecycle, instance)
	}
	if credentialLease != nil {
		defer credentialLease.Revoke()
	}
	if lifecycleErr != nil {
		w.recordRetryableRemoteProviderFailure(ctx, job, lifecycleErr)
		if remoteReviewAdmitted {
			requeued, expired := w.waitForRemoteReviewCapacity(ctx, job, execConfig.Provider, jobTimeout, lifecycleErr)
			if requeued {
				return nil
			}
			if expired != nil {
				lifecycleErr = expired
			}
		}
		if finishErr := w.finishPreDeliveryJob(ctx, job, remoteReviewAdmitted, workflow.JobFailed, lifecycleErr); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, lifecycleErr)
		return nil
	}
	if remoteReviewAdmitted && payload.RemoteCapWaitSince != "" {
		w.clearRemoteReviewCapWait(ctx, job, execConfig.Provider)
	}
	var sandboxRunDeadline time.Time
	if lifecycle != nil && instance != nil {
		deliveryCheckout = instance.Workspace
		// A cloud E2B sandbox has a fixed lifetime, so the run deadline has to
		// end inside it: a review that outlives its sandbox dies mid-stream with
		// no verdict instead of timing out cleanly (#2331).
		sandboxLifetime := remoteSandboxLifetime(execBackend, execConfig)
		sandboxRunDeadline = remoteSandboxRunDeadline(sandboxLifetime, provisionStartedAt)
		w.executionRunner = execbackend.InstanceRunner{Backend: lifecycle, Instance: instance, Lifetime: sandboxLifetime, Started: provisionStartedAt}
		if len(credentialEnv) > 0 {
			w.executionRunner = subprocess.EnvInjectingRunner{Inner: w.executionRunner, Env: credentialEnv}
		}
		adapter, err = w.executionDeliveryAdapter(agent, deliveryCheckout)
		if err != nil {
			if finishErr := w.finishPreDeliveryJob(ctx, job, remoteReviewAdmitted, workflow.JobFailed, err); finishErr != nil {
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
			if finishErr := w.finishPreDeliveryJob(ctx, job, remoteReviewAdmitted, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
			return nil
		}
		writeLine(w.Stdout, "job %s tool cache grant failed: %v", job.ID, toolCacheErr)
	}
	// Related pull request trees (#2509): a local review gets them inside its own
	// worktree; a remote E2B review got them at provisioning.
	removeRelatedTrees := func() {}
	if execBackend == execbackend.Local && len(payload.ReviewRelatedPRs) > 0 && strings.EqualFold(strings.TrimSpace(job.Type), "review") {
		remove, relatedErr := w.installLocalReviewRelatedPRs(ctx, job.ID, payload, deliveryCheckout)
		if relatedErr != nil {
			err := fmt.Errorf("prepare related pull request trees: %w", relatedErr)
			if finishErr := w.finishPreDeliveryJob(ctx, job, remoteReviewAdmitted, workflow.JobFailed, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
			return nil
		}
		removeRelatedTrees = remove
		defer removeRelatedTrees()
	}
	// THE HOST READ-ONLY SEAT IS A HOST-BOUNDARY CONTROL, so it does not apply
	// when the runtime executes off-box. Its Landlock grants restrict what a
	// review may touch in the HOST filesystem; a remote job never runs on the
	// host, and its isolation boundary is the execution instance rather than
	// anything Landlock can express here.
	//
	// THE OBSERVED FAILURE CAME VIA THE NIL-FACTORY PATH, which is worth stating
	// precisely because the provisioned path would have been WORSE. When no
	// lifecycle factory is configured the adapter here is still
	// unprovisionedRemoteDeliveryAdapter, which wrapReadOnlyAdapterRunner cannot
	// type-switch, so the job died with "read-only Landlock sandbox cannot wrap
	// ... unprovisionedRemoteDeliveryAdapter". On the PROVISIONED path the
	// lifecycle has already rebuilt the adapter above, so the wrap would have
	// SUCCEEDED and rewritten every remote command into a host sandbox-exec
	// invocation - silently breaking delivery instead of refusing. Round 1 review
	// of #2226 corrected this description.
	//
	// The zero readOnlySeatSetup is already the supported "nothing to clean up"
	// value: wrapReadOnlySandboxAdapter returns exactly that for any agent
	// without ReadOnlySeat.
	var seatSetup readOnlySeatSetup
	if execBackend == execbackend.Local {
		adapter, seatSetup, err = wrapReadOnlySandboxAdapter(w.ConfigHome, agent, deliveryCheckout, payload.Repo, adapter)
	}
	// Registered before any early return below, so a seat refused for an
	// unavailable runtime or toolchain still has its cache root and private
	// temp dir removed (#2314).
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
	if len(seatSetup.dropped) > 0 {
		// Narrowing is not silent: a reviewer whose MCP tool is missing, or a
		// seat that cannot authenticate to a provider whose key was withheld,
		// can find out why from the job's own event log.
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "read_only_seat_config_narrowed", Message: "withheld from the seat's staged config: " + strings.Join(seatSetup.dropped, ", ")}); eventErr != nil {
			return eventErr
		}
	}
	if err != nil {
		if finishErr := w.finishPreDeliveryJob(ctx, job, remoteReviewAdmitted, workflow.JobFailed, err); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, err)
		return nil
	}
	if noExecution {
		// #2364: the seat must be the no-execution read-only wrapper; anything
		// else would run the runtime without the exec supervisor.
		stateAdapter, ok := adapter.(readOnlyRuntimeAdapter)
		var briefErr error
		if !ok {
			briefErr = fmt.Errorf("no-execution review requires the local read-only seat, got %T", adapter)
		} else {
			var client staticReviewCIGitHub
			client, briefErr = w.staticReviewCIClient(checkout, jobRunner)
			if briefErr == nil {
				evidenceCtx, cancelEvidence := context.WithTimeout(ctx, 60*time.Second)
				stateAdapter.staticReviewBrief = staticReviewBrief(staticReviewEvidence(evidenceCtx, client, payload.Repo, payload.HeadSHA))
				cancelEvidence()
				adapter = stateAdapter
			}
		}
		if briefErr != nil {
			if finishErr := w.finishPreDeliveryJob(ctx, job, remoteReviewAdmitted, workflow.JobFailed, briefErr); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, job.ID, agent, checkout, briefErr)
			return nil
		}
	}
	// #1817: THE SEAT'S OWN RUNTIME IS PUBLISHED UNAVAILABLE, SO REFUSE HERE -
	// after the adapter exists, so the real-adapter discriminator can be asked,
	// and before MarkAgentInstanceRunning and any delivery.
	//
	// BLOCKED, NOT FAILED. Every other error out of seat setup means something
	// went wrong while preparing this job, so failed is right. This one means
	// the job was fine and the capability it needs is absent - the same
	// distinction RuntimeContractDispatchError already draws above - and all ten
	// measured instances recorded `failed`, which reads as "this reviewer tried"
	// and invites the identical re-dispatch.
	//
	// GATED ON adapterIsDeclaredReal for the same reason the absent-binary
	// refusal is: an injected adapter never execs the runtime, so an absent one
	// is not its precondition. Measured, not reasoned - refusing unconditionally
	// failed five existing tests that compose a seat adapter and exec nothing.
	if seatSetup.runtimeUnavailable != "" && w.adapterIsDeclaredReal() {
		refusal := error(&seatRuntimeCapabilityError{agentName: agent.Name, runtimeName: agent.Runtime, cause: seatSetup.runtimeUnavailable})
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: seatRuntimeUnavailableEvent, Message: refusal.Error()}); eventErr != nil {
			writeLine(w.Stdout, "job %s %s event failed: %v", job.ID, seatRuntimeUnavailableEvent, eventErr)
		}
		if finishErr := w.finishPreDeliveryJob(ctx, job, remoteReviewAdmitted, workflow.JobBlocked, refusal); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, refusal)
		return nil
	}
	// #1817: THE SAME QUESTION FOR THE TOOL A REVIEW ACTUALLY RUNS. Measured
	// instance: local-review-gm-review-opus-18d3b8e6790b3231-1 reviewed
	// gitmoot/gitmoot#2066 at its exact head, returned APPROVED, and its whole
	// tests_run read "go test ./internal/workflow (attempted): could not run
	// ... `go` is a stub that exits 126". The seat's own runtime staged fine,
	// so the sibling arm above stayed silent, no event named the failure, and
	// the verdict satisfied an exact-head review gate having executed nothing.
	//
	// SCOPED TO REVIEW, not to every seat, because the un-evented staging miss
	// is a deliberate choice with a named cost: an event here would make a
	// job's event sequence depend on host disk state. That reason holds for
	// ask and implement; it does not hold for the one action whose contract IS
	// to execute the repository's gate.
	if job.Type == "review" && seatSetup.toolchainUnavailable != "" && w.adapterIsDeclaredReal() {
		refusal := error(&seatToolchainCapabilityError{agentName: agent.Name, cause: seatSetup.toolchainUnavailable})
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: seatToolchainUnavailableEvent, Message: refusal.Error()}); eventErr != nil {
			writeLine(w.Stdout, "job %s %s event failed: %v", job.ID, seatToolchainUnavailableEvent, eventErr)
		}
		if finishErr := w.finishPreDeliveryJob(ctx, job, remoteReviewAdmitted, workflow.JobBlocked, refusal); finishErr != nil {
			return finishErr
		}
		_ = w.postJobResultComment(ctx, job.ID, agent, checkout, refusal)
		return nil
	}
	if execBackend == execbackend.Local && len(toolCacheEnv) > 0 {
		if envAdapter, envErr := injectDeliveryAdapterEnv(adapter, toolCacheEnv); envErr != nil {
			writeLine(w.Stdout, "job %s tool cache env inject failed: %v", job.ID, envErr)
		} else {
			adapter = envAdapter
		}
	}
	// Default-on retained capture is attached to the already-composed adapter so
	// relay env, credential curation, gateway leases, and Landlock all survive.
	// Any open/composition failure is fail-open.
	retainedLogFile, retainedLogErr := openRetainedTranscriptLog(w.ConfigHome, job.ID, job.Type, effectiveTranscriptRuntime(payload, agent), job.LifecycleGeneration, w.Store)
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

	if managed.Instance {
		if err := w.Store.MarkAgentInstanceRunning(ctx, agent.Name, time.Now().UTC(), jobTimeout); err != nil {
			if finishErr := w.finishPreDeliveryJob(ctx, job, remoteReviewAdmitted, workflow.JobFailed, err); finishErr != nil {
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
	adapter = wrapManagedWorktreeRuntimeEnv(payload, adapter)
	if execBackend == execbackend.Remote {
		adapter = w.guardRemoteReviewEvidence(adapter, lifecycle, instance, job.ID, payload)
		adapter = w.guardRemoteReviewRelatedPRs(adapter, lifecycle, instance, job.ID, payload)
	}
	engine := w.workflowForJob(checkout, jobRunner)
	// Wire the PRE-TERMINAL operational-blocker deferrer (#532 slice E) on the LIVE
	// worker (not the WorkflowFactory-captured copy) so it observes this worker's
	// EventSink for the first-class job.deferred emit. When a delivery-seam failure
	// classifies as a retryable operational blocker the mailbox re-queues the job
	// BEFORE the terminal transition, so no job.failed reaches the event sink.
	engine.BlockerDeferrer = w.deferOperationalBlockerPreTerminal
	if !remoteReviewAdmitted {
		runCtx, stopRun = w.runningJobContext(ctx, job.ID)
		defer stopRun()
	}
	runStartedAt := time.Now().UTC()
	var cancel context.CancelFunc
	runCtx, cancel = context.WithDeadline(runCtx, w.runDeadline(ctx, job.ID, jobTimeout, sandboxRunDeadline))
	defer cancel()
	runDeadline, hasRunDeadline := runCtx.Deadline()
	stopKillPending := func() {}
	if hasRunDeadline {
		stopKillPending = armJobKillPending(w.Store, job.ID, runDeadline)
	}
	nativeReviewDeliveryStarted = true
	if remoteReviewAdmitted {
		_, err = engine.RunClaimedJob(runCtx, job.ID, agent, adapter)
	} else {
		_, err = engine.RunJob(runCtx, job.ID, agent, adapter)
	}
	err = w.recordUnrecordedResult(ctx, engine, err)
	removeRelatedTrees()
	stopKillPending()
	if readOnlyState != nil {
		err = errors.Join(err, readOnlyState.cleanup())
		readOnlyStateCleaned = true
	}
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
		// A sandbox retired at the end of its fixed lifetime leaves only the
		// runtime's "stream ended" symptom in err; lead with the cause instead,
		// on the job's events and in its PR comment.
		var lifetimeErr *execbackend.SandboxLifetimeExceededError
		if errors.As(err, &lifetimeErr) {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: sandboxTTLExceededEventKind, Message: lifetimeErr.Error()}); eventErr != nil {
				writeLine(w.Stdout, "job %s %s event failed: %v", job.ID, sandboxTTLExceededEventKind, eventErr)
			}
			commentErr = lifetimeErr
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
	if clearErr := clearRoleUnavailableOnSuccess(ctx, w.Store, payload.ActingOrgRole, agent.Runtime); clearErr != nil {
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

const maxReadOnlyRuntimeStateFileBytes = 1 << 20

type readOnlySandboxGrants struct {
	// dropped names what narrowing withheld from the staged config, so the
	// caller can record it. A seat that silently starts without its MCP
	// servers gives the reviewer no way to learn why a tool is missing.
	dropped   []string
	reads     []string
	readFiles []string
	writes    []string
	env       []string
	cacheRoot string
	stateDir  string
	// tempDir is the seat's private /tmp/gmr-<hex> TMPDIR (#2314), a write
	// grant of its own outside cacheRoot, or, when no short parent is
	// writable, <cacheRoot>/tmp. Either is removed at the end of the job.
	tempDir seatTempDir
	// evidenceFile is the rendered, repo-scoped list of prior verdicts, or ""
	// when none could be staged. It is inside cacheRoot, so it needs no grant
	// of its own.
	evidenceFile string
	// runtimeUnavailable names why the seat's OWN runtime resolves to the
	// engine's exit-126 unavailable command, or "" when it staged normally
	// (#1817). It is reported rather than returned as an error because whether
	// it MATTERS depends on the caller: only a caller that will actually exec
	// the runtime is entitled to refuse on it. Erroring here refused five
	// existing tests that legitimately compose a seat adapter and never exec
	// anything, and it would refuse every claude seat in CI, where no runtime
	// CLI is installed at all.
	runtimeUnavailable string
	// toolchainUnavailable names why the seat's staged Go toolchain is absent,
	// or "" when it staged normally. Same reporting rule as the field above and
	// the same reason: seat setup does not know whether this job's contract
	// requires executing anything, and the staging miss is an operator-visible
	// fact about the host rather than a defect in the job.
	toolchainUnavailable string
}

type readOnlyOmpBrokerSession struct {
	mu       sync.Mutex
	config   readOnlyOmpBrokerConfig
	stateDir string
	broker   *readOnlyOmpBrokerProxy
}

func (s *readOnlyOmpBrokerSession) prepare(model string) (string, error) {
	provider, err := runtime.OmpModelProvider(model)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broker != nil {
		if provider != s.broker.provider {
			return "", fmt.Errorf("read-only omp job model provider %q is outside the broker scope %q", provider, s.broker.provider)
		}
		return provider, nil
	}
	broker, err := startReadOnlyOmpBrokerProxy(s.config, model)
	if err != nil {
		return "", err
	}
	if err := broker.writeConfig(s.stateDir); err != nil {
		return "", errors.Join(err, broker.close())
	}
	s.broker = broker
	return provider, nil
}

func (s *readOnlyOmpBrokerSession) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broker == nil {
		return nil
	}
	err := s.broker.close()
	s.broker = nil
	return err
}

type readOnlyRuntimeAdapter struct {
	runtime.Adapter
	cleanupRoot string
	cleanupTemp seatTempDir
	// cleanupLaunch is a no-execution seat's launch dir (#2563), made under
	// the daemon-only run dir for this job and removed with it.
	cleanupLaunch string
	ompSession    *readOnlyOmpBrokerSession
	// staticReviewBrief is appended to the prompt of a no-execution review
	// (#2364): the statement that this seat cannot run anything, and the
	// exact-head CI evidence the host fetched for it.
	staticReviewBrief string
}

func (a readOnlyRuntimeAdapter) Deliver(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	if a.ompSession != nil {
		model := runtime.EffectiveModel(agent, job)
		provider, err := runtime.OmpModelProvider(model)
		if err != nil {
			return runtime.Result{}, err
		}
		if job.Plan {
			planProvider, err := runtime.OmpModelProvider(job.PlanInto)
			if err != nil {
				return runtime.Result{}, fmt.Errorf("read-only omp plan execution model: %w", err)
			}
			if planProvider != provider {
				return runtime.Result{}, fmt.Errorf("read-only omp plan execution provider %q is outside the broker scope %q", planProvider, provider)
			}
		}
		if _, err := a.ompSession.prepare(model); err != nil {
			return runtime.Result{}, err
		}
	}
	if a.staticReviewBrief != "" {
		job.Prompt = strings.TrimRight(job.Prompt, "\n") + "\n\n" + a.staticReviewBrief
	}
	// Mailbox repair turns reuse this adapter. Keep its isolated credentials and
	// session state until RunJob returns; the worker owns job-boundary cleanup.
	return a.Adapter.Deliver(ctx, agent, job)
}

func (a readOnlyRuntimeAdapter) cleanup() error {
	var cleanupErrors []error
	if a.ompSession != nil {
		if err := a.ompSession.close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("stop read-only omp broker: %w", err))
		}
	}
	// The prompt can move or rewrite anything under the job-private writable
	// cache. Delete that entire root so renamed credentials cannot escape cleanup;
	// never synchronize untrusted state back to the shared runtime profile.
	if a.cleanupRoot != "" {
		if err := os.RemoveAll(a.cleanupRoot); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove read-only seat cache: %w", err))
		}
	}
	if err := a.cleanupTemp.remove(); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if a.cleanupLaunch != "" {
		if err := os.RemoveAll(a.cleanupLaunch); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove no-execution seat launch dir: %w", err))
		}
	}
	return errors.Join(cleanupErrors...)
}

// reviewRepo is the repo of the JOB being run, which is what the seat's
// evidence must be scoped to. It is distinct from agent.RepoScope: a seat's
// registered scope is an authorisation boundary, not a statement about the job
// in hand, and nothing in the tree makes the two agree.
func wrapReadOnlySandboxAdapter(home string, agent runtime.Agent, checkout string, reviewRepo string, adapter workflow.DeliveryAdapter) (workflow.DeliveryAdapter, readOnlySeatSetup, error) {
	if !agent.ReadOnlySeat {
		return adapter, readOnlySeatSetup{}, nil
	}
	// Resolve credentials before creating or publishing any seat state. OMP
	// reads its upstream token from an owner-only file now, but starts its
	// provider-filtered proxy only when Deliver has the job's effective model.
	var (
		ompConfig *readOnlyOmpBrokerConfig
		launchDir string
	)
	if agent.Runtime == runtime.OmpRuntime {
		config, configErr := readOnlySeatOmpBrokerConfig(os.Environ())
		if configErr != nil {
			return nil, readOnlySeatSetup{}, configErr
		}
		ompConfig = &config
	}
	grants, err := readOnlyRuntimeSandboxGrants(home, agent, checkout, reviewRepo)
	if err != nil {
		return nil, readOnlySeatSetup{}, err
	}
	if agent.NoExecution {
		// The omp no-execution overlay is read by the runtime at startup; it is
		// data, never executable under the no-execution ruleset. It is ALSO
		// named in PI_CONFIG_FILES so no other source can claim that variable.
		// The launch dir is an empty job-private directory the runtime starts
		// in, so it never reads the checkout's .env (the checkout is passed
		// to omp with --cwd instead). It is created below, outside the cache
		// root, because the seat can write the cache root (#2563).
		//
		// The cache root is keyed by worktree path and is WRITABLE by every
		// seat that used it, including earlier seats that could run commands.
		// omp reads .env and user settings from HOME and the XDG roots at
		// startup, so each of those is recreated empty rather than trusted.
		if strings.TrimSpace(agent.NoExecutionConfig) == "" {
			cleanupErr := errors.Join(os.RemoveAll(grants.cacheRoot), grants.tempDir.remove())
			return nil, readOnlySeatSetup{}, errors.Join(errors.New("no-execution seat has no runtime settings overlay"), cleanupErr)
		}
		grants.readFiles = append(grants.readFiles, agent.NoExecutionConfig)
		grants.env = append(grants.env, "PI_CONFIG_FILES="+agent.NoExecutionConfig)
		for _, dir := range []string{"home", "xdg-config", "xdg-data", "xdg-state", "xdg-cache"} {
			dir = filepath.Join(grants.cacheRoot, dir)
			if err := errors.Join(os.RemoveAll(dir), os.MkdirAll(dir, 0o700)); err != nil {
				cleanupErr := errors.Join(os.RemoveAll(grants.cacheRoot), grants.tempDir.remove())
				return nil, readOnlySeatSetup{}, errors.Join(fmt.Errorf("reset no-execution seat dir %q: %w", dir, err), cleanupErr)
			}
		}
	}
	setup := readOnlySeatSetup{dropped: grants.dropped, runtimeUnavailable: grants.runtimeUnavailable, toolchainUnavailable: grants.toolchainUnavailable}
	var ompSession *readOnlyOmpBrokerSession
	if ompConfig != nil {
		if grants.stateDir == "" {
			cleanupErr := errors.Join(os.RemoveAll(grants.cacheRoot), grants.tempDir.remove())
			return nil, setup, errors.Join(errors.New("read-only omp broker requires isolated runtime state"), cleanupErr)
		}
		ompSession = &readOnlyOmpBrokerSession{config: *ompConfig, stateDir: grants.stateDir}
	}
	if agent.NoExecution {
		launchDir, err = createNoExecutionLaunchDir(home, grants.writes)
		if err != nil {
			cleanupErr := errors.Join(os.RemoveAll(grants.cacheRoot), grants.tempDir.remove())
			return nil, setup, errors.Join(err, cleanupErr)
		}
		// Read-only: omp reads its start directory (.env, project settings)
		// and finds it empty; sandbox-exec grants the working directory no
		// access of its own once the seat names explicit reads.
		grants.reads = append(grants.reads, launchDir)
	}
	// A seat's env is rebuilt from scratch. OMP's proxy token exists only in
	// its job-private config; its upstream bearer, operator profile, and
	// ambient provider keys remain outside the child environment.
	wrap := func(runner subprocess.Runner) subprocess.Runner {
		baseEnv := readOnlyRuntimeBaseEnv(os.Environ(), filepath.Join(grants.cacheRoot, "gh"))
		// The launch dir rides on the same graft: on an execution-backend
		// instance the backend starts a command outside the workspace only in
		// a launch dir the runner names (#2563).
		curated := graftRuntimeBaseRunner(runner, subprocess.CuratedGroupRunner{BaseEnv: baseEnv}, launchDir)
		if agent.NoExecution {
			return landlockNoExecutionRunner(curated, grants.reads, grants.readFiles, grants.writes, grants.env)
		}
		return landlockReadOnlyRunner(curated, grants.reads, grants.readFiles, grants.writes, grants.env)
	}
	wrapped, err := wrapReadOnlyAdapterRunner(agent.Runtime, adapter, grants.stateDir, wrap)
	if err == nil && agent.NoExecution {
		wrapped, err = withOmpNoExecutionLaunchDir(wrapped, launchDir)
	}
	if err != nil {
		// Staging and narrowing are already done at this point, so the
		// withheld list is reported even though the wrap failed.
		cleanupErr := errors.Join(os.RemoveAll(grants.cacheRoot), grants.tempDir.remove(), os.RemoveAll(launchDir))
		return nil, setup, errors.Join(err, cleanupErr)
	}
	runtimeAdapter, ok := wrapped.(runtime.Adapter)
	if !ok {
		// The narrowing already happened, so report it even though delivery
		// cannot be built: a withheld credential is news whether or not the
		// wrap succeeds.
		cleanupErr := errors.Join(os.RemoveAll(grants.cacheRoot), grants.tempDir.remove(), os.RemoveAll(launchDir))
		return nil, setup, errors.Join(fmt.Errorf("read-only Landlock sandbox returned incompatible %T adapter", wrapped), cleanupErr)
	}
	// A seat without isolated runtime state (a shell seat) keeps its cache
	// root as before, but its temp dir is still job-scoped, so it is wrapped
	// for cleanup too (#2314).
	cleanupRoot := grants.cacheRoot
	if grants.stateDir == "" {
		cleanupRoot = ""
	}
	return readOnlyRuntimeAdapter{
		Adapter:       runtimeAdapter,
		cleanupRoot:   cleanupRoot,
		cleanupTemp:   grants.tempDir,
		cleanupLaunch: launchDir,
		ompSession:    ompSession,
	}, setup, nil
}

func wrapReadOnlyAdapterRunner(runtimeName string, adapter workflow.DeliveryAdapter, stateDir string, wrap func(subprocess.Runner) subprocess.Runner) (workflow.DeliveryAdapter, error) {
	switch a := adapter.(type) {
	case runtime.OmpAdapter:
		a.Runner = wrap(a.Runner)
		return a, nil
	case *runtime.OmpAdapter:
		a.Runner = wrap(a.Runner)
		return a, nil
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

// withOmpNoExecutionLaunchDir points an omp adapter at the neutral directory a
// no-execution seat starts in. Any other adapter is refused: only omp is a
// supported no-execution runtime, and a seat that cannot be given its launch
// dir must not run from the checkout instead.
func withOmpNoExecutionLaunchDir(adapter workflow.DeliveryAdapter, launchDir string) (workflow.DeliveryAdapter, error) {
	switch a := adapter.(type) {
	case runtime.OmpAdapter:
		a.LaunchDir = launchDir
		return a, nil
	case *runtime.OmpAdapter:
		copied := *a
		copied.LaunchDir = launchDir
		return &copied, nil
	default:
		return nil, fmt.Errorf("no-execution seat cannot run on adapter %T; only omp is supported", adapter)
	}
}

// noExecutionLaunchParent is the daemon-only directory no-execution launch
// dirs are made in, beside the other runtime state under <home>/run.
func noExecutionLaunchParent(paths config.Paths) string {
	return filepath.Join(paths.Home, "run", "noexec-launch")
}

// createNoExecutionLaunchDir makes a fresh, empty, mode-0700 launch dir for a
// no-execution seat (#2563). Its parent is owned by the daemon user, mode
// 0700, and must not overlap any path the seat may write: a launch dir the
// seat (or a concurrent seat sharing its cache root) could write could be
// filled or swapped between the backend's check and the runtime's chdir.
// The seat is granted the launch dir read-only.
func createNoExecutionLaunchDir(home string, writes []string) (string, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return "", fmt.Errorf("resolve no-execution launch dir paths: %w", err)
	}
	parent := noExecutionLaunchParent(paths)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create no-execution launch parent: %w", err)
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect no-execution launch parent: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("no-execution launch parent %q is not a directory", parent)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return "", fmt.Errorf("no-execution launch parent %q is not owned by the daemon user", parent)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(parent, 0o700); err != nil {
			return "", fmt.Errorf("restrict no-execution launch parent: %w", err)
		}
	}
	resolvedParent, err := resolvePathForContainment(parent)
	if err != nil {
		return "", fmt.Errorf("resolve no-execution launch parent: %w", err)
	}
	for _, write := range compactCleanPaths(writes) {
		resolvedWrite, err := resolvePathForContainment(write)
		if err != nil {
			return "", fmt.Errorf("resolve seat write path %q: %w", write, err)
		}
		if pathsOverlap(resolvedParent, resolvedWrite) {
			return "", fmt.Errorf("no-execution launch parent %q overlaps seat-writable path %q", parent, write)
		}
	}
	dir, err := os.MkdirTemp(parent, "seat-")
	if err != nil {
		return "", fmt.Errorf("create no-execution launch dir: %w", err)
	}
	return dir, nil
}

// applyReadOnlySeat rewrites an agent for hard runtime isolation.
//
// SESSION SAFETY: prepareReadOnlyRuntimeState builds the seat's runtime state
// dir EMPTY (RemoveAll, MkdirAll, then only the credential file is staged)
// and points CLAUDE_CONFIG_DIR/CODEX_HOME at it. An isolated home therefore can
// never contain the session history a concrete ref names, so a seat resuming
// the agent's stored ref fails: a codex review died with "thread/resume: no
// rollout found for thread id <uuid>" while that rollout sat in the real
// ~/.codex/sessions tree the seat cannot see. Run the seat on a job-scoped
// FRESH ref instead, which also stops a disposable review seat from OCCUPYING
// the runtime-session lock key of the agent's live default-runtime session
// (the lock is taken on this effective agent), and keeps the mailbox from
// persisting the minted session id back onto the stored agent row
// (runtime.IsFreshRef gates that write).
//
// An already-fresh ref is left alone: a per-job runtime override minted a
// unique ref at enqueue and a registered fresh:<seat> ref was already scoped by
// scopeRegisteredFreshRefForJob, and both are the keys the scheduler gate
// computed, so rewriting them would desync gate from acquisition.
//
// ONLY RESUMABLE RUNTIMES ARE REWRITTEN. A shell ref is a COMMAND, not a
// session: overwriting it with a fresh ref would replace the command with
// nothing, so a read-only shell agent keeps its ref.
func applyReadOnlySeat(readOnlySeat bool, configDir string, jobID string, agent *runtime.Agent) error {
	if agent == nil || !readOnlySeat {
		return nil
	}
	agent.ReadOnlySeat = true
	agent.RuntimeConfigDir = strings.TrimSpace(configDir)
	agent.WritablePaths = nil
	agent.ReadablePaths = nil
	agent.ReadableFiles = nil
	if !resumableSessionRuntime(agent.Runtime) || runtime.IsFreshRef(agent.RuntimeRef) {
		return nil
	}
	ref, err := readOnlySeatRuntimeRef(jobID)
	if err != nil {
		return err
	}
	agent.RuntimeRef = ref
	return nil
}

// noExecutionRuntimes are the runtimes whose adapter applies a verified
// read-only tool allowlist for a no-execution seat (#2364). Any other runtime
// would still offer the model a shell or code tool, so it is refused.
var noExecutionRuntimes = map[string]bool{runtime.OmpRuntime: true}

// applyNoExecution narrows a read-only seat to one that runs nothing. It fails
// closed: a job recorded as no-execution never runs with a weaker contract.
func applyNoExecution(home string, readOnlySeat bool, agent *runtime.Agent) error {
	if !readOnlySeat || agent == nil || !agent.ReadOnlySeat {
		return errors.New("no-execution review requires a read-only review seat")
	}
	if !noExecutionRuntimes[agent.Runtime] {
		return fmt.Errorf("no-execution review is not supported on runtime %q; use an omp reviewer", agent.Runtime)
	}
	if err := sandbox.NoExecutionSupported(); err != nil {
		return fmt.Errorf("no-execution review refused on this host: %w", err)
	}
	overlay, err := ensureOmpNoExecutionOverlay(home)
	if err != nil {
		return err
	}
	agent.NoExecution = true
	agent.NoExecutionConfig = overlay
	return nil
}

// ensureOmpNoExecutionOverlay writes the fixed omp no-execution settings
// overlay once under the Gitmoot home and returns its path. Its content is a
// constant, so every no-execution job shares one read-only file; a file whose
// content differs is rewritten atomically rather than trusted.
func ensureOmpNoExecutionOverlay(home string) (string, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return "", fmt.Errorf("resolve gitmoot home for no-execution overlay: %w", err)
	}
	dir := filepath.Join(filepath.Dir(paths.Database), "runtimes")
	path := filepath.Join(dir, "omp-no-execution-v1.yml")
	if current, readErr := os.ReadFile(path); readErr == nil && string(current) == runtime.OmpNoExecutionOverlay {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create no-execution overlay dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".omp-no-execution-*.yml")
	if err != nil {
		return "", fmt.Errorf("write no-execution overlay: %w", err)
	}
	_, writeErr := tmp.WriteString(runtime.OmpNoExecutionOverlay)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr, os.Chmod(tmp.Name(), 0o400)); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("write no-execution overlay: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("install no-execution overlay: %w", err)
	}
	return path, nil
}

// resumableSessionRuntime reports whether a runtime's RuntimeRef names a
// resumable session (rather than a shell command or nothing at all). It is the
// same set runtimeSessionResourceKey keys locks for.
//
// No current job runtime resumes sessions: omp mints a session per job and shell
// refs are commands (#2587 removed codex, claude and kimi). The check and the
// session-lock machinery behind it are kept until a follow-up removes them.
func resumableSessionRuntime(string) bool {
	return false
}

// readOnlySeatRuntimeRef is the fresh session ref a read-only seat DELIVERS on.
//
// It is never a lock key, and that is the point: the seat locks on the agent's
// REGISTERED ref (jobWorker.run keeps a pre-seat copy in sessionLockAgent) and
// queuedJobRuntimeResourceKey reads the stored agent for a REGISTERED seat and
// therefore computes the same registered key. Gate and acquisition agree because
// neither one uses this function.
//
// #1952 narrowed that to REGISTERED seats, and a first version of this note
// asserted "no seat job carries an ephemeral spec" on the strength of a grep for
// co-assignment — which cannot establish absence. TRACED, the opposite was true:
// the retired delegation engine (#2588) set Ephemeral and ReadOnlySeat on the
// SAME request for an ask/review action, so historical ephemeral read-only
// seat rows exist.
//
// It needs no seat branch anyway, because the registered-ref story does not
// describe it: an ephemeral job has no registered session at all, so the gate
// keys it by job id (see queuedJobRuntimeResourceKey) whether or not it is a
// seat, and the worker still locks the materialized session. Nothing here
// depends on the two flags being mutually exclusive.
//
// An earlier version of this comment claimed both derived their key from HERE.
// That is the opposite of the code, and acting on it is exactly the mutation
// TestDaemonWorkerLocksTheRegisteredSessionForAReadOnlySeat exists to kill. The
// genuine #1034 shared-key shape is one branch up, in the
// isolatedShellStageRuntimeSessionKey path. A job id is always
// available at both call sites; the unnamed case still gets a fresh, never
// resumed ref rather than silently falling back to a resumable session.
func readOnlySeatRuntimeRef(jobID string) (string, error) {
	if id := strings.TrimSpace(jobID); id != "" {
		return runtime.FreshRefForJob(id), nil
	}
	return runtime.NewFreshRef()
}

// stagePriorVerdicts writes the read-only seat a RENDERED, REPO-SCOPED list of
// the prior review verdicts it needs, inside its own cache root.
//
// #1839 rounds 2-3. The first attempt granted the live database as read-only
// files, which was INERT: SQLite opens the -wal and -shm sidecars
// O_RDWR|O_CREAT and a read-file grant refuses both, so the open failed before
// a verdict was read. The second attempt staged a VACUUM INTO snapshot of the
// WHOLE store, which worked and was far too wide - every repo's jobs, prompts
// and results, plus resource_locks.owner_token and task_state_claims.token,
// handed to an untrusted runtime that forwards what it reads to a third-party
// model API. For a daemon managing several repos that is cross-repo disclosure.
//
// A RENDERED artifact fixes both at once and deletes four other defects rather
// than patching them: there is no database to open, so no sidecar problem and
// no staleness ambiguity (the file states its own as-of time); it carries only
// the verdict fields a reviewer needs, so tokens, memories and org state are
// structurally absent rather than merely unmentioned; and nothing has to teach
// the CLI to read an alternate home, so no write path can be redirected into a
// throwaway copy.
//
// A failure here is NEVER fatal: the seat loses evidence and says so through
// the returned diagnostic.
func stagePriorVerdicts(ctx context.Context, paths config.Paths, cacheRoot string, reviewRepo string) (string, string) {
	if strings.TrimSpace(cacheRoot) == "" {
		return "", ""
	}
	body, diagnostic := renderPriorVerdictsFile(ctx, paths, reviewRepo)
	if body == nil {
		return "", diagnostic
	}
	dir := filepath.Join(cacheRoot, "evidence")
	// The daemon owns this root and has just re-created it, so a leftover from
	// a job whose cleanup never ran must not make THIS seat evidence-less -
	// that was a real defect: the destination-exists refusal turned a crashed
	// predecessor into a silently unreviewed head.
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Sprintf("workflow evidence: clear %s: %v", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Sprintf("workflow evidence: create %s: %v", dir, err)
	}
	file := filepath.Join(dir, "prior-verdicts.json")
	if err := os.WriteFile(file, body, 0o600); err != nil {
		return "", fmt.Sprintf("workflow evidence: write %s: %v", file, err)
	}
	return file, ""
}

// renderPriorVerdictsFile renders the repo-scoped prior-verdict list that
// GITMOOT_PRIOR_VERDICTS names, for a host seat or a remote instance. A nil
// body means nothing can be staged; the diagnostic says why unless the absence
// is the normal fresh-home case.
func renderPriorVerdictsFile(ctx context.Context, paths config.Paths, reviewRepo string) ([]byte, string) {
	if strings.TrimSpace(paths.Database) == "" {
		return nil, ""
	}
	switch info, err := os.Stat(paths.Database); {
	case err != nil && os.IsNotExist(err):
		// No store yet is the normal case on a fresh home, not a defect, and
		// it is the ONLY error that may pass silently.
		return nil, ""
	case err != nil:
		// Everything else - EACCES, ENOTDIR, EIO, a dangling symlink - used to
		// return silently here and read as "fresh home", so the seat reviewed
		// with no prior verdicts and the operator saw no diagnostic. That is
		// the silent degradation this function exists to remove.
		return nil, fmt.Sprintf("workflow evidence: stat store %s: %v", paths.Database, err)
	case info.IsDir():
		return nil, fmt.Sprintf("workflow evidence: store path %s is a directory", paths.Database)
	}
	repo := strings.TrimSpace(reviewRepo)
	if repo == "" || strings.Contains(repo, "*") {
		// Without the repo of the job in hand there is nothing to scope TO,
		// and an unscoped copy is the defect this function exists to remove.
		//
		// The key is deliberately the REVIEWED repo and not agent.RepoScope. A
		// seat's registered scope is an authorisation boundary - it may be a
		// wildcard, and nothing in the tree makes it agree with a job's repo -
		// so keying on it could hand a seat a different repo's verdicts than
		// the one it is reviewing while the artifact claimed otherwise.
		return nil, "workflow evidence: no repo under review, so no prior-verdict list was staged"
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return nil, fmt.Sprintf("workflow evidence: open store read-only: %v", err)
	}
	defer func() { _ = store.Close() }()
	jobs, err := store.ListJobsByRepo(ctx, repo)
	if err != nil {
		return nil, fmt.Sprintf("workflow evidence: list prior verdicts: %v", err)
	}
	rendered := renderPriorVerdicts(repo, jobs)
	body, err := json.MarshalIndent(rendered, "", "  ")
	if err != nil {
		return nil, fmt.Sprintf("workflow evidence: render prior verdicts: %v", err)
	}
	return append(body, '\n'), ""
}

// priorVerdictList is the rendered artifact's shape. It states its own scope
// and as-of time, because a reader who cannot tell a frozen list from a live
// query is the failure mode this whole change is about.
type priorVerdictList struct {
	Repo     string         `json:"repo"`
	AsOf     string         `json:"as_of"`
	Note     string         `json:"note"`
	Verdicts []priorVerdict `json:"verdicts"`
}

// priorVerdict carries only what a reviewer needs to enumerate what came
// before: who decided what, at which head, on what evidence, and every
// finding as the reviewer recorded it. A summary need not enumerate its
// findings, and a seat without GitHub access (a remote instance) has no other
// way to read them, so it could not answer each prior finding (#2281).
type priorVerdict struct {
	JobID          string            `json:"job_id"`
	Agent          string            `json:"agent"`
	PullRequest    int               `json:"pull_request,omitempty"`
	HeadSHA        string            `json:"head_sha,omitempty"`
	Decision       string            `json:"decision,omitempty"`
	Severity       string            `json:"severity,omitempty"`
	Evidence       string            `json:"evidence,omitempty"`
	Findings       int               `json:"findings"`
	FindingDetails []json.RawMessage `json:"finding_details,omitempty"`
	Summary        string            `json:"summary,omitempty"`
}

// renderPriorVerdicts projects review jobs onto the artifact. Anything not
// named here is structurally absent from what the seat can read.
func renderPriorVerdicts(repo string, jobs []db.Job) priorVerdictList {
	list := priorVerdictList{
		Repo: repo,
		AsOf: time.Now().UTC().Format(time.RFC3339),
		Note: "Frozen at as_of, scoped to the repo of the job under review, rendered from the workflow store. " +
			"Not a live query: a verdict recorded after as_of is absent rather than missing.",
		Verdicts: []priorVerdict{},
	}
	for _, job := range jobs {
		if strings.TrimSpace(job.Type) != "review" {
			continue
		}
		var payload workflow.JobPayload
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			continue
		}
		if payload.Result == nil {
			continue
		}
		list.Verdicts = append(list.Verdicts, priorVerdict{
			JobID:          job.ID,
			Agent:          job.Agent,
			PullRequest:    payload.PullRequest,
			HeadSHA:        payload.HeadSHA,
			Decision:       payload.Result.Decision,
			Severity:       payload.Result.Severity,
			Evidence:       payload.Result.Evidence,
			Findings:       len(payload.Result.Findings),
			FindingDetails: payload.Result.Findings,
			Summary:        payload.Result.Summary,
		})
	}
	return list
}

func readOnlyRuntimeSandboxGrants(home string, agent runtime.Agent, checkout string, reviewRepo string) (grants readOnlySandboxGrants, err error) {
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
	// The seat's private, short TMPDIR (#2314). It joins the write grants
	// before any staged root is placed, so every placement check below also
	// proves no staged copy sits inside it. Any setup failure after this point
	// removes it again; the adapter's cleanup removes it at the end of the job.
	// The cache-root fallback is already inside grants.cacheRoot.
	grants.tempDir, err = createSeatTempDirOrFallback(paths.Home, grants.cacheRoot)
	if err != nil {
		return grants, err
	}
	tempDir := grants.tempDir.dir
	if !grants.tempDir.inCacheRoot {
		grants.writes = append(grants.writes, tempDir)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, grants.tempDir.remove())
			grants.tempDir = seatTempDir{}
		}
	}()
	// The write grants are final here. Validate them against the checkout
	// before anything runs under them, including the module prefetch below,
	// which writes scratch files beneath tempDir.
	if err := validateReadOnlyWritablePaths(checkout, grants.writes); err != nil {
		return grants, err
	}

	grants.reads = append(grants.reads, checkout)
	metadata, err := reviewGitMetadataPaths(checkout)
	if err != nil {
		return grants, err
	}
	grants.reads = append(grants.reads, metadata...)

	stateDir, stateEnv, err := prepareReadOnlyRuntimeState(agent, grants.cacheRoot)
	if err != nil {
		return grants, err
	}
	grants.stateDir = stateDir
	grants.env = append(grants.env, stateEnv...)
	// A DAEMON-OWNED COPY RATHER THAN A GRANT ON THE OPERATOR'S TREE (#1878).
	// Granting a seat a rule over the operator's installation produced
	// escape-class defects in three review rounds, including a member symlink
	// that redirected a grant outside containment with no privilege at all. The
	// copy is staged under the gitmoot home, which this seat can never write,
	// and is added as an ordinary read.
	//
	// A STAGING MISS IS LOGGED, NOT EVENTED, AND IT IS DELIBERATELY NOT PUT IN
	// grants.dropped. That field is documented above as what narrowing WITHHELD
	// from the staged config, and its event text says so; a copy that never
	// materialised was never in the config to be withheld, so reporting it there
	// would claim a narrowing that did not happen. It would also make a seat's
	// EVENT SEQUENCE depend on the host's toolchain and disk state, which a CI
	// race shard caught: TestExecBackendLocalDefaultDaemonE2E asserts an exact
	// baseline, passed on a host where staging succeeded, and failed on a runner
	// where it did not. An operator-visible fact about the host belongs in the
	// daemon's log, not in a job's event stream.
	//
	// Failure publishes an engine-owned exit-126 command; inability to publish
	// that fail-closed path aborts setup rather than exposing the host copy.
	staged, stagedEnv, diagnostic, err := stageSeatToolchain(paths, checkout)
	if err != nil {
		return grants, err
	}
	if diagnostic != "" {
		fmt.Fprintf(os.Stderr, "gitmoot: read-only seat toolchain: %s\n", diagnostic)
	}
	// #1817: WHETHER THE SEAT CAN RUN `go` AT ALL, recorded beside the sibling
	// runtime answer. The diagnostic above cannot carry this: the `go` absent
	// arm passes "" (toolchain_seat.go:39), so a host with no Go on the daemon
	// PATH publishes the exit-126 shim and reports nothing. The ROOT is the
	// fact - stageSeatToolchain returns the published-unavailable root in every
	// failure arm - so key on it and use the diagnostic only to sharpen the
	// cause when one exists.
	// #2513: only a checkout that is Go makes a missing `go` a reason to
	// refuse the review; a repository in another language never runs it, and
	// requiring Go there forced every image to ship ~250 MB it never uses. The
	// seat still gets the exit-126 `go` either way.
	if staged != "" && filepath.Base(staged) == "go"+toolchain.UnavailableRuntimeSuffix && checkoutUsesGo(checkout) {
		grants.toolchainUnavailable = strings.TrimSpace(diagnostic)
		if grants.toolchainUnavailable == "" {
			grants.toolchainUnavailable = "no Go installation resolved on the daemon PATH, so the seat's `go` is the engine's exit-126 unavailable command"
		}
	}
	if staged != "" {
		if err := validateStagedToolchainPlacement(staged, grants.writes); err != nil {
			return grants, err
		}
		grants.reads = append(grants.reads, staged)
		grants.env = append(grants.env, stagedEnv...)
		// An explicit or enclosing workspace file can sit outside the checkout
		// directory grant. WorkspaceGoRequirement validated one bounded regular
		// file; grant that file only, never its parent directory.
		gowork := "off"
		for _, entry := range stagedEnv {
			workFile, ok := strings.CutPrefix(entry, "GOWORK=")
			if !ok {
				continue
			}
			gowork = workFile
			if filepath.IsAbs(workFile) {
				grants.readFiles = append(grants.readFiles, workFile)
			}
		}
		// #2314: a populated, read-only module cache, so an offline build in
		// the seat finds its dependencies. See seat_go_modules.go for why the
		// per-job cache was always empty.
		if grants.toolchainUnavailable == "" {
			modCache, modDiagnostic := stageSeatGoModules(seatGoModuleRequest{
				paths:      paths,
				checkout:   checkout,
				reviewRepo: reviewRepo,
				goroot:     staged,
				gowork:     gowork,
				scratch:    filepath.Join(tempDir, ".gitmoot-go-mod-download"),
				writes:     grants.writes,
			})
			if modDiagnostic != "" {
				fmt.Fprintf(os.Stderr, "gitmoot: read-only seat Go modules: %s\n", modDiagnostic)
			}
			if modCache != "" {
				grants.reads = append(grants.reads, modCache)
				grants.env = withSeatGoModuleCache(grants.env, modCache)
			}
		}
	}
	// Runtime executables are staged beside the Go toolchain and exposed by
	// fingerprint-local shims. Grant the PUBLISHED roots the daemon owns, never
	// the operator PATH roots they were copied from (#1921, ruling 122157).
	stagedRuntimes, runtimeInterpreters, runtimeDiagnostics, err := stageSeatRuntimes(paths, agent.Runtime)
	if err != nil {
		return grants, err
	}
	for _, diagnostic := range runtimeDiagnostics {
		fmt.Fprintf(os.Stderr, "gitmoot: read-only seat runtime: %s\n", diagnostic)
	}
	// #1817: THE SEAT'S OWN RUNTIME IS THE CAPABILITY QUESTION, AND THIS IS
	// WHERE THE ANSWER EXISTS. stageSeatRuntimes has just decided, for this host
	// and this home, whether the command the seat will exec is the daemon's copy
	// of the runtime or the exit-126 unavailable shim. Recording it here costs
	// nothing extra - the staging pass already ran - and it is available before
	// the adapter is composed, before MarkAgentInstanceRunning, and before any
	// model token is billed.
	//
	// RECORDED, NOT REFUSED. Seat setup has callers that compose a seat adapter
	// and never exec it, so refusing here refuses valid work: it broke five
	// existing tests on this host and would refuse every claude seat in CI,
	// where no runtime CLI is installed. The worker refuses, because the worker
	// is the caller that knows whether its adapter will exec the binary.
	//
	// The question is deliberately NOT answered by probing the dispatching
	// host's PATH, which #1817's first comment measured as the wrong side of the
	// boundary: on this deployment a dispatch-time LookPath("claude") resolves
	// the operator's working installation while every seat receives the shim, so
	// a host-side check reports "present" for a runtime no seat can run.
	grants.runtimeUnavailable = seatRuntimeUnavailable(agent.Runtime, stagedRuntimes, runtimeDiagnostics)
	for _, shim := range stagedRuntimes {
		root, err := toolchain.StagedRuntimeRoot(paths.Home, shim)
		if err != nil {
			return grants, err
		}
		if err := validateStagedToolchainPlacement(root, grants.writes); err != nil {
			return grants, err
		}
		grants.reads = append(grants.reads, root)
	}
	// THE EXEC CLOSURE, NOT THE DISPATCH LIST (#2141). seatRuntimeNames
	// enumerates what the engine can DISPATCH - claude, kimi, codex - and its
	// own comment says so: "derived from what the engine can start". A script
	// runtime is not started, it is EXEC'd by an interpreter, and staging
	// publishes that interpreter into a SIBLING root. The loop above grants
	// each launcher's own root, so before this the codex launcher was granted
	// and the node tree it execs was not: the seat received an engine-staged
	// command it was forbidden to run, and reported exit 126 with no finding.
	//
	// Granting RuntimeRoot(paths.Home) wholesale would also fix it and is
	// refused: that hands a seat exec over every staged runtime including the
	// ones no dispatch decision chose, which is the widening #1878 and #1921
	// spent three rounds narrowing away from. Grant exactly what the launcher
	// execs.
	for _, interpreter := range runtimeInterpreters {
		root, err := toolchain.StagedRuntimeRoot(paths.Home, interpreter)
		if err != nil {
			return grants, err
		}
		if err := validateStagedToolchainPlacement(root, grants.writes); err != nil {
			return grants, err
		}
		grants.reads = append(grants.reads, root)
	}
	var pathDiagnostics []string
	grants.env, pathDiagnostics = withSeatRuntimePath(grants.env, stagedRuntimes)
	for _, diagnostic := range pathDiagnostics {
		fmt.Fprintf(os.Stderr, "gitmoot: read-only seat runtime: %s\n", diagnostic)
	}
	// BOUNDED, because a read on the worker path must not be able to hang seat
	// setup: the previous form copied the entire database under a hardcoded
	// context.Background() with no deadline and no cancellation. Rendering a
	// repo-scoped list is a bounded read of its own, and this makes the bound
	// explicit rather than incidental.
	evidenceCtx, cancelEvidence := context.WithTimeout(context.Background(), 30*time.Second)
	evidenceFile, evidenceDiagnostic := stagePriorVerdicts(evidenceCtx, paths, grants.cacheRoot, reviewRepo)
	cancelEvidence()
	if evidenceFile != "" {
		grants.evidenceFile = evidenceFile
		grants.env = append(grants.env, "GITMOOT_PRIOR_VERDICTS="+evidenceFile)
	}
	if evidenceDiagnostic != "" {
		grants.dropped = append(grants.dropped, evidenceDiagnostic)
	}

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
	for _, path := range append([]string{filepath.Join(grants.cacheRoot, "home")}, grants.writes...) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return grants, fmt.Errorf("create read-only sandbox write directory %q: %w", path, err)
		}
	}
	grants.reads = compactCleanPaths(grants.reads)
	grants.readFiles = compactCleanPaths(grants.readFiles)
	grants.writes = compactCleanPaths(grants.writes)
	return grants, nil
}

// readOnlySeatStatePolicy declares where a runtime keeps the state a seat needs
// in a wiped, job-private dir. No file from the operator's profile is staged:
// omp authenticates only through the separately validated scoped proxy
// (#2587 removed the codex, claude and kimi credential and config staging).
//
// TestReadOnlySeatStatePolicyCoversEveryRegisteredRuntime fails when a
// registered runtime has no policy.
type readOnlySeatStatePolicy struct {
	// relativeState is where the isolated dir is created under runtime-state.
	relativeState string
	// stateEnv names the environment variable that points the runtime at the
	// staged dir. stateRootEnv names a config-root variable whose value the
	// runtime resolves relative to HOME. Empty when HOME alone locates state.
	stateEnv     string
	stateRootEnv string
}

// readOnlySeatStatePolicyFor returns the staging policy for a runtime. The
// second result is false for a runtime that needs no isolated state at all.
func readOnlySeatStatePolicyFor(runtimeName string) (readOnlySeatStatePolicy, bool, error) {
	switch runtimeName {
	case runtime.ShellRuntime:
		return readOnlySeatStatePolicy{}, false, nil
	case runtime.OmpRuntime:
		// Its writable config, database, snapshot cache and logs need fresh
		// job-private roots, but no file from the operator's OMP profile is staged.
		return readOnlySeatStatePolicy{
			relativeState: filepath.Join(".omp", "agent"),
			stateEnv:      "PI_CODING_AGENT_DIR",
			stateRootEnv:  "PI_CONFIG_DIR",
		}, true, nil
	default:
		return readOnlySeatStatePolicy{}, false, fmt.Errorf("read-only seat runtime %q has no isolated state policy", runtimeName)
	}
}

func prepareReadOnlyRuntimeState(agent runtime.Agent, cacheRoot string) (string, []string, error) {
	// THE THIRD WRITER (#1810). This creates the seat's state dir by joining
	// onto cacheRoot, so a relative cacheRoot writes relative to the working
	// directory - the mechanism that once published a token. Production
	// supplies an absolute cacheRoot from the isolated-tool-cache grant; this
	// makes that checked.
	if err := requireAbsoluteCredentialHome(cacheRoot, "runtime-state"); err != nil {
		return "", nil, err
	}
	policy, needsState, err := readOnlySeatStatePolicyFor(agent.Runtime)
	if err != nil || !needsState {
		return "", nil, err
	}
	stateDir := filepath.Join(cacheRoot, "runtime-state", policy.relativeState)
	if err := os.RemoveAll(stateDir); err != nil {
		return "", nil, fmt.Errorf("reset isolated runtime state %q: %w", stateDir, err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create isolated runtime state %q: %w", stateDir, err)
	}
	var stateEnv []string
	if policy.stateEnv != "" {
		stateEnv = append(stateEnv, policy.stateEnv+"="+stateDir)
	}
	if policy.stateRootEnv != "" {
		rootFromHome, err := filepath.Rel(filepath.Join(cacheRoot, "home"), filepath.Dir(stateDir))
		if err != nil {
			return "", nil, fmt.Errorf("resolve isolated runtime config root: %w", err)
		}
		if filepath.IsAbs(rootFromHome) {
			return "", nil, errors.New("isolated runtime config root did not resolve relative to HOME")
		}
		stateEnv = append(stateEnv, policy.stateRootEnv+"="+rootFromHome)
	}
	return stateDir, stateEnv, nil
}

func readOnlyRuntimeBaseEnv(environ []string, githubDir string) []string {
	allowed := make(map[string]struct{}, len(curatedBaseEnvNames))
	for _, name := range curatedBaseEnvNames {
		allowed[name] = struct{}{}
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

func landlockReadOnlyRunner(runner subprocess.Runner, reads, readFiles, writes, env []string) subprocess.Runner {
	return landlockRuntimeRunner(runner, reads, readFiles, writes, env, true, false)
}

// landlockNoExecutionRunner is landlockReadOnlyRunner for a review seat that
// must run nothing (#2364).
func landlockNoExecutionRunner(runner subprocess.Runner, reads, readFiles, writes, env []string) subprocess.Runner {
	return landlockRuntimeRunner(runner, reads, readFiles, writes, env, true, true)
}

func landlockRuntimeRunner(runner subprocess.Runner, reads, readFiles, writes, env []string, readOnlyWorkdir, noExecution bool) subprocess.Runner {
	readable := append([]string(nil), reads...)
	files := append([]string(nil), readFiles...)
	writable := append([]string(nil), writes...)
	runtimeEnv := append([]string(nil), env...)
	wrap := func(inner subprocess.Runner) subprocess.WrappingRunner {
		return subprocess.WrappingRunner{Inner: inner, ReadablePaths: readable, ReadableFiles: files, WritablePaths: writable, ReadOnlyWorkdir: readOnlyWorkdir, NoExecution: noExecution, Env: runtimeEnv}
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

// perJobAdmissionEstimate maps a queued job to its admission cost (#365):
// whether it holds a resumable runtime session (so it counts against
// max_concurrent_sessions) and its RAM estimate (GB). A job whose runtime has no
// resumable session key — exactly the runtimes already exempt from the runtime
// session lock (queuedJobRuntimeResourceKey returns "") — is "not
// session-counted" and contributes 0 RAM. Otherwise the job is session-counted
// at default_memory_gb.
func perJobAdmissionEstimate(ctx context.Context, store *db.Store, job db.Job, policy config.AdmissionPolicy) admissionEstimate {
	if queuedJobRuntimeResourceKey(ctx, store, job) == "" {
		return admissionEstimate{session: false, memGB: 0}
	}
	return admissionEstimate{session: true, memGB: policy.DefaultMemoryGB}
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
	if !resumableSessionRuntime(agent.Runtime) {
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

func (w jobWorker) runWithTempWorker(ctx context.Context, job db.Job, payload workflow.JobPayload, backend execbackend.Backend, original runtime.Agent, checkout string, policy config.ParallelSessionPolicy, reason string) error {
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
	if err == nil {
		// #1807: the temp-worker fork leaves run() before the sandbox wrap at
		// the normal delivery site, and startTempWorker COPIES the delivery
		// agent, so ReadOnlySeat survives into this path. Without this the seat
		// routes around both the Landlock wrap and the staging policy - the very
		// property this path's caller was gated on. The wrap is a no-op for an
		// ordinary temp worker (it returns the adapter unchanged unless
		// ReadOnlySeat is set), so this cannot affect the common path.
		var forkSetup readOnlySeatSetup
		adapter, forkSetup, err = wrapReadOnlySandboxAdapter(w.ConfigHome, started.Agent, checkout, payload.Repo, adapter)
		// Same job-end cleanup as run(): this path never had one, so a
		// temp-worker seat leaked its cache root, and would leak its private
		// temp dir (#2314).
		if stateAdapter, ok := adapter.(readOnlyRuntimeAdapter); ok {
			defer func() {
				if cleanupErr := stateAdapter.cleanup(); cleanupErr != nil {
					writeLine(w.Stdout, "job %s read-only runtime state cleanup failed: %v", job.ID, cleanupErr)
				}
			}()
		}
		if len(forkSetup.dropped) > 0 {
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "read_only_seat_config_narrowed", Message: "withheld from the seat's staged config: " + strings.Join(forkSetup.dropped, ", ")}); eventErr != nil {
				return eventErr
			}
		}
		// #1817's fourth entry point, measured on 2026-09-05 07:44:52: the
		// temp-worker fork allocated a read-only worktree, minted a worker,
		// marked the job running and posted an attributed PR comment, all before
		// anything asked whether the selected path could start. Refuse the same
		// capability here as in run(); a temp worker is new work, so its runtime
		// being unavailable is not less true for being delegated.
		if err == nil && forkSetup.runtimeUnavailable != "" && w.adapterIsDeclaredReal() {
			err = &seatRuntimeCapabilityError{agentName: started.Agent.Name, runtimeName: started.Agent.Runtime, cause: forkSetup.runtimeUnavailable}
			if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: seatRuntimeUnavailableEvent, Message: err.Error()}); eventErr != nil {
				writeLine(w.Stdout, "job %s %s event failed: %v", job.ID, seatRuntimeUnavailableEvent, eventErr)
			}
			if finishErr := w.finishQueuedJob(ctx, job, workflow.JobBlocked, err); finishErr != nil {
				return finishErr
			}
			_ = w.postJobResultComment(ctx, delegatedJob.ID, started.Agent, checkout, err)
			return nil
		}
	}
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
	if len(tempToolCacheEnv) > 0 {
		if envAdapter, envErr := injectDeliveryAdapterEnv(adapter, tempToolCacheEnv); envErr != nil {
			writeLine(w.Stdout, "job %s tool cache env inject failed: %v", delegatedJob.ID, envErr)
		} else {
			adapter = envAdapter
		}
	}
	// Temp-session delivery is a separate early-return path; attach the same
	// append-only capture here or it would be absent from the trajectory corpus.
	retainedLogFile, retainedLogErr := openRetainedTranscriptLog(w.ConfigHome, delegatedJob.ID, delegatedJob.Type, effectiveTranscriptRuntime(payload, started.Agent), delegatedJob.LifecycleGeneration, w.Store)
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
	engine := w.workflowForJob(checkout, jobRunner)
	_, err = engine.RunJob(runCtx, delegatedJob.ID, started.Agent, adapter)
	err = w.recordUnrecordedResult(ctx, engine, err)
	stopKillPending()
	if err != nil {
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
	if clearErr := clearRoleUnavailableOnSuccess(ctx, w.Store, payload.ActingOrgRole, started.Agent.Runtime); clearErr != nil {
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
	mailbox := workflow.NewMailbox(w.Store)
	mailbox.RuntimeDefaultModel = runtimeDefaultModelResolver(w.workflowHome())
	mailbox.ReviewModelPool = reviewModelPoolResolver(w.workflowHome())
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
	jobTimeout := effectiveJobTimeout(payload, managed, job.Type)
	tempAgent := original
	tempAgent.Name = tempWorkerInstanceName(original.Name, job.ID)
	tempAgent.RuntimeRef = ""
	// A temp worker's session is started for this one job and disposed after it
	// — single-use, so session-cumulative usage (codex, #658) is the job's cost.
	tempAgent.SingleUseSession = true
	now := time.Now().UTC()
	reserved := db.AgentInstance{
		Name:           tempAgent.Name,
		Type:           tempWorkerAgentType(original.Name),
		Runtime:        tempAgent.Runtime,
		RuntimeRef:     "starting:" + tempAgent.Name,
		RepoFullName:   payload.Repo,
		Role:           tempAgent.Role,
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
	started, err := adapter.Start(ctx, runtime.StartRequest{Agent: tempAgent, Prompt: agentStartupPrompt(tempAgent)})
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
	// ReviewJobTimeout is the floor a REVIEW gets whichever reviewer answers it
	// (#2191). Zero leaves review deadlines exactly as they were.
	ReviewJobTimeout time.Duration
	// ReviewClassFallbackReason is set when [review_router] could not be read,
	// so the built-in floor is in use instead of the operator's value. Reported
	// on the job rather than swallowed (#2192 review).
	ReviewClassFallbackReason string
}

func (w jobWorker) managedJobConfig(ctx context.Context, agentName string) (managedJobRuntimeConfig, error) {
	runtimeConfig := managedJobRuntimeConfig{
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
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
		// The review-class floor is operator-settable as [review_router]
		// job_timeout (#2191). Fails OPEN to the built-in default: an unreadable
		// router section must not silently remove the floor that stops a
		// rotated review from aborting mid-analysis.
		routerSettings, routerErr := config.LoadReviewRouterSettings(paths)
		switch {
		case routerErr != nil:
			runtimeConfig.ReviewJobTimeout = config.DefaultReviewJobTimeout
			// FAIL OPEN, BUT SAY SO (#2192 review, P3). A malformed
			// [review_router] silently restored the built-in 3h, so an operator
			// who deliberately set a SHORTER class deadline got one six times
			// longer with nothing logged - the fallback was indistinguishable
			// from an absent key.
			runtimeConfig.ReviewClassFallbackReason = routerErr.Error()
		case routerSettings.JobTimeout > 0:
			runtimeConfig.ReviewJobTimeout = routerSettings.JobTimeout
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
	// InvalidPayload carries a payload job_timeout that could not be parsed as
	// a positive duration, so the caller can report it instead of silently
	// falling through to a lower-priority source (#2192 review).
	InvalidPayload string
}

// effectiveJobTimeout returns the kill deadline selected by the independent
// timeout authority. The stale-running threshold is deliberately absent: it is
// only a crash/staleness predicate and can never become a run-context deadline.
func effectiveJobTimeout(payload workflow.JobPayload, managed managedJobRuntimeConfig, jobType string) time.Duration {
	return resolveEffectiveJobTimeout(payload, managed, jobType).Timeout
}

// isReviewPayload reports whether this job is a review, for the class-deadline
// floor. Both markers are checked because they are written by different
// producers: the router sets a purpose, and a pooled review dispatch carries a
// pool.
// isReviewJob takes the JOB TYPE as well as the payload markers (#2192 review,
// P3). Markers alone missed a review carrying NEITHER a purpose NOR a pool -
// a markerless review escaped the class deadline entirely, which is the defect
// the class deadline exists to prevent, reached by a different door. The type
// is what the store says the job IS; the markers are what a producer happened
// to fill in.
func isReviewJob(payload workflow.JobPayload, jobType string) bool {
	if strings.EqualFold(strings.TrimSpace(jobType), "review") {
		return true
	}
	return strings.TrimSpace(payload.ReviewPurpose) != "" || len(payload.ReviewModelPool) > 0
}

func resolveEffectiveJobTimeout(payload workflow.JobPayload, managed managedJobRuntimeConfig, jobType string) jobTimeoutResolution {
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
	invalidPayloadTimeout := ""
	if raw := strings.TrimSpace(payload.JobTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err == nil && d > 0:
			requested = d
			source = "payload"
		default:
			// SILENTLY IGNORED BEFORE (#2192 review, P3). The level just
			// promoted to highest priority had a silent-failure mode: a
			// coordinator writing a malformed timeout got the class floor and
			// no signal - the same absence-versus-claim defect as the malformed
			// [review_router], one layer up.
			invalidPayloadTimeout = raw
		}
	}
	// THE REVIEW CLASS FLOOR (#2191). A review's duration is a property of the
	// PROMPT, not of whichever agent was selected to answer it. Before this, the
	// deadline came from the agent - 10m, 30m, 45m or 2h on this box - so
	// rotating reviewers rotated the deadline, and the first rotated review drew
	// 10 minutes for a class whose observed maximum is 164 minutes.
	//
	// A FLOOR, not an assignment: an agent configured LONGER keeps its own
	// value. The daemon's job_timeout_max still clamps the result, so this
	// cannot raise a review above the operator's ceiling.
	// AN EXPLICIT PER-JOB TIMEOUT WINS OVER THE FLOOR (#2192 review, P3).
	//
	// Without this a coordinator that deliberately caps a review - JobTimeout
	// "15m" on the payload - silently got 3h, so a delegation timeout could not
	// bound a review at all. That is the same principle this campaign already
	// applied to runtimes: the router routes, it does not OVERRULE a stated
	// choice. The floor exists to stop an UNSTATED deadline being inherited
	// from whichever agent was selected, not to overrule an operator.
	if managed.ReviewJobTimeout > 0 && source != "payload" && isReviewJob(payload, jobType) && requested < managed.ReviewJobTimeout {
		requested = managed.ReviewJobTimeout
		source = "review class"
	}
	resolution := jobTimeoutResolution{Timeout: requested, Requested: requested, Max: jobTimeoutMax, Source: source, InvalidPayload: invalidPayloadTimeout}
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
	cancelIfCancelled := func() bool {
		job, err := w.Store.GetJob(ctx, jobID)
		if err == nil && job.State == string(workflow.JobCancelled) {
			cancel()
			return true
		}
		return false
	}
	cancelled := cancelIfCancelled()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if cancelled {
			return
		}
		ticker := time.NewTicker(daemonJobCancelPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if cancelIfCancelled() {
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
	backend, _, err := w.resolveExecutionBackendForJob(job, payload)
	if err != nil {
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry backend resolution failed: "+err.Error())
	}
	jobRunner, err := jobSubprocessRunnerForBackend(backend)
	if err != nil {
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry backend consumption failed: "+err.Error())
	}
	checkout, err := w.checkoutForPostDeliveryAdvance(ctx, job, payload, agent, jobRunner)
	if err != nil {
		return w.recordAdvanceRetryOnce(ctx, job.ID, "post-delivery workflow retry preflight failed: "+err.Error())
	}
	engine := w.workflowForJob(checkout, jobRunner)
	if err := engine.AdvanceJob(ctx, job.ID); err != nil {
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

func (w jobWorker) defaultAdapter(agent runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
	return buildRuntimeAdapter(w.ConfigHome, agent, checkout, nil)
}

// buildJobAdapterForBackend consumes the already-resolved backend at the
// daemon's adapter boundary. Adding a name to the selector's implemented set is
// insufficient: a new backend must extend execbackend.Consume and supply its
// builder at every compiler-identified call site before it can execute.
func (w jobWorker) buildJobAdapterForBackend(backend execbackend.Backend, agent runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
	return execbackend.Consume(backend, func() (workflow.DeliveryAdapter, error) {
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
	if len(agent.WritablePaths) > 0 || len(agent.ReadablePaths) > 0 || len(agent.ReadableFiles) > 0 {
		return nil, errors.New("runtime path grants are not supported on the remote execution backend")
	}
	switch agent.Runtime {
	case runtime.ShellRuntime:
		return runtime.ShellAdapter{Dir: checkout, Runner: runner}, nil
	case runtime.OmpRuntime:
		return runtime.OmpAdapter{Dir: checkout, Runner: runner}, nil
	default:
		return nil, fmt.Errorf("runtime %q is not supported on the remote execution backend", agent.Runtime)
	}
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

// buildLocalRuntimeAdapter is the pre-#1536 runner-composition pipeline:
// runtimeJobRunner, then the concrete runtime adapter.
func buildLocalRuntimeAdapter(home string, agent runtime.Agent, checkout string, runner subprocess.Runner) (workflow.DeliveryAdapter, error) {
	var err error
	runner, err = runtimeJobRunner(home, agent.Runtime, runner)
	if err != nil {
		return nil, err
	}
	switch agent.Runtime {
	case runtime.OmpRuntime:
		return runtime.OmpAdapter{Dir: checkout, Runner: runner}, nil
	case runtime.ShellRuntime:
		return runtime.ShellAdapter{Dir: checkout, Runner: runner}, nil
	default:
		if runtime.IsRemovedRuntime(agent.Runtime) {
			return nil, runtime.UnsupportedAgentRuntimeError(agent.Name, agent.Runtime)
		}
		return nil, fmt.Errorf("unsupported runtime: %s", agent.Runtime)
	}
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
	return daemonWorkflowEngineForRunner(w.Store, github.NewClient(checkout), checkout, w.workflowHome(), runner, nil)
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
