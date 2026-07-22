package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// The checkout-bound git client backs every per-delegation worktree role; assert
// at compile time so the engine's runtime type-assertions can never silently fall
// back (which would skip read-only-fanout or #332 integration worktrees).
var (
	_ workflow.WorktreeManager            = gitutil.Client{}
	_ workflow.ReadOnlyWorktreeManager    = gitutil.Client{}
	_ workflow.IntegrationWorktreeManager = gitutil.Client{}
	_ workflow.WorktreeCommitter          = gitutil.Client{}
)

// daemonWorkflowEngine builds the per-tick/per-repo workflow.Engine. Its `home`
// param is — by convention (#459) — the already-RESOLVED <home>/.gitmoot root
// (config.Paths.Home), NOT the raw --home. All three callers comply:
// jobWorker.workflowHome() (resolves ConfigHome once), the registered-repo
// supervisor (paths.Home), and local dispatch (paths.Home). The resolved root is
// used verbatim for engine.ArtifactRoot, engine.Home, and daemonEventSink — none
// of which re-resolve — so handing it the raw --home would misplace delegation
// artifacts and the event-sink config probe.
func daemonWorkflowEngine(store *db.Store, gh github.Client, checkout string, home string) workflow.Engine {
	engine := workflow.Engine{
		Store:                   store,
		RequireWorkflowPolicy:   requireWorkflowPolicyResolverRoot(home),
		ProduceCheckDir:         checkout,
		MergeGate:               daemonMergeGate{Store: store, GitHub: gh, FallbackCheckout: checkout, Home: home},
		ImplementationFinalizer: daemonImplementationFinalizer{Store: store, GitHub: gh, FallbackCheckout: checkout},
		// escalate_human (#340): @-tag the human on the tree's PR/issue when a leg
		// pauses awaiting a decision. Best-effort and nil-safe in the engine; the
		// handle is filled in from policy by applyOrchestratePolicy.
		EscalationNotifier: &daemonEscalationNotifier{Store: store, GitHub: gh},
		// Off-by-default outbound event stream (#446): the engine emits
		// job.finished/job.failed/job.blocked on its terminal Mailbox path and
		// job.needs_attention on an escalate_human pause through this best-effort,
		// nil-safe sink. daemonEventSink returns nil unless [events].webhook_url is
		// set, so with no config NO sink is constructed and behavior is
		// byte-identical. The sink is a process-global shared singleton (one drain
		// goroutine), so re-building the engine per tick never leaks goroutines.
		EventSink: daemonEventSink(store, home),
		// Off-by-default Mode-A trace-harvester (#465): on a verifiable implement-job
		// outcome (merge merged/blocked, review changes_requested, revert) the engine
		// harvests a synthetic {score, feedback} FeedbackEvent for the job's template
		// version through this best-effort, nil-safe seam. daemonOutcomeHarvester
		// returns nil unless [skillopt].auto_trace_enabled is set, so with no config
		// NO harvester is constructed and behavior — and every human-run
		// TrainingPackage — is byte-identical. The harvester writes ONLY
		// eval/feedback rows; promotion stays 100% manual (the #484 canary wrapper
		// below is the only path that may graduate/roll back, and only when canary
		// mode is configured AND a live canary exists).
		//
		// Off-by-default #484 canary regression window: when [skillopt].auto_promote_canary
		// is configured with a valid sample, the base harvester is wrapped so that AFTER
		// a verifiable outcome it loads the active canary + prior champion auto-trace runs
		// and graduates (-> current) or auto-rolls-back (reusing RevertAgentTemplateVersion
		// to keep the champion live + rejecting the canary) on a material regression.
		// daemonOutcomeHarvesterWithCanary returns the bare base harvester when canary is
		// off and nil when auto_trace is off, so both default paths stay byte-identical.
		OutcomeHarvester: daemonOutcomeHarvesterWithCanary(store, gh, home),
		// Off-by-default cross-family review-agent soft signal (#469): on a MERGE the
		// engine additionally runs a read-only CROSS-FAMILY review leg (off the
		// blocking merge path, best-effort) whose subjective-quality + scope-fidelity
		// rubric is projected into a SECOND, judge-tagged, down-weighted FeedbackEvent
		// in the SAME auto-trace run. daemonReviewLegDispatcher returns nil unless BOTH
		// [skillopt].cross_family_review_enabled AND auto_trace_enabled are set, so with
		// no config NO review leg runs and NO review row is written — byte-identical.
		// A review-leg failure never blocks or fails a job; promotion stays manual.
		ReviewLegDispatcher: daemonReviewLegDispatcher(store, gh, checkout, home),
		// Off-by-default OBJECTIVE deterministic-checker signal (#485): on a MERGE the
		// engine additionally runs a best-effort, DETACHED leg of plain external tools
		// (duplication/lint/complexity) + a pure-Go diff-size metric whose tool-derived
		// [0,1] dimensions are projected into a THIRD, objective-tagged FeedbackEvent in
		// the SAME auto-trace run, distinct from the verifiable floor and the subjective
		// review. daemonDeterministicCheckerDispatcher returns nil unless BOTH
		// [skillopt].deterministic_checkers_enabled AND auto_trace_enabled are set, so
		// with no config NO checker leg runs and NO checker row is written —
		// byte-identical. A missing tool/checkout/timeout SKIPS that dimension and never
		// blocks or fails the merge; promotion stays manual.
		DeterministicCheckerDispatcher: daemonDeterministicCheckerDispatcher(store, gh, checkout, home),
		// Off-by-default deterministic HARD-verifier tier (#474): on a MERGE the engine
		// additionally runs the operator's configured build/test/lint commands in a
		// FRESH sandbox checkout at the merged head (exit 0 == pass), best-effort and
		// DETACHED, and projects the binary pass/fail as the authoritative
		// EvaluatorScore.Hard into the SAME auto-trace run — an un-gameable gate distinct
		// from the verifiable floor, the subjective review, and the objective checker.
		// daemonHardVerifierDispatcher returns nil unless [skillopt].hard_verifiers_enabled
		// AND auto_trace_enabled are set AND at least one command is configured, so with
		// no config NO verifier leg runs and NO hard row is written — byte-identical. A
		// slow suite / unprovisionable sandbox never blocks or fails the merge; promotion
		// stays manual.
		HardVerifierDispatcher: daemonHardVerifierDispatcher(store, checkout, home),
		// Off-by-default agent persistent memory (#626, Phase 1 observation mode):
		// when at least one agent is enrolled ([agents.<name>].memory = true) and the
		// global kill switch is off, the engine's Mailbox injects a "Prior learnings"
		// block into enrolled agents' prompts (READ) and shadow-logs their returned
		// learnings + writes mechanical facts at job terminal (WRITE). daemonMemory-
		// Controller returns nil when nothing is enrolled (or on any config-load
		// error), so with no config NO memory hook is wired and prompt assembly +
		// the terminal path are byte-identical. Non-enrolled agents are never touched
		// even when the controller is present.
		Memory: daemonMemoryController(store, home),
		// Registry default model/effort fallbacks: when a delivered job pins no
		// agent/job override, fall back to the HOME-AWARE resolved runtime registry
		// (built-in defaults overlaid with [runtimes.<name>] config). Fail-open and
		// empty by default, so with no config no model or effort is forced; an
		// agent/job override always wins.
		RuntimeDefaultModel:  runtimeDefaultModelResolver(home),
		RuntimeDefaultEffort: runtimeDefaultEffortResolver(home),
		// Off-restores-byte-identical result-check audit (#526): the deterministic
		// binary-checklist audit of a job's parsed gitmoot_result. resultChecksMode
		// resolves the [workflow] result_checks knob (default warn) from the
		// home-aware config; result_checks = off restores the exact pre-feature
		// terminal path (no event, no payload field, no feed-forward row). Fail-safe
		// to the documented default warn on any load error.
		ResultCheckMode: resultChecksMode(home),
		PayloadRefresher: func(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
			return refreshDaemonJobPayload(ctx, store, checkout, job, payload)
		},
		// Gate the #484 canary ROUTING seam on the SAME policy.CanaryEnabled() the
		// OutcomeHarvester's regression comparator above is gated on, so both seams
		// are consistent: with canary off NO traffic is sampled (Mailbox.routeCanary
		// returns before its query, byte-identical) AND no comparator runs, so a
		// stranded canary row can never keep serving traffic with no auto-rollback.
		CanaryEnabled: canaryRoutingEnabled(home),
		// Off-by-default #530 coordinator routing-context injection: when [router]
		// context_enabled is set, the engine's Mailbox appends a bounded advisory
		// observed-performance table to a top-level coordinator job's prompt.
		// routerContextEnabled returns false with no config (or any load error), so
		// with no config NO telemetry query runs during a job and prompt assembly is
		// byte-identical. Capture (routing_telemetry rows) is always on and additive.
		RouterContextEnabled: routerContextEnabled(home),
	}
	// Opt-in risk-tiered adaptive review (#650): copy the [review] policy onto the
	// engine. Off by default (RiskTiersEnabled false), so the review fan-out is
	// byte-identical unless a home config turns it on.
	applyReviewPolicy(&engine, home)
	wireReviewRiskSignals(&engine, gh)
	if strings.TrimSpace(home) != "" {
		// Root delegation artifacts under GITMOOT_HOME (alongside worktrees)
		// rather than inside the repo checkout, so generated briefs stay out of
		// the tracked tree and are never committed.
		engine.ArtifactRoot = home
		engine.BeforeReadOnlyWorktreeCleanup = pipeline.PipelineServiceArtifactPrecleanupHook(store, config.Paths{Home: home})
	}
	if strings.TrimSpace(home) != "" && strings.TrimSpace(checkout) != "" {
		engine.Home = home
		engine.DelegationCheckout = checkout
		engine.DelegationWorktrees = gitutil.Client{Dir: checkout}
	}
	return engine
}

// daemonEscalationNotifier implements workflow.EscalationNotifier (#340): when a
// delegation tree pauses awaiting a human, it @-tags that human in a GitHub
// comment on the tree's PR (or the issue carrying the coordinator) with the
// resume instructions. Best-effort: any lookup/post failure is returned to the
// engine, which already treats notifier errors as non-fatal (the pause itself is
// durable via the task state + recorded event + dashboard Attention).
type daemonEscalationNotifier struct {
	Store  *db.Store
	GitHub github.Client
	// Handle is the configured escalation_handle (a GitHub login without the @).
	// Empty falls back to the PR author, then the repo owner.
	Handle string
}

func (n *daemonEscalationNotifier) NotifyEscalation(ctx context.Context, request workflow.EscalationRequest) error {
	if n == nil || n.Store == nil || n.GitHub == nil {
		return nil
	}
	repoFull := strings.TrimSpace(request.Repo)
	pull := request.PullRequest
	owner := ""
	// The engine seam leaves PR/repo best-effort; the coordinator job's payload is
	// the source of truth for both, so load it when either is missing.
	if repoFull == "" || pull <= 0 {
		if job, err := n.Store.GetJob(ctx, request.CoordinatorJobID); err == nil {
			if payload, perr := daemonJobPayload(job); perr == nil {
				if repoFull == "" {
					repoFull = strings.TrimSpace(payload.Repo)
				}
				if pull <= 0 {
					pull = payload.PullRequest
				}
			}
		}
	}
	if repoFull == "" || pull <= 0 {
		// No issue/PR to post on; the durable pause (state + event + Attention)
		// still stands. Nothing to notify.
		return nil
	}
	repo, err := daemon.ParseRepository(repoFull)
	if err != nil {
		return err
	}
	owner = repo.Owner

	// Default @-handle: the configured escalation_handle, else the repo owner (the
	// human who owns the tree). The PullRequest type carries no author field, so
	// the owner is the available, always-present human to tag.
	handle := strings.TrimPrefix(strings.TrimSpace(n.Handle), "@")
	if handle == "" {
		handle = owner
	}

	body := buildEscalationComment(handle, request)
	_, err = n.GitHub.PostIssueComment(ctx, repo, int64(pull), body)
	return err
}

// buildEscalationComment renders the @-tag escalation comment body (#340).
//
// The body must never begin a line with "@<handle>" or a bare "/gitmoot": the
// daemon ingests comments on its own PRs, and ParseCommand treats a line whose
// first token is "@<agent>" as a "@<agent> <action>" command — so a leading
// "@<handle> Gitmoot paused…" would make the daemon post a spurious "unsupported
// command action" ack on its own escalation notification. The human is mentioned
// mid-line ("cc @<handle>"), which still notifies them on GitHub but is not
// parsed as a command.
func buildEscalationComment(handle string, request workflow.EscalationRequest) string {
	if request.Ask {
		return buildAskGateComment(handle, request)
	}
	var b strings.Builder
	b.WriteString("Gitmoot paused a delegation tree awaiting your decision (escalate_human).\n")
	if h := strings.TrimPrefix(strings.TrimSpace(handle), "@"); h != "" {
		b.WriteString("cc @" + h + "\n")
	}
	b.WriteString("\n")
	if d := strings.TrimSpace(request.DelegationID); d != "" {
		b.WriteString(fmt.Sprintf("- failing leg: `%s`\n", d))
	}
	if r := strings.TrimSpace(request.Reason); r != "" {
		b.WriteString(fmt.Sprintf("- reason: %s\n", r))
	}
	if q := strings.TrimSpace(request.Question); q != "" {
		b.WriteString(fmt.Sprintf("- question: %s\n", q))
	}
	b.WriteString("\nResume with one of:\n")
	b.WriteString(fmt.Sprintf("- `/gitmoot resume %s retry <instructions>` — re-run the failing leg with your guidance\n", request.CoordinatorJobID))
	b.WriteString(fmt.Sprintf("- `/gitmoot resume %s continue` — proceed the coordinator with what completed\n", request.CoordinatorJobID))
	b.WriteString(fmt.Sprintf("- `/gitmoot resume %s abort` — stop and synthesize a best-effort final result\n", request.CoordinatorJobID))
	return b.String()
}

// buildAskGateComment renders the @-tag comment for a non-failure ask-gate pause
// (#445): a HEALTHY coordinator returned human_questions[] to ask a specific
// decision rather than guess. It quotes each question (id + prompt + choices) and
// gives the `answer` resume verb instead of the failure verbs. Like
// buildEscalationComment it never begins a line with "@<handle>" or "/gitmoot"
// (the human is mentioned mid-line) so the daemon does not parse its own
// notification as a command.
func buildAskGateComment(handle string, request workflow.EscalationRequest) string {
	var b strings.Builder
	b.WriteString("Gitmoot paused a job awaiting your answer to a question (no work failed; the agent chose to ask instead of guess).\n")
	if h := strings.TrimPrefix(strings.TrimSpace(handle), "@"); h != "" {
		b.WriteString("cc @" + h + "\n")
	}
	b.WriteString("\nQuestions:\n")
	if len(request.Questions) > 0 {
		for _, q := range request.Questions {
			line := fmt.Sprintf("- `%s`: %s", strings.TrimSpace(q.ID), strings.TrimSpace(q.Prompt))
			if len(q.Choices) > 0 {
				line += fmt.Sprintf(" (choices: %s)", strings.Join(q.Choices, ", "))
			}
			b.WriteString(line + "\n")
		}
	} else if q := strings.TrimSpace(request.Question); q != "" {
		b.WriteString(q + "\n")
	}
	b.WriteString("\nAnswer with:\n")
	b.WriteString(fmt.Sprintf("- `/gitmoot resume %s answer \"<id>: your answer\"` — one `<id>: ...` line per question\n", request.CoordinatorJobID))
	return b.String()
}

type daemonImplementationFinalizer struct {
	Store            *db.Store
	GitHub           github.Client
	FallbackCheckout string
}

func (f daemonImplementationFinalizer) FinalizeImplementation(ctx context.Context, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
	if f.Store == nil {
		return workflow.JobPayload{}, errors.New("implementation finalizer store is required")
	}
	if strings.TrimSpace(payload.TaskID) == "" {
		return payload, workflow.BlockedError{Reason: "implemented job has no task id; cannot finalize branch and PR"}
	}
	task, err := f.Store.GetTask(ctx, payload.TaskID)
	if err != nil {
		return payload, fmt.Errorf("load task %s for implementation finalizer: %w", payload.TaskID, err)
	}
	if strings.TrimSpace(task.WorktreePath) == "" {
		return payload, workflow.BlockedError{Reason: "implemented task has no worktree path; rerun through gitmoot task run or gitmoot agent implement"}
	}
	if strings.TrimSpace(task.Branch) == "" {
		return payload, workflow.BlockedError{Reason: "implemented task has no branch; cannot push or open PR"}
	}
	git := gitutil.Client{Dir: task.WorktreePath}
	branch, err := git.CurrentBranch(ctx)
	if err != nil {
		return payload, fmt.Errorf("resolve implementation branch: %w", err)
	}
	if branch != task.Branch {
		return payload, workflow.BlockedError{Reason: fmt.Sprintf("implemented task worktree is on branch %s, not %s", branch, task.Branch)}
	}
	validatedPR, hasValidatedPR, err := f.revalidateImplementationPullRequest(ctx, payload, task)
	if err != nil {
		return payload, err
	}
	// Write-ahead the skip-native-review-fanout flag onto the branch lock as soon
	// as the branch is confirmed — before EVERY downstream path that proceeds with
	// a PR: the no-changes-but-PR-exists early return below, the adopt path, and
	// the fresh EnsurePullRequest create. This closes the #390 TOCTOU: the daemon's
	// PR-watcher (trigger 2) must never observe a PR for this branch with the flag
	// still unpersisted. The branch lock already exists (acquired at job start);
	// SetBranchLockReviewFanout is an idempotent UPDATE keyed by repo+branch and a
	// no-op if the lock is somehow absent. Written only when set, mirroring the
	// engine path's default-fast on the common (false) case; the engine's
	// post-advance write now covers only the non-finalizer path (see engine.go).
	if payload.SkipNativeReviewFanout {
		if err := f.Store.SetBranchLockReviewFanout(ctx, payload.Repo, task.Branch, true); err != nil {
			return payload, fmt.Errorf("persist skip-native-review-fanout before opening PR: %w", err)
		}
	}
	status, err := git.StatusPorcelain(ctx)
	if err != nil {
		return payload, fmt.Errorf("inspect implementation diff: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		head, err := git.HeadSHA(ctx)
		if err != nil {
			return payload, fmt.Errorf("resolve clean implementation head: %w", err)
		}
		if strings.TrimSpace(payload.HeadSHA) == "" || head == payload.HeadSHA {
			if hasValidatedPR {
				return f.adoptValidatedImplementationPullRequest(ctx, payload, task, validatedPR, head)
			}
			if payload.PullRequest > 0 && head == payload.HeadSHA {
				payload.Branch = task.Branch
				return payload, nil
			}
			return payload, workflow.BlockedError{Reason: "implemented job produced no changes in the task worktree"}
		}
	} else {
		message := "Gitmoot implement " + task.ID
		if err := git.CommitAll(ctx, message); err != nil {
			return payload, workflow.BlockedError{Reason: "commit implementation changes failed: " + err.Error()}
		}
	}
	head, err := git.HeadSHA(ctx)
	if err != nil {
		return payload, fmt.Errorf("resolve implementation head after commit: %w", err)
	}
	if err := git.PushBranch(ctx, "origin", task.Branch); err != nil {
		return payload, workflow.BlockedError{Reason: "push implementation branch failed: " + err.Error()}
	}
	if hasValidatedPR {
		return f.adoptValidatedImplementationPullRequest(ctx, payload, task, validatedPR, head)
	}
	repo, err := daemon.ParseRepository(payload.Repo)
	if err != nil {
		return payload, err
	}
	record, err := f.Store.GetRepo(ctx, payload.Repo)
	if err != nil {
		return payload, err
	}
	base := strings.TrimSpace(record.DefaultBranch)
	if base == "" {
		base = "main"
	}
	if existing, ok, err := existingBranchPullRequest(ctx, f.Store, payload.Repo, task.Branch); err != nil {
		return payload, err
	} else if ok {
		payload.PullRequest = int(existing.Number)
		payload.HeadSHA = head
		payload.Branch = task.Branch
		if err := f.Store.UpsertPullRequest(ctx, db.PullRequest{
			RepoFullName: payload.Repo,
			Number:       existing.Number,
			URL:          existing.URL,
			HeadBranch:   task.Branch,
			BaseBranch:   firstNonEmpty(existing.BaseBranch, base),
			HeadSHA:      head,
			State:        firstNonEmpty(existing.State, "open"),
		}); err != nil {
			return payload, err
		}
		return payload, nil
	}
	// No local record yet: ensure the PR on GitHub idempotently. EnsurePullRequest
	// adopts an out-of-band/concurrent open PR for this head (and survives the 422
	// "already exists" create race) instead of erroring, so a benign race no longer
	// blocks the implementation after the work already landed.
	pr, err := f.githubClient(task.WorktreePath).EnsurePullRequest(ctx, github.CreatePullRequestInput{
		Repo:  repo,
		Title: finalizerPullRequestTitle(task),
		Body:  finalizerPullRequestBody(job, payload, task),
		Head:  task.Branch,
		Base:  base,
	})
	if err != nil {
		return payload, workflow.BlockedError{Reason: "open implementation PR failed: " + err.Error()}
	}
	payload.PullRequest = int(pr.Number)
	payload.Branch = task.Branch
	payload.HeadSHA = firstNonEmpty(pr.HeadSHA, head)
	if payload.TaskTitle == "" {
		payload.TaskTitle = task.Title
	}
	if payload.GoalID == "" {
		payload.GoalID = task.GoalID
	}
	if err := f.Store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: payload.Repo,
		Number:       pr.Number,
		URL:          pr.URL,
		HeadBranch:   firstNonEmpty(pr.HeadRef, task.Branch),
		BaseBranch:   firstNonEmpty(pr.BaseRef, base),
		HeadSHA:      payload.HeadSHA,
		State:        firstNonEmpty(pr.State, "open"),
	}); err != nil {
		return payload, err
	}
	return payload, nil
}

func (f daemonImplementationFinalizer) githubClient(checkout string) github.Client {
	if f.GitHub == nil {
		return github.NewClient(checkout)
	}
	if _, ok := f.GitHub.(*github.GhClient); ok {
		return github.NewClient(checkout)
	}
	return f.GitHub
}

func (f daemonImplementationFinalizer) revalidateImplementationPullRequest(ctx context.Context, payload workflow.JobPayload, task db.Task) (github.PullRequest, bool, error) {
	if !payload.ValidatedPullRequest {
		return github.PullRequest{}, false, nil
	}
	if payload.PullRequest <= 0 {
		return github.PullRequest{}, false, workflow.BlockedError{Reason: "validated implementation payload has no pull request number"}
	}
	repo, err := daemon.ParseRepository(payload.Repo)
	if err != nil {
		return github.PullRequest{}, false, err
	}
	pr, err := f.githubClient(task.WorktreePath).GetPullRequest(ctx, repo, int64(payload.PullRequest))
	if err != nil {
		return github.PullRequest{}, false, fmt.Errorf("revalidate fix-pass pull request #%d: %w", payload.PullRequest, err)
	}
	if pr.Number != int64(payload.PullRequest) {
		return github.PullRequest{}, false, workflow.BlockedError{Reason: fmt.Sprintf("fix-pass pull request revalidation returned #%d, want #%d", pr.Number, payload.PullRequest)}
	}
	if pr.Merged || strings.TrimSpace(pr.MergedAt) != "" || !strings.EqualFold(strings.TrimSpace(pr.State), "open") {
		return github.PullRequest{}, false, workflow.BlockedError{Reason: fmt.Sprintf("fix-pass pull request #%d is no longer open", payload.PullRequest)}
	}
	if strings.TrimSpace(pr.HeadRef) != task.Branch {
		return github.PullRequest{}, false, workflow.BlockedError{Reason: fmt.Sprintf("fix-pass pull request #%d now targets head branch %s, not task branch %s", payload.PullRequest, firstNonEmpty(pr.HeadRef, "<missing>"), task.Branch)}
	}
	if headRepo := strings.TrimSpace(pr.HeadRepoFullName); headRepo != "" && !strings.EqualFold(headRepo, payload.Repo) {
		return github.PullRequest{}, false, workflow.BlockedError{Reason: fmt.Sprintf("fix-pass pull request #%d head belongs to %s, not %s", payload.PullRequest, headRepo, payload.Repo)}
	}
	return pr, true, nil
}

func (f daemonImplementationFinalizer) adoptValidatedImplementationPullRequest(ctx context.Context, payload workflow.JobPayload, task db.Task, pr github.PullRequest, head string) (workflow.JobPayload, error) {
	base := strings.TrimSpace(pr.BaseRef)
	if base == "" {
		record, err := f.Store.GetRepo(ctx, payload.Repo)
		if err != nil {
			return payload, err
		}
		base = firstNonEmpty(strings.TrimSpace(record.DefaultBranch), "main")
	}
	payload.PullRequest = int(pr.Number)
	payload.Branch = task.Branch
	payload.HeadSHA = head
	if err := f.Store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: payload.Repo,
		Number:       pr.Number,
		URL:          pr.URL,
		HeadBranch:   task.Branch,
		BaseBranch:   base,
		HeadSHA:      head,
		State:        "open",
	}); err != nil {
		return payload, err
	}
	return payload, nil
}

func existingBranchPullRequest(ctx context.Context, store *db.Store, repo string, branch string) (db.PullRequest, bool, error) {
	pr, err := store.GetPullRequestByRepoBranch(ctx, repo, branch)
	if errors.Is(err, sql.ErrNoRows) {
		return db.PullRequest{}, false, nil
	}
	if err != nil {
		return db.PullRequest{}, false, err
	}
	if strings.EqualFold(pr.State, "closed") || strings.EqualFold(pr.State, "merged") {
		return db.PullRequest{}, false, nil
	}
	return pr, true, nil
}

func finalizerPullRequestTitle(task db.Task) string {
	title := strings.TrimSpace(task.Title)
	if title == "" {
		title = task.ID
	}
	return "Gitmoot: " + title
}

func finalizerPullRequestBody(job db.Job, payload workflow.JobPayload, task db.Task) string {
	summary := ""
	if payload.Result != nil {
		summary = strings.TrimSpace(payload.Result.Summary)
	}
	if summary == "" {
		summary = "Implementation completed by " + job.Agent + "."
	}
	body, err := workflow.RenderPullRequestBody(workflow.PullRequestBody{
		TaskID:          task.ID,
		AgentNames:      []string{job.Agent},
		What:            summary,
		Why:             "Gitmoot finalized this implementation from a task worktree.",
		Changes:         []string{"Committed changes from " + task.WorktreePath},
		Results:         finalizerResults(payload),
		Risk:            "Review the generated diff before merging.",
		RawReviewOutput: rawFinalizerOutput(payload),
	})
	if err == nil {
		return body
	}
	return summary
}

func finalizerResults(payload workflow.JobPayload) []string {
	if payload.Result == nil || len(payload.Result.TestsRun) == 0 {
		return []string{"No tests reported by the implementing agent."}
	}
	return append([]string{}, payload.Result.TestsRun...)
}

func rawFinalizerOutput(payload workflow.JobPayload) string {
	if payload.Result != nil && strings.TrimSpace(payload.Result.Summary) != "" {
		return payload.Result.Summary
	}
	if len(payload.RawOutputs) > 0 {
		return payload.RawOutputs[len(payload.RawOutputs)-1]
	}
	return "Implementation completed."
}

type daemonMergeGate struct {
	Store            *db.Store
	GitHub           github.Client
	FallbackCheckout string
	// Home is the resolved <home>/.gitmoot root (or raw --home) used to load the
	// [merge_gate] policy (#596). Empty => the off-by-default merge-gate behavior.
	Home string
}

func (g daemonMergeGate) Evaluate(ctx context.Context, request workflow.MergeRequest) (workflow.MergeDecision, error) {
	if nativeMergeGateDisabled() {
		return workflow.MergeDecision{
			Ready:  false,
			Reason: "native Gitmoot merge gate disabled by GITMOOT_DISABLE_NATIVE_MERGE_GATE; use external gate",
		}, nil
	}
	checkout, err := mergeGateCheckout(ctx, g.Store, request.Repo, g.FallbackCheckout)
	if err != nil {
		return workflow.MergeDecision{}, err
	}
	gate := newDaemonPolicyMergeGate(g.Store, g.githubClient(checkout), checkout)
	applyMergeGatePolicy(&gate, g.Home, request.Repo)
	// This last-moment check minimizes but does not eliminate the enqueue-to-merge
	// race: gate.Evaluate still performs review/CI reads before the squash merge, so
	// a job enqueued in that window can escape the check. A branch-activity
	// lease/barrier is the durable follow-up; until then, defer every job already in
	// flight and leave the task ready_to_merge for the next daemon tick.
	//
	// PRECONDITION (self-defer safety): the job that DROVE this evaluation must
	// already be terminal, or the gate would match it and defer forever. It holds
	// on every path today — AdvanceJob runs only after the mailbox transitions the
	// driving job to succeeded/failed/blocked, and the PR-watcher ready-to-merge
	// path is a daemon tick, not a job — so ListActiveJobs (queued/running only)
	// never sees the driver. Keep it that way: never call the gate from within a
	// still-running job's own execution.
	active, found, err := findActiveJobForBranch(ctx, g.Store, request.Repo, request.Branch)
	if err != nil {
		return workflow.MergeDecision{}, fmt.Errorf("inspect active jobs on merge branch: %w", err)
	}
	if found {
		return workflow.MergeDecision{
			Ready:      false,
			Merged:     false,
			Deferred:   true,
			Reason:     fmt.Sprintf("active %s job %s in flight on branch %s; holding merge until it settles", active.Type, active.ID, request.Branch),
			BlockClass: workflow.MergeBlockTransient,
		}, nil
	}
	return gate.Evaluate(ctx, request)
}

func nativeMergeGateDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_DISABLE_NATIVE_MERGE_GATE"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (g daemonMergeGate) githubClient(checkout string) github.Client {
	if g.GitHub == nil {
		return github.NewClient(checkout)
	}
	if _, ok := g.GitHub.(*github.GhClient); ok {
		return github.NewClient(checkout)
	}
	return g.GitHub
}

func newDaemonPolicyMergeGate(store *db.Store, gh github.Client, checkout string) workflow.PolicyMergeGate {
	return workflow.PolicyMergeGate{
		Store:        store,
		GitHub:       gh,
		Git:          gitutil.Client{Dir: checkout},
		Worktrees:    gitutil.Client{Dir: checkout},
		CheckoutPath: checkout,
		DeleteBranch: true,
	}
}

func refreshDaemonJobPayload(ctx context.Context, store *db.Store, checkout string, job db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
	if job.Type != "implement" || payload.Result == nil || payload.Result.Decision != "implemented" {
		return payload, nil
	}
	if !payloadHasTaskWorktree(ctx, store, payload) {
		head, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
		if err != nil {
			return workflow.JobPayload{}, err
		}
		payload.HeadSHA = head
	}
	if len(payload.Reviewers) == 0 {
		reviewers, err := daemonReviewers(ctx, store, payload.Repo)
		if err != nil {
			return workflow.JobPayload{}, err
		}
		payload.Reviewers = reviewers
	}
	return payload, nil
}

func payloadHasTaskWorktree(ctx context.Context, store *db.Store, payload workflow.JobPayload) bool {
	if store == nil {
		return false
	}
	taskID := strings.TrimSpace(payload.TaskID)
	if taskID == "" {
		return false
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		return false
	}
	return strings.TrimSpace(task.WorktreePath) != ""
}

func daemonReviewers(ctx context.Context, store *db.Store, repo string) ([]string, error) {
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	reviewers := []string{}
	for _, agent := range agents {
		allowed, err := store.AgentCanAccessRepo(ctx, agent.Name, repo)
		if err != nil {
			return nil, err
		}
		if allowed && agentHasCapability(agent.Capabilities, "review") {
			reviewers = append(reviewers, agent.Name)
		}
	}
	return reviewers, nil
}

func agentHasCapability(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if capability == target {
			return true
		}
	}
	return false
}
