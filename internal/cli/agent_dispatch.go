package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

var newAgentDispatchGitHubClient = func(checkout string) github.Client {
	return github.NewClient(checkout)
}

type foregroundRuntimeAdapterFactory func(string, runtime.Agent, string) (runtime.Adapter, error)

var localAgentDispatchRuntimeAdapterFor foregroundRuntimeAdapterFactory = func(home string, agent runtime.Agent, checkout string) (runtime.Adapter, error) {
	return runtimeAdapterFor(home, agent.Runtime, checkout)
}

var localAgentDispatchExecBackendFor = func(home string) (execbackend.Backend, error) {
	return (jobWorker{ConfigHome: home, ConfigHomeExplicit: true}).resolveExecBackend("", false)
}

func foregroundRuntimeAdapterFactoryFor(backend execbackend.Backend) (foregroundRuntimeAdapterFactory, error) {
	return execbackend.Consume(backend, func() (foregroundRuntimeAdapterFactory, error) {
		return localAgentDispatchRuntimeAdapterFor, nil
	})
}

var dispatchPromptHeadContradictionWarnings = promptHeadContradictionWarnings

var allocateDispatchReadOnlyWorktree = workflow.AllocateReadOnlyWorktree

var fetchDispatchReviewPullRequest = func(ctx context.Context, git gitutil.Client, pullRequest int) error {
	return git.FetchPullRequest(ctx, "origin", pullRequest)
}

const reviewReadOnlyWorktreeMinFreeBytes uint64 = 5 << 30

type localAgentDispatchRequest struct {
	RepoFlag       string
	Agent          string
	Action         string
	Instructions   string
	Background     bool
	Type           string
	Model          string
	Effort         string
	WorkflowID     string
	ActingOrgRole  string
	OperatorOrigin bool
	// Runtime, when non-empty, is the per-job runtime override (#531): this one
	// job runs through the named runtime while the agent's registered default
	// runtime (and its session) stays untouched. RuntimeSession optionally names
	// the session on the OVERRIDE runtime (required for shell, whose sessions
	// are commands); when empty a fresh per-job session ref is minted so the
	// overridden job can never resume the agent's default-runtime session.
	Runtime          string
	RuntimeSession   string
	Home             string
	AllowManagedSync bool
	JobTimeout       time.Duration
	TaskID           string
	PullRequest      int
	PullRequestReady bool
	HeadSHA          string
	// ImplementBase is the CLI/config worktree base for implement dispatches.
	// Before the request can enqueue, it is resolved to a commit SHA and
	// ImplementBaseResolved is set so allocation uses that exact commit.
	ImplementBase          string
	ImplementBaseResolved  bool
	ImplementPRValidated   bool
	Branch                 string
	GoalID                 string
	TaskTitle              string
	LeadAgent              string
	Reviewers              []string
	Cockpit                bool
	CockpitSession         string
	SkipNativeReviewFanout bool
	Recipe                 string
	SelectedAction         string
	SelectedActionReason   string
	ExecutionPath          string
	// ThreadID / ChatMessageID link a chat-promoted job (#534) back to the thread
	// and the promotion_request message it came from. Set only by `chat task`;
	// empty for every other dispatch, so the enqueued payload is byte-identical.
	ThreadID      string
	ChatMessageID string
	// MootSeat marks a `gitmoot moot` conversing seat (#732). Set ONLY by the moot
	// dispatch — never by `chat task` or any other chat-linked dispatch — so only a
	// real seat is elevated + relay-injected by the daemon. Additive: false leaves
	// the enqueued payload byte-identical.
	MootSeat bool
	// JSONOutput is true when the caller will emit machine-readable JSON (e.g.
	// `agent ask --json`). The live-A/B interceptor (#482) MUST stay byte-clean for
	// these consumers: it never presents the A/B block (which would prepend
	// "[live A/B] ..." to the JSON object and break parsing) and never runs the
	// second challenger Deliver, falling through to the plain single ask.
	JSONOutput bool
	// DispatchWarning surfaces advisory pre-delivery checks to the operator. It
	// is deliberately not persisted in the job payload.
	DispatchWarning func(string)
	// ReviewTaskHeadDivergence, when non-empty, records that a review rebound to
	// the task owning (repo, branch) even though that task's registered checkout
	// HEAD differs from the requested head (#1530): a fix-worktree leg pushed the
	// branch from an independent clone and left the registered checkout behind.
	// dispatchLocalAgentJob inserts it atomically with the queued job as a
	// review_task_head_divergence event, so a runnable review can never exist
	// without the required audit. It is set by prepareLocalReviewTask and never
	// persisted in the job payload.
	ReviewTaskHeadDivergence string
	jobRunner                subprocess.Runner
}

func localDispatchJobRunner(request localAgentDispatchRequest) subprocess.Runner {
	if request.jobRunner != nil {
		return request.jobRunner
	}
	// Direct helper callers are operator-side setup and tests, not admitted job
	// execution. dispatchLocalAgentJob always installs the resolved backend runner
	// before reaching these helpers.
	return subprocess.ExecRunner{}
}

var promptCommitTokenRE = regexp.MustCompile(`\b[0-9a-fA-F]{7,64}\b`)

type localAgentJobOutput struct {
	JobID                string                `json:"job_id"`
	State                string                `json:"state"`
	Repo                 string                `json:"repo"`
	Agent                string                `json:"agent"`
	Action               string                `json:"action"`
	SelectedAction       string                `json:"selected_action,omitempty"`
	SelectedActionReason string                `json:"selected_action_reason,omitempty"`
	ExecutionPath        string                `json:"execution_path,omitempty"`
	Result               *workflow.AgentResult `json:"result,omitempty"`
	RawOutputCount       int                   `json:"raw_output_count"`
	WatchCommand         string                `json:"watch_command,omitempty"`
	DaemonRunning        bool                  `json:"daemon_running,omitempty"`
	// AdvanceError is set only when the agent delivery + job succeeded
	// terminally but a benign post-success advance step errored (e.g. a
	// merge-gate block on a freshly-opened PR, or a 422 "PR already exists"
	// race). The terminal-success result is still surfaced; this carries the
	// advance warning so it is not silently lost.
	AdvanceError string `json:"advance_error,omitempty"`
}

func dispatchLocalAgentJob(ctx context.Context, store *db.Store, request localAgentDispatchRequest) (localAgentJobOutput, error) {
	// Validate a requested per-job runtime override FIRST — an unknown runtime
	// (or a shell override without a session command) must fail with a clear
	// error before any job is enqueued or any repo/agent state is touched.
	overrideRuntime, overrideRef, err := resolveJobRuntimeOverride(request.Runtime, request.RuntimeSession)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	// Dispatch performs checkout and git preparation before enqueue, so resolve
	// the execution backend before any of those job-associated subprocesses.
	// The claiming worker resolves it again from the durable payload/config at
	// execution time; this ingress decision governs only pre-enqueue work.
	var foregroundAdapterFactory foregroundRuntimeAdapterFactory
	execBackend, err := localAgentDispatchExecBackendFor(request.Home)
	if err != nil {
		if !request.Background {
			return localAgentJobOutput{}, err
		}
		// Preserve background dispatch's durable fail-loud contract: invalid
		// configuration is recorded by the claiming worker on the queued job. The
		// ingress-only checkout preparation is explicitly host-side in this case;
		// the invalid selection can never reach job execution.
		request.jobRunner = subprocess.ExecRunner{}
	} else {
		request.jobRunner, err = jobSubprocessRunnerForBackend(execBackend)
		if err != nil {
			return localAgentJobOutput{}, err
		}
	}
	if !request.Background {
		foregroundAdapterFactory, err = foregroundRuntimeAdapterFactoryFor(execBackend)
		if err != nil {
			return localAgentJobOutput{}, err
		}
	}
	// #1059: validate the --org-role against the registry (unknown role fails
	// loudly at ingress) and record passive presence for this dispatch. Restored
	// during the #1057 reconcile — #1057's preflightOrgScope covers scope
	// enforcement but not role-existence validation or presence.
	if err := validateAndTouchActingOrgRole(ctx, store, request.Home, request.ActingOrgRole, request.ExecutionPath); err != nil {
		return localAgentJobOutput{}, err
	}
	repo, record, err := resolveLocalAgentRepo(ctx, store, request.RepoFlag)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	// Resolve once per human dispatch. The snapshot is shared by the mutation
	// preflight and the enqueue chokepoint so both see precisely one registry.
	orgPolicy := orgPolicyResolver(request.Home)
	var resolvedOrgPolicy workflow.OrgEnforcement
	if request.OperatorOrigin {
		resolvedOrgPolicy = orgPolicy(repo.FullName())
		orgPolicy = fixedOrgPolicy(resolvedOrgPolicy)
		request.ActingOrgRole = workflow.NormalizeActingOrgRole(request.ActingOrgRole)
	}
	// Strict workflow rejection must happen before repo/task/worktree/branch-lock
	// mutation. Otherwise a retry without --workflow strands a fresh adhoc task.
	if request.Action == "implement" {
		if err := preflightStrictWorkflowPolicy(request.Home, repo.FullName(), request.WorkflowID, ""); err != nil {
			return localAgentJobOutput{}, err
		}
	}
	// Implement and review both allocate durable state before enqueue. Check the
	// shared decision first so a block never strands a task or worktree.
	if request.Action == "implement" || request.Action == "review" {
		if err := preflightOrgScope(resolvedOrgPolicy, repo.FullName(), request.ActingOrgRole, request.OperatorOrigin); err != nil {
			return localAgentJobOutput{}, err
		}
	}
	// A review that may return changes_requested must name a fix target that can
	// actually implement before Gitmoot spends a review session. Validate the
	// agents-table row here, before repo/task/worktree mutation, managed-agent
	// provisioning, enqueue, or runtime delivery. The workflow preflight remains
	// the universal defense for later policy changes.
	if request.Action == "review" {
		request, err = validateLocalReviewLeadAtDispatch(ctx, store, request, repo.FullName())
		if err != nil {
			return localAgentJobOutput{}, err
		}
	}
	if request.Action == "implement" {
		paths, err := pathsFromFlag(request.Home)
		if err != nil {
			return localAgentJobOutput{}, err
		}
		// Resolve only an explicit CLI/config base at this early seam. Permission-
		// blocked implement jobs never allocate a worktree, and historically they
		// can be recorded even for an unborn checkout with no HEAD. Runnable jobs
		// resolve the implicit HEAD and run the stale-checkout guard in prepare.
		base := strings.TrimSpace(request.ImplementBase)
		if base == "" {
			base, err = config.LoadImplementBase(paths)
			if err != nil {
				return localAgentJobOutput{}, fmt.Errorf("load workflow implement_base: %w", err)
			}
		}
		if base != "" {
			request.ImplementBase, err = resolveLocalImplementBaseForRunner(ctx, paths, record, base, localDispatchJobRunner(request))
			if err != nil {
				return localAgentJobOutput{}, err
			}
			request.ImplementBaseResolved = true
		}
		if request.PullRequest > 0 {
			request, err = bindLocalImplementRequestToPullRequest(ctx, store, record, repo, request)
			if err != nil {
				return localAgentJobOutput{}, err
			}
			request.ImplementPRValidated = true
		}
	}
	if err := store.UpsertRepo(ctx, record); err != nil {
		return localAgentJobOutput{}, err
	}
	var checkoutPath string
	checkoutPath = record.CheckoutPath
	if agent, blocked, err := readOnlyManagedImplementationBlock(ctx, store, request, repo.FullName()); err != nil {
		return localAgentJobOutput{}, err
	} else if blocked {
		return enqueuePermissionBlockedLocalAgentJob(ctx, store, request, repo.FullName(), record.DefaultBranch, agent.Name, overrideRuntime, overrideRef, orgPolicy)
	}
	agent, releaseAgentReservation, err := resolveLocalDispatchAgent(ctx, store, request, repo.FullName(), record)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	reservationReleased := false
	releaseReservation := func(releaseCtx context.Context) error {
		if reservationReleased {
			return nil
		}
		if err := releaseAgentReservation(releaseCtx); err != nil {
			return err
		}
		reservationReleased = true
		return nil
	}
	defer func() {
		_ = releaseReservation(context.Background())
	}()
	if err := ensureLocalAgentAccess(ctx, store, agent, repo.FullName(), request.Action); err != nil {
		return localAgentJobOutput{}, err
	}
	request.Agent = agent.Name
	// The EFFECTIVE runtime agent this job runs as: identical to the registered
	// agent unless a per-job runtime override is present (#531), in which case
	// it carries the override runtime + the job's own session ref (never the
	// agent's default-runtime session) and no default model.
	effectiveAgent := applyJobRuntimeOverride(runtimeAgent(agent), workflow.JobPayload{RuntimeOverride: overrideRuntime, RuntimeOverrideRef: overrideRef})
	if overrideRuntime != "" {
		if err := runtime.ValidateAgent(effectiveAgent); err != nil {
			return localAgentJobOutput{}, fmt.Errorf("runtime override: %w", err)
		}
	}
	if !request.Background {
		// The adapter factory closure already carries this selection, but the
		// workflow engine also owns job-associated subprocesses (produce checks and
		// result observation). Stamp the same resolved decision on the effective
		// foreground agent so those routes cannot silently default back to Local.
		effectiveAgent.ExecBackend = string(execBackend)
	}
	if readOnlyImplementationBlocked(request.Action, effectiveAgent) {
		return enqueuePermissionBlockedLocalAgentJob(ctx, store, request, repo.FullName(), record.DefaultBranch, agent.Name, overrideRuntime, overrideRef, orgPolicy)
	}
	var foregroundContract *runtime.RuntimeContractResult
	if !request.Background {
		result, err := runtimeContractPreflightForBackend(execBackend, func() runtime.RuntimeContractResult {
			return localRuntimeContractPreflight(ctx, effectiveAgent)
		})
		if err != nil {
			return localAgentJobOutput{}, err
		}
		if err := runtime.RuntimeContractDispatchError(effectiveAgent, result); err != nil {
			return localAgentJobOutput{}, err
		}
		foregroundContract = &result
	}
	switch request.Action {
	case "review":
		if err := reviewReadOnlyWorktreeCapacity(request.Home); err != nil {
			return localAgentJobOutput{}, err
		}
		var err error
		request, err = prepareLocalReviewDispatchRequest(ctx, store, record, repo, request)
		if err != nil {
			return localAgentJobOutput{}, err
		}
	case "implement":
		var task db.Task
		var err error
		task, request, err = prepareLocalImplementDispatchRequest(ctx, store, record, repo, request)
		if err != nil {
			return localAgentJobOutput{}, err
		}
		if strings.TrimSpace(task.WorktreePath) != "" {
			checkoutPath = task.WorktreePath
		}
	}
	var promptHeadWarnings []string
	if request.Action != "review" {
		// Keep ask and implement on the pre-allocation scanner seam: ask must scan
		// the canonical checkout with its inherited dispatch head before its
		// committed-tip worktree clears that head. Only review scans its newly
		// allocated exact-head checkout below.
		promptHeadWarnings = dispatchPromptHeadContradictionWarnings(ctx, jobGitClient(checkoutPath, localDispatchJobRunner(request)), request.Instructions, request.HeadSHA)
	}
	// A --recipe routes this coordinator to a named built-in recipe template's
	// prompt (resolved from the installed-template store) without rebinding the
	// agent; the override is captured into the job payload at enqueue time.
	var recipeTemplate *db.AgentTemplate
	if strings.TrimSpace(request.Recipe) != "" {
		tmpl, err := loadInstalledTemplate(ctx, store, request.Recipe)
		if err != nil {
			return localAgentJobOutput{}, err
		}
		recipeTemplate = &tmpl
	}
	// Give an eligible read-only job its own detached worktree BEFORE enqueue so
	// its checkout key is worktree:<path> (queuedJobCheckoutKey) and same-repo
	// seats (moot, chat-task, autorespond, `agent ask --background`) run
	// concurrently instead of serializing on the shared repo:<repo> key. Foreground
	// asks run inline and never serialize, so they are left untouched. Ask remains
	// FAIL-OPEN because its isolation is a throughput optimization. Review is
	// FAIL-CLOSED because falling back to its shared checkout would review a commit
	// other than the requested head.
	jobID := localAgentJobID(request.Action, agent.Name)
	readOnlyWorktreePath, readOnlyWorktreeErr := maybeAllocateDispatchReadOnlyWorktree(ctx, store, request, repo.FullName(), record.CheckoutPath, jobID)
	if request.Action == "review" {
		if readOnlyWorktreeErr != nil {
			return localAgentJobOutput{}, fmt.Errorf("allocate exact-head review worktree: %w", readOnlyWorktreeErr)
		}
		if strings.TrimSpace(readOnlyWorktreePath) == "" {
			return localAgentJobOutput{}, errors.New("allocate exact-head review worktree: no worktree was allocated")
		}
		checkoutPath = readOnlyWorktreePath
		promptHeadWarnings = dispatchPromptHeadContradictionWarnings(ctx, jobGitClient(checkoutPath, localDispatchJobRunner(request)), request.Instructions, request.HeadSHA)
	}
	if readOnlyWorktreePath != "" {
		if request.Action != "review" {
			// An ask worktree is the committed tip of checkout HEAD, which may have
			// advanced past its inherited HeadSHA. Review is different: its worktree
			// was allocated at that exact SHA, so the binding must remain in payload.
			request.HeadSHA = ""
			if note := workflow.ReadOnlyWorktreeContextNote(record.CheckoutPath); note != "" {
				request.Instructions += note
			}
		}
	}
	// A foreground dispatch already knows the runtime it will execute. Persist it
	// in the initial job insert so recording cannot fail separately and leave a
	// daemon-claimable queued row. Background jobs deliberately omit it here: the
	// daemon records the runtime selected when execution actually starts. A review
	// deferred after enqueue by runtime contention is likewise refreshed by the
	// daemon before that later execution.
	effectiveRuntimeAtEnqueue := ""
	if !request.Background {
		effectiveRuntimeAtEnqueue = effectiveAgent.Runtime
	}
	var requiredEvents []db.JobEvent
	if divergence := strings.TrimSpace(request.ReviewTaskHeadDivergence); divergence != "" {
		requiredEvents = append(requiredEvents, db.JobEvent{Kind: "review_task_head_divergence", Message: divergence})
	}
	job, err := (workflow.Mailbox{Store: store, CanaryEnabled: canaryRoutingEnabled(request.Home), RuntimeDefaultModel: runtimeDefaultModelResolver(request.Home), RequireWorkflowPolicy: requireWorkflowPolicyResolver(request.Home), OrgPolicy: orgPolicy}).Enqueue(ctx, workflow.JobRequest{
		ID:                     jobID,
		Agent:                  agent.Name,
		Action:                 request.Action,
		Repo:                   repo.FullName(),
		Branch:                 firstNonEmpty(request.Branch, record.DefaultBranch),
		PullRequest:            request.PullRequest,
		PullRequestReady:       request.PullRequestReady,
		HeadSHA:                request.HeadSHA,
		GoalID:                 request.GoalID,
		TaskID:                 request.TaskID,
		TaskTitle:              request.TaskTitle,
		LeadAgent:              firstNonEmpty(request.LeadAgent, agent.Name),
		Reviewers:              request.Reviewers,
		Sender:                 "local",
		ActingOrgRole:          request.ActingOrgRole,
		OperatorOrigin:         request.OperatorOrigin,
		Instructions:           request.Instructions,
		Model:                  request.Model,
		Effort:                 request.Effort,
		WorkflowID:             request.WorkflowID,
		RuntimeOverride:        overrideRuntime,
		RuntimeOverrideRef:     overrideRef,
		EffectiveRuntime:       effectiveRuntimeAtEnqueue,
		RequiredEvents:         requiredEvents,
		Cockpit:                request.Cockpit,
		CockpitSession:         request.CockpitSession,
		SkipNativeReviewFanout: request.SkipNativeReviewFanout,
		ValidatedPullRequest:   request.ImplementPRValidated,
		TemplateOverride:       recipeTemplate,
		ThreadID:               request.ThreadID,
		ChatMessageID:          request.ChatMessageID,
		MootSeat:               request.MootSeat,
		WorktreePath:           readOnlyWorktreePath,
		ReadOnlyWorktree:       readOnlyWorktreePath != "",
	})
	if err != nil {
		// #739: the read-only worktree is created on disk BEFORE Enqueue. If Enqueue
		// fails there is no job row, so neither the terminal AdvanceJob cleanup nor the
		// daemon reclaim pass will ever dispose it — roll it back here or it leaks a
		// detached worktree (+ its .git/worktrees admin entry) with no owner. Detached
		// from the (possibly cancelled) request context so removal still runs; the
		// reactive pool-isolation path defends its own allocation the same way.
		if readOnlyWorktreePath != "" {
			_ = jobGitClient(record.CheckoutPath, localDispatchJobRunner(request)).RemoveWorktreeForce(context.WithoutCancel(ctx), readOnlyWorktreePath)
		}
		return localAgentJobOutput{}, err
	}
	if foregroundContract != nil && foregroundContract.State == runtime.RuntimeContractUnknown {
		_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_contract_unknown", Message: runtime.RuntimeContractEventMessage(job.ID, effectiveAgent, *foregroundContract)})
	}
	if err := releaseReservation(ctx); err != nil {
		return localAgentJobOutput{}, err
	}
	for _, warning := range promptHeadWarnings {
		_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "prompt_head_warning", Message: warning})
		if request.DispatchWarning != nil {
			request.DispatchWarning(warning)
		}
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "route_selected", Message: routeSelectedMessage(request)}); err != nil {
		return localAgentJobOutput{}, err
	}
	// Emit the #739 read-only isolation outcome now that the job row exists (job
	// events carry a JobID FK). Allocated → observable worktree:<path> key; a
	// fail-open skip → loud event so a lost-parallelism serialize is never silent.
	if readOnlyWorktreePath != "" {
		_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "readonly_worktree_allocated", Message: fmt.Sprintf("read-only worktree %s allocated at dispatch (#739); job keyed worktree:<path> to run beside same-repo seats", readOnlyWorktreePath)})
	} else if readOnlyWorktreeErr != nil {
		_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "readonly_worktree_skipped", Message: fmt.Sprintf("read-only worktree isolation skipped (#739); job runs serialized in the shared checkout: %v", readOnlyWorktreeErr)})
	}
	if overrideRuntime == "" {
		effectiveAgent = scopeRegisteredFreshRefForJob(effectiveAgent, job.ID)
	}
	if request.Background {
		return localAgentJobOutput{
			JobID:                job.ID,
			State:                job.State,
			Repo:                 repo.FullName(),
			Agent:                job.Agent,
			Action:               job.Type,
			SelectedAction:       request.SelectedAction,
			SelectedActionReason: request.SelectedActionReason,
			ExecutionPath:        request.ExecutionPath,
			RawOutputCount:       0,
		}, nil
	}
	managed, err := localManagedAgentDispatchConfigForAgent(ctx, store, request.Home, agent.Name)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	lockTTL := daemonRunningJobStaleAfter
	jobTimeout := request.JobTimeout
	if managed.OK {
		jobTimeout = managed.JobTimeout
	}
	// Mirror run()/runWithTempWorker(): size the runtime-session lease to
	// jobTimeout + a teardown grace so a foreground `agent ask` that hits its
	// timeout releases the lock before the lease expires. Otherwise the lease
	// (== the run-context deadline) would expire mid-teardown and the stale
	// reaper could requeue the still-live job — the #536 double-run window.
	if jobTimeout > 0 {
		lockTTL = jobTimeout + runtimeLeaseTeardownGrace
	}
	// SESSION SAFETY (#531): the lock is taken on the EFFECTIVE agent, so an
	// overridden job locks the OVERRIDE runtime's session key and can never
	// collide with (or occupy) the agent's default-runtime session lock.
	releaseLock, acquired, lockKey, ownerToken, err := acquireJobRuntimeSessionLock(ctx, store, job.ID, effectiveAgent, overrideRuntime != "", time.Now().UTC(), lockTTL)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	if !acquired {
		// #684 failure mode B: a review is naturally asynchronous — a busy runtime
		// session just means "run me a moment later", exactly the daemon's own
		// runtime-busy handling (it bounces errRuntimeSessionBusy and keeps the job
		// QUEUED). Rather than cancelling and dropping a foreground review when the
		// serialized runtime session is busy (the reported "queued job … was not run"
		// drop), LEAVE it QUEUED so the daemon runs it when the session frees. Ask /
		// implement stay synchronous (the caller is waiting on the answer / the
		// mutation) and keep the existing cancel-and-report behavior byte-identically.
		if request.Action == "review" {
			waitMessage := fmt.Sprintf("runtime session %s is busy; review job %s left queued for the daemon to run when the session frees", lockKey, job.ID)
			_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_lock_wait", Message: waitMessage})
			_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "requeued_runtime_busy", Message: waitMessage})
			return buildLocalAgentJobOutput(job, request)
		}
		message := fmt.Sprintf("runtime session %s is busy; synchronous ask was not run", lockKey)
		_, _ = store.TransitionJobStateWithEvent(ctx, job.ID, string(workflow.JobQueued), string(workflow.JobCancelled), db.JobEvent{
			JobID:   job.ID,
			Kind:    string(workflow.JobCancelled),
			Message: message,
		})
		_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_lock_wait", Message: message})
		return localAgentJobOutput{}, fmt.Errorf("runtime session %s is busy; queued job %s was not run", lockKey, job.ID)
	}
	defer func() {
		_ = releaseLock(context.Background())
	}()
	stopRuntimeLockHeartbeat := startRuntimeSessionLockHeartbeat(ctx, store, lockKey, ownerToken, lockTTL)
	defer stopRuntimeLockHeartbeat()
	// Thread the owner token so a foreground run's terminal cleanup (RunJob ->
	// AdvanceJob, which fires while this lock is still held) recognizes its own lock
	// and does not refuse the healthy-path cleanup as a foreign live owner (#536).
	ctx = workflow.WithRuntimeSelfOwnerToken(ctx, ownerToken)
	// Adapter selection uses the complete EFFECTIVE agent: runtime overrides
	// select the adapter (#531), while the resolved execution backend selects
	// where that adapter runs (#1536). Neither decision may be discarded here.
	adapter, err := foregroundAdapterFactory(request.Home, effectiveAgent, checkoutPath)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	// Foreground dispatch bypasses the daemon worker, so attach opt-in retained
	// capture explicitly. Open/composition failures remain fail-open.
	_, retainedLogFile, retainedLogErr := openRetainedTranscriptLog(request.Home, job.ID)
	if retainedLogErr == nil && retainedLogFile != nil {
		teeAdapter, teeErr := appendDeliveryAdapterOutput(adapter, retainedLogFile)
		if teeErr != nil {
			_ = retainedLogFile.Close()
		} else if runtimeAdapter, ok := teeAdapter.(runtime.Adapter); ok {
			adapter = runtimeAdapter
			defer func() { _ = retainedLogFile.Close() }()
		} else {
			_ = retainedLogFile.Close()
		}
	}
	runCtx := ctx
	if managed.OK {
		now := time.Now().UTC()
		if err := store.MarkAgentInstanceRunning(ctx, agent.Name, now, managed.JobTimeout); err != nil {
			return localAgentJobOutput{}, err
		}
		defer func() {
			_ = store.TouchAgentInstance(context.Background(), agent.Name, time.Now().UTC(), managed.IdleTimeout)
		}()
	}
	if jobTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, jobTimeout)
		defer cancel()
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	// Journal runtime selection for every job. Only a real per-job override is
	// labelled runtime_override; default selection uses effective_runtime.
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: jobRuntimeEventKind(overrideRuntime != ""), Message: jobRuntimeOverrideEventMessage(agent.Runtime, effectiveAgent, lockKey)}); err != nil {
		return localAgentJobOutput{}, err
	}
	quotaHooks := newQuotaRoleUnavailableHooks(store, request.Home, io.Discard)
	recordRuntimeOutcome := func(runErr error) {
		// Availability bookkeeping is deliberately best-effort here, as it is in
		// the daemon worker: it must never replace the foreground job's outcome.
		_ = quotaHooks.recordRuntimeOutcome(ctx, job, payload, effectiveAgent, runErr, time.Now().UTC())
	}
	if request.Action == "ask" {
		// Live-traffic A/B interception (#482). Off by default: when
		// [skillopt].live_ab_sample_rate is 0 (every existing home + DefaultConfig),
		// the agent is unmanaged, the bandit floor is not met, or the sampling die
		// misses, maybeRunLiveAB returns handled=false and the EXACT single
		// Mailbox.Run below runs unchanged (byte-identical, no extra Deliver). It
		// reuses the runtime-session lock already held from acquireRuntimeSessionLock
		// above — no second lock acquisition — so the two serialized Deliver calls
		// can never self-deadlock on "session is busy".
		// A runtime-overridden ask skips the live-A/B interceptor: the A/B
		// compares template variants on the agent's OWN runtime session, which an
		// override job deliberately does not run on.
		handled := false
		if overrideRuntime == "" {
			var abErr error
			handled, abErr = maybeRunLiveAB(runCtx, store, request, agent, job, adapter, managed.OK, execBackend)
			if abErr != nil {
				recordRuntimeOutcome(abErr)
				return localAgentJobOutput{}, foregroundAskTimeoutError(runCtx, jobTimeout, abErr)
			}
		}
		if !handled {
			// Wire the home-aware registry defaults so a foreground ask with no
			// agent/job model or effort pin honors the runtime's defaults too.
			// Fail-open/empty by default; an agent/job pin wins.
			mailbox := workflow.Mailbox{Store: store, RuntimeDefaultModel: runtimeDefaultModelResolver(request.Home), RuntimeDefaultEffort: runtimeDefaultEffortResolver(request.Home)}
			if _, err := mailbox.Run(runCtx, job.ID, effectiveAgent, adapter); err != nil {
				recordRuntimeOutcome(err)
				return localAgentJobOutput{}, foregroundAskTimeoutError(runCtx, jobTimeout, err)
			}
			recordRuntimeOutcome(nil)
		}
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_completed", Message: "workflow advancement completed"}); err != nil {
			return localAgentJobOutput{}, err
		}
	} else {
		workflowHome := ""
		if paths, err := pathsFromFlag(request.Home); err == nil {
			workflowHome = paths.Home
		}
		engine := daemonWorkflowEngineForRunner(store, newAgentDispatchGitHubClient(checkoutPath), checkoutPath, workflowHome, localDispatchJobRunner(request))
		if _, err := engine.RunJob(runCtx, job.ID, effectiveAgent, adapter); err != nil {
			if out, ok, _ := recoverAdvanceErrorOutput(ctx, store, job.ID, request, err); ok {
				recordRuntimeOutcome(nil)
				return out, nil
			}
			recordRuntimeOutcome(err)
			return localAgentJobOutput{}, err
		}
		recordRuntimeOutcome(nil)
	}
	latest, err := store.GetJob(ctx, job.ID)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	return buildLocalAgentJobOutput(latest, request)
}

// localRuntimeContractPreflight is the FOREGROUND dispatch preflight. It is
// request-shaped on purpose, matching the daemon worker's RuntimePreflight, so the
// repo has ONE shape for one decision rather than two: the daemon threads
// payload.Plan, and this must thread the request's plan the moment a CLI surface
// can carry one.
//
// The empty request is correct TODAY and only today: no CLI flag reaches plan mode
// (gitmoot#1425 owes that surface), so a foreground dispatch can never be a plan
// request. When #1425 lands `--plan`, this call MUST pass it — otherwise the plan
// flags are dispatched to an omp CLI whose support was never verified, and the
// guard fails silently rather than loudly. That is the whole failure mode this
// contract exists to prevent, so it is named here at the call site.
var localRuntimeContractPreflight = func(ctx context.Context, agent runtime.Agent) runtime.RuntimeContractResult {
	return runtime.DefaultRuntimeContractChecker().CheckRequest(ctx, agent, runtime.RuntimeContractRequest{Plan: false})
}

func promptHeadContradictionWarnings(ctx context.Context, git gitutil.Client, prompt string, dispatchHead string) []string {
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	headRef := strings.TrimSpace(dispatchHead)
	if headRef == "" {
		headRef = "HEAD"
	}
	resolvedHead, err := git.RevParse(ctx, headRef+"^{commit}")
	if err != nil {
		return nil
	}

	warnings := make([]string, 0)
	seenCommits := make(map[string]struct{})
	for _, token := range promptCommitTokenRE.FindAllString(prompt, -1) {
		resolvedToken, err := git.RevParse(ctx, token+"^{commit}")
		if err != nil {
			continue
		}
		resolvedToken = strings.ToLower(strings.TrimSpace(resolvedToken))
		if _, seen := seenCommits[resolvedToken]; seen {
			continue
		}
		seenCommits[resolvedToken] = struct{}{}
		// Compare identity directly, not reachability: cdd6598a is an ancestor of
		// d8c17db0, which is exactly why it looked plausible and why an is-ancestor
		// check would have waved it through.
		if strings.EqualFold(resolvedToken, resolvedHead) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("prompt references commit %s, but the dispatch head is %s; Gitmoot will use dispatch head %s", token, resolvedHead, resolvedHead))
	}
	return warnings
}

// foregroundAskTimeoutError turns a JobTimeout-driven context cancel into an
// actionable message instead of the confusing "job ... is cancelled, not running".
func foregroundAskTimeoutError(runCtx context.Context, jobTimeout time.Duration, err error) error {
	if err == nil {
		return nil
	}
	if jobTimeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("ask timed out after %s; re-run with --background", jobTimeout)
	}
	return err
}

// recoverAdvanceErrorOutput salvages the persisted result when a post-success
// advance step errors benignly (a merge-gate block on the freshly-opened PR, or
// a 422 "PR already exists" race) AFTER the agent delivery + job already
// succeeded terminally. It returns (output, true, nil) — output carrying the
// advance warning — only when runErr is a workflow.AdvanceError AND the
// re-fetched job is terminally succeeded AND the result renders. Otherwise it
// returns recovered=false so the caller surfaces the raw run error; genuine
// delivery/run failures (where the re-fetched job is NOT terminally succeeded)
// never recover.
func recoverAdvanceErrorOutput(ctx context.Context, store *db.Store, jobID string, request localAgentDispatchRequest, runErr error) (localAgentJobOutput, bool, error) {
	var advErr workflow.AdvanceError
	if !errors.As(runErr, &advErr) {
		return localAgentJobOutput{}, false, nil
	}
	latest, err := store.GetJob(ctx, jobID)
	if err != nil {
		return localAgentJobOutput{}, false, err
	}
	if latest.State != string(workflow.JobSucceeded) {
		return localAgentJobOutput{}, false, nil
	}
	out, err := buildLocalAgentJobOutput(latest, request)
	if err != nil {
		return localAgentJobOutput{}, false, err
	}
	out.AdvanceError = advErr.Error()
	return out, true, nil
}

// buildLocalAgentJobOutput renders the terminal job into the success-path
// localAgentJobOutput. It is shared by the normal success return and the
// post-success advance-error recovery so both surface the identical result.
func buildLocalAgentJobOutput(latest db.Job, request localAgentDispatchRequest) (localAgentJobOutput, error) {
	payload, err := daemonJobPayload(latest)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	return localAgentJobOutput{
		JobID:                latest.ID,
		State:                latest.State,
		Repo:                 payload.Repo,
		Agent:                latest.Agent,
		Action:               latest.Type,
		SelectedAction:       request.SelectedAction,
		SelectedActionReason: request.SelectedActionReason,
		ExecutionPath:        request.ExecutionPath,
		Result:               payload.Result,
		RawOutputCount:       len(payload.RawOutputs),
	}, nil
}

func enqueuePermissionBlockedLocalAgentJob(ctx context.Context, store *db.Store, request localAgentDispatchRequest, repo string, defaultBranch string, agentName string, overrideRuntime string, overrideRef string, orgPolicy func(string) workflow.OrgEnforcement) (localAgentJobOutput, error) {
	job, err := (workflow.Mailbox{Store: store, CanaryEnabled: canaryRoutingEnabled(request.Home), RuntimeDefaultModel: runtimeDefaultModelResolver(request.Home), RequireWorkflowPolicy: requireWorkflowPolicyResolver(request.Home), OrgPolicy: orgPolicy}).Enqueue(ctx, workflow.JobRequest{
		ID:             localAgentJobID(request.Action, agentName),
		Agent:          agentName,
		Action:         request.Action,
		Repo:           repo,
		Branch:         firstNonEmpty(request.Branch, defaultBranch),
		PullRequest:    request.PullRequest,
		HeadSHA:        request.HeadSHA,
		GoalID:         request.GoalID,
		TaskID:         request.TaskID,
		TaskTitle:      request.TaskTitle,
		LeadAgent:      request.LeadAgent,
		Reviewers:      request.Reviewers,
		Sender:         "local",
		ActingOrgRole:  request.ActingOrgRole,
		OperatorOrigin: request.OperatorOrigin,
		Instructions:   request.Instructions,
		// Persist the per-job --model and the resolved --runtime/--session override
		// (#531) on the BLOCKED job too: `gitmoot job retry` re-runs the stored
		// payload as-is, so dropping them here would silently retry the job on the
		// agent's default runtime — resuming the default-runtime session the user's
		// --runtime explicitly asked it to stay off.
		Model:                request.Model,
		Effort:               request.Effort,
		WorkflowID:           request.WorkflowID,
		ValidatedPullRequest: request.ImplementPRValidated,
		RuntimeOverride:      overrideRuntime,
		RuntimeOverrideRef:   overrideRef,
	})
	if err != nil {
		return localAgentJobOutput{}, err
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "route_selected", Message: routeSelectedMessage(request)}); err != nil {
		return localAgentJobOutput{}, err
	}
	if _, err := markJobPermissionBlocked(ctx, store, job); err != nil {
		return localAgentJobOutput{}, err
	}
	return localAgentJobOutput{
		JobID:                job.ID,
		State:                string(workflow.JobBlocked),
		Repo:                 repo,
		Agent:                job.Agent,
		Action:               job.Type,
		SelectedAction:       request.SelectedAction,
		SelectedActionReason: request.SelectedActionReason,
		ExecutionPath:        request.ExecutionPath,
		RawOutputCount:       0,
	}, nil
}

func routeSelectedMessage(request localAgentDispatchRequest) string {
	action := strings.TrimSpace(request.SelectedAction)
	if action == "" {
		action = request.Action
	}
	reason := strings.TrimSpace(request.SelectedActionReason)
	if reason == "" {
		reason = "explicit action"
	}
	path := strings.TrimSpace(request.ExecutionPath)
	if path == "" {
		path = "local_agent"
	}
	message := fmt.Sprintf("selected %s via %s: %s", action, path, reason)
	if override := strings.TrimSpace(request.Runtime); override != "" {
		message += fmt.Sprintf("; runtime override: %s", override)
	}
	return message
}

func validateLocalReviewLeadAtDispatch(ctx context.Context, store *db.Store, request localAgentDispatchRequest, repo string) (localAgentDispatchRequest, error) {
	typeName := strings.TrimSpace(request.Type)
	if typeName != "" {
		exists, err := managedAgentTypeExists(request.Home, typeName)
		if err != nil {
			return localAgentDispatchRequest{}, err
		}
		if !exists {
			return localAgentDispatchRequest{}, forcedManagedAgentTypeNotFoundError(typeName)
		}
	}
	leadName := strings.TrimSpace(request.LeadAgent)
	if leadName == "" {
		if typeName != "" {
			return localAgentDispatchRequest{}, fmt.Errorf("review dispatch through managed type %q requires --lead naming a registered implementer", typeName)
		}
		leadName = strings.TrimSpace(request.Agent)
	}
	lead, err := store.GetAgent(ctx, leadName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if strings.TrimSpace(request.LeadAgent) == "" {
				return localAgentDispatchRequest{}, fmt.Errorf("agent %q not found; review dispatch requires --lead naming a registered implementer", leadName)
			}
			return localAgentDispatchRequest{}, fmt.Errorf("review lead %q is not subscribed; --lead must name a registered implementer", leadName)
		}
		return localAgentDispatchRequest{}, err
	}
	allowed, err := store.AgentCanAccessRepo(ctx, lead.Name, repo)
	if err != nil {
		return localAgentDispatchRequest{}, err
	}
	if !allowed {
		return localAgentDispatchRequest{}, fmt.Errorf("review lead %q is not allowed on %q", lead.Name, repo)
	}
	if !agentHasCapability(lead.Capabilities, "implement") {
		return localAgentDispatchRequest{}, fmt.Errorf("review lead %q lacks implement capability; --lead must name an implementation-capable agent", lead.Name)
	}
	if err := runtime.ImplementWritePolicyError(lead.Capabilities, lead.AutonomyPolicy); err != nil {
		return localAgentDispatchRequest{}, fmt.Errorf("review lead %q: %w", lead.Name, err)
	}
	request.LeadAgent = lead.Name
	return request, nil
}

func prepareLocalReviewDispatchRequest(ctx context.Context, store *db.Store, record db.Repo, repo github.Repository, request localAgentDispatchRequest) (localAgentDispatchRequest, error) {
	if request.PullRequest <= 0 {
		return localAgentDispatchRequest{}, errors.New("agent review requires --pr number")
	}
	if strings.TrimSpace(request.Branch) == "" || strings.TrimSpace(request.HeadSHA) == "" {
		pr, err := newAgentDispatchGitHubClient(record.CheckoutPath).GetPullRequest(ctx, repo, int64(request.PullRequest))
		if err != nil {
			return localAgentDispatchRequest{}, fmt.Errorf("resolve pull request #%d: %w", request.PullRequest, err)
		}
		if strings.TrimSpace(request.Branch) == "" {
			request.Branch = pr.HeadRef
		}
		if strings.TrimSpace(request.HeadSHA) == "" {
			request.HeadSHA = pr.HeadSHA
		}
	}
	if match, detected, err := workflow.DetectReviewLoop(ctx, store, repo.FullName(), request.PullRequest, request.HeadSHA, []string{request.Agent}); err != nil {
		return localAgentDispatchRequest{}, err
	} else if detected {
		return localAgentDispatchRequest{}, errors.New(match.Reason())
	}
	return prepareLocalReviewTask(ctx, store, repo, request)
}

func prepareLocalReviewTask(ctx context.Context, store *db.Store, repo github.Repository, request localAgentDispatchRequest) (localAgentDispatchRequest, error) {
	if strings.TrimSpace(request.HeadSHA) == "" {
		return localAgentDispatchRequest{}, errors.New("agent review requires a pull request head SHA")
	}
	if strings.TrimSpace(request.Branch) != "" {
		if task, err := store.GetTaskByRepoBranch(ctx, repo.FullName(), request.Branch); err == nil {
			if workflow.IsDisposedTaskState(task.State) {
				return localAgentDispatchRequest{}, disposedReviewTaskError(task)
			}
			if strings.TrimSpace(task.WorktreePath) != "" {
				head, headErr := jobGitClient(task.WorktreePath, localDispatchJobRunner(request)).HeadSHA(ctx)
				if headErr != nil {
					return localAgentDispatchRequest{}, headErr
				}
				if head != request.HeadSHA {
					// #1530: the disk-HEAD comparison was a proxy for "same unit of
					// work", but the engine advances and deletes worktrees
					// independently of the task row — a fix-worktree leg pushes the
					// branch from an independent clone and leaves the registered
					// checkout legitimately behind. A review never reads this
					// checkout (it runs in its own exact-head read-only worktree),
					// so the task owning (repo, branch) IS the same unit of work:
					// bind attribution to it and record the divergence, rather
					// than minting a review-pr-* identity the merge gate cannot
					// attribute to any implement job. This rebind affects review
					// attribution ONLY: every implement path that consumes
					// task.WorktreePath re-validates the checkout independently
					// (validateFixPassTaskWorktreeHead at dispatch, and the daemon
					// pre-flight "checkout head is ..." guard deferred by the
					// checkout-contention classifier).
					request.ReviewTaskHeadDivergence = fmt.Sprintf(
						"review rebound to task %s owning branch %s although its registered checkout HEAD %s differs from the requested head %s (#1530); the review runs in an exact-head read-only worktree, so the rebind affects attribution only",
						task.ID, request.Branch, head, request.HeadSHA)
				}
			}
			request.TaskID = task.ID
			request.GoalID = firstNonEmpty(request.GoalID, task.GoalID)
			request.TaskTitle = firstNonEmpty(request.TaskTitle, task.Title)
			return request, nil
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return localAgentDispatchRequest{}, err
		}
	}
	taskID := strings.TrimSpace(request.TaskID)
	if taskID == "" {
		taskID = fmt.Sprintf("review-pr-%d-%s", request.PullRequest, shortHash(repo.FullName()))
	}
	if existing, err := store.GetTask(ctx, taskID); err == nil {
		if workflow.IsDisposedTaskState(existing.State) {
			return localAgentDispatchRequest{}, disposedReviewTaskError(existing)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return localAgentDispatchRequest{}, err
	}
	task := db.Task{
		ID:           taskID,
		RepoFullName: repo.FullName(),
		GoalID:       firstNonEmpty(request.GoalID, "local-review"),
		Title:        firstNonEmpty(request.TaskTitle, fmt.Sprintf("Review PR #%d", request.PullRequest)),
		State:        string(workflow.TaskReviewing),
	}
	updated, err := store.UpsertTaskUnlessStates(ctx, task, []string{
		string(workflow.TaskDismissed), string(workflow.TaskSuperseded), string(workflow.TaskStranded),
	})
	if err != nil {
		return localAgentDispatchRequest{}, err
	}
	if !updated {
		existing, loadErr := store.GetTask(ctx, task.ID)
		if loadErr != nil {
			return localAgentDispatchRequest{}, loadErr
		}
		return localAgentDispatchRequest{}, disposedReviewTaskError(existing)
	}
	request.TaskID = task.ID
	request.GoalID = task.GoalID
	request.TaskTitle = task.Title
	return request, nil
}

func dismissedReviewTaskError(taskID string) error {
	return fmt.Errorf("task %s is dismissed; run task recover first", taskID)
}

func disposedReviewTaskError(task db.Task) error {
	if task.State == string(workflow.TaskDismissed) {
		return dismissedReviewTaskError(task.ID)
	}
	return fmt.Errorf("task %s is %s; create a successor task before dispatching another review", task.ID, task.State)
}

func prepareLocalImplementDispatchRequest(ctx context.Context, store *db.Store, record db.Repo, repo github.Repository, request localAgentDispatchRequest) (db.Task, localAgentDispatchRequest, error) {
	paths, err := initializedPaths(request.Home)
	if err != nil {
		return db.Task{}, localAgentDispatchRequest{}, err
	}
	baseSHA := strings.TrimSpace(request.ImplementBase)
	deferImplicitPRBase := request.PullRequest > 0 && !request.ImplementBaseResolved && baseSHA == ""
	if !request.ImplementBaseResolved && !deferImplicitPRBase {
		baseSHA, err = resolveLocalImplementBaseForRunner(ctx, paths, record, baseSHA, localDispatchJobRunner(request))
		if err != nil {
			return db.Task{}, localAgentDispatchRequest{}, err
		}
	}
	taskID := strings.TrimSpace(request.TaskID)
	taskTitle := strings.TrimSpace(request.TaskTitle)
	goalID := strings.TrimSpace(request.GoalID)
	branchHint := strings.TrimSpace(request.Branch)
	validatedPRBinding := request.ImplementPRValidated
	if request.PullRequest > 0 && !validatedPRBinding {
		request, err = bindLocalImplementRequestToPullRequest(ctx, store, record, repo, request)
		if err != nil {
			return db.Task{}, localAgentDispatchRequest{}, err
		}
		validatedPRBinding = true
		taskID = strings.TrimSpace(request.TaskID)
		taskTitle = strings.TrimSpace(request.TaskTitle)
		goalID = strings.TrimSpace(request.GoalID)
		branchHint = strings.TrimSpace(request.Branch)
	}
	if (taskID == "" || validatedPRBinding) && branchHint != "" {
		existing, err := store.GetTaskByRepoBranch(ctx, repo.FullName(), branchHint)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return db.Task{}, localAgentDispatchRequest{}, err
		}
		if err == nil {
			if taskID != "" && existing.ID != taskID {
				return db.Task{}, localAgentDispatchRequest{}, fmt.Errorf("branch %s belongs to task %s, not requested task %s", branchHint, existing.ID, taskID)
			}
			if validatedPRBinding {
				if err := validateFixPassTaskWorktreeHead(ctx, existing, github.PullRequest{
					Number:  int64(request.PullRequest),
					HeadRef: branchHint,
					HeadSHA: request.HeadSHA,
				}, localDispatchJobRunner(request)); err != nil {
					return db.Task{}, localAgentDispatchRequest{}, err
				}
			}
			prOpenFixPass := validatedPRBinding && workflow.TaskState(existing.State) == workflow.TaskPullRequestOpen
			if !taskBranchReusableForImplement(existing.State) && !prOpenFixPass {
				return db.Task{}, localAgentDispatchRequest{}, fmt.Errorf("branch %s belongs to task %s in state %s; choose a fresh branch or recover/review the existing task", branchHint, existing.ID, existing.State)
			}
			if active, ok, err := findActiveImplementJobForTask(ctx, store, repo.FullName(), branchHint, existing.ID); err != nil {
				return db.Task{}, localAgentDispatchRequest{}, err
			} else if ok {
				return db.Task{}, localAgentDispatchRequest{}, fmt.Errorf("branch %s already has active implement job %s for task %s", branchHint, active.ID, existing.ID)
			}
			if strings.TrimSpace(existing.WorktreePath) != "" && taskWorktreeHasLiveProcess(existing.WorktreePath) {
				return db.Task{}, localAgentDispatchRequest{}, fmt.Errorf("branch %s has a live process still inside task worktree %s; wait for it to exit or stop the orphaned implementer before retrying implement", branchHint, existing.WorktreePath)
			}
			if dirty, err := taskWorktreeDirtyWithRunner(ctx, existing, localDispatchJobRunner(request)); err != nil {
				return db.Task{}, localAgentDispatchRequest{}, err
			} else if dirty {
				if baseSHA != "" {
					handled, blockErr := (workflow.Engine{Store: store}).ReconcileDirtyTaskWorktreeLineage(
						ctx,
						jobGitClient(record.CheckoutPath, localDispatchJobRunner(request)),
						existing,
						existing.WorktreePath,
						baseSHA,
					)
					if handled {
						return db.Task{}, localAgentDispatchRequest{}, blockErr
					}
				}
				if prOpenFixPass {
					return db.Task{}, localAgentDispatchRequest{}, fmt.Errorf("branch %s has uncommitted changes in task worktree %s; inspect and commit/push them, or clean/stash them before retrying the PR fix-pass", branchHint, existing.WorktreePath)
				}
				skipFanout := taskRecoverSkipFanout(ctx, store, repo.FullName(), branchHint)
				return db.Task{}, localAgentDispatchRequest{}, fmt.Errorf("branch %s has uncommitted changes in task worktree %s; inspect it, then run %s to commit/push/open a PR, or clean/stash it before retrying implement", branchHint, existing.WorktreePath, taskRecoverCommand(existing.ID, request.Home, repo.FullName(), request.Agent, skipFanout))
			}
			taskID = existing.ID
			taskTitle = firstNonEmpty(taskTitle, existing.Title)
			goalID = firstNonEmpty(goalID, existing.GoalID)
			request.TaskID = taskID
			request.TaskTitle = taskTitle
			request.GoalID = goalID
		}
	}
	if taskID == "" {
		taskID = "adhoc-" + shortHash(request.Instructions+"\x00"+time.Now().UTC().Format(time.RFC3339Nano))
		taskTitle = firstNonEmpty(taskTitle, shortTaskTitle(request.Instructions))
		goalID = firstNonEmpty(goalID, "local-agent")
		if err := store.UpsertTask(ctx, db.Task{
			ID:           taskID,
			RepoFullName: repo.FullName(),
			GoalID:       goalID,
			Title:        taskTitle,
			State:        string(workflow.TaskPlanned),
			Branch:       firstNonEmpty(request.Branch, "gitmoot/"+taskID),
		}); err != nil {
			return db.Task{}, localAgentDispatchRequest{}, err
		}
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		return db.Task{}, localAgentDispatchRequest{}, fmt.Errorf("load task %q: %w", taskID, err)
	}
	if strings.TrimSpace(task.RepoFullName) != "" && task.RepoFullName != repo.FullName() {
		return db.Task{}, localAgentDispatchRequest{}, fmt.Errorf("task %s belongs to repo %s, not %s", task.ID, task.RepoFullName, repo.FullName())
	}
	if strings.TrimSpace(task.RepoFullName) == "" {
		task.RepoFullName = repo.FullName()
	}
	if strings.TrimSpace(task.Title) == "" {
		task.Title = firstNonEmpty(taskTitle, shortTaskTitle(request.Instructions))
	}
	if strings.TrimSpace(task.GoalID) == "" {
		task.GoalID = firstNonEmpty(goalID, "local-agent")
	}
	branch := firstNonEmpty(request.Branch, task.Branch, "gitmoot/"+task.ID)
	if deferImplicitPRBase {
		expectedWorktree, pathErr := workflow.TaskWorktreePath(paths.Home, repo.FullName(), task.ID)
		if pathErr != nil {
			return db.Task{}, localAgentDispatchRequest{}, pathErr
		}
		if task.Branch != branch || strings.TrimSpace(task.WorktreePath) == "" || task.WorktreePath != expectedWorktree {
			baseSHA, err = resolveLocalImplementBaseForRunner(ctx, paths, record, "", localDispatchJobRunner(request))
			if err != nil {
				return db.Task{}, localAgentDispatchRequest{}, err
			}
		}
	}
	owner := strings.TrimSpace(request.Agent)
	started, err := (workflow.Engine{Store: store}).AllocateTaskWorktree(ctx, workflow.TaskWorktreeRequest{
		Home:           paths.Home,
		Repo:           repo.FullName(),
		GoalID:         task.GoalID,
		TaskID:         task.ID,
		TaskTitle:      task.Title,
		Branch:         branch,
		BaseBranch:     baseSHA,
		LineageUnknown: deferImplicitPRBase && baseSHA == "",
		Owner:          owner,
		Checkout:       record.CheckoutPath,
	}, jobGitClient(record.CheckoutPath, localDispatchJobRunner(request)))
	if err != nil {
		return db.Task{}, localAgentDispatchRequest{}, err
	}
	headSHA, err := jobGitClient(started.WorktreePath, localDispatchJobRunner(request)).HeadSHA(ctx)
	if err != nil {
		return db.Task{}, localAgentDispatchRequest{}, fmt.Errorf("resolve task worktree head: %w", err)
	}
	request.TaskID = started.ID
	request.GoalID = started.GoalID
	request.TaskTitle = started.Title
	request.Branch = started.Branch
	request.HeadSHA = headSHA
	request.LeadAgent = owner
	return started, request, nil
}

func bindLocalImplementRequestToPullRequest(ctx context.Context, store *db.Store, record db.Repo, repo github.Repository, request localAgentDispatchRequest) (localAgentDispatchRequest, error) {
	pr, err := newAgentDispatchGitHubClient(record.CheckoutPath).GetPullRequest(ctx, repo, int64(request.PullRequest))
	if err != nil {
		return localAgentDispatchRequest{}, fmt.Errorf("resolve pull request #%d for implement fix-pass: %w", request.PullRequest, err)
	}
	if pr.Merged || strings.TrimSpace(pr.MergedAt) != "" || strings.EqualFold(strings.TrimSpace(pr.State), "merged") {
		return localAgentDispatchRequest{}, fmt.Errorf("pull request #%d is merged; implement fix-pass requires an open pull request", request.PullRequest)
	}
	if !strings.EqualFold(strings.TrimSpace(pr.State), "open") {
		return localAgentDispatchRequest{}, fmt.Errorf("pull request #%d is %s; implement fix-pass requires an open pull request", request.PullRequest, firstNonEmpty(strings.TrimSpace(pr.State), "not open"))
	}
	headRepo := strings.TrimSpace(pr.HeadRepoFullName)
	if headRepo == "" || !strings.EqualFold(headRepo, repo.FullName()) {
		return localAgentDispatchRequest{}, fmt.Errorf("pull request #%d head belongs to %s, not %s; fork or unrelated heads cannot enter the implement fix-pass", request.PullRequest, firstNonEmpty(headRepo, "an unknown repository"), repo.FullName())
	}
	headBranch := strings.TrimSpace(pr.HeadRef)
	if headBranch == "" {
		return localAgentDispatchRequest{}, fmt.Errorf("pull request #%d has no head branch; cannot bind an implement fix-pass", request.PullRequest)
	}
	if requested := strings.TrimSpace(request.Branch); requested != "" && requested != headBranch {
		return localAgentDispatchRequest{}, fmt.Errorf("pull request #%d head branch %s does not match requested branch %s", request.PullRequest, headBranch, requested)
	}
	task, err := store.GetTaskByRepoBranch(ctx, repo.FullName(), headBranch)
	if errors.Is(err, sql.ErrNoRows) {
		return localAgentDispatchRequest{}, fmt.Errorf("pull request #%d head branch %s is not bound to an existing task", request.PullRequest, headBranch)
	}
	if err != nil {
		return localAgentDispatchRequest{}, err
	}
	if requested := strings.TrimSpace(request.TaskID); requested != "" && requested != task.ID {
		return localAgentDispatchRequest{}, fmt.Errorf("pull request #%d head branch %s belongs to task %s, not requested task %s", request.PullRequest, headBranch, task.ID, requested)
	}
	switch workflow.TaskState(strings.TrimSpace(task.State)) {
	case workflow.TaskReviewing, workflow.TaskReadyToMerge:
		return localAgentDispatchRequest{}, fmt.Errorf("task %s is %s; implement fix-pass is refused while review or merge is in progress", task.ID, task.State)
	case workflow.TaskAwaitingHumanMerge:
		return localAgentDispatchRequest{}, fmt.Errorf("task %s is awaiting a human merge decision; implement fix-pass is refused until it resolves", task.ID)
	}
	if err := validateFixPassTaskWorktreeHead(ctx, task, pr, localDispatchJobRunner(request)); err != nil {
		return localAgentDispatchRequest{}, err
	}
	request.Branch = headBranch
	request.TaskID = task.ID
	request.TaskTitle = firstNonEmpty(request.TaskTitle, task.Title)
	request.GoalID = firstNonEmpty(request.GoalID, task.GoalID)
	request.HeadSHA = strings.TrimSpace(pr.HeadSHA)
	return request, nil
}

func validateFixPassTaskWorktreeHead(ctx context.Context, task db.Task, pr github.PullRequest, runner subprocess.Runner) error {
	path := strings.TrimSpace(task.WorktreePath)
	expectedBranch := strings.TrimSpace(pr.HeadRef)
	expectedHead := strings.TrimSpace(pr.HeadSHA)
	guidance := fmt.Sprintf("inspect or stash local changes, then run `git -C %q fetch origin refs/pull/%d/head` and `git -C %q reset --hard FETCH_HEAD`; retry the fix-pass after synchronization", path, pr.Number, path)
	if path == "" {
		return fmt.Errorf("pull request #%d is bound to task %s, but the task has no worktree path; restore the task worktree, then %s", pr.Number, task.ID, guidance)
	}
	if expectedHead == "" {
		return fmt.Errorf("pull request #%d returned no head SHA; cannot prove task worktree %s is current; %s", pr.Number, path, guidance)
	}
	git := jobGitClient(path, runner)
	branch, err := git.CurrentBranch(ctx)
	if err != nil {
		return fmt.Errorf("inspect task worktree branch for pull request #%d: %w; %s", pr.Number, err, guidance)
	}
	localHead, err := git.HeadSHA(ctx)
	if err != nil {
		return fmt.Errorf("inspect task worktree HEAD for pull request #%d: %w; %s", pr.Number, err, guidance)
	}
	if branch != expectedBranch || !strings.EqualFold(localHead, expectedHead) {
		return fmt.Errorf("pull request #%d head is %s at %s, but task %s worktree is %s at %s; refusing to run against stale code; %s", pr.Number, expectedBranch, expectedHead, task.ID, branch, localHead, guidance)
	}
	return nil
}

// resolveLocalImplementBase returns the exact commit an implement worktree must
// start from. A CLI value wins over [workflow].implement_base. With neither set,
// HEAD preserves checkout-following behavior after the stale-feature guard.
func resolveLocalImplementBase(ctx context.Context, paths config.Paths, record db.Repo, requested string) (string, error) {
	return resolveLocalImplementBaseForRunner(ctx, paths, record, requested, subprocess.ExecRunner{})
}

func resolveLocalImplementBaseForRunner(ctx context.Context, paths config.Paths, record db.Repo, requested string, runner subprocess.Runner) (string, error) {
	base := strings.TrimSpace(requested)
	if base == "" {
		configured, err := config.LoadImplementBase(paths)
		if err != nil {
			return "", fmt.Errorf("load workflow implement_base: %w", err)
		}
		base = strings.TrimSpace(configured)
	}
	git := jobGitClient(record.CheckoutPath, runner)
	if base == "" {
		if err := guardImplicitImplementBase(ctx, git, record.DefaultBranch); err != nil {
			return "", err
		}
		base = "HEAD"
	}
	if strings.HasPrefix(base, "origin/") {
		if err := git.FetchRemote(ctx, "origin"); err != nil {
			return "", fmt.Errorf("fetch origin for implement base %q: %w", base, err)
		}
	}
	sha, err := git.RevParse(ctx, base+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("unknown implement base ref %q: %w", base, err)
	}
	return sha, nil
}

func guardImplicitImplementBase(ctx context.Context, git gitutil.Client, defaultBranch string) error {
	defaultBranch = strings.TrimSpace(defaultBranch)
	if defaultBranch == "" {
		return nil
	}
	branch, err := git.CurrentBranch(ctx)
	if err != nil {
		// Detached HEAD has no branch to compare or name. Preserve the existing
		// checkout-HEAD behavior rather than turning a detached checkout into a new
		// refusal mode.
		if strings.Contains(err.Error(), "current git branch is empty") {
			return nil
		}
		return fmt.Errorf("inspect checkout branch before implement: %w", err)
	}
	if branch == defaultBranch {
		return nil
	}
	upstream := "origin/" + defaultBranch
	if err := git.FetchRemote(ctx, "origin"); err != nil {
		return fmt.Errorf("check whether checkout branch %s is behind %s: fetch origin: %w; pass --base HEAD to use checkout HEAD", branch, upstream, err)
	}
	behind, err := git.BehindCount(ctx, upstream)
	if err != nil {
		return fmt.Errorf("check whether checkout branch %s is behind %s: %w; pass --base HEAD to use checkout HEAD", branch, upstream, err)
	}
	if behind > 0 {
		return fmt.Errorf("checkout is on %s, %d behind %s; pass --base %s or --base HEAD", branch, behind, upstream, upstream)
	}
	return nil
}

func shortTaskTitle(message string) string {
	fields := strings.Fields(strings.TrimSpace(message))
	if len(fields) > 8 {
		fields = fields[:8]
	}
	title := strings.Join(fields, " ")
	if title == "" {
		return "Local agent implementation"
	}
	return title
}

func resolveLocalDispatchAgent(ctx context.Context, store *db.Store, request localAgentDispatchRequest, repo string, record db.Repo) (db.Agent, func(context.Context) error, error) {
	forceType := strings.TrimSpace(request.Type)
	if forceType == "" {
		agent, err := store.GetAgent(ctx, request.Agent)
		if err == nil {
			return agent, noopAgentReservationRelease, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return db.Agent{}, noopAgentReservationRelease, err
		}
	}
	typeName := firstNonEmpty(forceType, request.Agent)
	if forceType != "" {
		exists, err := managedAgentTypeExists(request.Home, forceType)
		if err != nil {
			return db.Agent{}, noopAgentReservationRelease, err
		}
		if !exists {
			return db.Agent{}, noopAgentReservationRelease, forcedManagedAgentTypeNotFoundError(forceType)
		}
	}
	// A background dispatch (or a caller that explicitly opted in via
	// AllowManagedSync, e.g. the skillopt path) always reaches the managed path.
	// For a plain foreground dispatch, fall through to the managed path only for
	// the `ask` action AND only when the resolved name maps to a configured
	// managed agent type; otherwise preserve the historical "agent not found"
	// error so a name that resolves to neither a single instance nor a type still
	// fails as before. Scoped to `ask`: `implement` keeps its existing
	// read-only/finalize semantics (readOnlyManagedImplementationBlock), and
	// `review` carries required params (--pr / --head-sha) that the foreground
	// path does not validate before this point — letting a heuristic-selected
	// `run`->`review` reach the managed path would spin an instance and then fail
	// downstream (#395).
	if !request.Background && !request.AllowManagedSync {
		allowSync := false
		if strings.TrimSpace(request.Action) == "ask" {
			ok, err := managedAgentTypeExists(request.Home, typeName)
			if err != nil {
				return db.Agent{}, noopAgentReservationRelease, err
			}
			allowSync = ok
		}
		if !allowSync {
			return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("agent %q not found", request.Agent)
		}
	}
	return ensureManagedAgentInstance(ctx, store, request.Home, typeName, repo, record)
}

func forcedManagedAgentTypeNotFoundError(typeName string) error {
	return fmt.Errorf("managed agent type %q not found: --type selects a managed agent type; --action chooses the job action", typeName)
}

// managedAgentTypeExists reports whether typeName names a configured managed
// agent type for the given home. It is used to decide whether a foreground
// ask/run may dispatch synchronously to a managed type (#395) without changing
// the historical "agent not found" behavior for names that match neither a
// single instance nor a type.
func managedAgentTypeExists(home string, typeName string) (bool, error) {
	typeName = strings.TrimSpace(typeName)
	if typeName == "" {
		return false, nil
	}
	types, err := loadAgentTypeConfig(home)
	if err != nil {
		return false, err
	}
	_, ok := types[typeName]
	return ok, nil
}

func readOnlyManagedImplementationBlock(ctx context.Context, store *db.Store, request localAgentDispatchRequest, repo string) (runtime.Agent, bool, error) {
	if strings.TrimSpace(request.Action) != "implement" {
		return runtime.Agent{}, false, nil
	}
	forceType := strings.TrimSpace(request.Type)
	if forceType == "" {
		if _, err := store.GetAgent(ctx, request.Agent); err == nil {
			return runtime.Agent{}, false, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return runtime.Agent{}, false, err
		}
	}
	if !request.Background && !request.AllowManagedSync {
		return runtime.Agent{}, false, nil
	}
	typeName := firstNonEmpty(forceType, request.Agent)
	types, err := loadAgentTypeConfig(request.Home)
	if err != nil {
		return runtime.Agent{}, false, err
	}
	agentType, ok := types[typeName]
	if !ok {
		return runtime.Agent{}, false, nil
	}
	agent := runtimeAgentFromType(agentType, repo, typeName)
	if !agentHasCapability(agent.Capabilities, request.Action) {
		return runtime.Agent{}, false, fmt.Errorf("agent %q lacks %s capability", agent.Name, request.Action)
	}
	return agent, readOnlyImplementationBlocked(request.Action, agent), nil
}

func noopAgentReservationRelease(context.Context) error {
	return nil
}

type localManagedAgentDispatchConfig struct {
	OK          bool
	IdleTimeout time.Duration
	JobTimeout  time.Duration
}

func localManagedAgentDispatchConfigForAgent(ctx context.Context, store *db.Store, home string, agentName string) (localManagedAgentDispatchConfig, error) {
	instance, err := store.GetAgentInstance(ctx, agentName)
	if errors.Is(err, sql.ErrNoRows) {
		return localManagedAgentDispatchConfig{}, nil
	}
	if err != nil {
		return localManagedAgentDispatchConfig{}, err
	}
	types, err := loadAgentTypeConfig(home)
	if err != nil {
		return localManagedAgentDispatchConfig{}, err
	}
	agentType, ok := types[instance.Type]
	if !ok {
		return localManagedAgentDispatchConfig{}, fmt.Errorf("agent type %s not found for managed agent %s", instance.Type, agentName)
	}
	idleTimeout, err := time.ParseDuration(agentType.IdleTimeout)
	if err != nil {
		return localManagedAgentDispatchConfig{}, fmt.Errorf("agent type %s idle_timeout: %w", instance.Type, err)
	}
	jobTimeout, err := time.ParseDuration(agentType.JobTimeout)
	if err != nil {
		return localManagedAgentDispatchConfig{}, fmt.Errorf("agent type %s job_timeout: %w", instance.Type, err)
	}
	return localManagedAgentDispatchConfig{OK: true, IdleTimeout: idleTimeout, JobTimeout: jobTimeout}, nil
}

func ensureManagedAgentInstance(ctx context.Context, store *db.Store, home string, typeName string, repo string, record db.Repo) (db.Agent, func(context.Context) error, error) {
	types, err := loadAgentTypeConfig(home)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	agentType, ok := types[typeName]
	if !ok {
		return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("agent %q not found", typeName)
	}
	idleTimeout, err := time.ParseDuration(agentType.IdleTimeout)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("agent type %s idle_timeout: %w", typeName, err)
	}
	jobTimeout, err := time.ParseDuration(agentType.JobTimeout)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("agent type %s job_timeout: %w", typeName, err)
	}
	now := time.Now().UTC()
	releaseTypeLock, acquiredTypeLock, typeLockKey, err := acquireManagedAgentTypeLockWithWait(ctx, store, typeName, daemonRunningJobStaleAfter, jobTimeout)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	if !acquiredTypeLock {
		return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("managed agent type %s is busy reserving %s", typeName, typeLockKey)
	}
	now = time.Now().UTC()
	releaseOnError := true
	defer func() {
		if releaseOnError {
			_ = releaseTypeLock(context.Background())
		}
	}()
	if instance, ok, err := store.FindReusableAgentInstance(ctx, typeName, repo, agentType.AutonomyPolicy, now); err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	} else if ok {
		if err := store.TouchAgentInstance(ctx, instance.Name, now, idleTimeout); err != nil {
			return db.Agent{}, noopAgentReservationRelease, err
		}
		agent, err := store.GetAgent(ctx, instance.Name)
		if err != nil {
			return db.Agent{}, noopAgentReservationRelease, err
		}
		releaseOnError = false
		return agent, releaseTypeLock, nil
	}
	count, err := store.CountActiveAgentInstances(ctx, typeName, agentType.AutonomyPolicy, now)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	if count >= agentType.MaxBackground {
		instance, ok, err := store.FindActiveAgentInstance(ctx, typeName, repo, agentType.AutonomyPolicy, now)
		if err != nil {
			return db.Agent{}, noopAgentReservationRelease, err
		}
		if ok && strings.TrimSpace(instance.State) == "starting" {
			return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("managed agent type %s reached max_background while instances are still starting", typeName)
		}
		if ok {
			agent, err := store.GetAgent(ctx, instance.Name)
			if err != nil {
				return db.Agent{}, noopAgentReservationRelease, err
			}
			releaseOnError = false
			return agent, releaseTypeLock, nil
		}
		return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("managed agent type %s reached max_background but no active instance is available", typeName)
	}
	instanceAgent := runtimeAgentFromType(agentType, repo, managedAgentInstanceName(typeName))
	var cachedTemplate db.AgentTemplate
	if instanceAgent.TemplateID != "" {
		var err error
		cachedTemplate, err = loadInstalledTemplate(ctx, store, instanceAgent.TemplateID)
		if err != nil {
			return db.Agent{}, noopAgentReservationRelease, err
		}
	}
	execBackend, err := localAgentDispatchExecBackendFor(home)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	adapter, err := startRuntimeAdapterForBackend(execBackend, home, instanceAgent.Runtime, record.CheckoutPath)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	reservedInstance := db.AgentInstance{
		Name:           instanceAgent.Name,
		Type:           agentType.Name,
		Runtime:        instanceAgent.Runtime,
		RuntimeRef:     "starting:" + instanceAgent.Name,
		RepoFullName:   repo,
		Role:           instanceAgent.Role,
		TemplateID:     instanceAgent.TemplateID,
		Model:          instanceAgent.Model,
		Effort:         instanceAgent.Effort,
		Capabilities:   instanceAgent.Capabilities,
		AutonomyPolicy: instanceAgent.AutonomyPolicy,
		State:          "starting",
		CreatedAt:      formatManagedAgentTime(now),
		LastUsedAt:     formatManagedAgentTime(now),
		ExpiresAt:      formatManagedAgentTime(now.Add(jobTimeout)),
	}
	if err := store.UpsertAgentInstance(ctx, reservedInstance); err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	if err := releaseTypeLock(ctx); err != nil {
		_ = store.DeleteAgentInstance(context.Background(), reservedInstance.Name)
		return db.Agent{}, noopAgentReservationRelease, err
	}
	releaseOnError = false
	started, err := adapter.Start(ctx, runtime.StartRequest{Agent: instanceAgent, Prompt: agentStartupPrompt(instanceAgent, cachedTemplate)})
	if err != nil {
		_ = store.DeleteAgentInstance(context.Background(), reservedInstance.Name)
		return db.Agent{}, noopAgentReservationRelease, err
	}
	instanceAgent.RuntimeRef = strings.TrimSpace(started.RuntimeRef)
	if err := runtime.ValidateAgent(instanceAgent); err != nil {
		_ = store.DeleteAgentInstance(context.Background(), reservedInstance.Name)
		return db.Agent{}, noopAgentReservationRelease, err
	}
	instance := db.AgentInstance{
		Name:           instanceAgent.Name,
		Type:           agentType.Name,
		Runtime:        instanceAgent.Runtime,
		RuntimeRef:     instanceAgent.RuntimeRef,
		RepoFullName:   repo,
		Role:           instanceAgent.Role,
		TemplateID:     instanceAgent.TemplateID,
		Model:          instanceAgent.Model,
		Effort:         instanceAgent.Effort,
		Capabilities:   instanceAgent.Capabilities,
		AutonomyPolicy: instanceAgent.AutonomyPolicy,
		State:          "starting",
		CreatedAt:      formatManagedAgentTime(now),
		LastUsedAt:     formatManagedAgentTime(now),
		ExpiresAt:      formatManagedAgentTime(now.Add(jobTimeout)),
	}
	if err := store.UpsertAgentInstance(ctx, instance); err != nil {
		_ = store.DeleteAgentInstance(context.Background(), reservedInstance.Name)
		return db.Agent{}, noopAgentReservationRelease, err
	}
	agent, err := store.GetAgent(ctx, instance.Name)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	return agent, func(releaseCtx context.Context) error {
		return store.TouchAgentInstance(releaseCtx, instance.Name, time.Now().UTC(), idleTimeout)
	}, nil
}

func acquireManagedAgentTypeLockWithWait(ctx context.Context, store *db.Store, typeName string, ttl time.Duration, waitTimeout time.Duration) (func(context.Context) error, bool, string, error) {
	if waitTimeout <= 0 {
		waitTimeout = ttl
	}
	deadline := time.Now().UTC().Add(waitTimeout)
	var lastKey string
	for {
		release, acquired, key, err := acquireManagedAgentTypeLock(ctx, store, typeName, time.Now().UTC(), ttl)
		lastKey = key
		if err != nil || acquired {
			return release, acquired, key, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return noopAgentReservationRelease, false, firstNonEmpty(lastKey, "agent-type:"+typeName), nil
		}
		sleep := 100 * time.Millisecond
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			return release, false, key, ctx.Err()
		case <-time.After(sleep):
		}
	}
}

func acquireManagedAgentTypeLock(ctx context.Context, store *db.Store, typeName string, now time.Time, ttl time.Duration) (func(context.Context) error, bool, string, error) {
	if ttl <= 0 {
		return nil, false, "", fmt.Errorf("managed agent type lock ttl must be positive")
	}
	key := "agent-type:" + typeName
	ownerToken, err := newRuntimeLockOwnerToken()
	if err != nil {
		return nil, false, key, err
	}
	owner := "agent-type:" + typeName
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  owner,
		OwnerToken:  ownerToken,
		ExpiresAt:   now.UTC().Add(ttl).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		return func(context.Context) error { return nil }, acquired, key, err
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, owner, ownerToken)
		return err
	}, true, key, nil
}

func runtimeAgentFromType(agentType config.AgentType, repo string, name string) runtime.Agent {
	return runtime.Agent{
		Name:           name,
		Role:           agentType.Role,
		Runtime:        agentType.Runtime,
		RepoScope:      repo,
		TemplateID:     agentType.Template,
		Model:          agentType.Model,
		Effort:         agentType.Effort,
		Capabilities:   agentType.Capabilities,
		AutonomyPolicy: runtime.NormalizeStoredAutonomyPolicy(agentType.AutonomyPolicy),
		HealthStatus:   "idle",
	}
}

func managedAgentInstanceName(typeName string) string {
	return fmt.Sprintf("%s-bg-%x", typeName, time.Now().UTC().UnixNano())
}

func formatManagedAgentTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func resolveLocalAgentRepo(ctx context.Context, store *db.Store, repoFlag string) (github.Repository, db.Repo, error) {
	repo, err := localAgentTargetRepo(ctx, repoFlag)
	if err != nil {
		return github.Repository{}, db.Repo{}, err
	}
	record, err := resolveRepoRecord(ctx, store, repo, ".")
	if err != nil {
		return github.Repository{}, db.Repo{}, err
	}
	// Without --repo, preserve the historical current-checkout branch selection
	// for the job payload while retaining the registered/stable checkout path.
	// This prevents an ephemeral cwd from becoming repo.CheckoutPath without
	// silently rebasing local ask/review behavior onto the stored default branch.
	if strings.TrimSpace(repoFlag) == "" {
		if cwdRecord, cwdErr := repoRecordForCheckout(ctx, repo, gitutil.NewHostClient(".")); cwdErr == nil {
			record.DefaultBranch = cwdRecord.DefaultBranch
		}
	}
	return repo, record, nil
}

func localAgentTargetRepo(ctx context.Context, repoFlag string) (github.Repository, error) {
	if strings.TrimSpace(repoFlag) != "" {
		return daemon.ParseRepository(repoFlag)
	}
	remote, err := (gitutil.NewHostClient(".")).OriginRemote(ctx)
	if err != nil {
		return github.Repository{}, fmt.Errorf("infer repo from current checkout: %w", err)
	}
	parsed, err := gitutil.ParseGitHubRemote(remote)
	if err != nil {
		return github.Repository{}, err
	}
	return github.Repository{Owner: parsed.Owner, Name: parsed.Name}, nil
}

func ensureLocalAgentAccess(ctx context.Context, store *db.Store, agent db.Agent, repo string, action string) error {
	allowed, err := store.AgentCanAccessRepo(ctx, agent.Name, repo)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("agent %q is not allowed on %q", agent.Name, repo)
	}
	if !agentHasCapability(agent.Capabilities, action) {
		return fmt.Errorf("agent %q lacks %s capability", agent.Name, action)
	}
	return nil
}

func localAgentJobID(action string, agent string) string {
	return fmt.Sprintf("local-%s-%s-%x", action, agent, time.Now().UTC().UnixNano())
}

// dispatchReadOnlyWorktreeEligible reports whether a dispatch should allocate a
// dedicated detached committed-tip worktree for read-only isolation (#739). It is
// true for every review and only a BACKGROUND ask without a TaskID. Review task
// identity is durable lifecycle metadata, not checkout ownership; each review
// job needs its own exact-head worktree. A foreground ask runs inline, and a
// task-bearing ask preserves its existing checkout behavior.
func dispatchReadOnlyWorktreeEligible(request localAgentDispatchRequest) bool {
	switch strings.TrimSpace(request.Action) {
	case "review":
		return true
	case "ask":
		return request.Background && strings.TrimSpace(request.TaskID) == ""
	default:
		return false
	}
}

// maybeAllocateDispatchReadOnlyWorktree allocates a throwaway detached
// committed-tip worktree for an eligible background read-only job so it is born
// with a distinct worktree:<path> checkout key (#739). It resolves the ref to the
// checkout HEAD for ask and to HeadSHA for review via the shared
// workflow.AllocateReadOnlyWorktree primitive, holding the checkout mutation
// lock. Ask remains fail-open at the call site. Review retries after fetching the
// PR ref for a cold checkout, then fails closed at the call site rather than
// falling back to the stale shared checkout.
func maybeAllocateDispatchReadOnlyWorktree(ctx context.Context, store *db.Store, request localAgentDispatchRequest, repo string, checkout string, jobID string) (string, error) {
	if !dispatchReadOnlyWorktreeEligible(request) {
		return "", nil
	}
	if strings.TrimSpace(checkout) == "" {
		return "", nil
	}
	paths, err := pathsFromFlag(request.Home)
	if err != nil {
		return "", err
	}
	baseRef := ""
	if strings.TrimSpace(request.Action) == "review" {
		baseRef = strings.TrimSpace(request.HeadSHA)
		if baseRef == "" {
			return "", errors.New("review read-only worktree requires a head SHA")
		}
	}
	git := jobGitClient(checkout, localDispatchJobRunner(request))
	path, err := allocateDispatchReadOnlyWorktree(ctx, store, paths.Home, repo, checkout, jobID, "readonly-seat", 0, baseRef, workflow.ReadOnlyWorktreeDispatchLockWaitBudget, git)
	if err == nil || strings.TrimSpace(request.Action) != "review" {
		return path, err
	}
	// A cold checkout may not have the PR commit object even though the forge
	// supplied its SHA. Preserve the existing pull/<n>/head fetch fallback before
	// refusing the dispatch.
	if fetchErr := fetchDispatchReviewPullRequest(ctx, git, request.PullRequest); fetchErr != nil {
		return "", fmt.Errorf("allocate review worktree: %w; fetch PR ref: %v", err, fetchErr)
	}
	path, retryErr := allocateDispatchReadOnlyWorktree(ctx, store, paths.Home, repo, checkout, jobID, "readonly-seat", 0, baseRef, workflow.ReadOnlyWorktreeDispatchLockWaitBudget, git)
	if retryErr != nil {
		return "", fmt.Errorf("allocate review worktree after fetch: %w", retryErr)
	}
	return path, nil
}

func reviewReadOnlyWorktreeCapacity(home string) error {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return err
	}
	usage, err := measureDiskGuardFilesystem(paths.Home)
	if err != nil {
		return fmt.Errorf("review worktree capacity is unknown: %w", err)
	}
	if usage.FreeBytes < reviewReadOnlyWorktreeMinFreeBytes {
		return fmt.Errorf("review worktree requires at least %d free bytes; %d available", reviewReadOnlyWorktreeMinFreeBytes, usage.FreeBytes)
	}
	return nil
}

func printLocalAgentJobOutput(stdout io.Writer, output localAgentJobOutput) {
	writeLine(stdout, "job: %s", output.JobID)
	writeLine(stdout, "state: %s", output.State)
	writeLine(stdout, "repo: %s", output.Repo)
	writeLine(stdout, "agent: %s", output.Agent)
	writeLine(stdout, "action: %s", output.Action)
	if output.AdvanceError != "" {
		writeLine(stdout, "advance_error: %s", output.AdvanceError)
	}
	if output.WatchCommand != "" {
		writeLine(stdout, "next: %s", output.WatchCommand)
	}
	if output.Result == nil {
		return
	}
	writeLine(stdout, "decision: %s", output.Result.Decision)
	writeLine(stdout, "summary: %s", output.Result.Summary)
	printRawMessages(stdout, "findings", output.Result.Findings)
	printStringList(stdout, "needs", output.Result.Needs)
	printStringList(stdout, "tests_run", output.Result.TestsRun)
	printStringList(stdout, "delegations", delegationAgentNames(output.Result.Delegations))
}

func delegationAgentNames(delegations []workflow.Delegation) []string {
	names := make([]string, 0, len(delegations))
	for _, d := range delegations {
		name := strings.TrimSpace(d.Agent)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func printRawMessages(stdout io.Writer, label string, values []json.RawMessage) {
	if len(values) == 0 {
		return
	}
	writeLine(stdout, "%s:", label)
	for _, value := range values {
		writeLine(stdout, "- %s", strings.TrimSpace(string(value)))
	}
}

func printStringList(stdout io.Writer, label string, values []string) {
	if len(values) == 0 {
		return
	}
	writeLine(stdout, "%s:", label)
	for _, value := range values {
		writeLine(stdout, "- %s", value)
	}
}
