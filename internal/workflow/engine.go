package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

type Engine struct {
	Store *db.Store
	// RequireWorkflowPolicy is passed to every mailbox the engine creates so
	// continuations and delegation enqueue share the same home-aware policy.
	RequireWorkflowPolicy func(repo string) RequireWorkflowPolicy
	// ProduceCheckDir is the resolved checkout cwd for trusted produce-stage
	// deterministic checks when no disposable worktree path is present.
	ProduceCheckDir         string
	RequiredReviewers       []string
	MergeGate               MergeGate
	JobID                   func(JobRequest) string
	PayloadRefresher        func(context.Context, db.Job, JobPayload) (JobPayload, error)
	ImplementationFinalizer ImplementationFinalizer
	// EscalationNotifier is the injected, best-effort seam (mirroring
	// ImplementationFinalizer) the engine calls when a delegation fails under the
	// escalate_human failure_policy and the tree pauses awaiting a human (#340).
	// The daemon implements it to @-tag the human in a PR/issue comment with the
	// resume instructions. It is optional: when nil the pause still happens (the
	// dashboard "Attention" section and the recorded event remain), only the
	// GitHub notification is skipped. A notifier error never fails the pause.
	EscalationNotifier EscalationNotifier
	// EventSink is the injected, best-effort outbound event seam (#446), mirroring
	// EscalationNotifier: when configured, the engine emits a redacted, versioned
	// events.Event on each terminal transition it owns (job.finished on a
	// succeeded terminal, job.needs_attention on an escalate_human pause) so an
	// off-box consumer (a webhook) can observe the run. It is optional and
	// nil-safe: when nil (the default, no [events] config) NO event is constructed
	// or emitted and behavior is byte-identical. Emit is fire-and-forget and best-
	// effort — a slow/hung/erroring sink never blocks or fails a job (see
	// internal/events). The daemon emits the failure/blocked/awaiting-human
	// terminal cases it owns through the SAME sink so the whole terminal set is
	// covered. #445 (the ask-gate) rides this seam to emit its job.needs_attention.
	EventSink events.Sink
	// BlockerDeferrer is the injected, best-effort, nil-by-default PRE-TERMINAL
	// operational-blocker deferrer (#532 slice E). When set, the engine's Mailbox
	// consults it on a delivery-seam failure BEFORE the terminal transition: if it
	// re-queues the job behind a classified operational blocker (runtime auth/quota/
	// network), Run reports ErrJobDeferred and never emits job.failed, so the
	// [events] stream sees the deferral as a first-class transition instead of a
	// failed→deferred flap. It mirrors EventSink: optional and nil-safe (when nil —
	// foreground/ask paths and every non-daemon construction — Run is byte-identical),
	// and best-effort (a deferrer error is treated as "not deferred" so the job takes
	// its normal terminal path). The concrete impl lives in cli (it classifies with
	// the #602 matcher and writes the payload hold fields), keeping the engine free of
	// the classification coupling; it is wired only on the daemon run path.
	BlockerDeferrer func(ctx context.Context, jobID string, cause error) (bool, error)
	// OutcomeHarvester is the injected, best-effort, nil-by-default seam (#465,
	// Mode A) the engine calls after a verifiable implement-job outcome transition
	// (merge merged/blocked, review changes_requested, revert) to harvest a
	// synthetic {score, feedback} FeedbackEvent for the job's template version. It
	// mirrors EventSink/EscalationNotifier: optional and nil-safe (when nil — the
	// default, no [skillopt].auto_trace_enabled — NO Outcome is constructed and
	// Harvest is never called, so behavior is byte-identical), and best-effort (a
	// Harvest error is swallowed and recorded as an auto_trace_harvest_failed job
	// event, never returned up). It writes ONLY eval/feedback rows and never
	// promotes. The concrete impl lives in internal/skillopt and is wired only in
	// cli (daemonWorkflowEngine), keeping the engine free of skillopt coupling.
	OutcomeHarvester OutcomeHarvester
	// ReviewLegDispatcher is the injected, best-effort, nil-by-default seam (#469)
	// the engine calls AFTER a merge harvest to run a CROSS-FAMILY review leg whose
	// rubric is projected into the SAME auto-trace run as a SOFT, down-weighted,
	// judge-tagged secondary signal. It mirrors OutcomeHarvester: optional and
	// nil-safe (when nil — the default, no [skillopt].cross_family_review_enabled —
	// NO review leg runs and NO review row is written, so behavior is
	// byte-identical), and best-effort (a dispatch error is swallowed and recorded
	// as a cross_family_review_failed job event, never returned up, so it can never
	// block or fail a job). It runs OFF the blocking merge path. The concrete impl
	// is wired only in cli (gated by cross_family_review_enabled AND
	// auto_trace_enabled), keeping the engine free of runtime/skillopt coupling.
	ReviewLegDispatcher ReviewLegDispatcher
	// DeterministicCheckerDispatcher is the injected, best-effort, nil-by-default
	// seam (#485) the engine calls AFTER a merge harvest to run an OBJECTIVE,
	// non-LLM deterministic-checker leg (code duplication / lint / cyclomatic
	// complexity tools + a pure-Go diff-size metric) whose tool-derived dimensions
	// are projected into the SAME auto-trace run as a THIRD coexisting signal,
	// distinct from the verifiable floor and the subjective cross-family review. It
	// mirrors ReviewLegDispatcher EXACTLY: optional and nil-safe (when nil — the
	// default, no [skillopt].deterministic_checkers_enabled — NO checker leg runs
	// and NO checker row is written, byte-identical), DETACHED off the blocking
	// merge path (a wedged tool can never stall AdvanceJob), and best-effort (a
	// dispatch error is swallowed and recorded as a deterministic_checkers_failed
	// job event, never returned up). The concrete impl is wired only in cli (gated
	// by deterministic_checkers_enabled AND auto_trace_enabled), keeping the engine
	// free of subprocess/skillopt coupling.
	DeterministicCheckerDispatcher DeterministicCheckerDispatcher
	// HardVerifierDispatcher is the injected, best-effort, nil-by-default seam (#474)
	// the engine calls AFTER a merge harvest to run the deterministic HARD-verifier
	// tier: the operator's configured build/test/lint COMMANDS run in a FRESH clean
	// sandbox checkout at the merged head (exit 0 == pass), producing a BINARY
	// pass/fail verdict the harvester maps onto the authoritative EvaluatorScore.Hard.
	// It mirrors DeterministicCheckerDispatcher EXACTLY: optional and nil-safe (when
	// nil — the default, no [skillopt].hard_verifiers_enabled — NO verifier leg runs
	// and NO hard row is written, byte-identical), DETACHED off the blocking merge
	// path (a slow test suite can never stall AdvanceJob or the daemon checkoutLock),
	// and best-effort (a dispatch error is swallowed and recorded as a
	// hard_verifiers_failed job event, never returned up). The concrete impl is wired
	// only in cli (gated by hard_verifiers_enabled AND auto_trace_enabled AND a
	// non-empty command list), keeping the engine free of subprocess/skillopt/git
	// coupling.
	HardVerifierDispatcher HardVerifierDispatcher
	// Home is the resolved GITMOOT_HOME root used to place per-delegation
	// worktrees. DelegationWorktrees is the checkout-bound git client that
	// performs the worktree-add. Both are optional: when either is unset, the
	// dispatcher enqueues implement delegations against the shared checkout
	// (legacy behavior) rather than allocating isolated worktrees.
	Home                string
	DelegationWorktrees WorktreeManager
	DelegationCheckout  string
	// BeforeReadOnlyWorktreeCleanup is an optional terminal hook invoked after a
	// read-only job has settled but before its detached worktree is force-removed.
	// It lets the CLI durably collect service-stage outputs without teaching the
	// workflow package about pipeline artifacts. An error is recorded as a job
	// event but never suppresses cleanup; the hook owns any durable failure marker.
	BeforeReadOnlyWorktreeCleanup func(context.Context, string, string, JobPayload) error
	// OwnerPIDLive reports whether a recorded owner PID is a live process on this
	// host. It gates the DESTRUCTIVE implement-delegation worktree/branch cleanup so
	// a worktree still owned by a live runtime worker is never force-removed out from
	// under it (#536): a job whose terminal state was synthesized by stale recovery
	// while its worker was still running keeps an unexpired/live runtime-session
	// lock, and cleanup refuses while that lock is active. Optional and nil-safe:
	// when nil the engine uses the default same-host syscall probe; tests inject a
	// fake. On a healthy terminal the lock is already released, so cleanup is
	// byte-identical to before this field existed.
	OwnerPIDLive func(pid int64) bool
	// WorktreeHasLiveProcess reports whether any live process on this host still has
	// its working directory inside the given worktree path. It is the PID-reuse- and
	// hostname-rename-immune never-clobber gate the DESTRUCTIVE implement-delegation
	// cleanup consults IN ADDITION to the runtime-session lock (#536 finding 1):
	// past lease expiry the lock is reaped, but a daemon-crash-reparented worker can
	// still be writing to the worktree. Removing it then would orphan the live worker
	// onto a deleted cwd — the original #536 corruption shifted to the lease boundary.
	// Optional and nil-safe: when nil the engine uses the default best-effort /proc
	// cwd scan (defaultWorktreeHasLiveProcess); tests inject a fake. On a healthy
	// terminal the worker has already exited, so the probe reports false and cleanup
	// proceeds unchanged.
	WorktreeHasLiveProcess func(path string) bool
	// CanaryEnabled gates the #484 canary ROUTING seam (Mailbox.routeCanary) on the
	// SAME [skillopt] policy.CanaryEnabled() the daemon's regression comparator
	// (daemonOutcomeHarvesterWithCanary) is gated on, so both seams turn on/off
	// together. It is resolved ONCE at daemon-engine construction
	// (daemonWorkflowEngine) and propagated into every Mailbox the engine builds via
	// mailbox(). Default false (the engine's zero value and every test/ask-path
	// Engine) means routing is off and resolution is byte-identical — a canary row
	// left behind by a since-disabled run is never sampled, so it can never serve
	// traffic without the comparator that would graduate or roll it back.
	CanaryEnabled bool
	// DelegationTimeoutDefaults carries optional [orchestrate] default child-job
	// timeouts. Empty fields mean unbounded. Explicit per-delegation timeout values
	// still win, so an engine with the zero value is byte-identical to historical
	// behavior.
	DelegationTimeoutDefaults DelegationTimeoutDefaults
	// ArtifactRoot is the filesystem root under which delegation artifacts
	// (delegations/<parent-job-id>/brief.md and context-manifest.json) are
	// written when a coordinator returns delegations that request artifacts.
	// It is the resolved GITMOOT_HOME root (already ending in .gitmoot), kept
	// outside any repo checkout so generated briefs are never committed. When
	// empty, artifact writing is skipped (ask-path and tests that build an
	// Engine without it keep their existing behavior).
	ArtifactRoot string
	// Now returns the current time and is the engine's only clock source. It is
	// optional and defaults to time.Now; tests inject it to drive the per-root
	// wall-clock backstop (see MaxDelegationWallClock) deterministically.
	Now func() time.Time
	// InlineArtifactBodies, when true, makes buildContinuationPrompt append each
	// finished child's payload.Result.ArtifactBody as a fenced block after the
	// child's decision/summary/PR line, so a coordinator continuation can read the
	// child briefs inline rather than re-opening every child job. It is opt-in
	// (default false) because inlining bodies can be large; when false the
	// continuation prompt is byte-identical to the legacy output.
	InlineArtifactBodies bool
	// MaxInlineArtifactBytes is the per-body cap (in bytes) applied to each child's
	// inlined ArtifactBody when InlineArtifactBodies is true. A value <= 0 means
	// defaultMaxInlineArtifactBytes. The total inlined across all children in one
	// continuation is additionally bounded by maxInlineArtifactTotalBytes.
	MaxInlineArtifactBytes int
	// InjectUpstreamDepContext, when true, makes deps[] real dataflow (#419):
	// when advanceDelegations enqueues a ready dependent leg, each of that leg's
	// succeeded DIRECT deps' results (decision, summary preview, PR link,
	// changes_made count, short HeadSHA, then the fenced artifact_body) are
	// appended to the dependent's Instructions as a byte-budgeted "Upstream
	// dependency results" block, so the dependent runs WITH its upstream results
	// rather than blind to them. It mirrors InlineArtifactBodies: opt-in (default
	// false) and reusing the SAME MaxInlineArtifactBytes per-body cap and
	// maxInlineArtifactTotalBytes aggregate budget (no new knob). With the flag
	// off the enqueued prompt is byte-identical to before this field existed.
	InjectUpstreamDepContext bool
	// RouterContextEnabled, when true, appends the bounded (<=12 line) advisory
	// observed-performance table (#530) to a TOP-LEVEL coordinator job's prompt so
	// the coordinator can weigh which runtime/model/template has done well on this
	// repo. It is opt-in via [router] context_enabled (default false); with the flag
	// off no telemetry query runs and prompt assembly is byte-identical. Routing
	// stays advisory in v1 — the block never forces a route.
	RouterContextEnabled bool
	// MaxDelegationTokenBudget is the cumulative per-root token budget (input +
	// output, summed across a coordination tree) that bounds a delegation tree by
	// cost in addition to depth/width/total-jobs/wall-clock (#338 Part B). When a
	// coordinator is about to dispatch a new generation and the tree has already
	// used at least this many tokens, dispatchDelegations refuses further fan-out
	// and routes to the #305 finalize continuation (delegation_cost_exceeded).
	// 0 (the default) means unlimited: the check is skipped entirely so default
	// behavior is byte-identical to before this knob existed. It is sourced from
	// the host [orchestrate].max_delegation_token_budget config at daemon startup.
	// NOTE: token capture is best-effort per runtime (see internal/runtime); a
	// runtime that does not report usage contributes 0 to the sum, so the budget
	// under-counts that runtime rather than failing.
	MaxDelegationTokenBudget int
	// MaxDelegationCostUSD is the cumulative per-root dollar-cost budget that bounds
	// a delegation tree by its measured spend, layered on top of the token budget
	// (#380). Cost is derived from the same per-job token usage the token budget
	// already sums, priced through a small per-model price table (see cost.go):
	// cost = Σ (input × input_price + output × output_price) over every job in the
	// tree. When a coordinator is about to dispatch a new generation and the tree
	// has already spent at least this many dollars, dispatchDelegations refuses
	// further fan-out and routes to the #305 finalize continuation
	// (delegation_cost_usd_exceeded). 0 (the default) means unlimited: the check is
	// skipped entirely so default behavior is byte-identical to before this knob
	// existed. It is sourced from the host [orchestrate].max_delegation_cost_usd
	// config at daemon startup. Because cost is derived from best-effort token
	// capture and a hardcoded price table, treat it as a coarse runaway-cost
	// backstop, not a precise spend meter.
	MaxDelegationCostUSD float64
	// MaxDelegationNonProgressStreak bounds how many consecutive continuation
	// generations a coordination tree may produce with NO new durable side effect
	// before the result-aware loop detector trips (#339). Where the structural
	// fast-path (handleDelegationLoop / canonicalDelegationSetHash) only catches a
	// coordinator literally re-issuing the same delegation SET, this catches a
	// coordinator that perturbs the set each round (evading the set hash) yet whose
	// children keep returning nothing new — comparing a mechanical progressDigest of
	// each generation's verifiable child side effects (decision, changes_made,
	// tests_run, PR/HeadSHA, artifact body) against the previous digest threaded
	// through the payload. A streak of unchanged digests at or above this threshold
	// trips the SAME ladder as the structural check (delegation_loop_warning +
	// corrective continuation, then delegation_loop_detected + graceful finalize).
	// Any new durable side effect resets the streak to 0 even if the self-reported
	// summary repeats. <= 0 means use defaultMaxDelegationNonProgressStreak (2); it
	// is configurable per-root alongside the depth/width/budget bounds.
	MaxDelegationNonProgressStreak int
	// MaxVerifyReplanAttempts bounds the engine-level verify→replan corrective loop
	// (#439): when a delegation set declares synthesis_rule "verify" and the
	// verify-tagged legs reach a FAILED verdict, the engine — instead of blocking
	// (vote/quorum) — enqueues a bounded corrective "replan" continuation so the
	// coordinator can self-correct. This is the dedicated per-root cap on how many
	// such replan attempts may fire before the loop routes to the #305 graceful
	// finalize continuation (verify_replan_exhausted) rather than looping forever.
	// It is layered ON TOP OF all existing structural bounds (depth/width/total
	// jobs/wall-clock/token/cost), which still count every replan continuation as a
	// generation. <= 0 means use defaultMaxVerifyReplanAttempts (2); it only ever
	// matters once a set actually tags a verify leg, so default behavior for every
	// existing set is byte-identical. It is sourced from the host
	// [orchestrate].max_verify_replan_attempts config at daemon startup.
	MaxVerifyReplanAttempts int
	// ReviewLegTimeout bounds the DETACHED cross-family review leg (#469): the
	// review runs a live LLM adapter.Deliver that can take minutes, so it must
	// never run unbounded. The detached goroutine's context is wrapped in this
	// timeout so a wedged reviewer process is reaped rather than leaking forever.
	// <= 0 means defaultReviewLegTimeout. It only matters when a ReviewLegDispatcher
	// is wired (off by default).
	ReviewLegTimeout time.Duration
	// ReviewSpawner runs the detached cross-family review leg OFF the AdvanceJob /
	// daemon-poll path so a live, possibly-wedged reviewer adapter.Deliver can never
	// block AdvanceJob, the worker tick, or the daemon's checkoutLock. The default
	// (nil) spawns a goroutine; tests inject a synchronous runner so the review is
	// deterministic. It mirrors the EventSink fire-and-forget seam: the engine hands
	// off a self-contained closure that owns its own bounded, cancellation-detached
	// context.
	ReviewSpawner func(func())
	// CheckerLegTimeout bounds the DETACHED deterministic-checker leg (#485): the
	// leg shells out to external tools (dupl/jscpd/golangci-lint/gocyclo) that can
	// be slow, so it must never run unbounded. The detached goroutine's context is
	// wrapped in this timeout so a wedged tool is reaped rather than leaking
	// forever. <= 0 means defaultCheckerLegTimeout. It only matters when a
	// DeterministicCheckerDispatcher is wired (off by default).
	CheckerLegTimeout time.Duration
	// CheckerSpawner runs the detached deterministic-checker leg OFF the AdvanceJob
	// / daemon-poll path so a slow, possibly-wedged external tool can never block
	// AdvanceJob, the worker tick, or the daemon's checkoutLock. The default (nil)
	// spawns a goroutine; tests inject a synchronous runner so the checker leg is
	// deterministic. It mirrors ReviewSpawner.
	CheckerSpawner func(func())
	// HardVerifierLegTimeout bounds the DETACHED hard-verifier leg (#474): the leg
	// provisions a fresh sandbox and runs the operator's build/test/lint commands,
	// which can be slow, so it must never run unbounded. The detached goroutine's
	// context is wrapped in this timeout so a wedged verifier is reaped rather than
	// leaking forever. <= 0 means defaultHardVerifierLegTimeout. It only matters when
	// a HardVerifierDispatcher is wired (off by default).
	HardVerifierLegTimeout time.Duration
	// HardVerifierSpawner runs the detached hard-verifier leg OFF the AdvanceJob /
	// daemon-poll path so a slow, possibly-wedged test suite can never block
	// AdvanceJob, the worker tick, or the daemon's checkoutLock. The default (nil)
	// spawns a goroutine; tests inject a synchronous runner so the verifier leg is
	// deterministic. It mirrors CheckerSpawner.
	HardVerifierSpawner func(func())
	// Memory is the injected, off-by-default agent persistent-memory controller
	// (#626). When set (only when at least one agent is enrolled and the global
	// kill switch is off), the engine's Mailbox injects a "Prior learnings" block
	// into the job prompt (READ path) and shadow-logs returned learnings + writes
	// mechanical facts at job terminal (WRITE path). When nil (the default, every
	// path with no enrolled agent), the Mailbox is built with nil memory hooks and
	// both prompt assembly and the terminal path are byte-identical.
	Memory *MemoryController
	// RuntimeDefaultModel, when set, resolves a runtime's configured registry
	// default_model (HOME-AWARE) for the runtime named by the argument (#652). It is
	// copied onto the Mailbox in mailbox() and consulted at delivery ONLY as the
	// final model fallback — after the job --model and the agent --model — so an
	// agent/job pin always wins. Nil (the default) forces nothing, so delivery is
	// byte-identical to before #652.
	RuntimeDefaultModel func(runtimeName string) string
	// RuntimeDefaultEffort mirrors RuntimeDefaultModel for the runtime registry's
	// default_effort fallback.
	RuntimeDefaultEffort func(runtimeName string) string
	// ResultCheckMode is the resolved [workflow] result_checks policy (#526): the
	// deterministic binary-checklist audit run on a job's parsed gitmoot_result.
	// It is copied onto every Mailbox the engine builds (mailbox()). The zero
	// value ("") and "off" disable the audit entirely, so an Engine built with a
	// bare struct literal (every test, the ask/foreground path) is byte-identical;
	// the daemon resolves the real mode (default warn) from config and sets it
	// here. "warn" records failures as a job event + job-detail field + feed-
	// forward row; "block" additionally fails the job via the contract-violation
	// path.
	ResultCheckMode ResultCheckMode
	// RiskTiersEnabled gates the opt-in risk-tiered adaptive review (#650). When
	// false (the default), HandlePullRequestOpened NEVER classifies a PR and runs
	// the single-review fan-out byte-identically. When true, a PR opened event is
	// classified (label > path > default); a `high` tier replaces the single
	// fan-out with a refutation-lens delegation batch synthesized by the EXISTING
	// quorum synthesis_rule engine, while a `routine` tier stays on the unchanged
	// single-review path. It is sourced from the host [review].risk_tiers_enabled
	// config at daemon startup.
	RiskTiersEnabled bool
	// HighRiskPaths is the changed-path glob list a PR is matched against to
	// resolve the `high` tier when RiskTiersEnabled. Empty falls back to
	// DefaultHighRiskPaths. Sourced from [review].high_risk_paths.
	HighRiskPaths []string
	// RiskLabelHigh / RiskLabelRoutine are the PR label names that force a tier,
	// winning over path heuristics. Empty falls back to DefaultRiskLabelHigh /
	// DefaultRiskLabelRoutine. Sourced from [review].risk_label_high /
	// [review].risk_label_routine.
	RiskLabelHigh    string
	RiskLabelRoutine string
	// PullRequestSignals resolves the risk classifier's inputs (PR labels +
	// changed file paths) for a PR whose event does not already carry them (#650).
	// It is the seam HandlePullRequestOpened uses on the IN-PROCESS implement->PR
	// trigger, which has no GitHub file data. It is nil-safe and best-effort: when
	// nil (every non-daemon construction) or when risk tiers are off, it is never
	// consulted and classification falls back to the event's own signals; a lookup
	// error yields no signals (the change classifies routine). The concrete impl is
	// wired only in cli (a GitHub read), keeping the engine free of the github
	// client coupling.
	PullRequestSignals func(ctx context.Context, repo string, number int) (labels []string, changedPaths []string, err error)
}

func (e Engine) block(ctx context.Context, ref taskRef, reason string) error {
	if err := e.setTaskState(ctx, ref, TaskBlocked); err != nil {
		return err
	}
	return BlockedError{Reason: reason}
}

func (e Engine) setTaskState(ctx context.Context, ref taskRef, state TaskState) error {
	if strings.TrimSpace(ref.ID) == "" {
		return nil
	}
	task := db.Task{
		ID:           ref.ID,
		RepoFullName: ref.Repo,
		GoalID:       ref.GoalID,
		Title:        ref.Title,
		State:        string(state),
		Branch:       ref.Branch,
	}
	existing, err := e.Store.GetTask(ctx, ref.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if existing.State == string(TaskDismissed) && state != TaskDismissed {
			return fmt.Errorf("task %s is dismissed; workflow advancement cannot move it to %s", existing.ID, state)
		}
		if task.GoalID == "" {
			task.GoalID = existing.GoalID
		}
		if task.RepoFullName == "" {
			task.RepoFullName = existing.RepoFullName
		}
		if task.Title == "" {
			task.Title = existing.Title
		}
		if task.Branch == "" {
			task.Branch = existing.Branch
		}
	}
	// One task per (repo, branch) is enforced by the tasks(repo_full_name, branch)
	// partial-unique index. If this ref carries a non-empty branch already owned by a
	// DIFFERENT task -- e.g. workflow advancement re-running a phase on the same branch
	// under a fresh task id -- upserting `task` would fail with
	// "UNIQUE constraint failed: tasks.repo_full_name, tasks.branch" and wedge the
	// advancement. Advance the branch's canonical task in place instead of inserting a
	// duplicate (the same branch-reuse invariant used by task creation).
	if task.Branch != "" {
		byBranch, berr := e.Store.GetTaskByRepoBranch(ctx, task.RepoFullName, task.Branch)
		if berr != nil && !errors.Is(berr, sql.ErrNoRows) {
			return berr
		}
		if berr == nil && byBranch.ID != task.ID {
			if byBranch.State == string(TaskDismissed) && state != TaskDismissed {
				return fmt.Errorf("task %s is dismissed; workflow advancement cannot move it to %s", byBranch.ID, state)
			}
			byBranch.State = string(state)
			return e.Store.UpsertTask(ctx, byBranch)
		}
	}
	return e.Store.UpsertTask(ctx, task)
}

func (e Engine) jobID(request JobRequest) string {
	if e.JobID != nil {
		return e.JobID(request)
	}
	hash := fnv.New64a()
	for _, value := range []string{
		request.Repo,
		request.Branch,
		strconv.Itoa(request.PullRequest),
		request.TaskID,
		request.Agent,
		request.Action,
		request.ReviewRound,
		request.Instructions,
	} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return "workflow-" + strconv.FormatUint(hash.Sum64(), 36)
}

type taskRef struct {
	ID     string
	Repo   string
	GoalID string
	Title  string
	Branch string
}

func taskRefFromPullRequest(event PullRequestEvent) taskRef {
	return taskRef{
		ID:     event.TaskID,
		Repo:   event.Repo,
		GoalID: event.GoalID,
		Title:  event.TaskTitle,
		Branch: event.Branch,
	}
}

func taskRefFromPayload(payload JobPayload) taskRef {
	return taskRef{
		ID:     payload.TaskID,
		Repo:   payload.Repo,
		GoalID: payload.GoalID,
		Title:  payload.TaskTitle,
		Branch: payload.Branch,
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
