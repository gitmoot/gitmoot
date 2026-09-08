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

// reviewDecisionAgent resolves the identity a review row's decision is credited
// to. It is the funnel for three of #2008's agent-keyed consumers -
// reviewLegsAtHead, followUpReviewScopes and the approval scan below - which is
// why the fallback lives here rather than at each of those call sites.
//
// The re-delegation case stays FIRST and stays ahead of the identity rule: a
// runtime_session_busy hand-off records the delegate in job.Agent, so resolving
// the stored fields first would still return that delegate and credit the leg to
// it rather than to the agent whose slot it fills.
func reviewDecisionAgent(job db.Job, payload JobPayload) string {
	if job.Type == "review" &&
		payload.DelegationReason == "runtime_session_busy" &&
		payload.DelegatedAgent == job.Agent &&
		strings.TrimSpace(payload.OriginalAgent) != "" {
		return payload.OriginalAgent
	}
	// An externally driven session review persists ActingOrgRole IN PLACE OF an
	// agent, so bare job.Agent returned "" and every caller's non-empty check then
	// dropped the row from the legs, scopes and approvals it belonged in.
	name, _ := ReviewerIdentity(job.Agent, payload.ActingOrgRole)
	return name
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

func (e Engine) dispatchFix(ctx context.Context, verdictJob db.Job, reviewer string, payload JobPayload, result AgentResult, ref taskRef) error {
	// ONE FIX LEG PER BRANCH AT A TIME (#1533). A two-family panel that both
	// object dispatches one leg PER VERDICT with no mutual exclusion. Each leg
	// starts from the head it was dispatched against, works for minutes, and
	// pushes; whichever finishes second is GUARANTEED a non-fast-forward
	// rejection, and that rejection is terminal - daemon_workflow.go maps a push
	// failure to blockedResultDelivery with no rebase, no retry and no salvage,
	// so a completed leg's work is discarded.
	//
	// Measured on the live store: 34 implement jobs carry "failed to push some
	// refs", 2026-08-27 through 2026-09-07. Of the 20 whose own window is under
	// 6h, 14 overlapped another short-window implement leg on the SAME branch.
	//
	// THE BRANCH LOCK CANNOT SERVE THIS PURPOSE and it is worth saying why, since
	// the obvious question is why a lock is not enough: Store.AcquireLock returns
	// true when the existing owner EQUALS the requester, so one lock admits N
	// concurrent same-owner legs by design. Every leg in the measured races ran
	// under the same lock.
	//
	// SKIP, NOT BLOCK, and not a queue. Blocking the task would need an operator
	// resume-work for a condition that resolves itself in minutes. Skipping is
	// safe because the sibling verdict's substance does not live in this dispatch:
	// its findings are in the #1822 ledger, still open, and the merge gate refuses
	// a verdict at a head that has not observed them, so the next round re-raises
	// them against the head the in-flight leg is about to push. A dropped dispatch
	// costs one round; a lost race costs a completed leg's work and leaves the
	// finding open anyway.
	if active, found, err := e.activeImplementLegOnBranch(ctx, payload); err != nil {
		return err
	} else if found {
		return e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID: verdictJob.ID,
			Kind:  "auto_fix_skipped_active_leg",
			Message: fmt.Sprintf(
				"auto-fix leg not dispatched for %s pull request #%d: implement job %s is already %s on branch %q; a second writer would lose the push race and its work would be discarded. The findings stay open in the ledger and are re-raised against the head that leg pushes",
				payload.Repo, payload.PullRequest, active.ID, active.State, strings.TrimSpace(payload.Branch)),
		})
	}
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

// activeImplementLegOnBranch reports a queued or running implement job that
// already owns this fix leg's write target.
//
// THE KEY IS THE BRANCH, because the branch is the resource the legs collide on:
// they race on `git push`, not on the task row. When the payload carries no
// branch there is nothing to push to and no branch to key on, so it falls back
// to the task, which is the same pairing the operator-side refusal uses
// (findActiveImplementJobForTask takes both).
//
// The engine could not simply call that refusal: it lives in internal/cli, and
// workflow must never import cli. The query is duplicated rather than the
// dependency inverted, for the same reason db.SucceededReviewVerdicts duplicates
// ResultIsFanOut, and it is a narrower query than the CLI's - implement jobs
// only, since a review or ask job on the branch does not push a fix leg's work.
func (e Engine) activeImplementLegOnBranch(ctx context.Context, payload JobPayload) (db.Job, bool, error) {
	if e.Store == nil {
		return db.Job{}, false, nil
	}
	repo := strings.TrimSpace(payload.Repo)
	branch := strings.TrimSpace(payload.Branch)
	taskID := strings.TrimSpace(payload.TaskID)
	if repo == "" || (branch == "" && taskID == "") {
		return db.Job{}, false, nil
	}
	active, err := e.Store.ListActiveJobs(ctx)
	if err != nil {
		return db.Job{}, false, fmt.Errorf("inspect active implement legs on %s branch %q: %w", repo, branch, err)
	}
	for _, job := range active {
		if job.Type != "implement" {
			continue
		}
		candidate, err := unmarshalPayload(job.Payload)
		if err != nil {
			// A payload this engine cannot read cannot be proven to target another
			// branch, and admitting an unreadable row is how a second writer gets
			// in. Treat it as an owner of the branch it claims to be on.
			return job, true, nil
		}
		if !strings.EqualFold(strings.TrimSpace(candidate.Repo), repo) {
			continue
		}
		if branch != "" {
			if strings.TrimSpace(candidate.Branch) == branch {
				return job, true, nil
			}
			continue
		}
		if strings.TrimSpace(candidate.TaskID) == taskID {
			return job, true, nil
		}
	}
	return db.Job{}, false, nil
}

// autoFixOwner names the agent that will RUN the auto-fix, so it must return
// something dispatchable.
//
// AN EXPLICIT ACTING ROLE THAT CANNOT BE RESOLVED IS A HARD STOP, AND THAT IS
// DELIBERATE, NOT A LIMITATION. My first attempt at #1718 made it fall through to
// attribution-based resolution, and
// TestEngineAdvanceReviewChangesRequestedDoesNotBypassUnresolvableActingRole
// caught it: falling back reassigns ownership the coordinator EXPLICITLY set, to
// the task implementer or a payload default it did not choose. The refusal is the
// feature.
//
// WHAT WAS ACTUALLY WRONG (#1718) IS THE DIAGNOSTIC, NOT THE BLOCK. Returning the
// role unchanged made it JobRequest.Agent, so the stop surfaced three layers later
// from an agent-subscription check as `agent "gitmoot" is not subscribed` - a
// sentence that is false in its own terms, since gitmoot is not an unsubscribed
// agent but an org ROLE, a different namespace entirely. An operator reading it
// looks for a subscription that was never the problem. The stop now happens here,
// where the cause is known, and says so.
func (e Engine) autoFixOwner(ctx context.Context, payload JobPayload) (string, error) {
	if role := strings.TrimSpace(payload.ActingOrgRole); role != "" {
		if _, err := e.Store.GetAgent(ctx, role); err == nil {
			return role, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		return "", fmt.Errorf(
			"auto-fix ownership unresolved: acting org role %q is not a registered agent, so it cannot own a fix; org roles and agents are separate namespaces. Assign an implementing agent to this branch or dispatch the fix explicitly - the engine will not reassign an ownership you set",
			role)
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
	roles := make([]string, 0, len(evidence.agents))
	for name, identity := range evidence.agents {
		// A ROLE IS ATTRIBUTABLE BUT NOT DISPATCHABLE. Keeping the two apart here is
		// the whole point: an in-session role can own the credit for the work and
		// still be an impossible executor for the fix.
		if identity.FromActingRole {
			roles = append(roles, name)
			continue
		}
		agents = append(agents, name)
	}
	sort.Strings(agents)
	sort.Strings(roles)
	switch len(agents) {
	case 0:
		if len(roles) > 0 {
			return "", fmt.Errorf(
				"auto-fix ownership unresolved: task %s was implemented in session by org role %s, which is not a dispatchable agent; name an implementing agent for the fix instead",
				payload.TaskID, strings.Join(roles, " "))
		}
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
	// The seed credits the reviewer whose job is being advanced right now, whose
	// own row the loop below has not read yet. #1351/#1417/#1557: seeding it
	// UNCONDITIONALLY let a fan-out coordinator satisfy its own required-reviewer
	// slot, which is the one boundary the loop's own ResultIsFanOut skip cannot
	// reach - the skip only stops OTHER stored fan-out rows. A coordinator that
	// announces a panel has approved nothing, including on its own behalf.
	if currentReviewer != "" && !ResultIsFanOut(payload.Result) {
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

// dispatchFixWhenHeadHasSettled dispatches AT MOST ONE fix leg per head, and
// only once every review dispatched at that head has settled (#1522).
//
// THE OLD RULE WAS "THE FIRST BLOCKING VERDICT DISPATCHES", which makes a
// cross-family pair unsatisfiable in one round whenever it splits: the first
// objection's leg pushes a new head while the sibling is still reading the old
// one, so the sibling's verdict is stale the moment it lands. Measured over the
// live job store: 37 of 334 dispatched fix legs (11.1%) were created while at
// least one sibling review at the SAME head was still running, and those
// siblings then landed 51 verdicts against a head that no longer existed - 29
// approvals, average 12.0 minutes late, worst 58 minutes, and 22 further
// objections at 9.2 minutes late.
//
// The 29 stale APPROVALS are the merge-integrity half: an "approved" recorded
// for a commit the fix leg superseded minutes later is a record asserting
// something it cannot support, which is the whole subject of #1520.
//
// THE LAST REVIEW TO SETTLE IS THE ONE THAT DISPATCHES, which is why this is
// reachable from the approving arm too. If only the objecting arm called it, a
// pair that splits objection-then-approval would defer the leg and then never
// dispatch it: the approval does not fix anything, and the objection's arm has
// already returned. That is a deadlock, not a delay, and it is the shape #1524
// was opened for.
//
// ONE LEG PER HEAD is also what makes the concurrency this cannot see impossible
// in the panel case: two blocking verdicts at one head now produce one leg
// rather than two racing writers (#1533). It does not replace #1533's guard,
// which still bounds legs arriving from separate rounds and other routes.
func (e Engine) dispatchFixWhenHeadHasSettled(ctx context.Context, job db.Job, payload JobPayload, ref taskRef) error {
	head := strings.TrimSpace(payload.HeadSHA)
	if head == "" {
		// No evaluated head means no head to reason about, so behave exactly as
		// before rather than inventing a reason never to dispatch. Withholding a
		// fix is a liveness cost and it must never be the accidental default.
		if payload.Result == nil {
			return nil
		}
		return e.dispatchFix(ctx, job, reviewDecisionAgent(job, payload), payload, *payload.Result, ref)
	}
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return err
	}
	blockingSeverity := e.reviewBlockingSeverity(payload.Repo)
	var pending []string
	var existingLeg string
	var verdictJob db.Job
	var verdictPayload JobPayload
	// The current job is included in the scan on equal terms with its siblings:
	// the objecting arm arrives here carrying its own blocking result, and the
	// approving arm arrives carrying none, so a single rule covers both.
	candidates := append([]db.Job{job}, jobs...)
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		if _, dup := seen[candidate.ID]; dup {
			continue
		}
		seen[candidate.ID] = struct{}{}
		candidatePayload, err := unmarshalPayload(candidate.Payload)
		if err != nil {
			return err
		}
		if !sameTask(payload, candidatePayload) {
			continue
		}
		if strings.TrimSpace(candidatePayload.HeadSHA) != head {
			continue
		}
		switch candidate.Type {
		case "review":
			if candidate.ID != job.ID &&
				(JobState(candidate.State) == JobQueued || JobState(candidate.State) == JobRunning) {
				pending = append(pending, candidate.ID+" ("+candidate.State+")")
				continue
			}
			if candidatePayload.Result == nil || ResultIsFanOut(candidatePayload.Result) {
				continue
			}
			if effectiveReviewDecisionForPayload(candidatePayload, blockingSeverity) != "changes_requested" {
				continue
			}
			// NEWEST OBJECTION WINS, ordered by the row's own updated_at with the id
			// as a deterministic tie-break. NOT by ListJobs order: engine job ids are
			// deterministic strings like "review-audit-task-9-review-2", so id order
			// is lexical and not chronological. I wrote that claim first and it was
			// wrong; the leg must carry the most recent statement of the objection at
			// this head, and two rows in the same second must still pick the same one
			// on every replay.
			if verdictPayload.Result != nil {
				if candidate.UpdatedAt < verdictJob.UpdatedAt {
					continue
				}
				if candidate.UpdatedAt == verdictJob.UpdatedAt && candidate.ID <= verdictJob.ID {
					continue
				}
			}
			verdictJob, verdictPayload = candidate, candidatePayload
		case "implement":
			if candidatePayload.FixWorktree {
				existingLeg = candidate.ID
			}
		}
	}
	if len(pending) > 0 {
		sort.Strings(pending)
		return e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID: job.ID,
			Kind:  "auto_fix_deferred_live_sibling",
			Message: fmt.Sprintf(
				"auto-fix leg deferred at head %s: sibling review(s) %s have not settled. Dispatching now would push a new head while they are still reading this one, so their verdicts would be stale on arrival; the last review to settle at this head dispatches instead",
				head, strings.Join(pending, ", ")),
		})
	}
	if existingLeg != "" {
		return e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID: job.ID,
			Kind:  "auto_fix_skipped_head_already_dispatched",
			Message: fmt.Sprintf(
				"auto-fix leg not dispatched at head %s: implement job %s already carries this head's fix pass",
				head, existingLeg),
		})
	}
	if verdictPayload.Result == nil {
		return nil
	}
	return e.dispatchFix(ctx, verdictJob, reviewDecisionAgent(verdictJob, verdictPayload), verdictPayload, *verdictPayload.Result, ref)
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
		FindingsAdvisory:        e.findingsAdvisory(payload.Repo),
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
			// A TASK CANNOT BE MERGEABLE AND UNDER REPAIR AT THE SAME TIME (#1555).
			//
			// Measured on PR #1546: a reviewer reproduced a host escape with a
			// compiled probe at 20:18:06, the engine had already dispatched the fix
			// leg for it at 20:18:04, and at 20:21:12 two later approvals drove the
			// task to ready_to_merge WHILE that leg was still running. task_events
			// held zero rows for the transition, so the only remaining permitted
			// operation was the merge, and a coordinator reading `gitmoot task list`
			// plus an approval count would have shipped the escape.
			//
			// The parking below is not wrong in general: ready_to_merge is what
			// lookupReadyPullRequestTask polls, so it is how a transient hold gets
			// re-driven. It is wrong for exactly one deferral, the one where a live
			// job owns the branch AND the task arrived carrying an objection. There
			// the state would assert mergeability over an unanswered blocking verdict
			// AND over the repair addressing it.
			//
			// Liveness is preserved without the poll in that case: the holding leg's
			// own completion advances the task, which is how every fix round already
			// reaches its next review. Nothing else re-drives from changes_requested,
			// and nothing needs to.
			if decision.HeldByJob != "" && expectedTaskState == string(TaskChangesRequested) {
				return decision, e.recordTaskEventBestEffort(ctx, ref, db.TaskEvent{
					Kind:      "merge_deferred_branch_held",
					FromState: expectedTaskState,
					ToState:   expectedTaskState,
					Reason: fmt.Sprintf(
						"not transitioned to ready_to_merge: job %s still owns branch %s while an objection stands at this head; a task cannot be mergeable and under repair at once. %s",
						decision.HeldByJob, strings.TrimSpace(payload.Branch), decision.Reason.Render()),
				})
			}
			// Park the task in ready_to_merge (NOT whatever state it arrived in) so
			// the daemon's lookupReadyPullRequestTask poll re-drives it every tick until
			// the hold settles. A task-owned retry already expected in ready_to_merge
			// needs no write; this also preserves a retained external-merge claim.
			if expectedTaskState == string(TaskReadyToMerge) {
				return decision, nil
			}
			// AUDITED, because it was not (#1555 ask 2). ready_to_merge is the
			// strongest merge signal the engine produces and it was written through a
			// plain upsert with no task_events row, so an operator could see the state
			// and never learn why. The write itself is unchanged; only the record is
			// added, and it names the deferral that caused it rather than implying a
			// clean approval.
			if err := e.setTaskState(ctx, ref, TaskReadyToMerge); err != nil {
				return decision, err
			}
			return decision, e.recordTaskEventBestEffort(ctx, ref, db.TaskEvent{
				Kind:      "task_ready_to_merge_deferred",
				FromState: expectedTaskState,
				ToState:   string(TaskReadyToMerge),
				Reason: fmt.Sprintf("parked in ready_to_merge to be re-driven by the merge poll: %s",
					decision.Reason.Render()),
			})
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
	// The READY arm, audited for the same reason as the deferred one (#1555 ask 2).
	// This is the transition that means "this really is mergeable", so it is the one
	// an operator most needs to be able to audit after the fact.
	if err := e.setTaskState(ctx, ref, TaskReadyToMerge); err != nil {
		return decision, err
	}
	reason := decision.Reason.Render()
	if reason == "" {
		reason = "merge gate reported ready"
	}
	return decision, e.recordTaskEventBestEffort(ctx, ref, db.TaskEvent{
		Kind:      "task_ready_to_merge",
		FromState: expectedTaskState,
		ToState:   string(TaskReadyToMerge),
		Reason:    reason,
	})
}

// recordTaskEventBestEffort records a task_events row for a transition the
// engine has already committed.
//
// BEST EFFORT IS DELIBERATE AND IT IS NOT LAXITY. The state write has already
// landed by the time this is called, so returning an error here would report a
// failure for an effect that succeeded, and a caller retrying on it would
// re-drive a transition that does not need re-driving. An audit row that fails
// to insert must not invalidate the thing it describes. A refused write is
// visible as a missing row, which is the same signal the absence of these rows
// was in the first place.
func (e Engine) recordTaskEventBestEffort(ctx context.Context, ref taskRef, event db.TaskEvent) error {
	if e.Store == nil || strings.TrimSpace(ref.ID) == "" {
		return nil
	}
	event.TaskID = ref.ID
	_ = e.Store.AddTaskEvent(ctx, event)
	return nil
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
// superseded commit.
//
// ONLY A CONTRADICTED HEAD REFUSES; both unknowns admit. What refusing would
// cost is the CONSERVATIVE transition and, inline from here, the FIX PASS - for
// an objection nobody can show is stale. That liveness cost is the whole reason
// the unknowns admit.
//
// A CLI review dispatched without --head-sha produces the headless payload
// today, which is why that case is real traffic. The arms are pinned in
// stale_verdict_head_test.go.
//
// ACCEPTED LIMITATION: when the ONLY objection on a PR is bound
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
