package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func TestRepositoryExists(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}

	t.Run("exists", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"nameWithOwner":"o/r"}`}}}
		client := GhClient{Runner: runner}
		ok, err := client.RepositoryExists(context.Background(), repo)
		if err != nil || !ok {
			t.Fatalf("RepositoryExists = (%v, %v), want (true, nil)", ok, err)
		}
		runner.wantArgs(t, 0, "repo", "view", "o/r", "--json", "nameWithOwner")
	})

	t.Run("not found", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "HTTP 404: Not Found (https://api.github.com/repos/o/r)"}},
			errs:    []error{errors.New("exit status 1")},
		}
		client := GhClient{Runner: runner}
		ok, err := client.RepositoryExists(context.Background(), repo)
		if err != nil || ok {
			t.Fatalf("RepositoryExists = (%v, %v), want (false, nil)", ok, err)
		}
	})

	t.Run("graphql could-not-resolve maps to not found", func(t *testing.T) {
		// `gh repo view` reports a missing repo via the GraphQL resolver rather
		// than HTTP 404; this is the phrasing that previously dead-ended the
		// setup pickers' create offer.
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "GraphQL: Could not resolve to a Repository with the name 'o/r'. (repository)"}},
			errs:    []error{errors.New("exit status 1")},
		}
		client := GhClient{Runner: runner}
		ok, err := client.RepositoryExists(context.Background(), repo)
		if err != nil || ok {
			t.Fatalf("RepositoryExists = (%v, %v), want (false, nil) for could-not-resolve", ok, err)
		}
	})

	t.Run("auth error propagates", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "gh: To get started with GitHub CLI, please run: gh auth login"}},
			errs:    []error{errors.New("exit status 4")},
		}
		client := GhClient{Runner: runner}
		ok, err := client.RepositoryExists(context.Background(), repo)
		if err == nil || ok {
			t.Fatalf("RepositoryExists = (%v, %v), want (false, non-nil) on auth error", ok, err)
		}
	})

	t.Run("empty repo", func(t *testing.T) {
		client := GhClient{Runner: &fakeRunner{}}
		if _, err := client.RepositoryExists(context.Background(), Repository{}); err == nil {
			t.Fatal("expected error for empty repo")
		}
	})
}

func TestCreateRepository(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}

	t.Run("private", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{}}}
		client := GhClient{Runner: runner}
		if err := client.CreateRepository(context.Background(), repo, true); err != nil {
			t.Fatalf("CreateRepository: %v", err)
		}
		runner.wantArgs(t, 0, "repo", "create", "o/r", "--private", "--add-readme")
	})

	t.Run("public", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{}}}
		client := GhClient{Runner: runner}
		if err := client.CreateRepository(context.Background(), repo, false); err != nil {
			t.Fatalf("CreateRepository: %v", err)
		}
		runner.wantArgs(t, 0, "repo", "create", "o/r", "--public", "--add-readme")
	})

	t.Run("error propagates", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "name already exists"}},
			errs:    []error{errors.New("exit status 1")},
		}
		client := GhClient{Runner: runner}
		if err := client.CreateRepository(context.Background(), repo, true); err == nil {
			t.Fatal("expected error to propagate")
		}
	})
}

func TestDeleteFile(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}

	t.Run("deletes existing file", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"sha":"blob-sha"}`}, {Stdout: `{}`}}}
		client := GhClient{Runner: runner, MaxRetries: 1}
		file, err := client.DeleteFile(context.Background(), DeleteFileInput{
			Repo: repo, Path: "pipelines/nightly/old.md", Message: "Publish pipeline nightly", Branch: "main",
		})
		if err != nil {
			t.Fatalf("DeleteFile: %v", err)
		}
		if file.Path != "pipelines/nightly/old.md" || file.SHA != "blob-sha" {
			t.Fatalf("DeleteFile result = %+v", file)
		}
		runner.wantArgs(t, 0, "api", "-X", "GET", "repos/o/r/contents/pipelines/nightly/old.md", "-f", "ref=main")
		runner.wantArgs(t, 1, "api", "-X", "DELETE", "repos/o/r/contents/pipelines/nightly/old.md", "-f", "message=Publish pipeline nightly", "-f", "sha=blob-sha", "-f", "branch=main")
	})

	t.Run("already missing is idempotent", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "gh: Not Found (HTTP 404)"}},
			errs:    []error{errors.New("exit status 1")},
		}
		client := GhClient{Runner: runner, MaxRetries: 1}
		file, err := client.DeleteFile(context.Background(), DeleteFileInput{Repo: repo, Path: "gone.md"})
		if err != nil || file.Path != "gone.md" {
			t.Fatalf("DeleteFile missing = (%+v, %v)", file, err)
		}
		if len(runner.calls) != 1 {
			t.Fatalf("DeleteFile missing calls = %d", len(runner.calls))
		}
	})
}

func TestCloneRepository(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}

	t.Run("clones to dir", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{}}}
		client := GhClient{Runner: runner}
		if err := client.CloneRepository(context.Background(), repo, "/tmp/o-r"); err != nil {
			t.Fatalf("CloneRepository: %v", err)
		}
		runner.wantArgs(t, 0, "repo", "clone", "o/r", "/tmp/o-r")
	})

	t.Run("requires a destination", func(t *testing.T) {
		client := GhClient{Runner: &fakeRunner{results: []subprocess.Result{{}}}}
		if err := client.CloneRepository(context.Background(), repo, ""); err == nil {
			t.Fatal("expected an error for an empty destination")
		}
	})
}

func TestDeleteRepository(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}

	t.Run("deletes", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{}}}
		client := GhClient{Runner: runner}
		if err := client.DeleteRepository(context.Background(), repo); err != nil {
			t.Fatalf("DeleteRepository: %v", err)
		}
		runner.wantArgs(t, 0, "repo", "delete", "o/r", "--yes")
	})

	t.Run("missing delete_repo scope maps to remedy", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "HTTP 403: Must have admin rights... This API operation needs the \"delete_repo\" scope"}},
			errs:    []error{errors.New("exit status 1")},
		}
		client := GhClient{Runner: runner}
		err := client.DeleteRepository(context.Background(), repo)
		if err == nil || !strings.Contains(err.Error(), "gh auth refresh -h github.com -s delete_repo") {
			t.Fatalf("expected the scope remedy in the error, got %v", err)
		}
	})

	t.Run("other errors propagate unchanged", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "HTTP 404: Not Found"}},
			errs:    []error{errors.New("exit status 1")},
		}
		client := GhClient{Runner: runner}
		err := client.DeleteRepository(context.Background(), repo)
		if err == nil || strings.Contains(err.Error(), "delete_repo") {
			t.Fatalf("non-scope error should propagate without the remedy, got %v", err)
		}
	})

	t.Run("empty repo", func(t *testing.T) {
		client := GhClient{Runner: &fakeRunner{}}
		if err := client.DeleteRepository(context.Background(), Repository{}); err == nil {
			t.Fatal("expected error for empty repo")
		}
	})
}

func TestListIssueCommentsDedupesByID(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `[
				{"id": 11, "body": "first", "html_url": "https://example.com/1", "user": {"login": "alice"}},
				{"id": 11, "body": "duplicate", "html_url": "https://example.com/1", "user": {"login": "alice"}}
			]
			[
				{"id": 12, "body": "second", "html_url": "https://example.com/2", "user": {"login": "bob"}}
			]`,
		}},
	}
	client := GhClient{Runner: runner}

	comments, err := client.ListIssueComments(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListIssueComments returned error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments length = %d, want 2", len(comments))
	}
	if comments[0].Body != "first" || comments[1].Author != "bob" {
		t.Fatalf("comments were not decoded in first-seen order: %+v", comments)
	}
	runner.wantArgs(t, 0, "api", "--paginate", "repos/gitmoot/gitmoot/issues/2/comments")
}

func TestListRepoIssueCommentsPassesSinceAndGroupsByIssue(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			// Single-page (apiPageJSON) repo-wide payload: comments for a plain issue
			// (#41) AND a PR (#42, every PR is an issue), each carrying issue_url.
			Stdout: `[
				{"id": 11, "body": "@a ask on issue", "issue_url": "https://api.github.com/repos/gitmoot/gitmoot/issues/41", "updated_at": "2026-06-27T10:00:00Z", "user": {"login": "alice"}},
				{"id": 12, "body": "@a ask on PR", "issue_url": "https://api.github.com/repos/gitmoot/gitmoot/issues/42", "updated_at": "2026-06-27T11:00:00Z", "user": {"login": "bob"}},
				{"id": 11, "body": "duplicate", "issue_url": "https://api.github.com/repos/gitmoot/gitmoot/issues/41", "updated_at": "2026-06-27T10:00:00Z", "user": {"login": "alice"}}
			]`,
		}},
	}
	client := GhClient{Runner: runner}

	since := time.Date(2026, 6, 27, 9, 30, 0, 0, time.UTC)
	comments, err := client.ListRepoIssueComments(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, since)
	if err != nil {
		t.Fatalf("ListRepoIssueComments returned error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments length = %d, want 2 (deduped by id): %+v", len(comments), comments)
	}
	// PR conversation comments are INCLUDED (the @mention command channel).
	if comments[0].IssueNumber != 41 || comments[1].IssueNumber != 42 {
		t.Fatalf("IssueNumber not parsed from issue_url: %+v", comments)
	}
	if comments[1].Author != "bob" {
		t.Fatalf("author decode: %+v", comments[1])
	}
	// One bounded page (no --paginate), since passthrough as RFC3339, oldest-first.
	runner.wantArgs(t, 0, "api", "-i", "-X", "GET", "repos/gitmoot/gitmoot/issues/comments",
		"-f", "sort=updated", "-f", "direction=asc", "-f", "per_page=100",
		"-f", "since=2026-06-27T09:30:00Z")
}

func TestListRepoIssueCommentsZeroSinceOmitsFilter(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `[]`}}}
	client := GhClient{Runner: runner}

	if _, err := client.ListRepoIssueComments(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, time.Time{}); err != nil {
		t.Fatalf("ListRepoIssueComments returned error: %v", err)
	}
	// A zero since omits the since= arg entirely (no whole-history backfill trigger).
	runner.wantArgs(t, 0, "api", "-i", "-X", "GET", "repos/gitmoot/gitmoot/issues/comments",
		"-f", "sort=updated", "-f", "direction=asc", "-f", "per_page=100")
}

func TestPostIssueCommentUsesIssueCommentsEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"id": 21, "body": "done", "html_url": "https://github.com/gitmoot/gitmoot/pull/2#issuecomment-21", "user": {"login": "gitmoot"}}`,
		}},
	}
	client := GhClient{Runner: runner}

	comment, err := client.PostIssueComment(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 2, "done")

	if err != nil {
		t.Fatalf("PostIssueComment returned error: %v", err)
	}
	if comment.ID != 21 || comment.Body != "done" {
		t.Fatalf("comment = %+v", comment)
	}
	runner.wantArgs(t, 0, "api", "repos/gitmoot/gitmoot/issues/2/comments", "-f", "body=done")
}

func TestCreateIssueUsesIssuesEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"number": 8, "title": "Review run-1", "state": "open", "html_url": "https://github.com/gitmoot/gitmoot/issues/8"}`,
		}},
	}
	client := GhClient{Runner: runner}

	issue, err := client.CreateIssue(context.Background(), CreateIssueInput{
		Repo:  Repository{Owner: "gitmoot", Name: "gitmoot"},
		Title: "Review run-1",
		Body:  "body",
	})

	if err != nil {
		t.Fatalf("CreateIssue returned error: %v", err)
	}
	if issue.Number != 8 || issue.URL != "https://github.com/gitmoot/gitmoot/issues/8" {
		t.Fatalf("issue = %+v", issue)
	}
	runner.wantArgs(t, 0, "api", "repos/gitmoot/gitmoot/issues", "-f", "title=Review run-1", "-f", "body=body")
}

func TestCreateIssueAppliesLabelsBestEffort(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: `{"number": 8, "title": "Bug", "state": "open", "html_url": "https://github.com/gitmoot/gitmoot/issues/8"}`},
			{Stderr: "HTTP 422: Validation Failed"},
			{Stderr: "HTTP 422: Validation Failed"},
			{Stderr: "HTTP 422: Validation Failed"},
		},
		errs: []error{
			nil,
			errors.New("exit status 1"),
			errors.New("exit status 1"),
			errors.New("exit status 1"),
		},
	}
	client := GhClient{Runner: runner}

	issue, err := client.CreateIssue(context.Background(), CreateIssueInput{
		Repo:   Repository{Owner: "gitmoot", Name: "gitmoot"},
		Title:  "Bug",
		Body:   "body",
		Labels: []string{"gitmoot-dashboard-report", "bug"},
	})

	if err != nil {
		t.Fatalf("CreateIssue returned error: %v", err)
	}
	if issue.Number != 8 {
		t.Fatalf("issue = %+v", issue)
	}
	runner.wantArgs(t, 0, "api", "repos/gitmoot/gitmoot/issues", "-f", "title=Bug", "-f", "body=body")
	runner.wantArgs(t, 1, "api", "repos/gitmoot/gitmoot/labels", "-f", "name=gitmoot-dashboard-report", "-f", "color=5319e7", "-f", "description=Gitmoot-generated bug report")
	runner.wantArgs(t, 2, "api", "repos/gitmoot/gitmoot/labels", "-f", "name=bug", "-f", "color=d73a4a", "-f", "description=Something is not working")
	runner.wantArgs(t, 3, "api", "repos/gitmoot/gitmoot/issues/8/labels", "-f", "labels[]=gitmoot-dashboard-report", "-f", "labels[]=bug")
}

func TestSearchOpenIssuesUsesIssueSearchEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"items":[{"number":8,"title":"Bug","state":"open","html_url":"https://github.com/gitmoot/gitmoot/issues/8","body":"<!-- gitmoot:dashboard-report fingerprint:abc -->"}]}`,
		}},
	}
	client := GhClient{Runner: runner}

	issues, err := client.SearchOpenIssues(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, "<!-- gitmoot:dashboard-report fingerprint:abc -->")

	if err != nil {
		t.Fatalf("SearchOpenIssues returned error: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 8 || !strings.Contains(issues[0].Body, "fingerprint:abc") {
		t.Fatalf("issues = %+v", issues)
	}
	runner.wantArgs(t, 0, "api", "-X", "GET", "search/issues", "-f", `q=repo:gitmoot/gitmoot is:issue is:open in:body "<!-- gitmoot:dashboard-report fingerprint:abc -->"`)
}

func TestListIssuesFiltersOutPullRequests(t *testing.T) {
	// GET /repos/{o}/{r}/issues returns issues AND PRs; PRs carry a
	// `pull_request` object. ListIssues must drop those so the issue-comment
	// watcher does not duplicate the PR-watcher.
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `[
				{"number":42,"title":"A real issue","state":"open","html_url":"https://github.com/gitmoot/gitmoot/issues/42","body":"please help"},
				{"number":43,"title":"A pull request","state":"open","html_url":"https://github.com/gitmoot/gitmoot/pull/43","pull_request":{"url":"https://api.github.com/repos/gitmoot/gitmoot/pulls/43"}}
			]`,
		}},
	}
	client := GhClient{Runner: runner}

	issues, err := client.ListIssues(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, "open")
	if err != nil {
		t.Fatalf("ListIssues returned error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("ListIssues returned %d issues, want 1 (PR filtered out): %+v", len(issues), issues)
	}
	if issues[0].Number != 42 || issues[0].IsPullRequest {
		t.Fatalf("issues[0] = %+v, want plain issue #42", issues[0])
	}
	runner.wantArgs(t, 0, "api", "-i", "-X", "GET", "repos/gitmoot/gitmoot/issues", "-f", "state=open", "-f", "per_page=100")
}

func TestPreflightChecksGhAuthAndRepoAccess(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: "gh version 2.45.0"},
			{Stdout: "Logged in to github.com"},
			{Stdout: `{"nameWithOwner":"gitmoot/gitmoot"}`},
		},
	}
	client := GhClient{Runner: runner}

	if err := client.Preflight(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}); err != nil {
		t.Fatalf("Preflight returned error: %v", err)
	}

	runner.wantArgs(t, 0, "--version")
	runner.wantArgs(t, 1, "auth", "status", "--hostname", "github.com")
	runner.wantArgs(t, 2, "repo", "view", "gitmoot/gitmoot", "--json", "nameWithOwner")
}

func TestPreflightReportsAuthLoginHint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: "gh version 2.45.0"},
			{Stderr: "not logged in"},
		},
		errs: []error{nil, errors.New("exit status 1")},
	}
	client := GhClient{Runner: runner}

	err := client.Preflight(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"})

	if err == nil || !strings.Contains(err.Error(), "gh auth login --hostname github.com") {
		t.Fatalf("Preflight error = %v, want auth login hint", err)
	}
	runner.wantArgs(t, 1, "auth", "status", "--hostname", "github.com")
}

func TestPreflightReportsExpectedRepoAccess(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: "gh version 2.45.0"},
			{Stdout: "Logged in to github.com"},
			{Stderr: "Could not resolve repository"},
		},
		errs: []error{nil, nil, errors.New("exit status 1")},
	}
	client := GhClient{Runner: runner}

	err := client.Preflight(context.Background(), Repository{Owner: "owner", Name: "previews"})

	if err == nil || !strings.Contains(err.Error(), "cannot view expected repo owner/previews") {
		t.Fatalf("Preflight error = %v, want expected repo access error", err)
	}
	runner.wantArgs(t, 2, "repo", "view", "owner/previews", "--json", "nameWithOwner")
}

func TestCloseIssueUsesIssueEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"number": 8, "title": "Review run-1", "state": "closed", "html_url": "https://github.com/gitmoot/gitmoot/issues/8"}`,
		}},
	}
	client := GhClient{Runner: runner}

	issue, err := client.CloseIssue(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 8)

	if err != nil {
		t.Fatalf("CloseIssue returned error: %v", err)
	}
	if issue.Number != 8 || issue.State != "closed" {
		t.Fatalf("issue = %+v", issue)
	}
	runner.wantArgs(t, 0, "api", "-X", "PATCH", "repos/gitmoot/gitmoot/issues/8", "-f", "state=closed")
}

func TestGetUserPermissionUsesCollaboratorPermissionEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"permission": "write", "role_name": "write"}`,
		}},
	}
	client := GhClient{Runner: runner}

	permission, err := client.GetUserPermission(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, "alice")

	if err != nil {
		t.Fatalf("GetUserPermission returned error: %v", err)
	}
	if permission.Permission != "write" || permission.RoleName != "write" {
		t.Fatalf("permission = %+v", permission)
	}
	runner.wantArgs(t, 0, "api", "repos/gitmoot/gitmoot/collaborators/alice/permission")
}

func TestGetUserPermissionMapsNotFoundToNone(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "HTTP 404: Not Found"}},
		errs:    []error{errors.New("exit status 1")},
	}
	client := GhClient{Runner: runner}

	permission, err := client.GetUserPermission(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, "mallory")

	if err != nil {
		t.Fatalf("GetUserPermission returned error: %v", err)
	}
	if permission.Permission != "none" {
		t.Fatalf("permission = %+v, want none", permission)
	}
}

func TestCreateCommitStatusUsesStatusesEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"id": 31, "state": "success", "context": "gitmoot/task", "description": "ok", "target_url": "https://example.com"}`,
		}},
	}
	client := GhClient{Runner: runner}

	status, err := client.CreateCommitStatus(context.Background(), CommitStatusInput{
		Repo:        Repository{Owner: "gitmoot", Name: "gitmoot"},
		SHA:         "abc123",
		State:       "success",
		Context:     "gitmoot/task",
		Description: "ok",
		TargetURL:   "https://example.com",
	})

	if err != nil {
		t.Fatalf("CreateCommitStatus returned error: %v", err)
	}
	if status.ID != 31 || status.State != "success" {
		t.Fatalf("status = %+v", status)
	}
	runner.wantArgs(t, 0,
		"api",
		"repos/gitmoot/gitmoot/statuses/abc123",
		"-f", "state=success",
		"-f", "context=gitmoot/task",
		"-f", "description=ok",
		"-f", "target_url=https://example.com",
	)
}

func TestGetPullRequestDecodesBaseSHA(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"number": 2, "title": "Task", "state": "open", "html_url": "https://github.com/gitmoot/gitmoot/pull/2", "head": {"ref": "task", "sha": "head123"}, "base": {"ref": "main", "sha": "base123"}}`,
		}},
	}
	client := GhClient{Runner: runner}

	pr, err := client.GetPullRequest(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.HeadSHA != "head123" || pr.BaseSHA != "base123" {
		t.Fatalf("pull request = %+v", pr)
	}
	runner.wantArgs(t, 0, "api", "repos/gitmoot/gitmoot/pulls/2")
}

func TestGetPullRequestDecodesHeadRepository(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"number": 2, "state": "open", "head": {"ref": "task", "sha": "head123", "repo": {"full_name": "gitmoot/gitmoot"}}, "base": {"ref": "main"}}`,
		}},
	}
	client := GhClient{Runner: runner}

	pr, err := client.GetPullRequest(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 2)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.HeadRepoFullName != "gitmoot/gitmoot" {
		t.Fatalf("head repository = %q, want gitmoot/gitmoot", pr.HeadRepoFullName)
	}
}

// TestGetPullRequestDecodesBody proves PullRequest.UnmarshalJSON decodes the wire
// `body` field into the additive PullRequest.Body (#467) so the daemon's revert
// detection can read a GitHub Revert-button body (`Reverts owner/repo#NN`).
func TestGetPullRequestDecodesBody(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"number": 9, "title": "Revert \"Add feature\"", "state": "closed", "merged": true, "html_url": "https://github.com/gitmoot/gitmoot/pull/9", "body": "Reverts gitmoot/gitmoot#7", "head": {"ref": "revert-7", "sha": "rev123"}, "base": {"ref": "main", "sha": "base123"}}`,
		}},
	}
	client := GhClient{Runner: runner}

	pr, err := client.GetPullRequest(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 9)

	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.Body != "Reverts gitmoot/gitmoot#7" {
		t.Fatalf("pull request body = %q, want the Reverts anchor", pr.Body)
	}
	if !pr.Merged {
		t.Fatalf("pull request merged = %v, want true", pr.Merged)
	}
}

// TestGetPullRequestBodyDefaultsEmpty proves Body defaults to "" when the wire
// payload omits `body` — the additive field is byte-identical for every existing
// caller that ignores it.
func TestGetPullRequestBodyDefaultsEmpty(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"number": 2, "title": "Task", "state": "open", "html_url": "https://github.com/gitmoot/gitmoot/pull/2", "head": {"ref": "task", "sha": "head123"}, "base": {"ref": "main", "sha": "base123"}}`,
		}},
	}
	client := GhClient{Runner: runner}

	pr, err := client.GetPullRequest(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.Body != "" {
		t.Fatalf("pull request body = %q, want empty default", pr.Body)
	}
}

// TestListRecentClosedPullRequestsDecodesMergedAt proves the bounded closed-PR
// scan (#467) decodes the LIST endpoint's real merged shape: a merged PR comes
// back as state="closed" with merged_at set and NO `merged` boolean. revert
// detection gates on merged_at, so this field must survive decoding.
func TestListRecentClosedPullRequestsDecodesMergedAt(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `[{"number": 20, "title": "Revert \"Task 7\"", "state": "closed", "merged_at": "2026-06-27T12:00:00Z", "html_url": "https://github.com/gitmoot/gitmoot/pull/20", "body": "Reverts #7", "head": {"ref": "revert-task-7"}, "base": {"ref": "main"}}]`,
		}},
	}
	client := GhClient{Runner: runner}

	pulls, err := client.ListRecentClosedPullRequests(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"})
	if err != nil {
		t.Fatalf("ListRecentClosedPullRequests returned error: %v", err)
	}
	if len(pulls) != 1 {
		t.Fatalf("pulls = %d, want 1", len(pulls))
	}
	if pulls[0].MergedAt != "2026-06-27T12:00:00Z" {
		t.Fatalf("merged_at = %q, want the merge timestamp", pulls[0].MergedAt)
	}
	if pulls[0].Merged {
		t.Fatalf("list endpoint must not set Merged; got %v", pulls[0].Merged)
	}

	// The scan must be BOUNDED: a single page (no --paginate) sorted by recency.
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.calls))
	}
	args := strings.Join(runner.calls[0], " ")
	if strings.Contains(args, "--paginate") {
		t.Fatalf("recent closed scan must NOT paginate the whole history; args = %q", args)
	}
	for _, want := range []string{"state=closed", "sort=updated", "direction=desc", "per_page="} {
		if !strings.Contains(args, want) {
			t.Fatalf("recent closed scan args missing %q; args = %q", want, args)
		}
	}
}

func TestCompareCommitsUsesEscapedCompareEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"status": "ahead", "ahead_by": 3, "behind_by": 0}`,
		}},
	}
	client := GhClient{Runner: runner}

	compare, err := client.CompareCommits(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, "release/1.0", "head123")

	if err != nil {
		t.Fatalf("CompareCommits returned error: %v", err)
	}
	if compare.Status != "ahead" || compare.AheadBy != 3 || compare.BehindBy != 0 {
		t.Fatalf("compare = %+v", compare)
	}
	runner.wantArgs(t, 0, "api", "repos/gitmoot/gitmoot/compare/release%2F1.0...head123")
}

func TestUpdatePullRequestBranchUsesExpectedHeadSHA(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"message":"Updating pull request branch.","url":"https://github.com/repos/gitmoot/gitmoot/pulls/2"}`,
		}},
	}
	client := GhClient{Runner: runner}

	result, err := client.UpdatePullRequestBranch(context.Background(), UpdatePullRequestBranchInput{
		Repo:            Repository{Owner: "gitmoot", Name: "gitmoot"},
		Number:          2,
		ExpectedHeadSHA: "head123",
	})

	if err != nil {
		t.Fatalf("UpdatePullRequestBranch returned error: %v", err)
	}
	if result.Message != "Updating pull request branch." || result.URL == "" {
		t.Fatalf("result = %+v", result)
	}
	runner.wantArgs(t, 0,
		"api",
		"-X", "PUT",
		"repos/gitmoot/gitmoot/pulls/2/update-branch",
		"-f", "expected_head_sha=head123",
	)
}

func TestUpdatePullRequestBranchClassifiesStaleHead(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "HTTP 422: expected_head_sha does not match"}},
		errs:    []error{errors.New("exit status 1")},
	}
	client := GhClient{Runner: runner}

	_, err := client.UpdatePullRequestBranch(context.Background(), UpdatePullRequestBranchInput{
		Repo:            Repository{Owner: "gitmoot", Name: "gitmoot"},
		Number:          2,
		ExpectedHeadSHA: "old",
	})

	if !IsUpdatePullRequestBranchError(err, UpdatePullRequestBranchErrorStaleHead) {
		t.Fatalf("error = %v, want stale head", err)
	}
}

func TestUpdatePullRequestBranchClassifiesConflict(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "HTTP 422: branch cannot be cleanly merged due to conflicts"}},
		errs:    []error{errors.New("exit status 1")},
	}
	client := GhClient{Runner: runner}

	_, err := client.UpdatePullRequestBranch(context.Background(), UpdatePullRequestBranchInput{
		Repo:            Repository{Owner: "gitmoot", Name: "gitmoot"},
		Number:          2,
		ExpectedHeadSHA: "head123",
	})

	if !IsUpdatePullRequestBranchError(err, UpdatePullRequestBranchErrorConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestUpdatePullRequestBranchClassifiesUnsupported(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "HTTP 403: Resource not accessible by integration; missing permission to write head repository"}},
		errs:    []error{errors.New("exit status 1")},
	}
	client := GhClient{Runner: runner}

	_, err := client.UpdatePullRequestBranch(context.Background(), UpdatePullRequestBranchInput{
		Repo:            Repository{Owner: "gitmoot", Name: "gitmoot"},
		Number:          2,
		ExpectedHeadSHA: "head123",
	})

	if !IsUpdatePullRequestBranchError(err, UpdatePullRequestBranchErrorUnsupported) {
		t.Fatalf("error = %v, want unsupported", err)
	}
}

func TestListPullRequestChecksUsesGhChecksOutput(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `[
				{"name": "test", "state": "SUCCESS", "bucket": "pass", "link": "https://example.com/check", "workflow": "ci"},
				{"name": "lint", "state": "PENDING", "bucket": "pending", "workflow": "ci"}
			]`,
		}},
	}
	client := GhClient{Runner: runner}

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListPullRequestChecks returned error: %v", err)
	}
	if len(checks) != 2 || checks[0].Bucket != "pass" || checks[1].Bucket != "pending" {
		t.Fatalf("checks = %+v", checks)
	}
	runner.wantArgs(t, 0,
		"pr", "checks", "2",
		"--repo", "gitmoot/gitmoot",
		"--json", "name,state,bucket,link,workflow,completedAt",
	)
}

func TestListCheckRunsForRefUsesExactCommitSHAAndPaginates(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `
{"name":"page-1-build","status":"completed","conclusion":"success","html_url":"https://example.test/build","completed_at":"2026-07-23T10:00:00Z"}

{"name":"page-2-lint","status":"in_progress","conclusion":"","html_url":"https://example.test/lint","completed_at":null}
`}}}
	client := GhClient{Runner: runner}

	checks, err := client.ListCheckRunsForRef(
		context.Background(),
		Repository{Owner: "gitmoot", Name: "gitmoot"},
		"head123",
	)
	if err != nil {
		t.Fatalf("ListCheckRunsForRef: %v", err)
	}
	if len(checks) != 2 ||
		checks[0].Name != "page-1-build" || checks[0].State != "success" ||
		checks[1].Name != "page-2-lint" || checks[1].State != "in_progress" {
		t.Fatalf("checks = %+v", checks)
	}
	runner.wantArgs(t, 0,
		"api", "--paginate", "--jq",
		".check_runs[] | {name, status, conclusion, html_url, completed_at}",
		"repos/gitmoot/gitmoot/commits/head123/check-runs?per_page=100",
	)
}

func TestListCheckRunsForRefRejectsMalformedNDJSON(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `
{"name":"build","status":"completed","conclusion":"success"}
not-json
{"name":"lint","status":"completed","conclusion":"success"}
`}}}
	client := GhClient{Runner: runner}

	_, err := client.ListCheckRunsForRef(
		context.Background(),
		Repository{Owner: "gitmoot", Name: "gitmoot"},
		"head123",
	)
	if err == nil || !strings.Contains(err.Error(), "decode gh check-run line 3") {
		t.Fatalf("error = %v, want malformed NDJSON line", err)
	}
}

func TestListPullRequestChecksAcceptsPendingExitWithJSON(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `[{"name": "test", "state": "PENDING", "bucket": "pending"}]`,
		}},
		errs: []error{errors.New("exit status 8")},
	}
	client := GhClient{Runner: runner}

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListPullRequestChecks returned error: %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != "pending" {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestListPullRequestChecksTreatsNoChecksAsEmpty(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stderr: "no checks reported on the 'task' branch",
		}},
		errs: []error{errors.New("exit status 1")},
	}
	client := GhClient{Runner: runner}

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListPullRequestChecks returned error: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("checks = %+v, want empty", checks)
	}
}

func TestListPullRequestChecksFallsBackToStatusCheckRollup(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "unknown flag: --json"},
			{Stdout: `{"statusCheckRollup":[
				{"name":"test","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://example.com/check","workflowName":"ci"},
				{"name":"lint","status":"IN_PROGRESS","workflowName":"ci"},
				{"context":"legacy","state":"FAILURE","targetUrl":"https://example.com/status"},
				{"context":"required","state":"EXPECTED"}
			]}`},
		},
		errs: []error{errors.New("exit status 1"), nil},
	}
	client := GhClient{Runner: runner}

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListPullRequestChecks returned error: %v", err)
	}
	if len(checks) != 4 {
		t.Fatalf("checks = %+v, want 4", checks)
	}
	if checks[0].Bucket != "pass" || checks[1].Bucket != "pending" || checks[2].Bucket != "fail" || checks[3].Bucket != "pending" {
		t.Fatalf("checks = %+v", checks)
	}
	if checks[2].Name != "legacy" || checks[2].Link != "https://example.com/status" {
		t.Fatalf("legacy status check = %+v", checks[2])
	}
	runner.wantArgs(t, 0,
		"pr", "checks", "2",
		"--repo", "gitmoot/gitmoot",
		"--json", "name,state,bucket,link,workflow,completedAt",
	)
	runner.wantArgs(t, 1,
		"pr", "view", "2",
		"--repo", "gitmoot/gitmoot",
		"--json", "statusCheckRollup",
	)
}

func TestMergePullRequestUsesSafeHeadMatch(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{
		{Stdout: "merged"},
		{Stdout: `{"number": 2, "title": "Task", "state": "closed", "merged": true, "html_url": "https://github.com/gitmoot/gitmoot/pull/2", "merge_commit_sha": "merge123", "head": {"ref": "task", "sha": "abc123"}, "base": {"ref": "main"}}`},
	}}
	client := GhClient{Runner: runner}

	result, err := client.MergePullRequest(context.Background(), MergePullRequestInput{
		Repo:            Repository{Owner: "gitmoot", Name: "gitmoot"},
		Number:          2,
		Method:          "squash",
		Subject:         "feat: task",
		MatchHeadCommit: "abc123",
		DeleteBranch:    true,
	})

	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	if !result.Merged || result.SHA != "merge123" {
		t.Fatalf("merge result = %+v", result)
	}
	runner.wantArgs(t, 0,
		"pr", "merge", "2",
		"--repo", "gitmoot/gitmoot",
		"--squash",
		"--subject", "feat: task",
		"--match-head-commit", "abc123",
		"--delete-branch",
	)
	runner.wantArgs(t, 1, "api", "repos/gitmoot/gitmoot/pulls/2")
}

func TestMergePullRequestRequiresConfirmedMergedState(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: "merged"},
			{Stderr: "HTTP 502"},
		},
		errs: []error{nil, errors.New("exit status 1")},
	}
	client := GhClient{Runner: runner}

	result, err := client.MergePullRequest(context.Background(), MergePullRequestInput{
		Repo:            Repository{Owner: "gitmoot", Name: "gitmoot"},
		Number:          2,
		MatchHeadCommit: "abc123",
	})

	if err == nil || !strings.Contains(err.Error(), "fetch merged pull request") {
		t.Fatalf("error = %v, want fetch confirmation error; result=%+v", err, result)
	}
}

func TestMergePullRequestReportsQueuedMergeAsPending(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{
		{Stdout: "queued"},
		{Stdout: `{"number": 2, "title": "Task", "state": "open", "html_url": "https://github.com/gitmoot/gitmoot/pull/2", "merge_commit_sha": "synthetic", "head": {"ref": "task", "sha": "abc123"}, "base": {"ref": "main"}}`},
	}}
	client := GhClient{Runner: runner}

	result, err := client.MergePullRequest(context.Background(), MergePullRequestInput{
		Repo:            Repository{Owner: "gitmoot", Name: "gitmoot"},
		Number:          2,
		MatchHeadCommit: "abc123",
	})

	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	if result.Merged || !strings.Contains(result.Message, "pending") {
		t.Fatalf("merge result = %+v", result)
	}
}

func TestRateLimitBackoffRetries(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stderr: "HTTP 429: secondary rate limit"},
			{Stdout: `{"id": 42, "state": "pending", "context": "gitmoot/task"}`},
		},
		errs: []error{errors.New("exit 1"), nil},
	}
	var sleeps []time.Duration
	client := GhClient{
		Runner: runner,
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
		MaxRetries: 1,
	}

	status, err := client.CreateCommitStatus(context.Background(), CommitStatusInput{
		Repo:    Repository{Owner: "gitmoot", Name: "gitmoot"},
		SHA:     "abc123",
		State:   "pending",
		Context: "gitmoot/task",
	})

	if err != nil {
		t.Fatalf("CreateCommitStatus returned error: %v", err)
	}
	if status.ID != 42 {
		t.Fatalf("status = %+v", status)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{time.Second}) {
		t.Fatalf("sleeps = %v, want [1s]", sleeps)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls = %d, want 2", len(runner.calls))
	}
}

func TestCreatePullRequestFetchesCreatedPR(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: "https://github.com/gitmoot/gitmoot/pull/7\n"},
			{Stdout: `{"number": 7, "title": "Task 7", "state": "open", "html_url": "https://github.com/gitmoot/gitmoot/pull/7", "head": {"ref": "task-7", "sha": "abc123"}, "base": {"ref": "main"}}`},
		},
	}
	client := GhClient{Runner: runner}

	pr, err := client.CreatePullRequest(context.Background(), CreatePullRequestInput{
		Repo:  Repository{Owner: "gitmoot", Name: "gitmoot"},
		Title: "Task 7",
		Body:  "body",
		Head:  "task-7",
		Base:  "main",
	})

	if err != nil {
		t.Fatalf("CreatePullRequest returned error: %v", err)
	}
	if pr.Number != 7 || pr.HeadSHA != "abc123" {
		t.Fatalf("pr = %+v", pr)
	}
	runner.wantArgs(t, 0,
		"pr", "create",
		"--repo", "gitmoot/gitmoot",
		"--title", "Task 7",
		"--body", "body",
		"--head", "task-7",
		"--base", "main",
	)
	runner.wantArgs(t, 1, "api", "repos/gitmoot/gitmoot/pulls/7")
}

func TestGetOpenPullRequestByHead(t *testing.T) {
	repo := Repository{Owner: "gitmoot", Name: "gitmoot"}

	t.Run("found", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{
			{Stdout: `[{"number":7,"url":"https://github.com/gitmoot/gitmoot/pull/7","headRefOid":"abc123","baseRefName":"main","state":"OPEN"}]`},
		}}
		client := GhClient{Runner: runner}
		pr, ok, err := client.GetOpenPullRequestByHead(context.Background(), repo, "task-7", "main")
		if err != nil || !ok {
			t.Fatalf("GetOpenPullRequestByHead = (%+v, %v, %v), want found", pr, ok, err)
		}
		if pr.Number != 7 || pr.HeadSHA != "abc123" || pr.BaseRef != "main" || pr.HeadRef != "task-7" || pr.State != "open" {
			t.Fatalf("pr = %+v", pr)
		}
		runner.wantArgs(t, 0,
			"pr", "list",
			"--repo", "gitmoot/gitmoot",
			"--head", "task-7",
			"--base", "main",
			"--state", "open",
			"--json", "number,url,headRefOid,baseRefName,state",
		)
	})

	t.Run("none", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: `[]`}}}
		client := GhClient{Runner: runner}
		_, ok, err := client.GetOpenPullRequestByHead(context.Background(), repo, "task-7", "main")
		if err != nil || ok {
			t.Fatalf("GetOpenPullRequestByHead = (ok=%v, err=%v), want (false, nil)", ok, err)
		}
	})
}

func TestEnsurePullRequest(t *testing.T) {
	repo := Repository{Owner: "gitmoot", Name: "gitmoot"}
	input := CreatePullRequestInput{Repo: repo, Title: "Task 7", Body: "body", Head: "task-7", Base: "main"}

	t.Run("adopts existing open head PR without creating", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{
			{Stdout: `[{"number":9,"url":"https://github.com/gitmoot/gitmoot/pull/9","headRefOid":"def456","baseRefName":"main","state":"OPEN"}]`},
		}}
		client := GhClient{Runner: runner}
		pr, err := client.EnsurePullRequest(context.Background(), input)
		if err != nil {
			t.Fatalf("EnsurePullRequest returned error: %v", err)
		}
		if pr.Number != 9 || pr.HeadSHA != "def456" {
			t.Fatalf("pr = %+v", pr)
		}
		// Only the query ran; no create.
		if len(runner.calls) != 1 {
			t.Fatalf("calls = %v, want exactly the query call", runner.calls)
		}
		runner.wantArgs(t, 0, "pr", "list", "--repo", "gitmoot/gitmoot", "--head", "task-7", "--base", "main", "--state", "open", "--json", "number,url,headRefOid,baseRefName,state")
	})

	t.Run("creates when absent", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{
			{Stdout: `[]`}, // query: none
			{Stdout: "https://github.com/gitmoot/gitmoot/pull/11\n"}, // create
			{Stdout: `{"number":11,"title":"Task 7","state":"open","html_url":"https://github.com/gitmoot/gitmoot/pull/11","head":{"ref":"task-7","sha":"sha11"},"base":{"ref":"main"}}`}, // getPullRequest
		}}
		client := GhClient{Runner: runner}
		pr, err := client.EnsurePullRequest(context.Background(), input)
		if err != nil {
			t.Fatalf("EnsurePullRequest returned error: %v", err)
		}
		if pr.Number != 11 || pr.HeadSHA != "sha11" {
			t.Fatalf("pr = %+v", pr)
		}
		runner.wantArgs(t, 0, "pr", "list", "--repo", "gitmoot/gitmoot", "--head", "task-7", "--base", "main", "--state", "open", "--json", "number,url,headRefOid,baseRefName,state")
		runner.wantArgs(t, 1, "pr", "create", "--repo", "gitmoot/gitmoot", "--title", "Task 7", "--body", "body", "--head", "task-7", "--base", "main")
	})

	t.Run("re-queries and adopts on 422 already exists race", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{
				{Stdout: `[]`}, // query: none (TOCTOU window)
				{Stderr: "pull request create failed: GraphQL: A pull request already exists for gitmoot:task-7. (createPullRequest)"},                  // create: 422
				{Stdout: `[{"number":13,"url":"https://github.com/gitmoot/gitmoot/pull/13","headRefOid":"sha13","baseRefName":"main","state":"OPEN"}]`}, // re-query: winner
			},
			errs: []error{
				nil,
				errors.New("exit status 1"),
				nil,
			},
		}
		client := GhClient{Runner: runner}
		pr, err := client.EnsurePullRequest(context.Background(), input)
		if err != nil {
			t.Fatalf("EnsurePullRequest returned error: %v", err)
		}
		if pr.Number != 13 || pr.HeadSHA != "sha13" {
			t.Fatalf("pr = %+v", pr)
		}
		// query, create (422), re-query.
		if len(runner.calls) != 3 {
			t.Fatalf("calls = %v, want query+create+requery", runner.calls)
		}
	})

	t.Run("propagates a non-422 create error", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{
				{Stdout: `[]`},
				{Stderr: "HTTP 403: forbidden"},
			},
			errs: []error{nil, errors.New("exit status 1")},
		}
		client := GhClient{Runner: runner}
		if _, err := client.EnsurePullRequest(context.Background(), input); err == nil {
			t.Fatal("expected a non-422 create error to propagate")
		}
		if len(runner.calls) != 2 {
			t.Fatalf("calls = %v, want query+create only (no re-query on a hard error)", runner.calls)
		}
	})

	t.Run("returns the 422 error when the re-query still finds nothing", func(t *testing.T) {
		createErr := errors.New("pull request create failed: GraphQL: A pull request already exists for gitmoot:task-7. (createPullRequest)")
		runner := &fakeRunner{
			results: []subprocess.Result{
				{Stdout: `[]`}, // query: none
				{Stderr: "pull request create failed: A pull request already exists"}, // create: 422
				{Stdout: `[]`}, // re-query: still none (e.g. the existing PR targets a different base)
			},
			errs: []error{
				nil,
				createErr,
				nil,
			},
		}
		client := GhClient{Runner: runner}
		_, err := client.EnsurePullRequest(context.Background(), input)
		if err == nil {
			t.Fatal("expected the original 422 error when the re-query finds no adoptable PR")
		}
		if !errors.Is(err, createErr) {
			t.Fatalf("err = %v, want the original create (422) error", err)
		}
		// query, create (422), re-query — then surface the original error.
		if len(runner.calls) != 3 {
			t.Fatalf("calls = %v, want query+create+requery", runner.calls)
		}
	})
}

func TestWorkflowsExistAtRef(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}

	t.Run("present", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: `[{"name":"ci.yml"}]`}}}
		client := GhClient{Runner: runner}
		exists, err := client.WorkflowsExistAtRef(context.Background(), repo, "d342f97")
		if err != nil {
			t.Fatalf("WorkflowsExistAtRef returned error: %v", err)
		}
		if !exists {
			t.Fatalf("exists = false, want true")
		}
		args := strings.Join(runner.calls[0], " ")
		if !strings.Contains(args, "repos/o/r/contents/.github/workflows") || !strings.Contains(args, "ref=d342f97") {
			t.Fatalf("call args = %q, want contents/.github/workflows at ref", args)
		}
	})

	t.Run("absent 404 is false not error", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "HTTP 404: Not Found (https://api.github.com/repos/o/r/contents/.github/workflows)"}},
			errs:    []error{errors.New("exit status 1")},
		}
		client := GhClient{Runner: runner}
		exists, err := client.WorkflowsExistAtRef(context.Background(), repo, "d342f97")
		if err != nil {
			t.Fatalf("WorkflowsExistAtRef 404 returned error: %v", err)
		}
		if exists {
			t.Fatalf("exists = true on 404, want false")
		}
	})

	t.Run("other error surfaces", func(t *testing.T) {
		runner := &fakeRunner{
			results: []subprocess.Result{{Stderr: "HTTP 500: Server Error"}},
			errs:    []error{errors.New("exit status 1")},
		}
		client := GhClient{Runner: runner, MaxRetries: 0}
		if _, err := client.WorkflowsExistAtRef(context.Background(), repo, "d342f97"); err == nil {
			t.Fatal("WorkflowsExistAtRef swallowed a non-404 error; caller must be able to fail safe")
		}
	})
}

type fakeRunner struct {
	results []subprocess.Result
	errs    []error
	calls   [][]string
}

func (f *fakeRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	call := append([]string{command}, args...)
	f.calls = append(f.calls, call)
	index := len(f.calls) - 1
	if command != "gh" {
		return subprocess.Result{}, errors.New("unexpected command: " + command)
	}
	result := subprocess.Result{Command: command, Args: args}
	if index < len(f.results) {
		result = f.results[index]
		result.Command = command
		result.Args = args
	}
	var err error
	if index < len(f.errs) {
		err = f.errs[index]
	}
	return result, err
}

func (f *fakeRunner) LookPath(file string) (string, error) {
	if file != "gh" {
		return "", errors.New("not found")
	}
	return "/usr/bin/gh", nil
}

func (f *fakeRunner) wantArgs(t *testing.T, index int, want ...string) {
	t.Helper()
	if index >= len(f.calls) {
		t.Fatalf("missing call %d; calls=%v", index, f.calls)
	}
	got := f.calls[index]
	if !reflect.DeepEqual(got, append([]string{"gh"}, want...)) {
		t.Fatalf("call %d = %s\nwant %s", index, strings.Join(got, " "), strings.Join(append([]string{"gh"}, want...), " "))
	}
}
