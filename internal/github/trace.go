package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TraceIssue is the issue projection needed by the on-demand issue trace.
type TraceIssue struct {
	Number    int64  `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	URL       string `json:"html_url"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	ClosedAt  string `json:"closed_at"`
}

// TracePullRequest is the PR projection needed by the on-demand issue trace.
type TracePullRequest struct {
	Number    int64  `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	URL       string `json:"html_url"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	MergedAt  string `json:"merged_at"`
	MergeSHA  string `json:"merge_commit_sha"`
	Head      struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// TraceCrossReference is a cross-reference event from an issue or PR timeline.
type TraceCrossReference struct {
	CreatedAt string `json:"created_at"`
	Source    struct {
		Issue struct {
			Number        int64           `json:"number"`
			Title         string          `json:"title"`
			Body          string          `json:"body"`
			URL           string          `json:"html_url"`
			RepositoryURL string          `json:"repository_url"`
			PullRequest   json.RawMessage `json:"pull_request"`
		} `json:"issue"`
	} `json:"source"`
}

// IsPullRequest reports whether the cross-reference source is a pull request.
func (r TraceCrossReference) IsPullRequest() bool {
	return len(r.Source.Issue.PullRequest) > 0 && string(r.Source.Issue.PullRequest) != "null"
}

// TraceRepository is the repository projection needed for ancestry evidence.
type TraceRepository struct {
	DefaultBranch string `json:"default_branch"`
}

// GetTraceIssue returns one issue through the shared limiter and ETag cache.
func (c *GhClient) GetTraceIssue(ctx context.Context, repo Repository, number int64) (TraceIssue, error) {
	var issue TraceIssue
	if err := c.conditionalTraceJSON(ctx, repo, &issue, endpoint(repo, "issues", number)); err != nil {
		return TraceIssue{}, err
	}
	return issue, nil
}

// GetTracePullRequest returns one pull request through the shared limiter and
// ETag cache.
func (c *GhClient) GetTracePullRequest(ctx context.Context, repo Repository, number int64) (TracePullRequest, error) {
	var pr TracePullRequest
	if err := c.conditionalTraceJSON(ctx, repo, &pr, endpoint(repo, "pulls", number)); err != nil {
		return TracePullRequest{}, err
	}
	return pr, nil
}

// ListTraceCrossReferences returns every cross-reference in an issue or PR
// timeline. A full first page falls back to the existing complete pagination
// helper rather than retaining a partial ETag entry.
func (c *GhClient) ListTraceCrossReferences(ctx context.Context, repo Repository, number int64) ([]TraceCrossReference, error) {
	args := []string{
		"-X", "GET", endpoint(repo, "issues", number, "timeline"),
		"-H", "Accept: application/vnd.github+json",
		"-f", "per_page=100",
	}
	if !conditionalEnabled() {
		return apiPaginatedJSON[TraceCrossReference](ctx, c, args...)
	}
	page, key, err := conditionalPageJSON[TraceCrossReference](ctx, c, repo, args...)
	if err != nil {
		return nil, err
	}
	if len(page) < 100 {
		return page, nil
	}
	evictConditionalEntry(key)
	return apiPaginatedJSON[TraceCrossReference](ctx, c,
		"-X", "GET", endpoint(repo, "issues", number, "timeline"),
		"-H", "Accept: application/vnd.github+json",
		"-f", "per_page=100",
	)
}

// SearchTracePullRequests performs a narrow same-repository PR search. The
// resolver applies exact closing-keyword and successor-title matching to these
// candidates; GitHub search is only candidate discovery.
func (c *GhClient) SearchTracePullRequests(ctx context.Context, repo Repository, text string) ([]TracePullRequest, error) {
	var response struct {
		TotalCount int `json:"total_count"`
		Items      []struct {
			Number    int64  `json:"number"`
			Title     string `json:"title"`
			Body      string `json:"body"`
			URL       string `json:"html_url"`
			State     string `json:"state"`
			CreatedAt string `json:"created_at"`
		} `json:"items"`
	}
	query := fmt.Sprintf("repo:%s is:pr %s", repo.FullName(), strings.TrimSpace(text))
	args := []string{"-X", "GET", "search/issues", "-f", "q=" + query, "-f", "per_page=100"}
	if !conditionalEnabled() {
		return apiPaginatedJSON[TracePullRequest](ctx, c,
			"--jq", ".items", "-X", "GET", "search/issues",
			"-f", "q="+query, "-f", "per_page=100",
		)
	}
	result, key, err := c.conditionalRun(ctx, repo, args...)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		evictConditionalEntry(key)
		return nil, fmt.Errorf("decode gh api response: %w", err)
	}
	if response.TotalCount > len(response.Items) && len(response.Items) == 100 {
		evictConditionalEntry(key)
		return apiPaginatedJSON[TracePullRequest](ctx, c,
			"--jq", ".items", "-X", "GET", "search/issues",
			"-f", "q="+query, "-f", "per_page=100",
		)
	}
	out := make([]TracePullRequest, 0, len(response.Items))
	for _, item := range response.Items {
		out = append(out, TracePullRequest{
			Number: item.Number, Title: item.Title, Body: item.Body, URL: item.URL,
			State: item.State, CreatedAt: item.CreatedAt,
		})
	}
	return out, nil
}

// GetTraceRepository returns the default branch through the shared conditional
// request path.
func (c *GhClient) GetTraceRepository(ctx context.Context, repo Repository) (TraceRepository, error) {
	var metadata TraceRepository
	if err := c.conditionalTraceJSON(ctx, repo, &metadata, endpoint(repo)); err != nil {
		return TraceRepository{}, err
	}
	return metadata, nil
}

// CompareTraceCommit compares an observed commit to a branch through the shared
// conditional request path.
func (c *GhClient) CompareTraceCommit(ctx context.Context, repo Repository, commit, branch string) (CompareResult, error) {
	var result CompareResult
	if err := c.conditionalTraceJSON(ctx, repo, &result, endpoint(repo, "compare", commit+"..."+branch)); err != nil {
		return CompareResult{}, err
	}
	return result, nil
}

func (c *GhClient) conditionalTraceJSON(ctx context.Context, repo Repository, output any, args ...string) error {
	if !conditionalEnabled() {
		return c.apiJSON(ctx, false, output, args...)
	}
	result, key, err := c.conditionalRun(ctx, repo, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(result.Stdout), output); err != nil {
		evictConditionalEntry(key)
		return fmt.Errorf("decode gh api response: %w", err)
	}
	return nil
}
