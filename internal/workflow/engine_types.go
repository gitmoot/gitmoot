package workflow

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

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
// EnqueueMailbox is e.mailbox() for callers outside this package (#2186 round
// 4). It exists so a producer that holds an Engine can never enqueue through a
// LESS configured mailbox than the engine's own.
//
// The daemon's PR-comment producer used to build a fresh Mailbox and forward
// three fields by name; ReviewModelPool was the fourth and was not in the list,
// so the daemon held a home-aware resolver and discarded it. Enumerated
// forwarding is the defect: the next home-aware field is forgotten at the same
// site the same way. One call site, no field list, exhaustive by construction.
func (e Engine) EnqueueMailbox() Mailbox {
	return e.mailbox()
}

func (e Engine) mailbox() Mailbox {
	mb := NewMailbox(e.Store)
	mb.RequireWorkflowPolicy = e.RequireWorkflowPolicy
	mb.OrgPolicy = e.OrgPolicy
	mb.deferBlocker = e.BlockerDeferrer
	mb.RuntimeDefaultModel = e.RuntimeDefaultModel
	// #2224: the Mailbox owns the delivery-failure path, where a dead review's
	// findings are salvaged. The ledger writer lives here, so hand it over as a
	// closure rather than duplicating the writer.
	mb.RecordReviewFindings = func(ctx context.Context, jobID string) error {
		job, payload, err := e.jobPayload(ctx, jobID)
		if err != nil {
			return err
		}
		return e.RecordReviewFindingsToLedger(ctx, job, payload)
	}
	mb.ReviewModelPool = e.ReviewModelPool
	mb.RuntimeDefaultEffort = e.RuntimeDefaultEffort
	mb.resultCheckMode = normalizeResultCheckMode(e.ResultCheckMode)
	// Session review jobs close through the Mailbox and never run AdvanceJob, so
	// they need the same repository policy the advancement path uses in order to
	// record the folded outcome (#1685-adjacent): without it the merge gate folds
	// their sub-threshold verdict into an approval while the durable event history
	// shows no approval.
	mb.reviewBlockingSeverity = e.reviewBlockingSeverity
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
			if jobErr == nil && strings.EqualFold(strings.TrimSpace(job.Type), "review") {
				decision := effectiveReviewDecision(payload.Result, e.reviewBlockingSeverity(payload.Repo))
				if decision == "approved" || decision == "changes_requested" {
					owner := ""
					resolved, resolveErr := e.Store.ResolvePullRequestOwner(
						ctx, payload.Repo, payload.PullRequest, payload.TaskID,
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
					// #1945: THE WAKE ANNOUNCES THE REVIEWER'S RECORDED DECISION, NOT
					// THE GATE'S TRANSFORMED ONE. `decision` above is
					// threshold-adjusted, and that adjustment is correct for the merge
					// gate: a changes_requested whose severity is below the repository
					// bar does not block a merge. It is NOT correct for a human
					// notification. review_threshold.go:42 folds changes_requested to
					// "approved" when the severity does not block, so the wake told a
					// coordinator `approved` for a job row that records
					// changes_requested. Measured twice on #1943, hours apart, with
					// distinct reviewer payload shapes. `approved` is precisely the
					// word that suppresses the reflex to reread the row.
					//
					// ONLY THE ANNOUNCED VALUE CHANGES. Admission above and the
					// changes_requested retargeting below deliberately keep the
					// threshold-effective decision: which wake fires, and who
					// receives it, are gate concerns and are not this defect. The
					// only divergence the transform can produce is
					// changes_requested -> approved, so whenever admission passes the
					// recorded word is also one of the two admitted words; there is
					// no input where this widens the wake vocabulary, and
					// TestReviewVerdictWakeVocabularyStaysTwoWords pins that rather
					// than a defensive branch that no mutant could kill.
					event.ReviewDecision = strings.TrimSpace(payload.Result.Decision)
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
					// An explicit waiter receives the durable fact notice. The
					// legacy event is only for recipients without that obligation.
					targets := event.WakeTargetRoles[:0]
					for _, role := range event.WakeTargetRoles {
						subscribed, err := e.Store.HasReviewSubscription(ctx, role, payload.Repo, payload.PullRequest,
							payload.HeadSHA, db.ReviewRequestPurpose(payload.ReviewPurpose, payload.PostMergeReview, payload.ReviewPart))
						if err != nil {
							slog.Warn("review notification subscription lookup failed", "job_id", jobID, "role", role, "error", err)
						}
						if !subscribed {
							targets = append(targets, role)
						} else if role == wakeTargetRole {
							wakeTargetRole = ""
						}
					}
					event.WakeTargetRoles = targets
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
	// DispatchedBy is the identity that caused this fan-out to be asked for
	// (#1967). Both PR-open triggers carry the implementing seat: the daemon
	// PR-watcher reads the branch lock owner, the in-process trigger uses the
	// advancing implement job's agent. It is NOT the reviewer and NOT LeadAgent
	// by coincidence - LeadAgent happens to be the lock owner here, but on a CLI
	// dispatch it is the reviewer itself, so the two must not be conflated.
	// Empty degrades to Sender at the enqueue chokepoint.
	DispatchedBy string
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
	// FindingsAdvisory carries the repository's #1969 declaration: when true,
	// unobserved ledger obligations are RECORDED and REPORTED but do not hold
	// the merge. Empty/false preserves today's behaviour, so an undeclared
	// repository is unchanged and only an explicit operator declaration relaxes
	// anything.
	FindingsAdvisory bool
	// HumanMergeRequested is an explicit, authorized human instruction. It is
	// evaluated inside PolicyMergeGate, never by a caller-side bypass.
	HumanMergeRequested bool
	// ActingOrgRole is the durable role attribution from the branch lock. The
	// explicit /gitmoot merge path uses it to enforce that role's merge_rule.
	ActingOrgRole string
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
	// ReviewLevel is the exact-head review level that permitted a low-risk
	// auto-merge (per-repo auto_merge = "low_risk"); empty otherwise.
	ReviewLevel string
	// MergedHeadSHA is the exact head a low-risk auto-merge merged.
	MergedHeadSHA string
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
	// HeldByJob names the live job that owns the pull-request branch when THAT is
	// why the decision deferred. It is a JOB ID rather than a flag so the record
	// can say which writer the merge is waiting on.
	//
	// IT IS TYPED BECAUSE THE ALTERNATIVE WAS MATCHING THE REASON PROSE (#1555).
	// The deferral reason already reads "active <type> job <id> in flight on
	// branch <name>", and a consumer deciding a task's state by substring-matching
	// that sentence is precisely the proxy-instead-of-property defect this
	// campaign exists to remove.
	//
	// Only the branch-ownership deferral sets it. Every other transient hold
	// (head not yet observable, freshness unknown) leaves it empty and keeps the
	// pre-existing parking behaviour.
	HeldByJob string
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
