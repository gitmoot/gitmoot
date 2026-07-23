package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

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
	// Opt-in risk-tiered adaptive review (#650). When RiskTiersEnabled, classify
	// the PR (label > path > default). A `high` tier replaces the single native
	// fan-out with a refutation-lens delegation batch synthesized by the EXISTING
	// quorum synthesis_rule engine. The whole block is gated on the flag, so with
	// risk tiers off (the default) the classifier is never called and the
	// single-review path below is byte-identical.
	if e.RiskTiersEnabled {
		labels, changedPaths := event.Labels, event.ChangedPaths
		// The in-process implement->PR trigger carries no GitHub file data. Resolve
		// the classifier's signals through the best-effort seam when the event has
		// none and the seam is wired.
		if len(labels) == 0 && len(changedPaths) == 0 && e.PullRequestSignals != nil && event.PullRequest > 0 {
			l, p, err := e.PullRequestSignals(ctx, event.Repo, event.PullRequest)
			if err != nil {
				// The signals are UNKNOWN this poll (a transient GitHub error). Do NOT
				// fall through to the routine single-review fan-out: committing this head
				// to a routine review job would let a later poll (seam recovered ->
				// `high`) dispatch the lens quorum onto the SAME review round, so a routine
				// single review and the high-risk lens quorum would coexist and the routine
				// reviewer's approval could drive the merge gate without ever satisfying the
				// adversarial quorum. Defer classification to the next poll instead: the
				// daemon re-fires HandlePullRequestOpened at the same head, and the routine
				// path stays reachable if the seam keeps resolving `routine`. Only record
				// the PR baseline (idempotent) so nothing is dispatched this poll.
				return e.recordPullRequestBaseline(ctx, event)
			}
			labels, changedPaths = l, p
		}
		classification := ClassifyRisk(e.HighRiskPaths, e.RiskLabelHigh, e.RiskLabelRoutine, labels, changedPaths)
		if classification.Tier == RiskTierHigh {
			return e.dispatchHighRiskReview(ctx, event, reviewers, classification, ref)
		}
	}
	reviewRound, err := e.nextReviewRound(ctx, event)
	if err != nil {
		return err
	}
	requests := make([]JobRequest, 0, len(reviewers))
	for _, reviewer := range reviewers {
		request := JobRequest{
			PolicyExempt: "exempt",
			Agent:        reviewer,
			Action:       "review",
			Repo:         event.Repo,
			Branch:       event.Branch,
			PullRequest:  event.PullRequest,
			HeadSHA:      event.HeadSHA,
			GoalID:       event.GoalID,
			TaskID:       event.TaskID,
			TaskTitle:    event.TaskTitle,
			LeadAgent:    event.LeadAgent,
			Reviewers:    reviewers,
			ReviewRound:  reviewRound,
			Sender:       event.Sender,
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

// dispatchHighRiskReview replaces the single native review fan-out with a
// refutation-lens delegation batch for a PR classified `high` (#650). It seeds a
// synthetic, already-completed review COORDINATOR job whose result carries the
// lens delegations (each tagged synthesis_rule "quorum") and then invokes the
// EXISTING delegation dispatcher — so the fan-out, the deps machinery, and the
// cross-lens synthesis are all the same engine the coordinator-returns-delegations
// path uses, never a bespoke synthesis. When every lens approves, the quorum is
// satisfied and the coordinator continuation is enqueued; when ANY lens reports a
// critical refutation (a `blocked` decision, a NON-approving quorum outcome) the
// quorum fails and the shared task is blocked — the explicit "blocks on a critical
// refutation or a failed quorum" acceptance behavior.
//
// It is idempotent against the daemon's re-poll: the coordinator id is derived
// from the stable review round for this head SHA, and the lens children are
// review jobs the daemon's PR-watcher routing already recognizes, so a re-poll at
// the same head never re-dispatches.
func (e Engine) dispatchHighRiskReview(ctx context.Context, event PullRequestEvent, reviewers []string, classification RiskClassification, ref taskRef) error {
	round, err := e.nextReviewRound(ctx, event)
	if err != nil {
		return err
	}
	coordID := "review-coordinator/" + event.Branch + "/" + round
	if _, err := e.Store.GetJob(ctx, coordID); err == nil {
		// Already dispatched for this head SHA/round: idempotent no-op.
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	delegations := highRiskLensDelegations(reviewers, event)
	if len(delegations) < 2 {
		// Defensive: no reviewers to fan out to. Fall back to recording the baseline
		// rather than silently dropping the PR (should not happen — callers guarantee
		// len(reviewers) >= 1, which yields >= 2 lenses).
		return e.recordPullRequestBaseline(ctx, event)
	}

	coordPayload := JobPayload{
		Repo:        event.Repo,
		Branch:      event.Branch,
		PullRequest: event.PullRequest,
		HeadSHA:     event.HeadSHA,
		GoalID:      event.GoalID,
		TaskID:      event.TaskID,
		TaskTitle:   event.TaskTitle,
		LeadAgent:   event.LeadAgent,
		Reviewers:   reviewers,
		ReviewRound: round,
		Sender:      event.Sender,
		Instructions: fmt.Sprintf(
			"Synthesize the high-risk adversarial review of pull request #%d for task %s from the lens findings below.",
			event.PullRequest, taskLabel(event.TaskID, event.TaskTitle),
		),
		RiskTier: classification.Tier,
		Result: &AgentResult{
			Decision:    "approved",
			Summary:     "high-risk adversarial lens fan-out",
			Delegations: delegations,
		},
	}
	encoded, err := marshalPayload(coordPayload)
	if err != nil {
		return err
	}
	coordJob := db.Job{
		ID:      coordID,
		Agent:   event.LeadAgent,
		Type:    "review_coordinator",
		State:   string(JobSucceeded),
		Payload: encoded,
	}
	if err := e.Store.CreateJobWithEvent(ctx, coordJob, db.JobEvent{
		JobID:   coordID,
		Kind:    "risk_tier_resolved",
		Message: fmt.Sprintf("risk tier %q (%s): %s", classification.Tier, classification.Source, classification.Reason),
	}); err != nil {
		return err
	}
	if err := e.dispatchDelegations(ctx, coordJob, coordPayload, ref); err != nil {
		return err
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
	// Best-effort observability: CI readiness must never block or roll back the
	// primary merge-gate path. Repeated polls are deduped durably by the store.
	_, _ = RecordPullRequestWorkflowTransition(ctx, e.Store, event, PullRequestJournalReady)
	ref := taskRefFromPullRequest(event)
	_, err := e.runMergeGateWithHumanMerge(ctx, "", JobPayload{
		Repo:        event.Repo,
		Branch:      event.Branch,
		PullRequest: event.PullRequest,
		HeadSHA:     event.HeadSHA,
		GoalID:      event.GoalID,
		TaskID:      event.TaskID,
		TaskTitle:   event.TaskTitle,
		LeadAgent:   event.LeadAgent,
		Reviewers:   compactStrings(append([]string{}, event.RequiredReviewers...)),
	}, ref, event.HumanMergeRequested)
	return err
}

// HandleReviewPullRequestClosed reconciles a PR lifecycle task whose pull
// request is no longer open on GitHub (#543, #893). The daemon poll loop only
// lists OPEN pull requests, so an externally merged PR otherwise disappears
// while its task and local PR mirror remain stale.
//
// Merged PRs resolve any of
// pr_open/reviewing/changes_requested/ready_to_merge/awaiting_human_merge/blocked; an already-merged
// task is accepted only to repair its stale PR mirror. A clean closed-unmerged
// detection resolves pr_open/reviewing/changes_requested/awaiting_human_merge to
// blocked. The daemon's
// closed-PR reconcile pass is the sole caller for that transition; the external-
// merge pass only records its workflow breadcrumb, avoiding double handling.
// Existing PR row fields (url/base/merge SHA) are preserved.
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
	taskState := TaskState(task.State)
	alreadyMerged := false
	if merged {
		switch taskState {
		case TaskPullRequestOpen, TaskReviewing, TaskChangesRequested, TaskReadyToMerge, TaskAwaitingHumanMerge, TaskBlocked:
		case TaskMerged:
			// Keep the task terminal while repairing a stale local PR mirror after
			// another path (notably the ready-to-merge gate) completed first.
			alreadyMerged = true
		default:
			return nil
		}
	} else {
		switch taskState {
		case TaskPullRequestOpen, TaskReviewing, TaskChangesRequested, TaskAwaitingHumanMerge:
		default:
			return nil
		}
	}
	prState := "closed"
	nextTaskState := TaskBlocked
	if merged {
		prState = "merged"
		nextTaskState = TaskMerged
	}
	if !alreadyMerged {
		stateRef := ref
		if strings.TrimSpace(task.Branch) == "" {
			// Legacy/local review-pr tasks intentionally have no branch because the
			// canonical implement task may already own (repo, head branch). Keeping the
			// ref empty advances the review task itself instead of setTaskState's branch
			// collision fallback advancing the implement task.
			stateRef.Branch = ""
		}
		if merged {
			if err := e.setTaskState(ctx, stateRef, nextTaskState); err != nil {
				return err
			}
		} else {
			changed, _, err := e.Store.TransitionTaskStateWithEvent(ctx, task.ID,
				[]string{string(TaskPullRequestOpen), string(TaskReviewing), string(TaskChangesRequested), string(TaskAwaitingHumanMerge)},
				string(TaskBlocked), "pr_closed_unmerged", "pull request closed without merging")
			if err != nil {
				return err
			}
			if !changed {
				// A concurrent lifecycle move won the CAS. Preserve that newer state and
				// do not let this stale close observation rewrite the PR mirror.
				return nil
			}
		}
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
	if merged && !alreadyMerged {
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
	if !merged && taskState == TaskAwaitingHumanMerge {
		// Preserve the worktree for task recovery, but do not leave a closed,
		// human-parked PR holding the branch lock indefinitely.
		if branch := strings.TrimSpace(task.Branch); branch != "" {
			if lock, err := e.Store.GetBranchLock(ctx, event.Repo, branch); err == nil {
				_, _ = e.Store.ReleaseLockWithEvent(ctx, lock, db.BranchLockEvent{
					Kind: "released", Message: "released after parked pull request closed without merging",
				})
			}
		}
	}
	transition := PullRequestJournalClosed
	if merged {
		transition = PullRequestJournalMerged
	}
	// The task/PR transition above is already durable. Journal failures are
	// intentionally swallowed so observability can never undo lifecycle state.
	_, _ = RecordPullRequestWorkflowTransition(ctx, e.Store, event, transition)
	return nil
}

// reconcileMergedCleanup releases the branch lock and removes the task worktree
// after HandleReviewPullRequestClosed resolves an externally-merged lifecycle or
// blocked task to `merged` (#543, #953). It mirrors
// PolicyMergeGate.finishMerged's post-merge cleanup so the self-heal reconcile
// path does not leak a held branch lock or an on-disk worktree. Every step is
// best-effort and nil-safe: failures are
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
