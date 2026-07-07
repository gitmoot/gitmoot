package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

type Engine struct {
	Store                   *db.Store
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
}

// DelegationTimeoutDefaults are optional fallback timeouts for child delegation
// jobs. They are intentionally generic orchestration policy, not tied to any
// particular external coordinator or agent naming convention.
type DelegationTimeoutDefaults struct {
	Default   string
	Plan      string
	Implement string
	Review    string
	Gate      string
	Repair    string
}

func (d DelegationTimeoutDefaults) timeoutFor(del Delegation) string {
	switch strings.ToLower(strings.TrimSpace(del.Phase)) {
	case "plan":
		if d.Plan != "" {
			return d.Plan
		}
	case "implement":
		if d.Implement != "" {
			return d.Implement
		}
	case "review", "review-prep", "review-dispatch":
		if d.Review != "" {
			return d.Review
		}
	case "gate":
		if d.Gate != "" {
			return d.Gate
		}
	case "repair", "continue":
		if d.Repair != "" {
			return d.Repair
		}
	}
	switch strings.ToLower(strings.TrimSpace(del.Action)) {
	case "implement":
		if d.Implement != "" {
			return d.Implement
		}
	case "review":
		if d.Review != "" {
			return d.Review
		}
	}
	return d.Default
}

// defaultMaxDelegationNonProgressStreak is the streak threshold used when the
// engine's MaxDelegationNonProgressStreak is unset (<= 0): two consecutive
// non-progress generations trip the result-aware loop detector (#339).
const defaultMaxDelegationNonProgressStreak = 2

// defaultMaxVerifyReplanAttempts is the verify→replan attempt cap used when the
// engine's MaxVerifyReplanAttempts is unset (<= 0): the engine issues at most two
// bounded corrective replan continuations on a failed verify verdict before
// routing to the #305 graceful finalize continuation (#439).
const defaultMaxVerifyReplanAttempts = 2

// defaultReviewLegTimeout bounds the detached cross-family review leg (#469) when
// the engine's ReviewLegTimeout is unset (<= 0). It is a generous ceiling — a live
// cross-family LLM review can legitimately take a few minutes — whose only job is
// to reap a wedged reviewer so the detached goroutine cannot leak forever.
const defaultReviewLegTimeout = 10 * time.Minute

// defaultCheckerLegTimeout bounds the detached deterministic-checker leg (#485)
// when the engine's CheckerLegTimeout is unset (<= 0). It is a generous ceiling —
// running dupl/jscpd/golangci-lint/gocyclo over a working tree can legitimately
// take a few minutes — whose only job is to reap a wedged tool so the detached
// goroutine cannot leak forever.
const defaultCheckerLegTimeout = 10 * time.Minute

// defaultHardVerifierLegTimeout bounds the detached hard-verifier leg (#474) when
// the engine's HardVerifierLegTimeout is unset (<= 0). It is a generous ceiling —
// a real build + full test suite in a fresh checkout can legitimately take many
// minutes — whose only job is to reap a wedged verifier so the detached goroutine
// cannot leak forever. A verifier that runs past this is treated as a FAIL (the
// context-cancelled command returns a non-zero exit), never a hang.
const defaultHardVerifierLegTimeout = 15 * time.Minute

// nonProgressStreakThreshold returns the configured result-aware non-progress
// streak threshold, falling back to defaultMaxDelegationNonProgressStreak when
// unset so a zero-valued Engine keeps the documented default.
func (e Engine) nonProgressStreakThreshold() int {
	if e.MaxDelegationNonProgressStreak > 0 {
		return e.MaxDelegationNonProgressStreak
	}
	return defaultMaxDelegationNonProgressStreak
}

// verifyReplanAttemptCap returns the configured verify→replan attempt cap,
// falling back to defaultMaxVerifyReplanAttempts when unset so a zero-valued
// Engine keeps the documented default (#439).
func (e Engine) verifyReplanAttemptCap() int {
	if e.MaxVerifyReplanAttempts > 0 {
		return e.MaxVerifyReplanAttempts
	}
	return defaultMaxVerifyReplanAttempts
}

// now returns the engine's current time, defaulting to time.Now when Now is unset.
func (e Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// mailbox builds the engine's Mailbox with the best-effort terminal-event hook
// wired to e.EventSink (#446). When EventSink is nil (the default) the hook is
// nil too, so finishWithPayload neither constructs nor emits an event and the
// path is byte-identical. The hook maps the terminal JobState to the event_type,
// resolves root_id from the payload, and ships a redacted event fire-and-forget.
func (e Engine) mailbox() Mailbox {
	mb := Mailbox{Store: e.Store, CanaryEnabled: e.CanaryEnabled, deferBlocker: e.BlockerDeferrer, RuntimeDefaultModel: e.RuntimeDefaultModel, routerContextEnabled: e.RouterContextEnabled, resultCheckMode: normalizeResultCheckMode(e.ResultCheckMode)}
	// Wire the off-by-default memory hooks (#626). When e.Memory is nil (every
	// non-enrolled path) both hooks stay nil, so Run's prompt assembly and terminal
	// path are byte-identical. The hooks themselves also no-op when the executor
	// agent is not enrolled, so a controller shared across mixed agents is safe.
	if e.Memory != nil {
		mb.injectMemory = e.Memory.injectBlock
		mb.recordMemory = e.Memory.record
	}
	if e.EventSink == nil {
		return mb
	}
	mb.emitTerminal = func(ctx context.Context, jobID string, state JobState, payload JobPayload) {
		eventType, ok := terminalEventType(state)
		if !ok {
			return
		}
		rootID := strings.TrimSpace(payload.RootJobID)
		if rootID == "" {
			rootID = jobID
		}
		detail := ""
		if payload.Result != nil {
			detail = payload.Result.Summary
		}
		events.EmitEvent(ctx, e.EventSink, events.NewEvent(
			eventType,
			jobID,
			rootID,
			payload.Repo,
			string(state),
			detail,
			e.now(),
			RedactCommentText,
		))
	}
	return mb
}

// terminalEventType maps a terminal JobState to the outbound event_type (#446).
// Only the terminal set {succeeded,failed,blocked} maps; any other state returns
// ok=false so no event is emitted for it.
func terminalEventType(state JobState) (events.EventType, bool) {
	switch state {
	case JobSucceeded:
		return events.EventJobFinished, true
	case JobFailed:
		return events.EventJobFailed, true
	case JobBlocked:
		return events.EventJobBlocked, true
	default:
		return "", false
	}
}

type PullRequestEvent struct {
	Repo              string
	Branch            string
	PullRequest       int
	HeadSHA           string
	GoalID            string
	TaskID            string
	TaskTitle         string
	LeadAgent         string
	Sender            string
	RequiredReviewers []string
	// SkipReviewFanout, when true, suppresses Gitmoot's native PR advancement in
	// HandlePullRequestOpened: zero review jobs are enqueued, the PR baseline is
	// recorded, and the native merge gate is not run. Council-style external
	// orchestrators use this to make their own gate the only merge authority.
	// Defaults false => full native fanout/advancement.
	SkipReviewFanout bool
}

// RevertEvent locates the ORIGINAL PR that a (now-merged) revert PR undid (#467).
// The daemon PR-watcher fills it after parsing a GitHub Revert-button body
// (`Reverts owner/repo#NN`) back to the original PR number; HandlePullRequestReverted
// resolves the original implement job from these fields and fires the corrective
// OutcomeReverted harvest. It carries ONLY what the Reverted projection needs
// (Repo + the original PR number); OriginalBranch/OriginalTaskID are optional
// attribution hints that improve implementJobForTask's sameTask match.
type RevertEvent struct {
	// Repo is the "owner/name" the original PR lives in (same-repo as the revert).
	Repo string
	// OriginalPullRequest is the number of the PR that was reverted — the anchor for
	// the harvester's per-PR item_id / source_url UNIQUE corrective-overwrite key.
	OriginalPullRequest int
	// OriginalBranch is the original PR's head branch, if known, used as an
	// attribution hint for implementJobForTask. Optional.
	OriginalBranch string
	// OriginalTaskID is the original PR's task id, if known, used as a strong
	// attribution hint for implementJobForTask (sameTask prefers TaskID). Optional.
	OriginalTaskID string
}

type MergeRequest struct {
	Repo           string
	Branch         string
	PullRequest    int
	HeadSHA        string
	TaskID         string
	Reviewer       string
	ReviewOptional bool
}

type MergeDecision struct {
	Ready          bool
	Merged         bool
	MergeCommitSHA string
	Reason         string
	// BlockClass classifies a not-ready block (Ready=false) so the Mode-A
	// trace-harvester (#465) only scores AUTHORITATIVE template-quality rejections as
	// a negative and skips transient/infra blocks (branch staleness, dirty local
	// worktree, missing-SHA/base, freshness-unknown). It is the zero value
	// (MergeBlockNone) for a ready/merged decision and is purely advisory — it never
	// changes the block/merge transition itself, so behavior is byte-identical when
	// the harvester is off.
	BlockClass MergeBlockClass
}

// MergeBlockClass classifies a merge-gate block (#465 INFRA-NOISE-FILTERED).
type MergeBlockClass int

const (
	// MergeBlockNone is the zero value (a ready/merged decision, or a block whose
	// class was not set). The harvester treats an unclassified block conservatively
	// as transient (no negative) so a missed classification under-rewards rather than
	// pollutes the corpus with a false negative.
	MergeBlockNone MergeBlockClass = iota
	// MergeBlockQuality is an authoritative template-quality rejection — external CI
	// failed, the latest review captured a blocking result, or the PR was closed
	// without merging. These are the only blocks the harvester scores as a negative.
	MergeBlockQuality
	// MergeBlockTransient is an operational/branch-staleness/infra condition (not
	// mergeable; rebase, dirty local worktree, missing head/base SHA, branch update
	// conflict, freshness unknown) that says nothing about template quality. The
	// harvester skips it so branch-staleness and daemon-machine state are not
	// mis-attributed to the template.
	MergeBlockTransient
)

type MergeGate interface {
	Evaluate(ctx context.Context, request MergeRequest) (MergeDecision, error)
}

type ImplementationFinalizer interface {
	FinalizeImplementation(ctx context.Context, job db.Job, payload JobPayload) (JobPayload, error)
}

// EscalationRequest carries the context the EscalationNotifier needs to notify a
// human that a delegation tree has paused awaiting their decision (#340).
type EscalationRequest struct {
	// CoordinatorJobID is the paused coordinator job; the human resumes the tree
	// with `/gitmoot resume <CoordinatorJobID> retry|continue|abort`.
	CoordinatorJobID string
	// DelegationID is the failing leg that triggered the escalation.
	DelegationID string
	// ChildJobID is the failed child job id for that leg (best-effort; may be
	// empty if the child could not be resolved).
	ChildJobID string
	// Reason is the child's failure summary (why the leg failed).
	Reason string
	// Question is the human-facing escalation question (the delegation's prompt),
	// so the notification can quote what is being asked of the human.
	Question string
	// Ask is true when this is a non-failure ask-gate pause (#445) rather than a
	// failure escalation, so the notifier renders the ask wording + the `answer`
	// resume verb instead of the retry/continue/abort verbs.
	Ask bool
	// Questions carries the ask-gate's human_questions[] (#445) so the notifier can
	// render each id + prompt (+ choices) and tell the human exactly what to answer.
	// Empty for a failure escalation.
	Questions []HumanQuestion
	// Repo/PullRequest/Branch/TaskID/TaskTitle locate the tree's PR or issue so
	// the notifier can post the @-tag comment in the right place.
	Repo        string
	PullRequest int
	Branch      string
	TaskID      string
	TaskTitle   string
}

// EscalationNotifier is the injected seam the engine calls (best-effort) when a
// tree pauses awaiting a human (#340). The daemon implements it to post a GitHub
// comment that @-tags the human with the resume instructions.
type EscalationNotifier interface {
	NotifyEscalation(ctx context.Context, request EscalationRequest) error
}

// OutcomeKind enumerates the verifiable terminal/outcome transitions the engine
// reports to the OutcomeHarvester (#465, Mode A). Only GENUINE outcome
// transitions are reported; operational job_events (runtime_lock_wait,
// repair_retry, comment_post_failed, advance_retry, …) are structurally excluded
// because the harvester is hooked at the outcome seams (runMergeGate result,
// dispatchFix), not the job_events firehose.
type OutcomeKind string

const (
	// OutcomeMerged is reported after a PR merges through the merge gate (a
	// positive). The harvester applies the no-CI guard at the PR HEAD SHA (where the
	// gate posted statuses/checks) before scoring it as a strong positive.
	OutcomeMerged OutcomeKind = "merged"
	// OutcomeBlocked is reported when the merge gate rejects the action
	// (decision not ready) — an authoritative gate-fail negative.
	OutcomeBlocked OutcomeKind = "blocked"
	// OutcomeChangesRequested is reported on a review changes_requested decision
	// that dispatches a fix round — a graded negative whose severity grows with the
	// fix-round count.
	OutcomeChangesRequested OutcomeKind = "changes_requested"
	// OutcomeReverted is reported when a previously-merged PR's work is later
	// reverted — a delayed, corrective negative that overwrites the prior positive
	// in place on the same UNIQUE feedback key.
	//
	// WIRED (#467): the daemon PR-watcher detects a GitHub Revert-button PR (whose
	// body is `Reverts owner/repo#NN` / `Reverts #NN`) being merged, parses the
	// ORIGINAL PR number, and — gated off-by-default on
	// [skillopt].auto_trace_enabled AND revert_detection_enabled — calls
	// HandlePullRequestReverted, which resolves the original implement job via
	// implementJobForTask and fires harvestOutcome with this kind. The harvester's
	// in-place corrective upsert re-writes the SAME per-PR item_id row, flipping the
	// prior positive choice a->b (row count unchanged). It is best-effort: a
	// malformed/cross-repo/unresolvable revert never blocks the poll. See
	// CORRECTIVE-ON-REVERT in docs/skillopt-exchange-contract.md.
	OutcomeReverted OutcomeKind = "reverted"
	// OutcomeReviewed is the SOFT, down-weighted, judge-tagged secondary signal a
	// cross-family review leg produces (#469). Unlike the verifiable kinds above it
	// is NOT derived from a gate transition: it is the rubric a different-runtime
	// reviewer scored on subjective quality + scope-fidelity, projected into a
	// SECOND FeedbackEvent in the SAME auto-trace run under a distinct item id and a
	// gitmoot-review[-self]:<rt> reviewer so it never overwrites the verifiable
	// floor. It is best-effort and off-by-default: only constructed when
	// [skillopt].cross_family_review_enabled (which also requires auto_trace_enabled)
	// is on, and a review-leg failure NEVER blocks or fails a job.
	OutcomeReviewed OutcomeKind = "reviewed"
)

// Outcome carries the verifiable signal the engine derives at an outcome seam and
// hands to the OutcomeHarvester (#465). The engine fills only the fields it
// already knows from the transition (kind, merge/review context, the merged head
// SHA, the PR URL); the harvester owns the no-CI guard read and the projection
// into a {score, feedback} FeedbackEvent. It is a pure value with no behavior so
// the engine stays free of any skillopt/db-write coupling.
type Outcome struct {
	// Kind is the verifiable transition that fired.
	Kind OutcomeKind
	// Repo is the "owner/name" the PR lives in.
	Repo string
	// PullRequest is the PR number, used (with Repo) to build the deterministic
	// per-PR feedback item_id / source_url UNIQUE key.
	PullRequest int
	// HeadSHA is the PR HEAD SHA (for OutcomeMerged) the harvester reads the combined
	// status / check-runs at for the no-CI guard — the SHA the merge gate evaluated
	// and posted statuses/checks at, NOT the merge commit (GitHub does not copy
	// statuses/checks onto the merge commit). It falls back to the merge commit SHA
	// only when the payload head SHA is empty.
	HeadSHA string
	// Reason is the merge-gate rejection reason for OutcomeBlocked (free text from
	// merge_gates.Reason), surfaced verbatim in the negative feedback reasoning.
	Reason string
	// FixRounds is the review-round number at a changes_requested decision (round 1
	// is the first), used to grade the negative: more rounds => worse score.
	FixRounds int

	// The fields below are populated ONLY for Kind == OutcomeReviewed (the
	// cross-family review-agent soft signal, #469); they are zero for every
	// verifiable kind so an OutcomeMerged/Blocked/ChangesRequested/Reverted is
	// byte-identical to before.

	// Reviewer is the reviewer runtime family that produced the rubric (codex /
	// claude / kimi). The harvester writes the review FeedbackEvent under a FIXED
	// gitmoot-review reviewer sentinel (distinct from the verifiable-floor reviewer)
	// and carries this family — plus the SelfFamily flag — in the review item
	// metadata, so a re-review by a different family overwrites the row in place
	// rather than accumulating a stale duplicate.
	Reviewer string
	// SelfFamily is true when no DIFFERENT-family reviewer was available and the
	// review fell back to a SAME-family reviewer (REFINEMENT #1). A self-family row
	// is tagged distinctly and weights BELOW a cross-family review because
	// self-preference bias applies. It is always false for a true cross-family
	// review.
	SelfFamily bool
	// Rubric is the reviewer's subjective-quality + scope-fidelity rubric, each
	// dimension in [0,1] (coverage/containment/fidelity + architecture/readability/
	// abstraction). The harvester maps it to EvaluatorScore.DimensionScores and lets
	// ProjectSignal take the mean (#462 path), so no new aggregation is invented.
	Rubric map[string]float64
	// Findings is the reviewer's free-text reasoning, surfaced verbatim in the soft
	// FeedbackEvent's reasoning.
	Findings string

	// Objective distinguishes an OBJECTIVE, tool-measured deterministic-checker
	// outcome (#485) from the SUBJECTIVE cross-family review outcome (#469) WITHOUT a
	// new OutcomeKind. Both share Kind == OutcomeReviewed and the Rubric/Findings
	// fields (so they reuse the SAME OutcomeReviewed -> Harvest -> ProjectSignal
	// path), but the harvester branches on this flag to write the checker row under a
	// DISTINCT reviewer sentinel (gitmoot-checker) + item-id prefix (checker#) so the
	// objective row coexists with the verifiable floor AND the subjective review row
	// instead of overwriting either. It is zero (false) for every existing
	// merge/block/changes-requested/revert/review path, so those outcomes are
	// byte-identical to before this field existed (ADDITIVE: Outcome is an internal
	// non-wire struct, so this touches no exported contract field or ContractVersion).
	Objective bool

	// HardVerifier distinguishes a deterministic HARD-verifier outcome (#474) from
	// BOTH the OBJECTIVE deterministic-checker outcome (#485, Objective:true) and the
	// SUBJECTIVE cross-family review outcome (#469), again WITHOUT a new OutcomeKind.
	// It shares Kind == OutcomeReviewed but carries a BINARY pass/fail verdict
	// (HardPassed) the harvester maps onto EvaluatorScore.Hard (1.0 pass / 0.0 fail) —
	// an authoritative, un-gameable gate the LLM judge's prose can never move — and
	// writes under a DISTINCT reviewer sentinel (gitmoot-verifier) + item-id prefix
	// (hard#) so it coexists with the verifiable floor, the objective checker, and the
	// subjective review instead of overwriting any of them. It is zero (false) for
	// every existing path, so those outcomes are byte-identical (ADDITIVE: Outcome is
	// an internal non-wire struct).
	HardVerifier bool
	// HardPassed is the deterministic hard-verifier verdict (#474), meaningful ONLY
	// when HardVerifier is true: true when EVERY configured verifier command exited 0
	// in the fresh sandbox (fail-closed set membership), false when ANY command failed
	// or timed out. The harvester projects true → EvaluatorScore.Hard = 1.0 (strong,
	// evidence-backed positive) and false → Hard = 0.0 (authoritative gate-fail
	// negative), so a merge whose code actually fails a clean build/test is caught
	// even when it merged through an empty (no-CI) gate.
	HardPassed bool
}

// OutcomeHarvester is the injected, best-effort, nil-by-default seam (#465, Mode
// A) the engine calls AFTER a verifiable implement-job outcome transition. The
// concrete implementation lives in internal/skillopt and writes a synthetic
// FeedbackEvent (and its eval_run/eval_review_item) into the EXISTING feedback
// tables so a template accrues training signal from verifiable outcomes without
// human ranking. It NEVER promotes — a human still promotes a candidate.
//
// It mirrors EventSink/EscalationNotifier: optional and nil-safe (when nil — the
// default, no [skillopt].auto_trace_enabled — the engine neither constructs an
// Outcome nor calls Harvest, so behavior is byte-identical), and best-effort (a
// Harvest error never blocks or fails a job — the engine swallows it and records
// an auto_trace_harvest_failed job event). The harvester itself decides whether a
// job is in scope (implement-family with a resolvable template version, skipping
// coordinator continuations) and returns nil for out-of-scope jobs.
type OutcomeHarvester interface {
	Harvest(ctx context.Context, job db.Job, payload JobPayload, outcome Outcome) error
}

// harvestOutcome calls the injected OutcomeHarvester best-effort: it is nil-safe
// (no harvester => no-op), and a harvester error is swallowed and recorded as a
// best-effort auto_trace_harvest_failed job event — it is NEVER returned up, so a
// harvest failure can never block or fail a job (mirrors emitDaemonTerminalEvent /
// EscalationNotifier). It must only be called on a GENUINE verifiable outcome
// transition (#465).
func (e Engine) harvestOutcome(ctx context.Context, job db.Job, payload JobPayload, outcome Outcome) {
	if e.OutcomeHarvester == nil {
		return
	}
	if err := e.OutcomeHarvester.Harvest(ctx, job, payload, outcome); err != nil {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "auto_trace_harvest_failed",
			Message: string(outcome.Kind) + ": " + err.Error(),
		})
	}
}

type BlockedError struct {
	Reason string
}

func (e BlockedError) Error() string {
	return "workflow blocked: " + e.Reason
}

// AwaitingHumanError is returned by pauseAwaitingHuman when a delegation fails
// under the escalate_human failure_policy (#340). It is distinct from
// BlockedError so the engine and daemon can tell a durable human-in-the-loop
// pause (resumable via `/gitmoot resume`) apart from a terminal block. Like
// BlockedError it propagates up through AdvanceJob as an AdvanceError, so the
// agent's already-delivered result is preserved while the tree waits.
type AwaitingHumanError struct {
	Reason string
}

func (e AwaitingHumanError) Error() string {
	return "workflow awaiting human: " + e.Reason
}

// AdvanceError wraps an error that occurred while advancing a job *after* the
// agent delivery + job already succeeded terminally. RunJob returns the
// agent's result alongside it, so callers can distinguish a benign
// post-success advance condition (e.g. a merge-gate block on a freshly-opened
// PR, or a 422 "PR already exists" race) from a genuine delivery/run failure
// and surface the persisted terminal-success result instead of discarding it.
type AdvanceError struct {
	Err error
}

func (e AdvanceError) Error() string {
	if e.Err == nil {
		return "workflow advance failed"
	}
	return "workflow advance failed: " + e.Err.Error()
}

func (e AdvanceError) Unwrap() error {
	return e.Err
}

func (e Engine) HandlePullRequestOpened(ctx context.Context, event PullRequestEvent) error {
	if err := e.validate(); err != nil {
		return err
	}
	if err := validatePullRequestEvent(event); err != nil {
		return err
	}
	ref := taskRefFromPullRequest(event)
	if err := e.setTaskState(ctx, ref, TaskPullRequestOpen); err != nil {
		return err
	}
	if err := e.ensureAgentAllowed(ctx, JobRequest{
		Agent:  event.LeadAgent,
		Action: "implement",
		Repo:   event.Repo,
		Branch: event.Branch,
	}, ref); err != nil {
		return err
	}
	reviewers := compactStrings(append([]string{}, event.RequiredReviewers...))
	if len(reviewers) == 0 {
		reviewers = compactStrings(append([]string{}, e.RequiredReviewers...))
	}
	if event.SkipReviewFanout {
		return e.recordPullRequestBaseline(ctx, event)
	}
	if len(reviewers) == 0 {
		decision, err := e.runMergeGate(ctx, "", JobPayload{
			Repo:        event.Repo,
			Branch:      event.Branch,
			PullRequest: event.PullRequest,
			HeadSHA:     event.HeadSHA,
			GoalID:      event.GoalID,
			TaskID:      event.TaskID,
			TaskTitle:   event.TaskTitle,
			LeadAgent:   event.LeadAgent,
		}, ref)
		if err != nil {
			return err
		}
		if decision.Merged {
			return nil
		}
		return e.recordPullRequestBaseline(ctx, event)
	}
	reviewRound, err := e.nextReviewRound(ctx, event)
	if err != nil {
		return err
	}
	requests := make([]JobRequest, 0, len(reviewers))
	for _, reviewer := range reviewers {
		request := JobRequest{
			Agent:       reviewer,
			Action:      "review",
			Repo:        event.Repo,
			Branch:      event.Branch,
			PullRequest: event.PullRequest,
			HeadSHA:     event.HeadSHA,
			GoalID:      event.GoalID,
			TaskID:      event.TaskID,
			TaskTitle:   event.TaskTitle,
			LeadAgent:   event.LeadAgent,
			Reviewers:   reviewers,
			ReviewRound: reviewRound,
			Sender:      event.Sender,
			Instructions: fmt.Sprintf(
				"Review pull request #%d for task %s.",
				event.PullRequest,
				taskLabel(event.TaskID, event.TaskTitle),
			),
		}
		requests = append(requests, request)
	}
	for _, request := range requests {
		if err := e.ensureAgentAllowed(ctx, request, ref); err != nil {
			return err
		}
	}
	for _, request := range requests {
		if err := e.enqueue(ctx, request); err != nil {
			return err
		}
	}
	if err := e.setTaskState(ctx, ref, TaskReviewing); err != nil {
		return err
	}
	return e.recordPullRequestBaseline(ctx, event)
}

func (e Engine) HandlePullRequestReadyToMerge(ctx context.Context, event PullRequestEvent) error {
	if err := e.validate(); err != nil {
		return err
	}
	if err := validatePullRequestEvent(event); err != nil {
		return err
	}
	ref := taskRefFromPullRequest(event)
	_, err := e.runMergeGate(ctx, "", JobPayload{
		Repo:        event.Repo,
		Branch:      event.Branch,
		PullRequest: event.PullRequest,
		HeadSHA:     event.HeadSHA,
		GoalID:      event.GoalID,
		TaskID:      event.TaskID,
		TaskTitle:   event.TaskTitle,
		LeadAgent:   event.LeadAgent,
		Reviewers:   compactStrings(append([]string{}, event.RequiredReviewers...)),
	}, ref)
	return err
}

// HandleReviewPullRequestClosed reconciles a task wedged in `reviewing` whose
// pull request is no longer open on GitHub (#543). The daemon poll loop only
// lists OPEN pull requests, so once a reviewing task's PR is closed — most often
// a duplicate/superseded PR that a cleanup job closed on GitHub — nothing
// re-routes the task and it stays in `reviewing` pointing at a stale `open`
// local PR row forever.
//
// It is idempotent and narrowly scoped: it acts ONLY when the task is still in
// `reviewing`, so any already-advanced task (a genuinely-open PR still under
// review, or one already merged/blocked) is left untouched and the healthy
// merge/open paths are never regressed. It transitions the task out of
// `reviewing` and rewrites the stale local PR row to its true state: a merged PR
// resolves the task to `merged`; a closed-unmerged PR resolves it to the
// terminal `blocked` state (there is no open PR left to review or merge, so it
// surfaces to a human). Existing PR row fields (url/base/merge SHA) are
// preserved — only the state (and, when merged, head SHA) is reconciled.
func (e Engine) HandleReviewPullRequestClosed(ctx context.Context, event PullRequestEvent, merged bool) error {
	if err := e.validate(); err != nil {
		return err
	}
	if err := validatePullRequestEvent(event); err != nil {
		return err
	}
	ref := taskRefFromPullRequest(event)
	task, err := e.Store.GetTask(ctx, ref.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if task.State != string(TaskReviewing) {
		return nil
	}
	prState := "closed"
	taskState := TaskBlocked
	if merged {
		prState = "merged"
		taskState = TaskMerged
	}
	if err := e.setTaskState(ctx, ref, taskState); err != nil {
		return err
	}
	pr := db.PullRequest{
		RepoFullName: event.Repo,
		Number:       int64(event.PullRequest),
		HeadBranch:   event.Branch,
		HeadSHA:      event.HeadSHA,
		State:        prState,
	}
	if existing, err := e.Store.GetPullRequest(ctx, event.Repo, int64(event.PullRequest)); err == nil {
		pr.URL = existing.URL
		pr.BaseBranch = existing.BaseBranch
		pr.MergeCommitSHA = existing.MergeCommitSHA
		if strings.TrimSpace(pr.HeadSHA) == "" {
			pr.HeadSHA = existing.HeadSHA
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := e.Store.UpsertPullRequest(ctx, pr); err != nil {
		return err
	}
	if merged {
		// An externally-merged PR detected by the reconciler must release the branch
		// lock and remove the task worktree, exactly as the canonical merge path
		// (PolicyMergeGate.finishMerged) does. Without this the reconcile method
		// would set TaskMerged but strand the branch lock (held forever) and leak
		// the task worktree on disk — the "strands a lock / leaves a worktree" class
		// that accumulates under unattended automation. The blocked branch
		// deliberately keeps the worktree/lock for human resumption, so only the
		// merged branch cleans up.
		e.reconcileMergedCleanup(ctx, event.Repo, task)
	}
	return nil
}

// reconcileMergedCleanup releases the branch lock and removes the task worktree
// after HandleReviewPullRequestClosed resolves an externally-merged reviewing
// task to `merged` (#543). It mirrors PolicyMergeGate.finishMerged's post-merge
// cleanup so the self-heal reconcile path does not leak a held branch lock or an
// on-disk worktree. Every step is best-effort and nil-safe: failures are
// swallowed so the already-durable terminal `merged` transition is never undone,
// matching finishMerged's treatment of these as non-fatal post-merge warnings.
func (e Engine) reconcileMergedCleanup(ctx context.Context, repo string, task db.Task) {
	if branch := strings.TrimSpace(task.Branch); branch != "" {
		if lock, err := e.Store.GetBranchLock(ctx, repo, branch); err == nil {
			_, _ = e.Store.ReleaseLockWithEvent(ctx, lock, db.BranchLockEvent{
				Kind:    "released",
				Message: "released after pull request merged (reconciled #543)",
			})
		}
	}
	path := strings.TrimSpace(task.WorktreePath)
	if path == "" {
		return
	}
	// Force-remove: the work is already merged, so a leftover dirty/locked worktree
	// (the common reason a non-force removal fails) must not block reclaiming it.
	manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager)
	if !ok {
		return
	}
	if err := manager.RemoveWorktreeForce(ctx, path); err != nil {
		return
	}
	_ = e.Store.ClearTaskWorktreePath(ctx, task.ID)
}

func (e Engine) RunJob(ctx context.Context, jobID string, agent runtime.Agent, adapter DeliveryAdapter) (AgentResult, error) {
	if err := e.validate(); err != nil {
		return AgentResult{}, err
	}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return AgentResult{}, err
	}
	if err := e.ensureJobExecutorAllowed(ctx, job, payload, taskRefFromPayload(payload)); err != nil {
		return AgentResult{}, err
	}
	result, err := e.mailbox().Run(ctx, jobID, agent, adapter)
	if err != nil {
		return result, err
	}
	if e.PayloadRefresher != nil {
		if err := e.refreshJobPayload(ctx, jobID); err != nil {
			return result, err
		}
	}
	if err := e.AdvanceJob(ctx, jobID); err != nil {
		// The agent delivery and the job itself already succeeded; only this
		// post-success advance step errored. Wrap it so callers can recover the
		// persisted terminal-success result (the result is in hand) instead of
		// discarding it. Delivery/run failures above stay raw.
		return result, AdvanceError{Err: err}
	}
	if err := e.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "advance_completed", Message: "workflow advancement completed"}); err != nil {
		return result, err
	}
	return result, nil
}

// FinalizeTimedOutDelegationChild turns a delegation child that was killed by its
// per-delegation timeout (or any runtime failure that left it JobRunning with no
// parseable gitmoot_result) into a terminal FAILED child carrying a synthetic
// failed result, then runs AdvanceJob so the parent's advanceDelegations applies
// the delegation's retry/failure_policy/continuation. Without this a timeout kill
// strands the child in JobRunning forever (Mailbox.Run errored, so RunJob returned
// before AdvanceJob), and only the blind 30m stale-running recovery would re-queue
// it, bypassing delegation.Retry and failure_policy.
//
// It is a no-op (returns false) for a non-delegation job, a job that already left
// JobRunning, or a job that already stored a result, so it is safe to call from
// the daemon's run-error handler and idempotent under concurrent recovery. When
// the synthetic failure blocks the parent task under block_parent, AdvanceJob
// returns a BlockedError, which is propagated like the result-bearing path.
func (e Engine) FinalizeTimedOutDelegationChild(ctx context.Context, jobID string, reason string) (bool, error) {
	if err := e.validate(); err != nil {
		return false, err
	}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(payload.ParentJobID) == "" {
		return false, nil
	}
	// Already has a result -> the normal RunJob/AdvanceJob path handled it; nothing
	// to recover. A deadline can leave the child either still JobRunning (the
	// cancelled context aborted Mailbox.Run's own fail write) or already
	// JobFailed/JobBlocked but WITHOUT a stored result (so the parent's
	// advanceDelegations never ran). Recover both, but never touch a succeeded or
	// cancelled child.
	if payload.Result != nil {
		return false, nil
	}
	switch job.State {
	case string(JobRunning), string(JobFailed), string(JobBlocked):
	default:
		return false, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "delegation child timed out before returning a result"
	}
	payload.Result = &AgentResult{
		Decision: "failed",
		Summary:  reason,
	}
	mailbox := e.mailbox()
	if job.State == string(JobRunning) {
		if err := mailbox.finishWithPayload(ctx, jobID, JobFailed, reason, payload); err != nil {
			return false, err
		}
	} else {
		// Already terminal-failed/blocked: only attach the synthetic result so
		// AdvanceJob (which requires a non-nil Result) can drive the parent DAG.
		if err := mailbox.savePayload(ctx, jobID, payload); err != nil {
			return false, err
		}
	}
	if err := e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    "delegation_timeout_finalized",
		Message: reason,
	}); err != nil {
		return false, err
	}
	if err := e.AdvanceJob(ctx, jobID); err != nil {
		return true, err
	}
	if err := e.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "advance_completed", Message: "workflow advancement completed"}); err != nil {
		return true, err
	}
	return true, nil
}

func (e Engine) refreshJobPayload(ctx context.Context, jobID string) error {
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return err
	}
	payload, err = e.PayloadRefresher(ctx, job, payload)
	if err != nil {
		return err
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		return err
	}
	return e.Store.UpdateJobPayload(ctx, jobID, encoded)
}

func (e Engine) AdvanceJob(ctx context.Context, jobID string) error {
	if err := e.validate(); err != nil {
		return err
	}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return err
	}
	if payload.Result == nil {
		return fmt.Errorf("job %q has no agent result", jobID)
	}
	ref := taskRefFromPayload(payload)

	// A read-only delegation child runs in a throwaway detached worktree; dispose
	// it once the child is terminal. Deferred so it fires on every return path
	// below (the delegation DAG early-returns for policy-handled failures and
	// pending retries). No-op for jobs that did not allocate a read-only worktree.
	defer e.cleanupReadOnlyDelegationWorktree(ctx, jobID, job.Type, payload)

	// An implement delegation child runs in a per-delegation worktree on its own
	// gitmoot-delegation-* branch; tear both down once the child is terminal so they
	// do not accumulate in the shared checkout and mislead a later coordinator (#478).
	// No-op for non-implement / non-delegation jobs and (idempotently) for already
	// cleaned ones. Skips succeeded legs still feeding a pending integration (#332).
	defer e.cleanupImplementDelegationWorktree(ctx, jobID, job.Type, payload)

	// When an integration step (#332) that consumed implement legs via its Deps
	// reaches a terminal state, tear down those consumed legs' worktrees+branches:
	// each leg's own terminal advance preserved its branch while this consumer was
	// still pending/running, and nothing else ever reclaims an integration-fed leg
	// (the merge gate cleans only the task worktree), so they would otherwise
	// accumulate forever (#478). No-op for a job with no parent/deps.
	defer e.cleanupConsumedImplementLegWorktrees(ctx, payload)

	// Commit a succeeded implement leg's work to its own branch BEFORE advancing
	// the parent's delegation DAG. The parent advance below may enqueue a dependent
	// that integrates this leg (#332); if the commit ran later (in the switch), the
	// leg that *triggered* the integration would not yet be on its branch and its
	// work would be missing from the merge. The task/PR finalizer path commits its
	// own way, so this only covers PR-less delegation legs; it is a no-op otherwise.
	if job.Type == "implement" && payload.Result.Decision == "implemented" && !e.implementationNeedsFinalizer(ctx, payload) {
		if err := e.commitDelegationLeg(ctx, job, payload); err != nil {
			return err
		}
	}

	// When a delegated child job finishes, advance its parent's delegation DAG
	// before running the child's own advancement: enqueue any now-ready
	// dependent siblings, apply the failed delegation's failure_policy, and
	// enqueue the coordinator continuation job once every top-level sibling is
	// terminal. This runs here (the child's AdvanceJob) because RunJob calls
	// AdvanceJob per finishing child. A child with no parent is unaffected.
	if strings.TrimSpace(payload.ParentJobID) != "" {
		parentJob, parentPayload, err := e.jobPayload(ctx, payload.ParentJobID)
		if err != nil {
			return err
		}
		// Ask-gate for a delegation CHILD (#445): a HEALTHY child that returned
		// human_questions[] pauses the SHARED parent task on the COORDINATOR's round
		// BEFORE advancing the parent DAG. Running it here — ahead of
		// advanceDelegations — is load-bearing: if the asking child is the last/only
		// sibling to finish, advanceDelegations would otherwise enqueue the coordinator
		// continuation (the tree would PROCEED past the question, consuming compute and
		// contradicting the open pause). Routing the pause to the coordinator
		// (pauseAwaitingHumanAnswer keys on payload.ParentJobID) also makes the human's
		// resume target the coordinator — whose answer-driven continuation is the one
		// the tree advances on — and lets sibling asks share the single round. A
		// blocked/failed child never asks (it takes the failure path below).
		if len(payload.Result.HumanQuestions) > 0 &&
			payload.Result.Decision != "blocked" && payload.Result.Decision != "failed" {
			open, perr := e.pauseAwaitingHumanAnswer(ctx, job, payload, ref)
			if perr != nil {
				return perr
			}
			if open {
				return AwaitingHumanError{Reason: fmt.Sprintf("job %q is awaiting a human answer to %d question(s)", payload.ParentJobID, len(payload.Result.HumanQuestions))}
			}
			// CLOSED: the human already answered (the coordinator continuation that
			// carries the answer is in flight) or the ask was TTL-finalized. The
			// answer-driven coordinator continuation is the asking child's sole
			// continuation, so short-circuit — neither the parent DAG advance nor this
			// child's own delegations[] re-dispatch.
			return nil
		}
		if parentPayload.Result != nil {
			if err := e.advanceDelegations(ctx, parentJob, parentPayload, parentPayload.Result, taskRefFromPayload(parentPayload)); err != nil {
				return err
			}
			// A child that failed under a continue/escalate failure_policy, one that
			// was re-enqueued by the retry pass, or one whose parent already has a
			// continuation in flight, is handled by the delegation graph (siblings
			// keep running, the retry runs, or the coordinator continuation absorbs
			// the failure); do not also block the shared parent task via the
			// failed-decision path below.
			if payload.Result.Decision == "blocked" || payload.Result.Decision == "failed" {
				if delegationFailureHandledByPolicy(parentPayload.Result, payload.DelegationID) {
					return nil
				}
				retrying, err := e.delegationRetryPending(ctx, parentJob.ID, payload.DelegationID)
				if err != nil {
					return err
				}
				if retrying {
					return nil
				}
				// Once a continuation has been enqueued (e.g. an earlier escalate
				// fired it), a later block_parent sibling failure must not block the
				// shared parent task: that would contradict the in-flight
				// continuation, which already carries every child outcome.
				parentEvents, err := e.Store.ListJobEvents(ctx, parentJob.ID)
				if err != nil {
					return err
				}
				if continuationEnqueued(parentEvents) {
					return nil
				}
			}
		}
	}

	if job.Type == "review" {
		latest, err := e.latestReviewRound(ctx, payload)
		if err != nil {
			return err
		}
		if latest != "" && strings.TrimSpace(payload.ReviewRound) != latest {
			return nil
		}
	}
	if payload.Result.Decision == "blocked" || payload.Result.Decision == "failed" {
		return e.block(ctx, ref, payload.Result.Summary)
	}
	// Ask-gate (#445): a HEALTHY result that carries human_questions[] pauses the
	// task at awaiting_human for a specific human answer instead of guessing —
	// reusing the escalate_human pause machinery on a SUCCESS result. It runs
	// AFTER the blocked/failed early-return (so a failed result never asks) and
	// BEFORE dispatchDelegations/the continuation, so NO delegations dispatch and
	// NO continuation enqueues while the round is open: zero compute, budget-
	// neutral. pauseAwaitingHumanAnswer returns AwaitingHumanError (an idempotent
	// re-advance within the open round returns it without re-recording), so the
	// switch below is skipped and the agent's already-delivered result is
	// preserved while the tree waits.
	if len(payload.Result.HumanQuestions) > 0 {
		open, err := e.pauseAwaitingHumanAnswer(ctx, job, payload, ref)
		if err != nil {
			return err
		}
		if open {
			// The round is OPEN (freshly opened now, or an idempotent re-advance while
			// awaiting the answer): the tree consumes zero compute until the human
			// resumes with `answer`.
			return AwaitingHumanError{Reason: fmt.Sprintf("job %q is awaiting a human answer to %d question(s)", job.ID, len(payload.Result.HumanQuestions))}
		}
		// The round is CLOSED: the human already answered (resumeAnswerLeg enqueued
		// the coordinator continuation that carries the answer) or the ask was
		// TTL-finalized. Either way the answer-driven continuation is the asking
		// job's sole continuation, so short-circuit here — the asking result's own
		// delegations[] must NOT also dispatch and no second continuation enqueues.
		return nil
	}
	if job.Type == "review" && payload.Result.Decision == "approved" {
		done, err := e.reviewApprovalAlreadyAdvanced(ctx, ref)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	if err := e.dispatchDelegations(ctx, job, payload, ref); err != nil {
		return err
	}

	switch job.Type {
	case "implement":
		if payload.Result.Decision != "implemented" {
			return nil
		}
		finalizerRan := false
		if e.implementationNeedsFinalizer(ctx, payload) {
			finalizerRan = true
			finalized, err := e.ImplementationFinalizer.FinalizeImplementation(ctx, job, payload)
			if err != nil {
				return err
			}
			encoded, err := marshalPayload(finalized)
			if err != nil {
				return err
			}
			if err := e.Store.UpdateJobPayload(ctx, job.ID, encoded); err != nil {
				return err
			}
			payload = finalized
		}
		if payload.PullRequest <= 0 {
			return e.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_skipped_no_pr", Message: "no pull request is attached; skipping PR advancement"})
		}
		leadAgent := strings.TrimSpace(payload.LeadAgent)
		if leadAgent == "" {
			leadAgent = job.Agent
			if payload.DelegationReason == "runtime_session_busy" &&
				payload.DelegatedAgent == job.Agent &&
				strings.TrimSpace(payload.OriginalAgent) != "" {
				leadAgent = payload.OriginalAgent
			}
		}
		event := PullRequestEvent{
			Repo:              payload.Repo,
			Branch:            payload.Branch,
			PullRequest:       payload.PullRequest,
			HeadSHA:           payload.HeadSHA,
			GoalID:            payload.GoalID,
			TaskID:            payload.TaskID,
			TaskTitle:         payload.TaskTitle,
			LeadAgent:         leadAgent,
			Sender:            job.Agent,
			RequiredReviewers: e.requiredReviewers(payload),
			// Trigger 1 (in-process path): carry the implement job's
			// skip_native_review_fanout straight onto the PR event.
			SkipReviewFanout: payload.SkipNativeReviewFanout,
		}
		// Persist the flag onto the branch lock for the daemon's PR-watcher path
		// (trigger 2). When the finalizer ran it already wrote the flag BEFORE
		// opening the PR (closing the #390 TOCTOU), so this write would be
		// redundant; only the non-finalizer path (PR pre-attached, no managed
		// worktree) reaches the PR with the flag unpersisted. Skipping the write on
		// the common default (false) keeps that path free of an extra lock UPDATE —
		// a freshly created lock already defaults to not-skip.
		if payload.SkipNativeReviewFanout && !finalizerRan {
			if err := e.Store.SetBranchLockReviewFanout(ctx, payload.Repo, payload.Branch, true); err != nil {
				return err
			}
		}
		return e.HandlePullRequestOpened(ctx, event)
	case "review":
		// A PR-less review (e.g. a review heartbeat enqueues Action="review" with
		// PullRequest=0/Branch="") has no PR-only machinery to route into: a
		// "changes_requested" decision would call dispatchFix -> leadAgent() ->
		// GetBranchLock(repo, "") -> ErrNoRows ("lead agent is required"), and an
		// "approved" decision would runMergeGate against PR #0. Both error every
		// tick and drop the review outcome. Mirror the implement arm's guard:
		// treat the delivered review (the agent's comments) as terminal, like ask.
		if payload.PullRequest <= 0 {
			return e.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_skipped_no_pr", Message: "no pull request is attached; skipping review advancement"})
		}
		reviewer := reviewDecisionAgent(job, payload)
		switch payload.Result.Decision {
		case "changes_requested":
			if err := e.setTaskState(ctx, ref, TaskChangesRequested); err != nil {
				return err
			}
			if err := e.dispatchFix(ctx, reviewer, payload, *payload.Result, ref); err != nil {
				return err
			}
			// Verifiable graded negative (#465): a review asked for changes, so the
			// implement job's diff did not pass review. Harvested AFTER dispatchFix so a
			// harvest error can never affect the (already-completed) fix dispatch. The
			// fix-round count (the review round number, round 1 = first) grades severity:
			// more rounds => a worse score.
			e.harvestOutcomeForMergeGate(ctx, payload, Outcome{
				Kind:        OutcomeChangesRequested,
				Repo:        payload.Repo,
				PullRequest: payload.PullRequest,
				HeadSHA:     payload.HeadSHA,
				Reason:      strings.TrimSpace(payload.Result.Summary),
				FixRounds:   reviewRoundCount(payload.ReviewRound),
			})
			return nil
		case "approved":
			ready, err := e.allRequiredReviewersApproved(ctx, reviewer, payload)
			if err != nil {
				return err
			}
			if !ready {
				return e.setReviewingIfNotChangesRequested(ctx, ref)
			}
			_, err = e.runMergeGate(ctx, reviewer, payload, ref)
			return err
		}
	}
	return nil
}

// rootJobID returns the id of the coordinator that originated a coordination
// tree. A child or continuation inherits its parent's RootJobID via the payload;
// the originating coordinator has no RootJobID, so its own job id is the root.
// Every job in one tree therefore shares a single root, which lets the loop
// detector and per-root budget reason about the whole tree at once.
func (e Engine) rootJobID(job db.Job, payload JobPayload) string {
	if strings.TrimSpace(payload.RootJobID) != "" {
		return payload.RootJobID
	}
	return job.ID
}

// originalGoal resolves the goal text that a coordinator continuation prompt is
// anchored to (#418). It reads the ROOT coordinator's own Instructions — the
// user's original prompt — via rootJobID, which is depth-stable: a NESTED
// continuation's parentPayload.Instructions holds the parent continuation's built
// prompt (not the user's goal), so resolving from the root keeps every generation
// of the chain anchored to the same intent. The caller passes fallback (the
// immediate parentPayload.Instructions) for two cases: the root was pruned (lookup
// fails) or the root carries no instructions; in both the continuation must never
// error, so it falls back rather than failing. The result is whitespace-trimmed;
// empty/whitespace yields "" so the builders omit the Original goal header.
func (e Engine) originalGoal(ctx context.Context, rootJobID, fallback string) string {
	if root, err := e.Store.GetJob(ctx, rootJobID); err == nil {
		if payload, err := unmarshalPayload(root.Payload); err == nil {
			if goal := strings.TrimSpace(payload.Instructions); goal != "" {
				return goal
			}
		}
	}
	return strings.TrimSpace(fallback)
}

// goalAnchorHeader renders the "Original goal:" section prepended to a coordinator
// continuation prompt (#418). An empty/whitespace goal yields "" so the prompt
// never prints an empty header (the closing instruction degrades to generic
// synthesis framing — see goalSynthesisClosing). The goal is the user's own prompt,
// so it is emitted as plain labeled text without fencing.
func goalAnchorHeader(goal string) string {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return ""
	}
	return "Original goal:\n" + goal + "\n\n"
}

// goalSynthesisClosing returns the goal-anchored synthesis instruction that
// replaces the legacy "decide the next step" framing (#418). When a goal is
// present it asks the coordinator to synthesize an answer to THAT goal; when the
// goal is empty it degrades to generic synthesis framing. Both spell out the
// unchanged termination contract: an empty delegations list finishes, and new
// delegations are only for a genuine remaining gap.
func goalSynthesisClosing(goal string) string {
	if strings.TrimSpace(goal) != "" {
		return "Synthesize these results into an answer to the ORIGINAL GOAL above. " +
			"Reconcile any conflicts between children and flag any gaps. " +
			"Only return new delegations if a genuine gap remains; " +
			"otherwise return an EMPTY delegations list to finish."
	}
	return "Synthesize these results into a single coherent answer. " +
		"Reconcile any conflicts between children and flag any gaps. " +
		"Only return new delegations if a genuine gap remains; " +
		"otherwise return an EMPTY delegations list to finish."
}

// budgetPressureLine returns a one-line nudge to bias the coordinator toward
// synthesizing rather than re-delegating when the coordination tree is near a
// termination bound (#418). depth is the depth the NEXT generation would occupy
// (parentPayload.DelegationDepth+1) and jobs is the current per-root job count.
// It fires only within budgetPressureSlack of a bound; otherwise it returns "" so
// a tree with plenty of headroom prints no line (keeping the prompt clean). A
// non-positive jobs (the count was unavailable) suppresses the job clause.
func budgetPressureLine(depth, jobs int) string {
	maxDepth := effectiveMaxDelegationDepth()
	maxJobs := effectiveMaxDelegationTotalJobs()
	nearDepth := depth >= maxDepth-budgetPressureSlack
	nearJobs := jobs > 0 && jobs >= maxJobs-budgetPressureSlack
	if !nearDepth && !nearJobs {
		return ""
	}
	if jobs > 0 {
		return fmt.Sprintf("You are at depth %d/%d and %d/%d jobs — prefer finishing (synthesize now) over re-delegating.\n\n",
			depth, maxDepth, jobs, maxJobs)
	}
	return fmt.Sprintf("You are at depth %d/%d — prefer finishing (synthesize now) over re-delegating.\n\n",
		depth, maxDepth)
}

// budgetPressureSlack is how close to a termination bound (depth or per-root job
// count) the tree must be before budgetPressureLine injects its nudge (#418).
const budgetPressureSlack = 2

// rootWallClockExceeded reports whether the coordination tree rooted at rootID has
// been running longer than MaxDelegationWallClock, measured from the root job's
// created_at MINUS any time the tree spent paused awaiting a human (#340), and the
// adjusted elapsed duration. Excluding paused time means a tree a human took hours
// to answer is not punished as a runaway: only active wall-clock counts. It fails
// open: any lookup/parse problem returns (false, 0) so a clock or timestamp hiccup
// never blocks dispatch.
func (e Engine) rootWallClockExceeded(ctx context.Context, rootID string) (bool, time.Duration) {
	createdAt, err := e.Store.JobCreatedAt(ctx, rootID)
	if err != nil {
		return false, 0
	}
	start, err := parseJobTimestamp(createdAt)
	if err != nil {
		return false, 0
	}
	now := e.now().UTC()
	elapsed := now.Sub(start) - e.rootPausedDuration(ctx, rootID, now)
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed > MaxDelegationWallClock, elapsed
}

// rootPausedDuration sums the wall-clock time the coordination tree rooted at
// rootID spent paused awaiting a human across every job in the tree (#340). For
// each job that recorded a delegation_escalation_requested event, the pause runs
// from its paused_at until the matching delegation_escalation_resolved event's
// resolved_at (or until now if still paused). It fails open: any lookup/parse
// hiccup contributes 0 to the sum, so the wall-clock backstop never blocks
// dispatch on a bad timestamp. A tree with no escalation contributes 0, keeping
// default behavior byte-identical.
func (e Engine) rootPausedDuration(ctx context.Context, rootID string, now time.Time) time.Duration {
	// ListJobsByRoot (#420) is an indexed lookup on the denormalized root_id
	// column: it returns exactly the tree (the self-rooted coordinator plus every
	// child/continuation) instead of the whole table. The grouping key is
	// identical to the old payload.RootJobID filter, so the sum is byte-identical;
	// it still fails open (a lookup error contributes 0).
	jobs, err := e.Store.ListJobsByRoot(ctx, rootID)
	if err != nil {
		return 0
	}
	var total time.Duration
	for _, job := range jobs {
		total += e.jobPausedDuration(ctx, job.ID, now)
	}
	return total
}

// jobPausedDuration returns how long a single coordinator job spent (or has been)
// paused awaiting a human, derived from its escalation events. A job with no
// escalation contributes 0.
func (e Engine) jobPausedDuration(ctx context.Context, jobID string, now time.Time) time.Duration {
	events, err := e.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return 0
	}
	var pausedAt, resolvedAt time.Time
	havePaused, haveResolved := false, false
	for _, ev := range events {
		switch ev.Kind {
		case escalationRequestedEvent:
			var rec EscalationRecord
			if json.Unmarshal([]byte(ev.Message), &rec) == nil && strings.TrimSpace(rec.PausedAt) != "" {
				if t, err := parseJobTimestamp(rec.PausedAt); err == nil {
					pausedAt, havePaused = t, true
				}
			}
		case escalationResolvedEvent:
			var rec EscalationRecord
			if json.Unmarshal([]byte(ev.Message), &rec) == nil && strings.TrimSpace(rec.PausedAt) != "" {
				// Resolved events reuse the PausedAt field to carry resolved_at.
				if t, err := parseJobTimestamp(rec.PausedAt); err == nil {
					resolvedAt, haveResolved = t, true
				}
			}
		}
	}
	if !havePaused {
		return 0
	}
	end := now
	if haveResolved {
		end = resolvedAt
	}
	if dur := end.Sub(pausedAt); dur > 0 {
		return dur
	}
	return 0
}

// parseJobTimestamp parses a jobs.created_at value. SQLite's CURRENT_TIMESTAMP
// default is UTC in "2006-01-02 15:04:05" form; RFC3339 is also accepted
// defensively in case a timestamp was written explicitly.
func parseJobTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized job timestamp %q", s)
}

func effectiveDelegationTimeout(d Delegation, defaults DelegationTimeoutDefaults) string {
	if timeout := strings.TrimSpace(d.Timeout); timeout != "" {
		return timeout
	}
	return defaults.timeoutFor(d)
}

// delegationRequest builds the canonical child JobRequest for a delegation,
// inheriting the parent's repo/branch/PR context and stamping the DAG fields
// (ParentJobID/DelegationID/DelegationDepth/DelegatedBy/RootJobID/Deps). It is
// shared by dispatchDelegations (initial enqueue of ready delegations) and
// advanceDelegations (deferred enqueue once deps clear) so both paths produce
// identical, idempotent requests for the same delegation ID.
func (e Engine) delegationRequest(job db.Job, payload JobPayload, d Delegation) JobRequest {
	// An ephemeral delegation has no pre-registered agent: synthesize a stable
	// agent name (carrying the "-ephemeral-" infix the TUI filters on) and thread
	// the worker spec through so the daemon can materialize the worker and the
	// engine can skip the registered-agent checks. A non-ephemeral delegation
	// keeps routing to its named agent unchanged.
	agent := d.Agent
	var ephemeral *EphemeralSpec
	if d.Ephemeral != nil {
		agent = ephemeralAgentName(d.ID, job.ID)
		ephemeral = d.Ephemeral
	}
	rootID := e.rootJobID(job, payload)
	return JobRequest{
		ID:              job.ID + "/delegation/" + d.ID,
		Agent:           agent,
		Ephemeral:       ephemeral,
		Action:          d.Action,
		Repo:            payload.Repo,
		Branch:          payload.Branch,
		PullRequest:     payload.PullRequest,
		HeadSHA:         payload.HeadSHA,
		GoalID:          payload.GoalID,
		TaskID:          payload.TaskID,
		TaskTitle:       payload.TaskTitle,
		LeadAgent:       payload.LeadAgent,
		Reviewers:       payload.Reviewers,
		ReviewRound:     payload.ReviewRound,
		Sender:          job.Agent,
		Instructions:    strings.TrimSpace(d.Prompt),
		Constraints:     payload.Constraints,
		ParentJobID:     job.ID,
		DelegationID:    d.ID,
		DelegationDepth: payload.DelegationDepth + 1,
		DelegatedBy:     job.Agent,
		RootJobID:       rootID,
		Deps:            compactStrings(d.Deps),
		JobTimeout:      effectiveDelegationTimeout(d, e.DelegationTimeoutDefaults),
		Fingerprint:     strings.TrimSpace(d.Fingerprint),
		FailurePolicy:   strings.TrimSpace(d.FailurePolicy),
		SynthesisRule:   strings.TrimSpace(d.SynthesisRule),
		Model:           strings.TrimSpace(d.Model),
		Phase:           strings.TrimSpace(d.Phase),
		// Cockpit settings are inherited from the coordinator so every delegation
		// subagent in one tree renders a pane under the same workspace/session.
		Cockpit:        payload.Cockpit,
		CockpitSession: payload.CockpitSession,
		CockpitPaneKey: payload.CockpitPaneKey,
	}
}

// MaxDelegationDepth bounds how deep delegation nesting and coordinator
// continuation chains may go. Each delegation child and each coordinator
// continuation increments DelegationDepth; once a job at or beyond this depth
// would dispatch, dispatchDelegations refuses and records a
// delegation_depth_exceeded event. This is a safety net against runaway
// recursion: a coordinator whose continuation re-delegates (e.g. a static or
// looping agent) would otherwise spawn jobs forever.
const MaxDelegationDepth = 8

// MaxDelegationTotalJobs bounds how many jobs a single coordination tree (all
// children and continuations sharing one root, see rootJobID) may produce. Where
// MaxDelegationDepth caps nesting in one branch, this caps the total fan-out so a
// coordinator that re-delegates wide (rather than deep) on every continuation is
// still halted. Once a root's tree reaches this many jobs, dispatchDelegations
// refuses further children and records a delegation_budget_exceeded event.
const MaxDelegationTotalJobs = 64

// envIntOr returns the POSITIVE integer value of env var name, else def.
func envIntOr(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// effectiveMaxDelegationDepth / effectiveMaxDelegationTotalJobs let a deployment
// whose coordinator legitimately runs LONG continuation chains (e.g. a council
// coordinator that drives 6 rounds plus up to 4 fix cycles — each round and each
// fix-cycle continuation increments DelegationDepth, so the default 8 is exhausted
// after ~1 fix cycle) raise the bounds via env, WITHOUT weakening the safe default
// for every other gitmoot user. The other backstops (width, wall-clock, token/cost
// budgets) still bound runaway recursion. Env (positive ints):
//
//	GITMOOT_MAX_DELEGATION_DEPTH       (default MaxDelegationDepth = 8)
//	GITMOOT_MAX_DELEGATION_TOTAL_JOBS  (default MaxDelegationTotalJobs = 64)
func effectiveMaxDelegationDepth() int {
	return envIntOr("GITMOOT_MAX_DELEGATION_DEPTH", MaxDelegationDepth)
}
func effectiveMaxDelegationTotalJobs() int {
	return envIntOr("GITMOOT_MAX_DELEGATION_TOTAL_JOBS", MaxDelegationTotalJobs)
}

// MaxDelegationWidth bounds how many delegations a single coordinator result may
// fan out in one generation. The total-jobs budget is checked before a batch is
// dispatched, so it cannot stop one enormous fan-out on its own; this caps the
// width of a single dispatch so a coordinator returning hundreds of delegations
// at once is refused with a delegation_width_exceeded event.
const MaxDelegationWidth = 16

// defaultMaxInlineArtifactBytes is the per-body cap applied to each child's
// inlined ArtifactBody in a coordinator continuation prompt when
// Engine.MaxInlineArtifactBytes is unset (<= 0). See InlineArtifactBodies.
const defaultMaxInlineArtifactBytes = 32 * 1024

// maxInlineArtifactTotalBytes bounds the aggregate of all child ArtifactBodies
// inlined into a single coordinator continuation prompt, independent of the
// per-body cap, so a wide fan-out of briefs cannot balloon one continuation.
const maxInlineArtifactTotalBytes = 128 * 1024

// MaxDelegationWallClock bounds how long a single coordination tree (all children
// and continuations sharing one root, see rootJobID) may run before a coordinator
// is refused further fan-out. Where the depth/job-count caps bound the shape of
// the tree, this bounds its duration so an expensive-but-not-numerous tree (slow
// agents, long per-job work) cannot run unbounded. Measured from the root job's
// created_at; once exceeded, dispatchDelegations refuses further children and
// records a delegation_walltime_exceeded event. It is a generous runaway backstop,
// not a tight SLA. (A future enhancement could make it configurable — see #338.)
const MaxDelegationWallClock = 2 * time.Hour

// delegationHashWindowSize is how many recent delegation-set hashes a coordinator
// continuation chain remembers (threaded via the payload, not scanned across
// jobs). A repeat within this sliding window is what the loop detector treats as
// non-progress: a coordinator re-issuing a delegation set it already issued.
// NOTE (follow-up, issue #305 "Later"): detection is set-only (not result-aware)
// and only catches cycles of period <= delegationHashWindowSize; longer cycles
// fall back to the depth/total-job/width backstops, which now enqueue a graceful
// finalize continuation rather than stopping silently.
const delegationHashWindowSize = 3

// canonicalDelegationSetHash hashes a delegation set into a stable, order-
// independent fingerprint so two continuations that re-issue "the same work"
// produce the same hash even if the delegations are listed in a different order.
// Delegations are sorted by ID; for each, the ID, Agent, Action, trimmed Prompt,
// and sorted/compacted Deps are emitted with a separator that cannot appear in a
// normal field, then the whole thing is SHA-256 hashed. Any change to a prompt,
// agent, action, dep, or the set of ids changes the hash; pure reordering does
// not.
func canonicalDelegationSetHash(dels []Delegation) string {
	sorted := make([]Delegation, len(dels))
	copy(sorted, dels)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var builder strings.Builder
	for _, d := range sorted {
		deps := compactStrings(d.Deps)
		sort.Strings(deps)
		// For an ephemeral delegation, d.Agent is empty; fold the spec identity
		// (runtime/model/template/role) into the hash so two distinct ephemeral
		// specs are not mistaken for the same work by loop detection / dedup.
		eph := ""
		if d.Ephemeral != nil {
			eph = strings.Join([]string{d.Ephemeral.Runtime, d.Ephemeral.Model, d.Ephemeral.Template, d.Ephemeral.Role}, "|")
		}
		fields := []string{d.ID, d.Agent, eph, d.Action, strings.TrimSpace(d.Prompt), strings.Join(deps, ",")}
		builder.WriteString(strings.Join(fields, "\x1f"))
		builder.WriteString("\x1e")
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// appendDelegationHashWindow appends h to the sliding window, keeping only the
// most recent delegationHashWindowSize entries so the loop detector reasons over
// a bounded history threaded through the payload.
func appendDelegationHashWindow(window []string, h string) []string {
	window = append(window, h)
	if len(window) > delegationHashWindowSize {
		window = window[len(window)-delegationHashWindowSize:]
	}
	return window
}

// windowContainsHash reports whether the sliding window already holds h, i.e. the
// coordinator is re-issuing a delegation set it issued within the last few
// generations.
func windowContainsHash(window []string, h string) bool {
	for _, existing := range window {
		if existing == h {
			return true
		}
	}
	return false
}

// progressDigest fingerprints the VERIFIABLE side effects a finished generation
// of delegated children produced, so the result-aware loop detector (#339) can
// tell "a coordinator that keeps producing the same results" (no progress) from
// "genuinely new results" (progress). Two generations whose children advanced
// nothing externally observable hash identically EVEN IF the coordinator
// perturbed the delegation set (different ids/agents/prompts) or the children
// rewrote their self-reported prose; any new durable side effect — a different
// decision, a new commit/change, a test run, a new PR/HeadSHA, or a changed
// artifact body — changes the digest.
//
// Only externally-verifiable fields are folded in, per child: Result.Decision,
// Result.ChangesMade, Result.TestsRun, the child payload's PullRequest + HeadSHA
// (where a real PR advance lands), and Result.ArtifactBody. The self-reported
// Summary/Findings TEXT is deliberately excluded (weight 0) so a coordinator
// cannot defeat the detector by churning prose, and a repeated summary over a
// real new side effect is still correctly read as progress.
//
// Crucially the per-child side-effect tuple is hashed WITHOUT the delegation id,
// then the tuples are SORTED, so the digest depends only on the multiset of side
// effects the generation produced — not on the labels the coordinator chose.
// That is what closes the evasion hole: a coordinator that trivially perturbs the
// delegation set each round but whose children keep returning nothing new yields
// the same (empty-result) multiset every round, so the digest repeats and the
// streak climbs. A child whose Result did not parse contributes the same empty
// tuple as a child that ran but produced no durable effect. The separators (\x1f
// between fields, \x1e between children, \x1d between list items) keep field
// boundaries unambiguous.
func progressDigest(dels []Delegation, childPayloads map[string]JobPayload) string {
	tuples := make([]string, 0, len(dels))
	for _, d := range dels {
		fields := []string{"", "", "", "0", "", ""}
		if payload, ok := childPayloads[d.ID]; ok && payload.Result != nil {
			r := payload.Result
			changes := append([]string(nil), r.ChangesMade...)
			sort.Strings(changes)
			tests := append([]string(nil), r.TestsRun...)
			sort.Strings(tests)
			fields = []string{
				strings.TrimSpace(r.Decision),
				strings.Join(changes, "\x1d"),
				strings.Join(tests, "\x1d"),
				strconv.Itoa(payload.PullRequest),
				strings.TrimSpace(payload.HeadSHA),
				strings.TrimSpace(r.ArtifactBody),
			}
		}
		tuples = append(tuples, strings.Join(fields, "\x1f"))
	}
	sort.Strings(tuples)

	var builder strings.Builder
	for _, t := range tuples {
		builder.WriteString(t)
		builder.WriteString("\x1e")
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// countRootDelegationJobs counts every job belonging to a coordination tree: the
// originating coordinator itself (its row self-roots to rootID) plus every child
// or continuation whose root_id points back at it. The denormalized root_id
// column (#420) lets the store answer with an indexed COUNT(*) instead of a
// full-table scan that unmarshals every payload; the grouping key is identical
// (root_id is the write-time denormalization of the old payload.RootJobID
// filter), so the count is byte-identical.
func (e Engine) countRootDelegationJobs(ctx context.Context, rootID string) (int, error) {
	return e.Store.CountJobsByRoot(ctx, rootID)
}

// rootJobCountForPressure returns the per-root job count for the budget-pressure
// nudge (#418), failing open to 0 on any lookup error. A 0 makes budgetPressureLine
// drop only the job clause (depth pressure still fires); it must never error the
// continuation, which is why this swallows the error rather than propagating it.
func (e Engine) rootJobCountForPressure(ctx context.Context, rootID string) int {
	count, err := e.countRootDelegationJobs(ctx, rootID)
	if err != nil {
		return 0
	}
	return count
}

// projectedNewDelegationJobs computes how many NEW child jobs the given
// delegation batch would add to a coordination tree — the count used by the
// #649 projected, all-or-nothing job-budget admission in dispatchDelegations.
// Unlike the reactive token/cost budgets (which cannot know an unspawned child's
// usage), the job count is exactly projectable before enqueue, so the whole
// batch is admitted or refused as one unit against the live tree count.
//
// A leg contributes 0 when it is already accounted for in that live count, so
// re-advancing a coordinator (AdvanceJob is idempotent and re-runs
// dispatchDelegations) never double-counts children already enqueued:
//
//   - a leg whose child already exists under this parent is skipped — it is
//     already inside countRootDelegationJobs;
//   - a leg whose non-empty fingerprint already appears among an existing
//     sibling, or earlier in THIS batch, is skipped — it folds into the winning
//     sibling via the same dedup enqueueDelegation performs (engine.go ~2440),
//     so it never spawns its own child.
//
// Deferred (deps-bearing) legs ARE counted: they escape every later job-budget
// check (advanceDelegations/enqueueDelegation/allocateAndEnqueueDelegation do
// not re-check the per-root job count), so the entire batch — ready and
// deferred — must be pre-authorized here as a single unit.
func (e Engine) projectedNewDelegationJobs(ctx context.Context, parentJobID string, dels []Delegation) (int, error) {
	children, err := e.childDelegationJobs(ctx, parentJobID)
	if err != nil {
		return 0, err
	}
	// One scan of the existing sibling children collects the fingerprints already
	// present, instead of a per-leg delegationFingerprintSeen query.
	existingFingerprints := make(map[string]struct{})
	for _, child := range children {
		childPayload, err := unmarshalPayload(child.Payload)
		if err != nil {
			return 0, err
		}
		if fingerprint := strings.TrimSpace(childPayload.Fingerprint); fingerprint != "" {
			existingFingerprints[fingerprint] = struct{}{}
		}
	}
	batchFingerprints := make(map[string]struct{})
	projected := 0
	for _, d := range dels {
		if _, exists := children[d.ID]; exists {
			// Already enqueued on an earlier advance; counted in the live total.
			continue
		}
		if fingerprint := strings.TrimSpace(d.Fingerprint); fingerprint != "" {
			if _, seen := existingFingerprints[fingerprint]; seen {
				// Folds into an existing same-fingerprint sibling child.
				continue
			}
			if _, seen := batchFingerprints[fingerprint]; seen {
				// Folds into an earlier same-fingerprint leg in this same batch.
				continue
			}
			batchFingerprints[fingerprint] = struct{}{}
		}
		projected++
	}
	return projected, nil
}

// sumRootDelegationTokens sums the runtime token usage (input + output) across an
// entire coordination tree: the originating coordinator itself (self-rooted to
// rootID) plus every child or continuation whose root_id points back at it. Used
// by the per-root token budget (#338 Part B) to decide, before dispatching a new
// generation, whether the tree has already spent its budget. The denormalized
// root_id column (#420) lets the store sum entirely in SQL — same grouping key,
// byte-identical total, zero payload unmarshal. Token capture is best-effort per
// runtime (see internal/runtime); a job whose runtime did not report usage
// contributes 0, so the sum under-counts rather than over-counts.
func (e Engine) sumRootDelegationTokens(ctx context.Context, rootID string) (int, error) {
	return e.Store.SumJobTokensByRoot(ctx, rootID)
}

func (e Engine) dispatchDelegations(ctx context.Context, job db.Job, payload JobPayload, ref taskRef) error {
	if payload.Result == nil || len(payload.Result.Delegations) == 0 {
		return nil
	}

	// Ephemeral workers are leaf executors: they are auto-disposed when their job
	// completes, so a continuation enqueued to their (now-deleted) synthetic agent
	// would strand. Do not dispatch delegations returned by an ephemeral worker.
	if payload.Ephemeral != nil {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_ignored_ephemeral",
			Message: fmt.Sprintf("ephemeral workers cannot delegate; ignoring %d delegation(s)", len(payload.Result.Delegations)),
		})
		return nil
	}

	// A finalize continuation (enqueued when a backstop tripped, #305 "Later") is
	// terminal: the coordinator was asked to synthesize a best-effort final result,
	// so ignore any delegations it returned and stop the chain here. This must
	// precede the budget checks so an over-budget finalize continuation is not
	// itself re-tripped.
	if payload.DelegationFinalize {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_finalized",
			Message: fmt.Sprintf("finalize continuation is terminal; ignoring %d delegation(s)", len(payload.Result.Delegations)),
		})
		return nil
	}

	rootID := e.rootJobID(job, payload)

	// Operator kill switch (#341): an operator can terminate a runaway tree by
	// root id (gitmoot job kill). This is the FIRST backstop so operator action
	// wins over every budget cap: rather than dispatching the next generation,
	// route through the same #305 graceful finalize continuation (synthesize what
	// completed → stop). Fails open: a lookup error never blocks dispatch.
	if killed, _ := e.Store.IsRootJobKilled(ctx, rootID); killed {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_killed",
			Message: fmt.Sprintf("root delegation tree %s killed by operator; not dispatching %d delegation(s)", rootID, len(payload.Result.Delegations)),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, "delegation tree killed by operator")
	}

	maxDepth := effectiveMaxDelegationDepth()
	if payload.DelegationDepth >= maxDepth {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_depth_exceeded",
			Message: fmt.Sprintf("delegation depth %d reached the limit of %d; not dispatching %d delegation(s)", payload.DelegationDepth, maxDepth, len(payload.Result.Delegations)),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("delegation depth limit of %d reached", maxDepth))
	}

	total, err := e.countRootDelegationJobs(ctx, rootID)
	if err != nil {
		return err
	}
	// #649: PROJECTED, all-or-nothing job-budget admission. The old guard was
	// reactive (`total >= maxJobs`) and only tripped once the tree was ALREADY at
	// the cap, so a wide fan-out launched from just under the limit enqueued its
	// whole batch and overshot MaxDelegationTotalJobs by up to (batch_width - 1).
	// The job count is the one backstop that is exactly projectable before
	// enqueue, so count the NEW jobs this batch would add (ready AND deferred legs
	// — deferred legs escape every later budget check — minus already-enqueued /
	// fingerprint-deduped legs) and refuse the WHOLE batch when the tree would
	// cross the cap. total+projected == maxJobs ADMITS (may reach but not exceed);
	// > maxJobs refuses. When projected == 0 (an idempotent re-advance whose legs
	// all already have children) this admits and the enqueue loop below adds
	// nothing new — the correct idempotent outcome, where the old guard finalized.
	projected, err := e.projectedNewDelegationJobs(ctx, job.ID, payload.Result.Delegations)
	if err != nil {
		return err
	}
	maxJobs := effectiveMaxDelegationTotalJobs()
	if total+projected > maxJobs {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_budget_exceeded",
			Message: fmt.Sprintf("delegation batch of %d new job(s) would exceed the per-root job budget of %d (tree at %d); not dispatching %d delegation(s)", projected, maxJobs, total, len(payload.Result.Delegations)),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("per-root job budget of %d reached", maxJobs))
	}

	if exceeded, elapsed := e.rootWallClockExceeded(ctx, rootID); exceeded {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_walltime_exceeded",
			Message: fmt.Sprintf("delegation tree for root %s ran %s, exceeding the wall-clock limit of %s; not dispatching %d delegation(s)", rootID, elapsed.Round(time.Second), MaxDelegationWallClock, len(payload.Result.Delegations)),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("wall-clock limit of %s reached", MaxDelegationWallClock))
	}

	// Per-root token budget (#338 Part B). Where the depth/total-jobs/wall-clock
	// backstops bound the shape and duration of a tree, this bounds its cost: a
	// coordination tree that has already used at least MaxDelegationTokenBudget
	// tokens (input + output, summed across the whole tree) is refused further
	// fan-out and routed to the #305 finalize continuation. It is opt-in: when the
	// budget is 0 (the default) the check is skipped entirely, so default behavior
	// is byte-identical. The sum fails open — a lookup/parse hiccup yields 0 and
	// does not block dispatch — and is best-effort per runtime (a runtime that
	// reports no usage contributes 0, so the budget under-counts that runtime).
	if e.MaxDelegationTokenBudget > 0 {
		if used, _ := e.sumRootDelegationTokens(ctx, rootID); used >= e.MaxDelegationTokenBudget {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_cost_exceeded",
				Message: fmt.Sprintf("delegation tree %s reached token budget %d (used %d); not dispatching %d delegation(s)", rootID, e.MaxDelegationTokenBudget, used, len(payload.Result.Delegations)),
			})
			return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("token budget %d reached", e.MaxDelegationTokenBudget))
		}
	}

	// Per-root dollar-cost budget (#380). This is the cost analogue of the token
	// budget above: where the token budget bounds raw token count, this bounds the
	// measured spend derived from those same tokens × a per-model price table (see
	// cost.go). A tree that has already spent at least MaxDelegationCostUSD dollars
	// is refused further fan-out and routed to the same #305 finalize continuation,
	// exactly like every backstop above — never hard-killed. It is opt-in: when the
	// budget is 0 (the default) the check is skipped entirely, so default behavior
	// is byte-identical. The sum fails open (a lookup/parse hiccup yields 0 and does
	// not block dispatch) and is best-effort per runtime: a runtime that reports no
	// usage contributes $0, so the budget under-counts that runtime.
	if e.MaxDelegationCostUSD > 0 {
		if spent, _ := e.sumRootDelegationCost(ctx, rootID); spent >= e.MaxDelegationCostUSD {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_cost_usd_exceeded",
				Message: fmt.Sprintf("delegation tree %s reached cost budget $%.4f (spent $%.4f); not dispatching %d delegation(s)", rootID, e.MaxDelegationCostUSD, spent, len(payload.Result.Delegations)),
			})
			return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("cost budget $%.4f reached", e.MaxDelegationCostUSD))
		}
	}

	if width := len(payload.Result.Delegations); width > MaxDelegationWidth {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_width_exceeded",
			Message: fmt.Sprintf("delegation set width %d exceeds the per-coordinator limit of %d; not dispatching", width, MaxDelegationWidth),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("delegation set width %d exceeds the per-coordinator limit of %d", width, MaxDelegationWidth))
	}

	// Windowed non-progress detection. The depth/budget caps above are blunt
	// safety nets; this catches the common loop sooner: a coordinator whose
	// continuation re-issues a delegation set it already issued within the last
	// few generations (the window threaded through the payload). A real,
	// progressing coordinator emits a different set each round, so its hash is
	// never in the window and dispatch proceeds untouched.
	if stopped, err := e.handleDelegationLoop(ctx, job, payload, ref); err != nil || stopped {
		return err
	}

	delegations := payload.Result.Delegations

	// Preflight every delegation (ready and deferred) before enqueueing any
	// child job so a partial failure does not leave the parent half-dispatched
	// and an unknown/uncapable agent on a deferred delegation blocks the parent
	// immediately rather than only when its deps clear. Use a lightweight check
	// that does not acquire branch locks or other execution side effects.
	for _, d := range delegations {
		request := e.delegationRequest(job, payload, d)
		if err := e.preflightDelegation(ctx, request); err != nil {
			// An unroutable delegation set (an unknown / not-allowed / uncapable
			// agent — usually a runtime name where an agent NAME was required) is no
			// longer a terminal block: that dead-ends the coordinator before any
			// child or continuation enqueues. Instead emit a structured
			// delegation_preflight_failed event and route the actionable error back
			// through the corrective continuation so the coordinator can re-emit a
			// corrected set. The all-or-nothing preflight is preserved: we return on
			// the FIRST failure before the enqueue loop below, so no partial dispatch,
			// and deferred legs are covered because this loop sees every delegation.
			reason := fmt.Sprintf("delegation %q preflight failed: %v", request.DelegationID, err)
			return e.handleDelegationPreflightFailure(ctx, job, payload, reason)
		}
	}

	// Write the shared coordinator brief and context manifest once, fully,
	// before enqueueing any child job so a child that starts immediately reads a
	// complete directory. Every child of a parent that produced artifacts is
	// pointed at the same directory so its prompt can reference brief.md and
	// context-manifest.json. Writing is skipped when the engine has no artifact
	// root or no delegation requested artifacts.
	artifactDir, err := writeDelegationArtifacts(e.ArtifactRoot, job.ID, payload.Result)
	if err != nil {
		return e.block(ctx, ref, fmt.Sprintf("write delegation artifacts: %v", err))
	}
	if artifactDir != "" {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_artifacts_written",
			Message: fmt.Sprintf("delegation artifacts written to %s", artifactDir),
		})
	}

	// Only delegations with no unmet deps are enqueued now; deferred ones are
	// enqueued by advanceDelegations once every dep has succeeded. Allocate
	// worktrees/branch locks only for the ready implement delegations so a
	// deferred delegation does not hold a lock before it can run.
	for _, d := range delegations {
		if len(compactStrings(d.Deps)) > 0 {
			continue
		}
		// Ready (dep-free) delegations dispatch here with no upstream context: a
		// dep-free leg has no upstream deps to inject, and a deps-bearing leg is
		// deferred to advanceDelegations (which injects #419 upstream results).
		if err := e.enqueueDelegation(ctx, job, payload, d, artifactDir, "", ref); err != nil {
			return err
		}
	}
	return nil
}

// handleDelegationLoop implements the windowed non-progress check. It compares
// the current delegation set's canonical hash against the sliding window
// threaded through the payload and decides whether dispatch should be halted:
//
//   - the hash is NOT in the window: not a repeat. Returns (false, nil); the
//     caller dispatches normally and maybeEnqueueContinuation records this hash
//     in the window for the next generation.
//   - the hash IS in the window and the coordinator was already nudged once
//     (DelegationRepeatCount >= 1): a confirmed loop. Records a
//     delegation_loop_detected event and returns (true, nil) so dispatch is
//     skipped — the coordinator got a corrective continuation and repeated anyway.
//   - the hash IS in the window for the first time: records a
//     delegation_loop_warning and, instead of dispatching, enqueues one
//     corrective continuation that tells the coordinator to change its approach
//     or finish. Returns (true, nil) so the repeat is not dispatched.
//
// Returning (true, ...) means "do not dispatch"; (false, nil) means "proceed".
func (e Engine) handleDelegationLoop(ctx context.Context, job db.Job, payload JobPayload, ref taskRef) (bool, error) {
	currentHash := canonicalDelegationSetHash(payload.Result.Delegations)
	if !windowContainsHash(payload.RecentDelegationHashes, currentHash) {
		return false, nil
	}

	// Idempotent on re-advance: if this job already emitted a loop event it has
	// already enqueued its corrective continuation (deterministic id), so do not
	// re-emit the event or double-count it.
	events, err := e.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return true, err
	}
	for _, ev := range events {
		if ev.Kind == "delegation_loop_warning" || ev.Kind == "delegation_loop_detected" {
			return true, nil
		}
	}

	if payload.DelegationRepeatCount >= 1 {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_loop_detected",
			Message: fmt.Sprintf("delegation set %s repeated after a corrective nudge (repeat count %d); not dispatching %d delegation(s)", currentHash, payload.DelegationRepeatCount, len(payload.Result.Delegations)),
		})
		// Graceful finalize (#305 "Later"): rather than stopping silently, give the
		// coordinator one terminal continuation to synthesize a best-effort result.
		if err := e.enqueueFinalizeContinuation(ctx, job, payload, "delegation set repeated after a corrective nudge"); err != nil {
			return true, err
		}
		return true, nil
	}

	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_loop_warning",
		Message: fmt.Sprintf("delegation set %s repeats a recent round; sending a corrective continuation instead of dispatching %d delegation(s)", currentHash, len(payload.Result.Delegations)),
	})

	request := JobRequest{
		ID:     delegationContinuationID(job.ID),
		Agent:  job.Agent,
		Action: "ask",
		Model:  payload.Model,
		// Per-job runtime override (#531): a same-coordinator continuation stays on
		// the override runtime/ref, like Model (see maybeEnqueueContinuation).
		RuntimeOverride:    payload.RuntimeOverride,
		RuntimeOverrideRef: payload.RuntimeOverrideRef,
		Phase:              payload.Phase,
		Repo:               payload.Repo,
		Branch:             payload.Branch,
		PullRequest:        payload.PullRequest,
		HeadSHA:            payload.HeadSHA,
		GoalID:             payload.GoalID,
		TaskID:             payload.TaskID,
		TaskTitle:          payload.TaskTitle,
		LeadAgent:          payload.LeadAgent,
		Reviewers:          payload.Reviewers,
		Sender:             job.Agent,
		Instructions:       buildCorrectiveContinuationPrompt(e.originalGoal(ctx, e.rootJobID(job, payload), payload.Instructions), payload.Result),
		Constraints:        payload.Constraints,
		ParentJobID:        job.ID,
		DelegationDepth:    payload.DelegationDepth + 1,
		DelegatedBy:        job.Agent,
		RootJobID:          e.rootJobID(job, payload),
		// Carry the window forward (now including this repeat) and mark that a
		// corrective nudge has fired, so if the next generation repeats again the
		// detector escalates to delegation_loop_detected.
		RecentDelegationHashes: appendDelegationHashWindow(payload.RecentDelegationHashes, currentHash),
		DelegationRepeatCount:  payload.DelegationRepeatCount + 1,
		// Inherit the coordinator's cockpit settings so the continuation renders
		// its pane under the same workspace/session as the rest of the tree.
		Cockpit:        payload.Cockpit,
		CockpitSession: payload.CockpitSession,
		CockpitPaneKey: payload.CockpitPaneKey,
	}
	if err := e.enqueue(ctx, request); err != nil {
		return true, fmt.Errorf("enqueue corrective continuation for %q: %w", job.ID, err)
	}
	return true, nil
}

// handleDelegationPreflightFailure is the disposition of an unroutable delegation
// set (#451). It mirrors the delegation-loop seam (handleDelegationLoop) rather
// than terminal-blocking: the coordinator named an agent that does not resolve to
// a routable registered agent (unknown / not-allowed / uncapable — most often a
// runtime name where an agent NAME was required), so instead of dead-ending it is
// re-invoked once with the actionable error as a corrective continuation so it can
// re-emit a corrected set. The non-progress bound (MaxDelegationNonProgressStreak)
// guarantees a graceful finalize if it keeps naming bad agents: a repeat after a
// corrective nudge (DelegationRepeatCount >= 1) at the streak threshold routes to
// the #305 finalize continuation rather than looping forever.
//
// It always emits a structured delegation_preflight_failed event carrying the
// reason (the observability surface for `gitmoot job list`), then enqueues the
// corrective (or finalize) continuation and returns nil — NOT a BlockedError — so
// the coordinator can self-correct. Idempotent on re-advance: a deterministic
// continuation id makes e.enqueue a no-op, and a once-guard on the
// delegation_preflight_failed event avoids double-emitting/double-enqueueing.
func (e Engine) handleDelegationPreflightFailure(ctx context.Context, job db.Job, payload JobPayload, reason string) error {
	// Once-guard: if this generation already recorded a preflight failure it has
	// already enqueued its single continuation (deterministic id), so a re-advance
	// must not re-emit the event or re-run the streak logic.
	events, err := e.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.Kind == "delegation_preflight_failed" {
			return nil
		}
	}

	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_preflight_failed",
		Message: reason,
	})

	// Bound the corrective loop with the SAME streak ladder the loop/non-progress
	// detectors use: a coordinator that keeps re-emitting an unroutable set climbs
	// the streak and, once a corrective nudge has already fired, finalizes
	// gracefully instead of looping forever.
	nonProgressStreak := payload.NonProgressStreak + 1
	if nonProgressStreak >= e.nonProgressStreakThreshold() && payload.DelegationRepeatCount >= 1 {
		return e.enqueueFinalizeContinuation(ctx, job, payload, "delegation set named an unroutable agent after a corrective nudge")
	}

	goal := e.originalGoal(ctx, e.rootJobID(job, payload), payload.Instructions)
	request := JobRequest{
		ID:     delegationContinuationID(job.ID),
		Agent:  job.Agent,
		Action: "ask",
		Model:  payload.Model,
		// Per-job runtime override (#531): a same-coordinator continuation stays on
		// the override runtime/ref, like Model (see maybeEnqueueContinuation).
		RuntimeOverride:    payload.RuntimeOverride,
		RuntimeOverrideRef: payload.RuntimeOverrideRef,
		Phase:              payload.Phase,
		Repo:               payload.Repo,
		Branch:             payload.Branch,
		PullRequest:        payload.PullRequest,
		HeadSHA:            payload.HeadSHA,
		GoalID:             payload.GoalID,
		TaskID:             payload.TaskID,
		TaskTitle:          payload.TaskTitle,
		LeadAgent:          payload.LeadAgent,
		Reviewers:          payload.Reviewers,
		Sender:             job.Agent,
		Instructions:       buildPreflightCorrectiveContinuationPrompt(goal, payload.Result, reason),
		Constraints:        payload.Constraints,
		ParentJobID:        job.ID,
		DelegationDepth:    payload.DelegationDepth + 1,
		DelegatedBy:        job.Agent,
		RootJobID:          e.rootJobID(job, payload),
		// Carry the window forward and mark that a corrective nudge has fired, and
		// thread the streak forward, so a coordinator that keeps naming bad agents
		// escalates to a graceful finalize.
		RecentDelegationHashes: appendDelegationHashWindow(payload.RecentDelegationHashes, canonicalDelegationSetHash(payload.Result.Delegations)),
		DelegationRepeatCount:  payload.DelegationRepeatCount + 1,
		NonProgressStreak:      nonProgressStreak,
		LastProgressDigest:     payload.LastProgressDigest,
		// Inherit the coordinator's cockpit settings so the continuation renders its
		// pane under the same workspace/session as the rest of the tree.
		Cockpit:        payload.Cockpit,
		CockpitSession: payload.CockpitSession,
		CockpitPaneKey: payload.CockpitPaneKey,
	}
	if err := e.enqueue(ctx, request); err != nil {
		return fmt.Errorf("enqueue preflight corrective continuation for %q: %w", job.ID, err)
	}
	// The corrective continuation IS the coordinator's single continuation, so it
	// occupies the continuation slot: emit delegation_continuation_enqueued so a
	// re-advance hits the continuationEnqueued top-guard.
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_continuation_enqueued",
		Message: fmt.Sprintf("preflight corrective continuation occupies the continuation slot for job %s", request.ID),
	})
	return nil
}

// enqueueFinalizeContinuation enqueues a single best-effort "finalize"
// continuation back to the coordinator after a termination backstop trips (loop
// detected, or a per-root depth/job/width budget exceeded). Instead of dropping
// the offending delegations silently, the coordinator is re-invoked once with the
// completed results and told to synthesize a final result and return empty
// delegations. The continuation carries DelegationFinalize, so dispatchDelegations
// treats its result as terminal (any delegations it returns are ignored),
// guaranteeing the chain stops. Idempotent: the deterministic continuation id makes
// e.enqueue a no-op on re-advance, and the once-guard avoids duplicate events.
func (e Engine) enqueueFinalizeContinuation(ctx context.Context, job db.Job, payload JobPayload, reason string) error {
	events, err := e.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.Kind == "delegation_finalize_enqueued" {
			return nil
		}
	}
	request := JobRequest{
		ID:     delegationContinuationID(job.ID),
		Agent:  job.Agent,
		Action: "ask",
		Model:  payload.Model,
		// Per-job runtime override (#531): the finalize continuation stays on the
		// override runtime/ref, like Model (see maybeEnqueueContinuation).
		RuntimeOverride:    payload.RuntimeOverride,
		RuntimeOverrideRef: payload.RuntimeOverrideRef,
		Phase:              payload.Phase,
		Repo:               payload.Repo,
		Branch:             payload.Branch,
		PullRequest:        payload.PullRequest,
		HeadSHA:            payload.HeadSHA,
		GoalID:             payload.GoalID,
		TaskID:             payload.TaskID,
		TaskTitle:          payload.TaskTitle,
		LeadAgent:          payload.LeadAgent,
		Reviewers:          payload.Reviewers,
		Sender:             job.Agent,
		Instructions:       buildFinalizeContinuationPrompt(e.originalGoal(ctx, e.rootJobID(job, payload), payload.Instructions), payload.Result, reason),
		Constraints:        payload.Constraints,
		ParentJobID:        job.ID,
		DelegationDepth:    payload.DelegationDepth + 1,
		DelegatedBy:        job.Agent,
		RootJobID:          e.rootJobID(job, payload),
		DelegationFinalize: true,
		// Inherit the coordinator's cockpit settings so the finalize continuation
		// renders its pane under the same workspace/session as the rest of the tree.
		Cockpit:        payload.Cockpit,
		CockpitSession: payload.CockpitSession,
		CockpitPaneKey: payload.CockpitPaneKey,
	}
	if err := e.enqueue(ctx, request); err != nil {
		return fmt.Errorf("enqueue finalize continuation for %q: %w", job.ID, err)
	}
	// The finalize continuation IS the coordinator's single continuation, so it
	// occupies the continuation slot: emit delegation_continuation_enqueued too.
	// This makes continuationEnqueued() true, so if the tripped coordinator's
	// advanceDelegations later runs (when this finalize child completes) it can
	// never enqueue a *normal* continuation that would collide with the finalize
	// job's deterministic id. The finalize-specific event below drives the
	// once-guard above and keeps the backstop observable.
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_continuation_enqueued",
		Message: fmt.Sprintf("finalize continuation occupies the continuation slot for job %s", request.ID),
	})
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_finalize_enqueued",
		Message: fmt.Sprintf("termination backstop tripped (%s); enqueued a best-effort finalize continuation as job %s", reason, request.ID),
	})
	return nil
}

// enqueueDelegation allocates the per-delegation worktree (or branch lock) for
// implement delegations, then enqueues the child job and records a
// delegation_enqueued event on the parent. It is idempotent: a duplicate
// deterministic-ID insert is swallowed by e.enqueue when the existing job
// matches the request, so it is safe to call from both dispatchDelegations and
// advanceDelegations.
func (e Engine) enqueueDelegation(ctx context.Context, job db.Job, payload JobPayload, d Delegation, artifactDir string, upstreamContext string, ref taskRef) error {
	request := e.delegationRequest(job, payload, d)
	request.DelegationArtifactDir = artifactDir
	// Append the #419 "Upstream dependency results" block (built by the caller
	// from this dependent's succeeded direct deps) to the child's instructions.
	// upstreamContext is "" unless Engine.InjectUpstreamDepContext is set, so the
	// flag-off enqueued prompt is byte-identical to before this field existed.
	if upstreamContext != "" {
		request.Instructions = request.Instructions + upstreamContext
	}

	// Fingerprint dedup: skip enqueueing a child whose fingerprint already
	// appears among a sibling under the same parent. Scoped to this parent via
	// the (parentJobID, fingerprint) key so identical fingerprints under
	// different parents never collide. The delegation's own child is excluded so
	// an idempotent re-enqueue of the same delegation is not treated as a dup.
	if fingerprint := strings.TrimSpace(d.Fingerprint); fingerprint != "" {
		seen, err := e.delegationFingerprintSeen(ctx, job.ID, d.ID, fingerprint)
		if err != nil {
			return err
		}
		if seen {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_deduped",
				Message: fmt.Sprintf("delegation %q skipped: fingerprint %q already enqueued (key %s)", d.ID, fingerprint, delegationFingerprintKey(job.ID, fingerprint)),
			})
			return nil
		}
	}

	return e.allocateAndEnqueueDelegation(ctx, job, payload, d, request, ref)
}

type integrationDepResolution struct {
	branches       []string
	alreadyOnBase  []string
	unresolvedDeps []string
}

// resolveIntegrationDeps classifies delegation d's implement-leg dependencies.
// Succeeded implement legs on their own branches must be merged into an
// integration worktree; succeeded implement legs already on the parent base are
// safe to read from the base checkout; missing/not-succeeded/invalid legs are
// unresolved and must fail closed so reviewers do not inspect stale code.
func (e Engine) resolveIntegrationDeps(ctx context.Context, job db.Job, payload JobPayload, d Delegation) (integrationDepResolution, error) {
	deps := compactStrings(d.Deps)
	if len(deps) == 0 || payload.Result == nil {
		return integrationDepResolution{}, nil
	}
	byID := make(map[string]Delegation, len(payload.Result.Delegations))
	for _, sib := range payload.Result.Delegations {
		byID[strings.TrimSpace(sib.ID)] = sib
	}
	hasImplementDep := false
	for _, dep := range deps {
		if sib, ok := byID[dep]; ok && !readOnlyDelegationAction(sib.Action) {
			hasImplementDep = true
			break
		}
	}
	if !hasImplementDep {
		return integrationDepResolution{}, nil
	}

	// Resolve each dep to its winning child job the same way advanceDelegations
	// does (latest attempt per delegation id), so a leg that succeeded on a retry
	// contributes its retry branch rather than the failed original.
	children, err := e.childDelegationJobs(ctx, job.ID)
	if err != nil {
		return integrationDepResolution{}, err
	}
	base := strings.TrimSpace(payload.Branch)
	var result integrationDepResolution
	for _, dep := range deps {
		sib, ok := byID[dep]
		if !ok || readOnlyDelegationAction(sib.Action) {
			continue
		}
		legJob, ok := children[dep]
		if !ok || legJob.State != string(JobSucceeded) {
			result.unresolvedDeps = append(result.unresolvedDeps, dep)
			continue
		}
		legPayload, err := unmarshalPayload(legJob.Payload)
		if err != nil {
			result.unresolvedDeps = append(result.unresolvedDeps, dep)
			continue
		}
		legBranch := strings.TrimSpace(legPayload.Branch)
		switch {
		case legBranch == "":
			result.unresolvedDeps = append(result.unresolvedDeps, dep)
		case legBranch == base:
			result.alreadyOnBase = append(result.alreadyOnBase, dep)
		default:
			result.branches = append(result.branches, legBranch)
		}
	}
	return result, nil
}

// integrationDepBranches returns the per-delegation branches of delegation d's
// succeeded implement-leg dependencies, so a dependent read-only step (e.g. a
// decompose-and-verify verify gate) can run against a worktree with those legs
// merged in rather than the base checkout (issue #332). It returns nil when d has
// no branch-backed implement deps, in which case the normal read-only paths apply.
// Read-only deps contribute no branch (they produce no implementation), and a leg
// that ran in the shared checkout (branch == parent base) is skipped because its
// work is already on the base.
func (e Engine) integrationDepBranches(ctx context.Context, job db.Job, payload JobPayload, d Delegation) ([]string, error) {
	result, err := e.resolveIntegrationDeps(ctx, job, payload, d)
	if err != nil {
		return nil, err
	}
	return result.branches, nil
}

// commitDelegationLeg commits an implement delegation leg's worktree changes to
// its own branch when the leg has its own per-delegation worktree but no task/PR
// finalizer (a PR-less local orchestrate, where the finalizer never runs and the
// edits would otherwise stay uncommitted). This makes the leg's work available on
// its branch for a dependent integration step (#332). It is a no-op for jobs with
// no delegation worktree, a clean worktree, or a manager that cannot commit.
func (e Engine) commitDelegationLeg(ctx context.Context, job db.Job, payload JobPayload) error {
	if strings.TrimSpace(payload.DelegationID) == "" || strings.TrimSpace(payload.WorktreePath) == "" {
		return nil
	}
	committer, ok := e.DelegationWorktrees.(WorktreeCommitter)
	if !ok {
		return nil
	}
	committed, err := committer.CommitWorktree(ctx, payload.WorktreePath, fmt.Sprintf("Gitmoot delegation %s implementation", payload.DelegationID))
	if err != nil {
		return err
	}
	if committed {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_committed",
			Message: fmt.Sprintf("delegation %q committed its implementation to branch %s", payload.DelegationID, payload.Branch),
		})
	}
	return nil
}

// allocateAndEnqueueDelegation allocates the per-delegation worktree (or branch
// lock) for implement delegations and enqueues the prepared request, recording a
// delegation_enqueued event. It is shared by enqueueDelegation (initial/deferred
// dispatch) and requeueDelegation (retry) so both go through identical worktree
// allocation and idempotent enqueue.
func (e Engine) allocateAndEnqueueDelegation(ctx context.Context, job db.Job, payload JobPayload, d Delegation, request JobRequest, ref taskRef) error {
	if request.Action == "implement" {
		if e.DelegationWorktrees == nil || strings.TrimSpace(e.Home) == "" {
			// No per-delegation worktree isolation is available (the engine lacks a
			// Home/DelegationWorktrees manager), so the child falls back to a
			// shared-checkout branch lock. Emit a parent event so the loss of
			// isolation is observable rather than silent.
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_worktree_skipped",
				Message: fmt.Sprintf("delegation %q implement runs in the shared checkout on branch %s: per-delegation worktree isolation unavailable", request.DelegationID, request.Branch),
			})
			if err := e.ensureBranchLock(ctx, request.Repo, request.Branch, request.Agent, ref); err != nil {
				return err
			}
		} else {
			result, err := e.AllocateDelegationWorktree(ctx, DelegationWorktreeRequest{
				Home:         e.Home,
				Repo:         request.Repo,
				ParentJobID:  job.ID,
				DelegationID: request.DelegationID,
				Delegation:   d,
				BaseBranch:   payload.Branch,
				Owner:        request.Agent,
				Checkout:     e.DelegationCheckout,
				RetryAttempt: request.RetryCount,
			}, e.DelegationWorktrees)
			if err != nil {
				var blocked BlockedError
				if errors.As(err, &blocked) {
					return e.block(ctx, ref, blocked.Reason)
				}
				return err
			}
			request.Branch = result.Branch
			request.WorktreePath = result.Path
			// The freshly-allocated worktree is created off the parent's base
			// branch, whose tip may have advanced past the HeadSHA the child
			// inherited from the parent payload. validateTargetCheckout (daemon)
			// compares the worktree HEAD against payload.HeadSHA and would
			// spuriously reject the child on a moving parent branch. Clear the
			// inherited HeadSHA so the child validates against its own fresh
			// worktree HEAD instead of a stale parent SHA.
			request.HeadSHA = ""
		}
	} else if integration, err := e.resolveIntegrationDeps(ctx, job, payload, d); err != nil {
		return err
	} else if len(integration.unresolvedDeps) > 0 {
		// Fail closed (#19): this read-only delegation depends on implement legs that
		// are not safely readable from either the parent base checkout or branch-backed
		// integration. Falling through to BASE here would make the reviewer judge code
		// WITHOUT the implemented change. A zero-branch resolution is allowed only when
		// every implement dep is already on the parent base branch.
		unresolved := strings.Join(integration.unresolvedDeps, ", ")
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_integration_unresolved",
			Message: fmt.Sprintf("delegation %q depends on unresolved implement leg(s) %s; refusing to review the base checkout", request.DelegationID, unresolved),
		})
		return e.block(ctx, ref, fmt.Sprintf("delegation %q depends on unresolved implement leg(s) %s; refusing to review the base checkout", request.DelegationID, unresolved))
	} else if len(integration.branches) > 0 {
		// This read-only delegation (e.g. a decompose-and-verify verify gate) depends
		// on succeeded implement legs that each live on their own branch. Merge them
		// into one detached worktree so the dependent sees the combined work instead
		// of the base checkout (#332).
		if manager, ok := e.DelegationWorktrees.(IntegrationWorktreeManager); !ok || strings.TrimSpace(e.Home) == "" {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_worktree_skipped",
				Message: fmt.Sprintf("delegation %q runs against the base checkout: integration worktree unavailable", request.DelegationID),
			})
		} else {
			path, err := e.AllocateIntegrationWorktree(ctx, DelegationWorktreeRequest{
				Home:         e.Home,
				Repo:         request.Repo,
				ParentJobID:  job.ID,
				DelegationID: request.DelegationID,
				BaseBranch:   payload.Branch,
				Checkout:     e.DelegationCheckout,
				RetryAttempt: request.RetryCount,
			}, integration.branches, manager)
			if err != nil {
				var blocked BlockedError
				if errors.As(err, &blocked) {
					return e.block(ctx, ref, blocked.Reason)
				}
				return err
			}
			request.WorktreePath = path
			// Validate against the integration worktree's own HEAD, not the inherited
			// parent HeadSHA (see isDelegationWorktreeChild).
			request.HeadSHA = ""
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_integrated",
				Message: fmt.Sprintf("delegation %q runs in an integration worktree merging %d implement leg(s)", request.DelegationID, len(integration.branches)),
			})
		}
	} else if readOnlyFanoutNeedsWorktree(payload, d) {
		// Read-only fan-out: >=2 read-only siblings share the parent repo and would
		// otherwise serialize on the repo:<repo> checkout key (only one runs per
		// daemon tick). Give this child its own detached, branch-lock-free worktree
		// so its checkout key is worktree:<path> and the siblings run concurrently.
		if manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager); !ok || strings.TrimSpace(e.Home) == "" {
			// Isolation is unavailable (no Home/worktree manager, or the manager
			// cannot create detached worktrees): the siblings fall back to the shared
			// checkout and serialize. Emit a parent event so the loss of parallelism
			// is observable rather than silent.
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_worktree_skipped",
				Message: fmt.Sprintf("delegation %q read-only fan-out runs serialized in the shared checkout: detached worktree isolation unavailable", request.DelegationID),
			})
		} else {
			path, err := e.AllocateReadOnlyDelegationWorktree(ctx, DelegationWorktreeRequest{
				Home:         e.Home,
				Repo:         request.Repo,
				ParentJobID:  job.ID,
				DelegationID: request.DelegationID,
				Delegation:   d,
				BaseBranch:   payload.Branch,
				Checkout:     e.DelegationCheckout,
				RetryAttempt: request.RetryCount,
			}, manager)
			if err != nil {
				var blocked BlockedError
				if errors.As(err, &blocked) {
					return e.block(ctx, ref, blocked.Reason)
				}
				return err
			}
			request.WorktreePath = path
			// The detached worktree is created at the parent base-branch tip, which
			// may have advanced past the inherited HeadSHA. Clear it so the child
			// validates against its own fresh worktree HEAD (see isDelegationWorktreeChild).
			request.HeadSHA = ""
			// The worktree is the committed tip: it omits gitignored (repos/**) and
			// uncommitted working-tree files. Point the child at the canonical base
			// checkout so an analysis task does not silently report working-tree state
			// as missing (#654). Appending to Instructions keeps the payload
			// deterministic for the idempotent-enqueue equality check, matching the
			// #419 upstream-context pattern. Scoped to this branch only: the
			// single-read-only, no-manager-fallback, and integration-worktree cases
			// must NOT be pointed at the base checkout.
			if note := readOnlyWorktreeContextNote(e.DelegationCheckout); note != "" {
				request.Instructions += note
			}
		}
	}

	if err := e.enqueue(ctx, request); err != nil {
		return fmt.Errorf("dispatch delegation %q: %w", request.DelegationID, err)
	}
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_enqueued",
		Message: fmt.Sprintf("delegation %q enqueued as job %s", request.DelegationID, request.ID),
	})
	return nil
}

// requeueDelegation re-enqueues a failed delegation child as a fresh job when
// the delegation has retry budget left (failedChild's RetryCount < d.Retry). The
// retry uses a distinct .../retry/<n> id so the enqueue idempotency path does not
// mistake it for the already-failed original, carries RetryCount+1 in its
// payload, and inherits the same DelegationID so it represents the same node in
// the delegation graph. upstreamContext is the #419 "Upstream dependency results"
// block (or "" when InjectUpstreamDepContext is off); the caller computes it from
// the current children/dedupWinners so a retried dependent re-receives the same
// upstream block its first attempt got rather than running blind. It returns
// whether a retry was enqueued.
func (e Engine) requeueDelegation(ctx context.Context, parentJob db.Job, parentPayload JobPayload, d Delegation, failedChild db.Job, artifactDir string, upstreamContext string, ref taskRef) (bool, error) {
	if d.Retry <= 0 {
		return false, nil
	}
	attempt := delegationJobRetryCount(failedChild)
	if attempt >= d.Retry {
		return false, nil
	}
	next := attempt + 1

	request := e.delegationRequest(parentJob, parentPayload, d)
	request.ID = parentJob.ID + "/delegation/" + d.ID + "/retry/" + strconv.Itoa(next)
	request.RetryCount = next
	request.DelegationArtifactDir = artifactDir
	// Re-inject the #419 "Upstream dependency results" block so a retried dependent
	// runs WITH its upstream deps' results, exactly as its first attempt did.
	// upstreamContext is "" unless Engine.InjectUpstreamDepContext is set, so the
	// flag-off retry prompt is byte-identical to before this field existed.
	if upstreamContext != "" {
		request.Instructions = request.Instructions + upstreamContext
	}

	if err := e.allocateAndEnqueueDelegation(ctx, parentJob, parentPayload, d, request, ref); err != nil {
		return false, err
	}
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    "delegation_retry",
		Message: fmt.Sprintf("delegation %q retry %d/%d enqueued as job %s after %s failed", d.ID, next, d.Retry, request.ID, failedChild.ID),
	})
	return true, nil
}

// advanceDelegations runs on a parent coordinator job each time one of its
// delegated children finishes. It enqueues now-ready dependent siblings,
// applies the failed delegation's failure_policy, and enqueues a single
// coordinator continuation job once every top-level sibling has reached a
// terminal state. It is idempotent: deferred dependents and the continuation
// job use deterministic ids, so concurrent child completions cannot double
// enqueue.
func (e Engine) advanceDelegations(ctx context.Context, parentJob db.Job, parentPayload JobPayload, parentResult *AgentResult, ref taskRef) error {
	if parentResult == nil || len(parentResult.Delegations) == 0 {
		return nil
	}

	children, err := e.childDelegationJobs(ctx, parentJob.ID)
	if err != nil {
		return err
	}

	// Parent job events drive two decisions below: which delegations dispatch
	// skipped via fingerprint dedup (so they resolve against their winning
	// sibling rather than stalling the continuation forever), and whether a
	// continuation has already been enqueued (so a late block_parent failure does
	// not contradict it).
	parentEvents, err := e.Store.ListJobEvents(ctx, parentJob.ID)
	if err != nil {
		return err
	}
	dedupWinners := dedupedDelegationWinners(parentResult.Delegations, children, parentEvents)

	// Resolve the shared artifact directory the same way dispatchDelegations did
	// so late-running dependents reference the same brief.md/context-manifest.
	artifactDir, err := delegationArtifactDir(e.ArtifactRoot, parentJob.ID, parentResult)
	if err != nil {
		return err
	}

	// #438: structured sibling of the #419 prose block. Now that a child has
	// finished, augment context-manifest.json with the result-reference fields for
	// every SUCCEEDED delegation so a downstream reader can reference an upstream
	// output by structured JSON rather than re-parsing the prose block. The
	// just-finished child is already in the children map read above, so the enriched
	// view reflects the current succeeded set (later re-reads in this pass only add
	// newly-enqueued, not-yet-succeeded dependents). It is gated on
	// InjectUpstreamDepContext so the flag-off path never re-writes the manifest and
	// stays byte-identical to today, and the write is idempotent (deterministic,
	// sorted JSON) so repeated passes over a stable succeeded set produce no churn.
	// Best-effort like the dispatch artifact write: a manifest write failure must
	// not block draining ready dependents.
	if e.InjectUpstreamDepContext {
		if augmentedDir, augErr := augmentDelegationManifest(e.ArtifactRoot, parentJob.ID, parentResult, children, dedupWinners, e); augErr != nil {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   parentJob.ID,
				Kind:    "delegation_manifest_augment_failed",
				Message: fmt.Sprintf("augment delegation manifest: %v", augErr),
			})
		} else if augmentedDir != "" {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   parentJob.ID,
				Kind:    "delegation_manifest_augmented",
				Message: fmt.Sprintf("delegation manifest augmented at %s", augmentedDir),
			})
		}
	}

	// Retry pass: a delegation that failed but has retry budget left is
	// re-enqueued as a fresh child before any failure_policy is applied, so its
	// failure is absorbed by the retry rather than blocking/escalating. A
	// successful retry replaces the failed attempt in the children map (the retry
	// id sorts after the original), so the delegation looks active again this
	// pass and the failure switch below skips it.
	retried := false
	for _, d := range parentResult.Delegations {
		child, ok := children[d.ID]
		if !ok || !IsSettledJobState(child.State) || child.State == string(JobSucceeded) {
			continue
		}
		// #419: a retried dependent must not run blind to its upstream deps. The
		// first attempt got the "Upstream dependency results" block injected at its
		// enqueue point in advanceDelegations, but a retry re-enqueues from
		// requeueDelegation, so re-inject the same block here (resolved against the
		// current children/dedupWinners, which still hold the succeeded deps).
		// Gated on InjectUpstreamDepContext so the flag-off retry prompt is
		// byte-identical to before this change.
		upstreamContext := ""
		if e.InjectUpstreamDepContext {
			upstreamContext = e.buildUpstreamDepBlock(d, children, dedupWinners)
		}
		didRetry, err := e.requeueDelegation(ctx, parentJob, parentPayload, d, child, artifactDir, upstreamContext, ref)
		if err != nil {
			return err
		}
		if didRetry {
			retried = true
		}
	}
	if retried {
		children, err = e.childDelegationJobs(ctx, parentJob.ID)
		if err != nil {
			return err
		}
		// Re-read events too: a delegation deduped during this pass emits a new
		// delegation_deduped event, and dedupedDelegationWinners derives the
		// deduped set from events. Reusing the stale snapshot would hide it.
		parentEvents, err = e.Store.ListJobEvents(ctx, parentJob.ID)
		if err != nil {
			return err
		}
		dedupWinners = dedupedDelegationWinners(parentResult.Delegations, children, parentEvents)
	}

	// Apply failure policies first so a failed dependency stops its dependents
	// (or escalates/blocks) before we try to enqueue anything new. Once a
	// continuation has already been enqueued (e.g. an earlier escalate fired it),
	// a later block_parent sibling failure must not block the shared parent task:
	// that would leave a blocked task AND an in-flight continuation, a
	// contradictory end state the continuation prompt does not reflect. The
	// continuation already carries every child outcome, so we fold the block into
	// it by skipping the block here.
	continuationAlreadyEnqueued := continuationEnqueued(parentEvents)
	escalate := false
	for _, d := range parentResult.Delegations {
		child, ok := children[d.ID]
		if !ok || !IsSettledJobState(child.State) || child.State == string(JobSucceeded) {
			continue
		}
		switch delegationFailurePolicy(d) {
		case "continue":
			// Independent ready siblings still run; only this branch's
			// dependents are skipped (handled by depsSatisfied below).
			continue
		case "escalate":
			escalate = true
		case "escalate_human":
			// Durable human-in-the-loop pause (#340): set TaskAwaitingHuman,
			// record the one-shot escalation event, notify the human best-effort,
			// and return an AwaitingHumanError so NO continuation is enqueued and
			// the tree consumes zero compute until an operator resumes it. As with
			// block_parent, once a continuation is already in flight (an earlier
			// escalate sibling fired it) the tree is already proceeding
			// autonomously, so fold this leg in rather than contradict it.
			if continuationAlreadyEnqueued {
				continue
			}
			return e.pauseAwaitingHuman(ctx, parentJob, parentPayload, ref, d, child)
		default: // block_parent (also the empty default)
			if continuationAlreadyEnqueued {
				continue
			}
			return e.block(ctx, ref, fmt.Sprintf("delegation %q failed (failure_policy block_parent): %s", d.ID, childFailureReason(child)))
		}
	}

	// Enqueue deferred dependents whose deps have all succeeded. Skip any whose
	// dependency failed under a continue policy (depsSatisfied returns false
	// unless every dep is succeeded). Already-enqueued children are skipped via
	// the children map and e.enqueue's idempotency.
	if !escalate {
		for _, d := range parentResult.Delegations {
			if len(compactStrings(d.Deps)) == 0 {
				continue
			}
			if _, exists := children[d.ID]; exists {
				continue
			}
			// A deduped delegation folds into its winning sibling and must never
			// get its own child, even if its deps are now satisfied.
			if _, deduped := dedupWinners[d.ID]; deduped {
				continue
			}
			if !depsSatisfied(d.Deps, children, dedupWinners) {
				continue
			}
			// #419: make deps[] real dataflow. This dependent is enqueued exactly
			// here, after every dep succeeded and while children/dedupWinners hold
			// the dep payloads, so inject the succeeded direct deps' results into
			// its prompt. Gated behind InjectUpstreamDepContext so the flag-off
			// enqueued instructions are byte-identical to before this change.
			upstreamContext := ""
			if e.InjectUpstreamDepContext {
				upstreamContext = e.buildUpstreamDepBlock(d, children, dedupWinners)
			}
			// No per-root job-budget re-check here: the whole batch (ready +
			// deferred) was pre-authorized as one unit by the #649 projected
			// admission in dispatchDelegations (projectedNewDelegationJobs counts
			// deferred legs), so this deferred enqueue is intentionally uncapped —
			// a late refusal here would strand a deferred leg whose refused
			// sibling deps can never satisfy.
			if err := e.enqueueDelegation(ctx, parentJob, parentPayload, d, artifactDir, upstreamContext, ref); err != nil {
				return err
			}
			// Re-read children AND events so a second satisfied dependent in the
			// same pass is not mistaken for still-pending, and a delegation
			// deduped by the enqueueDelegation call above (which emits a fresh
			// delegation_deduped event) is observed by the final
			// allDelegationsResolved check below rather than stalling forever.
			children, err = e.childDelegationJobs(ctx, parentJob.ID)
			if err != nil {
				return err
			}
			parentEvents, err = e.Store.ListJobEvents(ctx, parentJob.ID)
			if err != nil {
				return err
			}
			dedupWinners = dedupedDelegationWinners(parentResult.Delegations, children, parentEvents)
		}
	}

	// Enqueue the coordinator continuation job once every top-level delegation
	// is resolved (terminal child, or permanently gated by a failed dependency
	// under a continue policy), or immediately when a failure escalates.
	if escalate || allDelegationsResolved(parentResult.Delegations, children, dedupWinners) {
		if err := e.maybeEnqueueContinuation(ctx, parentJob, parentPayload, parentResult, children, ref); err != nil {
			return err
		}
	}
	return nil
}

// continuationConfig holds the optional inputs threaded into a coordinator
// continuation (#445). It is empty by default so every existing call site builds
// the byte-identical continuation it always did.
type continuationConfig struct {
	// humanAnswer, when non-empty, is the rendered ask-gate answer block injected
	// at the top of the NORMAL continuation prompt so the resumed coordinator reads
	// the human's decision (the answer-driven resume path only ever reaches the
	// normal continuation, never the loop/verify-replan corrective ones).
	humanAnswer string
}

// continuationOption configures a maybeEnqueueContinuation call without changing
// the byte-identical default (no options) path.
type continuationOption func(*continuationConfig)

// withHumanAnswer threads a rendered ask-gate answer block into the continuation
// prompt (#445).
func withHumanAnswer(answer string) continuationOption {
	return func(c *continuationConfig) { c.humanAnswer = answer }
}

// maybeEnqueueContinuation enqueues exactly one coordinator continuation job for
// a parent whose delegations have all finished. Idempotency is enforced by a
// deterministic continuation id plus a one-shot delegation_continuation_enqueued
// event on the parent, so concurrent child completions enqueue it at most once.
// When any delegation declares synthesis_rule "vote", the continuation is gated:
// the parent task is blocked unless every child was approved/succeeded.
func (e Engine) maybeEnqueueContinuation(ctx context.Context, parentJob db.Job, parentPayload JobPayload, parentResult *AgentResult, children map[string]db.Job, ref taskRef, opts ...continuationOption) error {
	var cfg continuationConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	events, err := e.Store.ListJobEvents(ctx, parentJob.ID)
	if err != nil {
		return err
	}
	if continuationEnqueued(events) {
		return nil
	}

	childPayloads := make(map[string]JobPayload, len(children))
	for id, child := range children {
		childPayload, err := unmarshalPayload(child.Payload)
		if err != nil {
			return err
		}
		childPayloads[id] = childPayload
	}

	// Goal-anchor the continuation (#418): resolve the user's ORIGINAL goal from
	// the ROOT coordinator (depth-stable; parentPayload.Instructions is the parent
	// continuation's built prompt for a nested generation) so every variant below
	// restates the same intent. A pruned/empty root falls back to the parent's
	// instructions and, ultimately, an empty goal that omits the header.
	goal := e.originalGoal(ctx, e.rootJobID(parentJob, parentPayload), parentPayload.Instructions)

	// synthesis_rule "vote": block the parent unless every child approved or
	// succeeded. The default ("" / "summary") concatenates child summaries into
	// the continuation prompt below.
	if delegationSynthesisRequiresVote(parentResult.Delegations) && !delegationVoteSatisfied(parentResult.Delegations, children, childPayloads) {
		return e.block(ctx, ref, fmt.Sprintf("delegation synthesis_rule vote failed: not all delegated children for %s were approved/succeeded", parentJob.ID))
	}

	// synthesis_rule "quorum": block the parent unless at least K children
	// reached an approving outcome (succeeded state or an approving decision).
	if delegationSynthesisRequiresQuorum(parentResult.Delegations) {
		k := delegationQuorumThreshold(parentResult.Delegations)
		if !delegationQuorumSatisfied(parentResult.Delegations, children, childPayloads, k) {
			return e.block(ctx, ref, fmt.Sprintf("delegation synthesis_rule quorum failed: fewer than %d delegated children for %s were approved/succeeded", k, parentJob.ID))
		}
	}

	// Result-aware non-progress detection (#339). The structural fast-path
	// (handleDelegationLoop) only catches a coordinator literally re-issuing the
	// same delegation SET; a coordinator that perturbs the set each round to evade
	// the set hash, yet whose children keep returning nothing new, slips past it.
	// Here — after every child has finished and childPayloads carry full results —
	// fold the generation's verifiable side effects into a progressDigest and
	// compare it to the previous generation's digest threaded through the payload.
	// No new durable side effect + an unchanged digest => the streak climbs; any
	// new side effect (a different decision, a new commit/change, a test run, a new
	// PR/HeadSHA, a changed artifact body) resets it to 0 even when the summary
	// text repeats. At the threshold the result-aware path trips the SAME ladder as
	// the structural check: a first trip emits delegation_loop_warning and a
	// corrective continuation; a trip after a corrective nudge has already fired
	// (DelegationRepeatCount >= 1) emits delegation_loop_detected and routes to the
	// #305 graceful finalize continuation. Both reuse the existing once-guards
	// (the continuationEnqueued top-guard above + enqueueFinalizeContinuation's own
	// guard) so re-advance never double-fires.
	digest := progressDigest(parentResult.Delegations, childPayloads)
	nonProgressStreak := 0
	if digest == parentPayload.LastProgressDigest {
		// The previous generation recorded this exact digest and this generation
		// reproduced it: no new durable side effect => the streak climbs.
		nonProgressStreak = parentPayload.NonProgressStreak + 1
	}
	if nonProgressStreak >= e.nonProgressStreakThreshold() {
		if parentPayload.DelegationRepeatCount >= 1 {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   parentJob.ID,
				Kind:    "delegation_loop_detected",
				Message: fmt.Sprintf("delegation tree made no new durable side effect for %d consecutive generations after a corrective nudge (digest %s); finalizing instead of continuing", nonProgressStreak, digest),
			})
			// Graceful finalize (#305): give the coordinator one terminal continuation
			// to synthesize a best-effort result rather than stopping silently.
			return e.enqueueFinalizeContinuation(ctx, parentJob, parentPayload, "delegation tree made no progress after a corrective nudge")
		}

		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   parentJob.ID,
			Kind:    "delegation_loop_warning",
			Message: fmt.Sprintf("delegation tree made no new durable side effect for %d consecutive generations (digest %s); sending a corrective continuation instead of continuing", nonProgressStreak, digest),
		})
		correctiveRequest := JobRequest{
			ID:     delegationContinuationID(parentJob.ID),
			Agent:  parentJob.Agent,
			Action: "ask",
			Model:  parentPayload.Model,
			// Per-job runtime override (#531): the corrective continuation stays on
			// the override runtime/ref, like Model (see maybeEnqueueContinuation).
			RuntimeOverride:    parentPayload.RuntimeOverride,
			RuntimeOverrideRef: parentPayload.RuntimeOverrideRef,
			Phase:              parentPayload.Phase,
			Repo:               parentPayload.Repo,
			Branch:             parentPayload.Branch,
			PullRequest:        parentPayload.PullRequest,
			HeadSHA:            parentPayload.HeadSHA,
			GoalID:             parentPayload.GoalID,
			TaskID:             parentPayload.TaskID,
			TaskTitle:          parentPayload.TaskTitle,
			LeadAgent:          parentPayload.LeadAgent,
			Reviewers:          parentPayload.Reviewers,
			Sender:             parentJob.Agent,
			Instructions:       buildCorrectiveContinuationPrompt(goal, parentResult),
			Constraints:        parentPayload.Constraints,
			ParentJobID:        parentJob.ID,
			DelegationDepth:    parentPayload.DelegationDepth + 1,
			DelegatedBy:        parentJob.Agent,
			RootJobID:          e.rootJobID(parentJob, parentPayload),
			// Carry the window forward and mark that a corrective nudge has fired so a
			// further non-progress generation escalates to delegation_loop_detected.
			RecentDelegationHashes: appendDelegationHashWindow(parentPayload.RecentDelegationHashes, canonicalDelegationSetHash(parentResult.Delegations)),
			DelegationRepeatCount:  parentPayload.DelegationRepeatCount + 1,
			// Thread the non-progress streak forward: if the coordinator's corrective
			// continuation still produces no new side effect, the streak stays at or
			// above the threshold and the next generation escalates.
			NonProgressStreak:  nonProgressStreak,
			LastProgressDigest: digest,
			Cockpit:            parentPayload.Cockpit,
			CockpitSession:     parentPayload.CockpitSession,
			CockpitPaneKey:     parentPayload.CockpitPaneKey,
		}
		if err := e.enqueue(ctx, correctiveRequest); err != nil {
			return fmt.Errorf("enqueue corrective continuation for %q: %w", parentJob.ID, err)
		}
		// The corrective continuation IS the coordinator's single continuation, so it
		// occupies the continuation slot: emit delegation_continuation_enqueued so a
		// re-advance hits the continuationEnqueued top-guard rather than re-running
		// the streak logic.
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   parentJob.ID,
			Kind:    "delegation_continuation_enqueued",
			Message: fmt.Sprintf("corrective continuation occupies the continuation slot for job %s", correctiveRequest.ID),
		})
		return nil
	}

	// synthesis_rule "verify" (#439): unlike vote/quorum (which BLOCK on failure),
	// verify derives a pass/fail VERDICT from the verify-tagged legs and, on a
	// FAILED verdict, enqueues a BOUNDED corrective "replan" continuation so the
	// coordinator can self-correct — the verify→replan loop is enforced by the
	// engine rather than left to the coordinator. The verdict is mechanical: any
	// verify-tagged leg that did NOT reach an approving outcome (the same test
	// vote/quorum use) fails it (a missing verify child also fails). On a PASSED
	// verdict this falls through to the normal synthesis continuation below,
	// byte-identical to the pre-change path. The loop is bounded by a dedicated
	// per-root VerifyAttempt cap: below the cap a verify_replan_warning + corrective
	// replan continuation fire; at/over the cap it routes to the #305 graceful
	// finalize continuation (verify_replan_exhausted) like every other backstop.
	// This sits AFTER the continuationEnqueued top-guard and the non-progress check
	// and emits delegation_continuation_enqueued for its own request, so it occupies
	// the single continuation slot and a re-advance never double-enqueues.
	// dedupWinners mirrors advanceDelegations: a fingerprint-deduped verify leg
	// never owns its own child, so the verdict must resolve it against its winning
	// sibling rather than reading the absent child as a failed verdict.
	dedupWinners := dedupedDelegationWinners(parentResult.Delegations, children, events)
	if delegationSynthesisRequiresVerify(parentResult.Delegations) && !verifyVerdictPassed(parentResult.Delegations, children, childPayloads, dedupWinners) {
		attemptCap := e.verifyReplanAttemptCap()
		if parentPayload.VerifyAttempt >= attemptCap {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   parentJob.ID,
				Kind:    "verify_replan_exhausted",
				Message: fmt.Sprintf("verify→replan attempt cap of %d reached for job %s; finalizing instead of replanning", attemptCap, parentJob.ID),
			})
			// Graceful finalize (#305): give the coordinator one terminal continuation
			// to synthesize a best-effort result rather than looping on a failed verdict.
			return e.enqueueFinalizeContinuation(ctx, parentJob, parentPayload, fmt.Sprintf("verify→replan attempt cap of %d reached", attemptCap))
		}

		attempt := parentPayload.VerifyAttempt + 1
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   parentJob.ID,
			Kind:    "verify_replan_warning",
			Message: fmt.Sprintf("independent verification failed (attempt %d/%d); sending a corrective replan continuation for job %s", attempt, attemptCap, parentJob.ID),
		})
		replanRequest := JobRequest{
			ID:     delegationContinuationID(parentJob.ID),
			Agent:  parentJob.Agent,
			Action: "ask",
			Model:  parentPayload.Model,
			// Per-job runtime override (#531): the replan continuation stays on the
			// override runtime/ref, like Model (see maybeEnqueueContinuation).
			RuntimeOverride:    parentPayload.RuntimeOverride,
			RuntimeOverrideRef: parentPayload.RuntimeOverrideRef,
			Phase:              parentPayload.Phase,
			Repo:               parentPayload.Repo,
			Branch:             parentPayload.Branch,
			PullRequest:        parentPayload.PullRequest,
			HeadSHA:            parentPayload.HeadSHA,
			GoalID:             parentPayload.GoalID,
			TaskID:             parentPayload.TaskID,
			TaskTitle:          parentPayload.TaskTitle,
			LeadAgent:          parentPayload.LeadAgent,
			Reviewers:          parentPayload.Reviewers,
			Sender:             parentJob.Agent,
			Instructions:       buildVerifyReplanContinuationPrompt(goal, parentResult, children, childPayloads, attempt, attemptCap),
			Constraints:        parentPayload.Constraints,
			ParentJobID:        parentJob.ID,
			DelegationDepth:    parentPayload.DelegationDepth + 1,
			DelegatedBy:        parentJob.Agent,
			RootJobID:          e.rootJobID(parentJob, parentPayload),
			// Consume one verify attempt so a still-failing verdict next generation
			// climbs toward the cap and eventually finalizes.
			VerifyAttempt: attempt,
			// Carry the non-progress carry-forward fields so a genuine corrective replan
			// is not misclassified as a loop: record this generation's progressDigest and
			// thread the (sub-threshold) streak/window forward exactly like the normal
			// continuation below. A real verdict-driven replan IS a new generation.
			RecentDelegationHashes: appendDelegationHashWindow(parentPayload.RecentDelegationHashes, canonicalDelegationSetHash(parentResult.Delegations)),
			DelegationRepeatCount:  0,
			NonProgressStreak:      nonProgressStreak,
			LastProgressDigest:     digest,
			Cockpit:                parentPayload.Cockpit,
			CockpitSession:         parentPayload.CockpitSession,
			CockpitPaneKey:         parentPayload.CockpitPaneKey,
		}
		if err := e.enqueue(ctx, replanRequest); err != nil {
			return fmt.Errorf("enqueue verify replan continuation for %q: %w", parentJob.ID, err)
		}
		// The replan continuation IS the coordinator's single continuation, so it
		// occupies the continuation slot: emit delegation_continuation_enqueued so a
		// re-advance hits the continuationEnqueued top-guard rather than re-running
		// the verify gate (and never double-enqueues a normal continuation).
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   parentJob.ID,
			Kind:    "delegation_continuation_enqueued",
			Message: fmt.Sprintf("verify replan continuation occupies the continuation slot for job %s", replanRequest.ID),
		})
		return nil
	}

	request := JobRequest{
		ID:     delegationContinuationID(parentJob.ID),
		Agent:  parentJob.Agent,
		Action: "ask",
		Model:  parentPayload.Model,
		// Per-job runtime override (#531): a continuation is the SAME logical
		// coordinator run, so it must stay on the override runtime — dropping the
		// override here would run the continuation as the default agent, resuming
		// (and writing into) the agent's default-runtime session AND leaking the
		// override-runtime --model onto the default runtime's CLI. Reusing the
		// parent's ref is safe: continuation generations are strictly sequential
		// (one deterministic continuation id per parent), and adapters treat a
		// fresh ref as "start a brand-new session" on every delivery.
		RuntimeOverride:    parentPayload.RuntimeOverride,
		RuntimeOverrideRef: parentPayload.RuntimeOverrideRef,
		Phase:              parentPayload.Phase,
		Repo:               parentPayload.Repo,
		Branch:             parentPayload.Branch,
		PullRequest:        parentPayload.PullRequest,
		HeadSHA:            parentPayload.HeadSHA,
		GoalID:             parentPayload.GoalID,
		TaskID:             parentPayload.TaskID,
		TaskTitle:          parentPayload.TaskTitle,
		LeadAgent:          parentPayload.LeadAgent,
		Reviewers:          parentPayload.Reviewers,
		Sender:             parentJob.Agent,
		// Budget-pressure nudge (#418): when the tree is near a depth/job bound, bias
		// the coordinator toward synthesizing now over more fan-out. The job count is
		// best-effort — a lookup error yields 0, which suppresses only the job clause,
		// never the continuation.
		Instructions: e.buildContinuationPrompt(goal, budgetPressureLine(parentPayload.DelegationDepth+1, e.rootJobCountForPressure(ctx, e.rootJobID(parentJob, parentPayload))), parentResult, children, childPayloads, cfg.humanAnswer),
		Constraints:  parentPayload.Constraints,
		ParentJobID:  parentJob.ID,
		// Ask-gate answer (#445): carry the human's answer block on the continuation
		// for durability/observability; buildContinuationPrompt above already rendered
		// it at the top of the prompt. Empty (the default) for every non-answer path,
		// so omitempty keeps the stored payload byte-identical.
		HumanAnswer: cfg.humanAnswer,
		// Increment depth per continuation generation so a coordinator whose
		// continuation re-delegates is bounded by MaxDelegationDepth instead of
		// looping forever (the continuation reused the parent's depth before).
		DelegationDepth: parentPayload.DelegationDepth + 1,
		DelegatedBy:     parentJob.Agent,
		// Share the originating coordinator's root so the whole continuation
		// chain counts against one per-root budget and is visible to loop detection.
		RootJobID: e.rootJobID(parentJob, parentPayload),
		// Record the delegation set that was actually dispatched in the sliding
		// window so the next generation can detect a non-progress repeat. A real
		// dispatch happened => progress, so reset the repeat counter; the
		// corrective-nudge counter only climbs while the coordinator loops.
		RecentDelegationHashes: appendDelegationHashWindow(parentPayload.RecentDelegationHashes, canonicalDelegationSetHash(parentResult.Delegations)),
		DelegationRepeatCount:  0,
		// Result-aware non-progress carry-forward (#339): record this generation's
		// progressDigest so the next generation can detect a non-progress repeat, and
		// thread the (sub-threshold) streak forward. nonProgressStreak is 0 whenever
		// this generation produced a new durable side effect, so genuine progress
		// always resets the streak even when the self-reported summary repeats.
		NonProgressStreak:  nonProgressStreak,
		LastProgressDigest: digest,
		// Inherit the coordinator's cockpit settings so the continuation renders
		// its pane under the same workspace/session as the rest of the tree.
		Cockpit:        parentPayload.Cockpit,
		CockpitSession: parentPayload.CockpitSession,
		CockpitPaneKey: parentPayload.CockpitPaneKey,
	}
	if err := e.enqueue(ctx, request); err != nil {
		return fmt.Errorf("enqueue continuation for %q: %w", parentJob.ID, err)
	}
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    "delegation_continuation_enqueued",
		Message: fmt.Sprintf("delegation continuation enqueued as job %s", request.ID),
	})
	return nil
}

// childDelegationJobs returns the direct delegation children of a parent job,
// keyed by delegation id. There is no ListJobsByParent store query, so this
// filters ListJobs on ParentJobID (mirroring latestReviewRound's list+filter).
func (e Engine) childDelegationJobs(ctx context.Context, parentJobID string) (map[string]db.Job, error) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	children := make(map[string]db.Job)
	attempts := make(map[string]int)
	for _, job := range jobs {
		if job.ParentJobID != parentJobID || strings.TrimSpace(job.DelegationID) == "" {
			continue
		}
		// A delegation may have several attempts after retries; keep the latest
		// (highest RetryCount) so the failure/resolution logic always observes the
		// current attempt regardless of ListJobs ordering.
		attempt := delegationJobRetryCount(job)
		if _, ok := children[job.DelegationID]; ok && attempt < attempts[job.DelegationID] {
			continue
		}
		children[job.DelegationID] = job
		attempts[job.DelegationID] = attempt
	}
	return children, nil
}

// delegationJobRetryCount reads a child job's RetryCount from its payload,
// returning 0 when the payload is missing or cannot be parsed.
func delegationJobRetryCount(job db.Job) int {
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return 0
	}
	return payload.RetryCount
}

// delegationFingerprintSeen reports whether a sibling delegation under the same
// parent (other than skipDelegationID) has already been enqueued with the given
// fingerprint. It scans ListJobs filtered by ParentJobID, mirroring
// childDelegationJobs, and compares each child's stored payload.Fingerprint so
// dedup is scoped per the goal's (parentJobID, fingerprint) key.
func (e Engine) delegationFingerprintSeen(ctx context.Context, parentJobID, skipDelegationID, fingerprint string) (bool, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return false, nil
	}
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if job.ParentJobID != parentJobID || strings.TrimSpace(job.DelegationID) == "" {
			continue
		}
		if job.DelegationID == skipDelegationID {
			continue
		}
		childPayload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(childPayload.Fingerprint) == fingerprint {
			return true, nil
		}
	}
	return false, nil
}

// delegationFingerprintKey hashes (parentJobID, fingerprint) into a stable,
// parent-scoped dedup key, mirroring jobID's fnv hashing so identical
// fingerprints under different parents never collide.
func delegationFingerprintKey(parentJobID, fingerprint string) string {
	hash := fnv.New64a()
	for _, value := range []string{parentJobID, fingerprint} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return "deleg-fp-" + strconv.FormatUint(hash.Sum64(), 36)
}

// dedupedDelegationWinners maps each fingerprint-deduped delegation id to the
// winning sibling's child job: the same-fingerprint delegation that DID get a
// child (and is therefore the canonical attempt the deduped node folds into). A
// deduped delegation produces no child of its own (enqueueDelegation returns
// early after a delegation_deduped event), so without this mapping
// allDelegationsResolved/depsSatisfied would treat it as forever-active and stall
// the coordinator continuation. The deduped set is taken from the parent's
// recorded delegation_deduped events so it reflects exactly what dispatch skipped,
// and each deduped id is resolved to its winner by fingerprint among siblings that
// own a child.
func dedupedDelegationWinners(delegations []Delegation, children map[string]db.Job, events []db.JobEvent) map[string]db.Job {
	deduped := dedupedDelegationIDs(delegations, events)
	if len(deduped) == 0 {
		return nil
	}
	// Map each fingerprint to the winning sibling's child (the same-fingerprint
	// delegation that owns a child). Delegation order is deterministic, so the
	// first such sibling is a stable winner.
	winnerByFingerprint := make(map[string]db.Job)
	for _, d := range delegations {
		fingerprint := strings.TrimSpace(d.Fingerprint)
		if fingerprint == "" {
			continue
		}
		if _, taken := winnerByFingerprint[fingerprint]; taken {
			continue
		}
		if child, ok := children[d.ID]; ok {
			winnerByFingerprint[fingerprint] = child
		}
	}
	winners := make(map[string]db.Job)
	for _, d := range delegations {
		if !deduped[d.ID] {
			continue
		}
		if winner, ok := winnerByFingerprint[strings.TrimSpace(d.Fingerprint)]; ok {
			winners[d.ID] = winner
		}
	}
	if len(winners) == 0 {
		return nil
	}
	return winners
}

// dedupedDelegationIDs returns the set of delegation ids that dispatch skipped
// via fingerprint dedup, by matching each known delegation against the parent's
// recorded delegation_deduped event messages. enqueueDelegation formats those
// messages as `delegation %q skipped: ...`, so a delegation is deduped when an
// event message carries its %q-quoted id as the prefix. Reconstructing the
// quoted prefix per delegation (rather than parsing the id out of the message)
// keeps the match exact even for ids containing quotes or backslashes.
func dedupedDelegationIDs(delegations []Delegation, events []db.JobEvent) map[string]bool {
	var dedupedMessages []string
	for _, event := range events {
		if event.Kind == "delegation_deduped" {
			dedupedMessages = append(dedupedMessages, event.Message)
		}
	}
	if len(dedupedMessages) == 0 {
		return nil
	}
	var ids map[string]bool
	for _, d := range delegations {
		prefix := fmt.Sprintf("delegation %q skipped:", d.ID)
		for _, message := range dedupedMessages {
			if strings.HasPrefix(message, prefix) {
				if ids == nil {
					ids = make(map[string]bool)
				}
				ids[d.ID] = true
				break
			}
		}
	}
	return ids
}

// depsSatisfied reports whether every dependency id maps to a succeeded sibling.
// An unknown dep id (not yet a child, or never created) is never satisfied, so a
// failed or missing dependency keeps the dependent gated rather than enqueuing
// it prematurely. A dep that points at a fingerprint-deduped delegation is
// resolved against its winning sibling: satisfied iff that sibling succeeded.
func depsSatisfied(deps []string, children map[string]db.Job, dedupWinners map[string]db.Job) bool {
	for _, dep := range compactStrings(deps) {
		if winner, ok := dedupWinners[dep]; ok {
			if winner.State != string(JobSucceeded) {
				return false
			}
			continue
		}
		child, ok := children[dep]
		if !ok || child.State != string(JobSucceeded) {
			return false
		}
	}
	return true
}

// allDelegationsResolved reports whether every top-level delegation has reached
// a final disposition: either a terminal child job, or no child at all because a
// dependency failed under a continue policy and the delegation can never run.
// queued/running children, or a not-yet-enqueued delegation whose deps are still
// in flight, mean the batch is still active and no continuation is enqueued yet.
func allDelegationsResolved(delegations []Delegation, children map[string]db.Job, dedupWinners map[string]db.Job) bool {
	byID := delegationsByID(delegations)
	for _, d := range delegations {
		if !delegationResolved(d, children, byID, dedupWinners) {
			return false
		}
	}
	return true
}

func delegationResolved(d Delegation, children map[string]db.Job, byID map[string]Delegation, dedupWinners map[string]db.Job) bool {
	if child, ok := children[d.ID]; ok {
		return IsSettledJobState(child.State)
	}
	// A fingerprint-deduped delegation never gets its own child; it is resolved
	// when its winning sibling (the same-fingerprint delegation that did get a
	// child) reaches a terminal state, so it cannot stall the continuation.
	if winner, ok := dedupWinners[d.ID]; ok {
		return IsSettledJobState(winner.State)
	}
	// No child job yet: the delegation is resolved only if it can never run
	// because one of its dependencies is permanently unrunnable.
	return delegationPermanentlyBlocked(d, children, byID, dedupWinners, map[string]bool{})
}

// delegationPermanentlyBlocked reports whether a not-yet-enqueued delegation can
// never run because a dependency terminally failed (or is itself permanently
// blocked). It guards against cycles via the visiting set, treating a delegation
// caught in a dependency cycle as blocked so the batch cannot deadlock. A dep
// that points at a fingerprint-deduped delegation is resolved against its
// winning sibling: a terminally-failed winner permanently blocks the dependent.
func delegationPermanentlyBlocked(d Delegation, children map[string]db.Job, byID map[string]Delegation, dedupWinners map[string]db.Job, visiting map[string]bool) bool {
	if visiting[d.ID] {
		return true
	}
	visiting[d.ID] = true
	defer delete(visiting, d.ID)
	for _, dep := range compactStrings(d.Deps) {
		if winner, ok := dedupWinners[dep]; ok {
			if IsSettledJobState(winner.State) && winner.State != string(JobSucceeded) {
				return true
			}
			continue
		}
		if child, ok := children[dep]; ok {
			if IsSettledJobState(child.State) && child.State != string(JobSucceeded) {
				return true
			}
			continue
		}
		depDel, ok := byID[dep]
		if !ok {
			// Unknown dependency id can never be satisfied.
			return true
		}
		if delegationPermanentlyBlocked(depDel, children, byID, dedupWinners, visiting) {
			return true
		}
	}
	return false
}

func delegationsByID(delegations []Delegation) map[string]Delegation {
	byID := make(map[string]Delegation, len(delegations))
	for _, d := range delegations {
		byID[d.ID] = d
	}
	return byID
}

func delegationFailurePolicy(d Delegation) string {
	policy := strings.ToLower(strings.TrimSpace(d.FailurePolicy))
	if policy == "" {
		return "block_parent"
	}
	return policy
}

// delegationFailureHandledByPolicy reports whether the named delegation declares
// a continue/escalate/escalate_human failure_policy, meaning a failure of its
// child is governed by the delegation graph (siblings keep running, the
// coordinator continuation absorbs it, or the tree pauses awaiting a human)
// rather than blocking the shared parent task.
func delegationFailureHandledByPolicy(parentResult *AgentResult, delegationID string) bool {
	if parentResult == nil || strings.TrimSpace(delegationID) == "" {
		return false
	}
	for _, d := range parentResult.Delegations {
		if d.ID != delegationID {
			continue
		}
		switch delegationFailurePolicy(d) {
		case "continue", "escalate", "escalate_human":
			return true
		default:
			return false
		}
	}
	return false
}

// delegationRetryPending reports whether the named delegation currently has a
// non-terminal child, meaning the retry pass re-enqueued a fresh attempt and the
// failed attempt's outcome is now superseded. childDelegationJobs already keeps
// the latest attempt per delegation id, so a queued/running retry shows here.
func (e Engine) delegationRetryPending(ctx context.Context, parentJobID, delegationID string) (bool, error) {
	if strings.TrimSpace(delegationID) == "" {
		return false, nil
	}
	children, err := e.childDelegationJobs(ctx, parentJobID)
	if err != nil {
		return false, err
	}
	child, ok := children[delegationID]
	if !ok {
		return false, nil
	}
	return !IsSettledJobState(child.State), nil
}

func childFailureReason(child db.Job) string {
	payload, err := unmarshalPayload(child.Payload)
	if err == nil && payload.Result != nil && strings.TrimSpace(payload.Result.Summary) != "" {
		return payload.Result.Summary
	}
	return child.State
}

func continuationEnqueued(events []db.JobEvent) bool {
	for _, event := range events {
		if event.Kind == "delegation_continuation_enqueued" {
			return true
		}
	}
	return false
}

func delegationContinuationID(parentJobID string) string {
	return parentJobID + "/continuation"
}

// buildContinuationPrompt inlines each finished child's job id, agent, decision,
// summary, and PR link into the coordinator continuation prompt so the
// coordinator can synthesize the results without re-reading every child job. When
// humanAnswer is non-empty (the ask-gate `answer` resume path, #445) it is
// rendered as a clearly-labelled block at the TOP of the prompt so the resumed
// coordinator reads the human's decision before the delegation results; it is ""
// for every non-answer continuation, keeping that prompt byte-identical.
func (e Engine) buildContinuationPrompt(goal, budgetLine string, parentResult *AgentResult, children map[string]db.Job, childPayloads map[string]JobPayload, humanAnswer string) string {
	var builder strings.Builder
	builder.WriteString(goalAnchorHeader(goal))
	if block := strings.TrimSpace(humanAnswer); block != "" {
		builder.WriteString("Human answers to your questions (you paused to ask these; use them and proceed):\n")
		builder.WriteString(block)
		builder.WriteString("\n\n")
	}
	builder.WriteString("All delegated jobs have finished. Review the results below.\n\n")
	builder.WriteString(budgetLine)
	builder.WriteString("Delegation results:\n")
	// remainingInline tracks the aggregate ArtifactBody budget across all children
	// for this continuation; only consulted when InlineArtifactBodies is set.
	remainingInline := maxInlineArtifactTotalBytes
	for _, d := range parentResult.Delegations {
		child, ok := children[d.ID]
		if !ok {
			fmt.Fprintf(&builder, "- delegation %q (agent %s): not enqueued (dependencies unmet)\n", d.ID, d.Agent)
			continue
		}
		decision := child.State
		summary := ""
		if payload, ok := childPayloads[d.ID]; ok && payload.Result != nil {
			if strings.TrimSpace(payload.Result.Decision) != "" {
				decision = payload.Result.Decision
			}
			summary = strings.TrimSpace(payload.Result.Summary)
		}
		fmt.Fprintf(&builder, "- delegation %q (job %s, agent %s): %s", d.ID, child.ID, child.Agent, decision)
		if phase := strings.TrimSpace(d.Phase); phase != "" {
			fmt.Fprintf(&builder, " [phase: %s]", phase)
		}
		if summary != "" {
			fmt.Fprintf(&builder, " — %s", summary)
		}
		if link := childPullRequestLink(childPayloads[d.ID]); link != "" {
			fmt.Fprintf(&builder, " (%s)", link)
		}
		builder.WriteString("\n")
		// Opt-in: inline the child's brief body as a fenced block so a downstream
		// model reads it inline. Guarded entirely behind the flag so the disabled
		// output is byte-identical to the legacy prompt.
		if e.InlineArtifactBodies {
			e.appendInlineArtifactBody(&builder, childPayloads[d.ID], child.ID, &remainingInline)
		}
	}
	// Completion contract: make termination directed. The engine already treats
	// an empty delegations list as terminal, so spell it out for the agent. The
	// closing is reframed (#418) from "decide the next step" to goal-anchored
	// synthesis — but the termination semantics (empty delegations = done) are
	// unchanged.
	builder.WriteString("\n\n")
	builder.WriteString(goalSynthesisClosing(goal))
	return builder.String()
}

// appendInlineArtifactBody writes a fenced block containing the child's
// payload.Result.ArtifactBody, rune-safe truncated to the per-body cap
// (e.MaxInlineArtifactBytes or defaultMaxInlineArtifactBytes) and further bounded
// by the per-continuation aggregate budget (*remaining). The block is fenced so an
// embedded gitmoot_result sentinel inside a body cannot confuse a downstream
// model. When truncation occurs a trailing marker points at the full brief on
// disk. It is only called when Engine.InlineArtifactBodies is true.
func (e Engine) appendInlineArtifactBody(builder *strings.Builder, payload JobPayload, childJobID string, remaining *int) {
	if payload.Result == nil {
		return
	}
	body := payload.Result.ArtifactBody
	if body == "" {
		return
	}
	perBody := e.MaxInlineArtifactBytes
	if perBody <= 0 {
		perBody = defaultMaxInlineArtifactBytes
	}
	limit := perBody
	if *remaining < limit {
		limit = *remaining
	}
	if limit <= 0 {
		return
	}
	truncated, omitted := truncateUTF8Bytes(body, limit)
	*remaining -= len(truncated)
	// Assemble the inner block (body + optional truncation marker) first, then pick
	// a fence longer than the longest backtick run inside it. A plain ``` fence is
	// broken by a body that itself contains ``` (briefs/reviews routinely embed code
	// fences), which would let an embedded gitmoot_result sentinel escape — exactly
	// what fencing must prevent.
	var inner strings.Builder
	inner.WriteString(truncated)
	if !strings.HasSuffix(truncated, "\n") {
		inner.WriteString("\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&inner, "... (%d bytes truncated; full brief at %s)\n", omitted, e.inlineBriefPath(childJobID))
	}
	fence := artifactBodyFence(inner.String())
	builder.WriteString("  artifact_body:\n")
	builder.WriteString(fence)
	builder.WriteString("\n")
	builder.WriteString(inner.String())
	builder.WriteString(fence)
	builder.WriteString("\n")
}

// maxUpstreamDepSummaryPreviewBytes caps the inline summary preview emitted on a
// dependency's header line in the "Upstream dependency results" block (#419). The
// full body travels by reference as the fenced artifact_body below the line, so
// the header preview is intentionally short; it is rune-safe truncated and fenced
// so an embedded gitmoot_result sentinel in a summary cannot escape inline.
const maxUpstreamDepSummaryPreviewBytes = 280

// buildUpstreamDepBlock renders the "Upstream dependency results" block injected
// into a ready dependent leg's prompt when InjectUpstreamDepContext is set (#419):
// deps[] as real dataflow. For each of d's succeeded DIRECT deps (sorted by id for
// stable output), it emits a header line —
//
//   - dep <id> (agent <a>, action <act>): <decision> — <summary preview> (<PR>) [changes_made: N] [head <sha7>]
//
// then the dep's artifact_body as a fenced, byte-budgeted block reusing
// appendInlineArtifactBody (the SAME per-body cap and shared aggregate budget the
// continuation prompt uses). Deps travel by reference: decision + truncated
// summary preview + PR link + changes count + short HeadSHA + the on-disk body
// path in any truncation marker — never the bulk body by value beyond the budget.
//
// Decided defaults (#419): direct-deps only; succeeded only
// (State==JobSucceeded && Result!=nil), defensive on a nil Result (decision/state
// line only, no body); body-only (no Findings). A dep resolved through
// fingerprint dedup is followed to its winning sibling. Returns "" when d has no
// deps or none are succeeded, so the caller appends nothing (and the flag-off
// path is byte-identical). Callers MUST gate the call on InjectUpstreamDepContext.
func (e Engine) buildUpstreamDepBlock(d Delegation, children map[string]db.Job, dedupWinners map[string]db.Job) string {
	deps := compactStrings(d.Deps)
	if len(deps) == 0 {
		return ""
	}
	// Sort by id for stable, order-independent output regardless of how the
	// coordinator listed deps.
	sortedDeps := make([]string, len(deps))
	copy(sortedDeps, deps)
	sort.Strings(sortedDeps)

	// remaining tracks the SAME aggregate artifact-body budget the continuation
	// prompt uses, so a verbose upstream body cannot balloon the dependent's
	// prompt across multiple deps.
	remaining := maxInlineArtifactTotalBytes
	var builder strings.Builder
	wrote := false
	for _, dep := range sortedDeps {
		depJob, ok := children[dep]
		if !ok {
			// A dep that points at a fingerprint-deduped delegation has no child of
			// its own; follow it to the winning sibling that did run.
			depJob, ok = dedupWinners[dep]
		}
		if !ok || depJob.State != string(JobSucceeded) {
			// Succeeded-only: a not-yet-run/failed dep contributes nothing (and a
			// dependent only enqueues once depsSatisfied, so this is defensive).
			continue
		}
		depPayload, err := unmarshalPayload(depJob.Payload)
		if err != nil {
			continue
		}
		if !wrote {
			builder.WriteString("\n\nUpstream dependency results:\n")
			wrote = true
		}
		e.appendUpstreamDepEntry(&builder, dep, depJob, depPayload, &remaining)
	}
	if !wrote {
		return ""
	}
	return builder.String()
}

// appendUpstreamDepEntry writes one dependency's header line and (when present)
// its fenced artifact_body to the upstream block. The header line carries the
// pass-by-reference handle (decision/summary preview/PR/changes count/HeadSHA);
// the body, if any, is fenced + truncated via appendInlineArtifactBody so it
// shares the aggregate budget and cannot break out of its fence. Defensive on a
// nil Result: emits the decision/state line only.
func (e Engine) appendUpstreamDepEntry(builder *strings.Builder, depID string, depJob db.Job, depPayload JobPayload, remaining *int) {
	decision := depJob.State
	summary := ""
	if depPayload.Result != nil {
		if d := strings.TrimSpace(depPayload.Result.Decision); d != "" {
			decision = d
		}
		summary = strings.TrimSpace(depPayload.Result.Summary)
	}
	fmt.Fprintf(builder, "- dep %q (agent %s, action %s): %s", depID, depJob.Agent, depJob.Type, decision)
	if summary != "" {
		fmt.Fprintf(builder, " — %s", upstreamDepSummaryPreview(summary))
	}
	if link := childPullRequestLink(depPayload); link != "" {
		fmt.Fprintf(builder, " (%s)", link)
	}
	if depPayload.Result != nil && len(depPayload.Result.ChangesMade) > 0 {
		fmt.Fprintf(builder, " [changes_made: %d]", len(depPayload.Result.ChangesMade))
	}
	if sha := shortHeadSHA(depPayload.HeadSHA); sha != "" {
		fmt.Fprintf(builder, " [head %s]", sha)
	}
	builder.WriteString("\n")
	// Body by reference: a fenced, truncated artifact_body under the line, sharing
	// the aggregate budget. appendInlineArtifactBody is a no-op when the body is
	// empty or the budget is spent, so a nil/empty Result emits only the line.
	e.appendInlineArtifactBody(builder, depPayload, depJob.ID, remaining)
}

// upstreamDepSummaryPreview caps the summary shown inline on a dep's header line
// to a short, rune-safe preview and fences it when it contains a backtick run so
// an embedded sentinel cannot escape. The full body travels separately as the
// fenced artifact_body, so this preview is deliberately short.
func upstreamDepSummaryPreview(summary string) string {
	preview, omitted := truncateUTF8Bytes(summary, maxUpstreamDepSummaryPreviewBytes)
	if omitted > 0 {
		preview = strings.TrimRight(preview, " \t\n") + "…"
	}
	if strings.Contains(preview, "`") {
		fence := artifactBodyFence(preview)
		return fence + preview + fence
	}
	return preview
}

// shortHeadSHA returns the first 7 hex chars of a commit SHA for compact display,
// or "" when the SHA is empty. It does not validate hex; a shorter SHA is returned
// as-is.
func shortHeadSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// artifactBodyFence returns a backtick fence guaranteed longer than the longest
// run of backticks in content, so an embedded ``` (or a sentinel wrapped in one)
// cannot terminate the fenced block early. Minimum three backticks.
func artifactBodyFence(content string) string {
	longest, run := 0, 0
	for _, r := range content {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// inlineBriefPath renders the on-disk location of the parent's full brief.md, the
// same path writeDelegationArtifacts uses (ArtifactRoot/delegations/<sanitized
// parent job id>/brief.md). The parent job id is recovered from a child job id by
// stripping the trailing "/delegation/<child>" suffix; on any failure it falls
// back to a placeholder segment so the marker is still actionable.
func (e Engine) inlineBriefPath(childJobID string) string {
	root := strings.TrimSpace(e.ArtifactRoot)
	if root == "" {
		root = "<ArtifactRoot>"
	}
	segment := "<parent>"
	parentJobID := parentJobIDFromChild(childJobID)
	if seg, err := safeDelegationPathSegment(parentJobID, "parent job id"); err == nil {
		segment = seg
	}
	return root + "/delegations/" + segment + "/brief.md"
}

// parentJobIDFromChild recovers a parent job id from a delegation child job id of
// the form "<parent>/delegation/<child>". When the marker is absent it returns the
// input unchanged so inlineBriefPath can still sanitize it.
func parentJobIDFromChild(childJobID string) string {
	if idx := strings.LastIndex(childJobID, "/delegation/"); idx >= 0 {
		return childJobID[:idx]
	}
	return childJobID
}

// truncateUTF8Bytes returns s capped to at most maxBytes bytes without splitting a
// multi-byte UTF-8 rune, along with the number of bytes omitted from the original.
// Unlike the truncators in internal/cli it does NOT collapse whitespace; the body
// is preserved verbatim up to the cut point.
func truncateUTF8Bytes(s string, maxBytes int) (string, int) {
	if maxBytes <= 0 {
		return "", len(s)
	}
	if len(s) <= maxBytes {
		return s, 0
	}
	cut := maxBytes
	// Back up to a rune boundary: a continuation byte has the form 10xxxxxx.
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut], len(s) - cut
}

// buildCorrectiveContinuationPrompt is the one-shot nudge sent when a
// coordinator re-issues a delegation set it already issued. It tells the
// coordinator the repeat changed nothing and asks it to change approach or
// finish, then lists the repeated delegations for context. If it repeats again,
// handleDelegationLoop escalates to delegation_loop_detected and stops.
func buildCorrectiveContinuationPrompt(goal string, parentResult *AgentResult) string {
	var builder strings.Builder
	builder.WriteString(goalAnchorHeader(goal))
	builder.WriteString("You delegated the same set as a previous round; it did not change the outcome. Change your approach or return an EMPTY delegations list to finish.\n\n")
	if parentResult != nil && len(parentResult.Delegations) > 0 {
		builder.WriteString("Repeated delegation set:\n")
		for _, d := range parentResult.Delegations {
			fmt.Fprintf(&builder, "- delegation %q (agent %s, action %s)\n", d.ID, d.Agent, d.Action)
		}
		builder.WriteString("\n")
	}
	builder.WriteString(goalSynthesisClosing(goal))
	return builder.String()
}

// buildPreflightCorrectiveContinuationPrompt is the corrective continuation sent
// when a delegation set is unroutable (#451): one or more delegations named an
// agent that is not a routable registered agent (unknown / not-allowed /
// uncapable — most often a runtime name where an agent NAME was required). It
// carries the actionable preflight reason (which lists the agents valid for the
// repo and the inline ephemeral alternative) so the coordinator can re-emit a
// corrected set, and it restates that the agent field is a registered agent NAME,
// not a runtime. None of the set was dispatched (all-or-nothing preflight).
func buildPreflightCorrectiveContinuationPrompt(goal string, parentResult *AgentResult, reason string) string {
	var builder strings.Builder
	builder.WriteString(goalAnchorHeader(goal))
	builder.WriteString("Your delegation set could not be dispatched: it named an agent that is not routable, so NONE of it was dispatched (the preflight is all-or-nothing).\n\n")
	fmt.Fprintf(&builder, "%s\n\n", strings.TrimSpace(reason))
	builder.WriteString("A delegation's `agent` field is a registered agent NAME, not a runtime (codex/claude/kimi are runtimes). Re-emit the delegation set using a valid agent name from the list above, or use an inline `ephemeral` spec for an unregistered worker. If you cannot route the work, return an EMPTY delegations list to finish.\n")
	if parentResult != nil && len(parentResult.Delegations) > 0 {
		builder.WriteString("\nDelegations that were NOT dispatched:\n")
		for _, d := range parentResult.Delegations {
			fmt.Fprintf(&builder, "- delegation %q (agent %s, action %s)\n", d.ID, d.Agent, d.Action)
		}
	}
	return builder.String()
}

// buildVerifyReplanContinuationPrompt is the bounded corrective continuation sent
// when the engine-level verify gate (#439) derives a FAILED verdict from the
// verify-tagged legs. Unlike the finalize prompt it is NOT terminal — it asks the
// coordinator to REPLAN: fix the issues the independent verification surfaced and
// re-run the work. It is goal-anchored (#418), surfaces each verify leg's
// decision/summary so the replan can target the fix, and states the remaining
// attempt budget (after this attempt the loop routes to a best-effort finalize).
func buildVerifyReplanContinuationPrompt(goal string, parentResult *AgentResult, children map[string]db.Job, childPayloads map[string]JobPayload, attempt, attemptCap int) string {
	var builder strings.Builder
	builder.WriteString(goalAnchorHeader(goal))
	builder.WriteString("Independent verification FAILED: at least one verify leg reported the work is not yet correct.\n\n")
	// Surface the verify legs' findings so the coordinator can target the fix
	// rather than re-running blind. Only the verify-tagged legs are listed (the
	// failing verdict is derived from them).
	if parentResult != nil {
		wrote := false
		for _, d := range parentResult.Delegations {
			if delegationSynthesisRule(d) != "verify" {
				continue
			}
			if !wrote {
				builder.WriteString("Verification findings:\n")
				wrote = true
			}
			decision := "missing"
			summary := ""
			if child, ok := children[d.ID]; ok {
				decision = child.State
			}
			if payload, ok := childPayloads[d.ID]; ok && payload.Result != nil {
				if strings.TrimSpace(payload.Result.Decision) != "" {
					decision = payload.Result.Decision
				}
				summary = strings.TrimSpace(payload.Result.Summary)
			}
			fmt.Fprintf(&builder, "- verify leg %q (agent %s): %s", d.ID, d.Agent, decision)
			if summary != "" {
				fmt.Fprintf(&builder, " — %s", summary)
			}
			builder.WriteString("\n")
		}
		if wrote {
			builder.WriteString("\n")
		}
	}
	fmt.Fprintf(&builder, "This is verify→replan attempt %d of %d. Address the verification findings above, then re-delegate the corrective work. ", attempt, attemptCap)
	if attempt >= attemptCap {
		builder.WriteString("This is the LAST attempt: if verification fails again you will be asked to synthesize a best-effort final result.\n\n")
	} else {
		builder.WriteString("\n\n")
	}
	builder.WriteString(goalSynthesisClosing(goal))
	return builder.String()
}

// buildFinalizeContinuationPrompt is the terminal continuation sent when a
// termination backstop trips. It tells the coordinator it has hit a limit and
// cannot delegate further, asks it to synthesize a best-effort final result from
// the completed work, and states that any delegations it returns now are ignored
// (the engine enforces this via DelegationFinalize). It lists the delegations that
// were not dispatched for context.
func buildFinalizeContinuationPrompt(goal string, parentResult *AgentResult, reason string) string {
	var builder strings.Builder
	builder.WriteString(goalAnchorHeader(goal))
	fmt.Fprintf(&builder, "A termination backstop was reached (%s). You cannot delegate any more work.\n\n", reason)
	// Goal-anchored synthesis (#418): the finalize continuation is already
	// terminal (any delegations are ignored — DelegationFinalize), so it restates
	// the goal and asks for a best-effort answer to it, reconciling child conflicts
	// and flagging gaps, rather than a raw stitch of child outputs.
	if strings.TrimSpace(goal) != "" {
		builder.WriteString("Synthesize a best-effort FINAL answer to the ORIGINAL GOAL above from what has already completed — reconcile any conflicts between children and flag any gaps — and return an EMPTY delegations list. Any delegations you return now will be ignored.\n")
	} else {
		builder.WriteString("Synthesize a best-effort FINAL result from what has already completed — reconcile any conflicts between children and flag any gaps — and return an EMPTY delegations list. Any delegations you return now will be ignored.\n")
	}
	if parentResult != nil && len(parentResult.Delegations) > 0 {
		builder.WriteString("\nDelegations that were NOT dispatched:\n")
		for _, d := range parentResult.Delegations {
			fmt.Fprintf(&builder, "- delegation %q (agent %s, action %s)\n", d.ID, d.Agent, d.Action)
		}
	}
	return builder.String()
}

func childPullRequestLink(payload JobPayload) string {
	if payload.PullRequest <= 0 || strings.TrimSpace(payload.Repo) == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", payload.Repo, payload.PullRequest)
}

func delegationSynthesisRule(d Delegation) string {
	rule := strings.ToLower(strings.TrimSpace(d.SynthesisRule))
	if rule == "" {
		return "summary"
	}
	return rule
}

// delegationSynthesisRequiresVote reports whether any delegation in the batch
// declares synthesis_rule "vote", which gates the coordinator continuation on
// every child being approved/succeeded.
func delegationSynthesisRequiresVote(delegations []Delegation) bool {
	for _, d := range delegations {
		if delegationSynthesisRule(d) == "vote" {
			return true
		}
	}
	return false
}

// delegationVoteSatisfied reports whether every delegation's child reached an
// approving outcome: a succeeded job state, or a child result decision of
// approved/succeeded/implemented. A missing or non-approving child fails the
// vote.
func delegationVoteSatisfied(delegations []Delegation, children map[string]db.Job, childPayloads map[string]JobPayload) bool {
	for _, d := range delegations {
		child, ok := children[d.ID]
		if !ok {
			return false
		}
		if child.State == string(JobSucceeded) {
			continue
		}
		payload, ok := childPayloads[d.ID]
		if !ok || payload.Result == nil {
			return false
		}
		if !delegationDecisionApproves(payload.Result.Decision) {
			return false
		}
	}
	return true
}

// delegationSynthesisRequiresQuorum reports whether any delegation in the batch
// declares synthesis_rule "quorum", which gates the coordinator continuation on
// at least K children reaching an approving outcome.
func delegationSynthesisRequiresQuorum(delegations []Delegation) bool {
	for _, d := range delegations {
		if delegationSynthesisRule(d) == "quorum" {
			return true
		}
	}
	return false
}

// delegationQuorumThreshold returns the quorum K declared on the quorum
// delegation(s). When multiple quorum delegations declare different thresholds,
// the maximum is used.
func delegationQuorumThreshold(delegations []Delegation) int {
	k := 0
	for _, d := range delegations {
		if delegationSynthesisRule(d) != "quorum" {
			continue
		}
		if d.Quorum > k {
			k = d.Quorum
		}
	}
	return k
}

// delegationQuorumSatisfied reports whether at least k children reached an
// approving outcome: a succeeded job state, or a child result decision of
// approved/succeeded/implemented (the same approving-outcome test the vote rule
// uses).
func delegationQuorumSatisfied(delegations []Delegation, children map[string]db.Job, childPayloads map[string]JobPayload, k int) bool {
	approving := 0
	for _, d := range delegations {
		child, ok := children[d.ID]
		if !ok {
			continue
		}
		if child.State == string(JobSucceeded) {
			approving++
			continue
		}
		payload, ok := childPayloads[d.ID]
		if !ok || payload.Result == nil {
			continue
		}
		if delegationDecisionApproves(payload.Result.Decision) {
			approving++
		}
	}
	return approving >= k
}

func delegationDecisionApproves(decision string) bool {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "approved", "succeeded", "implemented":
		return true
	default:
		return false
	}
}

// delegationSynthesisRequiresVerify reports whether any delegation in the batch
// declares synthesis_rule "verify", which makes the coordinator continuation
// subject to the engine-level verify→replan gate (#439). Unlike vote/quorum it
// does NOT block on failure; it issues a bounded corrective replan continuation.
func delegationSynthesisRequiresVerify(delegations []Delegation) bool {
	for _, d := range delegations {
		if delegationSynthesisRule(d) == "verify" {
			return true
		}
	}
	return false
}

// verifyVerdictPassed derives the v1 verify VERDICT (#439) mechanically from the
// verify-tagged legs (synthesis_rule == "verify"): it returns false iff any such
// leg reached a NON-approving outcome. The verdict is DECISION-driven, reusing the
// approving-outcome test (delegationDecisionApproves) over the #421 convention that
// a verify leg returns approved on a pass and changes_requested on a fail. Because
// a review's changes_requested decision maps to a SUCCEEDED job state
// (stateForDecision), the decision is consulted FIRST whenever the leg produced a
// parsed result — otherwise the succeeded-state short-circuit vote/quorum use would
// read every changes_requested verdict as a pass and defeat the gate. The
// succeeded-state check is kept only as a fallback for a verify leg that finished
// in a succeeded state without a parsed result.
//
// A verify leg with NO child is interpreted by whether it could ever have run.
// A leg whose deps terminally failed (delegationPermanentlyBlocked, the same
// predicate allDelegationsResolved uses) or that was folded into a fingerprint
// dedup winner NEVER RAN: verification was not performed, so its outcome is
// already governed by the upstream failure policy (continue/escalate) and it must
// be SKIPPED here, not read as a failed verdict — otherwise the engine would
// fabricate a failed verdict from an absent verifier and fire a premature
// verify→replan continuation claiming verification failed when it never happened
// (#439 review). Only a verify leg that WAS dispatchable yet has no terminal child
// (a genuinely missing/crashed verification) fails the verdict, so an absent
// verification of work that actually ran is never read as a pass. Non-verify legs
// are ignored here (the conservative ordering runs the vote/quorum gates first).
// No engine-side verify subprocess or second model call is made: the engine reads
// the already-completed verdict the verify leg reported.
func verifyVerdictPassed(delegations []Delegation, children map[string]db.Job, childPayloads map[string]JobPayload, dedupWinners map[string]db.Job) bool {
	byID := delegationsByID(delegations)
	for _, d := range delegations {
		if delegationSynthesisRule(d) != "verify" {
			continue
		}
		child, ok := children[d.ID]
		if !ok {
			// A verify leg that never ran (its deps terminally failed, or it was
			// folded into a dedup winner) is governed by the upstream failure policy,
			// not by this verdict: skip it rather than fabricating a failed verdict.
			if _, deduped := dedupWinners[d.ID]; deduped {
				continue
			}
			if delegationPermanentlyBlocked(d, children, byID, dedupWinners, map[string]bool{}) {
				continue
			}
			// Dispatchable verify leg with no terminal child: a genuinely missing or
			// crashed verification fails the verdict.
			return false
		}
		// Decision-first: when the verify leg produced a parsed result, its decision
		// is the verdict (approved => pass, changes_requested/failed/blocked => fail).
		if payload, ok := childPayloads[d.ID]; ok && payload.Result != nil {
			if !delegationDecisionApproves(payload.Result.Decision) {
				return false
			}
			continue
		}
		// No parsed result: fall back to the job state — a succeeded leg with no
		// verdict is treated as a pass, anything else (failed/blocked/queued) fails.
		if child.State != string(JobSucceeded) {
			return false
		}
	}
	return true
}

func (e Engine) preflightDelegation(ctx context.Context, request JobRequest) error {
	// An ephemeral delegation routes to an on-demand worker that no agent row
	// backs, so the registered-agent existence, repo-access, and capability checks
	// do not apply: the ephemeral child inherits the coordinator's allowed repo
	// scope. Only validate that the spec runtime is a real agent runtime (never
	// shell); the daemon materializes the worker from the spec.
	if request.Ephemeral != nil {
		return validateEphemeralSpec(request.DelegationID, request.Action, request.Ephemeral)
	}
	// Check existence FIRST so a legitimately-named agent literally called
	// "claude" (GetAgent hits) is never mistaken for the runtime-name mixup below.
	agent, err := e.Store.GetAgent(ctx, request.Agent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The name resolves to no agent. Only when it is itself a runtime name
			// ({codex,claude,kimi}) is this the common runtime-vs-agent mixup, so
			// flag that explicitly; otherwise it is a typo/unknown agent.
			prefix := fmt.Sprintf("agent %q is not subscribed", request.Agent)
			if _, isRuntime := allowedSet(EphemeralRuntimes)[strings.TrimSpace(request.Agent)]; isRuntime {
				prefix = fmt.Sprintf("%q is a runtime, not a registered agent", request.Agent)
			}
			return fmt.Errorf("%s. %s", prefix, e.delegationAgentHint(ctx, request.Repo))
		}
		return err
	}
	allowed, err := e.Store.AgentCanAccessRepo(ctx, agent.Name, request.Repo)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("agent %q is not allowed on %q. %s", agent.Name, request.Repo, e.delegationAgentHint(ctx, request.Repo))
	}
	if !contains(agent.Capabilities, request.Action) {
		return fmt.Errorf("agent %q lacks %q capability. %s", agent.Name, request.Action, e.delegationAgentHint(ctx, request.Repo))
	}
	return nil
}

// availableAgentsForRepo returns the names of registered agents that can access
// repo, sorted by name (ListAgents already ORDER BY name). It fails soft: a
// ListAgents error yields nil so the caller's base error is never masked, and a
// per-agent AgentCanAccessRepo error simply drops that agent from the suggestion.
func (e Engine) availableAgentsForRepo(ctx context.Context, repo string) []string {
	agents, err := e.Store.ListAgents(ctx)
	if err != nil {
		return nil
	}
	var names []string
	for _, a := range agents {
		ok, err := e.Store.AgentCanAccessRepo(ctx, a.Name, repo)
		if err != nil || !ok {
			continue
		}
		names = append(names, a.Name)
	}
	return names
}

// delegationAgentHint renders the actionable suffix appended to every
// preflightDelegation error: which registered agents are usable on repo (so a
// coordinator can re-emit a corrected set) plus the inline ephemeral escape hatch
// that needs no pre-registration. The agent field of a delegation is a registered
// agent NAME, not a runtime.
func (e Engine) delegationAgentHint(ctx context.Context, repo string) string {
	var b strings.Builder
	names := e.availableAgentsForRepo(ctx, repo)
	if len(names) > 0 {
		fmt.Fprintf(&b, "Agents allowed on %s: %s. ", repo, strings.Join(names, ", "))
	} else {
		fmt.Fprintf(&b, "No agents are registered for %s. ", repo)
	}
	b.WriteString(`To run an unregistered worker, use an inline ephemeral spec ({runtime: "codex|claude|kimi"}).`)
	return b.String()
}

func (e Engine) implementationNeedsFinalizer(ctx context.Context, payload JobPayload) bool {
	if e.ImplementationFinalizer == nil {
		return false
	}
	taskID := strings.TrimSpace(payload.TaskID)
	if taskID == "" {
		return false
	}
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return false
	}
	return strings.TrimSpace(task.WorktreePath) != ""
}

func reviewDecisionAgent(job db.Job, payload JobPayload) string {
	if job.Type == "review" &&
		payload.DelegationReason == "runtime_session_busy" &&
		payload.DelegatedAgent == job.Agent &&
		strings.TrimSpace(payload.OriginalAgent) != "" {
		return payload.OriginalAgent
	}
	return job.Agent
}

func (e Engine) jobPayload(ctx context.Context, jobID string) (db.Job, JobPayload, error) {
	job, err := e.Store.GetJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.Job{}, JobPayload{}, fmt.Errorf("job %q not found", jobID)
		}
		return db.Job{}, JobPayload{}, err
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return db.Job{}, JobPayload{}, err
	}
	return job, payload, nil
}

func (e Engine) validate() error {
	if e.Store == nil {
		return errors.New("workflow engine store is required")
	}
	return nil
}

func validatePullRequestEvent(event PullRequestEvent) error {
	switch {
	case strings.TrimSpace(event.Repo) == "":
		return errors.New("pull request repo is required")
	case strings.TrimSpace(event.Branch) == "":
		return errors.New("pull request branch is required")
	case event.PullRequest <= 0:
		return errors.New("pull request number is required")
	case strings.TrimSpace(event.TaskID) == "":
		return errors.New("pull request task id is required")
	case strings.TrimSpace(event.LeadAgent) == "":
		return errors.New("pull request lead agent is required")
	}
	return nil
}

func (e Engine) dispatchFix(ctx context.Context, reviewer string, payload JobPayload, result AgentResult, ref taskRef) error {
	leadAgent, err := e.leadAgent(ctx, payload)
	if err != nil {
		return err
	}
	request := JobRequest{
		Agent:       leadAgent,
		Action:      "implement",
		Repo:        payload.Repo,
		Branch:      payload.Branch,
		PullRequest: payload.PullRequest,
		HeadSHA:     payload.HeadSHA,
		GoalID:      payload.GoalID,
		TaskID:      payload.TaskID,
		TaskTitle:   payload.TaskTitle,
		LeadAgent:   leadAgent,
		Reviewers:   e.requiredReviewers(payload),
		ReviewRound: payload.ReviewRound,
		Sender:      reviewer,
		Instructions: fmt.Sprintf(
			"Address requested changes from %s: %s",
			reviewer,
			result.Summary,
		),
	}
	return e.dispatch(ctx, request, ref)
}

func (e Engine) leadAgent(ctx context.Context, payload JobPayload) (string, error) {
	leadAgent := strings.TrimSpace(payload.LeadAgent)
	if leadAgent != "" {
		return leadAgent, nil
	}
	lock, err := e.Store.GetBranchLock(ctx, payload.Repo, payload.Branch)
	if err == nil {
		return lock.Owner, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("lead agent is required")
	}
	return "", err
}

func (e Engine) allRequiredReviewersApproved(ctx context.Context, currentReviewer string, payload JobPayload) (bool, error) {
	required := e.requiredReviewers(payload)
	if len(required) == 0 {
		return true, nil
	}

	approved := map[string]bool{}
	if currentReviewer != "" {
		approved[currentReviewer] = true
	}

	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		jobPayload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return false, err
		}
		if !sameTask(payload, jobPayload) || !sameReviewRound(payload, jobPayload) || jobPayload.Result == nil {
			continue
		}
		if jobPayload.Result.Decision == "approved" {
			approved[reviewDecisionAgent(job, jobPayload)] = true
		}
	}

	for _, reviewer := range required {
		if !approved[reviewer] {
			return false, nil
		}
	}
	return true, nil
}

func (e Engine) setReviewingIfNotChangesRequested(ctx context.Context, ref taskRef) error {
	if strings.TrimSpace(ref.ID) == "" {
		return nil
	}
	task, err := e.Store.GetTask(ctx, ref.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && task.State == string(TaskChangesRequested) {
		return nil
	}
	return e.setTaskState(ctx, ref, TaskReviewing)
}

func (e Engine) latestReviewRound(ctx context.Context, current JobPayload) (string, error) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return "", err
	}
	latestRound := ""
	latestNumber := 0
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return "", err
		}
		if !sameTask(current, payload) {
			continue
		}
		round := strings.TrimSpace(payload.ReviewRound)
		if round == "" {
			continue
		}
		number, ok := reviewRoundNumber(round)
		if ok && number > latestNumber {
			latestRound = round
			latestNumber = number
			continue
		}
		if !ok && latestNumber == 0 && round > latestRound {
			latestRound = round
		}
	}
	return latestRound, nil
}

func reviewRoundNumber(round string) (int, bool) {
	value, ok := strings.CutPrefix(round, "review-")
	if !ok {
		return 0, false
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return number, true
}

// reviewRoundCount maps a review-round label to a 1-based fix-round count for the
// Mode-A harvester's graded changes_requested negative (#465): "review-1" => 1,
// "review-3" => 3. An empty or unparseable round (a first/legacy review with no
// numbered round) counts as 1, so a single changes_requested is always at least
// the first fix round. The harvester turns a higher count into a worse score.
func reviewRoundCount(round string) int {
	if number, ok := reviewRoundNumber(strings.TrimSpace(round)); ok && number >= 1 {
		return number
	}
	return 1
}

func (e Engine) requiredReviewers(payload JobPayload) []string {
	reviewers := compactStrings(append([]string{}, payload.Reviewers...))
	if len(reviewers) == 0 {
		reviewers = compactStrings(append([]string{}, e.RequiredReviewers...))
	}
	return reviewers
}

func sameTask(left JobPayload, right JobPayload) bool {
	if left.Repo != "" && right.Repo != "" && left.Repo != right.Repo {
		return false
	}
	if left.PullRequest > 0 && right.PullRequest > 0 && left.PullRequest != right.PullRequest {
		return false
	}
	if left.TaskID != "" || right.TaskID != "" {
		return left.TaskID != "" && left.TaskID == right.TaskID
	}
	return left.Repo == right.Repo && left.PullRequest == right.PullRequest
}

func sameReviewRound(left JobPayload, right JobPayload) bool {
	leftRound := strings.TrimSpace(left.ReviewRound)
	rightRound := strings.TrimSpace(right.ReviewRound)
	if leftRound == "" {
		return rightRound == ""
	}
	return leftRound == rightRound
}

func (e Engine) dispatch(ctx context.Context, request JobRequest, ref taskRef) error {
	if request.ID == "" {
		request.ID = e.jobID(request)
	}
	if err := e.ensureAgentAllowed(ctx, request, ref); err != nil {
		return err
	}
	return e.enqueue(ctx, request)
}

func (e Engine) enqueue(ctx context.Context, request JobRequest) error {
	if request.ID == "" {
		request.ID = e.jobID(request)
	}
	_, err := e.mailbox().Enqueue(ctx, request)
	if err == nil {
		return nil
	}
	matches, matchErr := e.existingJobMatchesRequest(ctx, request)
	if matchErr != nil {
		return err
	}
	if matches {
		return nil
	}
	return err
}

func (e Engine) existingJobMatchesRequest(ctx context.Context, request JobRequest) (bool, error) {
	job, err := e.Store.GetJob(ctx, request.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if job.Type != request.Action {
		return false, nil
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return false, err
	}
	if !jobMatchesRequestAgent(job, payload, request.Agent) {
		return false, nil
	}
	return payloadMatchesRequest(payload, request), nil
}

func jobMatchesRequestAgent(job db.Job, payload JobPayload, requestAgent string) bool {
	if job.Agent == requestAgent {
		return true
	}
	return payload.DelegationReason == "runtime_session_busy" &&
		payload.DelegatedAgent == job.Agent &&
		payload.OriginalAgent == requestAgent
}

func payloadMatchesRequest(payload JobPayload, request JobRequest) bool {
	return payload.Repo == request.Repo &&
		payload.Branch == request.Branch &&
		payload.PullRequest == request.PullRequest &&
		payload.HeadSHA == request.HeadSHA &&
		payload.GoalID == request.GoalID &&
		payload.TaskID == request.TaskID &&
		payload.TaskTitle == request.TaskTitle &&
		payload.LeadAgent == request.LeadAgent &&
		payload.ReviewRound == request.ReviewRound &&
		payload.Sender == request.Sender &&
		payload.Instructions == request.Instructions &&
		payload.WorktreePath == request.WorktreePath &&
		payloadDelegationMatchesRequest(payload, request) &&
		equalStrings(payload.Reviewers, compactStrings(request.Reviewers)) &&
		equalStrings(payload.Constraints, compactStrings(request.Constraints))
}

func payloadDelegationMatchesRequest(payload JobPayload, request JobRequest) bool {
	if payload.OriginalAgent == request.OriginalAgent &&
		payload.DelegatedAgent == request.DelegatedAgent &&
		payload.DelegationReason == request.DelegationReason {
		return true
	}
	return request.OriginalAgent == "" &&
		request.DelegatedAgent == "" &&
		request.DelegationReason == "" &&
		payload.DelegationReason == "runtime_session_busy" &&
		payload.OriginalAgent == request.Agent
}

func equalStrings(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (e Engine) ensureAgentAllowed(ctx context.Context, request JobRequest, ref taskRef) error {
	return e.ensureAgentAllowedWithBranchOwner(ctx, request, request.Agent, ref, false)
}

func (e Engine) ensureJobExecutorAllowed(ctx context.Context, job db.Job, payload JobPayload, ref taskRef) error {
	branchOwner := job.Agent
	authorizationAgent := job.Agent
	if job.Type == "implement" && payload.DelegationReason == "runtime_session_busy" && payload.DelegatedAgent == job.Agent && strings.TrimSpace(payload.OriginalAgent) != "" {
		branchOwner = payload.OriginalAgent
	}
	if payload.DelegationReason == "runtime_session_busy" && payload.DelegatedAgent == job.Agent && strings.TrimSpace(payload.OriginalAgent) != "" {
		authorizationAgent = payload.OriginalAgent
	}
	allowMissingCapability := job.Type == "ask" &&
		payload.DelegationReason == "temp_worker_merge_back" &&
		payload.OriginalAgent == job.Agent
	return e.ensureAgentAllowedWithBranchOwner(ctx, JobRequest{
		Agent:        authorizationAgent,
		Action:       job.Type,
		Repo:         payload.Repo,
		Branch:       payload.Branch,
		DelegationID: payload.DelegationID,
		// Carry the worker spec so an ephemeral child's executor check inherits the
		// coordinator's repo scope (skip the registered-agent checks) instead of
		// blocking on a synthetic agent name that no agent row backs.
		Ephemeral: payload.Ephemeral,
	}, branchOwner, ref, allowMissingCapability)
}

func (e Engine) ensureAgentAllowedWithBranchOwner(ctx context.Context, request JobRequest, branchOwner string, ref taskRef, allowMissingCapability bool) error {
	// An ephemeral worker has no registered agent row: it inherits the
	// coordinator's allowed repo scope, so the existence, repo-access, and
	// capability checks are skipped. Validate only that the spec runtime is a real
	// agent runtime (never shell), then fall through to the shared branch-lock path
	// so an ephemeral implement still serializes on its branch like any other.
	if request.Ephemeral != nil {
		if err := validateEphemeralSpec(request.DelegationID, request.Action, request.Ephemeral); err != nil {
			return e.block(ctx, ref, err.Error())
		}
		if request.Action == "implement" {
			return e.ensureBranchLock(ctx, request.Repo, request.Branch, branchOwner, ref)
		}
		return nil
	}
	agent, err := e.Store.GetAgent(ctx, request.Agent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.block(ctx, ref, fmt.Sprintf("agent %q is not subscribed", request.Agent))
		}
		return err
	}
	allowed, err := e.Store.AgentCanAccessRepo(ctx, agent.Name, request.Repo)
	if err != nil {
		return err
	}
	if !allowed {
		return e.block(ctx, ref, fmt.Sprintf("agent %q is not allowed on %q", agent.Name, request.Repo))
	}
	if !contains(agent.Capabilities, request.Action) && !allowMissingCapability {
		return e.block(ctx, ref, fmt.Sprintf("agent %q lacks %q capability", agent.Name, request.Action))
	}
	if request.Action == "implement" {
		// Fail-closed: an implement job whose agent grants no headless write
		// (auto/empty or read-only) is BLOCKED here — at the universal dispatch
		// preflight — rather than running to completion and producing no files. This
		// catches pre-existing agents and later policy edits, using the same shared
		// guidance the CLI emits at start/subscribe.
		if err := runtime.ImplementWritePolicyError([]string{request.Action}, agent.AutonomyPolicy); err != nil {
			return e.block(ctx, ref, err.Error())
		}
		if err := e.ensureBranchLock(ctx, request.Repo, request.Branch, branchOwner, ref); err != nil {
			return err
		}
	}
	return nil
}

func (e Engine) ensureBranchLock(ctx context.Context, repo string, branch string, owner string, ref taskRef) error {
	if strings.TrimSpace(branch) == "" {
		return e.block(ctx, ref, "branch lock rejected action: branch is required")
	}
	acquired, err := e.Store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo, Branch: branch, Owner: owner})
	if err != nil {
		return err
	}
	if !acquired {
		return e.block(ctx, ref, fmt.Sprintf("branch lock rejected action for %s", branch))
	}
	return nil
}

func (e Engine) runMergeGate(ctx context.Context, reviewer string, payload JobPayload, ref taskRef) (MergeDecision, error) {
	if e.MergeGate == nil {
		return MergeDecision{Ready: true}, e.setTaskState(ctx, ref, TaskReadyToMerge)
	}
	reviewRequired, err := e.mergeGateReviewRequired(ctx, payload)
	if err != nil {
		return MergeDecision{}, err
	}
	decision, err := e.MergeGate.Evaluate(ctx, MergeRequest{
		Repo:           payload.Repo,
		Branch:         payload.Branch,
		PullRequest:    payload.PullRequest,
		HeadSHA:        payload.HeadSHA,
		TaskID:         payload.TaskID,
		Reviewer:       reviewer,
		ReviewOptional: !reviewRequired,
	})
	if err != nil {
		return MergeDecision{}, err
	}
	if !decision.Ready {
		reason := strings.TrimSpace(decision.Reason)
		if reason == "" {
			reason = "merge gate rejected action"
		}
		// e.block returns a BlockedError on SUCCESS (the task is durably blocked) and
		// a plain error only on a store failure. Harvest the verifiable negative (#465)
		// only when the block transition itself succeeded — i.e. the returned error is
		// a BlockedError — AND the block is an AUTHORITATIVE template-quality rejection
		// (external CI failed, blocking review captured, closed-without-merge). A
		// transient/infra block (branch staleness, dirty local worktree, missing
		// head/base SHA, freshness-unknown) says nothing about template quality, so it
		// is NOT harvested — otherwise branch-staleness/infra noise would be
		// mis-attributed to the template as a false Hard=0 negative (#465
		// INFRA-NOISE-FILTERED). A real store error skips the harvest and returns up.
		// Best-effort and nil-safe: a harvest error can never affect the (already-
		// durable) block.
		err := e.block(ctx, ref, reason)
		var blocked BlockedError
		if errors.As(err, &blocked) && decision.BlockClass == MergeBlockQuality {
			e.harvestOutcomeForMergeGate(ctx, payload, Outcome{
				Kind:        OutcomeBlocked,
				Repo:        payload.Repo,
				PullRequest: payload.PullRequest,
				HeadSHA:     payload.HeadSHA,
				Reason:      reason,
			})
		}
		return decision, err
	}
	if decision.Merged {
		if err := e.setTaskState(ctx, ref, TaskMerged); err != nil {
			return decision, err
		}
		// Verifiable positive (#465): a merge through the gate. Carry the PR HEAD SHA
		// (not the merge commit) so the harvester's no-CI guard can read
		// GetCombinedStatus/ListPullRequestChecks at the SHA the merge gate actually
		// evaluated and posted statuses/checks at — GitHub does not copy them onto the
		// new merge commit, so reading the merge commit would always look like no CI.
		// Fall back to the merge commit only if the head SHA is somehow empty.
		mergedHead := strings.TrimSpace(payload.HeadSHA)
		if mergedHead == "" {
			mergedHead = strings.TrimSpace(decision.MergeCommitSHA)
		}
		e.harvestOutcomeForMergeGate(ctx, payload, Outcome{
			Kind:        OutcomeMerged,
			Repo:        payload.Repo,
			PullRequest: payload.PullRequest,
			HeadSHA:     mergedHead,
		})
		// Soft cross-family review signal (#469), DETACHED from the blocking
		// AdvanceJob / daemon-poll path and strictly best-effort: a nil dispatcher
		// (the default, no cross_family_review_enabled) is a no-op, the live reviewer
		// adapter.Deliver runs in its own bounded, cancellation-detached goroutine so
		// it can never stall AdvanceJob / the worker tick / the daemon's checkoutLock,
		// and any review-leg failure is swallowed so it can never block or fail the
		// (already-completed) merge.
		e.reviewCrossFamilyForMergeGate(ctx, payload, mergedHead)
		// Objective deterministic-checker signal (#485), DETACHED off the blocking
		// AdvanceJob / daemon-poll path and strictly best-effort, mirroring the review
		// leg above: a nil dispatcher (the default, no deterministic_checkers_enabled)
		// is a no-op, the external tools (dupl/jscpd/golangci-lint/gocyclo) run in
		// their own bounded, cancellation-detached goroutine so a slow tool can never
		// stall AdvanceJob / the worker tick / the daemon's checkoutLock, and any
		// checker-leg failure is swallowed so it can never block or fail the
		// (already-completed) merge.
		e.checkDeterministicForMergeGate(ctx, payload, mergedHead)
		// Deterministic HARD-verifier tier (#474), DETACHED off the blocking AdvanceJob
		// / daemon-poll path and strictly best-effort, mirroring the review + checker
		// legs above: a nil dispatcher (the default, no hard_verifiers_enabled) is a
		// no-op, the operator's build/test commands run in a FRESH sandbox in their own
		// bounded, cancellation-detached goroutine so a slow suite can never stall
		// AdvanceJob / the worker tick / the daemon's checkoutLock, and any verifier-leg
		// failure is swallowed so it can never block or fail the (already-completed)
		// merge. Its binary pass/fail becomes the authoritative EvaluatorScore.Hard.
		e.verifyHardForMergeGate(ctx, payload, mergedHead)
		return decision, nil
	}
	return decision, e.setTaskState(ctx, ref, TaskReadyToMerge)
}

// reviewCrossFamilyForMergeGate runs the cross-family review leg for a just-merged
// PR and harvests its OutcomeReviewed rubric into the SAME auto-trace run as a
// SOFT, down-weighted, judge-tagged secondary signal (#469). It is nil-safe (no
// dispatcher => no-op, byte-identical) and best-effort.
//
// CRITICAL (review-fix): the review runs a LIVE reviewer adapter.Deliver that can
// take minutes, so it is DETACHED from the caller's path — it runs in its own
// goroutine (via spawnReview) under a fresh, cancellation-decoupled context bounded
// by ReviewLegTimeout. This guarantees a wedged reviewer process can NEVER stall
// AdvanceJob, the daemon worker tick, or the daemon's checkoutLock (which the
// AdvanceJob path holds), and is reaped by the timeout rather than leaking forever.
// A dispatch error is swallowed + recorded as a cross_family_review_failed job
// event, NEVER returned up, so a review failure can never block or fail a job. The
// outcome is attributed to the IMPLEMENT job that produced the diff (the same
// implementJobForTask attribution the merge-gate harvest uses), so the soft review
// row lands on the implementer's template version next to the verifiable floor.
func (e Engine) reviewCrossFamilyForMergeGate(ctx context.Context, reviewPayload JobPayload, mergedHead string) {
	if e.ReviewLegDispatcher == nil || e.OutcomeHarvester == nil {
		return
	}
	// Resolve the owning implement job up front (a cheap store read, no LLM) so the
	// detached closure is self-contained. If none resolves, there is nothing to
	// review/attribute.
	job, payload, ok := e.implementJobForTask(ctx, reviewPayload)
	if !ok {
		return
	}
	e.spawnReviewLeg(func() {
		// Fresh context decoupled from the caller's ctx (which is cancelled the moment
		// AdvanceJob returns) yet bounded so a wedged reviewer is reaped, not leaked.
		reviewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.reviewLegTimeout())
		defer cancel()
		e.runReviewLeg(reviewCtx, job, payload, mergedHead)
	})
}

// runReviewLeg performs the actual cross-family review dispatch + harvest. It is
// called from the DETACHED goroutine (never on the AdvanceJob path) so its live
// adapter.Deliver cannot block job advancement.
func (e Engine) runReviewLeg(ctx context.Context, job db.Job, payload JobPayload, mergedHead string) {
	outcome, ok, err := e.ReviewLegDispatcher.Review(ctx, job, payload, mergedHead)
	if err != nil {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "cross_family_review_failed",
			Message: err.Error(),
		})
		return
	}
	if !ok {
		// No review-capable runtime authed at all: skip silently (no review row).
		return
	}
	e.harvestOutcome(ctx, job, payload, outcome)
}

// reviewLegTimeout returns the configured detached-review-leg ceiling, defaulting
// to defaultReviewLegTimeout when unset so a zero-valued Engine keeps a bounded
// reviewer.
func (e Engine) reviewLegTimeout() time.Duration {
	if e.ReviewLegTimeout > 0 {
		return e.ReviewLegTimeout
	}
	return defaultReviewLegTimeout
}

// spawnReviewLeg runs fn off the caller's path. The default spawns a goroutine
// (fire-and-forget, like the EventSink seam); tests inject a synchronous runner via
// the ReviewSpawner hook so the detached review is deterministic.
func (e Engine) spawnReviewLeg(fn func()) {
	if e.ReviewSpawner != nil {
		e.ReviewSpawner(fn)
		return
	}
	go fn()
}

// checkDeterministicForMergeGate runs the OBJECTIVE deterministic-checker leg for a
// just-merged PR and harvests its tool-derived OutcomeReviewed{Objective:true}
// dimensions into the SAME auto-trace run as a THIRD coexisting, non-LLM signal
// (#485). It is nil-safe (no dispatcher => no-op, byte-identical) and best-effort,
// and mirrors reviewCrossFamilyForMergeGate EXACTLY.
//
// CRITICAL: the checker leg shells out to external tools that can be slow, so it is
// DETACHED from the caller's path — it runs in its own goroutine (via
// spawnCheckerLeg) under a fresh, cancellation-decoupled context bounded by
// CheckerLegTimeout. This guarantees a wedged tool can NEVER stall AdvanceJob, the
// daemon worker tick, or the daemon's checkoutLock, and is reaped by the timeout
// rather than leaking. A dispatch error is swallowed + recorded as a
// deterministic_checkers_failed job event, NEVER returned up, so a tool failure can
// never block or fail a job. The outcome is attributed to the IMPLEMENT job that
// produced the diff (the same implementJobForTask attribution the merge-gate harvest
// and the review leg use), so the objective row lands on the implementer's template
// version next to the verifiable floor and the subjective review.
func (e Engine) checkDeterministicForMergeGate(ctx context.Context, reviewPayload JobPayload, mergedHead string) {
	if e.DeterministicCheckerDispatcher == nil || e.OutcomeHarvester == nil {
		return
	}
	// Resolve the owning implement job up front (a cheap store read, no tools) so the
	// detached closure is self-contained. If none resolves, there is nothing to
	// check/attribute.
	job, payload, ok := e.implementJobForTask(ctx, reviewPayload)
	if !ok {
		return
	}
	e.spawnCheckerLeg(func() {
		// Fresh context decoupled from the caller's ctx (which is cancelled the moment
		// AdvanceJob returns) yet bounded so a wedged tool is reaped, not leaked.
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.checkerLegTimeout())
		defer cancel()
		e.runCheckerLeg(checkCtx, job, payload, mergedHead)
	})
}

// runCheckerLeg performs the actual deterministic-checker dispatch + harvest. It is
// called from the DETACHED goroutine (never on the AdvanceJob path) so its external
// tool runs cannot block job advancement (#485).
func (e Engine) runCheckerLeg(ctx context.Context, job db.Job, payload JobPayload, mergedHead string) {
	outcome, ok, err := e.DeterministicCheckerDispatcher.Check(ctx, job, payload, mergedHead)
	if err != nil {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "deterministic_checkers_failed",
			Message: err.Error(),
		})
		return
	}
	if !ok {
		// No producible dimension at all (every checker skipped): skip silently (no
		// checker row), exactly like an empty-rubric review.
		return
	}
	e.harvestOutcome(ctx, job, payload, outcome)
}

// checkerLegTimeout returns the configured detached-checker-leg ceiling, defaulting
// to defaultCheckerLegTimeout when unset so a zero-valued Engine keeps a bounded
// checker.
func (e Engine) checkerLegTimeout() time.Duration {
	if e.CheckerLegTimeout > 0 {
		return e.CheckerLegTimeout
	}
	return defaultCheckerLegTimeout
}

// spawnCheckerLeg runs fn off the caller's path. The default spawns a goroutine
// (fire-and-forget, like spawnReviewLeg); tests inject a synchronous runner via the
// CheckerSpawner hook so the detached checker leg is deterministic.
func (e Engine) spawnCheckerLeg(fn func()) {
	if e.CheckerSpawner != nil {
		e.CheckerSpawner(fn)
		return
	}
	go fn()
}

// verifyHardForMergeGate runs the deterministic HARD-verifier leg for a just-merged
// PR and harvests its BINARY pass/fail verdict into the SAME auto-trace run as the
// authoritative EvaluatorScore.Hard (#474). It is nil-safe (no dispatcher => no-op,
// byte-identical) and best-effort, and mirrors checkDeterministicForMergeGate EXACTLY.
//
// CRITICAL: the verifier leg provisions a fresh sandbox and runs the operator's
// build/test commands, which can take minutes, so it is DETACHED from the caller's
// path — it runs in its own goroutine (via spawnHardVerifierLeg) under a fresh,
// cancellation-decoupled context bounded by HardVerifierLegTimeout. This guarantees a
// slow/wedged verifier can NEVER stall AdvanceJob, the daemon worker tick, or the
// daemon's checkoutLock, and is reaped by the timeout rather than leaking. A dispatch
// error is swallowed + recorded as a hard_verifiers_failed job event, NEVER returned
// up, so a verifier failure can never block or fail a job. The outcome is attributed
// to the IMPLEMENT job that produced the diff (the same implementJobForTask
// attribution the merge-gate harvest, review leg, and checker leg use), so the hard
// row lands on the implementer's template version next to the verifiable floor.
func (e Engine) verifyHardForMergeGate(ctx context.Context, reviewPayload JobPayload, mergedHead string) {
	if e.HardVerifierDispatcher == nil || e.OutcomeHarvester == nil {
		return
	}
	// Resolve the owning implement job up front (a cheap store read, no sandbox) so the
	// detached closure is self-contained. If none resolves, there is nothing to
	// verify/attribute.
	job, payload, ok := e.implementJobForTask(ctx, reviewPayload)
	if !ok {
		return
	}
	e.spawnHardVerifierLeg(func() {
		// Fresh context decoupled from the caller's ctx (which is cancelled the moment
		// AdvanceJob returns) yet bounded so a wedged verifier is reaped, not leaked.
		verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.hardVerifierLegTimeout())
		defer cancel()
		e.runHardVerifierLeg(verifyCtx, job, payload, mergedHead)
	})
}

// runHardVerifierLeg performs the actual hard-verifier dispatch + harvest. It is
// called from the DETACHED goroutine (never on the AdvanceJob path) so its
// sandbox provision + command runs cannot block job advancement (#474).
func (e Engine) runHardVerifierLeg(ctx context.Context, job db.Job, payload JobPayload, mergedHead string) {
	outcome, ok, err := e.HardVerifierDispatcher.Verify(ctx, job, payload, mergedHead)
	if err != nil {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "hard_verifiers_failed",
			Message: err.Error(),
		})
		return
	}
	if !ok {
		// No verdict producible (sandbox could not be provisioned, or no commands):
		// skip silently (no hard row).
		return
	}
	e.harvestOutcome(ctx, job, payload, outcome)
}

// hardVerifierLegTimeout returns the configured detached-hard-verifier-leg ceiling,
// defaulting to defaultHardVerifierLegTimeout when unset so a zero-valued Engine
// keeps a bounded verifier.
func (e Engine) hardVerifierLegTimeout() time.Duration {
	if e.HardVerifierLegTimeout > 0 {
		return e.HardVerifierLegTimeout
	}
	return defaultHardVerifierLegTimeout
}

// spawnHardVerifierLeg runs fn off the caller's path. The default spawns a goroutine
// (fire-and-forget, like spawnCheckerLeg); tests inject a synchronous runner via the
// HardVerifierSpawner hook so the detached verifier leg is deterministic.
func (e Engine) spawnHardVerifierLeg(fn func()) {
	if e.HardVerifierSpawner != nil {
		e.HardVerifierSpawner(fn)
		return
	}
	go fn()
}

// harvestOutcomeForMergeGate resolves the implement job that owns this PR/task and
// harvests the merge-gate outcome against it (#465). runMergeGate runs in the
// context of the APPROVING REVIEW job (or a merge-gate re-run), so the outcome
// must be attributed to the implement job that produced the diff/PR — the one
// carrying the TemplateID/TemplateResolvedCommit. When no implement job can be
// resolved (best-effort), the harvest is skipped. Nil-safe: a nil harvester
// short-circuits before any lookup.
func (e Engine) harvestOutcomeForMergeGate(ctx context.Context, reviewPayload JobPayload, outcome Outcome) {
	if e.OutcomeHarvester == nil {
		return
	}
	job, payload, ok := e.implementJobForTask(ctx, reviewPayload)
	if !ok {
		return
	}
	e.harvestOutcome(ctx, job, payload, outcome)
}

// HandlePullRequestReverted fires the corrective OutcomeReverted harvest for an
// ORIGINAL PR that a (now-merged) revert PR undid (#467). It mirrors
// harvestOutcomeForMergeGate exactly: it resolves the original implement job via
// implementJobForTask (so the revert is attributed to the right template version,
// matched by Repo+PR or TaskID) and calls harvestOutcome with Outcome{Kind:
// OutcomeReverted, Repo, PullRequest}; the Reverted projection needs only
// Repo+PullRequest (no HeadSHA / no CI read), so the daemon need not fetch the
// original head.
//
// It is best-effort and FAIL-SAFE end to end: a nil harvester (the default —
// auto_trace off) short-circuits to no-op, an invalid event or an unresolvable
// original implement job returns nil (skip, no rows written), and a Harvest error
// is swallowed inside harvestOutcome and recorded as an auto_trace_harvest_failed
// job event — so a revert-detection call can NEVER block or fail the daemon poll.
// Re-firing is naturally idempotent: the harvester's per-PR item_id re-upserts the
// SAME UNIQUE feedback row in place (corrective overwrite, row count unchanged),
// so repeated polls of the same persistent revert PR are harmless.
func (e Engine) HandlePullRequestReverted(ctx context.Context, event RevertEvent) error {
	if e.OutcomeHarvester == nil {
		// No harvester (auto_trace off) => byte-identical no-op, no lookup.
		return nil
	}
	repo := strings.TrimSpace(event.Repo)
	if repo == "" || event.OriginalPullRequest <= 0 {
		// Nothing to anchor the corrective overwrite to; skip rather than guess.
		return nil
	}
	reviewPayload := JobPayload{
		Repo:        repo,
		PullRequest: event.OriginalPullRequest,
		Branch:      strings.TrimSpace(event.OriginalBranch),
		TaskID:      strings.TrimSpace(event.OriginalTaskID),
	}
	job, payload, ok := e.implementJobForTask(ctx, reviewPayload)
	if !ok {
		// No implement job owns the original PR (e.g. it was opened outside the
		// implement flow, or auto-trace was enabled only after the original merge):
		// skip rather than create a spurious fresh negative row.
		return nil
	}
	e.harvestOutcome(ctx, job, payload, Outcome{
		Kind:        OutcomeReverted,
		Repo:        repo,
		PullRequest: event.OriginalPullRequest,
	})
	return nil
}

// implementJobForTask finds the implement job that produced the diff/PR for the
// task the given payload belongs to, so a merge-gate outcome fired from a review
// job is attributed to the right template version (#465). It prefers the most
// recent implement job for the same task/PR. Returns ok=false when none exists
// (e.g. a PR opened outside the implement flow) so the caller skips the harvest.
func (e Engine) implementJobForTask(ctx context.Context, reviewPayload JobPayload) (db.Job, JobPayload, bool) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return db.Job{}, JobPayload{}, false
	}
	var best db.Job
	var bestPayload JobPayload
	found := false
	for _, job := range jobs {
		if job.Type != "implement" {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			continue
		}
		if !sameTask(reviewPayload, payload) {
			continue
		}
		// Prefer the latest implement job so the freshest diff's template version is
		// the one credited. ListJobs orders by id and populates UpdatedAt; compare by
		// UpdatedAt then id as a stable, deterministic tiebreak.
		if !found || implementJobNewer(job, best) {
			best = job
			bestPayload = payload
			found = true
		}
	}
	return best, bestPayload, found
}

// implementJobNewer reports whether candidate is a later implement job than
// current, ordering by UpdatedAt then id so the harvester credits the freshest
// diff's template version deterministically.
func implementJobNewer(candidate db.Job, current db.Job) bool {
	if candidate.UpdatedAt != current.UpdatedAt {
		return candidate.UpdatedAt > current.UpdatedAt
	}
	return candidate.ID > current.ID
}

func (e Engine) mergeGateReviewRequired(ctx context.Context, payload JobPayload) (bool, error) {
	if len(e.requiredReviewers(payload)) > 0 {
		return true, nil
	}
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		jobPayload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return false, err
		}
		if sameTask(payload, jobPayload) {
			return true, nil
		}
	}
	return false, nil
}

func (e Engine) recordPullRequestBaseline(ctx context.Context, event PullRequestEvent) error {
	if event.PullRequest <= 0 {
		return nil
	}
	return e.Store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: event.Repo,
		Number:       int64(event.PullRequest),
		HeadBranch:   event.Branch,
		HeadSHA:      event.HeadSHA,
		State:        "open",
	})
}

func (e Engine) nextReviewRound(ctx context.Context, event PullRequestEvent) (string, error) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return "", err
	}
	current := JobPayload{Repo: event.Repo, PullRequest: event.PullRequest, TaskID: event.TaskID}
	rounds := map[string]bool{}
	existingHeadRound := ""
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return "", err
		}
		if !sameTask(current, payload) {
			continue
		}
		round := strings.TrimSpace(payload.ReviewRound)
		if round == "" {
			round = job.ID
		}
		if payload.HeadSHA != "" && payload.HeadSHA == event.HeadSHA {
			existingHeadRound = round
		}
		rounds[round] = true
	}
	if existingHeadRound != "" {
		return existingHeadRound, nil
	}
	return "review-" + strconv.Itoa(len(rounds)+1), nil
}

func (e Engine) reviewApprovalAlreadyAdvanced(ctx context.Context, ref taskRef) (bool, error) {
	if strings.TrimSpace(ref.ID) == "" {
		return false, nil
	}
	task, err := e.Store.GetTask(ctx, ref.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return task.State == string(TaskReadyToMerge) || task.State == string(TaskMerged), nil
}

// Escalation event kinds (#340). Escalation is round-based: a coordinator/leg can
// pause more than once over its lifetime (a retried leg that fails again opens a
// NEW round). The pause is OPEN while requested > resolved, and CLOSED (resolved
// or finalizable) while requested == resolved. So a second escalate_human pass
// during an OPEN round re-notifies nothing and re-records nothing (idempotent),
// but the first failure of a round whose every prior escalation was resolved
// opens a fresh round: a new requested event, a re-notify, and a re-pause.
const (
	escalationRequestedEvent = "delegation_escalation_requested"
	escalationResolvedEvent  = "delegation_escalation_resolved"
)

// EscalationRecord is the structured payload stored in a
// delegation_escalation_requested event message, so the resume path can resolve
// the failing leg and child job, and the wall-clock backstop can exclude the
// paused duration. It is JSON-encoded into the event message (job_events has no
// dedicated columns); PausedAt is RFC3339-UTC.
type EscalationRecord struct {
	DelegationID string `json:"delegation_id"`
	ChildJobID   string `json:"child_job_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Question     string `json:"question,omitempty"`
	PausedAt     string `json:"paused_at,omitempty"`
	// Kind discriminates the pause flavor (#445): "" (or "failure") is the
	// escalate_human failure pause; "ask" is the non-failure ask-gate pause opened
	// by a healthy result's human_questions[]. It rides the SAME
	// delegation_escalation_requested/_resolved event kinds so TTL, wall-clock
	// pause exclusion, and round-idempotency all apply unchanged; only the
	// human-facing rendering and the resume verb gating branch on it.
	Kind string `json:"kind,omitempty"`
	// Questions carries the ask-gate's human_questions[] on the requested event so
	// the notifier can render them and the resume can map answers by id. Empty for
	// a failure escalation.
	Questions []HumanQuestion `json:"questions,omitempty"`
	// Answers carries the human's parsed id->answer map on the resolved event of an
	// ask round. Empty for a failure escalation or an unanswered (TTL) resolution.
	Answers map[string]string `json:"answers,omitempty"`
}

// escalationKindAsk is the EscalationRecord.Kind discriminator for an ask-gate
// pause (#445); the failure-escalation pause leaves Kind empty.
const escalationKindAsk = "ask"

// pauseAwaitingHuman is the escalate_human analogue of block (#340): it sets the
// shared parent task to TaskAwaitingHuman, records a per-round
// delegation_escalation_requested event (idempotent WITHIN an open round via the
// requested>resolved guard, but a retried leg that fails again opens a fresh
// round and re-pauses), notifies the human best-effort through the injected
// EscalationNotifier, and returns an AwaitingHumanError so the caller enqueues NO
// continuation. The tree therefore consumes zero compute until an operator
// resumes it with `/gitmoot resume <coordinator> retry|continue|abort`.
func (e Engine) pauseAwaitingHuman(ctx context.Context, parentJob db.Job, parentPayload JobPayload, ref taskRef, d Delegation, child db.Job) error {
	reason := childFailureReason(child)
	awaitErr := AwaitingHumanError{Reason: fmt.Sprintf("delegation %q failed (failure_policy escalate_human): %s", d.ID, reason)}

	// While an escalation round is OPEN (requested > resolved), this is an
	// idempotent re-advance (a concurrent child completion, or the same child's
	// AdvanceJob re-running): keep the task in awaiting_human and return the same
	// pause error, but re-record nothing and re-notify nobody. When the round is
	// CLOSED (requested == resolved: a prior escalation was resolved, e.g. a retry
	// that has now failed AGAIN) we fall through to open a FRESH round below — a
	// new requested event, a re-pause, and a re-notify — so the tree never strands
	// permanently in planned with nothing in flight (#340).
	open, err := e.escalationOpen(ctx, parentJob.ID)
	if err != nil {
		return err
	}
	if open {
		return awaitErr
	}

	if err := e.setTaskState(ctx, ref, TaskAwaitingHuman); err != nil {
		return err
	}

	record := EscalationRecord{
		DelegationID: d.ID,
		ChildJobID:   child.ID,
		Reason:       reason,
		Question:     strings.TrimSpace(d.Prompt),
		PausedAt:     e.now().UTC().Format(time.RFC3339),
	}
	encoded, marshalErr := json.Marshal(record)
	message := awaitErr.Reason
	if marshalErr == nil {
		message = string(encoded)
	}
	if err := e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    escalationRequestedEvent,
		Message: message,
	}); err != nil {
		return err
	}

	// Notify the human best-effort: a nil notifier (ask-path/tests) or a notifier
	// error never fails the pause — the dashboard "Attention" section and the
	// recorded event are the durable source of truth; the comment is a courtesy.
	if e.EscalationNotifier != nil {
		_ = e.EscalationNotifier.NotifyEscalation(ctx, EscalationRequest{
			CoordinatorJobID: parentJob.ID,
			DelegationID:     d.ID,
			ChildJobID:       child.ID,
			Reason:           reason,
			Question:         strings.TrimSpace(d.Prompt),
			Repo:             firstNonEmptyString(ref.Repo, parentPayload.Repo),
			PullRequest:      parentPayload.PullRequest,
			Branch:           firstNonEmptyString(ref.Branch, parentPayload.Branch),
			TaskID:           ref.ID,
			TaskTitle:        ref.Title,
		})
	}

	// Emit a best-effort job.needs_attention on the FRESH escalation round (#446).
	// It is gated on the same one-shot path as the escalationRequested event above
	// (we only reach here when the round was CLOSED), so a re-advance does NOT
	// re-emit. nil-safe: no EventSink => no event. detail carries the redacted
	// question. This is the seam #445's ask-gate rides to emit its own
	// job.needs_attention. The coordinator job id is the subject so a consumer can
	// resume the right tree; root_id groups the run.
	rootID := strings.TrimSpace(parentPayload.RootJobID)
	if rootID == "" {
		rootID = parentJob.ID
	}
	events.EmitEvent(ctx, e.EventSink, events.NewEvent(
		events.EventJobNeedsAttention,
		parentJob.ID,
		rootID,
		firstNonEmptyString(ref.Repo, parentPayload.Repo),
		string(TaskAwaitingHuman),
		strings.TrimSpace(d.Prompt),
		e.now(),
		RedactCommentText,
	))

	return awaitErr
}

// pauseAwaitingHumanAnswer is the non-failure ask-gate sibling of
// pauseAwaitingHuman (#445): a HEALTHY job that returned human_questions[] pauses
// its task at awaiting_human for a specific human answer. It reuses the exact
// escalate_human pause plumbing — the same delegation_escalation_requested event
// kind (tagged Kind="ask" + the questions), the same escalationOpen round guard,
// and the same EscalationNotifier / job.needs_attention (#446) seam — so TTL
// auto-finalize, wall-clock pause exclusion, and round-idempotency all apply with
// no extra plumbing. It enqueues NO continuation and dispatches NO delegations
// (the caller short-circuits on a true return), so the pause is budget-neutral
// and consumes zero compute until the human resumes with `answer`.
//
// It returns whether the ask round is currently OPEN. true => the caller returns
// AwaitingHumanError (freshly opened, or an idempotent re-advance while awaiting).
// false => the round is CLOSED (the human already answered and the answer-driven
// continuation is in flight, or the ask was TTL-finalized): the caller
// short-circuits AdvanceJob without redispatching.
//
// When the asking job is a delegation CHILD (#445), the round is keyed/recorded/
// routed on its COORDINATOR (payload.ParentJobID) exactly like the escalate_human
// failure pause (which keys on parentJob.ID), NOT on the child's own id. This
// preserves the frozen single-round-per-parent invariant: the FIRST asking sibling
// opens the one shared round and subsequent siblings hit the open-round guard, the
// shared parent task flips once, and the human resumes the COORDINATOR (whose
// continuation carries the answer) — not a child that the parent DAG would advance
// past independently.
func (e Engine) pauseAwaitingHumanAnswer(ctx context.Context, job db.Job, payload JobPayload, ref taskRef) (bool, error) {
	// Route the pause to the coordinator when the asking job is a delegation child:
	// the escalation event, round guard, notifier target, and task ref all key on
	// the coordinator so siblings share one round (mirrors pauseAwaitingHuman).
	targetID := job.ID
	targetRef := ref
	if parentID := strings.TrimSpace(payload.ParentJobID); parentID != "" {
		targetID = parentID
		if _, parentPayload, perr := e.jobPayload(ctx, parentID); perr == nil {
			if pref := taskRefFromPayload(parentPayload); pref.ID != "" {
				targetRef = pref
			}
		}
	}

	// While ANY escalation round is OPEN (requested > resolved) on the target
	// (coordinator for a child, else self), this is an idempotent re-advance: keep
	// the pause, re-record nothing, re-notify nobody.
	open, err := e.escalationOpen(ctx, targetID)
	if err != nil {
		return false, err
	}
	if open {
		return true, nil
	}

	// No OPEN round. If an ask round was already opened AND resolved for the target,
	// the human has answered (or TTL-finalized) and the answer-driven continuation
	// is the asking job's sole continuation: report CLOSED so the caller
	// short-circuits without re-pausing or redispatching. Mirrors the
	// escalate_human round model, but an asking job is never re-run to open a
	// second ask round (only a retried failing leg re-pauses), so a resolved ask
	// round stays closed.
	if _, exists, lerr := e.loadEscalation(ctx, targetID); lerr != nil {
		return false, lerr
	} else if exists {
		// A child that asks while the coordinator's round was already opened+resolved
		// by a sibling is CLOSED (the shared answer-continuation is in flight); a
		// resolved/non-ask round on the coordinator is also not ours to reopen.
		return false, nil
	}

	// Open a FRESH ask round: pause the (coordinator's) task, record the requested
	// event with the questions on the target, notify best-effort, and emit
	// job.needs_attention once. All keyed on targetID/targetRef so a child's ask
	// routes to its coordinator.
	if err := e.setTaskState(ctx, targetRef, TaskAwaitingHuman); err != nil {
		return false, err
	}

	questions := payload.Result.HumanQuestions
	record := EscalationRecord{
		Reason:    fmt.Sprintf("%d human question(s) awaiting an answer", len(questions)),
		Question:  renderHumanQuestions(questions),
		PausedAt:  e.now().UTC().Format(time.RFC3339),
		Kind:      escalationKindAsk,
		Questions: questions,
	}
	encoded, marshalErr := json.Marshal(record)
	message := record.Reason
	if marshalErr == nil {
		message = string(encoded)
	}
	if err := e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   targetID,
		Kind:    escalationRequestedEvent,
		Message: message,
	}); err != nil {
		return false, err
	}

	// Notify the human best-effort (nil notifier / notifier error never fails the
	// pause): the recorded event + dashboard Attention are the durable truth. The
	// CoordinatorJobID is the resume target (the coordinator for a child's ask) so
	// the human resumes the job whose continuation actually carries the answer.
	if e.EscalationNotifier != nil {
		_ = e.EscalationNotifier.NotifyEscalation(ctx, EscalationRequest{
			CoordinatorJobID: targetID,
			Question:         renderHumanQuestions(questions),
			Ask:              true,
			Questions:        questions,
			Repo:             firstNonEmptyString(targetRef.Repo, payload.Repo),
			PullRequest:      payload.PullRequest,
			Branch:           firstNonEmptyString(targetRef.Branch, payload.Branch),
			TaskID:           targetRef.ID,
			TaskTitle:        targetRef.Title,
		})
	}

	// Emit a best-effort job.needs_attention on the FRESH ask round via the SAME
	// #446 EventSink the failure escalation rides (NOT a parallel notify path).
	// One-shot (we only reach here when no round was open) and nil-safe. The subject
	// is the resume target (coordinator for a child) so a consumer resumes the right
	// tree; root_id groups the run.
	rootID := strings.TrimSpace(payload.RootJobID)
	if rootID == "" {
		rootID = targetID
	}
	events.EmitEvent(ctx, e.EventSink, events.NewEvent(
		events.EventJobNeedsAttention,
		targetID,
		rootID,
		firstNonEmptyString(targetRef.Repo, payload.Repo),
		string(TaskAwaitingHuman),
		renderHumanQuestions(questions),
		e.now(),
		RedactCommentText,
	))

	return true, nil
}

// renderHumanQuestions renders the ask-gate questions as a compact, human-facing
// multi-line block ("- <id>: <prompt> (choices: ...)") used both as the recorded
// Question text and the needs_attention detail. Empty for no questions.
func renderHumanQuestions(questions []HumanQuestion) string {
	if len(questions) == 0 {
		return ""
	}
	var b strings.Builder
	for i, q := range questions {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "- %s: %s", strings.TrimSpace(q.ID), strings.TrimSpace(q.Prompt))
		if len(q.Choices) > 0 {
			fmt.Fprintf(&b, " (choices: %s)", strings.Join(q.Choices, ", "))
		}
	}
	return b.String()
}

// firstNonEmptyString returns the first non-blank value, or "".
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// loadEscalation returns the structured escalation record recorded on a paused
// coordinator job, and whether one exists. It tolerates a legacy/plain-text
// message (pre-JSON) by returning a record with only the raw reason, so a resume
// still routes.
func (e Engine) loadEscalation(ctx context.Context, coordinatorJobID string) (EscalationRecord, bool, error) {
	events, err := e.Store.ListJobEvents(ctx, coordinatorJobID)
	if err != nil {
		return EscalationRecord{}, false, err
	}
	var rec EscalationRecord
	found := false
	for _, ev := range events {
		if ev.Kind != escalationRequestedEvent {
			continue
		}
		found = true
		if json.Unmarshal([]byte(ev.Message), &rec) != nil {
			rec = EscalationRecord{Reason: ev.Message}
		}
	}
	if !found {
		return EscalationRecord{}, false, nil
	}
	return rec, true, nil
}

// escalationRoundCounts returns how many escalation rounds were requested vs
// resolved for a coordinator (#340). Escalation is round-based: a retried leg
// that fails again opens a fresh round, so a coordinator can accumulate several
// requested/resolved pairs over its lifetime. The pause is OPEN (resolvable /
// finalizable) while requested > resolved, and CLOSED while requested ==
// resolved.
func (e Engine) escalationRoundCounts(ctx context.Context, coordinatorJobID string) (requested, resolved int, err error) {
	events, err := e.Store.ListJobEvents(ctx, coordinatorJobID)
	if err != nil {
		return 0, 0, err
	}
	for _, ev := range events {
		switch ev.Kind {
		case escalationRequestedEvent:
			requested++
		case escalationResolvedEvent:
			resolved++
		}
	}
	return requested, resolved, nil
}

// escalationOpen reports whether a coordinator currently has an UNRESOLVED
// escalation round (requested > resolved): the tree is paused awaiting a human
// right now. When false, either no escalation ever opened, or every round that
// opened has been resolved (the leg may then fail AGAIN to open a new round).
func (e Engine) escalationOpen(ctx context.Context, coordinatorJobID string) (bool, error) {
	requested, resolved, err := e.escalationRoundCounts(ctx, coordinatorJobID)
	if err != nil {
		return false, err
	}
	return requested > resolved, nil
}

// escalationResolved reports whether the coordinator has no OPEN escalation round
// to act on — i.e. every requested escalation has been resolved (requested ==
// resolved). The resume path and the TTL backstop are idempotency-guarded by
// this check: an OPEN round (requested > resolved) is resolvable/finalizable, a
// CLOSED one is a no-op. Round-based so a re-pause after a failed retry is again
// resolvable, never permanently stranded (#340).
func (e Engine) escalationResolved(ctx context.Context, coordinatorJobID string) (bool, error) {
	open, err := e.escalationOpen(ctx, coordinatorJobID)
	if err != nil {
		return false, err
	}
	return !open, nil
}

// ResumeDecision is one of the three human resume verbs for a paused tree (#340).
type ResumeDecision string

const (
	// ResumeRetry re-enqueues the failing delegation leg with the human's
	// instructions folded into its prompt.
	ResumeRetry ResumeDecision = "retry"
	// ResumeContinue proceeds the coordinator continuation (today's escalate path,
	// now human-approved): the coordinator synthesizes every child outcome.
	ResumeContinue ResumeDecision = "continue"
	// ResumeAbort routes to the #305 graceful finalize continuation for a terminal
	// best-effort synthesis of whatever completed.
	ResumeAbort ResumeDecision = "abort"
	// ResumeAnswer delivers a human's answer(s) to an ask-gate pause (#445): it
	// enqueues the coordinator continuation carrying the answer text. It is valid
	// only on an ask round (Kind="ask"); the retry/continue/abort verbs are valid
	// only on a failure-escalation round.
	ResumeAnswer ResumeDecision = "answer"
)

// validResumeDecision normalizes and validates a resume verb.
func validResumeDecision(decision string) (ResumeDecision, bool) {
	switch ResumeDecision(strings.ToLower(strings.TrimSpace(decision))) {
	case ResumeRetry:
		return ResumeRetry, true
	case ResumeContinue:
		return ResumeContinue, true
	case ResumeAbort:
		return ResumeAbort, true
	case ResumeAnswer:
		return ResumeAnswer, true
	default:
		return "", false
	}
}

// ParseResumeDecision is the exported normalizer the daemon uses to validate the
// human's resume verb before calling ResolveEscalation (#340).
func ParseResumeDecision(decision string) (ResumeDecision, bool) {
	return validResumeDecision(decision)
}

// ResolveEscalation resumes a tree paused at TaskAwaitingHuman (#340). The
// coordinatorJobID is the job that recorded the escalation (the resume target the
// notification quoted). decision selects the verb; instructions is the human's
// optional guidance, folded into the retried leg's prompt (retry) and recorded on
// the resolution event (all verbs). It is idempotent: a second resume on an
// already-resolved coordinator is a no-op. It clears TaskAwaitingHuman and records
// a delegation_escalation_resolved event carrying resolved_at so the wall-clock
// backstop can close the pause window. The caller (daemon handleResumeCommand) is
// authorize-commenter gated.
func (e Engine) ResolveEscalation(ctx context.Context, coordinatorJobID string, decision ResumeDecision, instructions string) error {
	if err := e.validate(); err != nil {
		return err
	}
	verb, ok := validResumeDecision(string(decision))
	if !ok {
		return fmt.Errorf("invalid resume decision %q; want retry|continue|abort|answer", decision)
	}

	rec, exists, err := e.loadEscalation(ctx, coordinatorJobID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("job %s has no pending human escalation", coordinatorJobID)
	}
	resolved, err := e.escalationResolved(ctx, coordinatorJobID)
	if err != nil {
		return err
	}
	if resolved {
		// Already answered: idempotent no-op so a duplicate comment poll cannot
		// re-run the verb (which could double-enqueue a leg or continuation).
		return nil
	}

	// Verb/round-kind gating (#445): the ask-gate's `answer` verb is valid ONLY on
	// an ask round, and the failure verbs (retry/continue/abort) ONLY on a failure
	// escalation round. A mismatch is a clear, side-effect-free error so a human who
	// sends the wrong verb gets a precise ack and the pause stays intact.
	isAskRound := rec.Kind == escalationKindAsk
	if verb == ResumeAnswer && !isAskRound {
		return fmt.Errorf("job %s is paused on a failure escalation, not an ask; resume it with retry|continue|abort, not answer", coordinatorJobID)
	}
	if verb != ResumeAnswer && isAskRound {
		return fmt.Errorf("job %s is awaiting a human answer; resume it with `answer \"<id>: ...\"`, not %s", coordinatorJobID, verb)
	}

	parentJob, parentPayload, err := e.jobPayload(ctx, coordinatorJobID)
	if err != nil {
		return err
	}
	if parentPayload.Result == nil {
		return fmt.Errorf("job %s has no result to resume", coordinatorJobID)
	}
	ref := taskRefFromPayload(parentPayload)

	// answers is populated only on the ask-gate `answer` verb; it is recorded on the
	// resolution event (parsed + any unmatched ids) and threaded into the continuation.
	var answers map[string]string

	switch verb {
	case ResumeRetry:
		if err := e.resumeRetryLeg(ctx, parentJob, parentPayload, rec, instructions); err != nil {
			return err
		}
	case ResumeContinue:
		children, err := e.childDelegationJobs(ctx, parentJob.ID)
		if err != nil {
			return err
		}
		if err := e.maybeEnqueueContinuation(ctx, parentJob, parentPayload, parentPayload.Result, children, ref); err != nil {
			return err
		}
	case ResumeAbort:
		reason := "human aborted the escalation"
		if strings.TrimSpace(instructions) != "" {
			reason = "human aborted the escalation: " + strings.TrimSpace(instructions)
		}
		if err := e.enqueueFinalizeContinuation(ctx, parentJob, parentPayload, reason); err != nil {
			return err
		}
	case ResumeAnswer:
		answers = parseHumanAnswers(rec.Questions, instructions)
		if err := e.resumeAnswerLeg(ctx, parentJob, parentPayload, ref, rec, answers); err != nil {
			return err
		}
	}

	// Clear the pause: move the task out of awaiting_human. retry/continue re-arm
	// the delegation machinery (the next child completion advances the DAG, or the
	// continuation runs), so move to reviewing-ish "implementing" intent; abort's
	// finalize continuation will settle the task itself. We use planned as a
	// neutral non-terminal state so the dashboard stops listing it under Attention.
	if err := e.setTaskState(ctx, ref, TaskPlanned); err != nil {
		return err
	}

	resolution := EscalationRecord{
		DelegationID: rec.DelegationID,
		ChildJobID:   rec.ChildJobID,
		Reason:       string(verb),
		Question:     strings.TrimSpace(instructions),
		PausedAt:     e.now().UTC().Format(time.RFC3339), // reused as resolved_at
		Kind:         rec.Kind,
		Answers:      answers,
	}
	message := string(verb)
	if encoded, marshalErr := json.Marshal(resolution); marshalErr == nil {
		message = string(encoded)
	}
	return e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   coordinatorJobID,
		Kind:    escalationResolvedEvent,
		Message: message,
	})
}

// resumeRetryLeg re-enqueues the failing delegation leg of a paused tree with the
// human's instructions folded into its prompt, under a deterministic resume id so
// a duplicate resume cannot double-enqueue it. It is the retry verb's worker.
func (e Engine) resumeRetryLeg(ctx context.Context, parentJob db.Job, parentPayload JobPayload, rec EscalationRecord, instructions string) error {
	d, ok := findDelegation(parentPayload.Result.Delegations, rec.DelegationID)
	if !ok {
		return fmt.Errorf("escalated delegation %q not found on job %s", rec.DelegationID, parentJob.ID)
	}
	if instr := strings.TrimSpace(instructions); instr != "" {
		d.Prompt = strings.TrimSpace(d.Prompt) + "\n\nHuman guidance on resume: " + instr
	}
	artifactDir, err := delegationArtifactDir(e.ArtifactRoot, parentJob.ID, parentPayload.Result)
	if err != nil {
		return err
	}
	request := e.delegationRequest(parentJob, parentPayload, d)
	request.ID = parentJob.ID + "/delegation/" + d.ID + "/resume"
	request.Instructions = strings.TrimSpace(d.Prompt)
	request.DelegationArtifactDir = artifactDir
	if err := e.allocateAndEnqueueDelegation(ctx, parentJob, parentPayload, d, request, taskRefFromPayload(parentPayload)); err != nil {
		return err
	}
	return e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    "delegation_escalation_retry",
		Message: fmt.Sprintf("human resume retry re-enqueued delegation %q as job %s", d.ID, request.ID),
	})
}

// parseHumanAnswers parses the human's free-form `answer` instructions into an
// id->answer map (#445). The instruction body is multi-line; each line of the
// form "<id>: <text>" maps the answer to the matching question id. An id that
// does not match any open question is recorded under its literal key so it is
// surfaced (never silently dropped) rather than failing the resume. When the body
// has no recognizable "<id>:" prefix at all AND there is exactly one open
// question, the whole body is taken as that question's answer (the common
// single-question convenience). Returns nil when nothing parses, so the
// resolution event omits the answers map.
func parseHumanAnswers(questions []HumanQuestion, instructions string) map[string]string {
	known := make(map[string]struct{}, len(questions))
	for _, q := range questions {
		known[strings.TrimSpace(q.ID)] = struct{}{}
	}
	answers := make(map[string]string)
	matchedAny := false
	lastID := ""
	for _, line := range strings.Split(instructions, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			id := strings.TrimSpace(line[:idx])
			text := strings.TrimSpace(line[idx+1:])
			if id != "" {
				answers[id] = text
				lastID = id
				if _, ok := known[id]; ok {
					matchedAny = true
				}
				continue
			}
		}
		// A line with no "<id>:" prefix continues the most recently parsed answer
		// (a multi-line answer body); with no answer yet it is dropped (the
		// single-question convenience below covers a prefix-less single answer).
		if lastID != "" {
			answers[lastID] = strings.TrimSpace(answers[lastID] + "\n" + line)
		}
	}
	// Single-question convenience: if nothing matched a known id and there is exactly
	// one question, treat the whole body as that question's answer.
	if !matchedAny && len(questions) == 1 {
		if body := strings.TrimSpace(instructions); body != "" {
			return map[string]string{strings.TrimSpace(questions[0].ID): body}
		}
	}
	if len(answers) == 0 {
		return nil
	}
	return answers
}

// resumeAnswerLeg enqueues the coordinator continuation carrying the human's
// answer(s) to an ask-gate pause (#445). It mirrors the ResumeContinue path
// (maybeEnqueueContinuation under the deterministic continuation id, idempotent
// via the continuationEnqueued guard) but threads the parsed answers into the
// continuation request's HumanAnswer field so buildContinuationPrompt renders a
// clearly-labelled "Human answers to your questions" block at the top of the
// coordinator's continuation prompt. It is the `answer` verb's worker.
func (e Engine) resumeAnswerLeg(ctx context.Context, parentJob db.Job, parentPayload JobPayload, ref taskRef, rec EscalationRecord, answers map[string]string) error {
	children, err := e.childDelegationJobs(ctx, parentJob.ID)
	if err != nil {
		return err
	}
	answerBlock := renderHumanAnswerBlock(rec.Questions, answers)
	// The ask-gate short-circuits AdvanceJob BEFORE dispatchDelegations (engine.go
	// ask-gate block), so an asking result's delegations[] were NEVER dispatched —
	// no children exist for them (#445). Feeding the un-dispatched delegations into
	// maybeEnqueueContinuation would (a) make the vote/quorum synthesis gates fail
	// (every delegation id is missing from the empty children map) and block the
	// parent — silently losing the human's answer — and (b) make a verify
	// synthesis_rule emit a misleading replan continuation. It would also render
	// each delegation as "not enqueued (dependencies unmet)" in the continuation
	// prompt, which is wrong: they were never attempted. Resume the coordinator from
	// a copy of its result with Delegations cleared, so the answer-driven
	// continuation always enqueues and the coordinator decides fresh (it may
	// re-issue the same delegations now that it has the answer).
	resumeResult := *parentPayload.Result
	resumeResult.Delegations = nil
	if err := e.maybeEnqueueContinuation(ctx, parentJob, parentPayload, &resumeResult, children, ref, withHumanAnswer(answerBlock)); err != nil {
		return err
	}
	return e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    "delegation_ask_answered",
		Message: fmt.Sprintf("human answered %d ask-gate question(s) for job %s", len(rec.Questions), parentJob.ID),
	})
}

// renderHumanAnswerBlock renders the human's answers (#445) as a stable,
// id-ordered block keyed by each original question, so the coordinator
// continuation reads exactly which question got which answer. Questions with no
// answer are shown as "(no answer provided)"; any answer keyed to an unknown id
// (a typo the human made) is appended under an "unmatched" section so it is
// surfaced, never silently dropped.
func renderHumanAnswerBlock(questions []HumanQuestion, answers map[string]string) string {
	if len(questions) == 0 && len(answers) == 0 {
		return ""
	}
	var b strings.Builder
	matched := make(map[string]struct{}, len(answers))
	for _, q := range questions {
		id := strings.TrimSpace(q.ID)
		fmt.Fprintf(&b, "- %s — %s\n", id, strings.TrimSpace(q.Prompt))
		if ans, ok := answers[id]; ok {
			matched[id] = struct{}{}
			fmt.Fprintf(&b, "  answer: %s\n", strings.TrimSpace(ans))
		} else {
			b.WriteString("  answer: (no answer provided)\n")
		}
	}
	var unmatched []string
	for id := range answers {
		if _, ok := matched[id]; !ok {
			unmatched = append(unmatched, id)
		}
	}
	if len(unmatched) > 0 {
		sort.Strings(unmatched)
		b.WriteString("Unmatched answer ids (no such question; treat as additional human guidance):\n")
		for _, id := range unmatched {
			fmt.Fprintf(&b, "- %s: %s\n", id, strings.TrimSpace(answers[id]))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// findDelegation returns the delegation with the given id from a result's set.
func findDelegation(delegations []Delegation, id string) (Delegation, bool) {
	for _, d := range delegations {
		if d.ID == id {
			return d, true
		}
	}
	return Delegation{}, false
}

// AutoFinalizeExpiredEscalations scans for trees paused awaiting a human past the
// TTL and gracefully finalizes them (#340): a never-answered escalation must not
// strand a tree forever. For each coordinator with an unresolved
// delegation_escalation_requested whose paused_at is older than ttl, it routes to
// the #305 finalize continuation (synthesize what completed), clears
// TaskAwaitingHuman, and records a delegation_escalation_resolved event tagged
// "ttl" (so wall-clock pause accounting closes too). ttl <= 0 disables the scan.
// It returns the number of trees finalized. Idempotent: an already-resolved
// escalation is skipped, and the finalize continuation has a deterministic id.
func (e Engine) AutoFinalizeExpiredEscalations(ctx context.Context, ttl time.Duration) (int, error) {
	if err := e.validate(); err != nil {
		return 0, err
	}
	if ttl <= 0 {
		return 0, nil
	}
	// BOUNDED (#598): iterate ONLY the coordinators with an open escalation round
	// (requested > resolved) instead of listing EVERY job and re-reading each one's
	// full event history twice. Zero candidates => the loop body never runs, so this
	// is an immediate return with no per-job GetJob/ListJobEvents on the overwhelming
	// common case (no tree paused awaiting a human). The retained per-candidate
	// exists/resolved gates below are always-pass for a candidate but kept verbatim,
	// both to defend against an event added between this query and the walk and to
	// keep the finalization logic byte-identical.
	jobIDs, err := e.Store.JobIDsWithOpenEscalation(ctx)
	if err != nil {
		return 0, err
	}
	now := e.now().UTC()
	finalized := 0
	for _, jobID := range jobIDs {
		// A candidate escalation event whose job row was pruned would make the
		// payload load below fail; skip it here on sql.ErrNoRows, mirroring the
		// reclaim/retry bounded passes (#598). Unreachable today (nothing prunes
		// jobs), but keeps the pattern and the PollOnce caller robust to a future
		// job-pruning feature. The fetched job is reused for the payload below, so a
		// finalized candidate costs a single GetJob rather than the old
		// GetJob-then-jobPayload double read (#615 review).
		job, err := e.Store.GetJob(ctx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return finalized, err
		}
		rec, exists, err := e.loadEscalation(ctx, jobID)
		if err != nil {
			return finalized, err
		}
		if !exists {
			continue
		}
		resolved, err := e.escalationResolved(ctx, jobID)
		if err != nil {
			return finalized, err
		}
		if resolved {
			continue
		}
		pausedAt, perr := parseJobTimestamp(rec.PausedAt)
		if perr != nil {
			// Without a parseable paused_at we cannot age the pause; skip rather than
			// finalize prematurely.
			continue
		}
		if now.Sub(pausedAt) < ttl {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return finalized, err
		}
		if payload.Result == nil {
			continue
		}
		reason := fmt.Sprintf("escalation TTL of %s elapsed with no human response", ttl)
		if err := e.enqueueFinalizeContinuation(ctx, job, payload, reason); err != nil {
			return finalized, err
		}
		if err := e.setTaskState(ctx, taskRefFromPayload(payload), TaskPlanned); err != nil {
			return finalized, err
		}
		resolution := EscalationRecord{
			DelegationID: rec.DelegationID,
			ChildJobID:   rec.ChildJobID,
			Reason:       "ttl",
			PausedAt:     now.Format(time.RFC3339), // reused as resolved_at
		}
		message := "ttl"
		if encoded, marshalErr := json.Marshal(resolution); marshalErr == nil {
			message = string(encoded)
		}
		if err := e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   jobID,
			Kind:    escalationResolvedEvent,
			Message: message,
		}); err != nil {
			return finalized, err
		}
		finalized++
	}
	return finalized, nil
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
	// duplicate (mirrors StartTaskBranch's reuse check).
	if task.Branch != "" {
		byBranch, berr := e.Store.GetTaskByRepoBranch(ctx, task.RepoFullName, task.Branch)
		if berr != nil && !errors.Is(berr, sql.ErrNoRows) {
			return berr
		}
		if berr == nil && byBranch.ID != task.ID {
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
