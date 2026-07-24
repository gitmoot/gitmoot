package github

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

type Client interface {
	Ping(ctx context.Context) error
	Preflight(ctx context.Context, repo Repository) error
	RepositoryExists(ctx context.Context, repo Repository) (bool, error)
	CreateRepository(ctx context.Context, repo Repository, private bool) error
	CloneRepository(ctx context.Context, repo Repository, dir string) error
	DeleteRepository(ctx context.Context, repo Repository) error
	ListUserRepositories(ctx context.Context, limit int) ([]RepoSummary, error)
	ListPullRequests(ctx context.Context, repo Repository, state string) ([]PullRequest, error)
	// ListRecentClosedPullRequests returns a bounded page of the most-recently-
	// updated closed PRs (#467) so the per-tick revert scan never re-paginates the
	// repo's entire closed-PR history.
	ListRecentClosedPullRequests(ctx context.Context, repo Repository) ([]PullRequest, error)
	ListIssues(ctx context.Context, repo Repository, state string) ([]Issue, error)
	GetPullRequest(ctx context.Context, repo Repository, number int64) (PullRequest, error)
	GetOpenPullRequestByHead(ctx context.Context, repo Repository, head string, base string) (PullRequest, bool, error)
	CreatePullRequest(ctx context.Context, input CreatePullRequestInput) (PullRequest, error)
	EnsurePullRequest(ctx context.Context, input CreatePullRequestInput) (PullRequest, error)
	CreateIssue(ctx context.Context, input CreateIssueInput) (Issue, error)
	CloseIssue(ctx context.Context, repo Repository, issueNumber int64) (Issue, error)
	ListIssueComments(ctx context.Context, repo Repository, issueNumber int64) ([]IssueComment, error)
	// ListRepoIssueComments returns one bounded page of the repo's issue/PR
	// conversation comments updated at/after `since` (#566), collapsing the
	// per-issue comment fan-out into a single repo-wide call.
	ListRepoIssueComments(ctx context.Context, repo Repository, since time.Time) ([]IssueComment, error)
	PostIssueComment(ctx context.Context, repo Repository, issueNumber int64, body string) (IssueComment, error)
	GetUserPermission(ctx context.Context, repo Repository, username string) (UserPermission, error)
	MergePullRequest(ctx context.Context, input MergePullRequestInput) (MergeResult, error)
	UpdatePullRequestBranch(ctx context.Context, input UpdatePullRequestBranchInput) (UpdatePullRequestBranchResult, error)
	GetCombinedStatus(ctx context.Context, repo Repository, ref string) (CombinedStatus, error)
	ListCheckRunsForRef(ctx context.Context, repo Repository, ref string) ([]PullRequestCheck, error)
	CompareCommits(ctx context.Context, repo Repository, base string, head string) (CompareResult, error)
	ListPullRequestChecks(ctx context.Context, repo Repository, number int64) ([]PullRequestCheck, error)
	CreateCommitStatus(ctx context.Context, input CommitStatusInput) (CommitStatus, error)
	ListPullRequestFiles(ctx context.Context, repo Repository, number int64) ([]PullRequestFile, error)
	ListPullRequestCommits(ctx context.Context, repo Repository, number int64) ([]PullRequestCommit, error)
	UpsertFile(ctx context.Context, input UpsertFileInput) (RepositoryFile, error)
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
	Number int64  `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	URL    string `json:"html_url"`
	Merged bool   `json:"merged"`
	// MergedAt is the PR's merge timestamp (RFC3339), or "" if not merged. It is
	// additive (#467): the GitHub LIST endpoint (GET /pulls?state=closed) reports
	// merged PRs as state="closed" and OMITS the top-level `merged` boolean —
	// `merged` is computed only on the single-PR GET endpoint. `merged_at` is the
	// ONLY merged signal the list carries, so revert detection treats a non-empty
	// MergedAt as merged (revertPullMerged) rather than trusting list `merged`,
	// which is always false on the list. It defaults to "" so callers that ignore
	// it are byte-identical.
	MergedAt string `json:"merged_at"`
	// Body is the PR description. It is additive (#467): the daemon's revert
	// detection reads it for a GitHub Revert-button body (`Reverts owner/repo#NN`)
	// to map a revert back to the original PR. It defaults to "" so every existing
	// caller that ignores it is byte-identical.
	Body    string `json:"body"`
	HeadRef string
	// HeadRepoFullName identifies the repository that owns the PR head branch.
	// GitHub permits fork PRs whose HeadRef text matches a local branch, so callers
	// that mutate an existing branch must validate this separately from HeadRef.
	HeadRepoFullName string
	BaseRef          string
	BaseSHA          string
	HeadSHA          string
	MergeSHA         string
	Mergeable        *bool `json:"mergeable"`
	// Labels carries the PR's GitHub labels (parsed from the list/get JSON, no
	// extra API call). It is additive (#650): existing callers that ignore it are
	// byte-identical; the opt-in risk classifier reads label names from it.
	Labels []PullRequestLabel `json:"labels,omitempty"`
}

// PullRequestLabel is a single GitHub label on a PR (only the name is used).
type PullRequestLabel struct {
	Name string `json:"name"`
}

// LabelNames returns the PR's label names, dropping blanks. It is the input the
// risk classifier consumes.
func (p PullRequest) LabelNames() []string {
	names := make([]string, 0, len(p.Labels))
	for _, l := range p.Labels {
		if name := strings.TrimSpace(l.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func (p *PullRequest) UnmarshalJSON(data []byte) error {
	type wire struct {
		Number    int64  `json:"number"`
		Title     string `json:"title"`
		State     string `json:"state"`
		URL       string `json:"html_url"`
		Merged    bool   `json:"merged"`
		MergedAt  string `json:"merged_at"`
		Body      string `json:"body"`
		Mergeable *bool  `json:"mergeable"`
		MergeSHA  string `json:"merge_commit_sha"`
		Head      struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"base"`
		Labels []PullRequestLabel `json:"labels"`
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
	p.MergedAt = decoded.MergedAt
	p.Body = decoded.Body
	p.Mergeable = decoded.Mergeable
	p.HeadRef = decoded.Head.Ref
	p.HeadRepoFullName = ""
	if decoded.Head.Repo != nil {
		p.HeadRepoFullName = decoded.Head.Repo.FullName
	}
	p.HeadSHA = decoded.Head.SHA
	p.MergeSHA = decoded.MergeSHA
	p.BaseRef = decoded.Base.Ref
	p.BaseSHA = decoded.Base.SHA
	p.Labels = decoded.Labels
	return nil
}

type IssueComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	URL       string `json:"html_url"`
	Author    string
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// IssueNumber is the issue/PR number this comment belongs to. The per-issue
	// endpoint omits it (the caller already knows the number), but the REPO-WIDE
	// endpoint (GET /repos/{owner}/{repo}/issues/comments, #566) returns comments
	// for every issue AND PR at once, carrying only an `issue_url`. It is parsed
	// from that `issue_url` so ListRepoIssueComments callers can group the flat
	// result back by issue/PR number. It is 0 when the source payload carried no
	// parseable issue_url (the per-issue endpoint), so existing callers that ignore
	// it are byte-identical.
	IssueNumber int64
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
		IssueURL  string `json:"issue_url"`
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
	c.IssueNumber = issueNumberFromURL(decoded.IssueURL)
	return nil
}

// issueNumberFromURL extracts the trailing issue/PR number from a GitHub
// `issue_url` (e.g. https://api.github.com/repos/owner/repo/issues/123 -> 123).
// It returns 0 for an empty or unparseable URL, which the repo-wide comment
// grouping treats as "unknown issue" and skips. Every comment (issue OR PR) on
// the /issues/comments endpoint carries this shape; PR review/diff comments live
// on a different endpoint and are not returned here.
func issueNumberFromURL(issueURL string) int64 {
	issueURL = strings.TrimRight(strings.TrimSpace(issueURL), "/")
	if issueURL == "" {
		return 0
	}
	idx := strings.LastIndex(issueURL, "/")
	if idx < 0 || idx == len(issueURL)-1 {
		return 0
	}
	number, err := strconv.ParseInt(issueURL[idx+1:], 10, 64)
	if err != nil || number <= 0 {
		return 0
	}
	return number
}

type Issue struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	URL    string `json:"html_url"`
	Body   string `json:"body"`
	// IsPullRequest is true when this row came back from the /issues listing as a
	// pull request. The GitHub issues endpoint returns issues AND PRs; PRs carry
	// a `pull_request` object. Callers that want plain issues filter these out so
	// the PR-watcher is not duplicated.
	IsPullRequest bool `json:"-"`
}

func (i *Issue) UnmarshalJSON(data []byte) error {
	type wire struct {
		Number      int64           `json:"number"`
		Title       string          `json:"title"`
		State       string          `json:"state"`
		URL         string          `json:"html_url"`
		Body        string          `json:"body"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	i.Number = decoded.Number
	i.Title = decoded.Title
	i.State = decoded.State
	i.URL = decoded.URL
	i.Body = decoded.Body
	i.IsPullRequest = len(decoded.PullRequest) > 0 && string(decoded.PullRequest) != "null"
	return nil
}

type CreateIssueInput struct {
	Repo   Repository
	Title  string
	Body   string
	Labels []string
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

type UpdatePullRequestBranchInput struct {
	Repo            Repository
	Number          int64
	ExpectedHeadSHA string
}

type UpdatePullRequestBranchResult struct {
	Message string `json:"message"`
	URL     string `json:"url"`
}

type UpdatePullRequestBranchErrorKind string

const (
	UpdatePullRequestBranchErrorStaleHead   UpdatePullRequestBranchErrorKind = "stale_head"
	UpdatePullRequestBranchErrorConflict    UpdatePullRequestBranchErrorKind = "conflict"
	UpdatePullRequestBranchErrorUnsupported UpdatePullRequestBranchErrorKind = "unsupported"
	UpdatePullRequestBranchErrorTransient   UpdatePullRequestBranchErrorKind = "transient"
)

type UpdatePullRequestBranchError struct {
	Kind   UpdatePullRequestBranchErrorKind
	Detail string
	Err    error
}

func (e UpdatePullRequestBranchError) Error() string {
	detail := strings.TrimSpace(e.Detail)
	if detail == "" && e.Err != nil {
		detail = e.Err.Error()
	}
	if detail == "" {
		detail = string(e.Kind)
	}
	return "update pull request branch " + string(e.Kind) + ": " + detail
}

func (e UpdatePullRequestBranchError) Unwrap() error {
	return e.Err
}

func IsUpdatePullRequestBranchError(err error, kind UpdatePullRequestBranchErrorKind) bool {
	var updateErr UpdatePullRequestBranchError
	return errors.As(err, &updateErr) && updateErr.Kind == kind
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

type UpsertFileInput struct {
	Repo    Repository
	Path    string
	Content []byte
	Message string
	Branch  string
}

type DeleteFileInput struct {
	Repo    Repository
	Path    string
	Message string
	Branch  string
}

type RepositoryFile struct {
	Path string
	URL  string
	SHA  string
}

type GhClient struct {
	Runner     subprocess.Runner
	Dir        string
	Sleep      func(context.Context, time.Duration) error
	MaxRetries int
	// Limiter is the process-wide GitHub-call scheduler (#683). When nil the client
	// uses the package-global DefaultLimiter() so every GhClient constructed via
	// NewClient shares one budget + one secondary-rate-limit backoff window. Tests
	// set it to an isolated limiter (fake clock) to exercise smoothing/backoff.
	Limiter *RateLimiter

	mutateMu sync.Mutex
	statsMu  sync.Mutex
	stats    ConditionalRequestStats
}

func NewClient(dir string) *GhClient {
	return &GhClient{Dir: dir}
}

func (c *GhClient) Ping(ctx context.Context) error {
	_, err := c.run(ctx, false, "repo", "view", "--json", "nameWithOwner")
	return err
}

func (c *GhClient) Preflight(ctx context.Context, repo Repository) error {
	if _, err := c.run(ctx, false, "--version"); err != nil {
		return fmt.Errorf("GitHub CLI preflight failed: `gh --version` failed; install GitHub CLI (`gh`) and ensure it is on PATH: %w", err)
	}
	if _, err := c.run(ctx, false, "auth", "status", "--hostname", "github.com"); err != nil {
		return fmt.Errorf("GitHub CLI preflight failed: `gh auth status --hostname github.com` failed; run `gh auth login --hostname github.com`: %w", err)
	}
	if strings.TrimSpace(repo.FullName()) != "" {
		if _, err := c.run(ctx, false, "repo", "view", repo.FullName(), "--json", "nameWithOwner"); err != nil {
			return fmt.Errorf("GitHub CLI preflight failed: cannot view expected repo %s; check repo access or run `gh auth login --hostname github.com`: %w", repo.FullName(), err)
		}
	}
	return nil
}

// RepoSummary is one repository in a recency-sorted listing.
type RepoSummary struct {
	FullName  string
	UpdatedAt string
}

// ListUserRepositories lists the authenticated user's repositories, most
// recently updated first, via `gh repo list`.
func (c *GhClient) ListUserRepositories(ctx context.Context, limit int) ([]RepoSummary, error) {
	if limit <= 0 {
		limit = 30
	}
	result, err := c.run(ctx, false, "repo", "list", "--json", "nameWithOwner,updatedAt", "--limit", strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		NameWithOwner string `json:"nameWithOwner"`
		UpdatedAt     string `json:"updatedAt"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &rows); err != nil {
		return nil, fmt.Errorf("parse gh repo list output: %w", err)
	}
	repos := make([]RepoSummary, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.NameWithOwner) == "" {
			continue
		}
		repos = append(repos, RepoSummary{FullName: row.NameWithOwner, UpdatedAt: row.UpdatedAt})
	}
	sort.SliceStable(repos, func(i, j int) bool { return repos[i].UpdatedAt > repos[j].UpdatedAt })
	return repos, nil
}

// RepositoryExists reports whether the repo is visible to the authenticated gh
// user. A 404, "not found", or the GraphQL "could not resolve to a repository"
// phrasing maps to (false, nil); any other error (auth, network, rate limit)
// propagates so callers never offer to create a repo on an ambiguous failure.
func (c *GhClient) RepositoryExists(ctx context.Context, repo Repository) (bool, error) {
	if strings.TrimSpace(repo.FullName()) == "" {
		return false, fmt.Errorf("repository owner/name is required")
	}
	result, err := c.run(ctx, false, "repo", "view", repo.FullName(), "--json", "nameWithOwner")
	if err != nil {
		if isNotFound(result) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CreateRepository creates the repo via `gh repo create`. It rides the mutate
// lock like other write operations.
func (c *GhClient) CreateRepository(ctx context.Context, repo Repository, private bool) error {
	if strings.TrimSpace(repo.FullName()) == "" {
		return fmt.Errorf("repository owner/name is required")
	}
	visibility := "--public"
	if private {
		visibility = "--private"
	}
	// --add-readme gives the new repo an initial commit and a default branch, so
	// it is immediately clonable into a usable checkout (gitmoot only creates
	// repos for its own workflows, where an empty repo is never useful).
	_, err := c.run(ctx, true, "repo", "create", repo.FullName(), visibility, "--add-readme")
	return err
}

// CloneRepository clones repo into dir via `gh repo clone`. dir must not already
// exist (gh refuses to clone into a non-empty target).
func (c *GhClient) CloneRepository(ctx context.Context, repo Repository, dir string) error {
	if strings.TrimSpace(repo.FullName()) == "" {
		return fmt.Errorf("repository owner/name is required")
	}
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("clone destination is required")
	}
	_, err := c.run(ctx, false, "repo", "clone", repo.FullName(), dir)
	return err
}

// DeleteRepository deletes the repo via `gh repo delete`. GitHub requires the
// non-default delete_repo token scope; a scope failure is mapped to the exact
// remedy so callers can show it verbatim.
func (c *GhClient) DeleteRepository(ctx context.Context, repo Repository) error {
	if strings.TrimSpace(repo.FullName()) == "" {
		return fmt.Errorf("repository owner/name is required")
	}
	result, err := c.run(ctx, true, "repo", "delete", repo.FullName(), "--yes")
	if err != nil && isMissingDeleteRepoScope(result) {
		return fmt.Errorf("deleting %s requires the delete_repo token scope; run `gh auth refresh -h github.com -s delete_repo` and retry: %w", repo.FullName(), err)
	}
	return err
}

func isMissingDeleteRepoScope(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "delete_repo") ||
		(strings.Contains(text, "http 403") && strings.Contains(text, "scope"))
}

func (c *GhClient) ListPullRequests(ctx context.Context, repo Repository, state string) ([]PullRequest, error) {
	if state == "" {
		state = "open"
	}
	args := []string{"-X", "GET", endpoint(repo, "pulls"), "-f", "state=" + state}
	if !conditionalEnabled() {
		return apiPaginatedJSON[PullRequest](ctx, c, args...)
	}
	page, key, err := conditionalPageJSON[PullRequest](ctx, c, repo,
		append(args, "-f", "per_page=100")...)
	if err != nil {
		return nil, err
	}
	if len(page) == 100 {
		// A full first page may have more results. Preserve the existing complete
		// pagination path and never retain the partial page in the ETag cache.
		evictConditionalEntry(key)
		return apiPaginatedJSON[PullRequest](ctx, c, args...)
	}
	return page, nil
}

// recentClosedPullRequestsPerPage caps the bounded closed-PR scan (#467) at one
// page of the most-recently-updated closed PRs. GitHub's pulls list allows up to
// 100 per page; 100 keeps the per-tick GitHub cost a single, fixed request even on
// a repo with thousands of closed PRs. A merged GitHub Revert-button PR is freshly
// updated when it lands, so sort=updated&direction=desc puts it at the top of this
// window — far more than enough to catch a revert between poll ticks.
const recentClosedPullRequestsPerPage = 100

// ListRecentClosedPullRequests returns ONE page of the most-recently-updated
// CLOSED PRs (sort=updated&direction=desc, no --paginate), bounding the per-tick
// cost of the revert scan (#467) to a single fixed GitHub request instead of
// walking the repo's entire closed-PR history every poll. It is the bounded
// counterpart to ListPullRequests(state="closed"); revert detection only needs
// recently-landed reverts, so a single recent page is sufficient.
func (c *GhClient) ListRecentClosedPullRequests(ctx context.Context, repo Repository) ([]PullRequest, error) {
	args := []string{
		"-X", "GET", endpoint(repo, "pulls"),
		"-f", "state=closed",
		"-f", "sort=updated",
		"-f", "direction=desc",
		"-f", "per_page=" + strconv.Itoa(recentClosedPullRequestsPerPage),
	}
	if !conditionalEnabled() {
		return apiPageJSON[PullRequest](ctx, c, args...)
	}
	values, _, err := conditionalPageJSON[PullRequest](ctx, c, repo, args...)
	return values, err
}

// ListIssues lists repository issues via GET /repos/{owner}/{repo}/issues. That
// endpoint returns issues AND pull requests; PRs carry a `pull_request` object,
// so the result is filtered down to plain issues here (IsPullRequest == true is
// dropped) to keep the issue-comment watcher from duplicating the PR-watcher.
func (c *GhClient) ListIssues(ctx context.Context, repo Repository, state string) ([]Issue, error) {
	if state == "" {
		state = "open"
	}
	args := []string{"-X", "GET", endpoint(repo, "issues"), "-f", "state=" + state}
	var issues []Issue
	var err error
	if conditionalEnabled() {
		var key string
		issues, key, err = conditionalPageJSON[Issue](ctx, c, repo,
			append(args, "-f", "per_page=100")...)
		if err == nil && len(issues) == 100 {
			evictConditionalEntry(key)
			issues, err = apiPaginatedJSON[Issue](ctx, c, args...)
		}
	} else {
		issues, err = apiPaginatedJSON[Issue](ctx, c, args...)
	}
	if err != nil {
		return nil, err
	}
	filtered := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.IsPullRequest {
			continue
		}
		filtered = append(filtered, issue)
	}
	return filtered, nil
}

func (c *GhClient) GetPullRequest(ctx context.Context, repo Repository, number int64) (PullRequest, error) {
	return c.getPullRequest(ctx, repo, number)
}

func (c *GhClient) CreatePullRequest(ctx context.Context, input CreatePullRequestInput) (PullRequest, error) {
	bodyArgs, cleanup, err := bodyFlagArgs(input.Body)
	if err != nil {
		return PullRequest{}, err
	}
	defer cleanup()
	args := []string{"pr", "create", "--repo", input.Repo.FullName(), "--title", input.Title}
	args = append(args, bodyArgs...)
	args = append(args, "--head", input.Head, "--base", input.Base)
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

// GetOpenPullRequestByHead returns the open PR whose head branch is `head` and
// whose base branch is `base`, if one exists, via `gh pr list`. Filtering by
// both head and base makes the match exact: GitHub guarantees a single open PR
// per (head, same-base) pair — that uniqueness is exactly the 422 "a pull
// request already exists" case. The boolean is false (with a nil error) when no
// open PR is found. `gh pr list --json` field names differ from the REST API
// (url/headRefOid/baseRefName), so the response is decoded through a dedicated
// wire shape and mapped onto PullRequest.
func (c *GhClient) GetOpenPullRequestByHead(ctx context.Context, repo Repository, head string, base string) (PullRequest, bool, error) {
	if strings.TrimSpace(repo.FullName()) == "" {
		return PullRequest{}, false, fmt.Errorf("repository owner/name is required")
	}
	if strings.TrimSpace(head) == "" {
		return PullRequest{}, false, fmt.Errorf("head branch is required")
	}
	if strings.TrimSpace(base) == "" {
		return PullRequest{}, false, fmt.Errorf("base branch is required")
	}
	result, err := c.run(ctx, false,
		"pr", "list",
		"--repo", repo.FullName(),
		"--head", head,
		"--base", base,
		"--state", "open",
		"--json", "number,url,headRefOid,baseRefName,state",
	)
	if err != nil {
		return PullRequest{}, false, err
	}
	var wire []struct {
		Number      int64  `json:"number"`
		URL         string `json:"url"`
		HeadRefOid  string `json:"headRefOid"`
		BaseRefName string `json:"baseRefName"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &wire); err != nil {
		return PullRequest{}, false, fmt.Errorf("decode gh pr list response: %w", err)
	}
	if len(wire) == 0 {
		return PullRequest{}, false, nil
	}
	w := wire[0]
	return PullRequest{
		Number:  w.Number,
		URL:     w.URL,
		State:   strings.ToLower(strings.TrimSpace(w.State)),
		HeadRef: head,
		HeadSHA: w.HeadRefOid,
		BaseRef: w.BaseRefName,
	}, true, nil
}

// EnsurePullRequest returns the open PR for input.Head idempotently: it adopts
// an existing open head PR (query-first, no create), otherwise creates one, and
// if create races a concurrent open (a 422 "a pull request already exists") it
// re-queries by head and adopts the winner. This removes the TOCTOU window
// where two finalizers (or an out-of-band PR) turn a benign "already exists"
// into a hard error.
func (c *GhClient) EnsurePullRequest(ctx context.Context, input CreatePullRequestInput) (PullRequest, error) {
	if existing, ok, err := c.GetOpenPullRequestByHead(ctx, input.Repo, input.Head, input.Base); err != nil {
		return PullRequest{}, err
	} else if ok {
		return existing, nil
	}
	pr, err := c.CreatePullRequest(ctx, input)
	if err == nil {
		return pr, nil
	}
	if !isPullRequestAlreadyExists(err) {
		return PullRequest{}, err
	}
	// A concurrent create won the race; adopt the now-existing open PR.
	if existing, ok, qerr := c.GetOpenPullRequestByHead(ctx, input.Repo, input.Head, input.Base); qerr != nil {
		return PullRequest{}, qerr
	} else if ok {
		return existing, nil
	}
	return PullRequest{}, err
}

// isPullRequestAlreadyExists detects the gh/GitHub 422 raised when a PR already
// exists for the head branch. gh surfaces it in the error text, so match on the
// "already exists" phrasing (and the 422 status) robustly.
func isPullRequestAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "already exists") ||
		(strings.Contains(text, "422") && strings.Contains(text, "pull request"))
}

func (c *GhClient) ListIssueComments(ctx context.Context, repo Repository, issueNumber int64) ([]IssueComment, error) {
	comments, err := apiPaginatedJSON[IssueComment](ctx, c, endpoint(repo, "issues", issueNumber, "comments"))
	return dedupeComments(comments), err
}

// ListRepoIssueComments returns a SINGLE bounded page of the repo's issue/PR
// conversation comments updated at or after `since` (#566), ordered oldest-first
// (sort=updated&direction=asc) so the poller always makes forward progress and can
// advance its `since` cursor to the newest comment it saw.
//
// It hits GET /repos/{owner}/{repo}/issues/comments — the REPO-WIDE endpoint that
// returns comments for EVERY issue AND pull request in one paginated call
// (excluding only PR review/diff comments, which the @mention command flow does
// not use). This collapses the daemon's former O(issues) per-issue comment
// fan-out into ONE gh call per repo per tick. Each returned comment carries its
// IssueNumber (parsed from `issue_url`) so callers can group the flat result back
// by issue/PR.
//
// BOUNDED COST: like ListRecentClosedPullRequests (#467) it fetches ONE page
// (apiPageJSON, no --paginate) with per_page=100. With direction=asc a backlog
// larger than one page is drained across successive ticks as the caller advances
// `since`, so steady-state polling is a single fixed request rather than
// re-paginating the repo's entire comment history. A zero `since` omits the
// filter (the caller seeds a bounded window on first poll, so this is only a
// safety fallback).
func (c *GhClient) ListRepoIssueComments(ctx context.Context, repo Repository, since time.Time) ([]IssueComment, error) {
	args := []string{
		"-X", "GET", endpoint(repo, "issues", "comments"),
		"-f", "sort=updated",
		"-f", "direction=asc",
		"-f", "per_page=100",
	}
	if !since.IsZero() {
		args = append(args, "-f", "since="+since.UTC().Format(time.RFC3339))
	}
	var comments []IssueComment
	var err error
	if conditionalEnabled() {
		comments, _, err = conditionalPageJSON[IssueComment](ctx, c, repo, args...)
	} else {
		comments, err = apiPageJSON[IssueComment](ctx, c, args...)
	}
	return dedupeComments(comments), err
}

func (c *GhClient) PostIssueComment(ctx context.Context, repo Repository, issueNumber int64, body string) (IssueComment, error) {
	var comment IssueComment
	bodyArgs, cleanup, err := apiBodyFieldArgs(body)
	if err != nil {
		return IssueComment{}, err
	}
	defer cleanup()
	args := append([]string{endpoint(repo, "issues", issueNumber, "comments")}, bodyArgs...)
	err = c.apiJSON(ctx, true, &comment, args...)
	return comment, err
}

func (c *GhClient) SearchOpenIssues(ctx context.Context, repo Repository, text string) ([]Issue, error) {
	if strings.TrimSpace(repo.FullName()) == "" {
		return nil, fmt.Errorf("repository owner/name is required")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("issue search text is required")
	}
	var response struct {
		Items []Issue `json:"items"`
	}
	query := fmt.Sprintf("repo:%s is:issue is:open in:body %q", repo.FullName(), text)
	err := c.apiJSON(ctx, false, &response, "-X", "GET", "search/issues", "-f", "q="+query)
	return response.Items, err
}

func (c *GhClient) CreateIssue(ctx context.Context, input CreateIssueInput) (Issue, error) {
	var issue Issue
	bodyArgs, cleanup, err := apiBodyFieldArgs(input.Body)
	if err != nil {
		return Issue{}, err
	}
	defer cleanup()
	args := []string{endpoint(input.Repo, "issues"), "-f", "title=" + input.Title}
	args = append(args, bodyArgs...)
	err = c.apiJSON(ctx, true, &issue, args...)
	if err != nil {
		return Issue{}, err
	}
	c.applyIssueLabelsBestEffort(ctx, input.Repo, issue.Number, input.Labels)
	return issue, nil
}

func (c *GhClient) applyIssueLabelsBestEffort(ctx context.Context, repo Repository, issueNumber int64, labels []string) {
	labels = compactUniqueStrings(labels)
	if strings.TrimSpace(repo.FullName()) == "" || issueNumber <= 0 || len(labels) == 0 {
		return
	}
	for _, label := range labels {
		color, description := issueLabelMetadata(label)
		args := []string{
			"api",
			endpoint(repo, "labels"),
			"-f", "name=" + label,
			"-f", "color=" + color,
		}
		if description != "" {
			args = append(args, "-f", "description="+description)
		}
		_, _ = c.run(ctx, true, args...)
	}
	args := []string{"api", endpoint(repo, "issues", issueNumber, "labels")}
	for _, label := range labels {
		args = append(args, "-f", "labels[]="+label)
	}
	_, _ = c.run(ctx, true, args...)
}

func issueLabelMetadata(label string) (string, string) {
	switch label {
	case "gitmoot-dashboard-report":
		return "5319e7", "Gitmoot-generated bug report"
	case "bug":
		return "d73a4a", "Something is not working"
	default:
		return "ededed", ""
	}
}

func compactUniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var compacted []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		compacted = append(compacted, value)
	}
	return compacted
}

func (c *GhClient) CloseIssue(ctx context.Context, repo Repository, issueNumber int64) (Issue, error) {
	var issue Issue
	err := c.apiJSON(ctx, true, &issue, "-X", "PATCH", endpoint(repo, "issues", issueNumber), "-f", "state=closed")
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
		bodyArgs, cleanup, err := bodyFlagArgs(input.Body)
		if err != nil {
			return MergeResult{}, err
		}
		defer cleanup()
		args = append(args, bodyArgs...)
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

func (c *GhClient) UpdatePullRequestBranch(ctx context.Context, input UpdatePullRequestBranchInput) (UpdatePullRequestBranchResult, error) {
	if strings.TrimSpace(input.Repo.FullName()) == "" {
		return UpdatePullRequestBranchResult{}, errors.New("repository is required")
	}
	if input.Number <= 0 {
		return UpdatePullRequestBranchResult{}, errors.New("pull request number is required")
	}
	args := []string{
		"api",
		"-X", "PUT",
		endpoint(input.Repo, "pulls", input.Number, "update-branch"),
	}
	if expected := strings.TrimSpace(input.ExpectedHeadSHA); expected != "" {
		args = append(args, "-f", "expected_head_sha="+expected)
	}
	result, err := c.run(ctx, true, args...)
	if err != nil {
		return UpdatePullRequestBranchResult{}, classifyUpdatePullRequestBranchError(result, err)
	}
	var response UpdatePullRequestBranchResult
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return UpdatePullRequestBranchResult{}, fmt.Errorf("decode gh update-branch response: %w", err)
	}
	return response, nil
}

func (c *GhClient) GetCombinedStatus(ctx context.Context, repo Repository, ref string) (CombinedStatus, error) {
	var status CombinedStatus
	err := c.apiJSON(ctx, false, &status, endpoint(repo, "commits", ref, "status"))
	return status, err
}

func (c *GhClient) ListCheckRunsForRef(ctx context.Context, repo Repository, ref string) ([]PullRequestCheck, error) {
	type checkRun struct {
		Name        string `json:"name"`
		Status      string `json:"status"`
		Conclusion  string `json:"conclusion"`
		HTMLURL     string `json:"html_url"`
		CompletedAt string `json:"completed_at"`
	}
	result, err := c.run(ctx, false,
		"api", "--paginate", "--jq",
		".check_runs[] | {name, status, conclusion, html_url, completed_at}",
		endpoint(repo, "commits", ref, "check-runs")+"?per_page=100",
	)
	if err != nil {
		return nil, err
	}
	checks := []PullRequestCheck{}
	scanner := bufio.NewScanner(strings.NewReader(result.Stdout))
	for line := 1; scanner.Scan(); line++ {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var run checkRun
		if err := json.Unmarshal([]byte(raw), &run); err != nil {
			return nil, fmt.Errorf("decode gh check-run line %d: %w", line, err)
		}
		state := strings.TrimSpace(run.Conclusion)
		if state == "" {
			state = strings.TrimSpace(run.Status)
		}
		checks = append(checks, PullRequestCheck{
			Name: run.Name, State: state, Link: run.HTMLURL, CompletedAt: run.CompletedAt,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan gh check-runs response: %w", err)
	}
	return checks, nil
}

// WorkflowsExistAtRef reports whether the repository has a `.github/workflows`
// directory at the given ref (#596). The merge gate uses it as a cheap, one-read
// workflow-awareness signal: if the head tree demonstrably carries workflow
// files, a zero-external-CI observation means GitHub Actions has not created the
// run yet (or it is queued), so the gate must NOT conclude "no CI". A 404 (no
// such path) reports false; any other error is returned so the caller can fail
// safe toward the grace path rather than instant-stamping.
func (c *GhClient) WorkflowsExistAtRef(ctx context.Context, repo Repository, ref string) (bool, error) {
	args := []string{"api", "-X", "GET", endpoint(repo, "contents", ".github/workflows")}
	if r := strings.TrimSpace(ref); r != "" {
		args = append(args, "-f", "ref="+r)
	}
	result, err := c.run(ctx, false, args...)
	if err == nil {
		return true, nil
	}
	if isNotFound(result) {
		return false, nil
	}
	return false, err
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

func (c *GhClient) UpsertFile(ctx context.Context, input UpsertFileInput) (RepositoryFile, error) {
	path := strings.Trim(strings.TrimSpace(input.Path), "/")
	if input.Repo.FullName() == "" {
		return RepositoryFile{}, errors.New("repository is required")
	}
	if path == "" {
		return RepositoryFile{}, errors.New("file path is required")
	}
	message := strings.TrimSpace(input.Message)
	if message == "" {
		message = "Update " + path
	}
	sha := ""
	var existing struct {
		SHA string `json:"sha"`
	}
	getArgs := []string{"api", "-X", "GET", endpoint(input.Repo, "contents", path)}
	if branch := strings.TrimSpace(input.Branch); branch != "" {
		getArgs = append(getArgs, "-f", "ref="+branch)
	}
	result, err := c.run(ctx, false, getArgs...)
	if err == nil {
		if decodeErr := json.Unmarshal([]byte(result.Stdout), &existing); decodeErr != nil {
			return RepositoryFile{}, fmt.Errorf("decode github contents response: %w", decodeErr)
		}
		sha = strings.TrimSpace(existing.SHA)
	} else if !isNotFound(result) {
		return RepositoryFile{}, err
	}
	args := []string{
		"-X", "PUT",
		endpoint(input.Repo, "contents", path),
		"-f", "message=" + message,
		"-f", "content=" + base64.StdEncoding.EncodeToString(input.Content),
	}
	if sha != "" {
		args = append(args, "-f", "sha="+sha)
	}
	if branch := strings.TrimSpace(input.Branch); branch != "" {
		args = append(args, "-f", "branch="+branch)
	}
	var response struct {
		Content struct {
			Path string `json:"path"`
			URL  string `json:"html_url"`
			SHA  string `json:"sha"`
		} `json:"content"`
	}
	if err := c.apiJSON(ctx, true, &response, args...); err != nil {
		return RepositoryFile{}, err
	}
	return RepositoryFile{
		Path: firstNonEmpty(response.Content.Path, path),
		URL:  response.Content.URL,
		SHA:  response.Content.SHA,
	}, nil
}

// DeleteFile removes a repository file through the GitHub contents API. A
// concurrently vanished path is an idempotent success.
func (c *GhClient) DeleteFile(ctx context.Context, input DeleteFileInput) (RepositoryFile, error) {
	path := strings.Trim(strings.TrimSpace(input.Path), "/")
	if input.Repo.FullName() == "" {
		return RepositoryFile{}, errors.New("repository is required")
	}
	if path == "" {
		return RepositoryFile{}, errors.New("file path is required")
	}
	message := strings.TrimSpace(input.Message)
	if message == "" {
		message = "Delete " + path
	}
	var existing struct {
		SHA string `json:"sha"`
	}
	getArgs := []string{"api", "-X", "GET", endpoint(input.Repo, "contents", path)}
	if branch := strings.TrimSpace(input.Branch); branch != "" {
		getArgs = append(getArgs, "-f", "ref="+branch)
	}
	result, err := c.run(ctx, false, getArgs...)
	if err != nil {
		if isNotFound(result) {
			return RepositoryFile{Path: path}, nil
		}
		return RepositoryFile{}, err
	}
	if err := json.Unmarshal([]byte(result.Stdout), &existing); err != nil {
		return RepositoryFile{}, fmt.Errorf("decode github contents response: %w", err)
	}
	sha := strings.TrimSpace(existing.SHA)
	if sha == "" {
		return RepositoryFile{}, errors.New("github contents response did not include a file sha")
	}
	args := []string{
		"api", "-X", "DELETE", endpoint(input.Repo, "contents", path),
		"-f", "message=" + message,
		"-f", "sha=" + sha,
	}
	if branch := strings.TrimSpace(input.Branch); branch != "" {
		args = append(args, "-f", "branch="+branch)
	}
	if _, err := c.run(ctx, true, args...); err != nil {
		return RepositoryFile{}, err
	}
	return RepositoryFile{Path: path, SHA: sha}, nil
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

// apiPageJSON fetches a SINGLE page (no --paginate), so the caller controls the
// page size and never walks the entire result set. It is the bounded counterpart
// to apiPaginatedJSON, used by ListRecentClosedPullRequests (#467) so the per-tick
// revert scan does not re-paginate a repo's entire closed-PR history.
func apiPageJSON[T any](ctx context.Context, c *GhClient, args ...string) ([]T, error) {
	result, err := c.run(ctx, false, append([]string{"api"}, args...)...)
	if err != nil {
		return nil, err
	}
	var values []T
	if err := json.Unmarshal([]byte(result.Stdout), &values); err != nil {
		return nil, fmt.Errorf("decode gh api response: %w", err)
	}
	return values, nil
}

func conditionalPageJSON[T any](ctx context.Context, c *GhClient, repo Repository, args ...string) ([]T, string, error) {
	result, key, err := c.conditionalRun(ctx, repo, args...)
	if err != nil {
		return nil, key, err
	}
	var values []T
	if err := json.Unmarshal([]byte(result.Stdout), &values); err != nil {
		evictConditionalEntry(key)
		return nil, key, fmt.Errorf("decode gh api response: %w", err)
	}
	return values, key, nil
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

// ConditionalRequestStats returns this client's per-tick conditional-read
// accounting. A fresh GhClient is constructed for each daemon repository poll.
func (c *GhClient) ConditionalRequestStats() ConditionalRequestStats {
	if c == nil {
		return ConditionalRequestStats{}
	}
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	return c.stats
}

func (c *GhClient) noteConditionalRequest(miss bool) {
	c.statsMu.Lock()
	c.stats.Calls++
	if miss {
		c.stats.Misses++
	}
	c.statsMu.Unlock()
}

func (c *GhClient) noteNonConditionalGET() {
	c.statsMu.Lock()
	c.stats.Misses++
	c.statsMu.Unlock()
}

func (c *GhClient) noteConditionalMiss() {
	c.statsMu.Lock()
	c.stats.Misses++
	c.statsMu.Unlock()
}

// conditionalRun executes one ETag-capable REST read. It adds gh's include
// flag, replays a cached raw body on 304, and atomically replaces the cache on a
// valid 200. A missing/corrupt body after 304 is evicted and retried once without
// If-None-Match.
func (c *GhClient) conditionalRun(ctx context.Context, repo Repository, args ...string) (subprocess.Result, string, error) {
	baseArgs := append([]string{"api"}, args...)
	key := conditionalRequestKey(repo, baseArgs)
	forceUnconditional := false
	for cacheAttempt := 0; cacheAttempt < 2; cacheAttempt++ {
		entry, cached := loadConditionalEntry(key)
		requestArgs := []string{"api", "-i"}
		if cached && !forceUnconditional {
			requestArgs = append(requestArgs, "-H", "If-None-Match: "+entry.etag)
		}
		requestArgs = append(requestArgs, args...)
		result, err := c.runRequest(ctx, false, true, requestArgs...)
		response := parseConditionalResponse([]byte(result.Stdout))
		if response.status == 304 {
			if cached && !forceUnconditional && len(entry.body) > 0 && json.Valid(entry.body) {
				result.Stdout = string(entry.body)
				return result, key, nil
			}
			c.noteConditionalMiss()
			evictConditionalEntry(key)
			if cacheAttempt == 0 {
				forceUnconditional = true
				continue
			}
			return result, key, fmt.Errorf("GitHub returned 304 without a usable cached response")
		}
		if err != nil {
			return result, key, err
		}
		if response.headers {
			result.Stdout = string(response.body)
		}
		if response.status == 200 {
			if response.etag != "" && json.Valid(response.body) {
				storeConditionalEntry(key, response.etag, response.body)
			} else {
				evictConditionalEntry(key)
			}
		} else if !response.headers {
			// `-i` should always supply headers. Preserve the body for decoding but
			// do not retain an older validator against an unparseable response.
			evictConditionalEntry(key)
		}
		return result, key, nil
	}
	return subprocess.Result{}, key, fmt.Errorf("conditional GitHub request retry exhausted")
}

func (c *GhClient) run(ctx context.Context, mutate bool, args ...string) (subprocess.Result, error) {
	return c.runRequest(ctx, mutate, false, args...)
}

func (c *GhClient) runRequest(ctx context.Context, mutate bool, conditional bool, args ...string) (subprocess.Result, error) {
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
	limiter := c.Limiter
	if limiter == nil {
		limiter = DefaultLimiter()
	}
	var result subprocess.Result
	var err error
	sawSecondary := false
	for attempt := 0; attempt <= retries; attempt++ {
		// Acquire smooths bursts (concurrency cap + min-interval) and, critically,
		// waits out any active process-wide secondary-rate-limit backoff so a fresh
		// gh call does not fire into an abuse window that another call already tripped.
		if acqErr := limiter.Acquire(ctx); acqErr != nil {
			return result, acqErr
		}
		limiter.NoteCall()
		result, err = runner.Run(ctx, c.Dir, "gh", args...)
		limiter.Release()
		if conditional {
			status := parseConditionalResponse([]byte(result.Stdout)).status
			c.noteConditionalRequest(status != 304)
			// gh 2.45 exits non-zero for a valid 304. Handle it before any
			// transient/rate-limit classifier can turn an ETag hit into a failure.
			if status == 304 {
				limiter.NoteSuccess()
				err = nil
				break
			}
		} else if !mutate && isRESTGet(args) {
			c.noteNonConditionalGET()
		}
		if err == nil {
			limiter.NoteSuccess()
			break
		}
		// A recovered retry never pauses the whole process; only a call that
		// ULTIMATELY fails with a secondary limit engages the process-wide backoff,
		// so a transient hit that the retry below clears stays byte-identical.
		if isSecondaryRateLimit(result) {
			sawSecondary = true
		}
		if !isRateLimit(result) || attempt == retries {
			break
		}
		if sleepErr := c.sleep(ctx, time.Duration(attempt+1)*time.Second); sleepErr != nil {
			return result, sleepErr
		}
	}
	if err != nil && sawSecondary {
		// Pause further GitHub calls process-wide (respecting Retry-After, else
		// exponential fallback) rather than retry-storming the abuse detector.
		limiter.NoteSecondaryLimit(parseRetryAfter(result))
	}
	if err != nil {
		// Tag a network/GitHub-outage failure with the transparent TransientError
		// marker (#532 slice D) so a caller that propagates it to a TERMINAL job
		// failure can be classified as network_outage and short-backoff deferred.
		// Non-outage failures (and every failure's exact text) are untouched, and
		// best-effort callers that swallow the error are byte-identical.
		return result, classifyTransientError(result, commandError(result, err))
	}
	return result, nil
}

func isRESTGet(args []string) bool {
	if len(args) == 0 || args[0] != "api" {
		return false
	}
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "-X" || args[i] == "--method" {
			return strings.EqualFold(args[i+1], "GET")
		}
	}
	return true
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

// bodyFileThreshold is the byte size above which a GitHub body (issue/PR/comment)
// is handed to gh via a temp file instead of a single argv argument. The OS caps
// a single argv element (and the whole argv+env block) at roughly 128KB
// (MAX_ARG_STRLEN); passing a larger body inline makes fork/exec fail with
// "argument list too long" before gh ever runs (#734). 100KB leaves headroom for
// the surrounding flags while staying byte-identical to the old inline behavior
// for every ordinary body.
const bodyFileThreshold = 100 * 1024

// writeGhBodyTempFile writes body to a fresh temp file and returns its path plus
// a cleanup func that removes it. The caller MUST defer cleanup so the file is
// removed after the gh invocation (including all internal retries) completes.
func writeGhBodyTempFile(body string) (string, func(), error) {
	f, err := os.CreateTemp("", "gitmoot-gh-body-*.md")
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return name, cleanup, nil
}

// bodyFlagArgs returns the gh arguments that supply body to a gh verb that
// accepts `--body`/`--body-file` (e.g. `gh pr create`, `gh pr merge`). A body at
// or below bodyFileThreshold is passed inline as `--body <body>` — byte-identical
// to the historical behavior — while a larger one is written to a temp file and
// passed as `--body-file <path>` to stay under ARG_MAX (#734). The returned
// cleanup removes any temp file and must be deferred by the caller.
func bodyFlagArgs(body string) (args []string, cleanup func(), err error) {
	if len(body) <= bodyFileThreshold {
		return []string{"--body", body}, func() {}, nil
	}
	path, cleanup, err := writeGhBodyTempFile(body)
	if err != nil {
		return nil, func() {}, err
	}
	return []string{"--body-file", path}, cleanup, nil
}

// apiBodyFieldArgs returns the `gh api` field arguments that supply the "body"
// parameter. A body at or below bodyFileThreshold is passed inline as
// `-f body=<body>` — byte-identical to the historical behavior — while a larger
// one is written to a temp file and passed as `-F body=@<path>`, which tells
// `gh api` to read the field value from that file instead of taking it as a
// single argv argument that would blow ARG_MAX (#734). gh's `-F/--field` `@file`
// magic reads the file content verbatim as the string value; the file-read path
// bypasses the true/false/number type coercion, so a body is never re-typed. The
// returned cleanup removes any temp file and must be deferred by the caller.
func apiBodyFieldArgs(body string) (args []string, cleanup func(), err error) {
	if len(body) <= bodyFileThreshold {
		return []string{"-f", "body=" + body}, func() {}, nil
	}
	path, cleanup, err := writeGhBodyTempFile(body)
	if err != nil {
		return nil, func() {}, err
	}
	return []string{"-F", "body=@" + path}, cleanup, nil
}

func isRateLimit(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "rate limit") || strings.Contains(text, "retry-after") || strings.Contains(text, "http 429")
}

func isNotFound(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	// `gh repo view` reports a missing repo via the GraphQL resolver phrasing
	// rather than an HTTP 404 ("Could not resolve to a Repository with the
	// name 'owner/name'"). gh uses the same phrasing for repos that exist but
	// the token cannot see (private/org/EMU); treating those as "not found"
	// is safe because the follow-up `gh repo create` then fails loudly with
	// "name already exists" instead of leaving the caller dead-ended.
	return strings.Contains(text, "http 404") ||
		strings.Contains(text, "not found") ||
		strings.Contains(text, "could not resolve to a repository")
}

func isNoChecks(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "no checks reported")
}

func isUnsupportedJSONFlag(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "unknown flag: --json") || strings.Contains(text, "unknown shorthand flag") && strings.Contains(text, "json")
}

func classifyUpdatePullRequestBranchError(result subprocess.Result, err error) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	text := strings.ToLower(detail + "\n" + err.Error())
	kind := UpdatePullRequestBranchErrorTransient
	switch {
	case strings.Contains(text, "expected_head_sha") ||
		strings.Contains(text, "expected head sha") ||
		strings.Contains(text, "head sha") && strings.Contains(text, "does not match"):
		kind = UpdatePullRequestBranchErrorStaleHead
	case strings.Contains(text, "merge conflict") ||
		strings.Contains(text, "cannot be cleanly merged") ||
		strings.Contains(text, "not mergeable") ||
		strings.Contains(text, "conflict"):
		kind = UpdatePullRequestBranchErrorConflict
	case strings.Contains(text, "forbidden") ||
		strings.Contains(text, "permission") ||
		strings.Contains(text, "protected branch") ||
		strings.Contains(text, "head repository") ||
		strings.Contains(text, "fork"):
		kind = UpdatePullRequestBranchErrorUnsupported
	}
	return UpdatePullRequestBranchError{Kind: kind, Detail: detail, Err: err}
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

func (NoopClient) RepositoryExists(context.Context, Repository) (bool, error) {
	return true, nil
}

func (NoopClient) CreateRepository(context.Context, Repository, bool) error {
	return nil
}

func (NoopClient) CloneRepository(context.Context, Repository, string) error {
	return nil
}

func (NoopClient) ListUserRepositories(context.Context, int) ([]RepoSummary, error) {
	return nil, nil
}

func (NoopClient) DeleteRepository(context.Context, Repository) error {
	return nil
}

func (NoopClient) Preflight(context.Context, Repository) error {
	return nil
}

func (NoopClient) ListPullRequests(context.Context, Repository, string) ([]PullRequest, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) ListRecentClosedPullRequests(context.Context, Repository) ([]PullRequest, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) ListIssues(context.Context, Repository, string) ([]Issue, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) GetPullRequest(context.Context, Repository, int64) (PullRequest, error) {
	return PullRequest{}, errors.ErrUnsupported
}

func (NoopClient) GetOpenPullRequestByHead(context.Context, Repository, string, string) (PullRequest, bool, error) {
	return PullRequest{}, false, errors.ErrUnsupported
}

func (NoopClient) CreatePullRequest(context.Context, CreatePullRequestInput) (PullRequest, error) {
	return PullRequest{}, errors.ErrUnsupported
}

func (NoopClient) EnsurePullRequest(context.Context, CreatePullRequestInput) (PullRequest, error) {
	return PullRequest{}, errors.ErrUnsupported
}

func (NoopClient) SearchOpenIssues(context.Context, Repository, string) ([]Issue, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) CreateIssue(context.Context, CreateIssueInput) (Issue, error) {
	return Issue{}, errors.ErrUnsupported
}

func (NoopClient) CloseIssue(context.Context, Repository, int64) (Issue, error) {
	return Issue{}, errors.ErrUnsupported
}

func (NoopClient) ListIssueComments(context.Context, Repository, int64) ([]IssueComment, error) {
	return nil, errors.ErrUnsupported
}

func (NoopClient) ListRepoIssueComments(context.Context, Repository, time.Time) ([]IssueComment, error) {
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

func (NoopClient) UpdatePullRequestBranch(context.Context, UpdatePullRequestBranchInput) (UpdatePullRequestBranchResult, error) {
	return UpdatePullRequestBranchResult{}, errors.ErrUnsupported
}

func (NoopClient) GetCombinedStatus(context.Context, Repository, string) (CombinedStatus, error) {
	return CombinedStatus{}, errors.ErrUnsupported
}

func (NoopClient) ListCheckRunsForRef(context.Context, Repository, string) ([]PullRequestCheck, error) {
	return nil, errors.ErrUnsupported
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

func (NoopClient) UpsertFile(context.Context, UpsertFileInput) (RepositoryFile, error) {
	return RepositoryFile{}, errors.ErrUnsupported
}
