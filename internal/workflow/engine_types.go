package workflow

import (
	"context"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

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
	mb := Mailbox{Store: e.Store, RequireWorkflowPolicy: e.RequireWorkflowPolicy, OrgPolicy: e.OrgPolicy, CanaryEnabled: e.CanaryEnabled, deferBlocker: e.BlockerDeferrer, RuntimeDefaultModel: e.RuntimeDefaultModel, RuntimeDefaultEffort: e.RuntimeDefaultEffort, routerContextEnabled: e.RouterContextEnabled, resultCheckMode: normalizeResultCheckMode(e.ResultCheckMode), produceCheckDir: e.ProduceCheckDir}
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
	// HumanMergeRequested records an explicit authorized @gitmoot merge command.
	// It permits the native policy gate to merge even when automatic merging is
	// disabled for the repository; ordinary daemon advancement leaves this false.
	HumanMergeRequested bool
	// SkipReviewFanout, when true, suppresses Gitmoot's native PR advancement in
	// HandlePullRequestOpened: zero review jobs are enqueued, the PR baseline is
	// recorded, and the native merge gate is not run. Council-style external
	// orchestrators use this to make their own gate the only merge authority.
	// Defaults false => full native fanout/advancement.
	SkipReviewFanout bool
	// Labels carries the PR's GitHub label names, used only by the opt-in risk
	// classifier (#650) when RiskTiersEnabled. Additive: empty (the default) means
	// the classifier falls back to path/default signals, and with risk tiers off
	// it is never read. The daemon PR-watcher populates it best-effort.
	Labels []string
	// ChangedPaths carries the PR's changed file paths (repo-relative), used only
	// by the opt-in risk classifier (#650) when RiskTiersEnabled. Additive: empty
	// (the default) means the classifier falls back to label/default signals, and
	// with risk tiers off it is never read. The daemon populates it best-effort
	// (a lookup failure leaves it empty rather than blocking the review).
	ChangedPaths []string
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
	// HumanMergeRequested is an explicit, authorized human instruction. It is
	// evaluated inside PolicyMergeGate, never by a caller-side bypass.
	HumanMergeRequested bool
}

type MergeDecision struct {
	Ready          bool
	Merged         bool
	MergeCommitSHA string
	Reason         string
	// LeaveOpen is a terminal-ish native merge-gate outcome: the pull request is
	// deliberately left for a human action. It is distinct from Deferred, which
	// must be retried automatically, and from a blocked quality/process failure.
	LeaveOpen bool
	// Deferred marks a transient, retry-later hold (for example, a job is in
	// flight on the pull-request branch). Unlike a block, runMergeGate parks the
	// task in ready_to_merge so the daemon re-evaluates it on a later tick; a
	// deferred decision neither blocks nor fails the task. It is the explicit
	// sibling of PolicyMergeGate.pending, which expresses the same "stay
	// ready_to_merge, retry next tick" outcome as a Ready:true (unmerged)
	// decision — do not collapse the two without preserving that parking.
	Deferred bool
	// BlockClass classifies a not-ready decision (Ready=false) so the Mode-A
	// trace-harvester (#465) only scores AUTHORITATIVE template-quality rejections as
	// a negative and skips transient/infra blocks (branch staleness, dirty local
	// worktree, missing-SHA/base, freshness-unknown). It is the zero value
	// (MergeBlockNone) for a ready/merged decision and is purely advisory — it never
	// changes the transition itself (Deferred controls retry-later semantics), so
	// behavior is byte-identical when the harvester is off.
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
