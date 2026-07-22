package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

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

func (e Engine) recordImplementNoPRAdvance(ctx context.Context, jobID, decision string) error {
	return e.Store.AddJobEventIfAbsent(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    "advance_skipped_no_pr",
		Message: fmt.Sprintf("implement decision %q produced no pull request; skipping PR advancement", strings.TrimSpace(decision)),
	})
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

	// High-risk lens normalization (#650). A refutation lens may report a CRITICAL
	// finding in AgentResult.Findings yet leave its OWN decision at
	// approved/changes_requested — the documented convention (SynthesizeLensDecision)
	// is that a critical refutation blocks the merge, but a reviewer that does not
	// self-normalize would otherwise let the change through. Convert a critical
	// refutation into a non-approving `blocked` decision (and terminal state) BEFORE
	// any advance so BOTH the cross-lens quorum synthesis AND the native
	// required-reviewer merge gate observe the block. Gated on RiskTier == high, so
	// every routine/non-lens job (empty RiskTier) is byte-identical; a no-op when the
	// lens reported no critical finding or already decided `blocked`.
	if job.Type == "review" && payload.RiskTier == RiskTierHigh && payload.Result != nil &&
		payload.Result.Decision != "blocked" &&
		SynthesizeLensDecision(ParseLensFindings(payload.Result.Findings)) == "blocked" {
		payload.Result.Decision = "blocked"
		encoded, err := marshalPayload(payload)
		if err != nil {
			return err
		}
		if err := e.Store.UpdateJobPayload(ctx, jobID, encoded); err != nil {
			return err
		}
		if err := e.Store.UpdateJobState(ctx, jobID, string(JobBlocked)); err != nil {
			return err
		}
		job.State = string(JobBlocked)
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   jobID,
			Kind:    "lens_critical_refutation",
			Message: "a critical refutation finding normalized the lens decision to blocked",
		})
	}

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
	// Non-implemented terminal decisions never enter the implementation finalizer,
	// so a missing PR is already final. Record it before delegated-child policy
	// handling, whose blocked/failed paths may return early after advancing the parent.
	if job.Type == "implement" && payload.Result.Decision != "implemented" && payload.PullRequest <= 0 {
		if err := e.recordImplementNoPRAdvance(ctx, job.ID, payload.Result.Decision); err != nil {
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

	if job.Type == "review" && payload.Sender == PipelineJobSender {
		// Pipeline PR reviews are report-only: delivery and the daemon's PR-comment
		// posting already happened, while the pipeline advancer owns decision folding.
		// Do not enter the native review lifecycle here: changes_requested would fan
		// out a fix job and approved could run the merge gate (and merge the PR), both
		// violating the human-merge pipeline policy. This is the single policy seam
		// where a future explicit `merge: auto` mode can branch.
		events, err := e.Store.ListJobEvents(ctx, job.ID)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Kind == "pipeline_review_report_only" {
				return nil
			}
		}
		return e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "pipeline_review_report_only",
			Message: "pipeline review recorded as report-only; pipeline advancement owns the verdict and human merge remains required",
		})
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
			return e.recordImplementNoPRAdvance(ctx, job.ID, payload.Result.Decision)
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
			// High-risk review (#650): the native required-reviewer gate approves a PR
			// as soon as every required reviewer has ONE approving review in the round.
			// With the lens fan-out that is too weak — a single approving lens (or one
			// approving lens per reviewer) would drive the merge before the sibling
			// refutation lenses even finish, defeating the adversarial quorum. Gate the
			// native merge on the coordinator's quorum being satisfied first, so an
			// approving lens can only advance toward merge once EVERY lens has approved.
			// A critical/changes_requested lens leaves the quorum unmet (and its own
			// terminal advance blocks the task), so this never merges a refuted change.
			if payload.RiskTier == RiskTierHigh {
				quorumMet, err := e.highRiskLensQuorumMet(ctx, payload)
				if err != nil {
					return err
				}
				if !quorumMet {
					return e.setReviewingIfNotChangesRequested(ctx, ref)
				}
			}
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
		WorkflowID:      payload.WorkflowID,
		Deps:            compactStrings(d.Deps),
		JobTimeout:      effectiveDelegationTimeout(d, e.DelegationTimeoutDefaults),
		Fingerprint:     strings.TrimSpace(d.Fingerprint),
		FailurePolicy:   strings.TrimSpace(d.FailurePolicy),
		SynthesisRule:   strings.TrimSpace(d.SynthesisRule),
		Model:           strings.TrimSpace(d.Model),
		Effort:          strings.TrimSpace(d.Effort),
		Phase:           strings.TrimSpace(d.Phase),
		// Inherit the coordinator's resolved risk tier (#650) so a high-risk lens
		// child carries it for explainable escalation. Empty for every non-risk tree.
		RiskTier: strings.TrimSpace(payload.RiskTier),
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
		// (runtime/model/effort/template/role) into the hash so two distinct ephemeral
		// specs are not mistaken for the same work by loop detection / dedup.
		eph := ""
		if d.Ephemeral != nil {
			eph = strings.Join([]string{d.Ephemeral.Runtime, d.Ephemeral.Model, d.Ephemeral.Effort, d.Ephemeral.Template, d.Ephemeral.Role}, "|")
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
