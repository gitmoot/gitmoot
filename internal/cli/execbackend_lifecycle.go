package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
	remoteexec "github.com/gitmoot/gitmoot/internal/execbackend/remote"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const executionBackendDestroyTimeout = 30 * time.Second

const executionBackendReapTimeout = 30 * time.Second

// sandboxTTLExceededEventKind is recorded when a run fails because its cloud
// sandbox reached the provider's fixed lifetime. The run is never retried: a
// fresh sandbox has the same lifetime, so the same run would die the same way.
const sandboxTTLExceededEventKind = "sandbox_ttl_exceeded"

// remoteSandboxLifetimeMargin is how long before a fixed-lifetime sandbox ends
// the run deadline is placed. It leaves the runtime its own shutdown and the
// mailbox's finalization a live sandbox, so the run times out cleanly instead
// of being cut off mid-stream by the provider.
const remoteSandboxLifetimeMargin = 5 * time.Minute

// remoteSandboxLifetime is the fixed lifetime the job's provider gives every
// sandbox it creates, or zero when the TTL keepalive can extend it.
//
// Cloud E2B caps a sandbox at its team's max instance length counted from the
// sandbox's start, and a set-timeout request past that cap succeeds with the
// end time silently clamped to it (e2b-dev/infra
// packages/api/internal/orchestrator/keep_alive.go, getMaxAllowedTTL). Create
// accepts at most remoteexec.ProviderMaxTTL here, so that is the lifetime: the
// keepalive cannot carry a cloud sandbox past it. Measured: both stream-loss
// review failures in production died 60m02s after their sandbox was created.
// sandboxd caps each extension at now + -max-ttl instead, so the keepalive does
// extend its sandboxes and they have no fixed lifetime.
func remoteSandboxLifetime(backend execbackend.Backend, cfg config.RemoteExecConfig) time.Duration {
	if backend != execbackend.Remote || config.IsSandboxdProvider(cfg.Provider) {
		return 0
	}
	return remoteexec.ProviderMaxTTL
}

// remoteSandboxRunDeadline is the latest instant a run on a sandbox with a
// fixed lifetime may end: remoteSandboxLifetimeMargin before the provider
// retires a sandbox whose create call started no earlier than
// provisionStarted. Zero means the sandbox has no fixed lifetime.
//
// It is an INSTANT, not a duration, because the run context is armed only
// after the rest of the worker's setup. A duration measured here and started
// there would move the run's end past the sandbox's by however long that setup
// took (#2338 review).
func remoteSandboxRunDeadline(lifetime time.Duration, provisionStarted time.Time) time.Time {
	if lifetime <= 0 {
		return time.Time{}
	}
	return provisionStarted.Add(lifetime - remoteSandboxLifetimeMargin)
}

// runDeadline is the instant the run context is armed at: jobTimeout from now,
// or sandboxDeadline when that comes first. A deadline the sandbox shortens is
// recorded as job_timeout_clamped, the event that already reports every other
// clamp of a job's deadline.
func (w jobWorker) runDeadline(ctx context.Context, jobID string, jobTimeout time.Duration, sandboxDeadline time.Time) time.Time {
	deadline := time.Now().Add(jobTimeout)
	if sandboxDeadline.IsZero() || !sandboxDeadline.Before(deadline) {
		return deadline
	}
	message := fmt.Sprintf("job_timeout %s does not fit in the remote sandbox: the provider retires it %s after creation and does not extend it, so the run deadline is clamped to %s (%s from now), %s before the sandbox ends",
		jobTimeout, remoteexec.ProviderMaxTTL, sandboxDeadline.UTC().Format(time.RFC3339), time.Until(sandboxDeadline).Truncate(time.Second), remoteSandboxLifetimeMargin)
	if _, err := w.Store.ClaimJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "job_timeout_clamped", Message: message}); err != nil {
		writeLine(w.Stdout, "job %s job_timeout_clamped event failed: %v", jobID, err)
	}
	return sandboxDeadline
}

var reapedExecutionBackendRoots sync.Map

var executionBackendFencingToken = sync.OnceValues(newRuntimeLockOwnerToken)

func (w jobWorker) defaultExecutionBackend(backend execbackend.Backend, cfg config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
	return execbackend.Consume(backend, func() (execbackend.ExecutionBackend, error) {
		home := strings.TrimSpace(w.workflowHome())
		if home == "" {
			return nil, errors.New("resolve local execution-backend home")
		}
		root := filepath.Join(home, "execbackends", string(execbackend.Local))
		if cfg.LocalRoot != "" {
			root = filepath.Clean(cfg.LocalRoot)
		}
		local, err := execbackend.NewLocalBackend(root, cfg.LocalIdentity())
		if err != nil {
			return nil, err
		}
		// Reap once per resolved root in this process. A restarted daemon has a
		// fresh map and therefore reconciles the prior process's instances before
		// provisioning its first new job.
		//
		// THIS PATH IS FAIL-CLOSED ON PURPOSE and must stay that way: a reap
		// failure below aborts backend construction, and therefore the dispatch
		// that triggered it. Double-running a job against a live instance the
		// previous process left behind is unrecoverable; refusing one dispatch is
		// not. The PERIODIC pass in execbackend_reconcile.go has the OPPOSITE
		// polarity - log-and-continue - because there the risk is a provider
		// outage becoming a gitmoot outage, and the restart invariant is already
		// held here (#1539). Do not align one with the other.
		rootKey := string(backend) + "|" + root
		if _, loaded := reapedExecutionBackendRoots.LoadOrStore(rootKey, struct{}{}); !loaded {
			var reaper execbackend.Reaper = local
			if _, err := reaper.Reap(context.Background()); err != nil {
				reapedExecutionBackendRoots.Delete(rootKey)
				return nil, fmt.Errorf("reap local execution backends: %w", err)
			}
		}
		return local, nil
	}, func() (execbackend.ExecutionBackend, error) {
		if err := cfg.ValidateE2BProvider(); err != nil {
			return nil, err
		}
		apiKey, err := cfg.LoadE2BAPIKey()
		if err != nil {
			return nil, err
		}
		client, err := e2b.NewClient(apiKey, e2b.Options{BaseURL: cfg.E2BBaseURL})
		if err != nil {
			return nil, err
		}
		// GITMOOT-IMPL: production resolves provider envd endpoints; the injected
		// resolver exists only so the lifecycle E2E remains offline and spend-free.
		resolver := w.RemoteEnvdEndpointResolver
		if resolver == nil {
			if cfg.E2BEnvdBaseURL != "" {
				origin := cfg.E2BEnvdBaseURL
				resolver = func(string, int) string { return origin }
			} else if cfg.E2BDomain != "" {
				domain := cfg.E2BDomain
				resolver = func(sandboxID string, port int) string {
					return fmt.Sprintf("https://%d-%s.%s", port, sandboxID, domain)
				}
			}
		}
		remoteBackend, err := remoteexec.NewBackend(client, remoteexec.Options{
			TemplateID:     cfg.E2BTemplate,
			Envd:           e2b.EnvdOptions{EndpointResolver: resolver, FixedHostRouting: cfg.E2BEnvdBaseURL != ""},
			ShouldRenewTTL: w.shouldRenewSandboxTTL,
		})
		if err != nil {
			return nil, err
		}
		fencingToken, err := executionBackendFencingToken()
		if err != nil {
			return nil, fmt.Errorf("create execution backend daemon fencing token: %w", err)
		}
		baseURL := strings.TrimSpace(cfg.E2BBaseURL)
		if baseURL == "" {
			baseURL = e2b.DefaultBaseURL
		}
		accountKey := sha256.Sum256([]byte(apiKey))
		var capacity *sandboxdCapacitySource
		cap := execBackendStoreCap(cfg.ExecBackendCost)
		if config.IsSandboxdProvider(cfg.Provider) {
			// sandboxd's cap is read per provision from its capacity report;
			// this zero policy is never used. Each sandboxd provider has its
			// own cache entry, ceiling and ledger rows, so caps never mix.
			cap = db.ExecBackendCostCap{}
			capacity = &sandboxdCapacitySource{
				cache: sandboxdCapacityReports, key: fmt.Sprintf("%s|%s|%x", cfg.Provider, baseURL, accountKey),
				read: client.Capacity, provider: cfg.Provider, ceiling: cfg.ExecBackendCost.MaxConcurrent, template: cfg.E2BTemplate,
			}
		}
		ledgeredBackend, err := newLedgeredExecutionBackend(w.Store, remoteBackend, cfg.Provider, fencingToken, db.BootID(), w.Stdout, cap)
		if err != nil {
			return nil, err
		}
		ledgeredBackend.capacity = capacity
		ledgeredBackend.setPerHourUSD(cfg.ExecBackendCost.PerHourUSD)
		reapKey := fmt.Sprintf("%s|%s|%s|%x", backend, cfg.Provider, baseURL, accountKey)
		home := w.workflowHome()
		revokingBackend := &credentialRevokingExecutionBackend{inner: ledgeredBackend, home: home}
		if _, loaded := reapedExecutionBackendRoots.LoadOrStore(reapKey, struct{}{}); !loaded {
			reapCtx, cancel := context.WithTimeout(context.Background(), executionBackendReapTimeout)
			defer cancel()
			var reaper execbackend.Reaper = revokingBackend
			if _, err := reaper.Reap(reapCtx); err != nil {
				reapedExecutionBackendRoots.Delete(reapKey)
				return nil, fmt.Errorf("reap remote execution backends: %w", err)
			}
		}
		return revokingBackend, nil
	})
}

func (w jobWorker) executionBackendConfig() (config.RemoteExecConfig, error) {
	cfg := config.DefaultRemoteExecConfig()
	if !w.ConfigHomeExplicit && strings.TrimSpace(w.ConfigHome) == "" {
		return cfg, nil
	}
	paths, err := w.configPaths()
	if err != nil {
		return config.RemoteExecConfig{}, err
	}
	loaded, loadErr := config.LoadRemoteExecConfig(paths)
	switch {
	case loadErr == nil:
		return loaded, nil
	case errors.Is(loadErr, os.ErrNotExist):
		return cfg, nil
	default:
		return config.RemoteExecConfig{}, fmt.Errorf("load [remote_exec] config: %w", loadErr)
	}
}

var lookPathRemoteRuntime = osexec.LookPath

func (w jobWorker) provisionExecutionBackend(ctx context.Context, backend execbackend.Backend, cfg config.RemoteExecConfig, runtimeName string, job db.Job, ttl time.Duration, checkout string) (execbackend.ExecutionBackend, *execbackend.Instance, *credgw.Lease, []string, error) {
	if w.ExecutionBackendFactory == nil {
		// A worker WITHOUT the lifecycle factory is the foreground/unit-test seam,
		// and for a local job "no instance" is the correct answer. For a non-local
		// job it is not: nothing will ever attach an instance, so the job proceeds
		// to unprovisionedRemoteDeliveryAdapter and dies with "remote execution
		// backend is not provisioned" - a message that describes the symptom and
		// hides the cause, which is that this worker cannot provision at all.
		//
		// Refuse here instead, naming the actual constraint. Measured while making
		// remote reviews dispatchable: `gitmoot job run` builds defaultJobWorker,
		// so every remote job run in the foreground failed this way regardless of
		// configuration.
		if backend != execbackend.Local {
			return nil, nil, nil, nil, fmt.Errorf("this worker cannot provision the %s execution backend: it was built without a lifecycle factory, which is the foreground/unit-test seam; run the job under the daemon", backend)
		}
		return nil, nil, nil, nil, nil
	}
	if backend == execbackend.Remote && !remoteCapableRuntime(runtimeName) {
		return nil, nil, nil, nil, fmt.Errorf("runtime %q is not supported on the remote execution backend; supported runtimes are %s", runtimeName, remoteCapableRuntimeNames())
	}
	if backend == execbackend.Remote && runtimeName == runtime.OmpRuntime {
		ompTemplate := strings.TrimSpace(cfg.E2BOMPTemplate)
		if ompTemplate == "" {
			return nil, nil, nil, nil, errors.New("remote omp requires [remote_exec].e2b_omp_template built with at least 2 GiB RAM")
		}
		cfg.E2BTemplate = ompTemplate
	}
	var credentialPlan remoteCredentialGatewayPlan
	if backend == execbackend.Remote {
		var err error
		credentialPlan, err = w.prepareRemoteCredentialGateway(cfg, ttl)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}
	var ompExecutable string
	var ompFile *os.File
	if backend == execbackend.Remote && runtimeName == runtime.OmpRuntime {
		if credentialPlan.gateway == nil {
			return nil, nil, nil, nil, errors.New("remote omp requires the model credential gateway; raw-key fallback is forbidden")
		}
		var err error
		ompExecutable, ompFile, err = remoteOmpExecutable(cfg)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("resolve host omp executable for remote runtime: %w", err)
		}
		if ompFile != nil {
			defer ompFile.Close()
		}
	}
	materials := execbackend.Materials{SourceWorktree: checkout}
	if backend == execbackend.Remote && strings.EqualFold(job.Type, "review") {
		diffBase, err := w.remoteReviewDiffBaseHEAD(ctx, job, checkout)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("resolve review diff base for job %s: %w", job.ID, err)
		}
		materials.DiffBaseHEAD = diffBase
	}
	lifecycle, err := w.ExecutionBackendFactory(backend, cfg)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("construct %s execution backend: %w", backend, err)
	}
	if backend == execbackend.Remote && strings.EqualFold(strings.TrimSpace(job.Type), "review") {
		latest, stateErr := w.Store.GetJob(context.WithoutCancel(ctx), job.ID)
		if stateErr != nil {
			return lifecycle, nil, nil, nil, fmt.Errorf("verify remote review %s before provider call: %w", job.ID, stateErr)
		}
		if latest.State != string(workflow.JobRunning) || latest.LifecycleGeneration != job.LifecycleGeneration {
			return lifecycle, nil, nil, nil, fmt.Errorf("provision remote execution backend for review job %s: durable state moved to %s generation %d: %w",
				job.ID, latest.State, latest.LifecycleGeneration, context.Canceled)
		}
	}
	if err := ctx.Err(); err != nil {
		return lifecycle, nil, nil, nil, fmt.Errorf("provision %s execution backend for job %s: %w", backend, job.ID, err)
	}
	instance, err := lifecycle.Provision(ctx, execbackend.JobScope{JobID: job.ID, LifecycleGeneration: job.LifecycleGeneration, TTL: ttl})
	if err != nil {
		// A provider can create an instance and then fail to persist its handle in
		// the ledger. Preserve both values so the caller installs its teardown defer
		// before handling the error; discarding them here strands a billed sandbox.
		return lifecycle, instance, nil, nil, fmt.Errorf("provision %s execution backend for job %s: %w", backend, job.ID, err)
	}

	if err := lifecycle.SyncIn(ctx, instance, materials); err != nil {
		return lifecycle, instance, nil, nil, fmt.Errorf("sync job %s into %s execution backend: %w", job.ID, backend, err)
	}
	if ompExecutable != "" {
		if err := installRemoteOmpRuntime(ctx, lifecycle, instance, ompExecutable, ompFile); err != nil {
			return lifecycle, instance, nil, nil, err
		}
	}
	var evidenceEnv []string
	if backend == execbackend.Remote && strings.EqualFold(strings.TrimSpace(job.Type), "review") {
		var err error
		evidenceEnv, err = w.installRemotePriorVerdicts(ctx, lifecycle, instance, job)
		if err != nil {
			return lifecycle, instance, nil, nil, err
		}
	}
	lease, env, err := w.provisionRemoteCredentialGateway(ctx, backend, runtimeName, job.ID, ttl, credentialPlan, lifecycle, instance)
	if err != nil {
		return lifecycle, instance, lease, nil, err
	}
	return lifecycle, instance, lease, append(env, evidenceEnv...), nil
}

// remotePriorVerdictsPath is where a remote review finds the same rendered,
// repo-scoped prior-verdict list a host read-only seat gets through
// GITMOOT_PRIOR_VERDICTS (#2281).
const remotePriorVerdictsPath = execbackend.RuntimeMaterialDir + "/evidence/prior-verdicts.json"

// installRemotePriorVerdicts gives a remote review the prior-verdict list
// that a host read-only seat stages in its cache root. The instance cannot
// read the host store or GitHub, so without this a review told to answer
// every prior finding can only end blocked. A render failure is not fatal,
// exactly as on the host seat, but it is recorded rather than silent.
func (w jobWorker) installRemotePriorVerdicts(ctx context.Context, lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance, job db.Job) ([]string, error) {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return nil, err
	}
	paths, err := w.configPaths()
	if err != nil {
		return nil, fmt.Errorf("resolve prior-verdict store for remote review: %w", err)
	}
	evidenceCtx, cancelEvidence := context.WithTimeout(ctx, 30*time.Second)
	body, diagnostic := renderPriorVerdictsFile(evidenceCtx, paths, payload.Repo)
	cancelEvidence()
	if body == nil {
		if diagnostic != "" && w.Store != nil {
			if eventErr := w.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{JobID: job.ID, Kind: "remote_review_evidence_unavailable", Message: diagnostic}); eventErr != nil {
				writeLine(w.Stdout, "job %s remote_review_evidence_unavailable event failed: %v", job.ID, eventErr)
			}
		}
		return nil, nil
	}
	installer, ok := lifecycle.(execbackend.InstanceFileInstaller)
	if !ok {
		return nil, fmt.Errorf("execution backend %q cannot install the prior-verdict list", lifecycle.Name())
	}
	if _, err := installer.InstallInstanceFile(ctx, instance, remotePriorVerdictsPath, bytes.NewReader(body), 0o600); err != nil {
		return nil, fmt.Errorf("install prior-verdict list in remote execution backend: %w", err)
	}
	return []string{"GITMOOT_PRIOR_VERDICTS=" + remotePriorVerdictsPath}, nil
}

// remoteOmpExecutable picks the host omp executable a remote omp job uploads.
// A sandboxd view uploads the file its section names for the provider's guest
// architecture (omp_linux_arm64_file or omp_linux_amd64_file), verified as a
// Linux ELF for that architecture and returned open so the verified bytes are
// the uploaded bytes. Cloud E2B uploads the host's own omp from PATH.
func remoteOmpExecutable(cfg config.RemoteExecConfig) (string, *os.File, error) {
	if !config.IsSandboxdProvider(cfg.Provider) {
		path, err := lookPathRemoteRuntime(runtime.OmpRuntime)
		return path, nil, err
	}
	section := config.SandboxdProviderSection(cfg.Provider)
	if cfg.OMPLinuxFile == "" {
		return "", nil, fmt.Errorf("%s sets neither omp_linux_arm64_file nor omp_linux_amd64_file: name the Linux omp executable for the provider's guest architecture", section)
	}
	file, err := execbackend.OpenLinuxExecutable(cfg.OMPLinuxFile, cfg.OMPGuestArch)
	if err != nil {
		return "", nil, fmt.Errorf("%s.%s: %w", section, config.OMPLinuxFileKey(cfg.OMPGuestArch), err)
	}
	return cfg.OMPLinuxFile, file, nil
}

func installRemoteOmpRuntime(ctx context.Context, lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance, source string, file *os.File) error {
	installer, ok := lifecycle.(execbackend.InstanceFileInstaller)
	if !ok {
		return fmt.Errorf("execution backend %q cannot install the omp runtime", lifecycle.Name())
	}
	if file == nil {
		var err error
		file, err = os.Open(source)
		if err != nil {
			return fmt.Errorf("open host omp executable: %w", err)
		}
		defer file.Close()
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat host omp executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("host omp executable %q is not an executable regular file", source)
	}
	if _, err := installer.InstallInstanceFile(ctx, instance, execbackend.RuntimeOmpExecutablePath, file, 0o700); err != nil {
		return fmt.Errorf("install omp runtime in remote execution backend: %w", err)
	}
	return nil
}

func (w jobWorker) remoteReviewDiffBaseHEAD(ctx context.Context, job db.Job, checkout string) (string, error) {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return "", err
	}
	if payload.PullRequest <= 0 {
		return "", nil
	}
	head := strings.TrimSpace(payload.HeadSHA)
	if head == "" {
		return "", fmt.Errorf("review job for PR #%d has no head SHA", payload.PullRequest)
	}
	git := gitutil.NewHostClient(checkout)
	head, err = git.RevParse(ctx, head+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve review head %q: %w", payload.HeadSHA, err)
	}

	base := ""
	if payload.ReviewScope != nil {
		base = strings.TrimSpace(payload.ReviewScope.PreviousHeadSHA)
	}
	if base == "" {
		pull, err := w.Store.GetPullRequest(ctx, payload.Repo, int64(payload.PullRequest))
		baseBranch := ""
		switch {
		case err == nil:
			baseBranch = strings.TrimSpace(pull.BaseBranch)
		case errors.Is(err, sql.ErrNoRows):
		default:
			return "", fmt.Errorf("load PR #%d: %w", payload.PullRequest, err)
		}
		if baseBranch == "" {
			// The watcher only caches PRs it has seen, and some cached rows
			// (e.g. recorded at merge time) carry no base branch. A merged or
			// older PR is still a valid exact-head review subject, so ask the
			// forge rather than refuse.
			repo, parseErr := github.ParseRepository(payload.Repo)
			if parseErr != nil {
				return "", fmt.Errorf("parse repo for PR #%d: %w", payload.PullRequest, parseErr)
			}
			forgePull, fetchErr := w.remoteReviewAdmissionClient(checkout, hostJobSubprocessRunner{}).GetPullRequest(ctx, repo, int64(payload.PullRequest))
			if fetchErr != nil {
				return "", fmt.Errorf("load PR #%d base from forge: %w", payload.PullRequest, fetchErr)
			}
			if strings.TrimSpace(forgePull.HeadSHA) != head {
				return "", fmt.Errorf("PR #%d head moved from %s to %s", payload.PullRequest, head, forgePull.HeadSHA)
			}
			baseBranch = strings.TrimSpace(forgePull.BaseRef)
		}
		if baseBranch == "" {
			return "", fmt.Errorf("PR #%d has no base branch", payload.PullRequest)
		}
		// A queued review resolves its scope from origin/<base>. A stale
		// remote-tracking ref silently widens that scope to commits already
		// merged into the base, so refresh it first and fail loudly rather
		// than hand the sandbox a wrong review subject.
		if err := git.FetchRemote(ctx, "origin"); err != nil {
			return "", fmt.Errorf("refresh origin before resolving PR #%d base: %w", payload.PullRequest, err)
		}
		base, err = git.MergeBase(ctx, "origin/"+baseBranch, head)
		if err != nil {
			return "", fmt.Errorf("resolve merge base of origin/%s and %s: %w", baseBranch, head, err)
		}
	} else {
		base, err = git.RevParse(ctx, base+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("resolve prior review head %q: %w", payload.ReviewScope.PreviousHeadSHA, err)
		}
	}
	ancestor, err := git.IsAncestor(ctx, base, head)
	if err != nil {
		return "", fmt.Errorf("verify review diff base %s: %w", base, err)
	}
	if !ancestor {
		return "", fmt.Errorf("review diff base %s is not an ancestor of head %s", base, head)
	}
	return base, nil
}

func executionChangeSetCollector(lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance, liveBackend execbackend.Backend, liveJobID string) func(context.Context, execbackend.Backend, string) (*execbackend.ChangeSet, error) {
	if lifecycle == nil || instance == nil {
		return nil
	}
	return func(ctx context.Context, requestedBackend execbackend.Backend, requestedJobID string) (*execbackend.ChangeSet, error) {
		if requestedBackend != liveBackend {
			return nil, fmt.Errorf("collect changeset backend %q does not match live instance backend %q", requestedBackend, liveBackend)
		}
		if requestedJobID != liveJobID {
			return nil, fmt.Errorf("collect changeset job %q does not own live instance for job %q", requestedJobID, liveJobID)
		}
		changes, err := lifecycle.Collect(ctx, instance)
		if err != nil {
			return nil, err
		}
		return &changes, nil
	}
}

func (w jobWorker) destroyExecutionBackend(jobID string, lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance) {
	if lifecycle == nil || instance == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), executionBackendDestroyTimeout)
	defer cancel()
	cancelled := false
	if w.Store != nil {
		if latest, err := w.Store.GetJob(ctx, jobID); err == nil {
			cancelled = latest.State == string(workflow.JobCancelled)
		}
	}
	var err error
	if cancelled {
		err = lifecycle.Cancel(ctx, instance)
	} else {
		err = lifecycle.Destroy(ctx, instance)
	}
	if err == nil {
		return
	}
	writeLine(w.Stdout, "job %s execution backend teardown failed: %v", jobID, err)
	if w.Store != nil {
		_ = w.Store.AddJobEvent(context.Background(), db.JobEvent{JobID: jobID, Kind: "execbackend_destroy_failed", Message: err.Error()})
	}
}

func (w *jobWorker) executionDeliveryAdapter(agent runtime.Agent, checkout string, outputs ...io.Writer) (workflow.DeliveryAdapter, error) {
	runner := w.executionRunner
	if runner == nil {
		return nil, errors.New("execution-backend runtime runner is required")
	}
	// Keep the run-scoped base runner so a later live-output adapter rebuild
	// preserves backend execution.
	w.executionRunner = runner
	if len(outputs) > 0 && outputs[0] != nil {
		stream, ok := runner.(subprocess.StreamRunner)
		if !ok {
			return nil, errors.New("execution-backend runtime runner does not support streaming")
		}
		runner = subprocess.TeeRunner{Inner: stream, Out: pipeline.RuntimeOutputWriter(outputs...)}
	}
	return buildRuntimeAdapter(w.ConfigHome, agent, checkout, runner)
}

// shouldRenewSandboxTTL decides whether a sandbox's provider TTL is still worth
// extending. It is the liveness half of the keepalive added in #2226, which
// until now renewed on the deadline alone.
//
// TWO CASES STOP RENEWAL, and neither stops the JOB:
//
//   - the job has left the running state, so nothing is waiting on the sandbox;
//   - its delegation root was killed, which by contract lets in-flight work
//     finish but should not buy it more provider time.
//
// `job kill` is graceful by design (workflow/job_kill.go: "it does NOT cancel
// in-flight jobs"), so a killed tree's running remote job keeps its sandbox.
// That was harmless while the sandbox lapsed at the provider's one-hour ceiling;
// with the keepalive it became a three-hour billed instance for a review the
// operator had already killed. Round 2 review of #2226 named that amplification.
//
// FAILS OPEN, deliberately. If the store cannot answer, renew: cutting a healthy
// review's sandbox short on a transient read error is a worse failure than
// paying for a few extra minutes, and the deadline still bounds the total.
func (w jobWorker) shouldRenewSandboxTTL(jobID string) bool {
	if w.Store == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxTTLLivenessTimeout)
	defer cancel()
	job, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		return true
	}
	if job.State != string(workflow.JobRunning) {
		return false
	}
	killed, err := w.Store.IsRootJobKilled(ctx, rootJobIDForTTLLiveness(job))
	if err != nil {
		return true
	}
	return !killed
}

// rootJobIDForTTLLiveness resolves the delegation root whose killed flag governs
// this job, falling back to the job itself when it is its own root.
func rootJobIDForTTLLiveness(job db.Job) string {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return job.ID
	}
	if root := strings.TrimSpace(payload.RootJobID); root != "" {
		return root
	}
	return job.ID
}

// sandboxTTLLivenessTimeout bounds the liveness read so a slow store cannot
// delay a renewal past its lead.
const sandboxTTLLivenessTimeout = 5 * time.Second

// remoteCapableRuntimes is the ONE authoritative answer to "may this runtime
// run on the remote execution backend".
//
// It replaces three independent copies of the same allowlist (#2234 review):
// provisionExecutionBackend here, provisionRemoteCredentialGateway in
// execbackend_credentials.go, and validateRuntimeExecutionBackend in
// agent_dispatch.go. The drift directions are asymmetric: dispatch admitting a
// runtime that the gateway rejects fails only after cost reservation and
// provisioning. Every predicate and refusal message must derive from this list.
var remoteCapableRuntimes = [...]string{
	runtime.ShellRuntime,
	runtime.OmpRuntime,
}

func remoteCapableRuntime(runtimeName string) bool {
	return slices.Contains(remoteCapableRuntimes[:], strings.TrimSpace(runtimeName))
}

func remoteCapableRuntimeNames() string {
	return strings.Join(remoteCapableRuntimes[:], " and ")
}
