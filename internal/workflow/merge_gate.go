package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

const (
	// GitmootMergeGateContext is the canonical commit-status context for the
	// native merge gate and every observer of its current-head verdict.
	GitmootMergeGateContext = "gitmoot/merge-gate"
	gitmootNoCIContext      = "gitmoot/ci"
	// MergeLeaveOpenAutoMergeKillSwitchReason is persisted with a parked task so
	// a later explicit auto_merge=false -> true config flip can re-arm only this
	// operator decision.
	MergeLeaveOpenAutoMergeKillSwitchReason = "native auto-merge is disabled by the repository kill-switch; leave the pull request open for a human merge"
	commitStatusDescriptionMaxRunes         = 140
	mergeQueueLockTTL                       = 30 * time.Minute
	// The initial lease outlives the maximum eight-hour worker context by one
	// hour. Hourly renewal also protects daemon-owned calls without a deadline.
	mergeTaskStateClaimTTL             = 9 * time.Hour
	mergeTaskStateClaimRenewalInterval = time.Hour
	mergeOutcomeConfirmationTimeout    = 30 * time.Second
)

var ErrMergeTaskStateChanged = errors.New("merge task state changed before external merge")

// NativeMergeGateDisabled reports whether the operator handed the merge decision
// to an external gate via GITMOOT_DISABLE_NATIVE_MERGE_GATE (#545). It is the
// single source for that answer: the native gate abstains, and every observer of
// the gate's commit status must stay silent rather than publish a verdict-shaped
// status no Gitmoot code path will ever resolve.
func NativeMergeGateDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_DISABLE_NATIVE_MERGE_GATE"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

type MergeGateGitHub interface {
	GetPullRequest(ctx context.Context, repo github.Repository, number int64) (github.PullRequest, error)
	GetCombinedStatus(ctx context.Context, repo github.Repository, ref string) (github.CombinedStatus, error)
	ListCheckRunsForRef(ctx context.Context, repo github.Repository, ref string) ([]github.PullRequestCheck, error)
	CompareCommits(ctx context.Context, repo github.Repository, base string, head string) (github.CompareResult, error)
	ListPullRequestChecks(ctx context.Context, repo github.Repository, number int64) ([]github.PullRequestCheck, error)
	CreateCommitStatus(ctx context.Context, input github.CommitStatusInput) (github.CommitStatus, error)
	PostIssueComment(ctx context.Context, repo github.Repository, issueNumber int64, body string) (github.IssueComment, error)
	UpdatePullRequestBranch(ctx context.Context, input github.UpdatePullRequestBranchInput) (github.UpdatePullRequestBranchResult, error)
	BaseRequiresUpToDateHead(ctx context.Context, repo github.Repository, branch string) (required bool, known bool, err error)
	MergePullRequest(ctx context.Context, input github.MergePullRequestInput) (github.MergeResult, error)
}

type MergeGateGit interface {
	WorktreeClean(ctx context.Context) (bool, error)
	UpdateBase(ctx context.Context, remote string, branch string) error
}

type NextTaskEnqueuer interface {
	EnqueueNextTask(ctx context.Context, completedTaskID string) error
}

type WorktreeCleaner interface {
	RemoveWorktree(ctx context.Context, path string) error
}

type PolicyMergeGate struct {
	Store        *db.Store
	GitHub       MergeGateGitHub
	Git          MergeGateGit
	Worktrees    WorktreeCleaner
	NextTasks    NextTaskEnqueuer
	CheckoutPath string
	DeleteBranch bool
	MergeMethod  string
	// AutoMerge permits this native task merge gate to perform a GitHub merge
	// after the mandatory exact-head review and CI gate. False is a kill-switch.
	// PipelineAutoMerger does not call Evaluate and remains governed only by its
	// independent pipeline allow_auto_merge mechanism.
	AutoMerge bool
	// RequireExternalCI hard-blocks a merge whose head reports zero external CI
	// instead of ever stamping the synthetic gitmoot/ci success (#596, layer 3 —
	// the [merge_gate] require_external_ci knob). Default false.
	RequireExternalCI bool
	// MinCIWait is the grace window between the first and second consecutive
	// zero-external observation at the same head before the gate concludes no-CI
	// (#596, layer 1). Zero means use the built-in default (defaultMinCIWait).
	MinCIWait time.Duration
	// MaxCIWait BOUNDS layer 2 (#596): when `.github/workflows/` exists at the head
	// but no external check ever appears (docs-only PRs under paths filters,
	// tag-only / workflow_dispatch-only workflows, or a branch the workflows do not
	// target), the gate stays pending only until MaxCIWait has elapsed with the head
	// unchanged, then falls through to conclude no-CI so such PRs still merge instead
	// of wedging forever. Zero means use the built-in default (defaultMaxCIWait).
	MaxCIWait time.Duration
	// Clock is injectable for deterministic tests. Nil means time.Now.
	Clock                      func() time.Time
	taskClaimTTL               time.Duration
	taskClaimRenewalInterval   time.Duration
	taskClaimRenewalTicks      func(context.Context, time.Duration) <-chan time.Time
	afterTaskClaimRenewal      func()
	outcomeConfirmationTimeout time.Duration
}

// defaultMinCIWait is the built-in grace window used when MinCIWait is unset. It
// mirrors config.DefaultMinCIWait; the workflow package keeps its own copy to
// avoid a config import cycle.
const defaultMinCIWait = 60 * time.Second

// defaultMaxCIWait is the built-in upper bound for layer 2 (workflow-awareness)
// used when MaxCIWait is unset. It mirrors config.DefaultMaxCIWait. It is
// deliberately wide (GitHub Actions creates a check-run within seconds even when
// the run itself is slow, so the only way to stay empty this long is that the
// workflows genuinely do not run for this head), yet finite so a workflow-present
// repo whose workflows never trigger for a given PR still merges.
const defaultMaxCIWait = 10 * time.Minute

func (g PolicyMergeGate) now() time.Time {
	if g.Clock != nil {
		return g.Clock()
	}
	return time.Now().UTC()
}

func (g PolicyMergeGate) minCIWait() time.Duration {
	if g.MinCIWait > 0 {
		return g.MinCIWait
	}
	return defaultMinCIWait
}

func (g PolicyMergeGate) maxCIWait() time.Duration {
	if g.MaxCIWait > 0 {
		return g.MaxCIWait
	}
	return defaultMaxCIWait
}

func (g PolicyMergeGate) taskStateClaimTTL() time.Duration {
	if g.taskClaimTTL > 0 {
		return g.taskClaimTTL
	}
	return mergeTaskStateClaimTTL
}

func (g PolicyMergeGate) taskStateClaimRenewalInterval() time.Duration {
	if g.taskClaimRenewalInterval > 0 {
		return g.taskClaimRenewalInterval
	}
	return mergeTaskStateClaimRenewalInterval
}

func (g PolicyMergeGate) taskStateClaimRenewalTickSource(ctx context.Context, interval time.Duration) <-chan time.Time {
	if g.taskClaimRenewalTicks != nil {
		return g.taskClaimRenewalTicks(ctx, interval)
	}
	out := make(chan time.Time)
	go func() {
		defer close(out)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case tick := <-ticker.C:
				select {
				case out <- tick:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (g PolicyMergeGate) startTaskStateClaimRenewal(taskID, token string) func() error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		ticks := g.taskStateClaimRenewalTickSource(ctx, g.taskStateClaimRenewalInterval())
		var lastErr error
		for {
			select {
			case _, ok := <-ticks:
				if !ok {
					if ctx.Err() != nil {
						done <- lastErr
					} else {
						done <- errors.Join(lastErr, errors.New("task claim renewal clock stopped"))
					}
					return
				}
				renewed, err := g.Store.RenewTaskStateClaim(context.Background(), taskID, token, g.taskStateClaimTTL())
				if err != nil {
					lastErr = fmt.Errorf("renew task state claim: %w", err)
					continue
				}
				if !renewed {
					done <- fmt.Errorf("%w: durable task claim for %s expired or changed owners", ErrMergeTaskStateChanged, taskID)
					return
				}
				lastErr = nil
				if g.afterTaskClaimRenewal != nil {
					g.afterTaskClaimRenewal()
				}
			case <-ctx.Done():
				done <- lastErr
				return
			}
		}
	}()
	return func() error {
		cancel()
		return <-done
	}
}

func (g PolicyMergeGate) mergeOutcomeConfirmationTimeout() time.Duration {
	if g.outcomeConfirmationTimeout > 0 {
		return g.outcomeConfirmationTimeout
	}
	return mergeOutcomeConfirmationTimeout
}

// workflowAwareGitHub is an OPTIONAL capability the merge gate probes for on its
// GitHub client (#596, layer 2). The real *github.GhClient implements it; a
// client that does not is treated as "workflows unknown", which fails safe
// toward the grace path (never toward an instant no-CI stamp).
type workflowAwareGitHub interface {
	WorkflowsExistAtRef(ctx context.Context, repo github.Repository, ref string) (bool, error)
}

func (g PolicyMergeGate) Evaluate(ctx context.Context, request MergeRequest) (MergeDecision, error) {
	// An explicit operator kill-switch remains before validation and every
	// GitHub/local-store operation except terminal recovery. Recovery re-reads
	// the forge and may only finish an already-merged pull request.
	if !g.AutoMerge && !request.HumanMergeRequested && !request.TerminalRecoveryOnly {
		return MergeDecision{LeaveOpen: true, Reason: PlainReason(MergeLeaveOpenAutoMergeKillSwitchReason)}, nil
	}
	if request.PullRequestDraftUnknown && !request.TerminalRecoveryOnly {
		return MergeDecision{LeaveOpen: true, Reason: PlainReason("pull request draft state is unknown")}, nil
	}
	if request.PullRequestDraft && !request.TerminalRecoveryOnly {
		return MergeDecision{LeaveOpen: true, Reason: PlainReason("pull request is draft")}, nil
	}
	if err := g.validate(); err != nil {
		return MergeDecision{}, err
	}
	repo, err := parseRepoFullName(request.Repo)
	if err != nil {
		return MergeDecision{}, err
	}
	if request.PullRequest <= 0 {
		return MergeDecision{}, errors.New("merge gate pull request number is required")
	}
	pr, err := g.GitHub.GetPullRequest(ctx, repo, int64(request.PullRequest))
	if err != nil {
		return MergeDecision{}, err
	}
	if !pullRequestMerged(pr) && strings.TrimSpace(pr.State) == "closed" &&
		strings.TrimSpace(request.TaskID) != "" && strings.TrimSpace(request.ExpectedTaskState) != "" {
		released, _, releaseErr := g.Store.ReleaseRetainedTaskStateClaim(ctx,
			request.TaskID, request.ExpectedTaskState, db.TaskStateClaimKindExternalMergeUncertain)
		if releaseErr != nil {
			return MergeDecision{}, fmt.Errorf("resolve retained task claim after authoritative non-merged observation: %w", releaseErr)
		}
		if released {
			log.Printf("merge gate released retained external merge claim for task %s after pull request #%d reached a terminal non-merge state",
				request.TaskID, request.PullRequest)
		}
	}
	if request.TerminalRecoveryOnly && !pullRequestMerged(pr) {
		return MergeDecision{
			Deferred:   true,
			BlockClass: MergeBlockTransient,
			Reason:     PlainReason("authoritative pull request re-read has not confirmed a merge"),
		}, nil
	}
	headSHA := strings.TrimSpace(pr.HeadSHA)
	if headSHA == "" {
		reason, reasonErr := GateMissReason("merge gate", "pull request head SHA is missing", "")
		if reasonErr != nil {
			return MergeDecision{}, reasonErr
		}
		return g.gateMiss(reason), nil
	}
	if !pullRequestMerged(pr) && strings.TrimSpace(pr.State) != "closed" {
		pendingDecision, isPending, reason, err := g.reviewAndCIGateMiss(ctx, repo, request, headSHA)
		if err != nil {
			return MergeDecision{}, err
		}
		if isPending {
			// CI is still resolving (a check is genuinely in flight, or we're
			// within the #596 Actions-creation-lag grace window) - this is not a
			// policy miss, so retry silently on the next poll instead of parking
			// and escalating.
			return pendingDecision, nil
		}
		if !reason.IsZero() {
			return g.gateMiss(reason), nil
		}
	}
	releaseCheckoutLock, err := g.acquireLocalCheckoutMutationLock(ctx, request)
	if err != nil {
		var blocked BlockedError
		if errors.As(err, &blocked) {
			return MergeDecision{}, fmt.Errorf("merge gate checkout is busy: %s", blocked.Reason)
		}
		return MergeDecision{}, err
	}
	if releaseCheckoutLock != nil {
		defer func() {
			_ = releaseCheckoutLock(context.Background())
		}()
	}
	if pullRequestMerged(pr) {
		if strings.TrimSpace(request.TaskID) != "" && strings.TrimSpace(request.ExpectedTaskState) != "" {
			if _, _, err := g.Store.RecoverClaimedTaskState(ctx, request.TaskID, string(TaskMerged),
				"pull_request_merged", fmt.Sprintf("recovered merged pull request #%d from durable task claim", request.PullRequest)); err != nil {
				return MergeDecision{}, err
			}
		}
		return g.finishMerged(ctx, request, pr, strings.TrimSpace(pr.MergeSHA))
	}
	if strings.TrimSpace(pr.State) == "closed" {
		return g.block(ctx, request, headSHA, "pull request is closed without being merged", MergeBlockQuality)
	}
	if g.Git != nil {
		clean, err := g.Git.WorktreeClean(ctx)
		if err != nil {
			return MergeDecision{}, err
		}
		if !clean {
			return g.block(ctx, request, headSHA, "local worktree is not clean", MergeBlockTransient)
		}
	}
	releaseMergeQueueLock, err := g.acquireMergeQueueLock(ctx, request, pr)
	if err != nil {
		var pending mergePending
		if errors.As(err, &pending) {
			return g.pending(ctx, request, headSHA, pending.reason)
		}
		return MergeDecision{}, err
	}
	defer func() {
		_ = releaseMergeQueueLock(context.Background())
	}()
	if decision, handled, err := g.ensureBranchFresh(ctx, repo, request, pr, headSHA); err != nil {
		return MergeDecision{}, err
	} else if handled {
		return decision, nil
	}
	if pr.Mergeable != nil && !*pr.Mergeable {
		return g.block(ctx, request, headSHA, "pull request is not mergeable; rebase or update the branch", MergeBlockTransient)
	}
	result, err := g.executePullRequestMergeFenced(ctx, request, github.MergePullRequestInput{
		Repo:            repo,
		Number:          int64(request.PullRequest),
		Method:          mergeMethod(g.MergeMethod),
		Subject:         mergeSubject(request),
		Body:            "Merged by Gitmoot after policy gate passed.",
		MatchHeadCommit: headSHA,
		DeleteBranch:    g.DeleteBranch,
	})
	if err != nil {
		return MergeDecision{}, err
	}
	if !result.Merged {
		reason := strings.TrimSpace(result.Message)
		if reason == "" {
			reason = "pull request merge is pending"
		}
		return g.pending(ctx, request, headSHA, reason)
	}
	// The merge is already durable. This status is observability and cannot
	// retroactively turn a completed merge into an error.
	_, _ = g.GitHub.CreateCommitStatus(ctx, github.CommitStatusInput{
		Repo:        repo,
		SHA:         headSHA,
		State:       "success",
		Context:     GitmootMergeGateContext,
		Description: "Gitmoot merge gate passed",
	})
	return g.finishMerged(ctx, request, pr, strings.TrimSpace(result.SHA))
}

func (g PolicyMergeGate) gateMiss(reason MergeReason) MergeDecision {
	// The kind IS the escalation signal: a gate miss escalates because it is one.
	// EscalateMergeGateMiss used to say the same thing a second time, and at :158 the
	// two had already drifted apart -- which is what two representations of one fact
	// always eventually do (#1381).
	return MergeDecision{LeaveOpen: true, Reason: reason}
}

// reviewAndCIGateMiss evaluates the exact-head review-clean and CI-green gate,
// independent of ReviewOptional (#1114 closes the review-required bypass that
// let a repo with no reviewer-capable agent skip review entirely - see
// mergeGateReviewRequired). isPending=true means CI is still resolving (a check
// is genuinely in flight, or evaluation is within the #596 Actions-creation-lag
// grace window preserved by ensureStatuses/concludeNoExternalCI) - this is not a
// policy miss; the caller must return pendingDecision as-is without escalating.
// A non-empty reason with isPending=false means review and/or CI were evaluated
// to completion and at least one is missing, stale, or failed - the caller
// escalates. Both zero-value means the gate is clean.
func (g PolicyMergeGate) reviewAndCIGateMiss(ctx context.Context, repo github.Repository, request MergeRequest, headSHA string) (pendingDecision MergeDecision, isPending bool, reason MergeReason, err error) {
	var miss MergeReason
	if reviewErr := g.ensureFinalReviewCaptured(ctx, request, headSHA); reviewErr != nil {
		var pending mergePending
		if errors.As(reviewErr, &pending) {
			decision, dErr := g.pending(ctx, request, headSHA, pending.reason)
			return decision, true, MergeReason{}, dErr
		}
		var missErr error
		miss, missErr = miss.WithGateMiss("review gate", reviewErr.Error(), headSHA)
		if missErr != nil {
			return MergeDecision{}, false, MergeReason{}, missErr
		}
	}
	if ciErr := g.ensureStatuses(ctx, repo, int64(request.PullRequest), headSHA); ciErr != nil {
		var pending mergePending
		if errors.As(ciErr, &pending) {
			decision, dErr := g.pending(ctx, request, headSHA, pending.reason)
			return decision, true, MergeReason{}, dErr
		}
		var missErr error
		miss, missErr = miss.WithGateMiss("CI gate", ciErr.Error(), headSHA)
		if missErr != nil {
			return MergeDecision{}, false, MergeReason{}, missErr
		}
	}
	return MergeDecision{}, false, miss, nil
}

// executePullRequestMergeFenced durably claims the task's expected state before
// the irreversible GitHub merge. The active lease renews while the external
// call is in flight. Accepted queued merges and outcomes that cannot be
// confirmed remain fenced without expiry until a later authoritative merged or
// terminal non-merge observation resolves them.
func (g PolicyMergeGate) executePullRequestMergeFenced(ctx context.Context, request MergeRequest, input github.MergePullRequestInput) (github.MergeResult, error) {
	expectedState := strings.TrimSpace(request.ExpectedTaskState)
	taskID := strings.TrimSpace(request.TaskID)
	if expectedState == "" || taskID == "" {
		return executePullRequestMerge(ctx, g.GitHub, input)
	}
	token, claimed, currentState, err := g.Store.ClaimTaskState(
		ctx, taskID, expectedState, db.TaskStateClaimKindExternalMerge, g.taskStateClaimTTL())
	if err != nil {
		return github.MergeResult{}, err
	}
	if !claimed {
		return github.MergeResult{}, fmt.Errorf("%w: task %s expected %q, current %q",
			ErrMergeTaskStateChanged, taskID, expectedState, currentState)
	}
	stopRenewal := g.startTaskStateClaimRenewal(taskID, token)
	result, mergeErr := executePullRequestMerge(ctx, g.GitHub, input)
	renewalErr := stopRenewal()

	if mergeErr != nil {
		confirmedPR, confirmationErr := g.confirmExternalMergeOutcome(ctx, input)
		switch {
		case confirmationErr == nil && pullRequestMerged(confirmedPR):
			result = github.MergeResult{Merged: true, SHA: strings.TrimSpace(confirmedPR.MergeSHA)}
			mergeErr = nil
		case confirmationErr == nil && strings.TrimSpace(confirmedPR.State) == "closed":
			releaseErr := g.Store.ReleaseTaskStateClaim(ctx, taskID, token)
			return result, errors.Join(mergeErr, renewalErr, releaseErr)
		default:
			retained, retainErr := g.Store.RetainTaskStateClaim(ctx, taskID, token,
				expectedState, db.TaskStateClaimKindExternalMergeUncertain)
			outcomeErr := errors.Join(mergeErr, renewalErr, retainErr)
			if confirmationErr != nil {
				outcomeErr = errors.Join(outcomeErr, fmt.Errorf("confirm external merge outcome: %w", confirmationErr))
			}
			if !retained {
				return result, fmt.Errorf("external merge outcome is unresolved and durable task ownership could not be retained: %w",
					errors.Join(outcomeErr, ErrMergeTaskStateChanged))
			}
			if confirmationErr == nil {
				return result, fmt.Errorf("external merge returned an error while the pull request remains open; durable task ownership retained for reconciliation: %w",
					outcomeErr)
			}
			return result, fmt.Errorf("external merge outcome is ambiguous; durable task ownership retained for reconciliation: %w",
				outcomeErr)
		}
	}
	if !result.Merged {
		retained, retainErr := g.Store.RetainTaskStateClaim(ctx, taskID, token,
			expectedState, db.TaskStateClaimKindExternalMergeUncertain)
		if !retained {
			return result, fmt.Errorf("external merge is pending and durable task ownership could not be retained: %w",
				errors.Join(renewalErr, retainErr, ErrMergeTaskStateChanged))
		}
		if renewalErr != nil {
			log.Printf("merge gate retained pending task %s after claim renewal warning: %v", taskID, renewalErr)
		}
		return result, nil
	}
	changed, currentState, err := g.Store.CompleteTaskStateClaim(ctx, taskID, token,
		string(TaskMerged), "pull_request_merged",
		fmt.Sprintf("merged pull request #%d while holding durable task-state claim", request.PullRequest))
	if err != nil {
		return result, fmt.Errorf("external merge succeeded but task %s claim completion failed: %w", taskID, err)
	}
	if !changed {
		return result, fmt.Errorf("external merge succeeded but %w: task %s expected %q, current %q",
			ErrMergeTaskStateChanged, taskID, expectedState, currentState)
	}
	if renewalErr != nil {
		log.Printf("merge gate completed task %s after claim renewal warning: %v", taskID, renewalErr)
	}
	return result, nil
}

func (g PolicyMergeGate) confirmExternalMergeOutcome(ctx context.Context, input github.MergePullRequestInput) (github.PullRequest, error) {
	confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.mergeOutcomeConfirmationTimeout())
	defer cancel()
	return g.GitHub.GetPullRequest(confirmCtx, input.Repo, input.Number)
}

// executePullRequestMerge is the single low-level GitHub merge path shared by
// the native policy merge gate and the opt-in pipeline auto-merge gate.
func executePullRequestMerge(ctx context.Context, client interface {
	MergePullRequest(context.Context, github.MergePullRequestInput) (github.MergeResult, error)
}, input github.MergePullRequestInput) (github.MergeResult, error) {
	return client.MergePullRequest(ctx, input)
}

func (g PolicyMergeGate) finishMerged(ctx context.Context, request MergeRequest, pr github.PullRequest, mergeSHA string) (MergeDecision, error) {
	if err := g.recordMerged(ctx, request, pr, mergeSHA); err != nil {
		return MergeDecision{}, err
	}
	branch := strings.TrimSpace(pr.HeadRef)
	if branch == "" {
		branch = strings.TrimSpace(request.Branch)
	}
	if _, err := RecordPullRequestWorkflowTransition(ctx, g.Store, PullRequestEvent{
		Repo:        request.Repo,
		Branch:      branch,
		PullRequest: request.PullRequest,
	}, PullRequestJournalMerged); err != nil {
		// The GitHub merge and local PR record are already durable. Journaling is
		// observability only and must never turn a successful merge into a failure
		// or alter its MergeDecision.
		log.Printf("WARNING: workflow journal PR-merged breadcrumb failed for %s#%d: %v", request.Repo, request.PullRequest, err)
	}
	postMergeWarnings := []string{}
	lock, err := g.Store.GetBranchLock(ctx, request.Repo, pr.HeadRef)
	if err == nil {
		if _, err := g.Store.ReleaseLockWithEvent(ctx, lock, db.BranchLockEvent{Kind: "released", Message: "released after pull request merge"}); err != nil {
			postMergeWarnings = append(postMergeWarnings, "release branch lock: "+err.Error())
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		postMergeWarnings = append(postMergeWarnings, "load branch lock: "+err.Error())
	}
	if err := g.cleanupTaskWorktree(ctx, request, pr.HeadRef); err != nil {
		postMergeWarnings = append(postMergeWarnings, "cleanup task worktree: "+err.Error())
	}
	if g.Git != nil && strings.TrimSpace(pr.BaseRef) != "" {
		if err := g.Git.UpdateBase(ctx, "origin", pr.BaseRef); err != nil {
			postMergeWarnings = append(postMergeWarnings, "update base: "+err.Error())
		}
	}
	if g.NextTasks != nil {
		if err := g.NextTasks.EnqueueNextTask(ctx, request.TaskID); err != nil {
			postMergeWarnings = append(postMergeWarnings, "enqueue next task: "+err.Error())
		}
	}
	reason := "merged"
	if len(postMergeWarnings) > 0 {
		reason = "merged with post-merge warnings: " + strings.Join(postMergeWarnings, "; ")
		_ = g.Store.UpsertMergeGate(ctx, db.MergeGate{RepoFullName: request.Repo, PullRequest: int64(request.PullRequest), State: "merged", Reason: reason})
	}
	return MergeDecision{Ready: true, Merged: true, MergeCommitSHA: mergeSHA, Reason: PlainReason(reason)}, nil
}

// mergeApprovalEvidenceEvent records, on the approving review job, the
// execution provenance of the verdict that authorised a merge.
const mergeApprovalEvidenceEvent = "merge_gate_approval_evidence"

// recordApprovalEvidence makes the executed/static-only distinction VISIBLE
// where merges are decided (#1839).
//
// Without this the distinction was durable JSON inside jobs.result and nothing
// more: a review caught that EvidenceWasExecuted had exactly one production
// caller - the result check that deliberately ignores it for static_only - so
// the gate behaved identically for a verdict that ran the suite and one that
// could not run anything. The claim that the gate consumed the field named a
// consumer that did not exist. This is that consumer.
//
// It records rather than REFUSES on purpose. A static-only verdict is a
// legitimate verdict, and refusing merges on it would be a policy change with
// a fleet-wide blast radius that belongs to whoever owns merge policy, not to
// the change that added the field. What must not happen is a merge whose record
// cannot say which kind of review authorised it.
//
// Best effort: losing the annotation must never fail a merge that the gate has
// otherwise approved.
func (g PolicyMergeGate) recordApprovalEvidence(ctx context.Context, job db.Job, payload JobPayload) {
	if g.Store == nil || payload.Result == nil || strings.TrimSpace(job.ID) == "" {
		return
	}
	evidence := strings.TrimSpace(payload.Result.Evidence)
	if evidence == "" {
		evidence = EvidenceStaticOnly
	}
	detail := "executed"
	if !EvidenceWasExecuted(*payload.Result) {
		detail = "NOT executed - this approval's claims were not produced by running anything"
	}
	// IF ABSENT, because ensureFinalReviewCaptured runs on EVERY merge-gate
	// pass: a PR pending on CI is re-evaluated on every poll, so AddJobEvent
	// grew one row per poll rather than one per approval. Measured at five
	// rows for five evaluations of a single approval, against a store that
	// already has documented job_events volume pain.
	_ = g.Store.AddJobEventIfAbsent(ctx, db.JobEvent{
		JobID: job.ID,
		Kind:  mergeApprovalEvidenceEvent,
		Message: fmt.Sprintf("approval by %s at %s: evidence=%s (%s)",
			strings.TrimSpace(job.Agent), strings.TrimSpace(payload.HeadSHA), evidence, detail),
	})
}

func (g PolicyMergeGate) cleanupTaskWorktree(ctx context.Context, request MergeRequest, headBranch string) error {
	if g.Worktrees == nil || strings.TrimSpace(request.TaskID) == "" {
		return nil
	}
	task, err := g.Store.GetTask(ctx, request.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	path := strings.TrimSpace(task.WorktreePath)
	if path == "" {
		return nil
	}
	if strings.TrimSpace(task.RepoFullName) != "" && task.RepoFullName != request.Repo {
		return fmt.Errorf("task %s belongs to repo %s, not %s", request.TaskID, task.RepoFullName, request.Repo)
	}
	expectedBranch := strings.TrimSpace(request.Branch)
	if expectedBranch == "" {
		expectedBranch = strings.TrimSpace(headBranch)
	}
	if strings.TrimSpace(task.Branch) != "" && task.Branch != expectedBranch {
		return fmt.Errorf("task %s branch is %s, not merged branch %s", request.TaskID, task.Branch, expectedBranch)
	}
	if err := g.Worktrees.RemoveWorktree(ctx, path); err != nil {
		return err
	}
	return g.Store.ClearTaskWorktreePath(ctx, request.TaskID)
}

func (g PolicyMergeGate) acquireLocalCheckoutMutationLock(ctx context.Context, request MergeRequest) (func(context.Context) error, error) {
	if g.Git == nil {
		return nil, nil
	}
	releaseCheckoutLock, _, err := acquireCheckoutMutationLock(ctx, g.Store, g.CheckoutPath, "merge:"+request.Repo+"#"+strconv.Itoa(request.PullRequest), time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return releaseCheckoutLock, nil
}

func (g PolicyMergeGate) acquireMergeQueueLock(ctx context.Context, request MergeRequest, pr github.PullRequest) (func(context.Context) error, error) {
	base := strings.TrimSpace(pr.BaseRef)
	if base == "" {
		base = strings.TrimSpace(pr.BaseSHA)
	}
	if base == "" {
		return nil, errors.New("pull request base ref is missing")
	}
	key := mergeQueueLockKey(request.Repo, base)
	ownerID := "merge-queue:" + request.Repo + "#" + strconv.Itoa(request.PullRequest)
	token, err := newCheckoutMutationOwnerToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	acquired, err := g.Store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(mergeQueueLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, mergePending{reason: fmt.Sprintf("merge queue for %s/%s is busy; daemon will retry", request.Repo, base)}
	}
	return func(releaseCtx context.Context) error {
		_, err := g.Store.ReleaseResourceLock(releaseCtx, key, ownerID, token)
		return err
	}, nil
}

func mergeQueueLockKey(repoFullName string, base string) string {
	return "merge-queue:" + strings.TrimSpace(repoFullName) + ":" + strings.TrimSpace(base)
}

func (g PolicyMergeGate) validate() error {
	if g.Store == nil {
		return errors.New("merge gate store is required")
	}
	if g.GitHub == nil {
		return errors.New("merge gate github client is required")
	}
	return nil
}

func (g PolicyMergeGate) ensureFinalReviewCaptured(ctx context.Context, request MergeRequest, headSHA string) error {
	jobs, err := g.Store.ListJobs(ctx)
	if err != nil {
		return err
	}
	current := JobPayload{Repo: request.Repo, PullRequest: request.PullRequest, TaskID: request.TaskID}
	implementerAttribution := collectImplementerAttribution(jobs, current)
	implementingAgents := implementerAttribution.agents
	missingImplementerReason := implementerAttribution.failureReason()
	// One row per (parent, delegation): the LATEST attempt, exactly as continuation
	// synthesis selects it (childDelegationJobs). Evaluating every attempt let an
	// obsolete FAILED original outlive an approved retry and block its panel
	// forever — the retry can never clear a row that is already terminal, so only
	// a new head could. Ties break on job id so ListJobs ordering never decides.
	latestAttempt := make(map[string]db.Job)
	latestAttemptCount := make(map[string]int)
	for _, job := range jobs {
		if !isDelegationChild(job) {
			continue
		}
		key := strings.TrimSpace(job.ParentJobID) + "\x00" + strings.TrimSpace(job.DelegationID)
		attempt := delegationJobRetryCount(job)
		if held, ok := latestAttempt[key]; ok {
			if attempt < latestAttemptCount[key] {
				continue
			}
			if attempt == latestAttemptCount[key] && job.ID <= held.ID {
				continue
			}
		}
		latestAttempt[key] = job
		latestAttemptCount[key] = attempt
	}
	delegationChildrenByParent := make(map[string][]db.Job)
	for _, job := range latestAttempt {
		parentID := strings.TrimSpace(job.ParentJobID)
		delegationChildrenByParent[parentID] = append(delegationChildrenByParent[parentID], job)
	}
	for parentID := range delegationChildrenByParent {
		sort.Slice(delegationChildrenByParent[parentID], func(i, j int) bool {
			return delegationChildrenByParent[parentID][i].ID < delegationChildrenByParent[parentID][j].ID
		})
	}
	// A round can order remediation, but it must not hide a blocking verdict or
	// an unfinished reviewer slot captured at the head currently being evaluated.
	type taskReview struct {
		job     db.Job
		payload JobPayload
	}
	var taskReviews []taskReview
	taskReviewIDs := make(map[string]struct{})
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return err
		}
		if !sameTask(current, payload) {
			continue
		}
		taskReviews = append(taskReviews, taskReview{job: job, payload: payload})
		taskReviewIDs[job.ID] = struct{}{}
	}
	var reviewsAtHead []taskReview
	for _, review := range taskReviews {
		reviewHead := strings.TrimSpace(review.payload.HeadSHA)
		if reviewHead == "" || reviewHead != headSHA {
			continue
		}
		reviewsAtHead = append(reviewsAtHead, review)
	}
	// Supersession is resolved BEFORE any state or verdict scan, and it covers a
	// row in any state: a strictly later terminal verdict from the same reviewer
	// is what clears a crashed or requeued slot. Rows whose order cannot be
	// established -- one explicit review-N against one empty round -- supersede
	// in neither direction, so no row is ever hidden by ListJobs' id order.
	supersededReviewIDs := map[string]struct{}{}
	for _, review := range reviewsAtHead {
		reviewer := strings.TrimSpace(review.job.Agent)
		if reviewer == "" {
			continue
		}
		for _, candidate := range reviewsAtHead {
			if candidate.payload.Result != nil &&
				strings.TrimSpace(candidate.job.Agent) == reviewer &&
				isReviewReplacementDecision(candidate.payload.Result.Decision) &&
				reviewJobSupersedes(candidate.job, candidate.payload, review.job, review.payload) {
				supersededReviewIDs[review.job.ID] = struct{}{}
				break
			}
		}
	}
	var activeAtHead []taskReview
	for _, review := range reviewsAtHead {
		if _, superseded := supersededReviewIDs[review.job.ID]; superseded {
			continue
		}
		activeAtHead = append(activeAtHead, review)
	}
	// EVERY unsuperseded slot at the evaluated head is inspected, not one row per
	// reviewer: an unfinished or crashed reviewer must not be hidden by a sibling
	// row of the same reviewer whose recency cannot be decided.
	for _, review := range activeAtHead {
		switch JobState(review.job.State) {
		case JobQueued, JobRunning:
			return mergePending{reason: fmt.Sprintf("waiting for reviewer %s at evaluated head (job %s is %s)", strings.TrimSpace(review.job.Agent), review.job.ID, review.job.State)}
		case JobFailed, JobCancelled:
			return fmt.Errorf("crashed reviewer %s at evaluated head (job %s is %s); retry or settle that same job, or push a new head. Reassigning the review to a different agent cannot clear this reviewer's slot", strings.TrimSpace(review.job.Agent), review.job.ID, review.job.State)
		}
	}
	for _, review := range activeAtHead {
		if review.payload.Result == nil {
			continue
		}
		// #1685: a review row that declares delegations is a coordinator FAN-OUT,
		// not a verdict about this head. The engine dispatches a result's
		// delegations AFTER the result is stored, so the panel such a row announces
		// cannot have reported at the moment it was written. An announcement
		// neither answers for the head nor vetoes it, so it is excluded from the
		// verdict population here and decided on its CHILDREN below — the only
		// evidence a fan-out ever produces.
		//
		// The first version of this guard BLOCKED instead, and that wedged the
		// head: supersession is same-agent-only and a coordinator's own
		// continuation is dispatched as an "ask", so no row at that head could ever
		// clear the block — not even a legitimate independent verdict from another
		// reviewer. Excluding the row costs nothing, because a skipped row cannot
		// satisfy the gate either.
		if reviewRowIsFanOut(review.payload.Result) {
			continue
		}
		switch effectiveReviewDecisionForPayload(review.payload, request.ReviewBlockingSeverity) {
		case "changes_requested", "blocked", "failed":
			return mergeBlocked{reason: fmt.Sprintf("review at evaluated head has blocking result from %s", review.job.Agent)}
		}
	}
	if len(activeAtHead) > 0 {
		var selfApprovalReason string
		var unknownImplementerReason string
		var unattributedReviewerReason string
		var undispatchedFanOuts []string
		satisfied := false
		var acceptedJob db.Job
		var acceptedPayload JobPayload
		for _, review := range activeAtHead {
			reviewer := strings.TrimSpace(review.job.Agent)
			if JobState(review.job.State) != JobSucceeded {
				return fmt.Errorf("reviewer %s at evaluated head has unusable job state %s (job %s)", reviewer, review.job.State, review.job.ID)
			}
			if review.payload.Result == nil {
				return fmt.Errorf("abstaining reviewer %s at evaluated head has no recognized decision (job %s); dispatch a fresh review for that same agent at this head, or push a new head. Reassigning to a different agent cannot clear this reviewer's slot", reviewer, review.job.ID)
			}
			decision := effectiveReviewDecisionForPayload(review.payload, request.ReviewBlockingSeverity)
			if reviewRowIsFanOut(review.payload.Result) {
				children := delegationChildrenByParent[review.job.ID]
				if len(children) == 0 {
					// Announced and never dispatched: there is nothing to judge, so this
					// slot stays UNFILLED rather than blocking. An independent verdict at
					// this same head must still be able to decide the PR, and the row is
					// named below if nothing else answers.
					undispatchedFanOuts = append(undispatchedFanOuts, fmt.Sprintf(
						"%s (job %s, %d declared)", reviewer, review.job.ID, len(review.payload.Result.Delegations)))
					continue
				}
				// Fall through as "approved" so the single evidence call in the arm
				// below decides this slot on the children. The fan-out's own decision
				// never was an answer, whichever way it pointed.
				decision = "approved"
			}
			switch decision {
			case "approved":
				if err := ensureDelegatedReviewEvidence(
					review.job, delegationChildrenByParent[review.job.ID], review.payload.Result.Delegations, request.ReviewBlockingSeverity,
				); err != nil {
					return err
				}
				satisfied = true
				acceptedJob, acceptedPayload = review.job, review.payload
				switch {
				case reviewer == "":
					if unattributedReviewerReason == "" {
						unattributedReviewerReason = "latest review round's approval has no recorded reviewer author; an independent reviewer cannot be verified"
					}
				case len(implementingAgents) == 0:
					if unknownImplementerReason == "" {
						unknownImplementerReason = missingImplementerReason
					}
				default:
					if _, selfApproved := implementingAgents[reviewer]; selfApproved && selfApprovalReason == "" {
						selfApprovalReason = fmt.Sprintf("latest review round's approval was authored by %s, the implementing agent; an independent reviewer is required", reviewer)
					}
				}
			case "changes_requested", "blocked", "failed":
				return mergeBlocked{reason: fmt.Sprintf("review at evaluated head has blocking result from %s", reviewer)}
			default:
				return fmt.Errorf("abstaining reviewer %s at evaluated head returned unrecognized decision %q (job %s); dispatch a fresh review for that same agent at this head, or push a new head. Reassigning to a different agent cannot clear this reviewer's slot", reviewer, review.payload.Result.Decision, review.job.ID)
			}
		}
		if reason := reviewAuthorshipFailureReason(selfApprovalReason, unknownImplementerReason, unattributedReviewerReason); reason != "" {
			return errors.New(reason)
		}
		if !satisfied {
			// Every reviewer slot at this head was an undispatched fan-out. Returning
			// nil here would merge on an announcement, which is the #1685 defect.
			return fmt.Errorf(
				"no review verdict at evaluated head: %s declared delegations that never reported; a fan-out is a coordinator continuation, not a verdict",
				strings.Join(undispatchedFanOuts, ", "))
		}
		// AFTER every refusal, never inside the loop: an approval the gate then
		// rejects (self-approval, unknown implementer, an undispatched fan-out)
		// must not leave a record saying it authorised anything.
		g.recordApprovalEvidence(ctx, acceptedJob, acceptedPayload)
		return nil
	}
	var latest reviewRoundKey
	haveLatest := false
	for _, review := range taskReviews {
		if isRoundHistoryDuplicate(review.job, taskReviewIDs) {
			continue
		}
		if _, superseded := supersededReviewIDs[review.job.ID]; superseded {
			continue
		}
		candidate := reviewRoundKeyForJob(review.job, review.payload)
		if !haveLatest || reviewRoundKeyAfter(candidate, latest) {
			latest = candidate
			haveLatest = true
		}
	}
	if !haveLatest {
		return errors.New("final agent review is not captured")
	}
	approved := false
	var acceptedJob db.Job
	var acceptedPayload JobPayload
	var selfApprovalReason string
	var unknownImplementerReason string
	var unattributedReviewerReason string
	var undispatchedFanOuts []string
	type eligibleReview struct {
		job     db.Job
		payload JobPayload
	}
	var eligible []eligibleReview
	for _, review := range taskReviews {
		job := review.job
		payload := review.payload
		if isRoundHistoryDuplicate(job, taskReviewIDs) {
			continue
		}
		candidate := reviewRoundKeyForJob(job, payload)
		if !sameReviewRoundKey(candidate, latest) || payload.Result == nil {
			continue
		}
		if _, superseded := supersededReviewIDs[job.ID]; superseded {
			continue
		}
		if effectiveReviewDecisionForPayload(payload, request.ReviewBlockingSeverity) == "approved" {
			reviewerAgent := strings.TrimSpace(job.Agent)
			switch {
			case reviewerAgent == "":
				if unattributedReviewerReason == "" {
					unattributedReviewerReason = "latest review round's approval has no recorded reviewer author; an independent reviewer cannot be verified"
				}
				continue
			case len(implementingAgents) == 0:
				if unknownImplementerReason == "" {
					unknownImplementerReason = missingImplementerReason
				}
				continue
			default:
				if _, selfApproved := implementingAgents[reviewerAgent]; selfApproved {
					if selfApprovalReason == "" {
						selfApprovalReason = fmt.Sprintf("latest review round's approval was authored by %s, the implementing agent; an independent reviewer is required", reviewerAgent)
					}
					continue
				}
			}
		}
		eligible = append(eligible, eligibleReview{job: job, payload: payload})
	}
	for _, review := range eligible {
		job := review.job
		payload := review.payload
		if err := g.ensureReviewMatchesHead(payload, headSHA, job.Agent); err != nil {
			if reason := reviewAuthorshipFailureReason(selfApprovalReason, unknownImplementerReason, unattributedReviewerReason); reason != "" {
				return errors.New(reason)
			}
			return err
		}
		decision := effectiveReviewDecisionForPayload(payload, request.ReviewBlockingSeverity)
		if reviewRowIsFanOut(payload.Result) {
			// Same rule as the head-bound population above: an announcement is not a
			// verdict, and its delegates are the only evidence it produces.
			children := delegationChildrenByParent[job.ID]
			if len(children) == 0 {
				undispatchedFanOuts = append(undispatchedFanOuts, fmt.Sprintf(
					"%s (job %s, %d declared)", job.Agent, job.ID, len(payload.Result.Delegations)))
				continue
			}
			// Decided as "approved" by the single evidence call in the arm below.
			decision = "approved"
		}
		switch decision {
		case "approved":
			if err := ensureDelegatedReviewEvidence(
				job, delegationChildrenByParent[job.ID], payload.Result.Delegations, request.ReviewBlockingSeverity,
			); err != nil {
				return err
			}
			approved = true
			acceptedJob, acceptedPayload = job, payload
		case "changes_requested", "blocked", "failed":
			// A captured blocking review is an authoritative template-quality rejection
			// (mergeBlocked), distinct from the transient/process review errors below
			// (missing approval, not-yet-captured), so the trace-harvester scores only
			// this one as a negative (#465 INFRA-NOISE-FILTERED).
			return mergeBlocked{reason: fmt.Sprintf("latest review round has blocking result from %s", job.Agent)}
		}
	}
	if !approved {
		if reason := reviewAuthorshipFailureReason(selfApprovalReason, unknownImplementerReason, unattributedReviewerReason); reason != "" {
			return errors.New(reason)
		}
		if len(undispatchedFanOuts) > 0 {
			return fmt.Errorf(
				"no review verdict in the latest round: %s declared delegations that never reported; a fan-out is a coordinator continuation, not a verdict",
				strings.Join(undispatchedFanOuts, ", "))
		}
		return errors.New("required reviewer approval is missing")
	}
	// AFTER every refusal: see the same call on the other acceptance path.
	g.recordApprovalEvidence(ctx, acceptedJob, acceptedPayload)
	return nil
}

// reviewRowIsFanOut reports whether a stored review result is a coordinator
// FAN-OUT rather than a verdict about the code. The engine dispatches a result's
// delegations after the result is stored, so a terminal review decision that
// declares delegations was produced before any delegate could report: it
// announces a panel, it does not answer for the head (#1685).
//
// "blocked" and "failed" are excluded deliberately. They are self-describing
// non-answers that already block on their own terms, and reclassifying them as
// announcements would report the wrong cause for a row that was never mistaken
// for an approval.
func reviewRowIsFanOut(result *AgentResult) bool {
	return ResultIsFanOut(result)
}

// ResultIsFanOut is the package-crossing form of the same rule, for the
// consumers outside this package that read a review decision and act on it
// (the pipeline auto-merge gate and the proof projector). Every surface that
// treats "approved" as an answer has to agree on what an answer IS, or the
// defect simply moves to whichever surface was not updated (#1685).
func ResultIsFanOut(result *AgentResult) bool {
	if result == nil || !isTerminalReviewVerdict(result.Decision) {
		return false
	}
	// Either the declared panel is still visible, or normalization recorded that
	// it was there before the executable instructions were stripped. Reading only
	// delegations[] made every consumer blind to a pipeline review fan-out, whose
	// delegations the mailbox seam removes by design.
	return len(result.Delegations) > 0 || result.FanOut
}

// ensureDelegatedReviewEvidence decides a delegating review on its CHILDREN,
// which are the only evidence a fan-out produces. declared is the parent's own
// delegations[]: a delegation that was announced but has no child row has not
// reported, and counting it as reported is how an announcement used to reach
// merge eligibility.
func ensureDelegatedReviewEvidence(parent db.Job, children []db.Job, declared []Delegation, blockingSeverity string) error {
	if len(children) == 0 {
		return nil
	}
	hasApproval := false
	var blocking []string
	var active []string
	var crashed []string
	var abstaining []string
	var parked []string
	var unrecognized []string
	reported := make(map[string]struct{}, len(children))
	for _, child := range children {
		if id := strings.TrimSpace(child.DelegationID); id != "" {
			reported[id] = struct{}{}
		}
	}
	for _, delegation := range declared {
		id := strings.TrimSpace(delegation.ID)
		if id == "" {
			continue
		}
		if _, ok := reported[id]; !ok {
			// Dispatch happened (children exist) but this delegate produced no row, so
			// its evidence is still outstanding rather than absent.
			active = append(active, fmt.Sprintf("%s (declared, no job)", id))
		}
	}
	for _, child := range children {
		childID := strings.TrimSpace(child.ID)
		switch JobState(child.State) {
		case JobQueued, JobRunning:
			active = append(active, fmt.Sprintf("%s (%s)", childID, child.State))
		case JobSucceeded:
			payload, err := unmarshalPayload(child.Payload)
			if err != nil {
				unrecognized = append(unrecognized, fmt.Sprintf("%s (malformed result)", childID))
				continue
			}
			if payload.Result == nil {
				unrecognized = append(unrecognized, fmt.Sprintf("%s (nil result)", childID))
				continue
			}
			decision := effectiveDelegationDecision(payload.Result, child.Type, "", blockingSeverity)
			switch decision {
			case "approved":
				hasApproval = true
			case "changes_requested", "blocked", "failed":
				blocking = append(blocking, fmt.Sprintf("%s (%s)", childID, decision))
			case "skipped", "implemented":
				abstaining = append(abstaining, fmt.Sprintf("%s (%s)", childID, decision))
			default:
				unrecognized = append(unrecognized, fmt.Sprintf("%s (unrecognized decision %q)", childID, decision))
			}
		case JobFailed, JobCancelled:
			crashed = append(crashed, fmt.Sprintf("%s (%s)", childID, child.State))
		case JobBlocked:
			parked = append(parked, fmt.Sprintf("%s (%s)", childID, child.State))
		default:
			unrecognized = append(unrecognized, fmt.Sprintf("%s (unrecognized state %q)", childID, child.State))
		}
	}
	sort.Strings(blocking)
	sort.Strings(active)
	sort.Strings(crashed)
	sort.Strings(abstaining)
	sort.Strings(parked)
	sort.Strings(unrecognized)
	details := make([]string, 0, 6)
	if len(blocking) > 0 {
		details = append(details, "blocking children: "+strings.Join(blocking, ", "))
	}
	if len(unrecognized) > 0 {
		details = append(details, "unrecognized children: "+strings.Join(unrecognized, ", "))
	}
	if len(active) > 0 {
		details = append(details, "active children: "+strings.Join(active, ", "))
	}
	if len(crashed) > 0 {
		details = append(details, "crashed children: "+strings.Join(crashed, ", "))
	}
	if len(abstaining) > 0 {
		details = append(details, "abstaining children: "+strings.Join(abstaining, ", "))
	}
	if len(parked) > 0 {
		details = append(details, "parked children: "+strings.Join(parked, ", "))
	}
	reasonDetails := strings.Join(details, "; ")
	// The first matching class decides the outcome; reasonDetails retains every
	// lower-priority obligation so a winning class cannot hide its siblings.
	if len(blocking) > 0 {
		return mergeBlocked{reason: fmt.Sprintf(
			"delegated review parent %s has blocking delegation evidence (%s)",
			parent.ID,
			reasonDetails,
		)}
	}
	if len(unrecognized) > 0 {
		return fmt.Errorf(
			"delegated review parent %s has unrecognized delegation evidence (%s); rerun or repair the delegated review",
			parent.ID,
			reasonDetails,
		)
	}
	if len(active) > 0 {
		return mergePending{reason: fmt.Sprintf(
			"waiting for delegated review parent %s to produce surviving evidence (%s)",
			parent.ID,
			reasonDetails,
		)}
	}
	if len(crashed) > 0 {
		return fmt.Errorf(
			"delegated review parent %s has crashed delegation children (%s); rerun or repair the delegated review",
			parent.ID,
			reasonDetails,
		)
	}
	if len(abstaining) > 0 {
		return fmt.Errorf(
			"delegated review parent %s has no surviving delegation evidence: abstaining delegation children (%s); rerun or repair the delegated review",
			parent.ID,
			reasonDetails,
		)
	}
	if len(parked) > 0 {
		return fmt.Errorf(
			"delegated review parent %s has no surviving delegation evidence (%s); rerun or repair the delegated review",
			parent.ID,
			reasonDetails,
		)
	}
	if hasApproval {
		return nil
	}
	return fmt.Errorf(
		"delegated review parent %s has no surviving delegation evidence; rerun or repair the delegated review",
		parent.ID,
	)
}

const (
	// The attribution gap and an independence FAILURE are different facts with
	// different remedies, and #1765 was filed because this constant reported the
	// first as the second. No implement row means the gate cannot say WHO
	// implemented, so it cannot prove the reviewer differs; it emphatically does
	// not mean the reviewer implemented this. In-session implementation (a seat
	// editing at its pane, no engine implement job) reaches this branch on every
	// PR, so the message must name the durable remedy rather than prescribe a
	// human bridge: session-recorded jobs (#657) create rows with
	// Type == "implement", which is exactly what collectImplementerAttribution
	// reads. The gate still refuses — attribution genuinely is unknown — but the
	// lane can now clear it without a coordinator round.
	noImplementJobAttributionReason            = "latest review round's approval is NOT disqualified, but independence cannot be verified: no implement job is recorded for this task, so the gate cannot establish who implemented it. This is an attribution gap, not a failed independence check, and it is the expected state for in-session implementation. Remedy, runnable by the implementing lane: record the durable attribution row with gitmoot job record --agent <implementing-agent> --repo <owner/repo> --type implement --decision implemented --task <task-id> --pr <number> --head-sha <sha>, then re-evaluate. Do not record an agent that did not implement, and do not record the reviewer"
	mismatchedImplementTaskAttributionReason   = "latest review round's approval cannot be verified as independent: implement jobs are recorded, but none match this task identity; this is an attribution anomaly and may indicate a stable-task-identity regression"
	emptyImplementAgentAttributionReason       = "latest review round's approval cannot be verified as independent: an implement job matches this task but has no recorded agent; this is an attribution data anomaly"
	malformedImplementPayloadAttributionReason = "latest review round's approval cannot be verified as independent: an implement job has a malformed payload, so attribution for this task cannot be verified; this is a corrupt-record anomaly"
)

type implementerAttributionEvidence struct {
	agents              map[string]struct{}
	sawImplementJob     bool
	sawTaskMismatch     bool
	sawEmptyAgent       bool
	sawMalformedPayload bool
}

func collectImplementerAttribution(jobs []db.Job, current JobPayload) implementerAttributionEvidence {
	evidence := implementerAttributionEvidence{agents: make(map[string]struct{})}
	for _, job := range jobs {
		if job.Type != "implement" {
			continue
		}
		evidence.sawImplementJob = true
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			evidence.sawMalformedPayload = true
			continue
		}
		if !sameTask(current, payload) {
			if current.Repo != "" && current.Repo == payload.Repo &&
				current.PullRequest > 0 && current.PullRequest == payload.PullRequest {
				evidence.sawTaskMismatch = true
			}
			continue
		}
		agent := strings.TrimSpace(job.Agent)
		if agent == "" {
			evidence.sawEmptyAgent = true
			continue
		}
		evidence.agents[agent] = struct{}{}
	}
	return evidence
}

func (e implementerAttributionEvidence) failureReason() string {
	if len(e.agents) > 0 {
		return ""
	}
	switch {
	case e.sawMalformedPayload:
		return malformedImplementPayloadAttributionReason
	case e.sawEmptyAgent:
		return emptyImplementAgentAttributionReason
	case e.sawTaskMismatch:
		return mismatchedImplementTaskAttributionReason
	case !e.sawImplementJob:
		return noImplementJobAttributionReason
	default:
		// Reached when implement rows exist but none belong to this task and the
		// repo/PR did not match either (an unrelated lane's row). For the
		// operator that is the SAME fact as having no row at all -- there is no
		// attribution for THIS task -- and the same remedy applies, so it
		// deliberately shares the reason. TestPolicyMergeGateNamesImplementer
		// AttributionDeclineCause pins both arms to this text; #1765 briefly
		// split them and that test caught it.
		return noImplementJobAttributionReason
	}
}

func reviewAuthorshipFailureReason(selfApproval string, unknownImplementer string, unattributedReviewer string) string {
	// Keep the operator-facing cause stable when a round contains multiple
	// disqualified approvals.
	for _, reason := range []string{selfApproval, unknownImplementer, unattributedReviewer} {
		if reason = strings.TrimSpace(reason); reason != "" {
			return reason
		}
	}
	return ""
}

func reviewJobRecordedAfter(left db.Job, right db.Job) bool {
	if after, decided := recordedTimestampAfter(left.UpdatedAt, right.UpdatedAt); decided {
		return after
	}
	if after, decided := recordedTimestampAfter(left.CreatedAt, right.CreatedAt); decided {
		return after
	}
	return false
}

func reviewJobSupersedes(leftJob db.Job, leftPayload JobPayload, rightJob db.Job, rightPayload JobPayload) bool {
	leftRound := reviewRoundKeyForJob(leftJob, leftPayload)
	rightRound := reviewRoundKeyForJob(rightJob, rightPayload)
	if (leftRound.name != "") != (rightRound.name != "") {
		// The two rows come from different dispatch paths and their rounds cannot be
		// ordered against each other: the engine always stamps review-N, while
		// `gitmoot agent review <reviewer> --repo <o/r> --pr <N> --head-sha <H>`
		// creates a row with the head set and NO round. Ranking them would let one
		// path hide the other's live or blocking evidence, so a real verdict may
		// resolve only a row that has already settled WITHOUT one -- no result at
		// all, or a decision that is not a verdict (for example "skipped").
		//
		// That single exception is load-bearing: such a row is a settled non-answer,
		// so nothing is hidden by resolving it, and it is otherwise inescapable.
		// `gitmoot job retry` and `gitmoot job cancel` both refuse a succeeded job,
		// and re-polling the same head dispatches nothing because the round already
		// has a job, so the CLI re-review this gate's own error message asks for is
		// the only exit. Queued, running, failed and cancelled rows keep blocking:
		// their exits are settlement and `gitmoot job retry`, which mutate the row
		// itself rather than adding a second one.
		return isSettledNonVerdictReview(rightJob, rightPayload) && reviewJobRecordedAfter(leftJob, rightJob)
	}
	if reviewRoundKeyAfter(leftRound, rightRound) {
		return true
	}
	if reviewRoundKeyAfter(rightRound, leftRound) || !sameReviewRoundKey(leftRound, rightRound) {
		return false
	}
	return reviewJobRecordedAfter(leftJob, rightJob)
}

// isSettledNonVerdictReview reports whether a review row has reached a terminal
// state carrying no verdict: succeeded with no result, or with a decision the
// gate does not recognize as one. Such a row can never become a verdict on its
// own, and its state alone proves no reviewer work is still in flight.
func isSettledNonVerdictReview(job db.Job, payload JobPayload) bool {
	if JobState(job.State) != JobSucceeded {
		return false
	}
	if payload.Result == nil {
		return true
	}
	return !isReviewReplacementDecision(payload.Result.Decision)
}

// isRoundHistoryDuplicate reports whether a delegated review row is a SUB-REVIEW
// of another review job for the same task. Those rows are round-history
// duplicates of their parent (#1737: a lens child's id is its parent's id plus a
// suffix, and its verdict is already validated as the parent's evidence by
// ensureDelegatedReviewEvidence), so they must not compete for the latest round.
//
// A delegated review whose parent is NOT a review job for this task is the
// gate's own required review, not a duplicate of one. A #332 integration-worktree
// review is exactly that shape -- the engine clears its HeadSHA, so it can never
// appear in reviewsAtHead -- and excluding it from round selection deadlocks the
// merge (#388).
func isRoundHistoryDuplicate(job db.Job, taskReviewIDs map[string]struct{}) bool {
	if !isDelegationChild(job) {
		return false
	}
	_, parentIsTaskReview := taskReviewIDs[strings.TrimSpace(job.ParentJobID)]
	return parentIsTaskReview
}

// reviewRoundKey is the ONE ordering key for the review rows of a task, shared by
// every decision that ranks them: supersession, latest-round selection and
// eligibility. An engine round orders by its number. A row dispatched by
// `gitmoot agent review <reviewer> --repo <o/r> --pr <N> --head-sha <H>` carries
// no round and orders by when its verdict was RECORDED -- UpdatedAt, falling back
// to CreatedAt -- never by dispatch time alone.
//
// Dispatch time cannot express verdict recency: `gitmoot job retry` re-runs a row
// IN PLACE, keeping created_at and bumping updated_at, so the EARLIEST-created row
// can hold the NEWEST verdict. Ranking by created_at let a later-created but
// earlier-recorded approval discard a retried row's changes_requested and merge.
type reviewRoundKey struct {
	name       string
	recordedAt string
	createdAt  string
}

func reviewRoundKeyForJob(job db.Job, payload JobPayload) reviewRoundKey {
	round := strings.TrimSpace(payload.ReviewRound)
	if round != "" {
		return reviewRoundKey{name: round}
	}
	return reviewRoundKey{recordedAt: job.UpdatedAt, createdAt: job.CreatedAt}
}

// reviewRoundKeyRecency is the single recency comparison behind both key
// operations below. Its precedence is the same as reviewJobRecordedAfter's, so a
// round key and a job row can never disagree about which of two rows is newer.
func reviewRoundKeyRecency(left reviewRoundKey, right reviewRoundKey) (after bool, decided bool) {
	if after, decided := recordedTimestampAfter(left.recordedAt, right.recordedAt); decided {
		return after, true
	}
	return recordedTimestampAfter(left.createdAt, right.createdAt)
}

func reviewRoundKeyAfter(left reviewRoundKey, right reviewRoundKey) bool {
	leftExplicit := left.name != ""
	rightExplicit := right.name != ""
	if leftExplicit != rightExplicit {
		return leftExplicit
	}
	if leftExplicit {
		return reviewRoundAfter(left.name, right.name)
	}
	after, decided := reviewRoundKeyRecency(left, right)
	return decided && after
}

func sameReviewRoundKey(left reviewRoundKey, right reviewRoundKey) bool {
	leftExplicit := left.name != ""
	rightExplicit := right.name != ""
	if leftExplicit || rightExplicit {
		return leftExplicit && rightExplicit && left.name == right.name
	}
	// Two roundless rows are the same round exactly when no persisted timestamp
	// separates them: recordedTimestampAfter decides only when both values parse
	// and differ, so an unparseable or equal pair stays one conservative round
	// rather than being ordered by job id.
	_, decided := reviewRoundKeyRecency(left, right)
	return !decided
}

func isDelegationChild(job db.Job) bool {
	return strings.TrimSpace(job.ParentJobID) != "" && strings.TrimSpace(job.DelegationID) != ""
}

func isReviewReplacementDecision(decision string) bool {
	switch decision {
	case "approved", "changes_requested", "blocked", "failed":
		return true
	default:
		return false
	}
}

func recordedTimestampAfter(left string, right string) (after bool, decided bool) {
	leftTime, leftOK := parseStoredJobTime(left)
	rightTime, rightOK := parseStoredJobTime(right)
	if !leftOK || !rightOK || leftTime.Equal(rightTime) {
		return false, false
	}
	return leftTime.After(rightTime), true
}

func (g PolicyMergeGate) ensureBranchFresh(ctx context.Context, repo github.Repository, request MergeRequest, pr github.PullRequest, headSHA string) (MergeDecision, bool, error) {
	base := strings.TrimSpace(pr.BaseRef)
	if base == "" {
		base = strings.TrimSpace(pr.BaseSHA)
	}
	if base == "" {
		decision, err := g.block(ctx, request, headSHA, "pull request base ref is missing", MergeBlockTransient)
		return decision, true, err
	}
	compare, err := g.GitHub.CompareCommits(ctx, repo, base, headSHA)
	if err != nil {
		return MergeDecision{}, false, err
	}
	status := strings.ToLower(strings.TrimSpace(compare.Status))
	if compare.BehindBy > 0 || status == "behind" || status == "diverged" {
		if status != "diverged" && g.baseAllowsBehindMerge(ctx, repo, base) {
			// #1865: merely behind, and GitHub does not require an up-to-date
			// head here. Requesting the update at this point would create a
			// merge commit and supersede the head the verdict is bound to
			// within seconds, buying a fresh paid review round. Merge the
			// reviewed head instead.
			return MergeDecision{}, false, nil
		}
		_, err := g.GitHub.UpdatePullRequestBranch(ctx, github.UpdatePullRequestBranchInput{
			Repo:            repo,
			Number:          int64(request.PullRequest),
			ExpectedHeadSHA: headSHA,
		})
		if err == nil {
			decision, pendingErr := g.pending(ctx, request, headSHA, fmt.Sprintf("pull request branch update from %s requested; daemon will retry after GitHub refreshes the head SHA and checks", base))
			return decision, true, pendingErr
		}
		switch {
		case github.IsUpdatePullRequestBranchError(err, github.UpdatePullRequestBranchErrorStaleHead):
			decision, pendingErr := g.pending(ctx, request, headSHA, "pull request head changed while updating branch; daemon will retry with the latest head SHA")
			return decision, true, pendingErr
		case github.IsUpdatePullRequestBranchError(err, github.UpdatePullRequestBranchErrorConflict):
			reason := fmt.Sprintf("branch update conflicts with %s; manual or agent fix required", base)
			_ = g.postMergeConflictComment(ctx, repo, request, pr, reason)
			decision, blockErr := g.block(ctx, request, headSHA, reason, MergeBlockTransient)
			return decision, true, blockErr
		case github.IsUpdatePullRequestBranchError(err, github.UpdatePullRequestBranchErrorUnsupported):
			decision, blockErr := g.block(ctx, request, headSHA, fmt.Sprintf("GitHub cannot update this pull request branch automatically: %s", err), MergeBlockTransient)
			return decision, true, blockErr
		default:
			decision, pendingErr := g.pending(ctx, request, headSHA, fmt.Sprintf("GitHub branch update failed transiently: %s; daemon will retry", err))
			return decision, true, pendingErr
		}
	}
	if status != "" && status != "ahead" && status != "identical" {
		decision, err := g.block(ctx, request, headSHA, fmt.Sprintf("pull request branch freshness is unknown: compare status %q", compare.Status), MergeBlockTransient)
		return decision, true, err
	}
	return MergeDecision{}, false, nil
}

// baseAllowsBehindMerge reports whether a head that is merely BEHIND base may be
// merged without first requesting a branch update (#1865).
//
// It FAILS CLOSED: an undetermined protection read keeps the pre-#1865
// behaviour of updating the branch and retrying, because an unprotected branch
// and a token that cannot read protection are indistinguishable from here.
func (g PolicyMergeGate) baseAllowsBehindMerge(ctx context.Context, repo github.Repository, base string) bool {
	if g.GitHub == nil {
		return false
	}
	required, known, err := g.GitHub.BaseRequiresUpToDateHead(ctx, repo, base)
	if err != nil || !known {
		return false
	}
	return !required
}

func (g PolicyMergeGate) postMergeConflictComment(ctx context.Context, repo github.Repository, request MergeRequest, pr github.PullRequest, reason string) error {
	if request.PullRequest <= 0 {
		return nil
	}
	base := strings.TrimSpace(pr.BaseRef)
	if base == "" {
		base = strings.TrimSpace(pr.BaseSHA)
	}
	body := strings.Join([]string{
		"Gitmoot merge gate is blocked.",
		"",
		"Gitmoot could not update this pull request branch before merge because it conflicts with `" + base + "`.",
		"",
		"- reason: " + reason,
		"- retry: stopped; this is not retryable until the branch is fixed",
		"- task: " + mergeConflictTaskLabel(request),
		"- next action: resolve the conflict manually, or queue a Gitmoot implement/fix job so Gitmoot applies file changes in the task worktree and owns commit/push/PR refresh",
		"- after fix: rerun review/merge on the updated pull request head",
	}, "\n")
	_, err := g.GitHub.PostIssueComment(ctx, repo, int64(request.PullRequest), body)
	return err
}

func mergeConflictTaskLabel(request MergeRequest) string {
	taskID := strings.TrimSpace(request.TaskID)
	if taskID == "" {
		return "unknown"
	}
	return taskID
}

func (g PolicyMergeGate) ensureReviewMatchesHead(payload JobPayload, headSHA string, agent string) error {
	reviewHead := strings.TrimSpace(payload.HeadSHA)
	if reviewHead == headSHA {
		return nil
	}
	if reviewHead != "" {
		return fmt.Errorf("latest review from %s is for a different head SHA", agent)
	}
	// A review that ran in an integration worktree (#332 decompose-and-verify)
	// has its inherited HeadSHA deliberately cleared by the engine
	// (allocateAndEnqueueDelegation in engine.go): the worktree carries no
	// branch and is validated against its own fresh HEAD, not the parent PR
	// head. Such a review legitimately records no head SHA, so accepting it here
	// is what lets a gate-required integration review advance instead of
	// deadlocking (#388). This is narrow: a normal review with a mismatched
	// non-empty head still fails above, and a normal review missing a head SHA
	// but lacking the integration-worktree markers still fails below.
	if isIntegrationWorktreeReview(payload) {
		return nil
	}
	return fmt.Errorf("latest review from %s does not record a head SHA; rerun review", agent)
}

// isIntegrationWorktreeReview reports whether the review job ran in a
// gitmoot-managed delegation worktree (it carries both a delegation id and an
// allocated worktree path). The engine clears the inherited HeadSHA for exactly
// these children so they validate against their isolated worktree HEAD, mirroring
// isDelegationWorktreeChild in the daemon's checkout validation.
func isIntegrationWorktreeReview(payload JobPayload) bool {
	return strings.TrimSpace(payload.DelegationID) != "" && strings.TrimSpace(payload.WorktreePath) != ""
}

func (g PolicyMergeGate) ensureStatuses(ctx context.Context, repo github.Repository, pullRequest int64, headSHA string) error {
	externalCount, err := g.evaluateStatuses(ctx, repo, pullRequest, headSHA)
	if err != nil {
		return err
	}
	if externalCount == 0 {
		return g.concludeNoExternalCI(ctx, repo, pullRequest, headSHA)
	}
	return nil
}

// evaluateStatuses applies the native merge gate's explicit state semantics and
// returns the total external status/check count. A caller chooses its zero-signal
// policy: the native gate uses its bounded no-CI machinery, while unattended
// pipeline auto-merge fails closed immediately.
func (g PolicyMergeGate) evaluateStatuses(ctx context.Context, repo github.Repository, pullRequest int64, headSHA string) (int, error) {
	status, err := g.GitHub.GetCombinedStatus(ctx, repo, headSHA)
	if err != nil {
		return 0, err
	}
	externalStatusCount := 0
	for _, item := range status.Statuses {
		if strings.HasPrefix(item.Context, "gitmoot/") {
			if item.Context == GitmootMergeGateContext {
				continue
			}
			if statusPending(item.State) {
				return 0, mergePending{reason: fmt.Sprintf("gitmoot status %q is pending", item.Context)}
			}
			if item.State != "success" {
				return 0, mergeBlocked{reason: fmt.Sprintf("gitmoot status %q is %s", item.Context, item.State)}
			}
			continue
		}
		externalStatusCount++
		if statusPending(item.State) {
			return 0, mergePending{reason: "external commit status " + item.Context + " is pending"}
		}
		if item.State != "success" {
			return 0, mergeBlocked{reason: "external commit status " + item.Context + " is not successful"}
		}
	}

	checks, err := g.GitHub.ListCheckRunsForRef(ctx, repo, headSHA)
	if err != nil {
		return 0, err
	}
	externalCheckCount := 0
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		if name == GitmootMergeGateContext {
			continue
		}
		externalCheckCount++
		if checkPending(check) {
			if name == "" {
				name = "unnamed check"
			}
			return 0, mergePending{reason: fmt.Sprintf("external CI check %q is pending", name)}
		}
		if !checkPassed(check) {
			if name == "" {
				name = "unnamed check"
			}
			return 0, mergeBlocked{reason: fmt.Sprintf("external CI check %q is not successful", name)}
		}
	}
	return externalCheckCount + externalStatusCount, nil
}

// concludeNoExternalCI is the layered defense for the #596 no-CI race. When a
// head reports zero external commit-statuses AND zero check-runs, it decides —
// across daemon polls — whether that truly means "this repo has no CI at this
// head" or merely "GitHub Actions has not created the run yet". Every layer is
// TIME-BOUNDED and measured from a SINGLE persisted first-zero observation at the
// head (recordOrLoadFirstZero), so no path can hold the gate pending forever and
// a new head resets every window together.
//
// Layer 2 (workflow-awareness): if `.github/workflows/` demonstrably exists at
// the head tree, a zero observation is most likely an Actions creation lag, so
// the gate stays pending — but only until MaxCIWait has elapsed with the head
// unchanged. Past that bound the workflows demonstrably never produce a check for
// this head (docs-only PRs under paths filters, tag-only / workflow_dispatch-only
// workflows, or a branch the workflows do not target), so the gate falls through
// and concludes no-CI rather than wedging the merge forever. A workflow-read error
// fails SAFE toward the grace path (treated as workflows-unknown), never toward an
// instant stamp.
//
// Layer 1 (grace deferral): otherwise, require TWO consecutive zero-external
// observations at the SAME head, at least MinCIWait apart, before concluding. The
// gate is retried every daemon poll, so a pending return is cheap and a genuinely
// CI-less repo merges exactly one grace window later.
//
// Layer 3 (require_external_ci): reached only AFTER the window above elapses with
// still-zero external CI (i.e. NOT during the creation-lag race). If the operator
// requires external CI, the empty gate hard-blocks rather than ever stamping
// gitmoot/ci — but as a MergeBlockTransient (an absent external CI is a
// repo-config/operator-policy condition, not a template-quality defect), so the
// trace-harvester never scores it as a false negative (#465).
func (g PolicyMergeGate) concludeNoExternalCI(ctx context.Context, repo github.Repository, pullRequest int64, headSHA string) error {
	shortHead := shortSHA(headSHA)
	now := g.now()

	// Persist (or read) the first zero-external observation at this head. All layers
	// measure their windows from this single persisted clock, and a new head resets
	// it — so no layer can conclude in the Actions creation-lag window and none can
	// hold pending forever.
	firstZero, err := g.recordOrLoadFirstZero(ctx, repo.FullName(), pullRequest, headSHA, now)
	if err != nil {
		return err
	}
	elapsed := now.Sub(firstZero)

	// Layer 2: workflows demonstrably exist at this head — a zero observation is
	// almost certainly an Actions creation lag, so stay pending, BOUNDED by
	// MaxCIWait. Past that bound with the head unchanged and still zero external CI,
	// the workflows genuinely never produce a check for this head, so fall through.
	if g.headHasWorkflows(ctx, repo, headSHA) {
		if elapsed < g.maxCIWait() {
			return mergePending{reason: fmt.Sprintf("repository has GitHub Actions workflows but no check run has appeared yet at head %s; waiting up to %s for CI to be created", shortHead, g.maxCIWait())}
		}
		// Bound elapsed: conclude no external CI for this head.
	} else if elapsed < g.minCIWait() {
		// Layer 1: grace deferral — no workflows detected, but wait one grace window
		// in case GitHub Actions simply has not created the run yet.
		return mergePending{reason: fmt.Sprintf("waiting to confirm no external CI at head %s; grace window has not elapsed since the first zero observation", shortHead)}
	}

	// Confident that no external CI will appear at this head (the workflow/grace
	// window elapsed with the head unchanged and still zero external observations).

	// Layer 3: operator requires external CI — never stamp the empty gate. This is
	// a repo-config/operator-policy condition, not a template defect, so it blocks
	// as MergeBlockTransient (unharvested), and only here — never during the
	// creation-lag race handled above.
	if g.RequireExternalCI {
		return mergeBlocked{
			reason: fmt.Sprintf("merge gate requires external CI but head %s still reports none after waiting for CI to appear; set [merge_gate] require_external_ci = false to allow no-CI merges, or ensure the CI workflow runs on this pull request", shortHead),
			class:  MergeBlockTransient,
		}
	}

	// Genuinely no external CI at this head: stamp the synthetic gitmoot/ci success.
	_, err = g.GitHub.CreateCommitStatus(ctx, github.CommitStatusInput{
		Repo:        repo,
		SHA:         headSHA,
		State:       "success",
		Context:     gitmootNoCIContext,
		Description: "No external CI reported",
	})
	return err
}

// recordOrLoadFirstZero persists (or reads) the first zero-external CI observation
// at headSHA and returns the effective first-zero timestamp used to measure the
// grace/max windows. A missing row, a head change, or an unreadable stored
// timestamp all (re)record `now` and return it — the fail-SAFE direction, so the
// caller measures a full window from a trustworthy clock instead of concluding
// early off a stale or corrupt observation.
func (g PolicyMergeGate) recordOrLoadFirstZero(ctx context.Context, repoFullName string, pullRequest int64, headSHA string, now time.Time) (time.Time, error) {
	obs, err := g.Store.GetNoCIObservation(ctx, repoFullName, pullRequest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	if err == nil && obs.HeadSHA == headSHA {
		if firstZero, parseErr := time.Parse(time.RFC3339Nano, obs.FirstZeroAt); parseErr == nil {
			return firstZero, nil
		}
		// Corrupt timestamp: fall through to re-record `now`.
	}
	if recordErr := g.Store.UpsertNoCIObservation(ctx, db.NoCIObservation{
		RepoFullName: repoFullName,
		PullRequest:  pullRequest,
		HeadSHA:      headSHA,
		FirstZeroAt:  now.Format(time.RFC3339Nano),
	}); recordErr != nil {
		return time.Time{}, recordErr
	}
	return now, nil
}

// headHasWorkflows reports whether the head tree carries a `.github/workflows/`
// directory (#596, layer 2). It probes the OPTIONAL workflowAwareGitHub
// capability and caches the immutable per-head result. A client without the
// capability, or a read error, returns false so the caller falls through to the
// grace path — the fail-safe direction (never an instant no-CI stamp).
func (g PolicyMergeGate) headHasWorkflows(ctx context.Context, repo github.Repository, headSHA string) bool {
	aware, ok := g.GitHub.(workflowAwareGitHub)
	if !ok {
		return false
	}
	if present, cached := lookupWorkflowPresence(repo.FullName(), headSHA); cached {
		return present
	}
	present, err := aware.WorkflowsExistAtRef(ctx, repo, headSHA)
	if err != nil {
		// Fail safe toward grace; do NOT cache a transient error.
		return false
	}
	storeWorkflowPresence(repo.FullName(), headSHA, present)
	return present
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func reviewRoundAfter(left string, right string) bool {
	leftNumber, leftOK := reviewRoundNumber(left)
	rightNumber, rightOK := reviewRoundNumber(right)
	if leftOK && rightOK {
		return leftNumber > rightNumber
	}
	if leftOK != rightOK {
		return leftOK
	}
	return left > right
}

// block records a not-ready block at the given quality classification (#465). The
// class drives the Mode-A trace-harvester AND is persisted on the merge-gate row
// (#1562): a transient/infra block is self-clearing, so an exit path outside this
// call stack must be able to tell it apart from an authoritative rejection. It
// still never changes the block transition itself. Call sites pass
// MergeBlockQuality for authoritative template-quality rejections (external CI
// failed, blocking review captured, closed without merge) and MergeBlockTransient
// for branch-staleness/infra conditions.
func (g PolicyMergeGate) block(ctx context.Context, request MergeRequest, sha string, reason string, class MergeBlockClass) (MergeDecision, error) {
	if err := g.Store.UpsertMergeGate(ctx, db.MergeGate{RepoFullName: request.Repo, PullRequest: int64(request.PullRequest), State: "blocked", Reason: reason, BlockClass: int(class)}); err != nil {
		return MergeDecision{}, err
	}
	if sha != "" {
		repo, err := parseRepoFullName(request.Repo)
		if err != nil {
			return MergeDecision{}, err
		}
		if _, err := g.GitHub.CreateCommitStatus(ctx, github.CommitStatusInput{
			Repo:        repo,
			SHA:         sha,
			State:       "failure",
			Context:     GitmootMergeGateContext,
			Description: commitStatusDescription(reason),
		}); err != nil {
			return MergeDecision{}, err
		}
	}
	return MergeDecision{Reason: PlainReason(reason), BlockClass: class}, nil
}

func (g PolicyMergeGate) pending(ctx context.Context, request MergeRequest, sha string, reason string) (MergeDecision, error) {
	if err := g.Store.UpsertMergeGate(ctx, db.MergeGate{RepoFullName: request.Repo, PullRequest: int64(request.PullRequest), State: "pending", Reason: reason}); err != nil {
		return MergeDecision{}, err
	}
	if sha != "" {
		repo, err := parseRepoFullName(request.Repo)
		if err != nil {
			return MergeDecision{}, err
		}
		// The ready_to_merge task state is the retry authority. Publishing this
		// status is observability only; a transient forge failure must not strand
		// an approved review before the daemon can re-evaluate pending CI (#1708).
		_, _ = g.GitHub.CreateCommitStatus(ctx, github.CommitStatusInput{
			Repo:        repo,
			SHA:         sha,
			State:       "pending",
			Context:     GitmootMergeGateContext,
			Description: commitStatusDescription(reason),
		})
	}
	return MergeDecision{Ready: true, Reason: PlainReason(reason)}, nil
}

func commitStatusDescription(description string) string {
	runes := []rune(description)
	if len(runes) <= commitStatusDescriptionMaxRunes {
		return description
	}
	return string(runes[:commitStatusDescriptionMaxRunes-3]) + "..."
}

func (g PolicyMergeGate) recordMerged(ctx context.Context, request MergeRequest, pr github.PullRequest, mergeSHA string) error {
	if err := g.Store.UpsertMergeGate(ctx, db.MergeGate{RepoFullName: request.Repo, PullRequest: int64(request.PullRequest), State: "merged", Reason: mergeSHA}); err != nil {
		return err
	}
	return g.Store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName:   request.Repo,
		Number:         int64(request.PullRequest),
		URL:            pr.URL,
		HeadBranch:     pr.HeadRef,
		BaseBranch:     pr.BaseRef,
		HeadSHA:        pr.HeadSHA,
		MergeCommitSHA: mergeSHA,
		State:          "merged",
	})
}

type mergeBlocked struct {
	reason string
	// class, when non-zero (MergeBlockNone), overrides the default MergeBlockQuality
	// classification the ensureStatuses handler assigns a mergeBlocked. It lets the
	// require_external_ci empty-gate block surface as MergeBlockTransient so the
	// trace-harvester does not score an absent-CI/operator-policy condition as a
	// template-quality negative (#465 INFRA-NOISE-FILTERED). Zero means "use the
	// default classification for this error site".
	class MergeBlockClass
}

type mergePending struct {
	reason string
}

func (e mergeBlocked) Error() string {
	return e.reason
}

func (e mergePending) Error() string {
	return e.reason
}

func statusPending(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "pending", "queued", "in_progress", "waiting", "requested":
		return true
	default:
		return false
	}
}

func checkPending(check github.PullRequestCheck) bool {
	bucket := strings.ToLower(strings.TrimSpace(check.Bucket))
	if bucket != "" {
		return bucket == "pending"
	}
	switch strings.ToLower(strings.TrimSpace(check.State)) {
	case "pending", "queued", "in_progress", "waiting", "requested":
		return true
	default:
		return false
	}
}

func checkPassed(check github.PullRequestCheck) bool {
	bucket := strings.ToLower(strings.TrimSpace(check.Bucket))
	if bucket != "" {
		return bucket == "pass" || bucket == "skipping"
	}
	state := strings.ToLower(strings.TrimSpace(check.State))
	return state == "success" || state == "skipped" || state == "neutral"
}

func pullRequestMerged(pr github.PullRequest) bool {
	return pr.Merged || strings.TrimSpace(pr.State) == "merged"
}

func mergeMethod(method string) string {
	method = strings.TrimSpace(method)
	if method == "" {
		return "squash"
	}
	return method
}

func mergeSubject(request MergeRequest) string {
	if strings.TrimSpace(request.TaskID) == "" {
		return "Gitmoot merge"
	}
	return "Gitmoot merge " + strings.TrimSpace(request.TaskID)
}

func parseRepoFullName(value string) (github.Repository, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return github.Repository{}, fmt.Errorf("invalid repo %q", value)
	}
	return github.Repository{Owner: owner, Name: name}, nil
}
