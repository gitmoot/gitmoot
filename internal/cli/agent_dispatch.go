package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

type localAgentDispatchRequest struct {
	RepoFlag     string
	Agent        string
	Action       string
	Instructions string
	Background   bool
	Type         string
	Model        string
	Effort       string
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
	HeadSHA          string
	// ImplementBase is the CLI/config worktree base for implement dispatches.
	// Before the request can enqueue, it is resolved to a commit SHA and
	// ImplementBaseResolved is set so allocation uses that exact commit.
	ImplementBase          string
	ImplementBaseResolved  bool
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
}

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
	repo, record, err := resolveLocalAgentRepo(ctx, store, request.RepoFlag)
	if err != nil {
		return localAgentJobOutput{}, err
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
			request.ImplementBase, err = resolveLocalImplementBase(ctx, paths, record, base)
			if err != nil {
				return localAgentJobOutput{}, err
			}
			request.ImplementBaseResolved = true
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
		return enqueuePermissionBlockedLocalAgentJob(ctx, store, request, repo.FullName(), record.DefaultBranch, agent.Name, overrideRuntime, overrideRef)
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
	if readOnlyImplementationBlocked(request.Action, effectiveAgent) {
		return enqueuePermissionBlockedLocalAgentJob(ctx, store, request, repo.FullName(), record.DefaultBranch, agent.Name, overrideRuntime, overrideRef)
	}
	switch request.Action {
	case "review":
		var checkout string
		var err error
		request, checkout, err = prepareLocalReviewDispatchRequest(ctx, store, record, repo, request)
		if err != nil {
			return localAgentJobOutput{}, err
		}
		if strings.TrimSpace(checkout) != "" {
			checkoutPath = checkout
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
	// #739: give a BACKGROUND read-only (ask) job its own detached committed-tip
	// worktree BEFORE enqueue — the proven read-only delegation fan-out shape — so
	// its checkout key is worktree:<path> (queuedJobCheckoutKey) and same-repo
	// seats (moot, chat-task, autorespond, `agent ask --background`) run
	// concurrently instead of serializing on the shared repo:<repo> key. Foreground
	// asks run inline and never serialize, so they are left untouched. Review is
	// excluded because it already carries a per-PR task worktree (TaskID set) that
	// keys it off its own worktree. FAIL-OPEN: on any allocation failure the job is
	// enqueued unchanged (shared checkout, serialized) with a loud skip event —
	// dispatch never fails for a lost-parallelism optimization.
	jobID := localAgentJobID(request.Action, agent.Name)
	readOnlyWorktreePath, readOnlyWorktreeErr := maybeAllocateDispatchReadOnlyWorktree(ctx, store, request, repo.FullName(), record.CheckoutPath, jobID)
	if readOnlyWorktreePath != "" {
		// The detached worktree is the committed tip of the checkout HEAD, which may
		// have advanced past any inherited HeadSHA. Clear it so the job validates
		// against its own fresh worktree HEAD (matching the delegation path). The
		// worktree omits gitignored (repos/**) and uncommitted working-tree files, so
		// point the job at the canonical base checkout for those (#654).
		request.HeadSHA = ""
		if note := workflow.ReadOnlyWorktreeContextNote(record.CheckoutPath); note != "" {
			request.Instructions += note
		}
	}
	job, err := (workflow.Mailbox{Store: store, CanaryEnabled: canaryRoutingEnabled(request.Home)}).Enqueue(ctx, workflow.JobRequest{
		ID:                     jobID,
		Agent:                  agent.Name,
		Action:                 request.Action,
		Repo:                   repo.FullName(),
		Branch:                 firstNonEmpty(request.Branch, record.DefaultBranch),
		PullRequest:            request.PullRequest,
		HeadSHA:                request.HeadSHA,
		GoalID:                 request.GoalID,
		TaskID:                 request.TaskID,
		TaskTitle:              request.TaskTitle,
		LeadAgent:              firstNonEmpty(request.LeadAgent, agent.Name),
		Reviewers:              request.Reviewers,
		Sender:                 "local",
		Instructions:           request.Instructions,
		Model:                  request.Model,
		Effort:                 request.Effort,
		RuntimeOverride:        overrideRuntime,
		RuntimeOverrideRef:     overrideRef,
		Cockpit:                request.Cockpit,
		CockpitSession:         request.CockpitSession,
		SkipNativeReviewFanout: request.SkipNativeReviewFanout,
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
			_ = gitutil.Client{Dir: record.CheckoutPath}.RemoveWorktreeForce(context.WithoutCancel(ctx), readOnlyWorktreePath)
		}
		return localAgentJobOutput{}, err
	}
	if err := releaseReservation(ctx); err != nil {
		return localAgentJobOutput{}, err
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
	// Thread the owner token so a foreground run's terminal cleanup (RunJob ->
	// AdvanceJob, which fires while this lock is still held) recognizes its own lock
	// and does not refuse the healthy-path cleanup as a foreign live owner (#536).
	ctx = workflow.WithRuntimeSelfOwnerToken(ctx, ownerToken)
	// Adapter selection uses the EFFECTIVE runtime: this is the seam that makes
	// a --runtime override actually deliver through the override adapter (#531).
	adapter, err := runtimeStartAdapter(newRuntimeFactory(), effectiveAgent.Runtime, checkoutPath)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	if overrideRuntime != "" {
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_override", Message: jobRuntimeOverrideEventMessage(agent.Runtime, effectiveAgent, lockKey)}); err != nil {
			return localAgentJobOutput{}, err
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
			handled, abErr = maybeRunLiveAB(runCtx, store, request, agent, job, adapter, managed.OK)
			if abErr != nil {
				return localAgentJobOutput{}, foregroundAskTimeoutError(runCtx, jobTimeout, abErr)
			}
		}
		if !handled {
			// Wire the home-aware registry defaults so a foreground ask with no
			// agent/job model or effort pin honors the runtime's defaults too.
			// Fail-open/empty by default; an agent/job pin wins.
			mailbox := workflow.Mailbox{Store: store, RuntimeDefaultModel: runtimeDefaultModelResolver(request.Home), RuntimeDefaultEffort: runtimeDefaultEffortResolver(request.Home)}
			if _, err := mailbox.Run(runCtx, job.ID, effectiveAgent, adapter); err != nil {
				return localAgentJobOutput{}, foregroundAskTimeoutError(runCtx, jobTimeout, err)
			}
		}
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_completed", Message: "workflow advancement completed"}); err != nil {
			return localAgentJobOutput{}, err
		}
	} else {
		workflowHome := ""
		if paths, err := pathsFromFlag(request.Home); err == nil {
			workflowHome = paths.Home
		}
		engine := daemonWorkflowEngine(store, github.NewClient(checkoutPath), checkoutPath, workflowHome)
		if _, err := engine.RunJob(runCtx, job.ID, effectiveAgent, adapter); err != nil {
			if out, ok, _ := recoverAdvanceErrorOutput(ctx, store, job.ID, request, err); ok {
				return out, nil
			}
			return localAgentJobOutput{}, err
		}
	}
	latest, err := store.GetJob(ctx, job.ID)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	return buildLocalAgentJobOutput(latest, request)
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

func enqueuePermissionBlockedLocalAgentJob(ctx context.Context, store *db.Store, request localAgentDispatchRequest, repo string, defaultBranch string, agentName string, overrideRuntime string, overrideRef string) (localAgentJobOutput, error) {
	job, err := (workflow.Mailbox{Store: store, CanaryEnabled: canaryRoutingEnabled(request.Home)}).Enqueue(ctx, workflow.JobRequest{
		ID:           localAgentJobID(request.Action, agentName),
		Agent:        agentName,
		Action:       request.Action,
		Repo:         repo,
		Branch:       firstNonEmpty(request.Branch, defaultBranch),
		PullRequest:  request.PullRequest,
		HeadSHA:      request.HeadSHA,
		GoalID:       request.GoalID,
		TaskID:       request.TaskID,
		TaskTitle:    request.TaskTitle,
		LeadAgent:    request.LeadAgent,
		Reviewers:    request.Reviewers,
		Sender:       "local",
		Instructions: request.Instructions,
		// Persist the per-job --model and the resolved --runtime/--session override
		// (#531) on the BLOCKED job too: `gitmoot job retry` re-runs the stored
		// payload as-is, so dropping them here would silently retry the job on the
		// agent's default runtime — resuming the default-runtime session the user's
		// --runtime explicitly asked it to stay off.
		Model:              request.Model,
		Effort:             request.Effort,
		RuntimeOverride:    overrideRuntime,
		RuntimeOverrideRef: overrideRef,
	})
	if err != nil {
		return localAgentJobOutput{}, err
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "route_selected", Message: routeSelectedMessage(request)}); err != nil {
		return localAgentJobOutput{}, err
	}
	if _, err := markJobPermissionBlocked(ctx, store, job.ID); err != nil {
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

func prepareLocalReviewDispatchRequest(ctx context.Context, store *db.Store, record db.Repo, repo github.Repository, request localAgentDispatchRequest) (localAgentDispatchRequest, string, error) {
	if request.PullRequest <= 0 {
		return localAgentDispatchRequest{}, "", errors.New("agent review requires --pr number")
	}
	if strings.TrimSpace(request.Branch) != "" && strings.TrimSpace(request.HeadSHA) != "" {
		return prepareLocalReviewWorktree(ctx, store, record, repo, request)
	}
	pr, err := github.NewClient(record.CheckoutPath).GetPullRequest(ctx, repo, int64(request.PullRequest))
	if err != nil {
		return localAgentDispatchRequest{}, "", fmt.Errorf("resolve pull request #%d: %w", request.PullRequest, err)
	}
	if strings.TrimSpace(request.Branch) == "" {
		request.Branch = pr.HeadRef
	}
	if strings.TrimSpace(request.HeadSHA) == "" {
		request.HeadSHA = pr.HeadSHA
	}
	return prepareLocalReviewWorktree(ctx, store, record, repo, request)
}

func prepareLocalReviewWorktree(ctx context.Context, store *db.Store, record db.Repo, repo github.Repository, request localAgentDispatchRequest) (localAgentDispatchRequest, string, error) {
	if strings.TrimSpace(request.HeadSHA) == "" {
		return localAgentDispatchRequest{}, "", errors.New("agent review requires a pull request head SHA")
	}
	if strings.TrimSpace(request.Branch) != "" {
		if task, err := store.GetTaskByRepoBranch(ctx, repo.FullName(), request.Branch); err == nil && strings.TrimSpace(task.WorktreePath) != "" {
			head, headErr := (gitutil.Client{Dir: task.WorktreePath}).HeadSHA(ctx)
			if headErr != nil {
				return localAgentDispatchRequest{}, "", headErr
			}
			if head == request.HeadSHA {
				request.TaskID = task.ID
				request.GoalID = firstNonEmpty(request.GoalID, task.GoalID)
				request.TaskTitle = firstNonEmpty(request.TaskTitle, task.Title)
				return request, task.WorktreePath, nil
			}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return localAgentDispatchRequest{}, "", err
		}
	}
	paths, err := initializedPaths(request.Home)
	if err != nil {
		return localAgentDispatchRequest{}, "", err
	}
	taskID := strings.TrimSpace(request.TaskID)
	if taskID == "" {
		taskID = fmt.Sprintf("review-pr-%d-%s", request.PullRequest, shortHash(repo.FullName()+"\x00"+request.HeadSHA))
	}
	path, err := workflow.TaskWorktreePath(paths.Home, repo.FullName(), taskID)
	if err != nil {
		return localAgentDispatchRequest{}, "", err
	}
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return localAgentDispatchRequest{}, "", err
		}
		git := gitutil.Client{Dir: record.CheckoutPath}
		if err := git.AddDetachedWorktree(ctx, path, request.HeadSHA); err != nil {
			if fetchErr := git.FetchPullRequest(ctx, "origin", request.PullRequest); fetchErr != nil {
				return localAgentDispatchRequest{}, "", fmt.Errorf("create PR review worktree: %w; fetch PR ref: %v", err, fetchErr)
			}
			if retryErr := git.AddDetachedWorktree(ctx, path, request.HeadSHA); retryErr != nil {
				return localAgentDispatchRequest{}, "", fmt.Errorf("create PR review worktree after fetch: %w", retryErr)
			}
		}
	}
	task := db.Task{
		ID:           taskID,
		RepoFullName: repo.FullName(),
		GoalID:       firstNonEmpty(request.GoalID, "local-review"),
		Title:        firstNonEmpty(request.TaskTitle, fmt.Sprintf("Review PR #%d", request.PullRequest)),
		State:        string(workflow.TaskReviewing),
		WorktreePath: path,
	}
	if err := store.UpsertTask(ctx, task); err != nil {
		return localAgentDispatchRequest{}, "", err
	}
	request.TaskID = task.ID
	request.GoalID = task.GoalID
	request.TaskTitle = task.Title
	return request, path, nil
}

func prepareLocalImplementDispatchRequest(ctx context.Context, store *db.Store, record db.Repo, repo github.Repository, request localAgentDispatchRequest) (db.Task, localAgentDispatchRequest, error) {
	paths, err := initializedPaths(request.Home)
	if err != nil {
		return db.Task{}, localAgentDispatchRequest{}, err
	}
	baseSHA := strings.TrimSpace(request.ImplementBase)
	if !request.ImplementBaseResolved {
		baseSHA, err = resolveLocalImplementBase(ctx, paths, record, baseSHA)
		if err != nil {
			return db.Task{}, localAgentDispatchRequest{}, err
		}
	}
	taskID := strings.TrimSpace(request.TaskID)
	taskTitle := strings.TrimSpace(request.TaskTitle)
	goalID := strings.TrimSpace(request.GoalID)
	branchHint := strings.TrimSpace(request.Branch)
	if taskID == "" && branchHint != "" {
		existing, err := store.GetTaskByRepoBranch(ctx, repo.FullName(), branchHint)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return db.Task{}, localAgentDispatchRequest{}, err
		}
		if err == nil {
			if !taskBranchReusableForImplement(existing.State) {
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
			if dirty, err := taskWorktreeDirty(ctx, existing); err != nil {
				return db.Task{}, localAgentDispatchRequest{}, err
			} else if dirty {
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
	owner := strings.TrimSpace(request.Agent)
	started, err := (workflow.Engine{Store: store}).AllocateTaskWorktree(ctx, workflow.TaskWorktreeRequest{
		Home:       paths.Home,
		Repo:       repo.FullName(),
		GoalID:     task.GoalID,
		TaskID:     task.ID,
		TaskTitle:  task.Title,
		Branch:     branch,
		BaseBranch: baseSHA,
		Owner:      owner,
		Checkout:   record.CheckoutPath,
	}, gitutil.Client{Dir: record.CheckoutPath})
	if err != nil {
		return db.Task{}, localAgentDispatchRequest{}, err
	}
	headSHA, err := (gitutil.Client{Dir: started.WorktreePath}).HeadSHA(ctx)
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

// resolveLocalImplementBase returns the exact commit an implement worktree must
// start from. A CLI value wins over [workflow].implement_base. With neither set,
// HEAD preserves checkout-following behavior after the stale-feature guard.
func resolveLocalImplementBase(ctx context.Context, paths config.Paths, record db.Repo, requested string) (string, error) {
	base := strings.TrimSpace(requested)
	if base == "" {
		configured, err := config.LoadImplementBase(paths)
		if err != nil {
			return "", fmt.Errorf("load workflow implement_base: %w", err)
		}
		base = strings.TrimSpace(configured)
	}
	git := gitutil.Client{Dir: record.CheckoutPath}
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
	adapter, err := runtimeStartAdapter(newRuntimeFactory(), instanceAgent.Runtime, record.CheckoutPath)
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
	if strings.TrimSpace(repoFlag) == "" {
		record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: "."})
		if err != nil {
			return github.Repository{}, db.Repo{}, err
		}
		return repo, record, nil
	}
	if existing, err := store.GetRepo(ctx, repo.FullName()); err == nil && strings.TrimSpace(existing.CheckoutPath) != "" {
		record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: existing.CheckoutPath})
		if err != nil {
			return github.Repository{}, db.Repo{}, err
		}
		// repoRecordForCheckout reports the checkout's current branch. For a
		// registered repo, keep the stored default branch so an old feature checkout
		// cannot redefine the base branch during dispatch.
		if strings.TrimSpace(existing.DefaultBranch) != "" {
			record.DefaultBranch = existing.DefaultBranch
		}
		record.PollInterval = existing.PollInterval
		return repo, record, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return github.Repository{}, db.Repo{}, err
	}
	record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: "."})
	if err != nil {
		return github.Repository{}, db.Repo{}, err
	}
	return repo, record, nil
}

func localAgentTargetRepo(ctx context.Context, repoFlag string) (github.Repository, error) {
	if strings.TrimSpace(repoFlag) != "" {
		return daemon.ParseRepository(repoFlag)
	}
	remote, err := (gitutil.Client{Dir: "."}).OriginRemote(ctx)
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
// true ONLY for a BACKGROUND read-only (ask/review) job that does not already
// carry a task worktree: a foreground ask runs inline and never serializes, and a
// job with a TaskID (every `agent review`, which prepares a per-PR worktree
// upstream) is already keyed off its own worktree. Mirrors poolIsolationEligible.
func dispatchReadOnlyWorktreeEligible(request localAgentDispatchRequest) bool {
	if !request.Background {
		return false
	}
	switch strings.TrimSpace(request.Action) {
	case "ask", "review":
	default:
		return false
	}
	return strings.TrimSpace(request.TaskID) == ""
}

// maybeAllocateDispatchReadOnlyWorktree allocates a throwaway detached
// committed-tip worktree for an eligible background read-only job so it is born
// with a distinct worktree:<path> checkout key (#739). It resolves the ref to the
// checkout HEAD (always resolvable — the researchers' diagnostic showed the
// stale-branch ref is a red herring; the real trigger was the reactive path being
// headroom-gated and silent) via the shared workflow.AllocateReadOnlyWorktree
// primitive, holding the checkout mutation lock. It is FAIL-OPEN: it returns
// ("", nil) when the job is ineligible or the checkout is unknown, and ("", err)
// on a genuine allocation failure so the caller enqueues unchanged and emits a
// loud skip event. The manager is the checkout-bound gitutil.Client, exactly as
// the reactive pool-isolation path builds it.
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
	// Ref defaults to HEAD (baseBranch left empty): a committed-tip worktree that is
	// always resolvable, avoiding the stale current-branch fragility the researchers
	// flagged. The #654 context note points the job at the canonical checkout for
	// uncommitted/gitignored paths.
	return workflow.AllocateReadOnlyWorktree(ctx, store, paths.Home, repo, checkout, jobID, "readonly-seat", 0, "", workflow.ReadOnlyWorktreeDispatchLockWaitBudget, gitutil.Client{Dir: checkout})
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
