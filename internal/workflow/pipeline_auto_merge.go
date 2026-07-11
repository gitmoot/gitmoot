package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

// PipelineAutoMergeRequest identifies the immutable PR head a pipeline gate is
// authorized to merge. MatchHeadCommit is enforced again by GitHub at write time.
type PipelineAutoMergeRequest struct {
	Repo        string
	PullRequest int
	HeadSHA     string
	Pipeline    string
	RunID       string
	StageID     string
}

// PipelineAutoMergeReadiness is one non-mutating GitHub observation. Ready means
// mergeability and checks are green; Waiting is a transient not-yet-green state;
// Blocked is terminal for this pipeline run. Merged is idempotent success.
type PipelineAutoMergeReadiness struct {
	Ready          bool
	Waiting        bool
	Blocked        bool
	Merged         bool
	CurrentHeadSHA string
	MergeCommitSHA string
	Reason         string
}

// PipelineAutoMergeResult is the result of the single audited merge attempt.
type PipelineAutoMergeResult struct {
	Merged         bool
	MergeCommitSHA string
	Reason         string
}

// PipelineAutoMerger adapts the existing policy merge-gate checks and the shared
// low-level merge call for the jobless pipeline gate. Review approval is checked
// by the pipeline advancer before this type is invoked.
type PipelineAutoMerger struct {
	Store             *db.Store
	GitHub            MergeGateGitHub
	RequireExternalCI bool
	MinCIWait         time.Duration
	MaxCIWait         time.Duration
	Clock             func() time.Time
}

// Evaluate performs no merge. It reads the current PR, enforces the stamped head,
// mergeability, and the same status/check policy used by PolicyMergeGate.
func (m PipelineAutoMerger) Evaluate(ctx context.Context, request PipelineAutoMergeRequest) (PipelineAutoMergeReadiness, error) {
	repo, err := parseRepoFullName(request.Repo)
	if err != nil {
		return PipelineAutoMergeReadiness{}, err
	}
	if request.PullRequest <= 0 {
		return PipelineAutoMergeReadiness{}, fmt.Errorf("pipeline auto-merge pull request number is required")
	}
	if m.Store == nil || m.GitHub == nil {
		return PipelineAutoMergeReadiness{}, fmt.Errorf("pipeline auto-merge executor is not configured")
	}
	pr, err := m.GitHub.GetPullRequest(ctx, repo, int64(request.PullRequest))
	if err != nil {
		return PipelineAutoMergeReadiness{}, err
	}
	head := strings.TrimSpace(pr.HeadSHA)
	readiness := PipelineAutoMergeReadiness{CurrentHeadSHA: head}
	if pullRequestMerged(pr) {
		readiness.Merged = true
		readiness.MergeCommitSHA = strings.TrimSpace(pr.MergeSHA)
		readiness.Reason = "pull request is already merged"
		return readiness, nil
	}
	if strings.EqualFold(strings.TrimSpace(pr.State), "closed") {
		readiness.Blocked = true
		readiness.Reason = "pull request is closed without being merged"
		return readiness, nil
	}
	if head == "" {
		readiness.Blocked = true
		readiness.Reason = "pull request head SHA is missing"
		return readiness, nil
	}
	if want := strings.TrimSpace(request.HeadSHA); head != want {
		readiness.Blocked = true
		readiness.Reason = fmt.Sprintf("pull request head drifted after review: reviewed %s, current %s", shortSHA(want), shortSHA(head))
		return readiness, nil
	}
	if pr.Mergeable == nil {
		readiness.Waiting = true
		readiness.Reason = "GitHub has not determined pull request mergeability yet"
		return readiness, nil
	}
	if !*pr.Mergeable {
		readiness.Blocked = true
		readiness.Reason = "pull request is not mergeable; rebase or update the branch"
		return readiness, nil
	}
	gate := PolicyMergeGate{
		Store:             m.Store,
		GitHub:            m.GitHub,
		RequireExternalCI: m.RequireExternalCI,
		MinCIWait:         m.MinCIWait,
		MaxCIWait:         m.MaxCIWait,
		Clock:             m.Clock,
	}
	externalCount, statusErr := gate.evaluateStatuses(ctx, repo, int64(request.PullRequest), head)
	if statusErr != nil {
		var pending mergePending
		if errors.As(statusErr, &pending) {
			readiness.Waiting = true
			readiness.Reason = pending.reason
			return readiness, nil
		}
		var blocked mergeBlocked
		if errors.As(statusErr, &blocked) {
			readiness.Blocked = true
			readiness.Reason = blocked.reason
			return readiness, nil
		}
		return PipelineAutoMergeReadiness{}, statusErr
	}
	if externalCount == 0 {
		readiness.Blocked = true
		readiness.Reason = fmt.Sprintf("pipeline auto-merge requires at least one external CI status or check at head %s; zero external checks reported", shortSHA(head))
		return readiness, nil
	}
	readiness.Ready = true
	readiness.Reason = "pull request is mergeable and checks passed"
	return readiness, nil
}

// Merge executes exactly one squash attempt against the reviewed head. Callers
// must record their auditable intent event before entering this method.
func (m PipelineAutoMerger) Merge(ctx context.Context, request PipelineAutoMergeRequest) (PipelineAutoMergeResult, error) {
	repo, err := parseRepoFullName(request.Repo)
	if err != nil {
		return PipelineAutoMergeResult{}, err
	}
	result, err := executePullRequestMerge(ctx, m.GitHub, github.MergePullRequestInput{
		Repo:            repo,
		Number:          int64(request.PullRequest),
		Method:          "squash",
		Subject:         fmt.Sprintf("Gitmoot pipeline %s run %s", request.Pipeline, request.RunID),
		Body:            fmt.Sprintf("Merged by Gitmoot pipeline gate %s after source-bound review approval and checks passed.", request.StageID),
		MatchHeadCommit: strings.TrimSpace(request.HeadSHA),
	})
	if err != nil {
		return PipelineAutoMergeResult{}, err
	}
	return PipelineAutoMergeResult{Merged: result.Merged, MergeCommitSHA: strings.TrimSpace(result.SHA), Reason: strings.TrimSpace(result.Message)}, nil
}
