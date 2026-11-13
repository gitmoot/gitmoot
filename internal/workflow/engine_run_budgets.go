package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

func (e Engine) RunJob(ctx context.Context, jobID string, agent runtime.Agent, adapter DeliveryAdapter) (AgentResult, error) {
	return e.runJob(ctx, jobID, agent, adapter, false)
}

// RunClaimedJob continues a job claimed by the remote-review admission gate
// immediately before execution-backend reservation.
func (e Engine) RunClaimedJob(ctx context.Context, jobID string, agent runtime.Agent, adapter DeliveryAdapter) (AgentResult, error) {
	return e.runJob(ctx, jobID, agent, adapter, true)
}

// RecordUnrecordedResult repeats the terminal write a ResultNotRecordedError
// describes, on the caller's ctx (no run deadline), and only while the row is
// still the run this process claimed at that generation. Like the original
// write it records advance_started, so the daemon's advance-retry pass drives
// the advancement RunJob did not reach.
func (e Engine) RecordUnrecordedResult(ctx context.Context, unrecorded *ResultNotRecordedError) error {
	if err := e.validate(); err != nil {
		return err
	}
	return e.mailbox().writeTerminalPayload(ctx, unrecorded.JobID, unrecorded.State, unrecorded.Message, unrecorded.Payload,
		&claimAnchor{generation: unrecorded.Generation, runner: db.CurrentJobRunner()})
}

func (e Engine) runJob(ctx context.Context, jobID string, agent runtime.Agent, adapter DeliveryAdapter, claimed bool) (AgentResult, error) {
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
	var result AgentResult
	if claimed {
		result, err = e.mailbox().RunClaimed(ctx, jobID, agent, adapter)
	} else {
		result, err = e.mailbox().Run(ctx, jobID, agent, adapter)
	}
	if err != nil {
		return result, err
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

// FinalizeTimedOutJob turns any job killed by its run deadline into a terminal
// FAILED job carrying a synthetic result and a job_timeout witness.
//
// timeoutDetail is persisted verbatim and is expected to carry the effective
// deadline, elapsed duration, and bounded stderr tail. The method is idempotent:
// a result-bearing or non-failed terminal job is never rewritten.
func (e Engine) FinalizeTimedOutJob(ctx context.Context, jobID string, reason string, timeoutDetail string) (bool, error) {
	if err := e.validate(); err != nil {
		return false, err
	}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return false, err
	}
	// Already has a result -> the normal RunJob/AdvanceJob path handled it; nothing
	// to recover. A deadline can leave the job either still JobRunning (the
	// cancelled context aborted Mailbox.Run's own fail write) or already
	// JobFailed/JobBlocked but WITHOUT a stored result. Recover both, but never
	// touch a succeeded or cancelled job.
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
		reason = "job timed out before returning a result"
	}
	payload.Result = &AgentResult{
		Decision: "failed",
		Summary:  reason,
	}
	// #1351/#1417/#1557. Synthesizing the result and leaving FailureDiagnostics
	// nil is what made a finalized leg unreadable: phase, exit_code and signal
	// all absent, so the row could not say WHY it ended and every cause looked
	// alike. Only the delivery path recorded diagnostics, so a leg the ENGINE
	// terminalized after the fact carried none at all.
	//
	// An EXISTING block is never overwritten: a real crash report (phase,
	// exit code, stderr tail) is strictly better evidence than this marker, and
	// replacing it would destroy the cause in favour of the observation.
	if payload.FailureDiagnostics == nil {
		payload.FailureDiagnostics = WithDeliveryError(&FailureDiagnostics{Phase: FailurePhaseFinalized}, timeoutDetail)
		if payload.FailureDiagnostics != nil && strings.TrimSpace(payload.FailureDiagnostics.DeliveryError) == "" {
			// A detail that redacts away to nothing still leaves the phase, which
			// is the fact the reader most needs: the engine ended this job.
			payload.FailureDiagnostics.Phase = FailurePhaseFinalized
		}
	}
	mailbox := e.mailbox()
	if job.State == string(JobRunning) {
		if err := mailbox.finishWithPayload(ctx, jobID, JobFailed, reason, payload); err != nil {
			return false, err
		}
	} else if err := mailbox.savePayload(ctx, jobID, payload); err != nil {
		// Already terminal-failed/blocked: only attach the synthetic result.
		return false, err
	}
	if err := e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    "job_timeout",
		Message: strings.TrimSpace(timeoutDetail),
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (e Engine) recordImplementNoPRAdvance(ctx context.Context, jobID, decision string) error {
	return e.Store.AddJobEventIfAbsent(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    "advance_skipped_no_pr",
		Message: fmt.Sprintf("implement decision %q produced no pull request; skipping PR advancement", strings.TrimSpace(decision)),
	})
}

// recordFoldedReviewOutcome persists the durable approved-with-notes outcome for a
// sub-threshold changes-requested review. The merge gate, required-reviewer
// evaluation and the review wake all fold the same verdict to approved
// regardless — so a fold that is not recorded leaves the job's durable
// event history contradicting those surfaces.
//
// It is called once at the TOP of AdvanceJob rather than at any point inside it.
// Historical fan-out announcements are excluded for free: unmarshalPayload
// replaces their decision with HistoricalFanOutDecision, so they never reach "approved" here. blocked/failed are
// excluded by the changes_requested test, and a part verdict by
// recordsApprovedWithNotes (#2373). AddJobEventIfAbsent makes every replay,
// re-advance and stale round idempotent.
func (e Engine) recordFoldedReviewOutcome(ctx context.Context, job db.Job, payload JobPayload) error {
	if job.Type != "review" || payload.Result == nil ||
		payload.Result.Decision != "changes_requested" {
		return nil
	}
	blockingSeverity := e.reviewBlockingSeverity(payload.Repo)
	if !recordsApprovedWithNotes(payload, blockingSeverity) {
		return nil
	}
	return e.Store.AddJobEventIfAbsent(ctx, db.JobEvent{
		JobID: job.ID,
		Kind:  ReviewApprovedWithNotesEventKind,
		Message: fmt.Sprintf("review severity %s is below repository blocking severity %s; findings remain recorded and no fix is dispatched",
			payload.Result.Severity, blockingSeverity),
	})
}

func (e Engine) AdvanceJob(ctx context.Context, jobID string) (retErr error) {
	if err := e.validate(); err != nil {
		return err
	}
	ctx, releaseLifecycle, err := e.Store.AcquireJobLifecycle(ctx)
	if err != nil {
		return err
	}
	releasedLifecycle := false
	releaseForCleanup := func() {
		if !releasedLifecycle {
			releaseLifecycle()
			releasedLifecycle = true
		}
	}
	defer releaseForCleanup()
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
	defer func() {
		releaseForCleanup()
		retErr = errors.Join(retErr, e.cleanupReadOnlyDelegationWorktree(ctx, jobID, job.Type, payload))
	}()

	// An implement delegation child runs in a per-delegation worktree on its own
	// gitmoot-delegation-* branch; tear both down once the child is terminal so they
	// do not accumulate in the shared checkout and mislead a later coordinator (#478).
	// No-op for non-implement / non-delegation jobs and (idempotently) for already
	// cleaned ones. Skips succeeded legs still feeding a pending integration (#332).
	defer func() {
		releaseForCleanup()
		retErr = errors.Join(retErr, e.cleanupImplementDelegationWorktree(ctx, jobID, job.Type, payload))
	}()

	// Recorded BEFORE any early return in this function, and deliberately not
	// positioned relative to them. Five consecutive review rounds each found a
	// DIFFERENT early return that skipped a positionally-placed write — the
	// approval replay guard, the PR-less arm, the top-level ask-gate, and the
	// delegated-child ask-gate. Position is not a property that survives edits;
	// being first is. Every consumer folds this verdict the same way whether or not
	// advancement proceeds, so the record is owed unconditionally.
	if err := e.recordFoldedReviewOutcome(ctx, job, payload); err != nil {
		return err
	}

	// A non-implemented terminal decision leaves a missing PR final.
	if job.Type == "implement" && payload.Result.Decision != "implemented" && payload.PullRequest <= 0 {
		if err := e.recordImplementNoPRAdvance(ctx, job.ID, payload.Result.Decision); err != nil {
			return err
		}
	}

	effectiveDecision := payload.Result.Decision
	if job.Type == "review" {
		latest, err := e.latestReviewRound(ctx, payload)
		if err != nil {
			return err
		}
		if latest != "" && strings.TrimSpace(payload.ReviewRound) != latest {
			// #1850 round 2 F7. A reviewer whose round was superseded WHILE STILL
			// RUNNING found a real defect at a real head, and a later round label
			// does not make that defect untrue. Dropping it here was silent: no
			// ledger row, no recorded event, and the stale verdict is discarded
			// too, so the finding was unrecoverable - which contradicts this
			// writer's own doctrine that a silent skip is indistinguishable from a
			// successful write, and loses exactly the finding a later round most
			// needs reminding of.
			//
			// The observations are keyed by the STALE round's own head, so they
			// can never discharge anything at the current head, and this runs
			// AFTER effectiveDecision is computed and BEFORE any state change, so
			// a stale round still cannot influence the decision.
			if err := e.RecordReviewFindingsToLedger(ctx, job, payload); err != nil {
				return err
			}
			return nil
		}
		// Folding through the payload authority keeps fan-out exclusion and the
		// severity bar in one place.
		effectiveDecision = effectiveReviewDecision(payload.Result, e.reviewBlockingSeverity(payload.Repo))
		// THE #1822 LEDGER'S PRODUCTION WRITER (#1850 review F1). This is the only
		// place a review's findings become ledger observations, and without it the
		// entire feature was inert at runtime: every guard passed against a table
		// nothing ever wrote. It runs for the CURRENT round only (the staleness
		// check above already returned for a superseded round) and it never fails
		// the review, because a real verdict must not be discarded over a ledger
		// row. Placed BEFORE the blocked/failed early return would record findings
		// from a review that never ran; placed here it records exactly the reviews
		// whose verdicts the gate will later read.
		if err := e.RecordReviewFindingsToLedger(ctx, job, payload); err != nil {
			return err
		}
		// A POST-MERGE REVIEW (#2265 level 2) reviews a head that already shipped.
		// Its findings are recorded above like any review's, but nothing here may
		// set changes_requested, dispatch a fix, or run the merge gate: the
		// follow-ups go to the requesting role instead.
		if payload.PostMergeReview {
			return e.handlePostMergeReview(ctx, job, payload)
		}
	}
	// AN ASK THAT REVIEWED SOMETHING LOSES ITS FINDINGS SILENTLY (#1962). Both
	// RecordReviewFindingsToLedger call sites above are inside `job.Type ==
	// "review"`, so for an ask the writer is never CALLED - its own internal
	// type guard is defence in depth and its recordLedgerSkip can never fire.
	// The verdict is real and the loss leaves no trace anywhere.
	//
	// Measured on 2026-09-08: 15 succeeded gm-review-opus ask jobs, all 15
	// carrying a decision and 11 carrying findings, produced ZERO ledger rows and
	// ZERO events explaining it. Positive control on that zero: the same join
	// over review-TYPE jobs the same day returns 28 jobs and 80 rows. Of the 15
	// decisions, 6 were changes_requested, and one of those - PR #2029 at
	// 8bdf8f54 - is an objection at that PR's CURRENT head, invisible to every
	// PR-keyed surface.
	//
	// THIS RECORDS THE LOSS; IT DOES NOT CLOSE IT, and writing a row here would
	// be worse than the gap. An observation with no pull request and no head
	// cannot be keyed to anything a reader can query, and inventing a key is the
	// #2054 class. So the honest outcome for an unbound verdict is a RECORDED
	// skip rather than a silent one or a fabricated row.
	//
	// Making a BOUND ask write real observations is deliberately not attempted
	// here: that path inherits #2059's field-name mismatch, where the wire reads
	// id/title/state while reviewers emit uid/evidence/disposition, and it would
	// convert these silent losses into empty OPEN findings. #2059 first.
	if job.Type != "review" && reviewShapedResult(payload.Result) {
		e.recordUnboundReviewVerdict(ctx, job, payload)
	}
	if payload.Result.Decision == "blocked" || payload.Result.Decision == "failed" {
		return e.block(ctx, ref, payload.Result.Summary)
	}
	if job.Type == "review" && effectiveDecision == "approved" {
		done, err := e.reviewApprovalAlreadyAdvanced(ctx, ref)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}

	switch job.Type {
	case "implement":
		if payload.Result.Decision != "implemented" {
			return nil
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
		// Trigger 1 (in-process path): the implement job's own payload carries
		// skip_native_review_fanout and the acting org role onto the PR event.
		// Empty role stays empty (unattributed).
		event := PullRequestEvent{
			Repo:                    payload.Repo,
			Branch:                  payload.Branch,
			PullRequest:             payload.PullRequest,
			PullRequestDraft:        payload.PullRequestDraft,
			PullRequestDraftUnknown: payload.PullRequestDraftUnknown,
			HeadSHA:                 payload.HeadSHA,
			GoalID:                  payload.GoalID,
			TaskID:                  payload.TaskID,
			TaskTitle:               payload.TaskTitle,
			LeadAgent:               leadAgent,
			Sender:                  job.Agent,
			RequiredReviewers:       e.requiredReviewers(payload),
			SkipReviewFanout:        payload.SkipNativeReviewFanout,
			ActingOrgRole:           NormalizeActingOrgRole(payload.ActingOrgRole),
			// #1967: the implement job that just opened the PR is the dispatcher.
			// job.Agent is already used as Sender here, but Sender is a channel on
			// every other path ("github", "local"), so attribution
			// cannot be read off it - it needs its own field to mean the same thing
			// everywhere.
			DispatchedBy: job.Agent,
		}
		return e.HandlePullRequestOpened(ctx, event)
	case "review":
		// A PR-less review (Action="review" with
		// PullRequest=0/Branch="") has no PR-only machinery to route into: an
		// "approved" decision would runMergeGate against PR #0, erroring every
		// tick and dropping the review outcome. Mirror the implement arm's guard:
		// treat the delivered review (the agent's comments) as terminal, like ask.
		if payload.PullRequest <= 0 {
			return e.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_skipped_no_pr", Message: "no pull request is attached; skipping review advancement"})
		}
		reviewer := reviewDecisionAgent(job, payload)
		switch effectiveDecision {
		case "changes_requested":
			// #1524: an objection transitions the task ONLY when it describes the
			// pull request's CURRENT head. This arm was left unconditional, so an
			// objection at a superseded head still pulled a PR out of
			// ready_to_merge over a commit the branch had moved past. Returning
			// early refuses that.
			//
			// THE GUARD IS DELIBERATELY ASYMMETRIC WITH THE APPROVAL SIDE, and the
			// asymmetry is a LIVENESS argument: refusing an objection withholds the
			// conservative transition and the inline fix pass from a complaint that
			// may well be about the current head. So when no observed pull request
			// row records a head, this arm ADMITS - refusing would block a
			// legitimate objection on a PR the daemon has not polled yet, which is
			// the CLI-dispatch path, and would make the engine's cheapest
			// transition the one demanding the most evidence.
			bound, unboundReason, err := e.objectionBindsToCurrentHead(ctx, payload)
			if err != nil {
				return err
			}
			if !bound {
				return e.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_skipped_stale_head", Message: unboundReason})
			}
			if err := e.setTaskState(ctx, ref, TaskChangesRequested); err != nil {
				return err
			}
			// #2203: a changes_requested verdict records the objection and stops
			// here. No fix leg is dispatched: gitmoot does not dispatch
			// implementation. The findings stay open in the #1822 ledger and the
			// implementing lane picks them up.
			return nil
		case "approved":
			ready, err := e.allRequiredReviewersApproved(ctx, reviewer, payload)
			if err != nil {
				return err
			}
			if !ready {
				return e.setReviewingIfNotChangesRequested(ctx, ref)
			}
			// #1834: the expectation handed to the gate must be the task's ACTUAL
			// state, not a constant. A task that reached changes_requested can only
			// be claimed as changes_requested, and only when this approval is bound
			// to the current head and unopposed there; otherwise the objection
			// stands and the approval is recorded rather than silently dropped.
			expectedState, proceed, held, retryable, err := e.mergeGateExpectedTaskState(ctx, ref, payload)
			if err != nil {
				return err
			}
			if !proceed {
				if retryable {
					// A TRANSIENT hold - the current head is not observable YET. Returning
					// an error leaves the advancement unreconciled, so the daemon's
					// advance-retry re-drives this approval once the daemon poll inserts
					// the pull_requests row. Recording an event and returning nil would
					// SETTLE it, and nothing would ever re-drive it: the approval reads as
					// advanced while the task stays wedged (#1871 review round 3, P1,
					// measured with a two-poll probe that ended gate_requests=0).
					//
					// No job event is written here on purpose. This path is re-entered on
					// every tick until the row appears, and recordAdvanceRetryOnce already
					// dedups the advance_retry marker that carries this message - appending
					// a row per tick is what grew job_events to ~1.8M rows once before.
					return fmt.Errorf("review approval held: %s", held)
				}
				// TERMINAL: a live objection at the current head, or an approval with
				// no head at all. Nothing will change without a new review, so settle
				// it with a durable reason instead of retrying forever.
				//
				// A HEAD MISMATCH IS NO LONGER TERMINAL (#2150). It used to be settled
				// here, which wedged approvals whose head was actually CURRENT while
				// the observed row still held the previous one - the normal ordering
				// when a review finishes inside one poll interval. It is retryable
				// above now, and a genuinely superseded approval still never admits,
				// because the head comparison runs again on each re-evaluation.
				return e.Store.AddJobEvent(ctx, db.JobEvent{
					JobID:   job.ID,
					Kind:    "review_advance_held",
					Message: held,
				})
			}
			_, err = e.runMergeGate(ctx, reviewer, payload, ref, expectedState)
			return err
		}
	}
	return nil
}
