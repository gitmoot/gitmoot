package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

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
		PolicyExempt: "exempt",
		Agent:        leadAgent,
		Action:       "implement",
		Repo:         payload.Repo,
		Branch:       payload.Branch,
		PullRequest:  payload.PullRequest,
		HeadSHA:      payload.HeadSHA,
		GoalID:       payload.GoalID,
		TaskID:       payload.TaskID,
		TaskTitle:    payload.TaskTitle,
		LeadAgent:    leadAgent,
		Reviewers:     e.requiredReviewers(payload),
		ReviewRound:   payload.ReviewRound,
		Sender:        reviewer,
		ActingOrgRole: payload.ActingOrgRole,
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
		payload.WorkflowID == request.WorkflowID &&
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
	// Risk-tiered synthesis continuation (#650): the high-risk review coordinator is
	// a SYNTHETIC job the engine seeds on the LEAD agent, and its continuation
	// (maybeEnqueueContinuation) is an `ask` job on that same lead. A normal lead
	// carries implement/review but need not carry `ask`, so requiring `ask` here
	// would BLOCK the synthesis of an already-approved high-risk review — a
	// non-additive capability demand the routine review path never imposed. The
	// continuation is synthesis-only (it summarizes the lens findings; it does not
	// grant any write/review authority), so allow it to run without the `ask` grant.
	if job.Type == "ask" && payload.RiskTier == RiskTierHigh {
		allowMissingCapability = true
	}
	return e.ensureAgentAllowedWithBranchOwner(ctx, JobRequest{
		Agent:        authorizationAgent,
		Action:       job.Type,
		Repo:         payload.Repo,
		Sender:       payload.Sender,
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
	if request.Action == "produce" {
		if request.Sender != PipelineJobSender {
			return e.block(ctx, ref, "job action produce is reserved for pipeline stages")
		}
		if err := runtime.ProduceDispatchError(request.Action, runtime.Agent{Name: agent.Name, Runtime: agent.Runtime, AutonomyPolicy: agent.AutonomyPolicy}); err != nil {
			return e.block(ctx, ref, err.Error())
		}
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
		if decision.Deferred {
			// Park the task in ready_to_merge (NOT whatever state it arrived in) so
			// the daemon's pullRequestReadyToMerge poll re-drives it every tick until
			// the in-flight branch job settles. This matters for the no-reviewers
			// auto-merge path: HandlePullRequestOpened reaches here with the task in
			// pull_request_open, a state that poll does not re-evaluate — without this
			// the deferred PR would wedge unmerged. Mirrors PolicyMergeGate.pending
			// (which parks via a Ready:true tail); a deferred decision is never
			// blocked, failed, or harvested.
			return decision, e.setTaskState(ctx, ref, TaskReadyToMerge)
		}
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
			// Only an AUTHORITATIVE template-quality block (external CI failed, a
			// blocking review captured, closed-without-merge) is worth waking an org
			// role for and harvesting. Transient/infra blocks (branch staleness, dirty
			// worktree, missing head/base SHA, freshness-unknown) are self-clearing
			// daemon-retry noise — the same set the #465 harvest excludes — so gating
			// the merge_guard wake here keeps it from re-firing on every push of a
			// self-clearing condition.
			//
			// On the poll-driven path (HandlePullRequestOpened) the payload carries no
			// driving job, so the id falls through to the task id — the only stable
			// identifier the task-scoped merge gate has. A rule --match on job id will
			// not match these; --match on repo (or an empty filter) will.
			jobID := firstNonEmptyString(payload.RootJobID, payload.ParentJobID, ref.ID, payload.TaskID)
			rootID := firstNonEmptyString(payload.RootJobID, jobID)
			ev := events.NewEvent(
				events.EventJobBlocked,
				jobID,
				rootID,
				payload.Repo,
				string(TaskBlocked),
				reason,
				e.now(),
				RedactCommentText,
			)
			ev.Cause = "merge_guard"
			events.EmitEvent(ctx, e.EventSink, ev)
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
