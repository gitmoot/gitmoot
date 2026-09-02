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
	mb := NewMailbox(e.Store, e.ResolveDeliveryWorktree)
	mb.CollectChangeSet = e.CollectChangeSet
	mb.ApplyChangeSet = e.ApplyChangeSet
	mb.RequireWorkflowPolicy = e.RequireWorkflowPolicy
	mb.OrgPolicy = e.OrgPolicy
	mb.deferBlocker = e.BlockerDeferrer
	mb.RuntimeDefaultModel = e.RuntimeDefaultModel
	mb.RuntimeDefaultEffort = e.RuntimeDefaultEffort
	mb.routerContextEnabled = e.RouterContextEnabled
	mb.resultCheckMode = normalizeResultCheckMode(e.ResultCheckMode)
	// Session review jobs close through the Mailbox and never run AdvanceJob, so
	// they need the same repository policy the advancement path uses in order to
	// record the folded outcome (#1685-adjacent): without it the merge gate folds
	// their sub-threshold verdict into an approval while gitmoot proof, which keys
	// its claim on the durable event, renders "0 approved".
	mb.reviewBlockingSeverity = e.reviewBlockingSeverity
	mb.produceCheckDir = e.ProduceCheckDir
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
		event := events.NewEvent(
			eventType,
			jobID,
			rootID,
			payload.Repo,
			string(state),
			detail,
			e.now(),
			RedactCommentText,
		)
		requesterRole := NormalizeActingOrgRole(payload.ActingOrgRole)
		wakeTargetRole := requesterRole
		if state == JobSucceeded && payload.PullRequest > 0 && payload.Result != nil && e.Store != nil {
			job, jobErr := e.Store.GetJob(ctx, jobID)
			// A fan-out is excluded: EventCauseReviewVerdict tells the PR owner a
			// verdict landed, and waking them with ReviewDecision=approved because a
			// coordinator announced a panel reports an answer nobody gave (#1685).
			// The delegates' own terminal events carry the real verdicts.
			if jobErr == nil && strings.EqualFold(strings.TrimSpace(job.Type), "review") && !ResultIsFanOut(payload.Result) {
				decision := effectiveReviewDecisionForPayload(payload, e.reviewBlockingSeverity(payload.Repo))
				if decision == "approved" || decision == "changes_requested" {
					owner := ""
					resolved, resolveErr := e.Store.ResolvePullRequestOwner(
						ctx, payload.Repo, payload.Branch, payload.PullRequest, payload.TaskID,
					)
					if resolveErr == nil {
						owner = NormalizeActingOrgRole(resolved)
					}
					if resolveErr == nil && owner == "" {
						// Persistent seats implement directly rather than through
						// implement jobs. Review dispatch still persists the
						// registered implementing seat as LeadAgent.
						owner = NormalizeActingOrgRole(payload.LeadAgent)
					}
					event.Cause = events.EventCauseReviewVerdict
					wakeTargetRole = owner
					event.PullRequest = payload.PullRequest
					event.ReviewDecision = decision
					if decision == "changes_requested" {
						// Sender is a transport or agent identity, not necessarily a
						// routable org role. ActingOrgRole is the persisted requester
						// role paired with that sender; legacy jobs without one keep
						// the resolved PR owner fallback above (#1712).
						if requesterRole != "" {
							wakeTargetRole = requesterRole
						}
					}
					if requesterRole != "" {
						event.WakeTargetRoles = append(event.WakeTargetRoles, requesterRole)
					}
					if owner != "" && !strings.EqualFold(owner, requesterRole) {
						event.WakeTargetRoles = append(event.WakeTargetRoles, owner)
					}
				}
			}
		}
		event.WakeTargetRole = wakeTargetRole
		events.EmitEvent(ctx, e.EventSink, event)
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
	Repo        string
	Branch      string
	PullRequest int
	// PullRequestDraft is the forge-observed draft state. Draft PRs are not
	// merge-gate eligible and do not represent a pending human merge decision.
	PullRequestDraft        bool
	PullRequestDraftUnknown bool
	// PullRequestMerged is an authoritative forge observation. It permits only
	// the production wrapper's active-job deferral bypass; PolicyMergeGate still
	// re-reads the pull request before applying terminal effects.
	PullRequestMerged bool
	HeadSHA           string
	GoalID            string
	TaskID            string
	TaskTitle         string
	LeadAgent         string
	Sender            string
	RequiredReviewers []string
	// ActingOrgRole is the org attribution carried from the branch lock (#1250).
	// BOTH PR-open triggers read it from that one durable source, so native review
	// fanout children inherit an attribution instead of being enqueued
	// unattributed — and an unattributed job's blocked event has no owner to wake
	// (#1347). Empty means unattributed, which is also the legacy value for locks
	// predating the migration: the fanout then behaves exactly as it does today.
	ActingOrgRole string
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

type MergeRequest struct {
	Repo                    string
	Branch                  string
	PullRequest             int
	PullRequestDraft        bool
	PullRequestDraftUnknown bool
	// PullRequestMerged carries an authoritative forge observation through the
	// production wrapper. It never decides the merge outcome by itself.
	PullRequestMerged bool
	// TerminalRecoveryOnly permits an authoritative merged hint to re-read the
	// forge and finish terminal recovery while merge initiation is disabled.
	// An open or closed-unmerged pull request must still leave without merging.
	TerminalRecoveryOnly bool
	HeadSHA              string
	TaskID               string
	// ExpectedTaskState asks PolicyMergeGate to revalidate and hold this task
	// state through the irreversible MergePullRequest call. Empty preserves
	// callers that do not own a task-state precondition.
	ExpectedTaskState string
	WorkflowID        string
	Reviewer          string
	ReviewOptional    bool
	// ReviewBlockingSeverity is the resolved repository threshold carried into
	// the merge gate. Empty preserves the historical block-all behavior.
	ReviewBlockingSeverity string
	// HumanMergeRequested is an explicit, authorized human instruction. It is
	// evaluated inside PolicyMergeGate, never by a caller-side bypass.
	HumanMergeRequested bool
}

type MergeDecision struct {
	Ready          bool
	Merged         bool
	MergeCommitSHA string
	// Reason is a VALUE, not prose: see MergeReason. Carrying parts means no consumer
	// holds a string it can append an instruction onto (#1381).
	Reason MergeReason
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

type FixWorktreeRequest struct {
	JobID  string
	Repo   string
	Branch string
}

type FixWorktreeAllocation struct {
	Path    string
	Created bool
}

type FixWorktreeAllocator func(context.Context, FixWorktreeRequest) (FixWorktreeAllocation, error)

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

type BlockedError struct {
	Reason string
	// ResultDeliveryFailed distinguishes a job whose stored result did not reach
	// its required delivery surface (commit, push, or PR) from a downstream
	// workflow precondition such as pending CI. Only the former invalidates the
	// job's terminal result and outward decision.
	ResultDeliveryFailed bool
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
