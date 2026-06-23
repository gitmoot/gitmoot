package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

type Engine struct {
	Store                   *db.Store
	RequiredReviewers       []string
	MergeGate               MergeGate
	JobID                   func(JobRequest) string
	PayloadRefresher        func(context.Context, db.Job, JobPayload) (JobPayload, error)
	ImplementationFinalizer ImplementationFinalizer
	// Home is the resolved GITMOOT_HOME root used to place per-delegation
	// worktrees. DelegationWorktrees is the checkout-bound git client that
	// performs the worktree-add. Both are optional: when either is unset, the
	// dispatcher enqueues implement delegations against the shared checkout
	// (legacy behavior) rather than allocating isolated worktrees.
	Home                string
	DelegationWorktrees WorktreeManager
	DelegationCheckout  string
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
}

// now returns the engine's current time, defaulting to time.Now when Now is unset.
func (e Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
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
	// SkipReviewFanout, when true, suppresses the native review-fanout loop in
	// HandlePullRequestOpened: zero review jobs are enqueued and the PR runs the
	// same no-reviewers tail (record baseline + merge gate) the zero-reviewers
	// case already runs, so the PR still advances. Defaults false => full fanout.
	SkipReviewFanout bool
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
}

type MergeGate interface {
	Evaluate(ctx context.Context, request MergeRequest) (MergeDecision, error)
}

type ImplementationFinalizer interface {
	FinalizeImplementation(ctx context.Context, job db.Job, payload JobPayload) (JobPayload, error)
}

type BlockedError struct {
	Reason string
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
	// When the caller asked to skip the native review fanout, run the exact same
	// no-reviewers tail the zero-reviewers case runs (merge gate + baseline) so the
	// PR still advances, but enqueue ZERO review jobs.
	if len(reviewers) == 0 || event.SkipReviewFanout {
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
	result, err := (Mailbox{Store: e.Store}).Run(ctx, jobID, agent, adapter)
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
	mailbox := Mailbox{Store: e.Store}
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
		reviewer := reviewDecisionAgent(job, payload)
		switch payload.Result.Decision {
		case "changes_requested":
			if err := e.setTaskState(ctx, ref, TaskChangesRequested); err != nil {
				return err
			}
			return e.dispatchFix(ctx, reviewer, payload, *payload.Result, ref)
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

// rootWallClockExceeded reports whether the coordination tree rooted at rootID has
// been running longer than MaxDelegationWallClock, measured from the root job's
// created_at, and the elapsed duration. It fails open: any lookup/parse problem
// returns (false, 0) so a clock or timestamp hiccup never blocks dispatch.
func (e Engine) rootWallClockExceeded(ctx context.Context, rootID string) (bool, time.Duration) {
	createdAt, err := e.Store.JobCreatedAt(ctx, rootID)
	if err != nil {
		return false, 0
	}
	start, err := parseJobTimestamp(createdAt)
	if err != nil {
		return false, 0
	}
	elapsed := e.now().UTC().Sub(start)
	return elapsed > MaxDelegationWallClock, elapsed
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
		RootJobID:       e.rootJobID(job, payload),
		Deps:            compactStrings(d.Deps),
		JobTimeout:      strings.TrimSpace(d.Timeout),
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

// countRootDelegationJobs counts every job belonging to a coordination tree: the
// originating coordinator itself (job.ID == rootID) plus every child or
// continuation whose payload RootJobID points back at it. There is no store query
// keyed on root, so it lists all jobs and filters (mirroring childDelegationJobs).
func (e Engine) countRootDelegationJobs(ctx context.Context, rootID string) (int, error) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, job := range jobs {
		if job.ID == rootID {
			count++
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return 0, err
		}
		if payload.RootJobID == rootID {
			count++
		}
	}
	return count, nil
}

// sumRootDelegationTokens sums the runtime token usage (input + output) across an
// entire coordination tree: the originating coordinator itself (job.ID == rootID)
// plus every child or continuation whose payload RootJobID points back at it. It
// mirrors countRootDelegationJobs (there is no store query keyed on root, so it
// lists all jobs and filters). Used by the per-root token budget (#338 Part B) to
// decide, before dispatching a new generation, whether the tree has already spent
// its budget. Token capture is best-effort per runtime (see internal/runtime); a
// job whose runtime did not report usage contributes 0, so the sum under-counts
// rather than over-counts.
func (e Engine) sumRootDelegationTokens(ctx context.Context, rootID string) (int, error) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, job := range jobs {
		if job.ID == rootID {
			total += job.InputTokens + job.OutputTokens
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return 0, err
		}
		if payload.RootJobID == rootID {
			total += job.InputTokens + job.OutputTokens
		}
	}
	return total, nil
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

	if payload.DelegationDepth >= MaxDelegationDepth {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_depth_exceeded",
			Message: fmt.Sprintf("delegation depth %d reached the limit of %d; not dispatching %d delegation(s)", payload.DelegationDepth, MaxDelegationDepth, len(payload.Result.Delegations)),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("delegation depth limit of %d reached", MaxDelegationDepth))
	}

	total, err := e.countRootDelegationJobs(ctx, rootID)
	if err != nil {
		return err
	}
	if total >= MaxDelegationTotalJobs {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_budget_exceeded",
			Message: fmt.Sprintf("delegation tree for root %s reached the job budget of %d (%d jobs); not dispatching %d delegation(s)", rootID, MaxDelegationTotalJobs, total, len(payload.Result.Delegations)),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("per-root job budget of %d reached", MaxDelegationTotalJobs))
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
			return e.block(ctx, ref, fmt.Sprintf("delegation %q preflight failed: %v", request.DelegationID, err))
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
		if err := e.enqueueDelegation(ctx, job, payload, d, artifactDir, ref); err != nil {
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
		ID:              delegationContinuationID(job.ID),
		Agent:           job.Agent,
		Action:          "ask",
		Model:           payload.Model,
		Phase:           payload.Phase,
		Repo:            payload.Repo,
		Branch:          payload.Branch,
		PullRequest:     payload.PullRequest,
		HeadSHA:         payload.HeadSHA,
		GoalID:          payload.GoalID,
		TaskID:          payload.TaskID,
		TaskTitle:       payload.TaskTitle,
		LeadAgent:       payload.LeadAgent,
		Reviewers:       payload.Reviewers,
		Sender:          job.Agent,
		Instructions:    buildCorrectiveContinuationPrompt(payload.Result),
		Constraints:     payload.Constraints,
		ParentJobID:     job.ID,
		DelegationDepth: payload.DelegationDepth + 1,
		DelegatedBy:     job.Agent,
		RootJobID:       e.rootJobID(job, payload),
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
		ID:                 delegationContinuationID(job.ID),
		Agent:              job.Agent,
		Action:             "ask",
		Model:              payload.Model,
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
		Instructions:       buildFinalizeContinuationPrompt(payload.Result, reason),
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
func (e Engine) enqueueDelegation(ctx context.Context, job db.Job, payload JobPayload, d Delegation, artifactDir string, ref taskRef) error {
	request := e.delegationRequest(job, payload, d)
	request.DelegationArtifactDir = artifactDir

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

// integrationDepBranches returns the per-delegation branches of delegation d's
// succeeded implement-leg dependencies, so a dependent read-only step (e.g. a
// decompose-and-verify verify gate) can run against a worktree with those legs
// merged in rather than the base checkout (issue #332). It returns nil when d has
// no such deps, in which case the normal read-only paths apply. Read-only deps
// contribute no branch (they produce no implementation), and a leg that ran in
// the shared checkout (branch == parent base) is skipped (its work is already on
// the base).
func (e Engine) integrationDepBranches(ctx context.Context, job db.Job, payload JobPayload, d Delegation) ([]string, error) {
	deps := compactStrings(d.Deps)
	if len(deps) == 0 || payload.Result == nil {
		return nil, nil
	}
	byID := make(map[string]Delegation, len(payload.Result.Delegations))
	for _, sib := range payload.Result.Delegations {
		byID[strings.TrimSpace(sib.ID)] = sib
	}
	// Resolve each dep to its winning child job the same way advanceDelegations
	// does (latest attempt per delegation id), so a leg that succeeded on a retry
	// contributes its retry branch rather than the failed original.
	children, err := e.childDelegationJobs(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSpace(payload.Branch)
	var branches []string
	for _, dep := range deps {
		sib, ok := byID[dep]
		if !ok || readOnlyDelegationAction(sib.Action) {
			continue
		}
		legJob, ok := children[dep]
		if !ok || legJob.State != string(JobSucceeded) {
			continue
		}
		legPayload, err := unmarshalPayload(legJob.Payload)
		if err != nil {
			return nil, err
		}
		if legBranch := strings.TrimSpace(legPayload.Branch); legBranch != "" && legBranch != base {
			branches = append(branches, legBranch)
		}
	}
	return branches, nil
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
	} else if legBranches, err := e.integrationDepBranches(ctx, job, payload, d); err != nil {
		return err
	} else if len(legBranches) > 0 {
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
			}, legBranches, manager)
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
				Message: fmt.Sprintf("delegation %q runs in an integration worktree merging %d implement leg(s)", request.DelegationID, len(legBranches)),
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
// the delegation graph. It returns whether a retry was enqueued.
func (e Engine) requeueDelegation(ctx context.Context, parentJob db.Job, parentPayload JobPayload, d Delegation, failedChild db.Job, artifactDir string, ref taskRef) (bool, error) {
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

	// Retry pass: a delegation that failed but has retry budget left is
	// re-enqueued as a fresh child before any failure_policy is applied, so its
	// failure is absorbed by the retry rather than blocking/escalating. A
	// successful retry replaces the failed attempt in the children map (the retry
	// id sorts after the original), so the delegation looks active again this
	// pass and the failure switch below skips it.
	retried := false
	for _, d := range parentResult.Delegations {
		child, ok := children[d.ID]
		if !ok || !isTerminalJobState(child.State) || child.State == string(JobSucceeded) {
			continue
		}
		didRetry, err := e.requeueDelegation(ctx, parentJob, parentPayload, d, child, artifactDir, ref)
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
		if !ok || !isTerminalJobState(child.State) || child.State == string(JobSucceeded) {
			continue
		}
		switch delegationFailurePolicy(d) {
		case "continue":
			// Independent ready siblings still run; only this branch's
			// dependents are skipped (handled by depsSatisfied below).
			continue
		case "escalate":
			escalate = true
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
			if err := e.enqueueDelegation(ctx, parentJob, parentPayload, d, artifactDir, ref); err != nil {
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

// maybeEnqueueContinuation enqueues exactly one coordinator continuation job for
// a parent whose delegations have all finished. Idempotency is enforced by a
// deterministic continuation id plus a one-shot delegation_continuation_enqueued
// event on the parent, so concurrent child completions enqueue it at most once.
// When any delegation declares synthesis_rule "vote", the continuation is gated:
// the parent task is blocked unless every child was approved/succeeded.
func (e Engine) maybeEnqueueContinuation(ctx context.Context, parentJob db.Job, parentPayload JobPayload, parentResult *AgentResult, children map[string]db.Job, ref taskRef) error {
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

	request := JobRequest{
		ID:           delegationContinuationID(parentJob.ID),
		Agent:        parentJob.Agent,
		Action:       "ask",
		Model:        parentPayload.Model,
		Phase:        parentPayload.Phase,
		Repo:         parentPayload.Repo,
		Branch:       parentPayload.Branch,
		PullRequest:  parentPayload.PullRequest,
		HeadSHA:      parentPayload.HeadSHA,
		GoalID:       parentPayload.GoalID,
		TaskID:       parentPayload.TaskID,
		TaskTitle:    parentPayload.TaskTitle,
		LeadAgent:    parentPayload.LeadAgent,
		Reviewers:    parentPayload.Reviewers,
		Sender:       parentJob.Agent,
		Instructions: e.buildContinuationPrompt(parentResult, children, childPayloads),
		Constraints:  parentPayload.Constraints,
		ParentJobID:  parentJob.ID,
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
		return isTerminalJobState(child.State)
	}
	// A fingerprint-deduped delegation never gets its own child; it is resolved
	// when its winning sibling (the same-fingerprint delegation that did get a
	// child) reaches a terminal state, so it cannot stall the continuation.
	if winner, ok := dedupWinners[d.ID]; ok {
		return isTerminalJobState(winner.State)
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
			if isTerminalJobState(winner.State) && winner.State != string(JobSucceeded) {
				return true
			}
			continue
		}
		if child, ok := children[dep]; ok {
			if isTerminalJobState(child.State) && child.State != string(JobSucceeded) {
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

func isTerminalJobState(state string) bool {
	switch state {
	case string(JobSucceeded), string(JobFailed), string(JobBlocked), string(JobCancelled):
		return true
	default:
		return false
	}
}

func delegationFailurePolicy(d Delegation) string {
	policy := strings.ToLower(strings.TrimSpace(d.FailurePolicy))
	if policy == "" {
		return "block_parent"
	}
	return policy
}

// delegationFailureHandledByPolicy reports whether the named delegation declares
// a continue/escalate failure_policy, meaning a failure of its child is governed
// by the delegation graph rather than blocking the shared parent task.
func delegationFailureHandledByPolicy(parentResult *AgentResult, delegationID string) bool {
	if parentResult == nil || strings.TrimSpace(delegationID) == "" {
		return false
	}
	for _, d := range parentResult.Delegations {
		if d.ID != delegationID {
			continue
		}
		switch delegationFailurePolicy(d) {
		case "continue", "escalate":
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
	return !isTerminalJobState(child.State), nil
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
// coordinator can synthesize the results without re-reading every child job.
func (e Engine) buildContinuationPrompt(parentResult *AgentResult, children map[string]db.Job, childPayloads map[string]JobPayload) string {
	var builder strings.Builder
	builder.WriteString("All delegated jobs have finished. Review the results below and decide the next step.\n\n")
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
	// an empty delegations list as terminal, so spell it out for the agent.
	builder.WriteString("\n\nIf the goal is now complete, return your result with an EMPTY delegations list to finish. Only return new delegations if more work is genuinely required.")
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
func buildCorrectiveContinuationPrompt(parentResult *AgentResult) string {
	var builder strings.Builder
	builder.WriteString("You delegated the same set as a previous round; it did not change the outcome. Change your approach or return an EMPTY delegations list to finish.\n\n")
	if parentResult != nil && len(parentResult.Delegations) > 0 {
		builder.WriteString("Repeated delegation set:\n")
		for _, d := range parentResult.Delegations {
			fmt.Fprintf(&builder, "- delegation %q (agent %s, action %s)\n", d.ID, d.Agent, d.Action)
		}
	}
	return builder.String()
}

// buildFinalizeContinuationPrompt is the terminal continuation sent when a
// termination backstop trips. It tells the coordinator it has hit a limit and
// cannot delegate further, asks it to synthesize a best-effort final result from
// the completed work, and states that any delegations it returns now are ignored
// (the engine enforces this via DelegationFinalize). It lists the delegations that
// were not dispatched for context.
func buildFinalizeContinuationPrompt(parentResult *AgentResult, reason string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "A termination backstop was reached (%s). You cannot delegate any more work.\n\n", reason)
	builder.WriteString("Synthesize a best-effort FINAL result from what has already completed, and return an EMPTY delegations list. Any delegations you return now will be ignored.\n")
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

func (e Engine) preflightDelegation(ctx context.Context, request JobRequest) error {
	// An ephemeral delegation routes to an on-demand worker that no agent row
	// backs, so the registered-agent existence, repo-access, and capability checks
	// do not apply: the ephemeral child inherits the coordinator's allowed repo
	// scope. Only validate that the spec runtime is a real agent runtime (never
	// shell); the daemon materializes the worker from the spec.
	if request.Ephemeral != nil {
		return validateEphemeralSpec(request.DelegationID, request.Ephemeral)
	}
	agent, err := e.Store.GetAgent(ctx, request.Agent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("agent %q is not subscribed", request.Agent)
		}
		return err
	}
	allowed, err := e.Store.AgentCanAccessRepo(ctx, agent.Name, request.Repo)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("agent %q is not allowed on %q", agent.Name, request.Repo)
	}
	if !contains(agent.Capabilities, request.Action) {
		return fmt.Errorf("agent %q lacks %q capability", agent.Name, request.Action)
	}
	return nil
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
	_, err := (Mailbox{Store: e.Store}).Enqueue(ctx, request)
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
		if err := validateEphemeralSpec(request.DelegationID, request.Ephemeral); err != nil {
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
		return decision, e.block(ctx, ref, reason)
	}
	if decision.Merged {
		return decision, e.setTaskState(ctx, ref, TaskMerged)
	}
	return decision, e.setTaskState(ctx, ref, TaskReadyToMerge)
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
