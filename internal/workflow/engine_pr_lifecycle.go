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
	nativeFanoutDisabled := e.NativeReviewFanoutEnabled != nil && !e.NativeReviewFanoutEnabled(event.Repo)
	if event.SkipReviewFanout || nativeFanoutDisabled {
		return e.recordPullRequestBaseline(ctx, event)
	}
	// #1236: filter the roster down to agents that may actually review THIS head.
	// Only runs when a roster was configured, so the unconfigured-repo arm below
	// is reached exactly as before.
	//
	// The empty-roster arm below means "this repo has no native review
	// discipline" and runs the MERGE GATE. A filtered roster must therefore never
	// be allowed to collapse into it: a PR whose every configured reviewer turns
	// out ineligible has to fail CLOSED — no reviews, and above all no merge gate
	// — rather than inheriting unconfigured-repo behaviour and becoming eligible
	// to merge with no review at all. That is why this returns rather than
	// falling through.
	if len(reviewers) > 0 {
		eligible, dropped := e.eligibleReviewers(ctx, event.Repo, event.LeadAgent, reviewers)
		if len(dropped) > 0 {
			// Provenance: the fanout decision was previously silent, so the only way
			// to notice a bad roster was to query the job table for workflow-* rows
			// nobody dispatched (#1277, #1260).
			_ = e.Store.AddTaskEvent(ctx, db.TaskEvent{
				TaskID: event.TaskID,
				Kind:   "review_fanout_roster_filtered",
				Reason: fmt.Sprintf("pull request #%d at %s: enlisting [%s]; dropped %s",
					event.PullRequest, event.HeadSHA, strings.Join(eligible, " "), strings.Join(dropped, "; ")),
			})
		}
		if len(eligible) == 0 {
			_ = e.Store.AddTaskEvent(ctx, db.TaskEvent{
				TaskID: event.TaskID,
				Kind:   "review_fanout_no_eligible_reviewer",
				Reason: fmt.Sprintf("pull request #%d at %s: every configured reviewer was ineligible (%s); no review dispatched and the native merge gate was NOT run",
					event.PullRequest, event.HeadSHA, strings.Join(dropped, "; ")),
			})
			return e.recordPullRequestBaseline(ctx, event)
		}
		reviewers = eligible
		for _, reviewer := range reviewers {
			if err := e.ensureAgentAllowed(ctx, JobRequest{
				Agent:  reviewer,
				Action: "review",
				Repo:   event.Repo,
				Branch: event.Branch,
			}, ref); err != nil {
				return err
			}
		}
		selected, familyDropped, family, err := e.selectNativeReviewFamily(ctx, reviewers)
		if err != nil {
			return err
		}
		if len(familyDropped) > 0 {
			_ = e.Store.AddTaskEvent(ctx, db.TaskEvent{
				TaskID: event.TaskID,
				Kind:   "review_fanout_family_selected",
				Reason: fmt.Sprintf("pull request #%d at %s: selected runtime family %q with [%s]; dropped %s",
					event.PullRequest, event.HeadSHA, family, strings.Join(selected, " "), strings.Join(familyDropped, "; ")),
			})
		}
		if len(selected) == 0 {
			_ = e.Store.AddTaskEvent(ctx, db.TaskEvent{
				TaskID: event.TaskID,
				Kind:   "review_fanout_no_resolved_family",
				Reason: fmt.Sprintf("pull request #%d at %s: no configured reviewer had a resolvable runtime family; no review dispatched and the native merge gate was NOT run",
					event.PullRequest, event.HeadSHA),
			})
			return e.recordPullRequestBaseline(ctx, event)
		}
		reviewers = selected
	}
	if len(reviewers) == 0 {
		decision, err := e.runMergeGate(ctx, "", JobPayload{
			Repo:                    event.Repo,
			Branch:                  event.Branch,
			PullRequest:             event.PullRequest,
			PullRequestDraft:        event.PullRequestDraft,
			PullRequestDraftUnknown: event.PullRequestDraftUnknown,
			HeadSHA:                 event.HeadSHA,
			GoalID:                  event.GoalID,
			TaskID:                  event.TaskID,
			TaskTitle:               event.TaskTitle,
			LeadAgent:               event.LeadAgent,
		}, ref)
		if err != nil {
			return err
		}
		if decision.Merged {
			return nil
		}
		return e.recordPullRequestBaseline(ctx, event)
	}
	repeated, err := FindRepeatedReviewers(ctx, e.Store, event.Repo, event.PullRequest, event.HeadSHA, reviewers)
	if err != nil {
		return err
	}
	if len(repeated) > 0 {
		repeatedAgents := make(map[string]struct{}, len(repeated))
		for _, match := range repeated {
			repeatedAgents[strings.ToLower(strings.TrimSpace(match.Agent))] = struct{}{}
		}
		filtered := reviewers[:0]
		for _, reviewer := range reviewers {
			if _, exists := repeatedAgents[strings.ToLower(strings.TrimSpace(reviewer))]; !exists {
				filtered = append(filtered, reviewer)
			}
		}
		_ = e.Store.AddTaskEvent(ctx, db.TaskEvent{
			TaskID: event.TaskID,
			Kind:   ReviewLoopDetectedEventKind,
			Reason: fmt.Sprintf("pull request #%d at %s: skipped agents with existing exact-head verdicts [%s]",
				event.PullRequest, event.HeadSHA, strings.Join(matchAgents(repeated), " ")),
		})
		if len(filtered) == 0 {
			return e.block(ctx, ref, repeated[0].Reason())
		}
		reviewers = filtered
	}
	reviewRound, reviewJobs, err := e.nextReviewRound(ctx, event)
	if err != nil {
		return err
	}
	reviewScopes, err := e.followUpReviewScopes(ctx, event, reviewers, reviewJobs)
	if err != nil {
		var unavailable ReviewScopeUnavailableError
		if !errors.As(err, &unavailable) {
			return err
		}
		// The range from the head this reviewer last saw to the current head cannot
		// be scoped: a force-push/rebase left it DIVERGED, or the compare is
		// truncated. Blocking here wedged the loop PERMANENTLY — followUpReviewScopes
		// derives the prior head from the newest review job carrying a Result, and
		// while dispatch is blocked no review job is ever created, so every later
		// head re-compared the same unscopable range and blocked again with no path
		// back. Record the loss of scope and degrade to a FULL review at THIS head
		// instead: one wasted full re-review is the pre-scoping behaviour and it
		// self-heals, because the new job re-anchors the prior head for the next
		// round. The event write is propagated, not ignored: it is the only durable
		// record that this head's review was unscoped.
		reason := fmt.Sprintf("repo=%s pull_request=%d head_sha=%s: %s: %v", event.Repo, event.PullRequest, event.HeadSHA, ReviewScopeUnavailableMarker, err)
		recorded, recordedErr := e.reviewScopeUnavailableRecorded(ctx, event.TaskID, event.PullRequest, event.HeadSHA)
		if recordedErr != nil {
			return recordedErr
		}
		if !recorded {
			if eventErr := e.Store.AddTaskEvent(ctx, db.TaskEvent{TaskID: event.TaskID, Kind: "review_scope_unavailable", Reason: reason}); eventErr != nil {
				return eventErr
			}
		}
		reviewScopes = nil
	}
	// Opt-in risk-tiered adaptive review (#650). When RiskTiersEnabled, classify
	// the PR (label > path > default). A `high` tier replaces the single native
	// fan-out with a refutation-lens delegation batch synthesized by the EXISTING
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
			return e.dispatchHighRiskReview(ctx, event, reviewers, reviewScopes, classification, reviewRound, ref)
		}
	}
	// A reviewer that already HAS a review job at this exact head and round gets no
	// second one, whatever state that job is in. The deterministic id encodes
	// reviewer + head + round, but the idempotent-enqueue collision check compares
	// DERIVED content (payloadMatchesRequest compares Instructions and WorktreePath),
	// so a round that scoped on one derivation and degraded to an unscoped review on
	// another surfaced a raw `UNIQUE constraint failed: jobs.id` out of this function
	// and re-fired it every poll. Skipping on identity is the idempotent answer.
	// FindRepeatedReviewers cannot serve this: it queries succeeded verdicts only, so
	// it never sees a queued, running or failed leg.
	if existing := reviewLegsAtHead(reviewJobs, event, reviewRound); len(existing) > 0 {
		remaining := make([]string, 0, len(reviewers))
		for _, reviewer := range reviewers {
			if _, dispatched := existing[strings.ToLower(strings.TrimSpace(reviewer))]; !dispatched {
				remaining = append(remaining, reviewer)
			}
		}
		if len(remaining) == 0 {
			return e.recordPullRequestBaseline(ctx, event)
		}
		reviewers = remaining
	}
	requests := make([]JobRequest, 0, len(reviewers))
	for _, reviewer := range reviewers {
		instructions := fmt.Sprintf(
			"Review pull request #%d for task %s.",
			event.PullRequest,
			taskLabel(event.TaskID, event.TaskTitle),
		)
		scope := reviewScopeForRoutine(reviewScopes, reviewer)
		if scope != nil {
			instructions = scopedReviewInstructions(event, scope)
		}
		request := JobRequest{
			PolicyExempt: "exempt",
			// #1250: fanout children were enqueued with NO attribution — measured at
			// 0 of 99 workflow-* jobs. An unattributed job's blocked event has no
			// owner to wake (#1347), so a whole class of jobs could never route one.
			// The role rides the event from the branch lock, one source for both
			// triggers.
			ActingOrgRole: event.ActingOrgRole,
			Agent:         reviewer,
			Action:        "review",
			Repo:          event.Repo,
			Branch:        event.Branch,
			PullRequest:   event.PullRequest,
			HeadSHA:       event.HeadSHA,
			GoalID:        event.GoalID,
			TaskID:        event.TaskID,
			TaskTitle:     event.TaskTitle,
			LeadAgent:     event.LeadAgent,
			Reviewers:     reviewers,
			ReviewRound:   reviewRound,
			ReviewScope:   scope,
			Sender:        event.Sender,
			Instructions:  instructions,
		}
		requests = append(requests, request)
	}

	for _, request := range requests {
		prepared, allocated, err := e.prepareNativeReviewWorktree(ctx, request)
		if err != nil {
			return err
		}
		if err := e.enqueue(ctx, prepared); err != nil {
			if allocated {
				// The worktree now belongs to a job that does not exist, and its path is
				// DETERMINISTIC: leaving it would make every later poll's allocation fail
				// with "already exists" and wedge this head permanently. Mirrors the
				// worker helper, which likewise reclaims on a failed payload persist.
				e.releaseNativeReviewWorktree(context.WithoutCancel(ctx), prepared.WorktreePath)
			}
			return err
		}
		if allocated {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   prepared.ID,
				Kind:    "review_worktree_allocated_exact_head",
				Message: fmt.Sprintf("allocated owned read-only worktree at review head %s before enqueue", strings.TrimSpace(prepared.HeadSHA)),
			})
		}
	}
	if err := e.setTaskState(ctx, ref, TaskReviewing); err != nil {
		return err
	}
	return e.recordPullRequestBaseline(ctx, event)
}

// prepareNativeReviewWorktree gives a ROUTINE native review leg its own detached
// exact-head worktree BEFORE it is enqueued, so the job is born with its FINAL
// WorktreePath and FINAL Instructions. That is the invariant every OTHER
// read-only dispatch path already holds (the `agent review/ask` CLI dispatch, the
// read-only delegation fan-out, and the pipeline stages all allocate then
// enqueue). Allocating AFTER enqueue — in the worker, which is where the routine
// leg alone used to do it — was one asymmetry behind three separate defects:
//
//   - queuedJobCheckoutKey reads payload.WorktreePath at scheduler admission, so
//     an empty path keyed every reviewer leg to the shared repo:<repo> key. Only
//     one was admitted per tick, each holding that key for a full LLM review.
//   - The worker then mutated Instructions and WorktreePath. A re-poll at the same
//     head re-derives the SAME deterministic job id (nextReviewRound returns the
//     existing head's round) but no longer matched the stored payload, so
//     existingJobMatchesRequest returned false and the raw
//     "UNIQUE constraint failed: jobs.id" surfaced out of HandlePullRequestOpened.
//   - An allocation failure terminally failed the leg inside the worker, and
//     because the payload was left UNMUTATED the next poll's re-enqueue matched it
//     and was a silent no-op — the id was burned and FindRepeatedReviewers only
//     re-enlists succeeded verdicts, so that verdict could never be re-attempted.
//
// An engine with no read-only worktree manager (or no Home / DelegationCheckout)
// is left byte-identical: the leg is enqueued with no path and the worker's
// prepareNativeReviewWorktreeForRunner allocates it exactly as before. The two
// configurations are DISJOINT — a leg born with a WorktreePath makes that worker
// helper return early on its own gate — so no configuration ever has two live
// allocation paths. The returned bool reports whether this call really allocated,
// so the durable audit event is written exactly once per worktree.
func (e Engine) prepareNativeReviewWorktree(ctx context.Context, request JobRequest) (JobRequest, bool, error) {
	manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager)
	if !ok || strings.TrimSpace(e.Home) == "" || strings.TrimSpace(e.DelegationCheckout) == "" {
		return request, false, nil
	}
	head := strings.TrimSpace(request.HeadSHA)
	if head == "" || request.PullRequest <= 0 {
		// Nothing to pin a worktree to (a PR-less review heartbeat, #564). The
		// worker's shared-checkout arm already handles this shape.
		return request, false, nil
	}
	if request.ID == "" {
		request.ID = e.jobID(request)
	}
	// jobID hashes Instructions, so it is resolved from the request as built — the
	// id is byte-identical to the one this leg has always had, and appending the
	// worktree note below cannot move it. The path is deterministic in
	// (home, repo, job id) and AllocateReadOnlyWorktree recomputes exactly this
	// value; resolving it here lets the idempotence check below compare the SAME
	// payload the enqueue will store.
	path, err := DelegationWorktreePath(e.Home, request.Repo, request.ID, "readonly-seat", 0)
	if err != nil {
		return request, false, err
	}
	request.WorktreePath = path
	request.ReadOnlyWorktree = true
	request.ReadOnlySeat = true
	if note := readOnlyWorktreeContextNote(e.DelegationCheckout); note != "" {
		request.Instructions += note
	}
	// `git worktree add` at an already-occupied path FAILS, so a second poll at the
	// same head must never reach the allocation: the leg is already enqueued and
	// e.enqueue no-ops on the payload match. Deciding that BEFORE the
	// side-effecting call is what keeps the re-poll idempotent instead of turning
	// it into an allocation error.
	matches, err := e.existingJobMatchesRequest(ctx, request)
	if err != nil {
		return request, false, err
	}
	if matches {
		return request, false, nil
	}
	if err := e.allocateNativeReviewWorktree(ctx, request, manager, head); err != nil {
		return request, false, err
	}
	return request, true, nil
}

// allocateNativeReviewWorktree creates the detached worktree at the review head,
// retrying once after fetching pull/<n>/head. It uses the SHORT
// ReadOnlyWorktreeDispatchLockWaitBudget, like every other pre-enqueue read-only
// site: this runs synchronously on the daemon's per-repo poll loop, where the full
// two-minute checkout-mutation wait would freeze that repo's whole dispatch loop.
func (e Engine) allocateNativeReviewWorktree(ctx context.Context, request JobRequest, manager ReadOnlyWorktreeManager, head string) error {
	allocate := func() error {
		_, err := AllocateReadOnlyWorktree(ctx, e.Store, e.Home, request.Repo, e.DelegationCheckout, request.ID, "readonly-seat", 0, head, ReadOnlyWorktreeDispatchLockWaitBudget, manager)
		return err
	}
	err := allocate()
	if err == nil {
		return nil
	}
	// A spent lock-wait budget is transient and self-healing: the holder is another
	// worker's short shared-.git op. Return it so THIS poll fails and the daemon
	// re-fires HandlePullRequestOpened, which re-derives the same id and
	// re-attempts. A fetch cannot help it, so do not spend one.
	var blocked BlockedError
	if errors.As(err, &blocked) {
		return fmt.Errorf("allocate exact-head review worktree for %s: %w", request.Agent, err)
	}
	// A cold checkout may not carry the PR commit object even though the forge
	// supplied its SHA, and nothing in the poll path fetches it; `git worktree add
	// --detach <sha>` then fails with "invalid reference". Both other exact-head
	// allocation sites carry this pull/<n>/head retry.
	fetcher, canFetch := manager.(PullRequestFetcher)
	if !canFetch {
		return fmt.Errorf("allocate exact-head review worktree for %s: %w", request.Agent, err)
	}
	if fetchErr := fetcher.FetchPullRequest(ctx, "origin", request.PullRequest); fetchErr != nil {
		return fmt.Errorf("allocate exact-head review worktree for %s: %w; fetch PR ref: %v", request.Agent, err, fetchErr)
	}
	if retryErr := allocate(); retryErr != nil {
		return fmt.Errorf("allocate exact-head review worktree for %s after fetching pull/%d/head: %w", request.Agent, request.PullRequest, retryErr)
	}
	return nil
}

// releaseNativeReviewWorktree force-removes a worktree this poll allocated for a
// leg that then failed to enqueue. Best-effort: the caller is already returning
// the enqueue error, and a failed removal leaves exactly the orphan that
// removal was meant to avoid, which the next poll surfaces as an allocation
// error rather than hiding.
func (e Engine) releaseNativeReviewWorktree(ctx context.Context, path string) {
	manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager)
	if !ok || strings.TrimSpace(path) == "" {
		return
	}
	_ = manager.RemoveWorktreeForce(ctx, path)
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
func (e Engine) dispatchHighRiskReview(ctx context.Context, event PullRequestEvent, reviewers []string, reviewScopes map[reviewScopeKey]*ReviewScope, classification RiskClassification, round string, ref taskRef) error {
	coordID := "review-coordinator/" + event.Branch + "/" + round
	if _, err := e.Store.GetJob(ctx, coordID); err == nil {
		// Already dispatched for this head SHA/round: idempotent no-op.
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	delegations := highRiskLensDelegations(reviewers, event, reviewScopes)
	if len(delegations) < 2 {
		// Defensive: no reviewers to fan out to. Fall back to recording the baseline
		// rather than silently dropping the PR (should not happen — callers guarantee
		// len(reviewers) >= 1, which yields >= 2 lenses).
		return e.recordPullRequestBaseline(ctx, event)
	}

	coordPayload := JobPayload{
		// #1250: the high-risk path diverts BEFORE the ordinary attributed request
		// loop, so without this the lens children inherit an empty coordinator
		// payload and the whole risk-tiered branch stays unattributed.
		ActingOrgRole: event.ActingOrgRole,
		Repo:          event.Repo,
		Branch:        event.Branch,
		PullRequest:   event.PullRequest,
		HeadSHA:       event.HeadSHA,
		GoalID:        event.GoalID,
		TaskID:        event.TaskID,
		TaskTitle:     event.TaskTitle,
		LeadAgent:     event.LeadAgent,
		Reviewers:     reviewers,
		ReviewRound:   round,
		Sender:        event.Sender,
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
	// A local review task deliberately owns no branch so it cannot collide with the
	// implement task that owns (repo, head branch). Carrying the PR's branch into
	// its ref would let setTaskState's branch-reuse fallback advance that OTHER
	// task instead of this one. Mirrors the same guard on the PR-closed path.
	if stored, storedErr := e.Store.GetTask(ctx, event.TaskID); storedErr == nil && strings.TrimSpace(stored.Branch) == "" {
		ref.Branch = ""
	}
	_, err := e.runMergeGateWithHumanMerge(ctx, "", JobPayload{
		Repo:                    event.Repo,
		Branch:                  event.Branch,
		PullRequest:             event.PullRequest,
		PullRequestDraft:        event.PullRequestDraft,
		PullRequestDraftUnknown: event.PullRequestDraftUnknown,
		HeadSHA:                 event.HeadSHA,
		GoalID:                  event.GoalID,
		TaskID:                  event.TaskID,
		TaskTitle:               event.TaskTitle,
		LeadAgent:               event.LeadAgent,
		Reviewers:               compactStrings(append([]string{}, event.RequiredReviewers...)),
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
			changed, observedState, _, err := e.Store.TransitionTaskStateWithEventObserved(ctx, task.ID,
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
			taskState = TaskState(observedState)
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

// eligibleReviewers filters a configured reviewer roster down to the agents that
// may actually review THIS head, and explains every drop so the fanout decision
// stops being silent (#1236, #1277).
//
// Two filters, neither of which the fanout previously had:
//
//   - THE IMPLEMENTER IS NEVER A REVIEWER OF ITS OWN HEAD. event.LeadAgent
//     identifies the implementer on both triggers — the implementing job's agent
//     on the in-process path, the branch-lock owner on the daemon PR-watcher path
//     — so one exclusion covers both. Reviewer identity distinct from implementer
//     is already fleet doctrine; it belongs in the engine and not only in briefs,
//     because the merge gate ignores verdict authorship (#1114) and a
//     self-approval produced this way is indistinguishable from a real one.
//   - THE ROSTER IS REPO-AWARE. The default roster is global config applied to
//     every repo, so a seat scoped elsewhere was enlisted anyway — on dashboard
//     PR #137 that was ["lead"], scoped to gitmoot/gitmoot. That fanout could not
//     have produced a verdict, and worse, the off-scope reviewer BLOCKED the task
//     at the dispatch preflight instead of being quietly skipped.
//
// It filters ONLY those two cases. Every other roster problem — an unsubscribed
// name, a missing `review` capability — is left in the roster deliberately, so
// the existing dispatch preflight still BLOCKS it exactly as before. That
// distinction is the point: a name that resolves to nothing is a MISCONFIGURED
// ROSTER and a human has to fix it, and silently dropping it would hide a typo
// while quietly reducing review coverage. A reviewer that exists but is wrong
// FOR THIS HEAD is not a misconfiguration — it is a correct global roster meeting
// a specific PR — and that is what gets filtered.
//
// Note an unknown agent is indistinguishable from an off-scope one at
// AgentCanAccessRepo (both return false, nil), so existence is checked first and
// unknown names are KEPT for the preflight to reject.
func (e Engine) eligibleReviewers(ctx context.Context, repo string, implementer string, reviewers []string) ([]string, []string) {
	implementer = strings.TrimSpace(implementer)
	eligible := make([]string, 0, len(reviewers))
	dropped := make([]string, 0, len(reviewers))
	for _, name := range reviewers {
		if implementer != "" && name == implementer {
			dropped = append(dropped, fmt.Sprintf("%s (implemented this head)", name))
			continue
		}
		if _, err := e.Store.GetAgent(ctx, name); err != nil {
			// Unknown or unreadable: keep it so the preflight blocks the task.
			eligible = append(eligible, name)
			continue
		}
		allowed, err := e.Store.AgentCanAccessRepo(ctx, name, repo)
		if err == nil && !allowed {
			dropped = append(dropped, fmt.Sprintf("%s (not scoped to %s)", name, repo))
			continue
		}
		eligible = append(eligible, name)
	}
	return eligible, dropped
}

// selectNativeReviewFamily keeps the configured order and selects the runtime
// family of the first reviewer whose family can be resolved. Later reviewers in
// that family remain in the fanout; every other family is reserved for an
// explicit `agent review` request.
func (e Engine) selectNativeReviewFamily(ctx context.Context, reviewers []string) ([]string, []string, string, error) {
	selected := make([]string, 0, len(reviewers))
	dropped := make([]string, 0, len(reviewers))
	selectedFamily := ""
	for _, reviewer := range reviewers {
		family, ok, err := ResolveRuntimeFamily(ctx, e.Store, reviewer, "")
		if err != nil {
			return nil, nil, "", err
		}
		if !ok {
			dropped = append(dropped, fmt.Sprintf("%s (runtime family unresolved)", reviewer))
			continue
		}
		if selectedFamily == "" {
			selectedFamily = family
		}
		if family != selectedFamily {
			dropped = append(dropped, fmt.Sprintf("%s (runtime family %s)", reviewer, family))
			continue
		}
		selected = append(selected, reviewer)
	}
	return selected, dropped, selectedFamily, nil
}

func matchAgents(matches []ReviewLoopMatch) []string {
	agents := make([]string, 0, len(matches))
	for _, match := range matches {
		agents = append(agents, match.Agent)
	}
	return agents
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

func (e Engine) nextReviewRound(ctx context.Context, event PullRequestEvent) (string, []db.Job, error) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return "", nil, err
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
			return "", nil, err
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
		return existingHeadRound, jobs, nil
	}
	return "review-" + strconv.Itoa(len(rounds)+1), jobs, nil
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
