// Package githubtest provides test doubles for the github package.
package githubtest

import (
	"context"
	"errors"
	"time"

	"github.com/gitmoot/gitmoot/internal/github"
)

type NoopClient struct{}

func (NoopClient) Ping(context.Context) error {
	return nil
}

func (NoopClient) RepositoryExists(context.Context, github.Repository) (bool, error) {
	return true, nil
}

func (NoopClient) CreateRepository(context.Context, github.Repository, bool) error {
	return nil
}

func (NoopClient) CloneRepository(context.Context, github.Repository, string) error {
	return nil
}

func (NoopClient) ListUserRepositories(context.Context, int) ([]github.RepoSummary, error) {
	return nil, nil
}

func (NoopClient) DeleteRepository(context.Context, github.Repository) error {
	return nil
}

func (NoopClient) Preflight(context.Context, github.Repository) error {
	return nil
}

func (NoopClient) ListPullRequests(context.Context, github.Repository, string) ([]github.PullRequest, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) ListRecentClosedPullRequests(context.Context, github.Repository) ([]github.PullRequest, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) ListIssues(context.Context, github.Repository, string) ([]github.Issue, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	return github.PullRequest{}, errors.ErrUnsupported
}

func (NoopClient) GetOpenPullRequestByHead(context.Context, github.Repository, string, string) (github.PullRequest, bool, error) {
	return github.PullRequest{}, false, errors.ErrUnsupported
}

func (NoopClient) CreatePullRequest(context.Context, github.CreatePullRequestInput) (github.PullRequest, error) {
	return github.PullRequest{}, errors.ErrUnsupported
}

func (NoopClient) EnsurePullRequest(context.Context, github.CreatePullRequestInput) (github.PullRequest, error) {
	return github.PullRequest{}, errors.ErrUnsupported
}

func (NoopClient) SearchOpenIssues(context.Context, github.Repository, string) ([]github.Issue, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) CreateIssue(context.Context, github.CreateIssueInput) (github.Issue, error) {
	return github.Issue{}, errors.ErrUnsupported
}

func (NoopClient) CloseIssue(context.Context, github.Repository, int64) (github.Issue, error) {
	return github.Issue{}, errors.ErrUnsupported
}

func (NoopClient) ListIssueComments(context.Context, github.Repository, int64) ([]github.IssueComment, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) ListRepoIssueComments(context.Context, github.Repository, time.Time) ([]github.IssueComment, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) PostIssueComment(context.Context, github.Repository, int64, string) (github.IssueComment, error) {
	return github.IssueComment{}, errors.ErrUnsupported
}

func (NoopClient) GetUserPermission(context.Context, github.Repository, string) (github.UserPermission, error) {
	return github.UserPermission{}, errors.ErrUnsupported
}

func (NoopClient) MergePullRequest(context.Context, github.MergePullRequestInput) (github.MergeResult, error) {
	return github.MergeResult{}, errors.ErrUnsupported
}

func (NoopClient) UpdatePullRequestBranch(context.Context, github.UpdatePullRequestBranchInput) (github.UpdatePullRequestBranchResult, error) {
	return github.UpdatePullRequestBranchResult{}, errors.ErrUnsupported
}

func (NoopClient) GetCombinedStatus(context.Context, github.Repository, string) (github.CombinedStatus, error) {
	return github.CombinedStatus{}, errors.ErrUnsupported
}

func (NoopClient) ListCheckRunsForRef(context.Context, github.Repository, string) ([]github.PullRequestCheck, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) CompareCommits(context.Context, github.Repository, string, string) (github.CompareResult, error) {
	return github.CompareResult{}, errors.ErrUnsupported
}

func (NoopClient) ListPullRequestChecks(context.Context, github.Repository, int64) ([]github.PullRequestCheck, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) CreateCommitStatus(context.Context, github.CommitStatusInput) (github.CommitStatus, error) {
	return github.CommitStatus{}, errors.ErrUnsupported
}

func (NoopClient) ListPullRequestFiles(context.Context, github.Repository, int64) ([]github.PullRequestFile, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) ListPullRequestCommits(context.Context, github.Repository, int64) ([]github.PullRequestCommit, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) UpsertFile(context.Context, github.UpsertFileInput) (github.RepositoryFile, error) {
	return github.RepositoryFile{}, errors.ErrUnsupported
}
