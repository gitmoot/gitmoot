package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

const (
	gitmootMergeGateContext         = "gitmoot/merge-gate"
	gitmootNoCIContext              = "gitmoot/ci"
	commitStatusDescriptionMaxRunes = 140
	mergeQueueLockTTL               = 30 * time.Minute
)

type MergeGateGitHub interface {
	GetPullRequest(ctx context.Context, repo github.Repository, number int64) (github.PullRequest, error)
	GetCombinedStatus(ctx context.Context, repo github.Repository, ref string) (github.CombinedStatus, error)
	CompareCommits(ctx context.Context, repo github.Repository, base string, head string) (github.CompareResult, error)
	ListPullRequestChecks(ctx context.Context, repo github.Repository, number int64) ([]github.PullRequestCheck, error)
	CreateCommitStatus(ctx context.Context, input github.CommitStatusInput) (github.CommitStatus, error)
	PostIssueComment(ctx context.Context, repo github.Repository, issueNumber int64, body string) (github.IssueComment, error)
	UpdatePullRequestBranch(ctx context.Context, input github.UpdatePullRequestBranchInput) (github.UpdatePullRequestBranchResult, error)
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
	Clock func() time.Time
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

// workflowAwareGitHub is an OPTIONAL capability the merge gate probes for on its
// GitHub client (#596, layer 2). The real *github.GhClient implements it; a
// client that does not is treated as "workflows unknown", which fails safe
// toward the grace path (never toward an instant no-CI stamp).
type workflowAwareGitHub interface {
	WorkflowsExistAtRef(ctx context.Context, repo github.Repository, ref string) (bool, error)
}

func (g PolicyMergeGate) Evaluate(ctx context.Context, request MergeRequest) (MergeDecision, error) {
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
	headSHA := strings.TrimSpace(pr.HeadSHA)
	if headSHA == "" {
		return g.block(ctx, request, "", "pull request head SHA is missing", MergeBlockTransient)
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
	if !request.ReviewOptional {
		if err := g.ensureFinalReviewCaptured(ctx, request, headSHA); err != nil {
			// A captured blocking review (mergeBlocked) is a template-quality rejection;
			// every other review error (approval missing / not yet captured / head
			// mismatch) is a transient/process condition the harvester must not score.
			class := MergeBlockTransient
			var blocked mergeBlocked
			if errors.As(err, &blocked) {
				class = MergeBlockQuality
			}
			return g.block(ctx, request, headSHA, err.Error(), class)
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
	if err := g.ensureStatuses(ctx, repo, int64(request.PullRequest), headSHA); err != nil {
		var pending mergePending
		if errors.As(err, &pending) {
			return g.pending(ctx, request, headSHA, pending.reason)
		}
		var blocked mergeBlocked
		if errors.As(err, &blocked) {
			// An external CI FAILURE is an authoritative template-quality rejection
			// (MergeBlockQuality, the default class). A require_external_ci empty-gate
			// block instead carries an explicit MergeBlockTransient class: an ABSENT
			// external CI is a repo-config/operator-policy condition, not a template
			// defect, so the trace-harvester must not score it as a false Hard=0
			// negative (#465 INFRA-NOISE-FILTERED).
			class := MergeBlockQuality
			if blocked.class != MergeBlockNone {
				class = blocked.class
			}
			return g.block(ctx, request, headSHA, blocked.reason, class)
		}
		return MergeDecision{}, err
	}

	if _, err := g.GitHub.CreateCommitStatus(ctx, github.CommitStatusInput{
		Repo:        repo,
		SHA:         headSHA,
		State:       "success",
		Context:     gitmootMergeGateContext,
		Description: "Gitmoot merge gate passed",
	}); err != nil {
		return MergeDecision{}, err
	}
	result, err := executePullRequestMerge(ctx, g.GitHub, github.MergePullRequestInput{
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
	return g.finishMerged(ctx, request, pr, strings.TrimSpace(result.SHA))
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
	return MergeDecision{Ready: true, Merged: true, MergeCommitSHA: mergeSHA, Reason: reason}, nil
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
	latest := ""
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
		round := strings.TrimSpace(payload.ReviewRound)
		if round == "" {
			round = job.ID
		}
		if latest == "" || reviewRoundAfter(round, latest) {
			latest = round
		}
	}
	if latest == "" {
		return errors.New("final agent review is not captured")
	}
	approved := false
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return err
		}
		round := strings.TrimSpace(payload.ReviewRound)
		if round == "" {
			round = job.ID
		}
		if !sameTask(current, payload) || round != latest || payload.Result == nil {
			continue
		}
		if err := g.ensureReviewMatchesHead(payload, headSHA, job.Agent); err != nil {
			return err
		}
		switch payload.Result.Decision {
		case "approved":
			approved = true
		case "changes_requested", "blocked", "failed":
			// A captured blocking review is an authoritative template-quality rejection
			// (mergeBlocked), distinct from the transient/process review errors below
			// (missing approval, not-yet-captured), so the trace-harvester scores only
			// this one as a negative (#465 INFRA-NOISE-FILTERED).
			return mergeBlocked{reason: fmt.Sprintf("latest review round has blocking result from %s", job.Agent)}
		}
	}
	if !approved {
		return errors.New("required reviewer approval is missing")
	}
	return nil
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
	// deadlocking. This is narrow: a normal review with a mismatched non-empty
	// head still fails above, and a normal review missing a head SHA but lacking
	// the integration-worktree markers still fails below.
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
			if item.Context == gitmootMergeGateContext {
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

	checks, err := g.GitHub.ListPullRequestChecks(ctx, repo, pullRequest)
	if err != nil {
		return 0, err
	}
	externalCheckCount := 0
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		if name == gitmootMergeGateContext {
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
// class is advisory metadata for the Mode-A trace-harvester only; it never changes
// the block transition itself. Call sites pass MergeBlockQuality for authoritative
// template-quality rejections (external CI failed, blocking review captured, closed
// without merge) and MergeBlockTransient for branch-staleness/infra conditions.
func (g PolicyMergeGate) block(ctx context.Context, request MergeRequest, sha string, reason string, class MergeBlockClass) (MergeDecision, error) {
	if err := g.Store.UpsertMergeGate(ctx, db.MergeGate{RepoFullName: request.Repo, PullRequest: int64(request.PullRequest), State: "blocked", Reason: reason}); err != nil {
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
			Context:     gitmootMergeGateContext,
			Description: commitStatusDescription(reason),
		}); err != nil {
			return MergeDecision{}, err
		}
	}
	return MergeDecision{Reason: reason, BlockClass: class}, nil
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
		if _, err := g.GitHub.CreateCommitStatus(ctx, github.CommitStatusInput{
			Repo:        repo,
			SHA:         sha,
			State:       "pending",
			Context:     gitmootMergeGateContext,
			Description: commitStatusDescription(reason),
		}); err != nil {
			return MergeDecision{}, err
		}
	}
	return MergeDecision{Ready: true, Reason: reason}, nil
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
