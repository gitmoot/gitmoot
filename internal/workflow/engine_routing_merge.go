package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

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
	policy, configured, err := e.Store.PullRequestAutoFixPolicyFor(ctx, payload.Repo, payload.PullRequest)
	if err != nil {
		return err
	}
	if configured && policy.Disabled {
		return e.blockAutoFix(ctx, ref, fmt.Sprintf(
			"auto-fix disabled for %s pull request #%d by %s: %s",
			payload.Repo,
			payload.PullRequest,
			policy.Actor,
			policy.Reason,
		))
	}
	leadAgent, err := e.autoFixOwner(ctx, payload)
	if err != nil {
		return e.blockAutoFix(ctx, ref, err.Error())
	}
	branchOwner, err := e.fixBranchLockOwner(ctx, payload, leadAgent)
	if err != nil {
		return e.blockAutoFix(ctx, ref, fmt.Sprintf("auto-fix branch lock owner unresolved: %v", err))
	}
	request := JobRequest{
		PolicyExempt:  "exempt",
		Agent:         leadAgent,
		Action:        "implement",
		Repo:          payload.Repo,
		Branch:        payload.Branch,
		PullRequest:   payload.PullRequest,
		HeadSHA:       payload.HeadSHA,
		GoalID:        payload.GoalID,
		TaskID:        payload.TaskID,
		TaskTitle:     payload.TaskTitle,
		LeadAgent:     leadAgent,
		Reviewers:     e.requiredReviewers(payload),
		ReviewRound:   payload.ReviewRound,
		Sender:        reviewer,
		ActingOrgRole: payload.ActingOrgRole,
		Instructions:  reviewFixInstructions(reviewer, result),
	}
	if request.ID == "" {
		request.ID = e.jobID(request)
	}
	if err := e.ensureAgentAllowedWithBranchOwner(ctx, request, branchOwner, ref, false); err != nil {
		return err
	}
	// Fail closed at the dispatch site. Falling through here would enqueue the fix
	// with no WorktreePath, which resolves to the registered checkout and permits a
	// concurrent agent to overwrite the lane owner's uncommitted work (#1462).
	if e.FixWorktreeAllocator == nil {
		return errors.New("review fix dispatch requires a writable per-job worktree allocator")
	}
	allocation, err := e.FixWorktreeAllocator(ctx, FixWorktreeRequest{
		JobID:  request.ID,
		Repo:   request.Repo,
		Branch: request.Branch,
	})
	if err != nil {
		return fmt.Errorf("allocate review fix worktree: %w", err)
	}
	request.WorktreePath = strings.TrimSpace(allocation.Path)
	if request.WorktreePath == "" {
		return errors.New("review fix worktree allocator returned an empty path")
	}
	request.FixWorktree = true
	if err := e.enqueue(ctx, request); err != nil {
		if allocation.Created {
			// Never delete a standalone fix clone: move it aside for the operator.
			_, _ = SetAsideFixClone(request.WorktreePath)
		}
		return err
	}
	return nil
}

func (e Engine) autoFixOwner(ctx context.Context, payload JobPayload) (string, error) {
	if role := strings.TrimSpace(payload.ActingOrgRole); role != "" {
		return role, nil
	}
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return "", err
	}
	evidence := collectImplementerAttribution(jobs, payload)
	switch {
	case evidence.sawMalformedPayload:
		return "", errors.New("auto-fix ownership unresolved: an implement job has a malformed payload")
	case evidence.sawEmptyAgent:
		return "", errors.New("auto-fix ownership unresolved: a matching implement job has no agent")
	}
	agents := make([]string, 0, len(evidence.agents))
	for agent := range evidence.agents {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	switch len(agents) {
	case 0:
		return "", fmt.Errorf("auto-fix ownership unresolved: %s", evidence.failureReason())
	case 1:
		return agents[0], nil
	default:
		return "", fmt.Errorf("auto-fix ownership ambiguous: task %s has implementing agents [%s]", payload.TaskID, strings.Join(agents, " "))
	}
}

// fixBranchLockOwner preserves the task's existing serialization owner while an
// acting org role executes the isolated fix. The lock is never an ownership
// source: it cannot change the agent selected by autoFixOwner.
func (e Engine) fixBranchLockOwner(ctx context.Context, payload JobPayload, executionAgent string) (string, error) {
	lock, err := e.Store.GetBranchLock(ctx, payload.Repo, payload.Branch)
	if errors.Is(err, sql.ErrNoRows) {
		return executionAgent, nil
	}
	if err != nil {
		return "", err
	}
	owner := strings.TrimSpace(lock.Owner)
	if owner == "" {
		return "", errors.New("existing branch lock has no owner")
	}
	return owner, nil
}

func (e Engine) blockAutoFix(ctx context.Context, ref taskRef, reason string) error {
	return e.blockAttributed(ctx, ref, "review_auto_fix_blocked", reason, "auto-fix")
}

// blockMergeGate attributes a merge-gate block on the task itself (#1562).
// All block writers converge on blockTask, so the event naming the owner is
// written only after the durable blocked transition.
func (e Engine) blockMergeGate(ctx context.Context, ref taskRef, reason string) error {
	return e.blockAttributed(ctx, ref, "merge_gate_blocked", reason, "merge-gate")
}

// blockSynthesisGate attributes a coordinator synthesis-gate block (vote or
// quorum unmet) on the task, mirroring blockMergeGate and blockAutoFix (#1562).
// Generic workflow blocks use workflow_blocked at the same choke point, making
// the latest blocking event a complete ownership record rather than a selective
// list of callers.
func (e Engine) blockSynthesisGate(ctx context.Context, ref taskRef, kind string, reason string) error {
	return e.blockAttributed(ctx, ref, kind, reason, "synthesis-gate")
}

// blockAttributed routes named blockers through the shared block-first,
// attribute-second choke point.
func (e Engine) blockAttributed(ctx context.Context, ref taskRef, kind string, reason string, label string) error {
	return e.blockTask(ctx, ref, kind, reason, label)
}

func (e Engine) allRequiredReviewersApproved(ctx context.Context, currentReviewer string, payload JobPayload) (bool, error) {
	required := e.requiredReviewers(payload)
	if len(required) == 0 {
		return true, nil
	}

	blockingSeverity := e.reviewBlockingSeverity(payload.Repo)
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
		// A fan-out announces a panel; it does not approve. Counting it here would
		// let a coordinator satisfy a required-reviewer slot without any delegate
		// having reported (#1685). The panel's children carry their own rows and
		// are counted on their own agents.
		if ResultIsFanOut(jobPayload.Result) {
			continue
		}
		if effectiveReviewDecisionForPayload(jobPayload, blockingSeverity) == "approved" {
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

// mergeGateExpectedTaskState resolves the task state the merge gate must claim
// before it merges, and reports whether the approval may proceed at all.
//
// It exists because the approved arm used to hand runMergeGate a CONSTANT
// TaskReviewing. Once a task reached changes_requested that claim could never
// match, so a later approval at a fresh head could not clear the gate and the
// task wedged permanently (#1834) - setReviewingIfNotChangesRequested is one-way
// by design and correctly refuses to erase a live objection, so nothing re-armed
// it. The fix is not to erase the objection earlier but to let an approval that
// is DEMONSTRABLY NEWER than it own the claim.
// The third return is the refusal reason and the fourth says whether the hold is
// TRANSIENT. A transient hold must be retried rather than settled: the caller
// returns an error so the advancement stays unreconciled and the daemon's
// advance-retry re-drives it. A terminal hold records a durable event and stops.
func (e Engine) mergeGateExpectedTaskState(ctx context.Context, ref taskRef, payload JobPayload) (TaskState, bool, string, bool, error) {
	if strings.TrimSpace(ref.ID) == "" {
		return TaskReviewing, true, "", false, nil
	}
	task, err := e.Store.GetTask(ctx, ref.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, "", false, err
	}
	if err != nil || task.State != string(TaskChangesRequested) {
		return TaskReviewing, true, "", false, nil
	}
	admitted, reason, retryable, err := e.approvalSupersedesChangesRequested(ctx, payload)
	if err != nil {
		return "", false, "", false, err
	}
	if !admitted {
		return "", false, reason, retryable, nil
	}
	return TaskChangesRequested, true, "", false, nil
}

// approvalSupersedesChangesRequested answers whether THIS approving review is
// bound to the task's current head and is unopposed there. It returns the reason
// when it refuses, so the caller can record why an approval did not advance
// rather than leaving a silent no-op - silence is what made #1834 invisible
// until four tasks had wedged.
//
// THE CURRENT HEAD COMES FROM THE OBSERVED PULL REQUEST, not from guessing which
// review row is newest. `tasks` has no head column, but `pull_requests.head_sha`
// already records what the forge last reported and is maintained by the daemon
// poll, the PR lifecycle and the merge gate - so this needs no migration and no
// recency heuristic. Ordering review rows would have been unsound: ListJobs
// orders BY ID, and created_at is an ISO string with SECOND granularity, so two
// reviews dispatched in the same second cannot be separated at all. A tie-break
// on job id is deterministic but arbitrary, which is precisely the "accident of
// iteration order" this rule must not rest on.
//
// THE SAME-HEAD TIE IS RULED EXPLICITLY: if any succeeded review at the current
// head requested changes, the objection stands even when a different reviewer
// approved the same head. An approval must not merge over a peer's live
// objection; the objector re-reviews, or a new head supersedes them both.
func (e Engine) approvalSupersedesChangesRequested(ctx context.Context, payload JobPayload) (bool, string, bool, error) {
	approvingHead := strings.TrimSpace(payload.HeadSHA)
	if approvingHead == "" {
		// Terminal: a review row does not gain a head later.
		return false, "the approving review carries no head SHA, so it cannot be bound to the current head", false, nil
	}
	currentHead := ""
	if payload.PullRequest > 0 {
		pr, err := e.Store.GetPullRequest(ctx, payload.Repo, int64(payload.PullRequest))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, "", false, err
		}
		if err == nil {
			currentHead = strings.TrimSpace(pr.HeadSHA)
		}
	}
	if currentHead == "" {
		// The current head could not be confirmed, so there is NO evidence this
		// approval is newer than the objection. An earlier draft admitted here when
		// some review had objected at a different head, but "a different head"
		// carries no ordering: this PR rejects job recency as unsound precisely
		// because ListJobs orders by id and created_at is second-granularity, and
		// the same argument disqualifies it here. Admitting would let an approval
		// merge over a live, current objection whenever the row is missing - which
		// the CLI dispatch path can reach on a PR the daemon never polled
		// (#1871 review, P1). Refusing only leaves the task where it already is.
		//
		// TRANSIENT: the row appears as soon as the daemon polls the PR, so this
		// hold must be RETRIED, not settled. Settling it silently is the recovery
		// wedge the round-3 review measured - the approval was recorded as advanced
		// and nothing re-drove it once the row landed (#1871 review round 3, P1).
		return false, fmt.Sprintf(
			"no observed pull request row records a current head for %s#%d, so this approval cannot be shown to be bound to it",
			payload.Repo, payload.PullRequest), true, nil
	}
	if approvingHead != currentHead {
		return false, fmt.Sprintf(
			"approval is bound to head %s but the pull request's current head is %s; an approval at a superseded head does not clear changes_requested",
			approvingHead, currentHead), false, nil
	}
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return false, "", false, err
	}
	blockingSeverity := e.reviewBlockingSeverity(payload.Repo)
	for _, job := range jobs {
		if job.Type != "review" || job.State != "succeeded" {
			continue
		}
		jobPayload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return false, "", false, err
		}
		if !sameTask(payload, jobPayload) || jobPayload.Result == nil || ResultIsFanOut(jobPayload.Result) {
			continue
		}
		if effectiveReviewDecisionForPayload(jobPayload, blockingSeverity) != "changes_requested" {
			continue
		}
		// Only an objection AT THE CURRENT HEAD can block: by here the approving
		// head IS the current head, and an objection at any other head is one the
		// current head supersedes. No ordering is needed or attempted.
		if strings.TrimSpace(jobPayload.HeadSHA) == approvingHead {
			return false, fmt.Sprintf(
				"a review at head %s requested changes, so the objection stands even though this review approved the same head",
				approvingHead), false, nil
		}
	}
	return true, "", false, nil
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

func (e Engine) enqueue(ctx context.Context, request JobRequest) error {
	if request.ID == "" {
		request.ID = e.jobID(request)
	}
	// An enqueue is irreversible, so it is bound to advance ownership immediately
	// before it happens rather than at the barrier that decided it (#1673). A pass
	// that has lost ownership aborts here instead of minting a job for a lifecycle
	// that no longer exists; ordinary callers carry no anchor and are unaffected.
	if err := e.renewSupersedeAdvanceLease(ctx); err != nil {
		return err
	}
	// CAPTURING: a resolution's enqueue is PREPARED, not written, so it can commit in
	// the same transaction as the task write and the receipt. PrepareEnqueue performs
	// no durable write and no git/network work.
	if e.capturing() {
		prepared, perr := e.mailbox().PrepareEnqueue(ctx, request)
		if perr != nil {
			return perr
		}
		e.resolutionSink.jobs = append(e.resolutionSink.jobs, db.PreparedJob{Job: prepared.Job, Events: prepared.Events})
		return nil
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
		payload.FixWorktree == request.FixWorktree &&
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
	if job.Type == "implement" && payload.FixWorktree {
		fixOwner, err := e.fixBranchLockOwner(ctx, payload, branchOwner)
		if err != nil {
			return e.block(ctx, ref, fmt.Sprintf("auto-fix branch lock owner unresolved: %v", err))
		}
		branchOwner = fixOwner
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
		Agent:  authorizationAgent,
		Action: job.Type,
		// #1250: the executor preflight is the path that actually reaches
		// ensureBranchLock for a task-run or isolated-delegation job, and the
		// allocator has already created that lock BLANK. Reconstructing the request
		// without the role meant the sole writer was handed an empty value and
		// correctly refused to fill — so the attribution died here, one call short
		// of the lock, on a payload that was carrying it the whole time.
		ActingOrgRole: payload.ActingOrgRole,
		Repo:          payload.Repo,
		Sender:        payload.Sender,
		Branch:        payload.Branch,
		DelegationID:  payload.DelegationID,
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
			return e.ensureBranchLock(ctx, request.Repo, request.Branch, branchOwner, request.ActingOrgRole, ref)
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
		if err := e.ensureBranchLock(ctx, request.Repo, request.Branch, branchOwner, request.ActingOrgRole, ref); err != nil {
			return err
		}
	}
	return nil
}

// ensureBranchLock also STAMPS the acting org role onto the lock at creation
// (#1250). This is the single writer of that attribution: it is captured when the
// branch is taken and never rewritten, so both PR-open triggers read one value
// and cannot drift. An empty role is legitimate and means unattributed.
func (e Engine) ensureBranchLock(ctx context.Context, repo string, branch string, owner string, actingOrgRole string, ref taskRef) error {
	if strings.TrimSpace(branch) == "" {
		return e.block(ctx, ref, "branch lock rejected action: branch is required")
	}
	acquired, err := e.Store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo, Branch: branch, Owner: owner, ActingOrgRole: NormalizeActingOrgRole(actingOrgRole)})
	if err != nil {
		return err
	}
	if !acquired {
		return e.block(ctx, ref, fmt.Sprintf("branch lock rejected action for %s", branch))
	}
	return nil
}

func (e Engine) runMergeGate(ctx context.Context, reviewer string, payload JobPayload, ref taskRef, expectedTaskState TaskState) (MergeDecision, error) {
	return e.runMergeGateWithHumanMerge(ctx, reviewer, payload, ref, false, string(expectedTaskState))
}

func (e Engine) runMergeGateWithHumanMerge(ctx context.Context, reviewer string, payload JobPayload, ref taskRef, humanMergeRequested bool, expectedTaskState string) (MergeDecision, error) {
	if strings.TrimSpace(ref.ID) != "" && strings.TrimSpace(expectedTaskState) == "" {
		return MergeDecision{}, errors.New("task-owned merge gate requires an expected task state")
	}
	if e.MergeGate == nil {
		return MergeDecision{Ready: true}, e.setTaskState(ctx, ref, TaskReadyToMerge)
	}
	reviewRequired, err := e.mergeGateReviewRequired(ctx, payload)
	if err != nil {
		return MergeDecision{}, err
	}
	decision, err := e.MergeGate.Evaluate(ctx, MergeRequest{
		Repo:                    payload.Repo,
		Branch:                  payload.Branch,
		PullRequest:             payload.PullRequest,
		PullRequestDraft:        payload.PullRequestDraft,
		PullRequestDraftUnknown: payload.PullRequestDraftUnknown,
		PullRequestMerged:       payload.PullRequestMerged,
		HeadSHA:                 payload.HeadSHA,
		TaskID:                  payload.TaskID,
		WorkflowID:              payload.WorkflowID,
		Reviewer:                reviewer,
		ReviewOptional:          !reviewRequired,
		ReviewBlockingSeverity:  e.reviewBlockingSeverity(payload.Repo),
		ExpectedTaskState:       expectedTaskState,
		HumanMergeRequested:     humanMergeRequested,
	})
	if err != nil {
		return MergeDecision{}, err
	}
	if decision.LeaveOpen {
		// A draft is an author-controlled hold, not a pending human merge decision.
		// Unknown draft state also fails toward NOT parking: parking requires a
		// classifier-gated override to escape, while leaving the task active is
		// recoverable when a later forge observation supplies the missing state.
		if payload.PullRequestDraft || payload.PullRequestDraftUnknown {
			return decision, nil
		}
		reason := decision.Reason.Render()
		if reason == "" {
			reason = "merge requires a human action"
		}
		return decision, e.parkTaskAwaitingHumanMerge(ctx, ref, reason)
	}
	if !decision.Ready {
		if decision.Deferred {
			// Park the task in ready_to_merge (NOT whatever state it arrived in) so
			// the daemon's lookupReadyPullRequestTask poll re-drives it every tick until
			// the hold settles. A task-owned retry already expected in ready_to_merge
			// needs no write; this also preserves a retained external-merge claim.
			if expectedTaskState == string(TaskReadyToMerge) {
				return decision, nil
			}
			return decision, e.setTaskState(ctx, ref, TaskReadyToMerge)
		}
		reason := decision.Reason.Render()
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
		err := e.blockMergeGate(ctx, ref, reason)
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
		}
		return decision, err
	}
	if decision.Merged {
		if err := e.setTaskState(ctx, ref, TaskMerged); err != nil {
			return decision, err
		}
		return decision, nil
	}
	return decision, e.setTaskState(ctx, ref, TaskReadyToMerge)
}

func (e Engine) parkTaskAwaitingHumanMerge(ctx context.Context, ref taskRef, reason string) error {
	if strings.TrimSpace(ref.ID) == "" {
		return e.setTaskState(ctx, ref, TaskAwaitingHumanMerge)
	}
	changed, current, err := e.Store.TransitionTaskStateWithEvent(ctx, ref.ID,
		[]string{
			string(TaskPullRequestOpen), string(TaskReviewing),
			string(TaskChangesRequested), string(TaskReadyToMerge),
		},
		string(TaskAwaitingHumanMerge), "task_awaiting_human_merge", reason)
	if err != nil {
		return err
	}
	if changed || current == string(TaskAwaitingHumanMerge) {
		return nil
	}
	// A concurrent lifecycle move won the CAS. Preserve it rather than rewriting
	// a merged, dismissed, or newly reviewed task from a stale gate result.
	return nil
}

// objectionBindsToCurrentHead answers whether a changes_requested verdict may
// transition the task (#1524).
//
// THE DEFECT: a verdict is evidence about a COMMIT, not about the branch. This
// arm transitioned the task unconditionally, so an objection bound to a
// superseded head pulled a PR out of ready_to_merge - and, because dispatchFix
// is called inline from it, dispatched a fix leg against findings about that
// superseded commit. #1834/#1871 bound the APPROVING side and left this one.
//
// ONLY A CONTRADICTED HEAD REFUSES. Both unknowns admit, and the asymmetry with
// the approval arm is deliberate - but the reason is LIVENESS, not merge safety.
// An earlier version of this comment claimed that refusing on a missing local
// row would let the merge gate merge on an approval "over a real current-head
// objection nobody recorded". That claim is FALSE, and #1903's independent
// review is what caught it:
//
//   - PolicyMergeGate blocks on the OBJECTION ROW itself, not on task state - but
//     only for the rows a given evaluation reaches, which is a POPULATION and
//     never a coverage absolute. Two later rounds of this PR killed two
//     successive attempts to state it as one, and a third killed the line numbers
//     for pointing at another tree and then at declarations rather than returns,
//     so this cites SYMBOLS and ordering.
//     On the STRICT path, Evaluate derives the head LIVE through
//     MergeGateGitHub.GetPullRequest, collects rows whose payload.HeadSHA equals
//     it, and returns mergeBlocked "review at evaluated head has blocking result"
//     for changes_requested, blocked or failed. When that population is EMPTY the
//     latest-round fallback selects the newest round and puts its
//     authorship-eligible rows through ensureReviewMatchesHead. So a CURRENT-head
//     objection is held at the gate; a STALE one is excluded while something
//     current survives, REFUSED by the fallback ("is for a different head SHA")
//     when it is in the selected round and nothing current exists, and not
//     reached at all when the selection passes over it. In every case refusing
//     the TASK transition does not un-record the review row, so refusing HERE
//     buys no merge protection. The three cases are pinned by name on
//     TestObjectionWithNoObservedPullRequestRowStillRequestsChanges; the headless
//     case is documented at TestObjectionWithNoHeadStillRequestsChanges.
//
// What refusing would actually cost is the CONSERVATIVE transition and, because
// dispatchFix is called inline from this arm, the FIX PASS - for an objection
// nobody can show is stale. Withholding both from an objection that is
// legitimately about the current head as far as any available evidence goes is a
// liveness loss, which is why the unknowns admit; the approval side refuses the
// mirror cases because withholding a merge fails safe.
//
// Admitting records a complaint and authorises nothing, so it is the
// claim-nothing direction. A CLI review dispatched without --head-sha produces
// the headless payload today, which is why that case is real traffic.
//
// ACCEPTED LIMITATION (#1512's family): when the ONLY objection on a PR is bound
// to a superseded head, this arm strands it. The task does not transition, no fix
// leg is dispatched, and nothing here re-drives anything - the PR waits for a
// review at the current head. That is deliberate: a fix pass carrying findings
// about a commit the branch has moved past is wrong work, not late work. It is
// also the reason the refusal is terminal rather than retried, and it is not
// mitigated in this change.
func (e Engine) objectionBindsToCurrentHead(ctx context.Context, payload JobPayload) (bool, string, error) {
	objectionHead := strings.TrimSpace(payload.HeadSHA)
	if objectionHead == "" || payload.PullRequest <= 0 {
		// Checked before any store read: an unbound objection claims nothing about
		// any commit, and a PR-less review is already terminal earlier in the
		// advance path.
		return true, "", nil
	}
	pr, err := e.Store.GetPullRequest(ctx, payload.Repo, int64(payload.PullRequest))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, "", nil
		}
		return false, "", err
	}
	currentHead := strings.TrimSpace(pr.HeadSHA)
	if currentHead == "" || currentHead == objectionHead {
		return true, "", nil
	}
	return false, fmt.Sprintf(
		"the objection is bound to head %s but the pull request's current head is %s; a verdict at a superseded head describes a commit the branch has moved past, so the task is not transitioned and no fix leg is dispatched",
		objectionHead, currentHead), nil
}
