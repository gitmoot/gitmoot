package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

type Client interface {
	Ping(ctx context.Context) error
	ListPullRequests(ctx context.Context, repo Repository, state string) ([]PullRequest, error)
	GetPullRequest(ctx context.Context, repo Repository, number int64) (PullRequest, error)
	CreatePullRequest(ctx context.Context, input CreatePullRequestInput) (PullRequest, error)
	CreateIssue(ctx context.Context, input CreateIssueInput) (Issue, error)
	ListIssueComments(ctx context.Context, repo Repository, issueNumber int64) ([]IssueComment, error)
	PostIssueComment(ctx context.Context, repo Repository, issueNumber int64, body string) (IssueComment, error)
	GetUserPermission(ctx context.Context, repo Repository, username string) (UserPermission, error)
	MergePullRequest(ctx context.Context, input MergePullRequestInput) (MergeResult, error)
	GetCombinedStatus(ctx context.Context, repo Repository, ref string) (CombinedStatus, error)
	CompareCommits(ctx context.Context, repo Repository, base string, head string) (CompareResult, error)
	ListPullRequestChecks(ctx context.Context, repo Repository, number int64) ([]PullRequestCheck, error)
	CreateCommitStatus(ctx context.Context, input CommitStatusInput) (CommitStatus, error)
	ListPullRequestFiles(ctx context.Context, repo Repository, number int64) ([]PullRequestFile, error)
	ListPullRequestCommits(ctx context.Context, repo Repository, number int64) ([]PullRequestCommit, error)
}

type Repository struct {
	Owner string
	Name  string
}

func (r Repository) FullName() string {
	if r.Owner == "" || r.Name == "" {
		return ""
	}
	return r.Owner + "/" + r.Name
}

type PullRequest struct {
	Number    int64  `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	URL       string `json:"html_url"`
	Merged    bool   `json:"merged"`
	HeadRef   string
	BaseRef   string
	BaseSHA   string
	HeadSHA   string
	MergeSHA  string
	Mergeable *bool `json:"mergeable"`
}

func (p *PullRequest) UnmarshalJSON(data []byte) error {
	type wire struct {
		Number    int64  `json:"number"`
		Title     string `json:"title"`
		State     string `json:"state"`
		URL       string `json:"html_url"`
		Merged    bool   `json:"merged"`
		Mergeable *bool  `json:"mergeable"`
		MergeSHA  string `json:"merge_commit_sha"`
		Head      struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"base"`
	}
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	p.Number = decoded.Number
	p.Title = decoded.Title
	p.State = decoded.State
	p.URL = decoded.URL
	p.Merged = decoded.Merged
	p.Mergeable = decoded.Mergeable
	p.HeadRef = decoded.Head.Ref
	p.HeadSHA = decoded.Head.SHA
	p.MergeSHA = decoded.MergeSHA
	p.BaseRef = decoded.Base.Ref
	p.BaseSHA = decoded.Base.SHA
	return nil
}

type IssueComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	URL       string `json:"html_url"`
	Author    string
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type UserPermission struct {
	Permission string `json:"permission"`
	RoleName   string `json:"role_name"`
}

func (c *IssueComment) UnmarshalJSON(data []byte) error {
	type wire struct {
		ID        int64  `json:"id"`
		Body      string `json:"body"`
		URL       string `json:"html_url"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	c.ID = decoded.ID
	c.Body = decoded.Body
	c.URL = decoded.URL
	c.Author = decoded.User.Login
	c.CreatedAt = decoded.CreatedAt
	c.UpdatedAt = decoded.UpdatedAt
	return nil
}

type Issue struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	URL    string `json:"html_url"`
}

type CreateIssueInput struct {
	Repo  Repository
	Title string
	Body  string
}

type CreatePullRequestInput struct {
	Repo  Repository
	Title string
	Body  string
	Head  string
	Base  string
	Draft bool
}

type MergePullRequestInput struct {
	Repo            Repository
	Number          int64
	Method          string
	Subject         string
	Body            string
	MatchHeadCommit string
	DeleteBranch    bool
}

type MergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

type CombinedStatus struct {
	State    string         `json:"state"`
	Statuses []CommitStatus `json:"statuses"`
}

type CompareResult struct {
	Status   string `json:"status"`
	AheadBy  int    `json:"ahead_by"`
	BehindBy int    `json:"behind_by"`
}

type CommitStatusInput struct {
	Repo        Repository
	SHA         string
	State       string
	Context     string
	Description string
	TargetURL   string
}

type CommitStatus struct {
	ID          int64  `json:"id"`
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
	URL         string `json:"url"`
}

type PullRequestCheck struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Bucket      string `json:"bucket"`
	Link        string `json:"link"`
	Workflow    string `json:"workflow"`
	CompletedAt string `json:"completedAt"`
}

type PullRequestFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"`
	SHA      string `json:"sha"`
	Patch    string `json:"patch"`
}

type PullRequestCommit struct {
	SHA string `json:"sha"`
}

type GhClient struct {
	Runner     subprocess.Runner
	Dir        string
	Sleep      func(context.Context, time.Duration) error
	MaxRetries int

	mutateMu sync.Mutex
}

func NewClient(dir string) *GhClient {
	return &GhClient{Dir: dir}
}

func (c *GhClient) Ping(ctx context.Context) error {
	_, err := c.run(ctx, false, "repo", "view", "--json", "nameWithOwner")
	return err
}

func (c *GhClient) ListPullRequests(ctx context.Context, repo Repository, state string) ([]PullRequest, error) {
	if state == "" {
		state = "open"
	}
	return apiPaginatedJSON[PullRequest](ctx, c, "-X", "GET", endpoint(repo, "pulls"), "-f", "state="+state)
}

func (c *GhClient) GetPullRequest(ctx context.Context, repo Repository, number int64) (PullRequest, error) {
	return c.getPullRequest(ctx, repo, number)
}

func (c *GhClient) CreatePullRequest(ctx context.Context, input CreatePullRequestInput) (PullRequest, error) {
	args := []string{"pr", "create", "--repo", input.Repo.FullName(), "--title", input.Title, "--body", input.Body, "--head", input.Head, "--base", input.Base}
	if input.Draft {
		args = append(args, "--draft")
	}
	result, err := c.run(ctx, true, args...)
	if err != nil {
		return PullRequest{}, err
	}
	ref, err := ParsePullRequestURL(firstLine(result.Stdout))
	if err != nil {
		return PullRequest{}, err
	}
	pr, err := c.getPullRequest(ctx, input.Repo, int64(ref.Number))
	if err != nil {
		return PullRequest{}, err
	}
	return pr, nil
}

func (c *GhClient) ListIssueComments(ctx context.Context, repo Repository, issueNumber int64) ([]IssueComment, error) {
	comments, err := apiPaginatedJSON[IssueComment](ctx, c, endpoint(repo, "issues", issueNumber, "comments"))
	return dedupeComments(comments), err
}

func (c *GhClient) PostIssueComment(ctx context.Context, repo Repository, issueNumber int64, body string) (IssueComment, error) {
	var comment IssueComment
	err := c.apiJSON(ctx, true, &comment, endpoint(repo, "issues", issueNumber, "comments"), "-f", "body="+body)
	return comment, err
}

func (c *GhClient) CreateIssue(ctx context.Context, input CreateIssueInput) (Issue, error) {
	var issue Issue
	err := c.apiJSON(ctx, true, &issue,
		endpoint(input.Repo, "issues"),
		"-f", "title="+input.Title,
		"-f", "body="+input.Body)
	return issue, err
}

func (c *GhClient) GetUserPermission(ctx context.Context, repo Repository, username string) (UserPermission, error) {
	var permission UserPermission
	result, err := c.run(ctx, false, "api", endpoint(repo, "collaborators", username, "permission"))
	if err != nil {
		if isNotFound(result) {
			return UserPermission{Permission: "none"}, nil
		}
		return UserPermission{}, err
	}
	if err := json.Unmarshal([]byte(result.Stdout), &permission); err != nil {
		return UserPermission{}, fmt.Errorf("decode gh api response: %w", err)
	}
	return permission, nil
}

func (c *GhClient) MergePullRequest(ctx context.Context, input MergePullRequestInput) (MergeResult, error) {
	method := input.Method
	if method == "" {
		method = "squash"
	}
	args := []string{"pr", "merge", strconv.FormatInt(input.Number, 10), "--repo", input.Repo.FullName()}
	switch method {
	case "merge":
		args = append(args, "--merge")
	case "rebase":
		args = append(args, "--rebase")
	case "squash":
		args = append(args, "--squash")
	default:
		return MergeResult{}, fmt.Errorf("unsupported merge method: %s", method)
	}
	if input.Subject != "" {
		args = append(args, "--subject", input.Subject)
	}
	if input.Body != "" {
		args = append(args, "--body", input.Body)
	}
	if input.MatchHeadCommit != "" {
		args = append(args, "--match-head-commit", input.MatchHeadCommit)
	}
	if input.DeleteBranch {
		args = append(args, "--delete-branch")
	}
	if _, err := c.run(ctx, true, args...); err != nil {
		return MergeResult{}, err
	}
	pr, err := c.getPullRequest(ctx, input.Repo, input.Number)
	if err != nil {
		return MergeResult{}, fmt.Errorf("fetch merged pull request: %w", err)
	}
	if !pr.Merged && strings.TrimSpace(pr.State) != "merged" {
		return MergeResult{Message: fmt.Sprintf("pull request merge is pending; current state is %q", pr.State)}, nil
	}
	return MergeResult{SHA: pr.MergeSHA, Merged: true}, nil
}

func (c *GhClient) GetCombinedStatus(ctx context.Context, repo Repository, ref string) (CombinedStatus, error) {
	var status CombinedStatus
	err := c.apiJSON(ctx, false, &status, endpoint(repo, "commits", ref, "status"))
	return status, err
}

func (c *GhClient) CompareCommits(ctx context.Context, repo Repository, base string, head string) (CompareResult, error) {
	var result CompareResult
	err := c.apiJSON(ctx, false, &result, endpoint(repo, "compare", url.PathEscape(base+"..."+head)))
	return result, err
}

func (c *GhClient) ListPullRequestChecks(ctx context.Context, repo Repository, number int64) ([]PullRequestCheck, error) {
	result, err := c.run(ctx, false,
		"pr", "checks", strconv.FormatInt(number, 10),
		"--repo", repo.FullName(),
		"--json", "name,state,bucket,link,workflow,completedAt",
	)
	if err != nil && isUnsupportedJSONFlag(result) {
		return c.listPullRequestChecksViaView(ctx, repo, number)
	}
	if err != nil && strings.TrimSpace(result.Stdout) == "" {
		if isNoChecks(result) {
			return nil, nil
		}
		return nil, err
	}
	var checks []PullRequestCheck
	if decodeErr := json.Unmarshal([]byte(result.Stdout), &checks); decodeErr != nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("decode gh pr checks response: %w", decodeErr)
	}
	return checks, nil
}

func (c *GhClient) listPullRequestChecksViaView(ctx context.Context, repo Repository, number int64) ([]PullRequestCheck, error) {
	result, err := c.run(ctx, false,
		"pr", "view", strconv.FormatInt(number, 10),
		"--repo", repo.FullName(),
		"--json", "statusCheckRollup",
	)
	if err != nil {
		return nil, err
	}
	var response struct {
		StatusCheckRollup []statusCheckRollupItem `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return nil, fmt.Errorf("decode gh pr view statusCheckRollup response: %w", err)
	}
	checks := make([]PullRequestCheck, 0, len(response.StatusCheckRollup))
	for _, item := range response.StatusCheckRollup {
		checks = append(checks, item.pullRequestCheck())
	}
	return checks, nil
}

type statusCheckRollupItem struct {
	Name         string `json:"name"`
	Context      string `json:"context"`
	State        string `json:"state"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	DetailsURL   string `json:"detailsUrl"`
	TargetURL    string `json:"targetUrl"`
	WorkflowName string `json:"workflowName"`
}

func (i statusCheckRollupItem) pullRequestCheck() PullRequestCheck {
	state := firstNonEmpty(i.State, i.Conclusion, i.Status)
	return PullRequestCheck{
		Name:     firstNonEmpty(i.Name, i.Context),
		State:    state,
		Bucket:   checkBucket(i),
		Link:     firstNonEmpty(i.DetailsURL, i.TargetURL),
		Workflow: i.WorkflowName,
	}
}

func checkBucket(item statusCheckRollupItem) string {
	state := strings.ToLower(strings.TrimSpace(item.State))
	if state != "" {
		switch state {
		case "success":
			return "pass"
		case "expected", "pending":
			return "pending"
		case "failure", "error":
			return "fail"
		}
	}
	status := strings.ToLower(strings.TrimSpace(item.Status))
	if status != "" && status != "completed" {
		return "pending"
	}
	switch strings.ToLower(strings.TrimSpace(item.Conclusion)) {
	case "success", "neutral":
		return "pass"
	case "skipped":
		return "skipping"
	case "failure", "cancelled", "timed_out", "action_required":
		return "fail"
	case "":
		if status == "completed" {
			return "pass"
		}
	}
	return ""
}

func (c *GhClient) CreateCommitStatus(ctx context.Context, input CommitStatusInput) (CommitStatus, error) {
	var status CommitStatus
	args := []string{
		endpoint(input.Repo, "statuses", input.SHA),
		"-f", "state=" + input.State,
		"-f", "context=" + input.Context,
	}
	if input.Description != "" {
		args = append(args, "-f", "description="+input.Description)
	}
	if input.TargetURL != "" {
		args = append(args, "-f", "target_url="+input.TargetURL)
	}
	err := c.apiJSON(ctx, true, &status, args...)
	return status, err
}

func (c *GhClient) ListPullRequestFiles(ctx context.Context, repo Repository, number int64) ([]PullRequestFile, error) {
	return apiPaginatedJSON[PullRequestFile](ctx, c, endpoint(repo, "pulls", number, "files"))
}

func (c *GhClient) ListPullRequestCommits(ctx context.Context, repo Repository, number int64) ([]PullRequestCommit, error) {
	return apiPaginatedJSON[PullRequestCommit](ctx, c, endpoint(repo, "pulls", number, "commits"))
}

func (c *GhClient) getPullRequest(ctx context.Context, repo Repository, number int64) (PullRequest, error) {
	var pr PullRequest
	err := c.apiJSON(ctx, false, &pr, endpoint(repo, "pulls", number))
	return pr, err
}

func (c *GhClient) apiJSON(ctx context.Context, mutate bool, output any, args ...string) error {
	result, err := c.run(ctx, mutate, append([]string{"api"}, args...)...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(result.Stdout), output); err != nil {
		return fmt.Errorf("decode gh api response: %w", err)
	}
	return nil
}

func apiPaginatedJSON[T any](ctx context.Context, c *GhClient, args ...string) ([]T, error) {
	result, err := c.run(ctx, false, append([]string{"api", "--paginate"}, args...)...)
	if err != nil {
		return nil, err
	}
	values, err := decodePaginatedJSON[T](result.Stdout)
	if err != nil {
		return nil, fmt.Errorf("decode paginated gh api response: %w", err)
	}
	return values, nil
}

func decodePaginatedJSON[T any](output string) ([]T, error) {
	decoder := json.NewDecoder(strings.NewReader(output))
	var values []T
	for {
		var page []T
		if err := decoder.Decode(&page); err != nil {
			if errors.Is(err, io.EOF) {
				return values, nil
			}
			return nil, err
		}
		values = append(values, page...)
	}
}

func (c *GhClient) run(ctx context.Context, mutate bool, args ...string) (subprocess.Result, error) {
	if mutate {
		c.mutateMu.Lock()
		defer c.mutateMu.Unlock()
	}

	runner := c.Runner
	if runner == nil {
		runner = subprocess.ExecRunner{}
	}
	retries := c.MaxRetries
	if retries == 0 {
		retries = 2
	}
	var result subprocess.Result
	var err error
	for attempt := 0; attempt <= retries; attempt++ {
		result, err = runner.Run(ctx, c.Dir, "gh", args...)
		if err == nil || !isRateLimit(result) || attempt == retries {
			break
		}
		if sleepErr := c.sleep(ctx, time.Duration(attempt+1)*time.Second); sleepErr != nil {
			return result, sleepErr
		}
	}
	if err != nil {
		return result, commandError(result, err)
	}
	return result, nil
}

func (c *GhClient) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func commandError(result subprocess.Result, err error) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		return err
	}
	return fmt.Errorf("%s: %w", detail, err)
}

func isRateLimit(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "rate limit") || strings.Contains(text, "retry-after") || strings.Contains(text, "http 429")
}

func isNotFound(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "http 404") || strings.Contains(text, "not found")
}

func isNoChecks(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "no checks reported")
}

func isUnsupportedJSONFlag(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "unknown flag: --json") || strings.Contains(text, "unknown shorthand flag") && strings.Contains(text, "json")
}

func endpoint(repo Repository, parts ...any) string {
	values := []string{"repos", repo.Owner, repo.Name}
	for _, part := range parts {
		values = append(values, fmt.Sprint(part))
	}
	return strings.Join(values, "/")
}

func dedupeComments(comments []IssueComment) []IssueComment {
	seen := make(map[int64]struct{}, len(comments))
	unique := make([]IssueComment, 0, len(comments))
	for _, comment := range comments {
		if _, ok := seen[comment.ID]; ok {
			continue
		}
		seen[comment.ID] = struct{}{}
		unique = append(unique, comment)
	}
	return unique
}

func firstLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

type NoopClient struct{}

func (NoopClient) Ping(context.Context) error {
	return nil
}

func (NoopClient) ListPullRequests(context.Context, Repository, string) ([]PullRequest, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) GetPullRequest(context.Context, Repository, int64) (PullRequest, error) {
	return PullRequest{}, errors.ErrUnsupported
}

func (NoopClient) CreatePullRequest(context.Context, CreatePullRequestInput) (PullRequest, error) {
	return PullRequest{}, errors.ErrUnsupported
}

func (NoopClient) CreateIssue(context.Context, CreateIssueInput) (Issue, error) {
	return Issue{}, errors.ErrUnsupported
}

func (NoopClient) ListIssueComments(context.Context, Repository, int64) ([]IssueComment, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) PostIssueComment(context.Context, Repository, int64, string) (IssueComment, error) {
	return IssueComment{}, errors.ErrUnsupported
}

func (NoopClient) GetUserPermission(context.Context, Repository, string) (UserPermission, error) {
	return UserPermission{}, errors.ErrUnsupported
}

func (NoopClient) MergePullRequest(context.Context, MergePullRequestInput) (MergeResult, error) {
	return MergeResult{}, errors.ErrUnsupported
}

func (NoopClient) GetCombinedStatus(context.Context, Repository, string) (CombinedStatus, error) {
	return CombinedStatus{}, errors.ErrUnsupported
}

func (NoopClient) CompareCommits(context.Context, Repository, string, string) (CompareResult, error) {
	return CompareResult{}, errors.ErrUnsupported
}

func (NoopClient) ListPullRequestChecks(context.Context, Repository, int64) ([]PullRequestCheck, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) CreateCommitStatus(context.Context, CommitStatusInput) (CommitStatus, error) {
	return CommitStatus{}, errors.ErrUnsupported
}

func (NoopClient) ListPullRequestFiles(context.Context, Repository, int64) ([]PullRequestFile, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) ListPullRequestCommits(context.Context, Repository, int64) ([]PullRequestCommit, error) {
	return nil, errors.ErrUnsupported
}
