package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	defaultPollInterval = 30 * time.Second
	// externalMergeReconcileLookupLimit bounds targeted GitHub reads per repo poll.
	// A stale backlog drains over successive ticks without walking paginated closed
	// PR history or allowing old task rows to dominate the daemon's API budget.
	externalMergeReconcileLookupLimit = 20
	staleTaskReconcileLimit           = 20
	staleTaskReconcileScanLimit       = 200
	mergeGateUnclearedDescription     = "Gitmoot merge gate has not cleared this head"
	mergeGateNotAppliedDescription    = "Gitmoot merge gate is not applied to this head"
	mergeGateStatusMarker             = "marker"
	mergeGateStatusObserved           = "observed"
	mergeGateStatusInactive           = "inactive"
)

// issueCommentPollOverlap is subtracted from the persisted last-seen cursor when
// computing the `since` bound for the repo-wide issue-comment fetch (#566). It
// re-fetches a few seconds of boundary comments each tick to absorb local/GitHub
// clock skew and same-second comments straddling the cursor; the seen_comments
// dedup makes the replay a no-op.
const issueCommentPollOverlap = 5 * time.Second

type Daemon struct {
	Repo         github.Repository
	PollInterval time.Duration
	Store        *db.Store
	GitHub       github.Client
	Workflow     *workflow.Engine
	// WorkflowForJob optionally resolves the workflow engine used to advance a
	// completed job. Supervisors use it to bind job-associated subprocess seams
	// to the backend stored on that job; nil preserves the static Workflow.
	WorkflowForJob func(context.Context, db.Job) (*workflow.Engine, error)
	Sleep          func(context.Context, time.Duration) error
	// Now is an injectable clock (test seam). It defaults to time.Now and is used
	// to seed/advance the #566 issue-comment `since` cursor deterministically.
	Now func() time.Time
	// RemoteBranches is the fakeable one-call seam used by stale-task
	// reconciliation. Nil uses git ls-remote against the registered checkout.
	RemoteBranches RemoteBranchChecker
	// Logf receives one diagnostic for invalid config or an uncertain remote
	// check. Nil uses the process logger.
	Logf func(format string, args ...any)
	// WatchIssues opts in to the issue-comment workflow (#389): when true,
	// PollOnce also polls open non-PR issues and routes `@<agent> ask …`
	// comments to jobs. Default false keeps the PR-only behavior unchanged.
	WatchIssues bool
	// EscalationTTL bounds how long a tree may sit paused awaiting a human before
	// PollOnce auto-finalizes it gracefully (#340). 0 disables the scan, keeping
	// behavior unchanged for trees that never use escalate_human.
	EscalationTTL time.Duration
	// ObservePermissionPolicy opts this daemon into the live-store
	// #1484 ratchet. False is a cheap short-circuit with no inventory query.
	ObservePermissionPolicy bool
	// AutoMergeEnabled resolves the current native auto_merge policy for this
	// repository. When it turns on, only tasks parked by the auto-merge-disabled
	// leave-open reason are re-armed. Nil preserves direct daemon users' behavior.
	AutoMergeEnabled func(repo string) bool
	// AfterCanonicalTerminalEffects is an optional interruption/observability
	// hook invoked only after durable completion is recorded and before
	// secondary identities settle.
	AfterCanonicalTerminalEffects func(context.Context, db.PullRequestTerminalReconciliation) error
	// parseCommentCommand is an injectable test seam for proving that the
	// system-verified author gate runs before untrusted comment text reaches the
	// command parser. Nil uses ParseCommand.
	parseCommentCommand func(string) (Command, bool)
}

// RemoteBranchChecker returns the subset of exact branch names present on
// origin. Implementations must batch the full bounded input into one check.
type RemoteBranchChecker interface {
	RemoteBranches(ctx context.Context, checkout string, branches []string) (map[string]struct{}, error)
}

type gitRemoteBranchChecker struct{}

func (gitRemoteBranchChecker) RemoteBranches(ctx context.Context, checkout string, branches []string) (map[string]struct{}, error) {
	return (gitutil.NewHostClient(checkout)).RemoteBranches(ctx, branches)
}

func (d Daemon) Run(ctx context.Context) error {
	interval := d.PollInterval
	if interval == 0 {
		interval = defaultPollInterval
	}
	if interval < 0 {
		return fmt.Errorf("poll interval must be positive")
	}
	if err := d.validate(); err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Continuing past a poll error is deliberate and pinned by
		// TestRunContinuesAfterPollError: one bad tick must not stop the watcher.
		// REPORTING it is equally deliberate. A discarded error made this loop
		// indistinguishable from a healthy one, which is tolerable while every error is
		// transient and self-describing elsewhere -- and stops being tolerable the moment
		// a CALLER DEFECT can reach here. #1381 made the merge gate REFUSE a malformed
		// gate miss instead of panicking, exactly so an unattended daemon survives it; a
		// refusal nobody can observe converts that panic into silence, which is the worse
		// of the two. The task stays retryable and fails identically every interval with
		// nothing emitted anywhere. Log it and keep polling.
		if err := d.PollOnce(ctx); err != nil {
			d.logf("poll error: %v", err)
		}
		if err := d.sleep(ctx, interval); err != nil {
			return err
		}
	}
}

func (d Daemon) PollOnce(ctx context.Context) error {
	if err := d.validate(); err != nil {
		return err
	}
	var firstErr error
	if err := d.rearmAutoMergeDisabledTasks(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	// Disposal is independent of repository admission. Run it before the open-PR
	// listing so an unavailable forge cannot bypass an already-expired task's
	// terminal bound; the per-item evaluator records "subject unavailable".
	paths := config.Paths{ConfigFile: filepath.Join(filepath.Dir(d.Store.DatabasePath()), config.ConfigName)}
	if disposalTTL, err := config.LoadStaleTaskTTL(paths); err != nil {
		d.logf("task disposal skipped for %s: %v", d.Repo.FullName(), err)
	} else if err := d.reconcileTaskDisposals(ctx, disposalTTL); err != nil && firstErr == nil {
		firstErr = err
	}
	pulls, err := d.GitHub.ListPullRequests(ctx, d.Repo, "open")
	if err != nil {
		return err
	}
	openBranches := map[string]struct{}{}
	openPullNumbers := map[int64]struct{}{}
	// Fetch the repo's review-job list AT MOST ONCE for this whole poll and share the
	// snapshot across every open PR's review-job consumers, instead of re-running
	// ListJobsByType("review") up to ~2× per PR (#619). Lazy: computed on the first
	// consumer that needs it, never retained beyond this poll.
	reviewMemo := newReviewJobsMemo(d.Store)
	for _, pull := range pulls {
		// A fork head shares nothing with a local branch but its NAME. Every step
		// below either resolves, stores or advances a local task keyed on that
		// name, so all of them are fenced behind one identity check: routing a
		// fork pull request as a local task lets an outside contributor's PR park
		// or merge local work. Comment handling stays outside the fence, because a
		// maintainer commenting on a fork PR is legitimate and the command paths
		// resolve tasks through the guarded resolver, which refuses a fork head.
		local := d.pullRequestHeadIsLocal(pull)
		if local {
			openBranches[pull.HeadRef] = struct{}{}
		}
		openPullNumbers[pull.Number] = struct{}{}
		if local {
			changed, err := d.pullRequestChanged(ctx, pull, reviewMemo)
			if err != nil {
				return err
			}
			d.ensureMergeGateStatus(ctx, pull)
			mergeReadinessHandled := false
			if changed {
				if handled, err := d.handlePullRequestWorkflowChange(ctx, pull, reviewMemo); err != nil {
					if firstErr == nil {
						firstErr = err
					}
					changed = false
				} else {
					// The lifecycle already ran for this PR at this head in THIS poll, so
					// reconcileReviewingPullRequest must not re-enter it: reading the
					// poll's pre-dispatch snapshot it would see no review at the current
					// head and derive the same round a second time, whose instructions,
					// and therefore whose deterministic job id, can differ from the ones
					// just enqueued. Recording that one fact is O(1); dropping the whole
					// repo-wide snapshot instead cost a fresh ListJobsByType per changed
					// PR, defeating the once-per-poll memo it was added to preserve.
					reviewMemo.noteLifecycleRun(pull)
					mergeReadinessHandled = handled
					merged, err := d.pullRequestStoredMerged(ctx, pull)
					if err != nil {
						return err
					}
					if merged {
						changed = false
					}
				}
			}
			if changed {
				if err := d.recordPullRequest(ctx, pull); err != nil {
					return err
				}
			}
			// Change routing and merge eligibility are independent. A retained stale
			// review can keep routing "changed" after its job is terminal, while a
			// ready task still needs its gate re-evaluated on every poll (#1336).
			// HandlePullRequestOpened runs the gate itself when no reviewers are
			// configured; avoid evaluating it twice in that one composition.
			if !mergeReadinessHandled {
				task, err := d.lookupReadyPullRequestTask(ctx, pull, reviewMemo)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err == nil {
					if err := d.handleReadyToMergeWorkflow(ctx, pull, task); err != nil && firstErr == nil {
						firstErr = err
					}
				}
			}
		}
		comments, err := d.GitHub.ListIssueComments(ctx, d.Repo, pull.Number)
		if err != nil {
			return err
		}
		for _, comment := range comments {
			if err := d.handleComment(ctx, pull, comment); err != nil {
				return err
			}
		}
		if local {
			if err := d.reconcileReviewingPullRequest(ctx, pull, reviewMemo); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	// Permission-policy visibility is warn-only and best-effort. An observation
	// error contributes to firstErr but never aborts the remaining poll chain.
	if d.ObservePermissionPolicy {
		if err := d.reconcilePermissionPolicyObservation(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := d.reconcilePROpenTasks(ctx, pulls); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := d.reconcileExternallyMergedTasks(ctx, openPullNumbers); err != nil && firstErr == nil {
		firstErr = err
	}
	// Queued legs whose PR closed or merged underneath them (#1673). Placed AFTER
	// the open-PR listing on purpose: that listing is fail-closed (a forge error
	// returns before this point), so the open set is complete-or-absent and never a
	// silently truncated page this sweep could read as "everything closed".
	if err := d.supersedeQueuedJobsForClosedPullRequests(ctx, openPullNumbers); err != nil && firstErr == nil {
		firstErr = err
	}
	// Supersessions whose follow-up work did not finish (#1673). It runs right
	// after the sweep that creates that debt so a failure recorded this poll is
	// retried on the next one, and BEFORE the remaining reconcilers so a
	// coordinator released here is visible to them.
	if err := d.completePendingSupersedeFinalizations(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := d.reconcileTransientlyBlockedMergeGates(ctx, openPullNumbers); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := d.retryClosedReadyToMerge(ctx, openBranches); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := d.reconcileClosedReviewingTasks(ctx, openBranches); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := d.reconcileStaleTasks(ctx, openBranches); err != nil && firstErr == nil {
		firstErr = err
	}
	if d.WatchIssues {
		if err := d.PollIssuesOnce(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// escalate_human TTL scan (#340): auto-finalize trees that have sat paused
	// awaiting a human past the configured TTL. No-op when the engine is unset or
	// EscalationTTL is 0, so default behavior is unchanged.
	if d.Workflow != nil && d.EscalationTTL > 0 {
		if _, err := d.Workflow.AutoFinalizeExpiredEscalations(ctx, d.EscalationTTL); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (d Daemon) reconcileStaleTasks(ctx context.Context, openBranches map[string]struct{}) error {
	paths := config.Paths{ConfigFile: filepath.Join(filepath.Dir(d.Store.DatabasePath()), config.ConfigName)}
	plannedTTL, err := config.LoadPlannedTaskTTL(paths)
	if err != nil {
		d.logf("planned task reconciler skipped for %s: %v", d.Repo.FullName(), err)
	} else if plannedTTL > 0 {
		if err := d.reconcileTaskTTL(ctx, openBranches, taskTTLReconcilePolicy{
			ttl:          plannedTTL,
			states:       []string{string(workflow.TaskPlanned)},
			eventKind:    "task_dismissed_planned_ttl",
			reasonPrefix: "planned task auto-dismissed",
			logLabel:     "planned task reconciler",
		}); err != nil {
			return err
		}
	}

	staleTTL, err := config.LoadStaleTaskTTL(paths)
	if err != nil {
		d.logf("stale task reconciler skipped for %s: %v", d.Repo.FullName(), err)
		return nil
	}
	if staleTTL == 0 {
		return nil
	}
	return d.reconcileTaskTTL(ctx, openBranches, taskTTLReconcilePolicy{
		ttl:          staleTTL,
		states:       []string{string(workflow.TaskImplementing)},
		eventKind:    "task_dismissed_auto",
		reasonPrefix: "stale task auto-dismissed",
		logLabel:     "stale task reconciler",
	})
}

type taskTTLReconcilePolicy struct {
	ttl          time.Duration
	states       []string
	eventKind    string
	reasonPrefix string
	logLabel     string
}

func (d Daemon) reconcileTaskTTL(ctx context.Context, openBranches map[string]struct{}, policy taskTTLReconcilePolicy) error {
	candidates, err := d.Store.ListStaleTaskCandidates(ctx, d.Repo.FullName(), policy.states,
		d.now().Add(-policy.ttl), staleTaskReconcileScanLimit)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}

	type readyCandidate struct {
		candidate db.StaleTaskCandidate
		task      db.Task
	}
	emptyBranch := []readyCandidate{}
	remoteCandidates := []readyCandidate{}
	branches := []string{}
	for _, candidate := range candidates {
		if len(emptyBranch)+len(remoteCandidates) >= staleTaskReconcileLimit {
			break
		}
		task := db.Task{ID: candidate.ID, RepoFullName: candidate.RepoFullName, State: candidate.State, Branch: candidate.Branch}
		if _, live, err := workflow.FindLiveTaskJob(ctx, d.Store, task); err != nil {
			return err
		} else if live {
			continue
		}
		branch := strings.TrimSpace(candidate.Branch)
		if branch == "" {
			emptyBranch = append(emptyBranch, readyCandidate{candidate: candidate, task: task})
			continue
		}
		if _, open := openBranches[branch]; open {
			continue
		}
		remoteCandidates = append(remoteCandidates, readyCandidate{candidate: candidate, task: task})
		branches = append(branches, branch)
	}

	remotePresent := map[string]struct{}{}
	remoteCertain := true
	if len(branches) > 0 {
		repo, err := d.Store.GetRepo(ctx, d.Repo.FullName())
		if err != nil {
			remoteCertain = false
			d.logf("%s remote check skipped for %s: %v", policy.logLabel, d.Repo.FullName(), err)
		} else {
			checker := d.RemoteBranches
			if checker == nil {
				checker = gitRemoteBranchChecker{}
			}
			remotePresent, err = checker.RemoteBranches(ctx, repo.CheckoutPath, branches)
			if err != nil {
				remoteCertain = false
				d.logf("%s remote check skipped for %s: %v", policy.logLabel, d.Repo.FullName(), err)
			}
		}
	}

	// A failed remote check makes the tick non-authoritative. Avoid even the
	// otherwise-certain empty-branch writes so one poll never partially applies
	// its bounded candidate batch.
	if !remoteCertain {
		return nil
	}
	dismissed := 0
	for _, item := range emptyBranch {
		reason := fmt.Sprintf("%s: empty branch; ttl=%s; updated_at=%s", policy.reasonPrefix, policy.ttl, item.candidate.UpdatedAt)
		changed, _, err := d.Store.TransitionTaskStateWithEventIfNoActiveJob(ctx, item.task.ID,
			policy.states, string(workflow.TaskDismissed), policy.eventKind, reason)
		if err != nil {
			if errors.Is(err, db.ErrTaskHasActiveJob) {
				continue
			}
			return err
		}
		if changed {
			dismissed++
		}
	}
	for _, item := range remoteCandidates {
		branch := strings.TrimSpace(item.candidate.Branch)
		if _, present := remotePresent[branch]; present {
			continue
		}
		reason := fmt.Sprintf("%s: remote ref refs/heads/%s absent; ttl=%s; updated_at=%s", policy.reasonPrefix, branch, policy.ttl, item.candidate.UpdatedAt)
		changed, _, err := d.Store.TransitionTaskStateWithEventIfNoActiveJob(ctx, item.task.ID,
			policy.states, string(workflow.TaskDismissed), policy.eventKind, reason)
		if err != nil {
			if errors.Is(err, db.ErrTaskHasActiveJob) {
				continue
			}
			return err
		}
		if changed {
			dismissed++
		}
	}
	d.logf("%s for %s: candidates=%d checked=%d dismissed=%d",
		policy.logLabel, d.Repo.FullName(), len(candidates), len(emptyBranch)+len(remoteCandidates), dismissed)
	return nil
}

func (d Daemon) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// pullRequestHeadIsLocal reports whether a pull request's head branch lives in
// the watched repository. HeadRef text can collide with a local branch name
// without being that branch, so every consumer that resolves a task, stores a
// mirror row, or writes a status from HeadRef must reject a fork head through
// this one predicate. The comparison is case-INSENSITIVE because GitHub repo
// names are, and rejecting a legitimate same-repo pull request over letter case
// would silently disable routing for ordinary work.
func (d Daemon) pullRequestHeadIsLocal(pull github.PullRequest) bool {
	headRepo := strings.TrimSpace(pull.HeadRepoFullName)
	return headRepo == "" || strings.EqualFold(headRepo, d.Repo.FullName())
}

// reconcilePROpenTasks promotes implementing/blocked tasks whose branch carries
// an open same-repo pull request back to pr_open (#920). It is a catch-up for
// missed or mis-sequenced PR-open events: without it a wedged task hides its PR
// from every "needs you" surface, which filters on pr_open. The promotion mirrors
// what HandlePullRequestOpened would have recorded and cannot trigger a merge —
// the merge gate acts only on ready_to_merge tasks. Fork heads are skipped:
// HeadRef text can collide with a local branch name without being that branch.
func (d Daemon) reconcilePROpenTasks(ctx context.Context, pulls []github.PullRequest) error {
	if len(pulls) == 0 {
		return nil
	}
	tasks, err := d.Store.ListTasksByRepo(ctx, d.Repo.FullName())
	if err != nil {
		return err
	}
	byBranch := map[string][]db.Task{}
	for _, task := range tasks {
		if task.State != string(workflow.TaskImplementing) && task.State != string(workflow.TaskBlocked) {
			continue
		}
		branch := strings.TrimSpace(task.Branch)
		if branch == "" {
			continue
		}
		byBranch[branch] = append(byBranch[branch], task)
	}
	var firstErr error
	for _, pull := range pulls {
		if !d.pullRequestHeadIsLocal(pull) {
			continue
		}
		for _, task := range byBranch[strings.TrimSpace(pull.HeadRef)] {
			changed, _, err := d.Store.TransitionTaskStateWithEvent(ctx, task.ID,
				[]string{string(workflow.TaskImplementing), string(workflow.TaskBlocked)},
				string(workflow.TaskPullRequestOpen), "task_pr_open_auto",
				fmt.Sprintf("open PR #%d found for branch %s", pull.Number, pull.HeadRef))
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if changed {
				d.logf("task %s promoted %s -> pr_open: open PR #%d on %s", task.ID, task.State, pull.Number, pull.HeadRef)
			}
		}
		// This is deliberately attempted on every open-PR observation, including
		// the replay after task promotion. The structured receipt makes it
		// at-most-once and lets a transient journal failure heal next tick.
		if _, err := workflow.RecordPullRequestWorkflowTransition(ctx, d.Store, workflow.PullRequestEvent{
			Repo:        d.Repo.FullName(),
			Branch:      pull.HeadRef,
			PullRequest: int(pull.Number),
		}, workflow.PullRequestJournalOpened); err != nil {
			d.logf("workflow journal PR-open breadcrumb failed for %s#%d: %v", d.Repo.FullName(), pull.Number, err)
		}
	}
	return firstErr
}

// reconcileTransientlyBlockedMergeGates gives a TRANSIENTLY blocked task an exit
// (#1562). The merge gate already separates an operational/branch-staleness/infra
// condition from an authoritative quality rejection, but that classification used
// to live only on the returned decision, so nothing outside the blocking call
// stack could act on it: daemon.pullRequestReadyToMerge admits ready_to_merge
// only, engine_pr_lifecycle's non-merged close branch omits blocked, and
// `task resume-work` refuses blocked. Measured on review-pr-1699-3f3a1026, whose
// dirty-worktree block outlived the dirt by 95 minutes and cleared only when a
// human merged the PR by hand.
//
// Release requires BOTH halves. The persisted class is the SELECTION filter — the
// gate's own statement that this condition is self-clearing — and returning the
// row to ready_to_merge is what restores it to the population that is
// re-evaluated, which is the same remedy the engine already applies to a deferred
// decision for the identical wedge ("a state that poll does not re-evaluate",
// engine_routing_merge.go). The gate is NOT re-run here: it runs on the
// ready_to_merge path, so a condition that has not actually cleared blocks again
// and the row simply returns here next tick. A quality-classed row is never
// selected, so an authoritative rejection stays blocked by construction rather
// than by a re-evaluation happening to agree.
//
// PR-number resolution mirrors reconcileExternallyMergedTasks exactly, because
// the wedged population is precisely the branchless local review task: a
// pull.HeadRef lookup cannot find it, which is why the branch-keyed poll predicate
// is the wrong seam for this exit.
const transientMergeGateRetryInterval = time.Minute

func (d Daemon) reconcileTransientlyBlockedMergeGates(ctx context.Context, openPullNumbers map[int64]struct{}) error {
	tasks, err := d.Store.ListTasksByRepo(ctx, d.Repo.FullName())
	if err != nil {
		return err
	}
	var firstErr error
	for _, task := range tasks {
		if task.State != string(workflow.TaskBlocked) {
			continue
		}
		branch := strings.TrimSpace(task.Branch)
		var number int64
		if branch == "" {
			var ok bool
			number, ok = reviewTaskPullRequestNumber(task.ID)
			if !ok {
				continue
			}
		} else {
			stored, storedErr := d.Store.GetPullRequestByRepoBranch(ctx, d.Repo.FullName(), branch)
			if storedErr != nil {
				if errors.Is(storedErr, sql.ErrNoRows) {
					continue
				}
				return storedErr
			}
			number = stored.Number
		}
		if number <= 0 {
			continue
		}
		// Only a still-open PR can clear a transient condition. A closed or merged
		// PR belongs to the external-merge and closed-reviewing reconcilers.
		if _, open := openPullNumbers[number]; !open {
			continue
		}
		gate, gateErr := d.Store.GetMergeGate(ctx, d.Repo.FullName(), number)
		if gateErr != nil {
			if errors.Is(gateErr, sql.ErrNoRows) {
				continue
			}
			return gateErr
		}
		if gate.State != "blocked" || gate.BlockClass != int(workflow.MergeBlockTransient) {
			continue
		}
		// The latest blocking event owns the current state. Retry its unchanged
		// transient condition indefinitely, but no more than once per interval.
		// The release transition preserves tasks.updated_at, so retries cannot
		// disguise the original blocked age.
		attributed, unattributed, lastRetryAt, attrErr := d.mergeGateOwnsCurrentBlock(ctx, task.ID, gate.Reason)
		if attrErr != nil {
			if firstErr == nil {
				firstErr = attrErr
			}
			continue
		}
		if !attributed && !unattributed {
			continue
		}
		now := d.now()
		if !lastRetryAt.IsZero() && now.Before(lastRetryAt.Add(transientMergeGateRetryInterval)) {
			continue
		}
		changed, _, transitionErr := d.Store.TransitionTaskStateWithEventPreserveAgeAt(ctx, task.ID,
			[]string{string(workflow.TaskBlocked)},
			string(workflow.TaskReadyToMerge), "merge_gate_transient_retry",
			fmt.Sprintf("re-evaluating transient merge-gate block on open PR #%d: %s", number, gate.Reason), now)
		if transitionErr != nil {
			if firstErr == nil {
				firstErr = transitionErr
			}
			continue
		}
		if changed {
			d.logf("task %s released blocked -> ready_to_merge: transient merge-gate block on open PR #%d (%s)", task.ID, number, gate.Reason)
		}
	}
	return firstErr
}

// mergeGateOwnsCurrentBlock reports whether the merge gate owns the task's
// current block, whether the task predates block attribution, and the most
// recent retry in the unchanged condition episode. Another blocker supersedes
// the merge gate; a changed reason permits an immediate retry.
func (d Daemon) mergeGateOwnsCurrentBlock(ctx context.Context, taskID string, currentGateReason string) (attributed bool, unattributed bool, lastRetryAt time.Time, err error) {
	events, err := d.Store.ListTaskEvents(ctx, taskID)
	if err != nil {
		return false, false, time.Time{}, err
	}
	currentOwner := -1
	for i := len(events) - 1; i >= 0; i-- {
		if taskEventIsBlocking(events[i]) {
			currentOwner = i
			break
		}
	}
	if currentOwner < 0 {
		for _, event := range events {
			if event.Kind != "merge_gate_transient_retry" {
				continue
			}
			parsed, parseErr := parseTaskStoreTime(event.CreatedAt)
			if parseErr != nil {
				return false, false, time.Time{}, fmt.Errorf("parse transient retry time for task %s: %w", taskID, parseErr)
			}
			if parsed.After(lastRetryAt) {
				lastRetryAt = parsed
			}
		}
		return false, true, lastRetryAt, nil
	}
	if events[currentOwner].Kind != "merge_gate_blocked" {
		return false, false, time.Time{}, nil
	}
	if strings.TrimSpace(events[currentOwner].Reason) != strings.TrimSpace(currentGateReason) {
		return true, false, time.Time{}, nil
	}

	episodeActive := false
	episodeReason := ""
	for _, event := range events {
		if taskEventIsBlocking(event) {
			if event.Kind != "merge_gate_blocked" {
				episodeActive = false
				episodeReason = ""
				lastRetryAt = time.Time{}
				continue
			}
			if !episodeActive || event.Reason != episodeReason {
				episodeActive = true
				episodeReason = event.Reason
				lastRetryAt = time.Time{}
			}
			continue
		}
		if event.Kind != "merge_gate_transient_retry" || !episodeActive {
			continue
		}
		parsed, parseErr := parseTaskStoreTime(event.CreatedAt)
		if parseErr != nil {
			return false, false, time.Time{}, fmt.Errorf("parse transient retry time for task %s: %w", taskID, parseErr)
		}
		if parsed.After(lastRetryAt) {
			lastRetryAt = parsed
		}
	}
	return true, false, lastRetryAt, nil
}

func taskEventIsBlocking(event db.TaskEvent) bool {
	if event.ToState == string(workflow.TaskBlocked) {
		return true
	}
	switch event.Kind {
	case "merge_gate_blocked", "review_auto_fix_blocked", "pr_closed_unmerged",
		"delegation_vote_unmet", "delegation_quorum_unmet", "delegation_child_failed",
		"block_parent", "quorum_unmet":
		return true
	default:
		return false
	}
}

// reconcileExternallyMergedTasks advances PR lifecycle tasks, plus blocked
// tasks, whose PR was merged outside Gitmoot. It uses targeted single-PR reads
// rather than a closed list, so old wedged tasks are not hidden by GitHub
// pagination. Empty-branch local review tasks are keyed by their durable
// review-pr-<number>-<hash> id. Closed-unmerged responses are deliberately
// ignored here; the existing reviewing/ready-to-merge paths retain their current
// semantics.
func (d Daemon) reconcileExternallyMergedTasks(ctx context.Context, openPullNumbers map[int64]struct{}) error {
	if d.Workflow == nil {
		return nil
	}
	tasks, err := d.Store.ListTasksByRepo(ctx, d.Repo.FullName())
	if err != nil {
		return err
	}
	type candidateGroup struct {
		number int64
		tasks  []db.Task
	}
	groups := make([]candidateGroup, 0)
	groupByNumber := make(map[int64]int)
	for _, task := range tasks {
		if !externalMergeCandidateState(task.State) {
			continue
		}
		branch := strings.TrimSpace(task.Branch)
		var number int64
		if branch == "" {
			var ok bool
			number, ok = reviewTaskPullRequestNumber(task.ID)
			if !ok {
				continue
			}
		} else {
			stored, err := d.Store.GetPullRequestByRepoBranch(ctx, d.Repo.FullName(), branch)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return err
			}
			number = stored.Number
		}
		if number <= 0 {
			continue
		}
		if _, open := openPullNumbers[number]; open {
			continue
		}
		if index, ok := groupByNumber[number]; ok {
			groups[index].tasks = append(groups[index].tasks, task)
			continue
		}
		groupByNumber[number] = len(groups)
		groups = append(groups, candidateGroup{number: number, tasks: []db.Task{task}})
	}

	var firstErr error
	if len(groups) > externalMergeReconcileLookupLimit {
		groups = groups[:externalMergeReconcileLookupLimit]
	}
	for _, group := range groups {
		pull, err := d.GitHub.GetPullRequest(ctx, d.Repo, group.number)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// A fork pull request can carry the same HeadRef as a local task's branch,
		// and a MERGED fork PR must never advance that task. The stored mirror row
		// is keyed on branch text alone, so identity is re-checked here against
		// the freshly fetched pull request.
		if !d.pullRequestHeadIsLocal(pull) {
			continue
		}
		if !pullRequestListedAsMerged(pull) {
			// The PR left the open set without merging. Closed branch-bearing
			// lifecycle tasks remain owned by their dedicated reconciliation
			// passes. A branchless ready task has no path through those branch
			// indexes, so resolve its retained merge claim here before applying
			// the terminal non-merge transition.
			if strings.EqualFold(strings.TrimSpace(pull.State), "closed") {
				if _, err := workflow.RecordPullRequestWorkflowTransition(ctx, d.Store, workflow.PullRequestEvent{
					Repo:        d.Repo.FullName(),
					Branch:      strings.TrimSpace(pull.HeadRef),
					PullRequest: int(group.number),
				}, workflow.PullRequestJournalClosed); err != nil {
					d.logf("workflow journal PR-closed breadcrumb failed for %s#%d: %v", d.Repo.FullName(), group.number, err)
				}
				for _, task := range group.tasks {
					if task.State != string(workflow.TaskReadyToMerge) || strings.TrimSpace(task.Branch) != "" {
						continue
					}
					event, eventErr := d.reconciledPullRequestEvent(ctx, pull, task, group.number)
					if eventErr != nil {
						if firstErr == nil {
							firstErr = eventErr
						}
						continue
					}
					if _, _, recoverErr := d.Store.RecoverClaimedTaskState(ctx, task.ID,
						string(workflow.TaskBlocked), "pr_closed_unmerged",
						fmt.Sprintf("pull request #%d closed without merging while holding a retained merge claim", group.number)); recoverErr != nil {
						if firstErr == nil {
							firstErr = recoverErr
						}
						continue
					}
					if closeErr := d.Workflow.HandleReviewPullRequestClosed(ctx, event, false); closeErr != nil && firstErr == nil {
						firstErr = closeErr
					}
				}
			}
			continue
		}
		canonicalTask, selectErr := d.selectCanonicalMergedTask(ctx, group.tasks)
		if selectErr != nil {
			if firstErr == nil {
				firstErr = selectErr
			}
			continue
		}
		taskIDs := make([]string, 0, len(group.tasks))
		for _, task := range group.tasks {
			taskIDs = append(taskIDs, task.ID)
		}
		reconciliation, reconcileErr := d.Store.BeginPullRequestTerminalReconciliation(ctx, db.PullRequestTerminalReconciliation{
			RepoFullName: d.Repo.FullName(),
			PullRequest:  group.number,
			HeadSHA:      strings.TrimSpace(pull.HeadSHA),
			OwnerTaskID:  canonicalTask.ID,
		}, taskIDs)
		if reconcileErr != nil {
			if firstErr == nil {
				firstErr = reconcileErr
			}
			continue
		}
		if reconciliation.OwnerTaskID != canonicalTask.ID {
			if reconciliation.EffectsCompleted {
				canonicalTask = db.Task{ID: reconciliation.OwnerTaskID}
			} else {
				canonicalTask, reconcileErr = d.Store.GetTask(ctx, reconciliation.OwnerTaskID)
				if reconcileErr != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("load canonical terminal-effects owner %s: %w", reconciliation.OwnerTaskID, reconcileErr)
					}
					continue
				}
			}
		}
		if !reconciliation.EffectsCompleted {
			event, eventErr := d.reconciledPullRequestEvent(ctx, pull, canonicalTask, group.number)
			if eventErr != nil {
				if firstErr == nil {
					firstErr = eventErr
				}
				continue
			}
			if canonicalTask.State == string(workflow.TaskReadyToMerge) && d.Workflow.MergeGate != nil {
				// Exactly one ready identity owns the per-PR boundary: atomic claim
				// recovery, merge-gate finalization, branch/worktree cleanup,
				// harvesting and detached signals, and continuation.
				if readyErr := d.handleReadyToMergeWorkflow(ctx, pull, canonicalTask); readyErr != nil {
					if firstErr == nil {
						firstErr = readyErr
					}
					continue
				}
				canonicalTask, reconcileErr = d.Store.GetTask(ctx, canonicalTask.ID)
				if reconcileErr != nil {
					if firstErr == nil {
						firstErr = reconcileErr
					}
					continue
				}
				if canonicalTask.State != string(workflow.TaskMerged) {
					// The wrapper's authoritative re-read did not confirm the
					// daemon's merged hint. Retain owner/debt and retry; never
					// apply terminal effects or settle another identity.
					continue
				}
			} else if canonicalTask.State == string(workflow.TaskReadyToMerge) && strings.TrimSpace(canonicalTask.Branch) == "" {
				// Defensive fallback for a gate removed between polls. There is no
				// configured canonical continuation to run, but the retained claim
				// must still resolve into the authoritative merged state.
				if _, _, recoverErr := d.Store.RecoverClaimedTaskState(ctx, canonicalTask.ID,
					string(workflow.TaskMerged), "pull_request_merged",
					fmt.Sprintf("recovered merged pull request #%d from retained branchless task claim", group.number)); recoverErr != nil {
					if firstErr == nil {
						firstErr = recoverErr
					}
					continue
				}
			}
			if closeErr := d.Workflow.HandleReviewPullRequestClosed(ctx, event, true); closeErr != nil {
				if firstErr == nil {
					firstErr = closeErr
				}
				continue
			}
			if completeErr := d.Store.CompletePullRequestTerminalEffects(ctx, reconciliation); completeErr != nil {
				if firstErr == nil {
					firstErr = completeErr
				}
				continue
			}
			reconciliation.EffectsCompleted = true
			if d.AfterCanonicalTerminalEffects != nil {
				if interruptErr := d.AfterCanonicalTerminalEffects(ctx, reconciliation); interruptErr != nil {
					if firstErr == nil {
						firstErr = interruptErr
					}
					continue
				}
			}
		}
		settlements, settlementErr := d.Store.ListPullRequestTerminalSettlements(ctx, reconciliation)
		if settlementErr != nil {
			if firstErr == nil {
				firstErr = settlementErr
			}
			continue
		}
		for _, taskID := range settlements {
			task, taskErr := d.Store.GetTask(ctx, taskID)
			if errors.Is(taskErr, sql.ErrNoRows) {
				taskErr = d.Store.ResolvePullRequestTerminalSettlement(ctx, reconciliation, taskID)
			}
			if taskErr != nil {
				if firstErr == nil {
					firstErr = taskErr
				}
				continue
			}
			if task.ID == "" {
				continue
			}
			if taskErr := d.reconcileAdditionalMergedTask(ctx, task, reconciliation.OwnerTaskID, group.number); taskErr != nil {
				if firstErr == nil {
					firstErr = taskErr
				}
				continue
			}
			if resolveErr := d.Store.ResolvePullRequestTerminalSettlement(ctx, reconciliation, task.ID); resolveErr != nil && firstErr == nil {
				firstErr = resolveErr
			}
		}
	}
	return firstErr
}

// selectCanonicalMergedTask chooses one identity to own per-PR terminal effects.
// A ready task with a durable merge claim wins, followed by a branch-owning
// ready task, then another ready task. Groups without a ready identity preserve
// the branch-owning lifecycle task as the canonical cleanup owner.
func (d Daemon) selectCanonicalMergedTask(ctx context.Context, tasks []db.Task) (db.Task, error) {
	claimedReady := -1
	branchReady := -1
	anyReady := -1
	branchFallback := -1
	anyFallback := -1
	for index := range tasks {
		task := tasks[index]
		if anyFallback < 0 || task.ID < tasks[anyFallback].ID {
			anyFallback = index
		}
		if strings.TrimSpace(task.Branch) != "" &&
			(branchFallback < 0 || task.ID < tasks[branchFallback].ID) {
			branchFallback = index
		}
		if task.State != string(workflow.TaskReadyToMerge) {
			continue
		}
		if anyReady < 0 || task.ID < tasks[anyReady].ID {
			anyReady = index
		}
		if strings.TrimSpace(task.Branch) != "" &&
			(branchReady < 0 || task.ID < tasks[branchReady].ID) {
			branchReady = index
		}
		claimed, err := d.Store.HasTaskStateClaim(ctx, task.ID)
		if err != nil {
			return db.Task{}, fmt.Errorf("inspect merge claim for task %s: %w", task.ID, err)
		}
		if claimed && (claimedReady < 0 || task.ID < tasks[claimedReady].ID) {
			claimedReady = index
		}
	}
	for _, index := range []int{claimedReady, branchReady, anyReady, branchFallback, anyFallback} {
		if index >= 0 {
			return tasks[index], nil
		}
	}
	return db.Task{}, errors.New("select canonical merged task from empty PR group")
}

// reconcileAdditionalMergedTask advances an additional local identity without
// replaying per-PR cleanup, harvesting, detached legs, or continuation.
func (d Daemon) reconcileAdditionalMergedTask(ctx context.Context, task db.Task, canonicalTaskID string, number int64) error {
	reason := fmt.Sprintf("reconciled merged pull request #%d; canonical terminal effects owned by task %s", number, canonicalTaskID)
	changed, current, err := d.Store.RecoverClaimedTaskState(ctx, task.ID,
		string(workflow.TaskMerged), "pull_request_merged", reason)
	if err != nil {
		return err
	}
	if changed || current == string(workflow.TaskMerged) {
		return nil
	}
	changed, current, err = d.Store.TransitionTaskStateWithEvent(ctx, task.ID, []string{
		string(workflow.TaskPullRequestOpen),
		string(workflow.TaskReviewing),
		string(workflow.TaskChangesRequested),
		string(workflow.TaskReadyToMerge),
		string(workflow.TaskAwaitingHumanMerge),
		string(workflow.TaskBlocked),
	}, string(workflow.TaskMerged), "pull_request_merged", reason)
	if err != nil {
		return err
	}
	if !changed && current != string(workflow.TaskMerged) {
		return fmt.Errorf("%w: additional task %s reached %q while reconciling merged PR #%d",
			db.ErrTaskStateConflict, task.ID, current, number)
	}
	return nil
}

func (d Daemon) reconciledPullRequestEvent(ctx context.Context, pull github.PullRequest, task db.Task, number int64) (workflow.PullRequestEvent, error) {
	branch := strings.TrimSpace(pull.HeadRef)
	if branch == "" {
		branch = strings.TrimSpace(task.Branch)
	}
	if branch == "" {
		return workflow.PullRequestEvent{}, fmt.Errorf("reconcile terminal PR #%d: head branch is empty", number)
	}
	leadAgent := "github"
	if lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), branch); err == nil {
		if owner := strings.TrimSpace(lock.Owner); owner != "" {
			leadAgent = owner
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return workflow.PullRequestEvent{}, err
	}
	return workflow.PullRequestEvent{
		Repo:        d.Repo.FullName(),
		Branch:      branch,
		PullRequest: int(number),
		HeadSHA:     pull.HeadSHA,
		GoalID:      task.GoalID,
		TaskID:      task.ID,
		TaskTitle:   task.Title,
		LeadAgent:   leadAgent,
		Sender:      "github",
	}, nil
}

func externalMergeCandidateState(state string) bool {
	switch workflow.TaskState(strings.TrimSpace(state)) {
	case workflow.TaskPullRequestOpen, workflow.TaskReviewing, workflow.TaskChangesRequested, workflow.TaskReadyToMerge, workflow.TaskAwaitingHumanMerge, workflow.TaskBlocked:
		return true
	default:
		return false
	}
}

func reviewTaskPullRequestNumber(taskID string) (int64, bool) {
	const prefix = "review-pr-"
	remainder, ok := strings.CutPrefix(strings.TrimSpace(taskID), prefix)
	if !ok {
		return 0, false
	}
	numberText, suffix, ok := strings.Cut(remainder, "-")
	if !ok || strings.TrimSpace(suffix) == "" {
		return 0, false
	}
	number, err := strconv.ParseInt(numberText, 10, 64)
	return number, err == nil && number > 0
}

// PollIssuesOnce is the opt-in issue-comment workflow (#389, bounded by #566). It
// lists open non-PR issues (PRs are filtered out by github.ListIssues so the
// PR-watcher is not duplicated) and routes `@<agent> ask …` comments to jobs via
// the shared comment->job->reply core, reusing the seen_comments dedup, the
// authorize-commenter gate, and PostIssueComment exactly like the PR path; an
// `ask` needs no branch/HeadSHA, so those are left empty.
//
// #566 collapses the former O(open-issues) per-issue ListIssueComments fan-out
// (one full paginated gh call per open issue every tick) into ONE repo-wide
// ListRepoIssueComments(repo, since) call per tick: it fetches only comments
// updated since the persisted cursor, groups them back by issue number, and feeds
// each open non-PR issue's comments through the UNCHANGED handleIssueComment path.
// The repo-wide endpoint also returns PR conversation comments; those are owned by
// PollOnce's per-PR loop and are skipped here (their number is not in the open
// non-PR issue set). NEW-issue enumeration still uses ListIssues, so nothing that
// depends on issue listing changes — only the comment pagination is collapsed.
//
// FIRST-POLL SEMANTICS (intentional #566 difference): with no persisted cursor the
// `since` window is seeded from `now` (minus the skew overlap), so a fresh watcher
// does NOT backfill the entire repo's comment history. The prior code paginated
// every open issue's full comment thread on the first tick (acting only on unseen
// comments); the new code instead ignores comments older than daemon start. In
// steady state both behave identically because every processed comment is recorded
// in seen_comments and the cursor advances monotonically.
func (d Daemon) PollIssuesOnce(ctx context.Context) error {
	if err := d.validate(); err != nil {
		return err
	}
	issues, err := d.GitHub.ListIssues(ctx, d.Repo, "open")
	if err != nil {
		return err
	}
	// Index open non-PR issues by number so repo-wide comments can be routed back
	// to their issue (handleIssueComment needs the issue's title). PR comments and
	// comments on issues not in this set are skipped.
	openIssues := make(map[int64]github.Issue, len(issues))
	for _, issue := range issues {
		if issue.IsPullRequest {
			continue
		}
		openIssues[issue.Number] = issue
	}

	// Resolve the `since` bound. base is the prior cursor, or `now` on the first
	// ever poll (bounded initial fetch — no history backfill). since re-fetches a
	// small overlap window for clock skew; seen_comments dedups the replay.
	base, hasCursor, err := d.Store.GetIssueCommentPollCursor(ctx, d.Repo.FullName())
	if err != nil {
		return err
	}
	if !hasCursor {
		base = d.now()
	}
	since := base.Add(-issueCommentPollOverlap)

	comments, err := d.GitHub.ListRepoIssueComments(ctx, d.Repo, since)
	if err != nil {
		return err
	}

	var firstErr error
	newCursor := base // monotonic: never regress below the prior cursor / seed
	for _, comment := range comments {
		if t, ok := parseCommentUpdatedAt(comment.UpdatedAt); ok && t.After(newCursor) {
			newCursor = t
		}
		issue, ok := openIssues[comment.IssueNumber]
		if !ok {
			// PR comment (owned by the PR loop) or a comment on a closed/unknown
			// issue: not routed here.
			continue
		}
		if err := d.handleIssueComment(ctx, issue, comment); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Persist the advanced cursor so the next tick's `since` moves forward. Done
	// even on an error above so a single bad comment never re-scans the backlog.
	if err := d.Store.UpsertIssueCommentPollCursor(ctx, d.Repo.FullName(), newCursor); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// parseCommentUpdatedAt parses a GitHub comment updated_at (RFC3339) timestamp.
// It returns ok=false for an empty/unparseable value so the cursor simply does
// not advance on that comment rather than regressing.
func parseCommentUpdatedAt(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// now returns the daemon's clock, defaulting to time.Now when unset.
func (d Daemon) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Daemon) PollRecoveryCommandsOnce(ctx context.Context) error {
	if err := d.validate(); err != nil {
		return err
	}
	pulls, err := d.GitHub.ListPullRequests(ctx, d.Repo, "open")
	if err != nil {
		return err
	}
	for _, pull := range pulls {
		comments, err := d.GitHub.ListIssueComments(ctx, d.Repo, pull.Number)
		if err != nil {
			return err
		}
		for _, comment := range comments {
			if err := d.handleRecoveryComment(ctx, pull, comment); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d Daemon) validate() error {
	if d.Store == nil {
		return errors.New("daemon store is required")
	}
	if d.GitHub == nil {
		return errors.New("daemon github client is required")
	}
	if d.Repo.FullName() == "" {
		return errors.New("daemon repo is required")
	}
	return nil
}

// reviewJobLister is the narrow store dependency reviewJobsMemo needs (satisfied by
// *db.Store). It exists purely so a counting fake can pin the once-per-poll fetch
// property in tests; production always threads the real *db.Store.
type reviewJobLister interface {
	ListJobsByType(ctx context.Context, jobType string) ([]db.Job, error)
}

// reviewJobsMemo fetches the repo's review-job list AT MOST ONCE per PollOnce and
// shares that snapshot across the poll's review-job consumers
// (pullRequestWorkflowRouting, supersedeStaleReviewJobs, reconcileReviewingPullRequest).
// Those consumers previously each ran ListJobsByType("review") per open PR — up to
// ~2× per PR — re-decoding every review payload each time (#619). The list is a
// point-in-time snapshot for the duration of ONE poll: the same staleness class as
// the old per-call fetches, which likewise observed only whatever review rows existed
// at their moment of call. Like the per-tick candidate memo it caches SUCCESS only — a
// failed fetch is returned and left unset so a later consumer re-fetches (in practice
// a fetch error aborts the whole poll). Lazy: a poll that reaches no consumer fetches
// nothing. Consumed only on the synchronous poll goroutine, so it needs no locking.
type reviewJobsMemo struct {
	store reviewJobLister
	done  bool
	jobs  []db.Job
	// lifecycleRuns records the head each PR's workflow lifecycle already ran at in
	// THIS poll. It is not part of the snapshot: it is the one fact that makes the
	// snapshot's staleness harmless, without re-fetching it.
	lifecycleRuns map[int64]string
}

func (m *reviewJobsMemo) get(ctx context.Context) ([]db.Job, error) {
	if m.done {
		return m.jobs, nil
	}
	jobs, err := m.store.ListJobsByType(ctx, "review")
	if err != nil {
		return nil, err
	}
	m.jobs = jobs
	m.done = true
	return m.jobs, nil
}

// noteLifecycleRun records that this PR's workflow lifecycle ran at this head in the
// current poll. The dispatch it may have performed is invisible to the snapshot taken
// before it, so a later consumer must not treat "no review at this head" as an
// instruction to derive the round again.
func (m *reviewJobsMemo) noteLifecycleRun(pull github.PullRequest) {
	if m.lifecycleRuns == nil {
		m.lifecycleRuns = map[int64]string{}
	}
	m.lifecycleRuns[pull.Number] = strings.TrimSpace(pull.HeadSHA)
}

// lifecycleRanAtHead reports whether noteLifecycleRun already recorded this PR at this
// exact head during this poll.
func (m *reviewJobsMemo) lifecycleRanAtHead(pull github.PullRequest) bool {
	if m == nil || len(m.lifecycleRuns) == 0 {
		return false
	}
	head, ok := m.lifecycleRuns[pull.Number]
	return ok && head == strings.TrimSpace(pull.HeadSHA)
}

// newReviewJobsMemo is a package var (not a plain func) only so the once-per-poll
// regression test can substitute a memo backed by a counting store; production never
// reassigns it.
var newReviewJobsMemo = func(store reviewJobLister) *reviewJobsMemo {
	return &reviewJobsMemo{store: store}
}

// reviewJobs returns the poll's shared review-job snapshot via memo when one is
// threaded (the PollOnce path), and otherwise fetches fresh from the store. The nil
// case covers standalone/test calls to a single consumer, which fetch exactly as they
// did before the per-poll memo (#619).
func (d Daemon) reviewJobs(ctx context.Context, memo *reviewJobsMemo) ([]db.Job, error) {
	if memo != nil {
		return memo.get(ctx)
	}
	return d.Store.ListJobsByType(ctx, "review")
}

func (d Daemon) pullRequestChanged(ctx context.Context, pull github.PullRequest, memo *reviewJobsMemo) (bool, error) {
	previous, err := d.Store.GetPullRequest(ctx, d.Repo.FullName(), pull.Number)
	switch {
	case err == nil:
		if previous.HeadSHA != pull.HeadSHA {
			return true, nil
		}
		routing, err := d.pullRequestWorkflowRouting(ctx, pull, memo)
		if err != nil {
			return false, err
		}
		return routing.stale, nil
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	default:
		return false, err
	}
}

type pullRequestRouting struct {
	stale bool
}

func (d Daemon) pullRequestWorkflowRouting(ctx context.Context, pull github.PullRequest, memo *reviewJobsMemo) (pullRequestRouting, error) {
	// Only review jobs are inspected below; ListJobsByType filters in SQL so this
	// poll path stops materializing every non-review job's payload (#619). The list is
	// shared across the poll's review-job consumers via memo (fetched once per poll).
	jobs, err := d.reviewJobs(ctx, memo)
	if err != nil {
		return pullRequestRouting{}, err
	}
	routing := pullRequestRouting{}
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		var payload workflow.JobPayload
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return pullRequestRouting{}, fmt.Errorf("parse job payload %q: %w", job.ID, err)
		}
		if workflowReviewJobMatchesPull(d.Repo.FullName(), pull, payload) {
			if strings.TrimSpace(payload.HeadSHA) == pull.HeadSHA {
				return pullRequestRouting{}, nil
			}
			routing.stale = true
		}
	}
	return routing, nil
}

func (d Daemon) recordPullRequest(ctx context.Context, pull github.PullRequest) error {
	return d.Store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: d.Repo.FullName(),
		Number:       pull.Number,
		URL:          pull.URL,
		HeadBranch:   pull.HeadRef,
		BaseBranch:   pull.BaseRef,
		HeadSHA:      pull.HeadSHA,
		State:        pull.State,
	})
}

// ensureMergeGateStatus reconciles the visible status for one managed PR head.
// Its exact-head observation is independent of both pull-request routing and the
// merge-gate decision, so a bookkeeping failure retries without changing either.
func (d Daemon) ensureMergeGateStatus(ctx context.Context, pull github.PullRequest) {
	if d.Workflow == nil || d.Workflow.MergeGate == nil {
		return
	}
	// A fork head must never resolve a local task by branch text, or an unrelated
	// contributor's pull request inherits a marker no Gitmoot workflow resolves.
	if !d.pullRequestHeadIsLocal(pull) {
		return
	}
	headSHA := strings.TrimSpace(pull.HeadSHA)
	if headSHA == "" {
		return
	}
	task, err := d.lookupPolledPullRequestTask(ctx, pull)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			d.logf("merge-gate marker task lookup failed for %s#%d: %v", d.Repo.FullName(), pull.Number, err)
		}
		return
	}

	observation, observationErr := d.Store.GetMergeGateStatusObservation(ctx, d.Repo.FullName(), pull.Number)
	if observationErr != nil && !errors.Is(observationErr, sql.ErrNoRows) {
		d.logf("merge-gate status observation lookup failed for %s#%d: %v", d.Repo.FullName(), pull.Number, observationErr)
	}
	// An operator who handed the merge decision away (either kill-switch) gets no
	// marker: a status describing a gate that will never run is a permanent
	// pending nothing in Gitmoot can resolve.
	gateOwnsDecision := !workflow.NativeMergeGateDisabled() &&
		(d.AutoMergeEnabled == nil || d.AutoMergeEnabled(d.Repo.FullName()))
	applies := gateOwnsDecision && mergeGateMarkerApplies(task.State)
	if observationErr == nil && observation.HeadSHA == headSHA {
		if applies && (observation.Kind == mergeGateStatusMarker || observation.Kind == mergeGateStatusObserved) {
			return
		}
		if !applies && observation.Kind == mergeGateStatusInactive {
			return
		}
	}

	combined, err := d.GitHub.GetCombinedStatus(ctx, d.Repo, headSHA)
	if err != nil {
		d.logf("merge-gate marker status lookup failed for %s#%d at %s: %v", d.Repo.FullName(), pull.Number, headSHA, err)
		return
	}
	status, found := latestMergeGateStatus(combined.Statuses)
	if applies {
		if found && !(status.State == "success" && status.Description == mergeGateNotAppliedDescription) {
			kind := mergeGateStatusObserved
			if status.State == "pending" && status.Description == mergeGateUnclearedDescription {
				kind = mergeGateStatusMarker
			}
			d.recordMergeGateStatusObservation(ctx, pull.Number, headSHA, kind)
			return
		}
		if _, err := d.GitHub.CreateCommitStatus(ctx, github.CommitStatusInput{
			Repo:        d.Repo,
			SHA:         headSHA,
			State:       "pending",
			Context:     workflow.GitmootMergeGateContext,
			Description: mergeGateUnclearedDescription,
		}); err != nil {
			d.logf("merge-gate marker write failed for %s#%d at %s: %v", d.Repo.FullName(), pull.Number, headSHA, err)
			return
		}
		d.recordMergeGateStatusObservation(ctx, pull.Number, headSHA, mergeGateStatusMarker)
		return
	}

	if found && status.State == "pending" && status.Description == mergeGateUnclearedDescription {
		if _, err := d.GitHub.CreateCommitStatus(ctx, github.CommitStatusInput{
			Repo:        d.Repo,
			SHA:         headSHA,
			State:       "success",
			Context:     workflow.GitmootMergeGateContext,
			Description: mergeGateNotAppliedDescription,
		}); err != nil {
			d.logf("merge-gate marker clearance failed for %s#%d at %s: %v", d.Repo.FullName(), pull.Number, headSHA, err)
			return
		}
	}
	d.recordMergeGateStatusObservation(ctx, pull.Number, headSHA, mergeGateStatusInactive)
}

func mergeGateMarkerApplies(state string) bool {
	switch workflow.TaskState(strings.TrimSpace(state)) {
	case workflow.TaskPullRequestOpen,
		workflow.TaskReviewing,
		workflow.TaskChangesRequested,
		workflow.TaskReadyToMerge,
		workflow.TaskBlocked,
		workflow.TaskAwaitingHuman:
		return true
	default:
		return false
	}
}

func latestMergeGateStatus(statuses []github.CommitStatus) (github.CommitStatus, bool) {
	for i := len(statuses) - 1; i >= 0; i-- {
		if strings.TrimSpace(statuses[i].Context) == workflow.GitmootMergeGateContext {
			return statuses[i], true
		}
	}
	return github.CommitStatus{}, false
}

func (d Daemon) recordMergeGateStatusObservation(ctx context.Context, pullRequest int64, headSHA string, kind string) {
	if err := d.Store.UpsertMergeGateStatusObservation(ctx, db.MergeGateStatusObservation{
		RepoFullName: d.Repo.FullName(),
		PullRequest:  pullRequest,
		HeadSHA:      headSHA,
		Kind:         kind,
	}); err != nil {
		d.logf("merge-gate status observation write failed for %s#%d at %s: %v", d.Repo.FullName(), pullRequest, headSHA, err)
	}
}

func (d Daemon) pullRequestStoredMerged(ctx context.Context, pull github.PullRequest) (bool, error) {
	stored, err := d.Store.GetPullRequest(ctx, d.Repo.FullName(), pull.Number)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(stored.State) == "merged", nil
}

func (d Daemon) handlePullRequestWorkflow(ctx context.Context, pull github.PullRequest, memo *reviewJobsMemo) error {
	_, err := d.handlePullRequestWorkflowChange(ctx, pull, memo)
	return err
}

// handlePullRequestWorkflowChange reports whether HandlePullRequestOpened
// evaluated merge readiness itself. That happens only for the configured
// no-reviewer path; PollOnce uses the signal to avoid an immediate duplicate
// evaluation while independently checking every other ready task.
func (d Daemon) handlePullRequestWorkflowChange(ctx context.Context, pull github.PullRequest, memo *reviewJobsMemo) (bool, error) {
	if d.Workflow == nil {
		return false, nil
	}
	if err := d.supersedeStaleReviewJobs(ctx, pull, memo); err != nil {
		return false, err
	}
	lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), pull.HeadRef)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	ref := workflowTaskRef{
		id:     pull.HeadRef,
		title:  pull.Title,
		branch: pull.HeadRef,
	}
	if task, err := d.lookupPolledPullRequestTask(ctx, pull); err == nil {
		ref.id = task.ID
		ref.goalID = task.GoalID
		ref.title = task.Title
		if task.Branch != "" {
			ref.branch = task.Branch
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	reviewers, err := d.workflowReviewers(ctx)
	if err != nil {
		return false, err
	}
	mergeReadinessHandled := len(reviewers) == 0 && !lock.SkipNativeReviewFanout
	err = d.Workflow.HandlePullRequestOpened(ctx, workflow.PullRequestEvent{
		Repo:                    d.Repo.FullName(),
		Branch:                  ref.branch,
		PullRequest:             int(pull.Number),
		PullRequestDraft:        pull.Draft,
		PullRequestDraftUnknown: pull.DraftUnknown,
		HeadSHA:                 pull.HeadSHA,
		GoalID:                  ref.goalID,
		TaskID:                  ref.id,
		TaskTitle:               ref.title,
		LeadAgent:               lock.Owner,
		Sender:                  "github",
		RequiredReviewers:       reviewers,
		// Trigger 2 (daemon path): the implement-job advancement persisted the
		// skip flag onto the branch lock; honor it so the PR-watcher path skips
		// the native review fanout too.
		SkipReviewFanout: lock.SkipNativeReviewFanout,
		// #1250 reader 2 of 2: the SAME lock row already fetched above also carries
		// the acting org role, so this trigger and the in-process one attribute
		// fanout children identically with zero extra queries. Empty is the legacy
		// and undirected value; the fanout then behaves exactly as it does today.
		ActingOrgRole: lock.ActingOrgRole,
	})
	return mergeReadinessHandled, err
}

func (d Daemon) supersedeStaleReviewJobs(ctx context.Context, pull github.PullRequest, memo *reviewJobsMemo) error {
	// Only review jobs can be superseded here; ListJobsByType filters in SQL so this
	// poll path stops materializing every non-review job's payload (#619). The list is
	// shared across the poll's review-job consumers via memo (fetched once per poll).
	jobs, err := d.reviewJobs(ctx, memo)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := workflowPayload(job)
		if err != nil {
			return err
		}
		if !reviewJobTargetsPull(d.Repo.FullName(), pull, payload) {
			continue
		}
		if strings.TrimSpace(payload.HeadSHA) == pull.HeadSHA {
			continue
		}
		reason := fmt.Sprintf("review job superseded_stale_head: PR #%d moved from head %q to %q", pull.Number, strings.TrimSpace(payload.HeadSHA), pull.HeadSHA)
		if _, _, err := workflow.SupersedeStaleHeadJob(ctx, d.Store, job.ID, reason); err != nil {
			return err
		}
	}
	return nil
}

// supersedeQueuedJobsForClosedPullRequests terminates queued jobs bound to a pull
// request that is no longer open (#1673). A merged or closed PR cannot be
// implemented or reviewed, so those legs are not waiting for capacity — they wait
// for a condition that has become impossible, and they wait forever: nothing else
// in the poll selects them (supersedeStaleReviewJobs only fires per OPEN pull, and
// the task reconcilers write task state, never job state). The cost is quiet: the
// queued count gains a floor it never drops below, so a real backlog hides behind a
// constant and any "is the queue empty" readiness check can never pass again.
//
// The selection is deliberately narrow, because the failure mode of a sweep that is
// too wide is silently discarding work somebody asked for:
//   - the job's payload repo must be THIS daemon's repo. PR numbers are not
//     repo-qualified, so PR #42 exists in every repo and payload.Repo is the only
//     binding.
//   - payload.PullRequest must be > 0. jobs.pull_request was never backfilled, so
//     pre-#843 rows read 0; 0 means "no PR recorded", never "PR zero".
//   - four PR-bound classes are exempt and enumerated in queuedJobSurvivesClosedPullRequest.
func (d Daemon) supersedeQueuedJobsForClosedPullRequests(ctx context.Context, openPullNumbers map[int64]struct{}) error {
	// Repo-scoped in SQL. ListQueuedJobs is HOME-WIDE and projects neither repo nor
	// pull_request, so selecting candidates from it meant decoding every payload in
	// the home — and one undecodable row in ANY repo then failed EVERY watched repo's
	// poll, permanently, because that condition never clears. This query never reads a
	// foreign row, and keeps the literal 'queued' predicate so the partial index
	// idx_jobs_queued_created still applies.
	jobs, err := d.Store.ListQueuedJobsForRepo(ctx, d.Repo.FullName())
	if err != nil {
		return err
	}
	// One forge answer per number per poll. Without it, N queued jobs bound to the
	// same unrecorded number cost N identical GetPullRequest calls in a single poll,
	// and an issue-bound job — which can never be terminated — repeats that cost on
	// every poll forever.
	evidence := map[int]forgeClosureAnswer{}
	var firstErr error
	for _, job := range jobs {
		payload, err := workflowPayload(job)
		if err != nil {
			// This repo's own row, and it cannot be parsed. Skip it — an unparseable
			// payload is not evidence the work is pointless — but do NOT fail the poll:
			// the condition is permanent, so returning it here would wedge every later
			// reconciler in the chain on every tick. The log line is the trace.
			d.logf("queued-job sweep: skipping %s: %v", job.ID, err)
			continue
		}
		// The PAYLOAD is authoritative for the number, not the projection.
		// jobs.pull_request was never backfilled, so a pre-#843 row carries 0 there
		// while its payload names a real PR — reading the projection alone left exactly
		// those legacy rows invisible, which is the population this sweep exists for.
		// The projection's job is done: it kept the SQL from ever reading another
		// repo's row.
		number := payload.PullRequest
		if number <= 0 {
			continue
		}
		// Case-insensitive, because forge repository identity is. A row projected
		// `Gitmoot/Gitmoot` against a daemon registered as `gitmoot/gitmoot` would
		// otherwise never be selected by any poll, forever.
		if !strings.EqualFold(strings.TrimSpace(payload.Repo), d.Repo.FullName()) {
			continue
		}
		if _, open := openPullNumbers[int64(number)]; open {
			continue
		}
		// Cheapest-first, and the order is load-bearing rather than cosmetic: the
		// exemption check reads job events from the local store, while the
		// pull-request-evidence check can reach the forge. Testing evidence first
		// would spend an API call on a job this sweep would never touch anyway.
		survives, err := d.queuedJobSurvivesClosedPullRequest(ctx, job, payload)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if survives {
			continue
		}
		// The open-PR listing is only a prefilter: it is a snapshot from the top of the
		// poll and it cannot separate a closed PR from an ISSUE number. Ask the forge
		// for the authoritative answer — is this a pull request, and is it not open
		// right now — before terminating anything.
		closed, closedErr := d.pullRequestIsClosedOnTheForge(ctx, number, evidence)
		if closedErr != nil {
			if firstErr == nil {
				firstErr = closedErr
			}
			continue
		}
		if unresolved := evidence[number].unresolved; unresolved != nil {
			// Recorded on the JOB, once, keyed on the event kind so repeated polls do not
			// append: an issue-bound job is a permanent condition, so this must be a fact a
			// reader can find rather than an error the dashboard shows forever.
			d.recordPullRequestUnresolvedOnce(ctx, job.ID, number, unresolved)
			continue
		}
		if !closed {
			continue
		}
		reason := fmt.Sprintf("queued %s job superseded: %s pull request #%d is no longer open",
			job.Type, payload.Repo, number)
		releasesCoordinator, err := d.queuedChildCanReleaseCoordinator(ctx, payload)
		if err != nil {
			// A store error here is not evidence about the coordinator. Skipping keeps a
			// transient failure from silently downgrading a child with a LIVE parent to
			// the cancel path, which would strand that coordinator for good.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if releasesCoordinator {
			engine, err := d.workflowForJob(ctx, job)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			// A delegation child must also release its coordinator; see
			// FinalizeClosedPullRequestDelegationChild for why its terminal state
			// differs from the top-level path. The OBSERVED row is passed, not its
			// id: the verdict is about the run this poll listed, and the settlement
			// is anchored on that row's lifecycle generation so a job that
			// completed and re-queued in the meantime is left alone.
			if _, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, job, reason); err != nil {
				// The parent's failure_policy decides what a dead child means, and it
				// RECORDS that decision: block_parent surfaces as BlockedError,
				// escalate_human as AwaitingHumanError. Both are the DAG acting, not this
				// sweep failing, and treating either as a poll error would stamp the
				// repo's last_error and (first-wins) mask a genuine error from a later
				// reconciler. Same treatment reconcileReviewingPullRequest gives an
				// AdvanceJob block.
				var blocked workflow.BlockedError
				var awaiting workflow.AwaitingHumanError
				if !errors.As(err, &blocked) && !errors.As(err, &awaiting) && firstErr == nil {
					firstErr = err
				}
			}
			continue
		}
		if _, _, err := workflow.SupersedeClosedPullRequestJob(ctx, d.Store, job, reason); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// completePendingSupersedeFinalizations pays supersession debt that a previous
// poll recorded and did not finish (#1673).
//
// The terminal state write moves a superseded job out of `queued`, and
// supersedeQueuedJobsForClosedPullRequests selects only queued jobs — so before
// this pass, a child whose cleanup, synthetic result or parent advance failed
// after the transition was UNREACHABLE by any later sweep and its coordinator
// waited forever. The pending marker written inside that transition is what makes
// the window recoverable, and this is the pass that closes it.
//
// Bounded by the store's marker query, so a poll with no outstanding debt costs
// one indexed read. A BlockedError/AwaitingHumanError is the parent's
// failure_policy acting — the same classification the creating sweep uses.
func (d Daemon) completePendingSupersedeFinalizations(ctx context.Context) error {
	ids, err := d.Store.JobIDsWithPendingSupersedeFinalization(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, id := range ids {
		job, err := d.Store.GetJob(ctx, id)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// The PAYLOAD names the repo. GetJob does not project the denormalized
		// jobs.repo column, so comparing job.Repo here would compare against an
		// always-empty string: a filter that can never match, silently skipping
		// every candidate and reporting a clean poll.
		payload, perr := workflowPayload(job)
		if perr != nil {
			d.logf("supersede-finalization recovery: skipping %s: %v", id, perr)
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(payload.Repo), d.Repo.FullName()) {
			// Another watched repo's row. Its own daemon owns it, and the marker
			// query is home-wide.
			continue
		}
		engine, err := d.workflowForJob(ctx, job)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if _, err := engine.CompletePendingSupersedeFinalization(ctx, id); err != nil {
			var blocked workflow.BlockedError
			var awaiting workflow.AwaitingHumanError
			if !errors.As(err, &blocked) && !errors.As(err, &awaiting) && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// pullRequestIsClosedOnTheForge reports whether this number names a pull request in
// this repo that is NOT open, memoized per number for the caller's poll.
//
// It answers the whole question in one place, because splitting it was wrong twice
// over. Absence from the poll's open-PR listing is a cheap PREFILTER and nothing more:
// it cannot tell a closed PR from an ISSUE number (payload.PullRequest carries those
// too, and delegationRequest copies them onto children), and it is a SNAPSHOT taken at
// the top of the poll, so a PR reopened before the sweep runs still looks absent. A
// recorded pull_requests row proves the number is a PR but says nothing current about
// its state.
//
// So the forge decides, and it must say BOTH things: this number is a pull request,
// and it is not open right now. Any error — including the 404 an issue number produces
// — is NO EVIDENCE and leaves the job queued, because a transient forge failure must
// never read as licence to terminate somebody's work. The memo caches negatives too:
// an issue-bound job can never be terminated, so without that every job sharing the
// number re-asks on every poll, forever.
func (d Daemon) pullRequestIsClosedOnTheForge(ctx context.Context, number int, memo map[int]forgeClosureAnswer) (bool, error) {
	if memo != nil {
		if answer, seen := memo[number]; seen {
			return answer.closed, answer.err
		}
	}
	answer := forgeClosureAnswer{}
	pull, err := d.GitHub.GetPullRequest(ctx, d.Repo, int64(number))
	switch {
	case err == nil:
		answer.closed = pull.Number == int64(number) && !strings.EqualFold(strings.TrimSpace(pull.State), "open")
	case github.AsTransient(err) || github.IsTransientMessage(err.Error()):
		// NO EVIDENCE, AND SAID OUT LOUD. Leaving the job queued was never the question;
		// observability was. A swallowed forge error makes a sweep that does nothing on
		// every poll look exactly like a sweep with nothing to do (#1673). The caller
		// records it and continues, so one unreachable number never blocks the rest of
		// the poll, and it CLEARS once the outage does.
		answer.err = fmt.Errorf("revalidate pull request #%d before superseding queued work: %w", number, err)
	default:
		// DEFINITIVE, SO NOT AN ERROR TO REPORT FOREVER. A 404 is the normal answer for a
		// number that is not a pull request in this repo, and payload.PullRequest carries
		// issue numbers - delegationRequest copies them onto children. Reporting it would
		// stamp repos.last_error on EVERY poll for the life of the job and, because
		// firstErr is first-wins and this sweep runs before the finalization sweep and the
		// reconcilers, mask a genuine error from every later stage. That is the same
		// argument this file already makes about BlockedError a few lines up, applied to
		// the forge read instead of to the DAG's decisions.
		//
		// It is NOT silence: the fact is recorded once per job, keyed on the event so
		// repeated polls do not append. And it is still fail-closed - a number that cannot
		// be proven to name a non-open pull request never terminates anything.
		answer.closed = false
		answer.unresolved = err
	}
	if memo != nil {
		// The ERROR is memoized alongside the answer, which is what keeps the cost bound
		// at one ask per number per poll: an issue-bound job produces a failure every
		// time it is asked, so re-asking per job would repeat it once per job forever.
		memo[number] = answer
	}
	return answer.closed, answer.err
}

// pullRequestUnresolvedEvent records that the forge answered definitively that a job's
// pull_request number does not name a pull request in this repo - an issue number, a
// deleted PR, a transferred repo. It is the observability half of NOT reporting that as
// a poll error.
const pullRequestUnresolvedEvent = "pull_request_unresolved"

// recordPullRequestUnresolvedOnce appends pullRequestUnresolvedEvent unless this job
// already carries one.
//
// ONCE is the requirement, not a nicety: the condition is permanent, so an append per
// poll would grow job_events without bound at the daemon's tick rate - the same failure
// the advance_retry collapse migration had to repair. Best-effort by design: this is a
// note for a reader, and failing the poll over it would reintroduce exactly the
// permanent red this path exists to avoid.
func (d Daemon) recordPullRequestUnresolvedOnce(ctx context.Context, jobID string, number int, cause error) {
	events, err := d.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return
	}
	for _, event := range events {
		if event.Kind == pullRequestUnresolvedEvent {
			return
		}
	}
	_ = d.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    pullRequestUnresolvedEvent,
		Message: fmt.Sprintf("#%d does not name a pull request in %s, so this job can never be superseded by a closed pull request: %v", number, d.Repo.FullName(), cause),
	})
}

// forgeClosureAnswer is one number's memoized revalidation outcome for a single poll.
// closed is only meaningful when both errors are nil.
//
// The two error fields are DIFFERENT FACTS and the distinction is the whole point: err
// means "could not read the forge" and is reported so a broken poll cannot look like a
// quiet one, while unresolved means "the forge answered, and this number is not a pull
// request here" - a permanent, expected condition that is recorded once rather than
// reported on every tick forever.
type forgeClosureAnswer struct {
	closed     bool
	err        error
	unresolved error
}

// workflowForJob resolves the engine to advance THIS job with. Every other daemon
// path that reaches AdvanceJob does this (reconcileReviewingPullRequest), because
// WorkflowForJob binds the exec backend recorded on the job; using the repo-default
// engine instead would advance a job on the wrong runner.
func (d Daemon) workflowForJob(ctx context.Context, job db.Job) (*workflow.Engine, error) {
	if d.WorkflowForJob == nil {
		return d.Workflow, nil
	}
	engine, err := d.WorkflowForJob(ctx, job)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow for job %q: %w", job.ID, err)
	}
	if engine == nil {
		return nil, fmt.Errorf("resolve workflow for job %q: workflow is nil", job.ID)
	}
	return engine, nil
}

// queuedChildCanReleaseCoordinator reports whether terminating this queued job
// should drive its coordinator, and distinguishes "no" from "cannot tell".
//
// It requires a delegation parent whose row still EXISTS. An orphaned child — parent
// purged, or a synthetic id never persisted — takes the top-level path, because the
// child finalizer walks to the parent and would fail with "job not found" on every
// poll, and an error that recurs forever is the camouflage this sweep removes.
//
// It does NOT refuse on a merged task any more, and that reversal is the point. The
// merged-task refusal traded one strand for another: in the PR's own headline case —
// reconcileExternallyMergedTasks drives the task to `merged` EARLIER IN THE SAME POLL
// — every child then took the cancel path and its coordinator was never released, so
// the strand simply moved from the child to the parent. Protecting `merged` belongs in
// the state machine, not here: setTaskState now refuses merged -> blocked, so the
// advance can run and the record that the work landed still cannot be undone.
//
// A store error returns an error rather than false. Reading a transient failure as
// "no coordinator" would downgrade a child with a LIVE parent to the cancel path and
// strand that coordinator permanently, for the duration of one failed query.
func (d Daemon) queuedChildCanReleaseCoordinator(ctx context.Context, payload workflow.JobPayload) (bool, error) {
	parentID := strings.TrimSpace(payload.ParentJobID)
	if parentID == "" || d.Workflow == nil {
		return false, nil
	}
	if _, err := d.Store.GetJob(ctx, parentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	taskID := strings.TrimSpace(payload.TaskID)
	if taskID == "" {
		return true, nil
	}
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No task at all: the child still has a coordinator to release, and there
			// is no task state for the advance to write.
			return true, nil
		}
		return false, err
	}
	// A DISPOSED task (dismissed, superseded, stranded) is deliberately outside the
	// state machine's normal transitions, so leave those alone.
	return !workflow.IsDisposedTaskState(task.State), nil
}

// queuedJobSurvivesClosedPullRequest reports whether a queued PR-bound job must be
// left alone even though its pull request is closed. Each class is here because
// killing it would discard work whose purpose outlives the PR:
//
//   - a job routed from a PR COMMENT is an explicit human request. An `ask` posted
//     minutes before a merge is still worth answering, and the operator who asked
//     has no way to see it was silently dropped. The `routed` event the comment path
//     writes is the durable marker.
//   - a CLI-DISPATCHED job is the same request arriving through a different door.
//     `gitmoot agent review <r> --repo o/r --pr N --head-sha <sha> --background`
//     enqueues with Sender "local" and writes no `routed` event, and a retrospective
//     review of a just-merged PR is legitimate work somebody asked for by name. The
//     stranded population this sweep exists for is ENGINE fan-out (Sender "github"),
//     never an operator's own dispatch, so exempting Sender "local" costs the fix
//     nothing and removes its only way to discard an explicit request.
//   - a COORDINATOR CONTINUATION synthesizes work that already happened; the
//     killed-root skip exempts it for the same reason (daemon_scheduler.go).
//   - a PIPELINE stage job owns run rows, and a pipeline PR review is legitimately
//     report-only on an already-merged PR.
//   - a TEMP-WORKER MERGE-BACK summary describes work that already ran. Today it
//     carries PullRequest 0 so the caller's `> 0` gate already skips it; it is named
//     here so a future field addition cannot make it collateral.
//   - a job an operator RETRIED after this sweep terminated it. `gitmoot job retry`
//     accepts `cancelled` and writes the row back to `queued` with a `retry_queued`
//     event, so without this the sweep re-cancelled it on the very next poll, forever:
//     an operator's explicit instruction silently undone in a loop, which is the exact
//     failure this sweep was built to remove rather than create. The test is ORDER, not
//     mere presence: a retry NEWER than the newest supersede means "I know, do it
//     anyway", while a retry older than the supersede is history.
func (d Daemon) queuedJobSurvivesClosedPullRequest(ctx context.Context, job db.Job, payload workflow.JobPayload) (bool, error) {
	if payload.Sender == workflow.PipelineJobSender || payload.Sender == "local" {
		return true, nil
	}
	if payload.DelegationReason == "temp_worker_merge_back" {
		return true, nil
	}
	if job.ID == workflow.DelegationContinuationID(strings.TrimSpace(payload.ParentJobID)) {
		return true, nil
	}
	events, err := d.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if event.Kind == "routed" {
			return true, nil
		}
	}
	// Newest-first: the last word between a supersede and a retry decides.
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Kind {
		case "retry_queued":
			return true, nil
		case workflow.JobEventSupersededPullRequestClosed:
			return false, nil
		}
	}
	return false, nil
}

// reviewJobTargetsPull is deliberately broader than
// workflowReviewJobMatchesPull: supersession applies to every queued/running
// review pinned to an obsolete head, including deliberate review roots that do
// not carry native fanout metadata.
func reviewJobTargetsPull(repoFullName string, pull github.PullRequest, payload workflow.JobPayload) bool {
	return payload.Repo == repoFullName &&
		payload.PullRequest == int(pull.Number) &&
		payload.Branch == pull.HeadRef
}

func workflowReviewJobMatchesPull(repoFullName string, pull github.PullRequest, payload workflow.JobPayload) bool {
	return payload.Repo == repoFullName &&
		payload.PullRequest == int(pull.Number) &&
		payload.Branch == pull.HeadRef &&
		strings.TrimSpace(payload.LeadAgent) != "" &&
		strings.TrimSpace(payload.ReviewRound) != "" &&
		len(payload.Reviewers) > 0
}

// lookupReadyPullRequestTask resolves exactly one ready task for the current PR
// head. A ready branch owner is canonical only when the stored PR mirror binds
// that branch to this PR number and head SHA. A branchless local-review task is
// eligible only when a succeeded review job supplies the same exact binding.
// Multiple eligible branchless rows fail closed.
func (d Daemon) lookupReadyPullRequestTask(ctx context.Context, pull github.PullRequest, memo *reviewJobsMemo) (db.Task, error) {
	if !d.pullRequestHeadIsLocal(pull) {
		return db.Task{}, sql.ErrNoRows
	}
	branchTask, branchErr := d.lookupPullRequestTask(ctx, d.Repo.FullName(), pull.HeadRef)
	if branchErr != nil && !errors.Is(branchErr, sql.ErrNoRows) {
		return db.Task{}, branchErr
	}
	if branchErr == nil && branchTask.State == string(workflow.TaskReadyToMerge) {
		stored, err := d.Store.GetPullRequestByRepoBranch(ctx, d.Repo.FullName(), pull.HeadRef)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return db.Task{}, err
		}
		if err == nil && stored.Number == pull.Number &&
			strings.TrimSpace(stored.HeadSHA) == strings.TrimSpace(pull.HeadSHA) {
			return branchTask, nil
		}
	}

	tasks, err := d.Store.ListTasksByRepo(ctx, d.Repo.FullName())
	if err != nil {
		return db.Task{}, err
	}
	jobs, err := d.reviewJobs(ctx, memo)
	if err != nil {
		return db.Task{}, err
	}
	candidates := make([]db.Task, 0, 1)
	for _, candidate := range tasks {
		if strings.TrimSpace(candidate.Branch) != "" ||
			candidate.State != string(workflow.TaskReadyToMerge) {
			continue
		}
		number, ok := reviewTaskPullRequestNumber(candidate.ID)
		if !ok || number != pull.Number {
			continue
		}
		bound, err := reviewTaskBoundToPullHead(candidate, d.Repo.FullName(), pull, jobs)
		if err != nil {
			return db.Task{}, err
		}
		if bound {
			candidates = append(candidates, candidate)
		}
	}
	switch len(candidates) {
	case 0:
		return db.Task{}, sql.ErrNoRows
	case 1:
		return candidates[0], nil
	default:
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.ID)
		}
		return db.Task{}, fmt.Errorf("ambiguous ready tasks for %s#%d at head %s: %s",
			d.Repo.FullName(), pull.Number, pull.HeadSHA, strings.Join(ids, ", "))
	}
}

func reviewTaskBoundToPullHead(task db.Task, repoFullName string, pull github.PullRequest, jobs []db.Job) (bool, error) {
	for _, job := range jobs {
		if job.State != string(workflow.JobSucceeded) {
			continue
		}
		var payload workflow.JobPayload
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return false, fmt.Errorf("parse job payload %q: %w", job.ID, err)
		}
		if payload.TaskID == task.ID &&
			payload.Repo == repoFullName &&
			payload.PullRequest == int(pull.Number) &&
			payload.Branch == pull.HeadRef &&
			strings.TrimSpace(payload.HeadSHA) == strings.TrimSpace(pull.HeadSHA) {
			return true, nil
		}
	}
	return false, nil
}

func (d Daemon) pullRequestReadyToMerge(ctx context.Context, pull github.PullRequest) (bool, error) {
	_, err := d.lookupReadyPullRequestTask(ctx, pull, nil)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (d Daemon) handleReadyToMergeWorkflow(ctx context.Context, pull github.PullRequest, task db.Task) error {
	if d.Workflow == nil {
		return nil
	}
	ready, _, err := d.Store.RevalidateTaskState(ctx, task.ID, string(workflow.TaskReadyToMerge))
	if err != nil {
		return err
	}
	if !ready {
		return nil
	}
	lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), pull.HeadRef)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	leadAgent := strings.TrimSpace(lock.Owner)
	if leadAgent == "" {
		leadAgent = "github"
	}
	branch := task.Branch
	if branch == "" {
		branch = pull.HeadRef
	}
	err = d.Workflow.HandlePullRequestReadyToMerge(ctx, workflow.PullRequestEvent{
		Repo:                    d.Repo.FullName(),
		Branch:                  branch,
		PullRequest:             int(pull.Number),
		PullRequestDraft:        pull.Draft,
		PullRequestDraftUnknown: pull.DraftUnknown,
		PullRequestMerged:       pullRequestListedAsMerged(pull),
		HeadSHA:                 pull.HeadSHA,
		GoalID:                  task.GoalID,
		TaskID:                  task.ID,
		TaskTitle:               task.Title,
		LeadAgent:               leadAgent,
		Sender:                  "github",
	})
	if errors.Is(err, workflow.ErrMergeTaskStateChanged) {
		return nil
	}
	return err
}

func (d Daemon) reconcileReviewingPullRequest(ctx context.Context, pull github.PullRequest, memo *reviewJobsMemo) error {
	if d.Workflow == nil {
		return nil
	}
	task, err := d.lookupPolledPullRequestTask(ctx, pull)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if task.State != string(workflow.TaskReviewing) {
		return nil
	}
	// Only review jobs advance the reviewing PR here; ListJobsByType filters in SQL
	// so this poll path stops materializing every non-review job's payload (#619). The
	// list is shared across the poll's review-job consumers via memo (once per poll).
	jobs, err := d.reviewJobs(ctx, memo)
	if err != nil {
		return err
	}
	hasCurrentReview := false
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := workflowPayload(job)
		if err != nil {
			return err
		}
		if !workflowReviewJobMatchesPull(d.Repo.FullName(), pull, payload) {
			continue
		}
		if strings.TrimSpace(payload.TaskID) != "" && payload.TaskID != task.ID {
			continue
		}
		if strings.TrimSpace(payload.HeadSHA) != pull.HeadSHA {
			continue
		}
		hasCurrentReview = true
		switch job.State {
		case string(workflow.JobQueued), string(workflow.JobRunning):
			return nil
		}
		if payload.Result == nil {
			continue
		}
		engine := d.Workflow
		if d.WorkflowForJob != nil {
			engine, err = d.WorkflowForJob(ctx, job)
			if err != nil {
				return fmt.Errorf("resolve workflow for job %q: %w", job.ID, err)
			}
			if engine == nil {
				return fmt.Errorf("resolve workflow for job %q: workflow is nil", job.ID)
			}
		}
		if err := engine.AdvanceJob(ctx, job.ID); err != nil {
			var blocked workflow.BlockedError
			if errors.As(err, &blocked) {
				return nil
			}
			return err
		}
		updated, err := d.Store.GetTask(ctx, task.ID)
		if err != nil {
			return err
		}
		if updated.State != string(workflow.TaskReviewing) {
			return nil
		}
	}
	// Either a review already exists at this head, or the lifecycle already ran for
	// this PR at this head earlier in THIS poll — in which case any job it dispatched
	// is invisible to the snapshot above, and re-entering would derive the same round
	// twice.
	if hasCurrentReview || memo.lifecycleRanAtHead(pull) {
		return nil
	}
	return d.handlePullRequestWorkflow(ctx, pull, memo)
}

func (d Daemon) retryClosedReadyToMerge(ctx context.Context, openBranches map[string]struct{}) error {
	tasks, err := d.Store.ListTasksByRepoState(ctx, d.Repo.FullName(), string(workflow.TaskReadyToMerge))
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	type readyPullRequest struct {
		number  int64
		headSHA string
		task    db.Task
	}
	readyBranches := map[string]readyPullRequest{}
	for _, task := range tasks {
		if task.Branch == "" {
			continue
		}
		if lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), task.Branch); err == nil && lock.SkipNativeReviewFanout {
			continue
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, open := openBranches[task.Branch]; open {
			continue
		}
		stored, err := d.Store.GetPullRequestByRepoBranch(ctx, d.Repo.FullName(), task.Branch)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		readyBranches[task.Branch] = readyPullRequest{number: stored.Number, headSHA: stored.HeadSHA, task: task}
	}
	if len(readyBranches) == 0 {
		return nil
	}
	closed, err := d.GitHub.ListPullRequests(ctx, d.Repo, "closed")
	if err != nil {
		return err
	}
	for _, pull := range closed {
		ready, ok := readyBranches[pull.HeadRef]
		if !ok {
			continue
		}
		if pull.Number != ready.number {
			continue
		}
		if ready.headSHA != "" && pull.HeadSHA != ready.headSHA {
			continue
		}
		if err := d.handleReadyToMergeWorkflow(ctx, pull, ready.task); err != nil {
			return err
		}
		delete(readyBranches, pull.HeadRef)
	}
	return nil
}

// reconcileClosedReviewingTasks self-heals tasks wedged in `reviewing` whose PR
// is no longer open on GitHub (#543). The main poll loop only iterates OPEN PRs,
// and the closed-PR retry path (retryClosedReadyToMerge) only covers
// `ready_to_merge` tasks, so a `reviewing` task whose duplicate/superseded PR was
// closed (e.g. by a cleanup job) is never reconciled and stays stuck forever with
// a stale local `open` PR row.
//
// It mirrors retryClosedReadyToMerge's shape and cheap short-circuit: it only
// consults GitHub's closed-PR list when a pr_open, reviewing, or
// changes_requested task has a branch with NO currently-open PR (the wedge), so
// the healthy path — where the task's PR is open and thus present in openBranches
// — makes zero extra GitHub reads.
// A genuinely-open PR is in openBranches and skipped, so the normal review path
// is never disturbed. Matching is by branch + PR number (+ head SHA when known);
// the engine CAS is a no-op unless the task is still one of those three states.
func (d Daemon) reconcileClosedReviewingTasks(ctx context.Context, openBranches map[string]struct{}) error {
	if d.Workflow == nil {
		return nil
	}
	var tasks []db.Task
	for _, state := range []workflow.TaskState{
		workflow.TaskPullRequestOpen,
		workflow.TaskReviewing,
		workflow.TaskChangesRequested,
		workflow.TaskAwaitingHumanMerge,
	} {
		stateTasks, err := d.Store.ListTasksByRepoState(ctx, d.Repo.FullName(), string(state))
		if err != nil {
			return err
		}
		tasks = append(tasks, stateTasks...)
	}
	if len(tasks) == 0 {
		return nil
	}
	type reviewingPullRequest struct {
		task    db.Task
		number  int64
		headSHA string
	}
	candidates := map[string]reviewingPullRequest{}
	for _, task := range tasks {
		if task.Branch == "" {
			continue
		}
		if _, open := openBranches[task.Branch]; open {
			continue
		}
		stored, err := d.Store.GetPullRequestByRepoBranch(ctx, d.Repo.FullName(), task.Branch)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		candidates[task.Branch] = reviewingPullRequest{task: task, number: stored.Number, headSHA: stored.HeadSHA}
	}
	if len(candidates) == 0 {
		return nil
	}
	closed, err := d.GitHub.ListPullRequests(ctx, d.Repo, "closed")
	if err != nil {
		return err
	}
	// Group the closed list by head ref so a candidate branch sees ALL of its
	// closed PRs at once. This is essential for the literal #543 scenario: the
	// real PR was MERGED while a duplicate on the SAME branch was closed unmerged.
	// The local row only ever recorded the duplicate (GetPullRequestByRepoBranch
	// returns the highest number = the duplicate), so matching only the pinned
	// number would resolve to `blocked` and re-surface already-merged work to a
	// human, even though the merge signal is already in this same list.
	closedByBranch := map[string][]github.PullRequest{}
	for _, pull := range closed {
		if _, ok := candidates[pull.HeadRef]; !ok {
			continue
		}
		// Fork heads are excluded outright: the fuzzy same-branch resolution below
		// is keyed on branch text, so a merged fork PR would otherwise drive a
		// local reviewing task to merged and delete its worktree.
		if !d.pullRequestHeadIsLocal(pull) {
			continue
		}
		closedByBranch[pull.HeadRef] = append(closedByBranch[pull.HeadRef], pull)
	}
	for branch, candidate := range candidates {
		pull, merged, ok := selectReconciledPull(candidate.number, candidate.headSHA, closedByBranch[branch])
		if !ok {
			continue
		}
		task := candidate.task
		// The fuzzy same-branch merged-sibling resolution (#543) is only safe for
		// `reviewing`, whose merged case this pass has always owned. pr_open and
		// changes_requested are folded in (#1054) ONLY for the closed-unmerged ->
		// blocked arm; their merged resolution stays with the precise, pinned-PR-
		// number reconcileExternallyMergedTasks pass. Otherwise an unrelated merged
		// PR that merely shares a (fork) branch name — with the head-SHA guard
		// skipped on an empty stored SHA — could drive a still-open task to a
		// spurious `merged` and delete its worktree.
		if merged && workflow.TaskState(task.State) != workflow.TaskReviewing {
			continue
		}
		leadAgent := "github"
		if lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), task.Branch); err == nil {
			if owner := strings.TrimSpace(lock.Owner); owner != "" {
				leadAgent = owner
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := d.Workflow.HandleReviewPullRequestClosed(ctx, workflow.PullRequestEvent{
			Repo:        d.Repo.FullName(),
			Branch:      task.Branch,
			PullRequest: int(pull.Number),
			HeadSHA:     pull.HeadSHA,
			GoalID:      task.GoalID,
			TaskID:      task.ID,
			TaskTitle:   task.Title,
			LeadAgent:   leadAgent,
			Sender:      "github",
		}, merged); err != nil {
			return err
		}
	}
	return nil
}

// selectReconciledPull picks which closed PR resolves a wedged reviewing task and
// whether that resolution is a merge (#543). A MERGED PR on the task's branch
// wins over the pinned-but-closed PR the stale local row points at: in the bug,
// the real PR was merged while a duplicate on the same branch was closed
// unmerged, so resolving only the pinned duplicate would drive the task to a
// spurious `blocked` and report completed work as unfinished. Head SHA is matched
// only when known on BOTH sides (a duplicate on the same branch normally shares
// the head SHA); a merged sibling whose SHA is unknown on either side is still
// preferred over a closed-unmerged pin. When no merged PR is present it falls
// back to reconciling the exact pinned PR (closed-unmerged -> blocked).
func selectReconciledPull(pinnedNumber int64, pinnedHeadSHA string, pulls []github.PullRequest) (github.PullRequest, bool, bool) {
	for _, pull := range pulls {
		if !pullRequestListedAsMerged(pull) {
			continue
		}
		if pinnedHeadSHA != "" && pull.HeadSHA != "" && pull.HeadSHA != pinnedHeadSHA {
			continue
		}
		return pull, true, true
	}
	for _, pull := range pulls {
		if pull.Number != pinnedNumber {
			continue
		}
		if pinnedHeadSHA != "" && pull.HeadSHA != "" && pull.HeadSHA != pinnedHeadSHA {
			continue
		}
		return pull, false, true
	}
	return github.PullRequest{}, false, false
}

// pullRequestListedAsMerged reports whether a PR from the GitHub list endpoint is
// merged. That endpoint reports merged PRs as state="closed" and omits the
// top-level `merged` boolean, carrying `merged_at` as the only reliable merge
// signal — so any of those three is treated as merged and the rest as
// closed-unmerged.
func pullRequestListedAsMerged(pull github.PullRequest) bool {
	return strings.TrimSpace(pull.MergedAt) != "" || pull.Merged ||
		strings.EqualFold(strings.TrimSpace(pull.State), "merged")
}

// lookupPolledPullRequestTask resolves the managed task for a POLLED pull
// request. A fork head whose HeadRef merely COLLIDES with a local branch name
// must resolve nothing: routing it as that task lets an outside contributor's
// pull request drive a local task's review, gate and merge. The identity check
// lives here, in the one resolver every polled-PR consumer calls, rather than at
// each call site, so a consumer added later cannot reintroduce the hole. A fork
// head reports sql.ErrNoRows, which every caller already handles as "this pull
// request has no managed task".
func (d Daemon) lookupPolledPullRequestTask(ctx context.Context, pull github.PullRequest) (db.Task, error) {
	if !d.pullRequestHeadIsLocal(pull) {
		return db.Task{}, sql.ErrNoRows
	}
	return d.lookupPullRequestTask(ctx, d.Repo.FullName(), pull.HeadRef)
}

func (d Daemon) lookupPullRequestTask(ctx context.Context, repoFullName string, branch string) (db.Task, error) {
	task, err := d.Store.GetTaskByRepoBranch(ctx, repoFullName, branch)
	if err == nil {
		return task, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return db.Task{}, err
	}
	task, err = d.Store.GetTask(ctx, branch)
	if err != nil {
		return db.Task{}, err
	}
	if task.RepoFullName != "" && task.RepoFullName != repoFullName {
		return db.Task{}, sql.ErrNoRows
	}
	return task, nil
}

func (d Daemon) workflowReviewers(ctx context.Context) ([]string, error) {
	if d.Workflow != nil && len(d.Workflow.RequiredReviewers) > 0 {
		return append([]string{}, d.Workflow.RequiredReviewers...), nil
	}
	agents, err := d.Store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	reviewers := []string{}
	for _, agent := range agents {
		allowed, err := d.Store.AgentCanAccessRepo(ctx, agent.Name, d.Repo.FullName())
		if err != nil {
			return nil, err
		}
		if allowed && hasCapability(agent.Capabilities, "review") {
			reviewers = append(reviewers, agent.Name)
		}
	}
	return reviewers, nil
}

func (d Daemon) handleComment(ctx context.Context, pull github.PullRequest, comment github.IssueComment) error {
	input := prepareCommentCommandInput(comment.Body)
	if !input.addressed {
		return nil
	}

	seen, err := d.Store.HasCommentSeen(ctx, d.Repo.FullName(), comment.ID)
	if err != nil {
		return err
	}
	if seen {
		return nil
	}

	authorized, err := d.authorizeCommenter(ctx, comment.Author)
	if err != nil {
		return err
	}
	if !authorized {
		if err := d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot ignored comment %d from `%s`: `/gitmoot` commands require write, maintain, or admin repository permission.", comment.ID, comment.Author)); err != nil {
			return err
		}
		return d.markCommentSeen(ctx, pull, comment)
	}

	commands := parseCommentCommands(input, d.commentCommandParser())
	for sequence, parsed := range commands {
		if parsed.err != nil {
			if err := d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not route comment %d: %v.", comment.ID, parsed.err)); err != nil {
				return err
			}
			continue
		}
		if err := d.handleCommand(ctx, pull, comment, sequence, parsed.command); err != nil {
			return err
		}
	}
	return d.markCommentSeen(ctx, pull, comment)
}

// handleIssueComment is the issue-side analogue of handleComment (#389). It
// reuses the same seen_comments dedup and authorize-commenter gate, but routes
// only `@<agent> ask …` commands: an `ask` needs no branch/HeadSHA, so plain
// issues carry no PR-specific actions (implement/review/merge/status/etc. are
// ignored on issues). Non-ask commands never mark the comment seen, so a later
// real ask in the same thread is still picked up.
func (d Daemon) handleIssueComment(ctx context.Context, issue github.Issue, comment github.IssueComment) error {
	input := prepareCommentCommandInput(comment.Body)
	if !input.addressed {
		return nil
	}

	seen, err := d.Store.HasCommentSeen(ctx, d.Repo.FullName(), comment.ID)
	if err != nil {
		return err
	}
	if seen {
		return nil
	}

	authorized, err := d.authorizeCommenter(ctx, comment.Author)
	if err != nil {
		return err
	}
	if !authorized {
		if err := d.ack(ctx, issue.Number, fmt.Sprintf("Gitmoot ignored comment %d from `%s`: `/gitmoot` commands require write, maintain, or admin repository permission.", comment.ID, comment.Author)); err != nil {
			return err
		}
		return d.markIssueCommentSeen(ctx, issue, comment)
	}

	commands := parseCommentCommands(input, d.commentCommandParser())
	handled := false
	for sequence, parsed := range commands {
		if parsed.err != nil {
			if err := d.ack(ctx, issue.Number, fmt.Sprintf("Gitmoot could not route comment %d: %v.", comment.ID, parsed.err)); err != nil {
				return err
			}
			handled = true
			continue
		}
		if parsed.command.Action != "ask" {
			continue
		}
		if err := d.handleIssueAsk(ctx, issue, comment, sequence, parsed.command); err != nil {
			return err
		}
		handled = true
	}
	if !handled {
		return nil
	}
	return d.markIssueCommentSeen(ctx, issue, comment)
}

// handleIssueAsk enqueues a deduped `ask` job for an issue comment and posts an
// acknowledgement, mirroring the agent branch of handleCommand but with an
// issue-comment job id (so issue jobs never collide with PR jobs) and empty
// branch/HeadSHA.
func (d Daemon) handleIssueAsk(ctx context.Context, issue github.Issue, comment github.IssueComment, sequence int, command Command) error {
	// Validate is still load-bearing here for the empty-agent case
	// (`/gitmoot ask @ something` parses with Agent == ""), but it can never
	// return ErrUnsupportedAction: handleIssueComment filters to
	// Action == "ask" before dispatching, and "ask" is an allowed action. An
	// unrecognized action on an issue is dropped at that filter instead (#1355).
	if err := command.Validate(); err != nil {
		return d.ack(ctx, issue.Number, fmt.Sprintf("Gitmoot could not route comment %d: %v.", comment.ID, err))
	}
	agent, err := d.Store.GetAgent(ctx, command.Agent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return d.ack(ctx, issue.Number, fmt.Sprintf("Gitmoot could not find subscribed agent `%s` for this repository.", command.Agent))
		}
		return err
	}
	allowed, err := d.Store.AgentCanAccessRepo(ctx, agent.Name, d.Repo.FullName())
	if err != nil {
		return err
	}
	if !allowed {
		return d.ack(ctx, issue.Number, fmt.Sprintf("Gitmoot agent `%s` is not allowed on `%s`.", agent.Name, d.Repo.FullName()))
	}
	if !hasCapability(agent.Capabilities, command.Action) {
		return d.ack(ctx, issue.Number, fmt.Sprintf("Gitmoot agent `%s` does not advertise `%s` capability.", agent.Name, command.Action))
	}

	job, created, err := d.enqueueJob(ctx, workflow.JobRequest{
		PolicyExempt: "auto-only",
		ID:           issueJobID(d.Repo, issue.Number, comment.ID, sequence, command.Agent, command.Action),
		Agent:        agent.Name,
		Action:       command.Action,
		Repo:         d.Repo.FullName(),
		PullRequest:  int(issue.Number),
		TaskID:       fmt.Sprintf("issue-%d-comment-%d", issue.Number, comment.ID),
		TaskTitle:    issue.Title,
		Sender:       comment.Author,
		Instructions: command.Instructions,
		Constraints: []string{
			"Respond using the gitmoot_result JSON contract.",
			"Keep the work scoped to answering the issue question.",
		},
	})
	if err != nil {
		return err
	}

	if created {
		if err := d.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "routed",
			Message: fmt.Sprintf("routed from issue #%d comment %d by %s", issue.Number, comment.ID, comment.Author),
		}); err != nil {
			return err
		}
	}
	return d.ack(ctx, issue.Number, fmt.Sprintf("Gitmoot queued `%s` job `%s` for `%s`.", command.Action, job.ID, agent.Name))
}

func (d Daemon) handleRecoveryComment(ctx context.Context, pull github.PullRequest, comment github.IssueComment) error {
	input := prepareCommentCommandInput(comment.Body)
	if !input.addressed {
		return nil
	}

	seen, err := d.Store.HasCommentSeen(ctx, d.Repo.FullName(), comment.ID)
	if err != nil {
		return err
	}
	if seen {
		return nil
	}

	authorized, err := d.authorizeCommenter(ctx, comment.Author)
	if err != nil {
		return err
	}
	if !authorized {
		if err := d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot ignored comment %d from `%s`: `/gitmoot` commands require write, maintain, or admin repository permission.", comment.ID, comment.Author)); err != nil {
			return err
		}
		return d.markCommentSeen(ctx, pull, comment)
	}

	parsedCommands := parseCommentCommands(input, d.commentCommandParser())
	commands := make([]Command, 0, len(parsedCommands))
	for _, parsed := range parsedCommands {
		if parsed.err != nil {
			if err := d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not route comment %d: %v.", comment.ID, parsed.err)); err != nil {
				return err
			}
			return d.markCommentSeen(ctx, pull, comment)
		}
		commands = append(commands, parsed.command)
	}
	if len(commands) == 0 || !onlyJobRecoveryCommands(commands) {
		return nil
	}

	for sequence, command := range commands {
		if err := d.handleCommand(ctx, pull, comment, sequence, command); err != nil {
			return err
		}
	}
	return d.markCommentSeen(ctx, pull, comment)
}

func (d Daemon) commentCommandParser() func(string) (Command, bool) {
	if d.parseCommentCommand != nil {
		return d.parseCommentCommand
	}
	return ParseCommand
}

func onlyJobRecoveryCommands(commands []Command) bool {
	for _, command := range commands {
		if command.Action != "retry" && command.Action != "cancel" && command.Action != "help" && command.Action != "resume" {
			return false
		}
	}
	return true
}

func (d Daemon) handleCommand(ctx context.Context, pull github.PullRequest, comment github.IssueComment, sequence int, command Command) error {
	if err := command.Validate(); err != nil {
		if errors.Is(err, ErrUnsupportedAction) {
			d.logf("comment %d on %s#%d addressed Gitmoot but names no known action, ignoring: %v",
				comment.ID, d.Repo.FullName(), pull.Number, err)
			return nil
		}
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not route comment %d: %v.", comment.ID, err))
	}
	switch command.Action {
	case "help":
		return d.handleHelpCommand(ctx, pull)
	case "status":
		return d.handleStatusCommand(ctx, pull, comment)
	case "merge":
		return d.handleMergeCommand(ctx, pull, comment)
	case "retry":
		return d.handleRetryCommand(ctx, pull, command)
	case "cancel":
		return d.handleCancelCommand(ctx, pull, command)
	case "resume":
		return d.handleResumeCommand(ctx, pull, command)
	}

	agent, err := d.Store.GetAgent(ctx, command.Agent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not find subscribed agent `%s` for this repository.", command.Agent))
		}
		return err
	}
	allowed, err := d.Store.AgentCanAccessRepo(ctx, agent.Name, d.Repo.FullName())
	if err != nil {
		return err
	}
	if !allowed {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot agent `%s` is not allowed on `%s`.", agent.Name, d.Repo.FullName()))
	}
	if !hasCapability(agent.Capabilities, command.Action) {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot agent `%s` does not advertise `%s` capability.", agent.Name, command.Action))
	}
	if command.Action == "implement" {
		allowed, err := d.agentOwnsBranchLock(ctx, agent.Name, pull.HeadRef)
		if err != nil {
			return err
		}
		if !allowed {
			return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot agent `%s` cannot implement on `%s` without holding the branch lock.", agent.Name, pull.HeadRef))
		}
	}

	ref, err := d.commentTaskRef(ctx, pull, comment)
	if err != nil {
		return err
	}
	job, created, err := d.enqueueJob(ctx, workflow.JobRequest{
		PolicyExempt: "auto-only",
		ID:           jobID(d.Repo, pull.Number, comment.ID, sequence, command.Agent, command.Action),
		Agent:        agent.Name,
		Action:       command.Action,
		Repo:         d.Repo.FullName(),
		Branch:       pull.HeadRef,
		PullRequest:  int(pull.Number),
		HeadSHA:      pull.HeadSHA,
		GoalID:       ref.goalID,
		TaskID:       ref.id,
		TaskTitle:    ref.title,
		Sender:       comment.Author,
		Instructions: command.Instructions,
		Constraints: []string{
			"Respond using the gitmoot_result JSON contract.",
			"Keep the work scoped to the pull request and requested action.",
		},
	})
	if err != nil {
		return err
	}

	if created {
		if err := d.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "routed",
			Message: fmt.Sprintf("routed from PR #%d comment %d by %s", pull.Number, comment.ID, comment.Author),
		}); err != nil {
			return err
		}
	}
	return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot queued `%s` job `%s` for `%s`.", command.Action, job.ID, agent.Name))
}

func (d Daemon) handleHelpCommand(ctx context.Context, pull github.PullRequest) error {
	lines := []string{
		fmt.Sprintf("Gitmoot help for `%s` PR #%d:", d.Repo.FullName(), pull.Number),
		"- `/gitmoot help`",
		"- `/gitmoot status`",
		"- `/gitmoot retry <job-id>`",
		"- `/gitmoot cancel <job-id>`",
		"- `/gitmoot resume <job-id> <retry|continue|abort> [instructions]`",
		"- `/gitmoot resume <job-id> answer \"<id>: ...\"` — answer a paused ask-gate question",
		"- `/gitmoot merge`",
	}
	agents, err := d.Store.ListAgents(ctx)
	if err != nil {
		return err
	}
	allowed := []string{}
	for _, agent := range agents {
		canAccess, err := d.Store.AgentCanAccessRepo(ctx, agent.Name, d.Repo.FullName())
		if err != nil {
			return err
		}
		if !canAccess {
			continue
		}
		caps := strings.Join(agent.Capabilities, ",")
		if caps == "" {
			caps = "none"
		}
		allowed = append(allowed, fmt.Sprintf("- `%s`: %s", agent.Name, caps))
	}
	if len(allowed) == 0 {
		lines = append(lines, "- agents: none allowed for this repo")
	} else {
		lines = append(lines, "- agents:")
		lines = append(lines, allowed...)
		lines = append(lines, "- agent command: `/gitmoot <agent> <review|implement|ask> <instructions>`")
	}
	return d.ack(ctx, pull.Number, strings.Join(lines, "\n"))
}

func (d Daemon) handleRetryCommand(ctx context.Context, pull github.PullRequest, command Command) error {
	if err := d.validateJobCommandScope(ctx, pull, command.JobID); err != nil {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not retry job `%s`: %v.", command.JobID, err))
	}
	job, err := workflow.RetryJob(ctx, d.Store, command.JobID)
	if err != nil {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not retry job `%s`: %v.", command.JobID, err))
	}
	return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot queued retry for job `%s`.", job.ID))
}

func (d Daemon) handleCancelCommand(ctx context.Context, pull github.PullRequest, command Command) error {
	if err := d.validateJobCommandScope(ctx, pull, command.JobID); err != nil {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not cancel job `%s`: %v.", command.JobID, err))
	}
	job, err := workflow.CancelJob(ctx, d.Store, command.JobID)
	if err != nil {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not cancel job `%s`: %v.", command.JobID, err))
	}
	return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot cancelled job `%s`.", job.ID))
}

// handleResumeCommand resolves a tree paused at awaiting_human (#340) via
// `/gitmoot resume <jobID> retry|continue|abort|answer [instructions]`. It is
// authorize-commenter gated by the caller (handleComment / handleRecoveryComment)
// and job-scope gated here, exactly like retry/cancel. retry re-enqueues the
// failed leg with the human's instructions; continue proceeds the coordinator
// continuation; abort routes to the #305 graceful finalize; answer (#445)
// delivers the human's reply to a non-failure ask-gate pause as injected context
// on the coordinator continuation. The engine rejects a verb whose flavor does
// not match the open round's kind (answer on a failure round, or
// retry/continue/abort on an ask round) with a clear message.
func (d Daemon) handleResumeCommand(ctx context.Context, pull github.PullRequest, command Command) error {
	if d.Workflow == nil {
		return d.ack(ctx, pull.Number, "Gitmoot cannot resume this tree because the workflow engine is not configured.")
	}
	if err := d.validateJobCommandScope(ctx, pull, command.JobID); err != nil {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not resume job `%s`: %v.", command.JobID, err))
	}
	decision, ok := workflow.ParseResumeDecision(command.Decision)
	if !ok {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not resume job `%s`: decision must be retry, continue, abort, or answer.", command.JobID))
	}
	if err := d.Workflow.ResolveEscalation(ctx, command.JobID, decision, command.Instructions); err != nil {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not resume job `%s`: %v.", command.JobID, err))
	}
	return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot resumed job `%s` with `%s`.", command.JobID, decision))
}

func (d Daemon) validateJobCommandScope(ctx context.Context, pull github.PullRequest, jobID string) error {
	job, err := d.Store.GetJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("job not found")
		}
		return err
	}
	payload, err := workflowPayload(job)
	if err != nil {
		return err
	}
	if payload.Repo != d.Repo.FullName() || int64(payload.PullRequest) != pull.Number {
		return fmt.Errorf("job belongs to %s PR #%d", payload.Repo, payload.PullRequest)
	}
	return nil
}

func (d Daemon) handleStatusCommand(ctx context.Context, pull github.PullRequest, comment github.IssueComment) error {
	ref, err := d.commentTaskRef(ctx, pull, comment)
	if err != nil {
		return err
	}
	statusTaskID := ""
	lines := []string{fmt.Sprintf("Gitmoot status for PR #%d:", pull.Number)}
	if task, err := d.Store.GetTask(ctx, ref.id); err == nil {
		statusTaskID = task.ID
		lines = append(lines, fmt.Sprintf("- task: `%s` `%s`", task.ID, task.State))
		if strings.TrimSpace(task.Branch) != "" {
			lines = append(lines, fmt.Sprintf("- branch: `%s`", task.Branch))
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		lines = append(lines, fmt.Sprintf("- task: `%s` not registered", ref.id))
	} else {
		return err
	}
	if strings.TrimSpace(pull.HeadSHA) != "" {
		lines = append(lines, fmt.Sprintf("- head: `%s`", pull.HeadSHA))
	}
	if lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), pull.HeadRef); err == nil {
		lines = append(lines, fmt.Sprintf("- branch_lock: `%s`", lock.Owner))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	counts, err := d.jobStateCounts(ctx, pull, statusTaskID)
	if err != nil {
		return err
	}
	lines = append(lines, "- jobs: "+formatJobCounts(counts))
	if gate, err := d.Store.GetMergeGate(ctx, d.Repo.FullName(), pull.Number); err == nil {
		lines = append(lines, fmt.Sprintf("- merge_gate: `%s` %s", gate.State, strings.TrimSpace(gate.Reason)))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return d.ack(ctx, pull.Number, strings.Join(lines, "\n"))
}

func (d Daemon) handleMergeCommand(ctx context.Context, pull github.PullRequest, comment github.IssueComment) error {
	if d.Workflow == nil {
		return d.ack(ctx, pull.Number, "Gitmoot cannot merge this PR because the workflow engine is not configured.")
	}
	task, err := d.lookupPolledPullRequestTask(ctx, pull)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot cannot merge PR #%d because branch `%s` is not registered as a task.", pull.Number, pull.HeadRef))
		}
		return err
	}
	if task.State == string(workflow.TaskMerged) {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot merged PR #%d.", pull.Number))
	}
	if task.State != string(workflow.TaskReadyToMerge) && task.State != string(workflow.TaskAwaitingHumanMerge) {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot cannot merge PR #%d because task `%s` is `%s`, not `%s` or `%s`.", pull.Number, task.ID, task.State, workflow.TaskReadyToMerge, workflow.TaskAwaitingHumanMerge))
	}
	leadAgent := "github"
	lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), pull.HeadRef)
	if err == nil {
		if lock.SkipNativeReviewFanout {
			return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot native merge is disabled for PR #%d because branch `%s` is managed by an external council gate.", pull.Number, pull.HeadRef))
		}
		if strings.TrimSpace(lock.Owner) != "" {
			leadAgent = lock.Owner
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	reviewers, err := d.workflowReviewers(ctx)
	if err != nil {
		return err
	}
	err = d.Workflow.HandlePullRequestReadyToMerge(ctx, workflow.PullRequestEvent{
		Repo:                    d.Repo.FullName(),
		Branch:                  firstNonEmpty(task.Branch, pull.HeadRef),
		PullRequest:             int(pull.Number),
		PullRequestDraft:        pull.Draft,
		PullRequestDraftUnknown: pull.DraftUnknown,
		HeadSHA:                 pull.HeadSHA,
		GoalID:                  task.GoalID,
		TaskID:                  task.ID,
		TaskTitle:               task.Title,
		LeadAgent:               leadAgent,
		Sender:                  comment.Author,
		RequiredReviewers:       reviewers,
		HumanMergeRequested:     true,
	})
	if err != nil {
		var blocked workflow.BlockedError
		if errors.As(err, &blocked) {
			return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot merge is blocked: %s.", blocked.Reason))
		}
		return err
	}
	task, err = d.Store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if task.State == string(workflow.TaskMerged) {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot merged PR #%d.", pull.Number))
	}
	return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot merge gate ran; task `%s` is `%s`.", task.ID, task.State))
}

func (d Daemon) jobStateCounts(ctx context.Context, pull github.PullRequest, taskID string) (map[string]int, error) {
	jobs, err := d.Store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, job := range jobs {
		payload, err := workflowPayload(job)
		if err != nil {
			return nil, err
		}
		if payload.Repo != d.Repo.FullName() || payload.PullRequest != int(pull.Number) {
			continue
		}
		if strings.TrimSpace(taskID) != "" && strings.TrimSpace(payload.TaskID) != "" && payload.TaskID != taskID {
			continue
		}
		state := strings.TrimSpace(job.State)
		if state == "" {
			state = "unknown"
		}
		counts[state]++
	}
	return counts, nil
}

func workflowPayload(job db.Job) (workflow.JobPayload, error) {
	var payload workflow.JobPayload
	if strings.TrimSpace(job.Payload) == "" {
		return payload, nil
	}
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return workflow.JobPayload{}, fmt.Errorf("parse job payload %q: %w", job.ID, err)
	}
	return payload, nil
}

func formatJobCounts(counts map[string]int) string {
	states := []string{
		string(workflow.JobQueued),
		string(workflow.JobRunning),
		string(workflow.JobSucceeded),
		string(workflow.JobFailed),
		string(workflow.JobBlocked),
		string(workflow.JobCancelled),
	}
	parts := make([]string, 0, len(states))
	for _, state := range states {
		parts = append(parts, fmt.Sprintf("%s=%d", state, counts[state]))
	}
	return strings.Join(parts, " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (d Daemon) commentTaskRef(ctx context.Context, pull github.PullRequest, comment github.IssueComment) (workflowTaskRef, error) {
	ref := workflowTaskRef{
		id:     fmt.Sprintf("pr-%d-comment-%d", pull.Number, comment.ID),
		title:  pull.Title,
		branch: pull.HeadRef,
	}
	task, err := d.lookupPolledPullRequestTask(ctx, pull)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ref, nil
		}
		return workflowTaskRef{}, err
	}
	ref.id = task.ID
	ref.goalID = task.GoalID
	ref.title = task.Title
	if task.Branch != "" {
		ref.branch = task.Branch
	}
	return ref, nil
}

func (d Daemon) agentOwnsBranchLock(ctx context.Context, agentName string, branch string) (bool, error) {
	lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), branch)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return lock.Owner == agentName, nil
}

func (d Daemon) authorizeCommenter(ctx context.Context, author string) (bool, error) {
	if strings.TrimSpace(author) == "" {
		return false, nil
	}
	permission, err := d.GitHub.GetUserPermission(ctx, d.Repo, author)
	if err != nil {
		return false, err
	}
	return hasWritePermission(permission.Permission), nil
}

func hasWritePermission(permission string) bool {
	switch permission {
	case "admin", "maintain", "write":
		return true
	default:
		return false
	}
}

func (d Daemon) enqueueJob(ctx context.Context, request workflow.JobRequest) (db.Job, bool, error) {
	existing, err := d.Store.GetJob(ctx, request.ID)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return db.Job{}, false, err
	}
	var runtimeDefaultModel func(string) string
	if d.Workflow != nil {
		runtimeDefaultModel = d.Workflow.RuntimeDefaultModel
	}
	var requireWorkflowPolicy func(string) workflow.RequireWorkflowPolicy
	var orgPolicy func(string) workflow.OrgEnforcement
	if d.Workflow != nil {
		requireWorkflowPolicy = d.Workflow.RequireWorkflowPolicy
		orgPolicy = d.Workflow.OrgPolicy
	}
	mailbox := workflow.NewMailbox(d.Store, workflow.UnavailableDeliveryWorktreeResolver("daemon comment enqueue"))
	mailbox.RuntimeDefaultModel = runtimeDefaultModel
	mailbox.RequireWorkflowPolicy = requireWorkflowPolicy
	mailbox.OrgPolicy = orgPolicy
	job, err := mailbox.Enqueue(ctx, request)
	return job, true, err
}

func (d Daemon) markCommentSeen(ctx context.Context, pull github.PullRequest, comment github.IssueComment) error {
	_, err := d.Store.MarkCommentSeenIfNew(ctx, db.Comment{
		RepoFullName: d.Repo.FullName(),
		CommentID:    comment.ID,
		PullRequest:  pull.Number,
		Body:         comment.Body,
	})
	return err
}

func (d Daemon) markIssueCommentSeen(ctx context.Context, issue github.Issue, comment github.IssueComment) error {
	_, err := d.Store.MarkCommentSeenIfNew(ctx, db.Comment{
		RepoFullName: d.Repo.FullName(),
		CommentID:    comment.ID,
		PullRequest:  issue.Number,
		Body:         comment.Body,
	})
	return err
}

func (d Daemon) ack(ctx context.Context, issueNumber int64, body string) error {
	_, err := d.GitHub.PostIssueComment(ctx, d.Repo, issueNumber, body)
	return err
}

func (d Daemon) sleep(ctx context.Context, duration time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type workflowTaskRef struct {
	id     string
	goalID string
	title  string
	branch string
}

func hasCapability(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if capability == target {
			return true
		}
	}
	return false
}

func jobID(repo github.Repository, pullNumber, commentID int64, sequence int, agent, action string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(repo.FullName()))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strconv.FormatInt(pullNumber, 10)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strconv.FormatInt(commentID, 10)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strconv.Itoa(sequence)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(agent))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(action))
	return "pr-comment-" + strconv.FormatUint(hash.Sum64(), 36)
}

// issueJobID is the issue-comment analogue of jobID. It hashes the same fields
// (repo + issue number + comment id + sequence + agent + action) but emits an
// `issue-comment-` prefix so an issue ask job never collides with a PR-comment
// job, even for the same numbers.
func issueJobID(repo github.Repository, issueNumber, commentID int64, sequence int, agent, action string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(repo.FullName()))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strconv.FormatInt(issueNumber, 10)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strconv.FormatInt(commentID, 10)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strconv.Itoa(sequence)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(agent))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(action))
	return "issue-comment-" + strconv.FormatUint(hash.Sum64(), 36)
}

func ParseRepository(value string) (github.Repository, error) {
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return github.Repository{}, fmt.Errorf("repo must be owner/repo")
	}
	return github.Repository{Owner: parts[0], Name: parts[1]}, nil
}
