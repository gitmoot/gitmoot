package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

const (
	gitmootMergeGateContext         = "gitmoot/merge-gate"
	gitmootNoCIContext              = "gitmoot/ci"
	commitStatusDescriptionMaxRunes = 140
)

type MergeGateGitHub interface {
	GetPullRequest(ctx context.Context, repo github.Repository, number int64) (github.PullRequest, error)
	GetCombinedStatus(ctx context.Context, repo github.Repository, ref string) (github.CombinedStatus, error)
	CompareCommits(ctx context.Context, repo github.Repository, base string, head string) (github.CompareResult, error)
	ListPullRequestChecks(ctx context.Context, repo github.Repository, number int64) ([]github.PullRequestCheck, error)
	CreateCommitStatus(ctx context.Context, input github.CommitStatusInput) (github.CommitStatus, error)
	MergePullRequest(ctx context.Context, input github.MergePullRequestInput) (github.MergeResult, error)
}

type MergeGateGit interface {
	WorktreeClean(ctx context.Context) (bool, error)
	UpdateBase(ctx context.Context, remote string, branch string) error
}

type NextTaskEnqueuer interface {
	EnqueueNextTask(ctx context.Context, completedTaskID string) error
}

type PolicyMergeGate struct {
	Store        *db.Store
	GitHub       MergeGateGitHub
	Git          MergeGateGit
	NextTasks    NextTaskEnqueuer
	DeleteBranch bool
	MergeMethod  string
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
		return g.block(ctx, request, "", "pull request head SHA is missing")
	}
	if pullRequestMerged(pr) {
		return g.finishMerged(ctx, request, pr, strings.TrimSpace(pr.MergeSHA))
	}
	if strings.TrimSpace(pr.State) == "closed" {
		return g.block(ctx, request, headSHA, "pull request is closed without being merged")
	}
	if g.Git != nil {
		clean, err := g.Git.WorktreeClean(ctx)
		if err != nil {
			return MergeDecision{}, err
		}
		if !clean {
			return g.block(ctx, request, headSHA, "local worktree is not clean")
		}
	}
	if pr.Mergeable != nil && !*pr.Mergeable {
		return g.block(ctx, request, headSHA, "pull request is not mergeable; rebase or update the branch")
	}
	if err := g.ensureBranchFresh(ctx, repo, pr, headSHA); err != nil {
		return g.block(ctx, request, headSHA, err.Error())
	}
	if !request.ReviewOptional {
		if err := g.ensureFinalReviewCaptured(ctx, request, headSHA); err != nil {
			return g.block(ctx, request, headSHA, err.Error())
		}
	}
	if err := g.ensureStatuses(ctx, repo, int64(request.PullRequest), headSHA); err != nil {
		var pending mergePending
		if errors.As(err, &pending) {
			return g.pending(ctx, request, headSHA, pending.reason)
		}
		var blocked mergeBlocked
		if errors.As(err, &blocked) {
			return g.block(ctx, request, headSHA, blocked.reason)
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
	result, err := g.GitHub.MergePullRequest(ctx, github.MergePullRequestInput{
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

func (g PolicyMergeGate) finishMerged(ctx context.Context, request MergeRequest, pr github.PullRequest, mergeSHA string) (MergeDecision, error) {
	if err := g.recordMerged(ctx, request, pr, mergeSHA); err != nil {
		return MergeDecision{}, err
	}
	postMergeWarnings := []string{}
	lock, err := g.Store.GetBranchLock(ctx, request.Repo, pr.HeadRef)
	if err == nil {
		if _, err := g.Store.ReleaseLock(ctx, lock); err != nil {
			postMergeWarnings = append(postMergeWarnings, "release branch lock: "+err.Error())
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		postMergeWarnings = append(postMergeWarnings, "load branch lock: "+err.Error())
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
			return fmt.Errorf("latest review round has blocking result from %s", job.Agent)
		}
	}
	if !approved {
		return errors.New("required reviewer approval is missing")
	}
	return nil
}

func (g PolicyMergeGate) ensureBranchFresh(ctx context.Context, repo github.Repository, pr github.PullRequest, headSHA string) error {
	base := strings.TrimSpace(pr.BaseRef)
	if base == "" {
		base = strings.TrimSpace(pr.BaseSHA)
	}
	if base == "" {
		return errors.New("pull request base ref is missing")
	}
	compare, err := g.GitHub.CompareCommits(ctx, repo, base, headSHA)
	if err != nil {
		return err
	}
	status := strings.ToLower(strings.TrimSpace(compare.Status))
	if compare.BehindBy > 0 || status == "behind" || status == "diverged" {
		return fmt.Errorf("pull request branch is behind base %s; update the branch before merging", base)
	}
	if status != "" && status != "ahead" && status != "identical" {
		return fmt.Errorf("pull request branch freshness is unknown: compare status %q", compare.Status)
	}
	return nil
}

func (g PolicyMergeGate) ensureReviewMatchesHead(payload JobPayload, headSHA string, agent string) error {
	reviewHead := strings.TrimSpace(payload.HeadSHA)
	if reviewHead == headSHA {
		return nil
	}
	if reviewHead != "" {
		return fmt.Errorf("latest review from %s is for a different head SHA", agent)
	}
	return fmt.Errorf("latest review from %s does not record a head SHA; rerun review", agent)
}

func (g PolicyMergeGate) ensureStatuses(ctx context.Context, repo github.Repository, pullRequest int64, headSHA string) error {
	status, err := g.GitHub.GetCombinedStatus(ctx, repo, headSHA)
	if err != nil {
		return err
	}
	externalStatusCount := 0
	for _, item := range status.Statuses {
		if strings.HasPrefix(item.Context, "gitmoot/") {
			if item.Context == gitmootMergeGateContext {
				continue
			}
			if statusPending(item.State) {
				return mergePending{reason: fmt.Sprintf("gitmoot status %q is pending", item.Context)}
			}
			if item.State != "success" {
				return mergeBlocked{reason: fmt.Sprintf("gitmoot status %q is %s", item.Context, item.State)}
			}
			continue
		}
		externalStatusCount++
		if statusPending(item.State) {
			return mergePending{reason: "external commit status " + item.Context + " is pending"}
		}
		if item.State != "success" {
			return mergeBlocked{reason: "external commit status " + item.Context + " is not successful"}
		}
	}

	checks, err := g.GitHub.ListPullRequestChecks(ctx, repo, pullRequest)
	if err != nil {
		return err
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
			return mergePending{reason: fmt.Sprintf("external CI check %q is pending", name)}
		}
		if !checkPassed(check) {
			if name == "" {
				name = "unnamed check"
			}
			return mergeBlocked{reason: fmt.Sprintf("external CI check %q is not successful", name)}
		}
	}
	if externalCheckCount == 0 && externalStatusCount == 0 {
		_, err := g.GitHub.CreateCommitStatus(ctx, github.CommitStatusInput{
			Repo:        repo,
			SHA:         headSHA,
			State:       "success",
			Context:     gitmootNoCIContext,
			Description: "No external CI reported",
		})
		return err
	}
	return nil
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

func (g PolicyMergeGate) block(ctx context.Context, request MergeRequest, sha string, reason string) (MergeDecision, error) {
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
	return MergeDecision{Reason: reason}, nil
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
