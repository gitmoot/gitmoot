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
	"github.com/gitmoot/gitmoot/internal/execbackend"
)

type Engine struct {
	Store *db.Store
	// ResolveDeliveryWorktree is the injected CLI-owned checkout-resolution seam
	// used after an implement delivery. The workflow package must not import cli;
	// daemonWorkflowEngine wires the effective checkout selected by the worker.
	ResolveDeliveryWorktree DeliveryWorktreeResolver
	// CollectChangeSet is wired only on an engine that owns a live job-scoped
	// execution-backend instance. Keeping it nil everywhere else prevents a
	// backend-less job from reaching the transactional importer.
	CollectChangeSet func(ctx context.Context, backend execbackend.Backend, jobID string) (*execbackend.ChangeSet, error)
	// ApplyChangeSet is the optional host materializer test seam. Production
	// leaves it nil and Mailbox selects execbackend.ImportChangeSet.
	ApplyChangeSet func(ctx context.Context, worktree string, changes execbackend.ChangeSet) error
	// RequireWorkflowPolicy is passed to every mailbox the engine creates so
	// continuations and delegation enqueue share the same home-aware policy.
	RequireWorkflowPolicy func(repo string) RequireWorkflowPolicy
	// OrgPolicy is passed to every mailbox the engine creates. Descendant jobs are
	// exempt from the gate but preserve their initiating role as provenance.
	OrgPolicy func(repo string) OrgEnforcement
	// ProduceCheckDir is the resolved checkout cwd for trusted produce-stage
	// deterministic checks when no disposable worktree path is present.
	ProduceCheckDir         string
	RequiredReviewers       []string
	MergeGate               MergeGate
	JobID                   func(JobRequest) string
	PayloadRefresher        func(context.Context, db.Job, JobPayload) (JobPayload, error)
	ImplementationFinalizer ImplementationFinalizer
	// FixWorktreeAllocator provisions a writable, branch-attached checkout for an
	// engine-dispatched review fix before that job is enqueued. Unlike read-only
	// review isolation, allocation is fail-closed: without this checkout the only
	// fallback is the registered checkout, where a fix can overwrite a human's
	// uncommitted work (#1462).
	FixWorktreeAllocator FixWorktreeAllocator
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
	// Home is the resolved GITMOOT_HOME root used to place per-delegation
	// worktrees. DelegationWorktrees is the checkout-bound git client that
	// performs the worktree-add. Both are optional: when either is unset, the
	// dispatcher enqueues implement delegations against the shared checkout
	// (legacy behavior) rather than allocating isolated worktrees.
	Home                   string
	DelegationWorktrees    WorktreeManager
	DelegationCheckout     string
	cleanupTargetValidator func(home, jobID, jobType string, payload JobPayload) error
	// BeforeReadOnlyWorktreeCleanup is an optional terminal hook invoked after a
	// read-only job has settled but before its detached worktree is force-removed.
	// It lets the CLI durably collect service-stage outputs and worktree diffs
	// without teaching the workflow package about those artifacts. An error is
	// recorded as a job event but never suppresses cleanup; the hook owns any
	// durable failure marker.
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
	// cwd scan exposed by WorktreeHasLiveProcess; tests inject a fake. On a healthy
	// terminal the worker has already exited, so the probe reports false and cleanup
	// proceeds unchanged.
	WorktreeHasLiveProcess func(path string) bool
	// WorktreeLiveness is the proof-bearing form used by terminal task reclaim.
	// known=false is a hard keep decision because an unreadable process table or
	// cwd link cannot prove that unlinking is safe. WorktreeHasLiveProcess remains
	// the compatibility seam for existing cleanup tests and callers.
	WorktreeLiveness func(path string) (live bool, known bool)
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
	// supersedeAdvance pins an AdvanceJob pass to ONE child lifecycle (#1673). It is
	// set only by the closed-PR supersession recovery, on a COPY of the engine, so
	// every other caller keeps the nil default and the delegation path is unchanged.
	// While it is set, each parent-effect class in advanceDelegations re-asserts the
	// anchor before running, so a retry that re-queued the child cannot fail,
	// enqueue or continue the parent from a run that no longer exists.
	supersedeAdvance *supersedeAdvanceAnchor
	// resolutionSink, when set, makes this engine copy CAPTURE its durable database
	// effects instead of writing them: job inserts, job events and the task-state
	// transition are appended here so a resolution can commit ALL of them - plus its
	// receipt - in ONE lease-guarded transaction (#1673).
	//
	// It is nil on every ordinary path, so those paths are byte-identical. It does NOT
	// capture pre-effects: allocating a delegation worktree and taking a branch lock are
	// git/lock operations that cannot live in a transaction, so they run under the held
	// fence and are recorded on the round for later release.
	resolutionSink *resolutionEffectSink
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
	// NativeReviewFanoutEnabled resolves whether native PR events may schedule
	// reviews for a repository. The daemon always wires it from [review] config,
	// whose default is false. Nil preserves the legacy enabled behavior for direct
	// Engine constructions that do not participate in host configuration.
	NativeReviewFanoutEnabled func(repo string) bool
	// ReviewBlockingSeverity resolves the least severe review finding that may
	// restart the fix loop for a repository. Nil preserves block-all behavior.
	ReviewBlockingSeverity func(repo string) string
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
	// LedgerPathExists resolves whether a repo-relative path exists at a head, so
	// the #1822 ledger can tell a STATIC answer whose cited file has been deleted
	// from one that still stands. It is the SAME resolver the merge gate uses; the
	// review brief and the gate must not compute different obligation sets
	// (#1850 round 2 F1). Nil skips the existence half and records a degradation.
	LedgerPathExists func(ctx context.Context, head string, path string) (bool, error)
	// ReviewChangedFiles resolves the repository-relative files changed from the
	// exact head a reviewer last saw to the current PR head. A scoped follow-up
	// never degrades SILENTLY: when this seam is unavailable or cannot scope the
	// range, HandlePullRequestOpened records a review_scope_unavailable task event
	// and re-reviews the full PR at that head, which re-anchors the prior head so
	// the next round is scoped again.
	ReviewChangedFiles func(ctx context.Context, repo string, pullRequest int, previousHead string, currentHead string) ([]string, error)
}

func (e Engine) block(ctx context.Context, ref taskRef, reason string) error {
	return e.blockTask(ctx, ref, "workflow_blocked", reason, "workflow")
}

// blockTaskPreWriteHook fires between blockTask's state pre-read and its guarded
// write, and between the dirty-worktree block's read and write. It is the seam a
// concurrent merge occupies in production; nil outside tests.
var blockTaskPreWriteHook func(ctx context.Context, taskID string)

// taskStatePreWriteHook fires between setTaskStateResolved's resolution and its
// write, which is the ONLY window in which a test can prove the write itself is
// ownership-bound: an interleaving placed at the barrier is refused by the barrier's
// renewal and never reaches this write at all. Nil in production.
var taskStatePreWriteHook func(ctx context.Context, taskID string)

// advanceOwnershipBinding returns the live-ownership predicate this engine's writes
// must satisfy, or nil for every ordinary caller (#1673). A supersession recovery's
// advance runs on a COPY of the engine carrying its lease, so any irreversible
// parent effect reached from that copy can bind to it at COMMIT rather than trusting
// the barrier that decided it: the pass can stall in between, its lease can lapse,
// and a retry can legally re-queue the child at the next generation.
func (e Engine) advanceOwnershipBinding() *db.AdvanceOwnership {
	if e.supersedeAdvance == nil || strings.TrimSpace(e.supersedeAdvance.LockKey) == "" {
		return nil
	}
	return &db.AdvanceOwnership{
		LockKey:      e.supersedeAdvance.LockKey,
		OwnerToken:   e.supersedeAdvance.Token,
		OwnerJobID:   e.supersedeAdvance.JobID,
		AtGeneration: e.supersedeAdvance.Generation,
	}
}

// blockTask is the single task-blocking choke point. Every durable block
// atomically writes the state and the event that owns it. A stale state
// observation or failed attribution write commits neither half.
func (e Engine) blockTask(ctx context.Context, ref taskRef, kind string, reason string, label string) error {
	task, err := e.resolveTaskState(ctx, ref, TaskBlocked)
	if err != nil {
		return err
	}
	blockErr := BlockedError{Reason: reason}
	if strings.TrimSpace(task.ID) == "" {
		return blockErr
	}
	fromState := ""
	current, err := e.Store.GetTask(ctx, task.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		fromState = current.State
	}
	// blockTaskPreWriteHook is the only place a test can land a merge in the exact
	// post-read/pre-write seam this classification exists for. Nil in production.
	if blockTaskPreWriteHook != nil {
		blockTaskPreWriteHook(ctx, task.ID)
	}
	// CAPTURING: a resolution's block is the ALTERNATIVE OUTCOME of its one
	// transaction, not a separate write. Landing it here would put a durable task
	// mutation outside the fence - so a pass that lost or outlived its lease could
	// still block the task, and every recovery would repeat the block. Capture it and
	// let CommitResolutionEffects apply it under the fence, with no receipt (#1673).
	if e.capturing() {
		task.State = string(TaskBlocked)
		forbidden, _ := taskStateWriteExclusions(TaskBlocked)
		e.resolutionSink.task = task
		e.resolutionSink.taskForbidden = forbidden
		e.resolutionSink.taskSet = true
		// The block's task_event travels WITH the transition rather than as a second
		// write. It does NOT by itself mean the decision was refused: continuation
		// synthesis legitimately blocks a parent task while still enqueuing work, so
		// only the allocation-refusal site sets resolutionSink.blocked.
		e.resolutionSink.taskEvent = db.TaskEvent{Kind: kind, FromState: fromState, Reason: reason}
		// IDEMPOTENT BY STATE. A refused resolution keeps its claim and withholds its
		// receipt, so the recovery sweep re-drives it until the attempt bound parks it -
		// and an event appended on every pass would turn one refusal into an unbounded
		// audit trail. The event records a TRANSITION, so it is written only when the
		// task is not already blocked; a genuine re-block after the task left the
		// blocked state still records, because fromState differs then (#1673).
		e.resolutionSink.taskEventValid = fromState != string(TaskBlocked)
		return blockErr
	}
	blockEvent := db.TaskEvent{
		Kind:      kind,
		FromState: fromState,
		Reason:    reason,
	}
	var blocked bool
	if own := e.advanceOwnershipBinding(); own != nil {
		blocked, err = e.Store.BlockTaskWithEventIfAdvanceOwned(ctx, task, blockEvent, *own, time.Now().UTC())
	} else {
		blocked, err = e.Store.BlockTaskWithEvent(ctx, task, blockEvent)
	}
	if errors.Is(err, db.ErrAdvanceOwnershipLost) {
		// Not a fault and not a merged-work refusal: this pass no longer owns the
		// advance, so the effect was correctly refused. Report it as the rolled-back
		// class the recovery already understands — the debt stays outstanding and the
		// next poll re-drives it against whatever run owns the row then.
		return supersedeAdvanceRolledBackError{
			JobID:      e.supersedeAdvance.JobID,
			Generation: e.supersedeAdvance.Generation,
			Barrier:    "block-commit",
		}
	}
	if err != nil {
		// #1673: a task whose pull request already MERGED must keep that record, and a
		// dead leg's block_parent must still RELEASE its coordinator. The store's
		// atomic guard refuses the write — which is right — but turning that refusal
		// into a hard error would fail the whole poll and strand the coordinator the
		// sweep exists to free.
		//
		// The classification reads the WINNING state, not the pre-read: the pre-read is
		// exactly the value the guard proved stale, so a merge that lands in the
		// post-read/pre-write seam would be classified from a state that no longer
		// exists and hard-fail the poll. Genuinely incompatible non-merged winners
		// still hard-error.
		if errors.Is(err, db.ErrTaskStateConflict) {
			winner := fromState
			if latest, getErr := e.Store.GetTask(ctx, task.ID); getErr == nil {
				winner = latest.State
			} else if !errors.Is(getErr, sql.ErrNoRows) {
				return fmt.Errorf("record %s block for task %s: %w", label, task.ID, err)
			}
			if winner == string(TaskMerged) {
				_ = e.Store.AddTaskEvent(ctx, db.TaskEvent{
					TaskID: task.ID,
					Kind:   TaskEventMergedRegressionRefused,
					Reason: fmt.Sprintf("refused %s -> %s: the pull request already merged, so the landed-work record is kept; the %s advance that requested it continues",
						TaskMerged, TaskBlocked, label),
				})
				return blockErr
			}
		}
		return fmt.Errorf("record %s block for task %s: %w", label, task.ID, err)
	}
	if !blocked {
		return fmt.Errorf("record %s block for task %s: state transition did not commit", label, task.ID)
	}
	return blockErr
}

func (e Engine) setTaskState(ctx context.Context, ref taskRef, state TaskState) error {
	_, err := e.setTaskStateResolved(ctx, ref, state)
	return err
}

// setTaskStateResolved returns the task row actually advanced. A branch ref can
// resolve to an existing canonical task.
func (e Engine) setTaskStateResolved(ctx context.Context, ref taskRef, state TaskState) (string, error) {
	task, err := e.resolveTaskState(ctx, ref, state)
	if err != nil || strings.TrimSpace(task.ID) == "" {
		return "", err
	}
	// persistTaskStateOwned, not a plain upsert: `planned` and `awaiting_human` are
	// merged-regression targets too (blocked now goes through the store's atomic
	// BlockTaskWithEvent), and every target must refuse to overwrite a disposed row.
	// Both exclusions live on the write, so a second daemon cannot win the race a
	// pre-read would lose.
	// An anchored advance binds this write to its live lease as well, so an
	// escalate_human pause (or any other state move it reaches) cannot land after the
	// lease lapsed and a retry re-queued the child (#1673).
	if taskStatePreWriteHook != nil {
		taskStatePreWriteHook(ctx, task.ID)
	}
	// CAPTURING: the task-state transition and its event belong in the resolution's one
	// transaction, alongside the receipt, so a crash cannot leave the write without it.
	if e.capturing() {
		task.State = string(state)
		forbidden, _ := taskStateWriteExclusions(state)
		e.resolutionSink.task = task
		e.resolutionSink.taskForbidden = forbidden
		e.resolutionSink.taskSet = true
		return task.ID, nil
	}
	if _, err := persistTaskStateOwned(ctx, e.Store, task, state, e.advanceOwnershipBinding()); err != nil {
		if errors.Is(err, db.ErrAdvanceOwnershipLost) {
			// Same classification as the block path: a refused effect, not a fault.
			return "", supersedeAdvanceRolledBackError{
				JobID:      e.supersedeAdvance.JobID,
				Generation: e.supersedeAdvance.Generation,
				Barrier:    "task-state-commit",
			}
		}
		return "", err
	}
	return task.ID, nil
}

// resolveTaskState preserves the branch-canonical identity rule without writing,
// allowing all blocked transitions to use Store.BlockTaskWithEvent.
func (e Engine) resolveTaskState(ctx context.Context, ref taskRef, state TaskState) (db.Task, error) {
	if strings.TrimSpace(ref.ID) == "" {
		return db.Task{}, nil
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
		return db.Task{}, err
	}
	if err == nil {
		if IsDisposedTaskState(existing.State) && existing.State != string(state) {
			return db.Task{}, fmt.Errorf("task %s is %s; workflow advancement cannot move it to %s", existing.ID, existing.State, state)
		}
		// The merged-regression refusal is NOT here: an advisory check on this read
		// loses the race a second daemon can win between the read and the write, so
		// it lives on the write itself (writeTaskState).
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
	// different task, advance the branch's canonical row instead of inserting a
	// duplicate.
	if task.Branch != "" {
		byBranch, berr := e.Store.GetTaskByRepoBranch(ctx, task.RepoFullName, task.Branch)
		if berr != nil && !errors.Is(berr, sql.ErrNoRows) {
			return db.Task{}, berr
		}
		if berr == nil && byBranch.ID != task.ID {
			if IsDisposedTaskState(byBranch.State) && byBranch.State != string(state) {
				return db.Task{}, fmt.Errorf("task %s is %s; workflow advancement cannot move it to %s", byBranch.ID, byBranch.State, state)
			}
			byBranch.State = string(state)
			return byBranch, nil
		}
	}
	return task, nil
}

// persistTaskStateOwned is the guarded write point for automated task-state
// advancement (#1673). Every exclusion it enforces lives on the conflict UPDATE,
// so the advisory pre-reads in resolveTaskState cannot be invalidated between the
// read and the write by another daemon — or by the same daemon earlier in the same
// poll.
//
// Two exclusion families, refused differently because they mean different things:
//
//	DISPOSED (dismissed/superseded/stranded) is an explicit operator or audit
//	disposition. Automation never moves a task out of one, so a refusal is an
//	ERROR naming the state — the same answer resolveTaskState's pre-read gives,
//	now unwinnable by a race. Writing the SAME disposed state is permitted, so a
//	disposal stays idempotent.
//
//	MERGED, for a target that would claim the work is not done
//	(IsMergedWorkRegressionTarget), is refused SILENTLY with a durable
//	TaskEventMergedRegressionRefused trace: the failure policy that requested it
//	still has to release its coordinator, so this must not become an error.
//
// `blocked` reaches the store through BlockTaskWithEvent, which carries its own
// terminal-state conflict check; this covers the remaining regression targets
// (`planned`, `awaiting_human`) and every ordinary advancement.
//
// own carries an optional live-ownership predicate INTO the write's transaction.
// It is nil for every ordinary caller, which keeps that path byte-identical.
func persistTaskStateOwned(ctx context.Context, store *db.Store, task db.Task, state TaskState, own *db.AdvanceOwnership) (bool, error) {
	task.State = string(state)
	forbidden, mergedGuarded := taskStateWriteExclusions(state)
	var (
		written bool
		err     error
	)
	if own != nil {
		written, err = store.UpsertTaskUnlessStatesIfAdvanceOwned(ctx, task, forbidden, *own, time.Now().UTC())
	} else {
		written, err = store.UpsertTaskUnlessStates(ctx, task, forbidden)
	}
	if err != nil {
		return false, err
	}
	if written {
		return true, nil
	}
	return false, classifyRefusedTaskStateWrite(ctx, store, task, state, mergedGuarded)
}

// taskStateWriteExclusions is the shared exclusion set every automated task-state
// write carries: never overwrite a disposed row, and never regress landed work.
func taskStateWriteExclusions(state TaskState) (forbidden []string, mergedGuarded bool) {
	forbidden = make([]string, 0, 4)
	for _, disposed := range []TaskState{TaskDismissed, TaskSuperseded, TaskStranded} {
		if disposed != state {
			forbidden = append(forbidden, string(disposed))
		}
	}
	mergedGuarded = IsMergedWorkRegressionTarget(string(state))
	if mergedGuarded {
		forbidden = append(forbidden, string(TaskMerged))
	}
	return forbidden, mergedGuarded
}

// classifyRefusedTaskStateWrite reads the state that actually WON and answers the
// two families differently: a disposed row is a hard error, landed work is a silent
// refusal with a durable trace. Only the row can say which one refused.
func classifyRefusedTaskStateWrite(ctx context.Context, store *db.Store, task db.Task, state TaskState, mergedGuarded bool) error {
	existing, getErr := store.GetTask(ctx, task.ID)
	if getErr != nil {
		return getErr
	}
	if IsDisposedTaskState(existing.State) && existing.State != string(state) {
		return fmt.Errorf("task %s is %s; automated advancement cannot move it to %s", task.ID, existing.State, state)
	}
	if mergedGuarded {
		_ = store.AddTaskEvent(ctx, db.TaskEvent{
			TaskID: task.ID,
			Kind:   TaskEventMergedRegressionRefused,
			Reason: fmt.Sprintf("refused %s -> %s: the pull request already merged, so the landed-work record is kept; the operation that requested it continues",
				TaskMerged, state),
		})
	}
	return nil
}

// openHumanRound is the ONE durable operation that opens a human round: the
// coordinator's exclusive unsettled-round SLOT, the awaiting_human transition and the
// requested event commit together, and under an anchored advance all of it is bound
// to live ownership (#1673).
//
// It returns the round's durable identity plus announce=true ONLY for the caller that
// took the slot and actually paused the task. Every announcement — notifier, event
// sink, chat link — MUST be gated on that value:
//
//	a LOSER would otherwise double-notify a human about one round;
//	a REFUSED transition would otherwise announce a pause that never happened.
func (e Engine) openHumanRound(ctx context.Context, ref taskRef, jobID string, kind string, record EscalationRecord, message func(EscalationRecord) string) (roundID string, announce bool, err error) {
	task, err := e.resolveTaskState(ctx, ref, TaskAwaitingHuman)
	if err != nil {
		return "", false, err
	}
	_, mergedGuarded := taskStateWriteExclusions(TaskAwaitingHuman)
	roundID = newEscalationRoundID()
	record.RoundID = roundID
	round := db.HumanRoundOpen{
		JobID:   jobID,
		RoundID: roundID,
		Kind:    kind,
		Event: db.JobEvent{
			JobID:   jobID,
			Kind:    escalationRequestedEvent,
			Message: message(record),
		},
	}
	if strings.TrimSpace(task.ID) != "" {
		task.State = string(TaskAwaitingHuman)
		round.Task = task
		round.ForbiddenStates, _ = taskStateWriteExclusions(TaskAwaitingHuman)
		if taskStatePreWriteHook != nil {
			taskStatePreWriteHook(ctx, task.ID)
		}
	}
	var outcome db.EscalationRoundOutcome
	if own := e.advanceOwnershipBinding(); own != nil {
		outcome, err = e.Store.OpenHumanRoundIfAdvanceOwned(ctx, round, *own, time.Now().UTC())
	} else {
		outcome, err = e.Store.OpenHumanRound(ctx, round, time.Now().UTC())
	}
	if err != nil {
		return "", false, e.classifyAdvanceOwnershipLoss(err, "human-round-commit")
	}
	switch outcome {
	case db.EscalationRoundOpened:
		return roundID, true, nil
	case db.EscalationRoundRefused:
		// THIS caller took the slot and its guarded pause was refused, so it owes a
		// classification of the winning row. A LOSER never reaches here: it wrote
		// nothing, so classifying it would invent a false landed-work refusal.
		return "", false, classifyRefusedTaskStateWrite(ctx, e.Store, task, TaskAwaitingHuman, mergedGuarded)
	default:
		// The coordinator already has an unsettled round: idempotent and silent.
		return "", false, nil
	}
}

// classifyAdvanceOwnershipLoss turns a refused ownership-bound write into the
// rolled-back class the recovery already understands: the debt stays outstanding and
// the next poll re-drives it. Any other error passes through untouched.
func (e Engine) classifyAdvanceOwnershipLoss(err error, barrier string) error {
	if err == nil || !errors.Is(err, db.ErrAdvanceOwnershipLost) || e.supersedeAdvance == nil {
		return err
	}
	return supersedeAdvanceRolledBackError{
		JobID:      e.supersedeAdvance.JobID,
		Generation: e.supersedeAdvance.Generation,
		Barrier:    barrier,
	}
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
