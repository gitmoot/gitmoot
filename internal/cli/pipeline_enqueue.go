package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type pipelineStageEnqueuer = pipeline.PipelineStageEnqueuer

func pipelineStageCheckoutPath(ctx context.Context, store *db.Store, repo string) string {
	if strings.TrimSpace(repo) == "" {
		return ""
	}
	record, err := store.GetRepo(ctx, repo)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(record.CheckoutPath)
}

// newPipelineStageEnqueuer builds the production enqueuer: a Mailbox bound to the
// store and the daemon's canary-routing policy, so a pipeline stage job is
// indistinguishable from a normal background job once enqueued (the runner agent
// carries no template, so canary never actually samples).
func newPipelineStageEnqueuer(store *db.Store, home string) pipelineStageEnqueuer {
	mailbox := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("pipeline stage enqueue"))
	mailbox.CanaryEnabled = canaryRoutingEnabled(home)
	mailbox.RuntimeDefaultModel = runtimeDefaultModelResolver(home)
	mailbox.RequireWorkflowPolicy = requireWorkflowPolicyResolver(home)
	mailbox.OrgPolicy = orgPolicyResolver(home)
	return func(ctx context.Context, request workflow.JobRequest) (db.Job, error) {
		backend, err := localAgentDispatchExecBackendFor(home)
		if err != nil {
			return db.Job{}, err
		}
		runner, err := jobSubprocessRunnerForBackend(backend)
		if err != nil {
			return db.Job{}, err
		}
		// #1011 service shell stages are an explicit fail-CLOSED exception to the
		// generic read-only allocator below. Their run row is authoritative: every
		// service-triggered shell command gets a detached worktree, and a missing
		// checkout/allocation failure aborts enqueue instead of falling back to the
		// registered checkout.
		serviceShell, err := pipelineServiceShellStage(ctx, store, request)
		if err != nil {
			return db.Job{}, err
		}
		var worktreePath string
		var worktreeErr error
		if serviceShell {
			// Allocation precedes Mailbox.Enqueue. If a scan crashed after enqueue but
			// before recording stage.job_id, adopt the deterministic existing job before
			// touching its equally deterministic worktree path.
			existing, getErr := store.GetJob(ctx, request.ID)
			if getErr == nil {
				payload, parseErr := workflow.ParseJobPayload(existing.Payload)
				if parseErr != nil {
					return db.Job{}, fmt.Errorf("parse existing service shell stage payload: %w", parseErr)
				}
				if strings.TrimSpace(payload.WorktreePath) == "" || !payload.ReadOnlyWorktree {
					return db.Job{}, errors.New("existing service shell stage is not isolated in a detached worktree")
				}
				return existing, nil
			}
			if !errors.Is(getErr, sql.ErrNoRows) {
				return db.Job{}, getErr
			}
			request, worktreePath, worktreeErr = allocatePipelineServiceShellWorktreeForRunner(ctx, store, home, request, runner)
			if worktreeErr != nil {
				return db.Job{}, fmt.Errorf("allocate service shell stage detached worktree: %w", worktreeErr)
			}
			if strings.TrimSpace(worktreePath) == "" {
				return db.Job{}, errors.New("service shell stage requires a detached worktree; managed repo checkout is unavailable")
			}
		}
		// Repo-bound agent stages are born in detached committed-tip worktrees
		// before enqueue. For ask/review, that worktree is a security boundary:
		// allocation must fail closed rather than silently run the prompt against
		// the shared checkout. Produce keeps its separate fail-closed writable
		// worktree policy. Shell stages carry a RuntimeOverride and use their own
		// service/non-service allocation policies.
		isolateShell := !serviceShell && pipelineShellStageReadOnlyWorktreeEligible(request)
		isolateAgentSeat := !serviceShell && pipelineStageReadOnlySeatEligible(request)
		if !serviceShell {
			if isolateShell {
				request, worktreePath, worktreeErr = allocatePipelineShellStageReadOnlyWorktreeForRunner(ctx, store, home, request, runner)
			} else {
				request, worktreePath, worktreeErr = allocatePipelineStageReadOnlyWorktreeForRunner(ctx, store, home, request, runner)
			}
		}
		if strings.TrimSpace(request.Action) == "produce" && strings.TrimSpace(request.WorktreePath) == "" {
			reason := "produce stage requires a disposable detached worktree; managed repo checkout is unavailable"
			if worktreeErr != nil {
				reason = fmt.Sprintf("produce stage requires a disposable detached worktree: %v", worktreeErr)
			}
			return createFailedPipelineProduceJob(ctx, store, request, reason)
		}
		// A source-bound review is pinned to the implement job's immutable PR head.
		// Unlike a generic read-only stage, it must never fail open onto the shared
		// checkout: that checkout may be on the default branch, which would review the
		// wrong tree. Keep generic #739 allocation fail-open, but fail this one binding
		// closed unless the pinned detached worktree was allocated successfully.
		if pipelineStageSourceBoundReviewRequest(request) && strings.TrimSpace(request.WorktreePath) == "" {
			if worktreeErr != nil {
				return db.Job{}, fmt.Errorf("allocate PR-bound pipeline review worktree at %s: %w", request.HeadSHA, worktreeErr)
			}
			return db.Job{}, fmt.Errorf("allocate PR-bound pipeline review worktree at %s: managed repo checkout is unavailable", request.HeadSHA)
		}
		if isolateAgentSeat && strings.TrimSpace(request.WorktreePath) == "" {
			if worktreeErr != nil {
				return db.Job{}, fmt.Errorf("allocate read-only pipeline %s worktree: %w", request.Action, worktreeErr)
			}
			return db.Job{}, fmt.Errorf("allocate read-only pipeline %s worktree: managed repo checkout is unavailable", request.Action)
		}
		// #768: a MUTATING implement stage takes the WRITABLE task-worktree path
		// instead of the read-only committed-tip worktree — it must commit + push. Unlike
		// the read-only allocator (fail-OPEN), this one is fail-CLOSED: on an active
		// implement job / live process / uncommitted changes it errors and the stage is
		// NOT enqueued, so a retry can never duplicate or clobber a branch/PR (`gitmoot
		// task recover` is the operator escape hatch). The two allocators are mutually
		// exclusive — read-only eligibility excludes the implement action.
		var writableErr error
		request, writableErr = allocatePipelineStageWritableWorktreeForRunner(ctx, store, home, request, runner)
		if writableErr != nil {
			return db.Job{}, writableErr
		}
		job, err := mailbox.Enqueue(ctx, request)
		if err != nil {
			// The worktree is created on disk BEFORE Enqueue; a failed Enqueue leaves
			// no job row, so neither the terminal cleanup nor the daemon reclaim pass
			// would ever dispose it. Roll it back here (detached from a possibly
			// cancelled ctx) exactly as the #739 dispatch path does. Enqueue commonly
			// fails with context.Canceled on daemon shutdown, so BOTH the checkout
			// lookup AND the removal must run on a WithoutCancel ctx — otherwise the
			// lookup itself returns context.Canceled -> empty checkout -> the removal
			// is skipped and the just-created worktree leaks with no recovery path.
			if worktreePath != "" {
				rollbackCtx := context.WithoutCancel(ctx)
				if checkout := pipelineStageCheckoutPath(rollbackCtx, store, request.Repo); checkout != "" {
					_ = jobGitClient(checkout, runner).RemoveWorktreeForce(rollbackCtx, worktreePath)
				}
			}
			return db.Job{}, err
		}
		// Emit the isolation outcome now that the job row exists (events carry a JobID
		// FK). Allocated → observable worktree:<path> key; a fail-open skip → a loud
		// event so a lost-parallelism serialize is never silent (#739).
		if worktreePath != "" {
			message := fmt.Sprintf("read-only worktree %s allocated for agent stage (#757/#739); job keyed worktree:<path> to run beside same-repo stages", worktreePath)
			if serviceShell {
				message = fmt.Sprintf("detached worktree %s allocated for service shell stage (#1011); registered checkout is never used", worktreePath)
			} else if isolateShell {
				message = fmt.Sprintf("read-only worktree %s allocated for opted-in shell stage (#1016); job keyed worktree:<path> and a job-scoped shell runtime-session key so it runs concurrently with same-repo siblings, including identical-command forks (#1034)", worktreePath)
			}
			_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "readonly_worktree_allocated", Message: message})
		} else if worktreeErr != nil {
			message := fmt.Sprintf("read-only worktree isolation skipped for agent stage (#757/#739); job runs serialized in the shared checkout: %v", worktreeErr)
			if isolateShell {
				message = fmt.Sprintf("read-only worktree isolation skipped for opted-in shell stage (#1016); job runs serialized in the shared checkout: %v", worktreeErr)
			}
			_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "readonly_worktree_skipped", Message: message})
		}
		return job, nil
	}
}

func pipelineServiceShellStage(ctx context.Context, store *db.Store, request workflow.JobRequest) (bool, error) {
	if request.Sender != workflow.PipelineJobSender || strings.TrimSpace(request.RuntimeOverride) != runtime.ShellRuntime {
		return false, nil
	}
	runID := strings.TrimSpace(request.RootJobID)
	if runID == "" {
		return false, nil // defensive compatibility for synthetic/non-run requests
	}
	run, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("load pipeline run %s for shell isolation: %w", runID, err)
	}
	return ok && strings.TrimSpace(run.Trigger) == "service", nil
}

func allocatePipelineServiceShellWorktree(ctx context.Context, store *db.Store, home string, request workflow.JobRequest) (workflow.JobRequest, string, error) {
	return allocatePipelineServiceShellWorktreeForRunner(ctx, store, home, request, subprocess.ExecRunner{})
}

func allocatePipelineServiceShellWorktreeForRunner(ctx context.Context, store *db.Store, home string, request workflow.JobRequest, runner subprocess.Runner) (workflow.JobRequest, string, error) {
	checkout := pipelineStageCheckoutPath(ctx, store, request.Repo)
	if checkout == "" {
		return request, "", nil
	}
	paths, err := pathsFromFlag(home)
	if err != nil {
		return request, "", err
	}
	path, err := workflow.AllocateReadOnlyWorktree(ctx, store, paths.Home, request.Repo, checkout, request.ID,
		"pipeline-service-stage", 0, "", workflow.ReadOnlyWorktreeDispatchLockWaitBudget, jobGitClient(checkout, runner))
	if err != nil {
		return request, "", err
	}
	if strings.TrimSpace(path) == "" {
		return request, "", nil
	}
	request.WorktreePath = path
	request.ReadOnlyWorktree = true
	return request, path, nil
}

// createFailedPipelineProduceJob records a fail-closed produce allocation as a
// real terminal stage job, including a loud event and result summary. The
// advancer can therefore fold/retry it normally without ever exposing a queued
// job whose cwd would resolve to the managed checkout.
func createFailedPipelineProduceJob(ctx context.Context, store *db.Store, request workflow.JobRequest, reason string) (db.Job, error) {
	payload := workflow.JobPayload{
		Repo:          request.Repo,
		Sender:        request.Sender,
		Instructions:  request.Instructions,
		RootJobID:     request.RootJobID,
		JobTimeout:    request.JobTimeout,
		Fingerprint:   request.Fingerprint,
		WritablePaths: append([]string(nil), request.WritablePaths...),
		ReadablePaths: append([]string(nil), request.ReadablePaths...),
		Network:       request.Network,
		Check:         request.Check,
		CheckRetries:  request.CheckRetries,
		Result:        &workflow.AgentResult{Decision: "failed", Summary: reason},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return db.Job{}, err
	}
	job := db.Job{ID: request.ID, Agent: request.Agent, Type: request.Action, State: string(workflow.JobFailed), Payload: string(encoded)}
	if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{JobID: job.ID, Kind: "produce_worktree_failed", Message: reason}); err != nil {
		if existing, getErr := store.GetJob(ctx, request.ID); getErr == nil {
			return existing, nil
		}
		return db.Job{}, err
	}
	return job, nil
}

func pipelineStageSourceBoundReviewRequest(request workflow.JobRequest) bool {
	return request.Sender == workflow.PipelineJobSender &&
		strings.TrimSpace(request.Action) == "review" &&
		request.PullRequest > 0
}

// pipelineStageReadOnlyWorktreeEligible reports whether a stage job request is a
// repo-bound AGENT stage that should be born with its own detached read-only
// worktree (#757). It is true only for a pipeline-sender ask/review job bound to a
// named agent, running against a repo, that carries NO runtime override (the shell
// runner sets one) and NO worktree yet. A pure-reasoning agent stage with no repo,
// and every shell stage, are excluded — they never need a worktree.
func pipelineStageReadOnlyWorktreeEligible(request workflow.JobRequest) bool {
	if request.Sender != workflow.PipelineJobSender {
		return false
	}
	if strings.TrimSpace(request.RuntimeOverride) != "" {
		return false // the hidden shell runner, not an agent stage
	}
	if strings.TrimSpace(request.Agent) == "" {
		return false
	}
	switch strings.TrimSpace(request.Action) {
	case "ask", "review", "produce":
	default:
		return false
	}
	if strings.TrimSpace(request.Repo) == "" {
		return false // pure-reasoning stage, nothing to isolate
	}
	return strings.TrimSpace(request.WorktreePath) == ""
}

func pipelineStageReadOnlySeatEligible(request workflow.JobRequest) bool {
	if !pipelineStageReadOnlyWorktreeEligible(request) {
		return false
	}
	switch strings.TrimSpace(request.Action) {
	case "ask", "review":
		return true
	default:
		return false
	}
}

// pipelineShellStageReadOnlyWorktreeEligible reports whether a non-service shell
// stage explicitly opted into fail-open committed-tip isolation. Service shell
// stages are detected before this seam and retain their fail-closed #1011 path.
func pipelineShellStageReadOnlyWorktreeEligible(request workflow.JobRequest) bool {
	return request.Sender == workflow.PipelineJobSender &&
		request.IsolateShellStage &&
		strings.TrimSpace(request.RuntimeOverride) == runtime.ShellRuntime &&
		strings.TrimSpace(request.Repo) != "" &&
		strings.TrimSpace(request.WorktreePath) == ""
}

// allocatePipelineShellStageReadOnlyWorktree gives an opted-in non-service shell
// stage its own detached committed-tip worktree. Allocation is FAIL-OPEN: callers
// enqueue the unchanged request on any error and emit readonly_worktree_skipped.
// Unlike agent isolation, shell commands receive the live managed checkout through
// GITMOOT_CHECKOUT and do not get prose appended to their fixed instructions.
func allocatePipelineShellStageReadOnlyWorktree(ctx context.Context, store *db.Store, home string, request workflow.JobRequest) (workflow.JobRequest, string, error) {
	return allocatePipelineShellStageReadOnlyWorktreeForRunner(ctx, store, home, request, subprocess.ExecRunner{})
}

func allocatePipelineShellStageReadOnlyWorktreeForRunner(ctx context.Context, store *db.Store, home string, request workflow.JobRequest, runner subprocess.Runner) (workflow.JobRequest, string, error) {
	checkout := pipelineStageCheckoutPath(ctx, store, request.Repo)
	if checkout == "" {
		return request, "", nil // repo not managed/checked out yet — serialize as before
	}
	paths, err := pathsFromFlag(home)
	if err != nil {
		return request, "", err
	}
	path, err := workflow.AllocateReadOnlyWorktree(ctx, store, paths.Home, request.Repo, checkout, request.ID, "pipeline-stage", 0, "", workflow.ReadOnlyWorktreeDispatchLockWaitBudget, jobGitClient(checkout, runner))
	if err != nil {
		return request, "", err
	}
	if strings.TrimSpace(path) == "" {
		return request, "", errors.New("read-only worktree allocator returned an empty path")
	}
	request.WorktreePath = path
	request.ReadOnlyWorktree = true
	request.ShellEnv = append(request.ShellEnv, "GITMOOT_CHECKOUT="+checkout)
	return request, path, nil
}

// allocatePipelineStageReadOnlyWorktree allocates a detached committed-tip worktree
// for an eligible repo-bound agent stage and returns the request with WorktreePath +
// the ReadOnlyWorktree disposal marker set (so the existing terminal cleanup and
// daemon reclaim dispose it) plus the #654 context note appended to Instructions. It
// resolves the ref to the checkout HEAD (always resolvable) via the shared
// workflow.AllocateReadOnlyWorktree primitive under the short
// ReadOnlyWorktreeDispatchLockWaitBudget. It is FAIL-OPEN: an ineligible stage or an
// unknown checkout returns the request UNCHANGED with a nil error and empty path; a
// genuine allocation failure returns the request unchanged with the error so the
// caller enqueues on the shared checkout and emits a loud skip event.
func allocatePipelineStageReadOnlyWorktree(ctx context.Context, store *db.Store, home string, request workflow.JobRequest) (workflow.JobRequest, string, error) {
	return allocatePipelineStageReadOnlyWorktreeForRunner(ctx, store, home, request, subprocess.ExecRunner{})
}

func allocatePipelineStageReadOnlyWorktreeForRunner(ctx context.Context, store *db.Store, home string, request workflow.JobRequest, runner subprocess.Runner) (workflow.JobRequest, string, error) {
	if !pipelineStageReadOnlyWorktreeEligible(request) {
		return request, "", nil
	}
	checkout := pipelineStageCheckoutPath(ctx, store, request.Repo)
	if checkout == "" {
		return request, "", nil // repo not managed/checked out yet — serialize as before
	}
	paths, err := pathsFromFlag(home)
	if err != nil {
		return request, "", err
	}
	// A source-bound review carries HeadSHA; use it as the detached base ref so the
	// existing review checkout validation proves HEAD == payload.HeadSHA. Other
	// read-only stages keep the empty-ref behavior (the checkout's committed tip).
	baseRef := strings.TrimSpace(request.HeadSHA)
	path, err := workflow.AllocateReadOnlyWorktree(ctx, store, paths.Home, request.Repo, checkout, request.ID, "pipeline-stage", 0, baseRef, workflow.ReadOnlyWorktreeDispatchLockWaitBudget, jobGitClient(checkout, runner))
	if err != nil {
		return request, "", err
	}
	if strings.TrimSpace(path) == "" {
		return request, "", nil
	}
	request.WorktreePath = path
	request.ReadOnlyWorktree = true
	switch strings.TrimSpace(request.Action) {
	case "ask", "review":
		request.ReadOnlySeat = true
	}
	// The detached worktree is the committed tip, so it omits gitignored paths
	// (repos/**) and uncommitted changes; point the read-only stage at the canonical
	// checkout for those (#654), exactly as the delegation/dispatch paths do.
	if note := workflow.ReadOnlyWorktreeContextNote(checkout); note != "" {
		request.Instructions += note
	}
	return request, path, nil
}

// pipelineStageImplementWorktreeEligible reports whether a stage job request is a
// repo-bound MUTATING implement stage (#768) that needs a WRITABLE task-worktree.
// True only for a pipeline-sender implement job bound to a named agent, running
// against a repo, that carries NO runtime override (the shell runner sets one) and NO
// worktree yet. Every read-only agent stage (ask/review) and every shell stage is
// excluded — the read-only allocator (which itself excludes non-ask/review) owns those.
func pipelineStageImplementWorktreeEligible(request workflow.JobRequest) bool {
	if request.Sender != workflow.PipelineJobSender {
		return false
	}
	if strings.TrimSpace(request.RuntimeOverride) != "" {
		return false
	}
	if strings.TrimSpace(request.Agent) == "" {
		return false
	}
	if strings.TrimSpace(request.Action) != "implement" {
		return false
	}
	if strings.TrimSpace(request.Repo) == "" {
		return false
	}
	return strings.TrimSpace(request.WorktreePath) == ""
}

// allocatePipelineStageWritableWorktree gives a MUTATING implement stage (#768) a real
// WRITABLE task-worktree on its DETERMINISTIC branch by REUSING the existing implement
// dispatch preparation (prepareLocalImplementDispatchRequest): its GetTaskByRepoBranch
// reuse lands a retry in the SAME branch/worktree (never a duplicate PR), and its
// fail-closed guards (an active implement job, a live process still inside the
// worktree, or uncommitted changes) reject a retry that would clobber or duplicate
// work. Unlike the read-only allocator it is FAIL-CLOSED: any error propagates so the
// stage is NOT enqueued. An ineligible request (every non-implement stage) returns
// unchanged with a nil error. On success the request carries the task worktree path +
// the resolved deterministic branch/task/head, so the enqueued job keys worktree:<path>
// (mutating same-repo stages parallelize; the only serialization is the brief
// checkout-mutation lock during allocation). ReadOnlyWorktree is deliberately left
// false — the task worktree is durable (disposed by the task lifecycle, not the #739
// read-only cleanup).
func allocatePipelineStageWritableWorktree(ctx context.Context, store *db.Store, home string, request workflow.JobRequest) (workflow.JobRequest, error) {
	return allocatePipelineStageWritableWorktreeForRunner(ctx, store, home, request, subprocess.ExecRunner{})
}

func allocatePipelineStageWritableWorktreeForRunner(ctx context.Context, store *db.Store, home string, request workflow.JobRequest, runner subprocess.Runner) (workflow.JobRequest, error) {
	if !pipelineStageImplementWorktreeEligible(request) {
		return request, nil
	}
	record, err := store.GetRepo(ctx, request.Repo)
	if err != nil {
		return request, fmt.Errorf("resolve repo %q for implement stage: %w", request.Repo, err)
	}
	repo, err := daemon.ParseRepository(request.Repo)
	if err != nil {
		return request, err
	}
	// Ensure the DETERMINISTIC task row exists so prepareLocalImplementDispatchRequest's
	// BRANCH-reuse path adopts THIS run+stage's task id — and, crucially, so its
	// fail-closed guards (active job / live process / uncommitted changes) run on EVERY
	// attempt. We therefore hand it an EMPTY TaskID (which routes through that guarded
	// branch-reuse block) plus the deterministic Branch; passing a non-empty TaskID would
	// skip the guards entirely. Idempotent: created once (Planned), reused thereafter.
	if _, gerr := store.GetTask(ctx, request.TaskID); gerr != nil {
		if !errors.Is(gerr, sql.ErrNoRows) {
			return request, gerr
		}
		if uerr := store.UpsertTask(ctx, db.Task{
			ID:           request.TaskID,
			RepoFullName: request.Repo,
			GoalID:       firstNonEmpty(request.GoalID, "pipeline"),
			Title:        firstNonEmpty(request.TaskTitle, request.TaskID),
			State:        string(workflow.TaskPlanned),
			Branch:       request.Branch,
		}); uerr != nil {
			return request, uerr
		}
	}
	dispatch := localAgentDispatchRequest{
		Home:         home,
		Agent:        request.Agent,
		Action:       "implement",
		Instructions: request.Instructions,
		Branch:       request.Branch,
		GoalID:       request.GoalID,
		TaskTitle:    request.TaskTitle,
		RepoFlag:     request.Repo,
		jobRunner:    runner,
	}
	task, dispatch, err := prepareLocalImplementDispatchRequest(ctx, store, record, repo, dispatch)
	if err != nil {
		return request, err
	}
	request.WorktreePath = task.WorktreePath
	request.Branch = dispatch.Branch
	request.TaskID = dispatch.TaskID
	request.HeadSHA = dispatch.HeadSHA
	request.GoalID = firstNonEmpty(request.GoalID, dispatch.GoalID)
	request.TaskTitle = firstNonEmpty(request.TaskTitle, dispatch.TaskTitle)
	request.LeadAgent = firstNonEmpty(request.LeadAgent, dispatch.LeadAgent)
	return request, nil
}
