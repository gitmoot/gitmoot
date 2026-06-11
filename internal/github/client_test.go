package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/subprocess"
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
		runner.wantArgs(t, 0, "repo", "create", "o/r", "--private")
	})

	t.Run("public", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{}}}
		client := GhClient{Runner: runner}
		if err := client.CreateRepository(context.Background(), repo, false); err != nil {
			t.Fatalf("CreateRepository: %v", err)
		}
		runner.wantArgs(t, 0, "repo", "create", "o/r", "--public")
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

	comments, err := client.ListIssueComments(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListIssueComments returned error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments length = %d, want 2", len(comments))
	}
	if comments[0].Body != "first" || comments[1].Author != "bob" {
		t.Fatalf("comments were not decoded in first-seen order: %+v", comments)
	}
	runner.wantArgs(t, 0, "api", "--paginate", "repos/jerryfane/gitmoot/issues/2/comments")
}

func TestPostIssueCommentUsesIssueCommentsEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"id": 21, "body": "done", "html_url": "https://github.com/jerryfane/gitmoot/pull/2#issuecomment-21", "user": {"login": "gitmoot"}}`,
		}},
	}
	client := GhClient{Runner: runner}

	comment, err := client.PostIssueComment(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2, "done")

	if err != nil {
		t.Fatalf("PostIssueComment returned error: %v", err)
	}
	if comment.ID != 21 || comment.Body != "done" {
		t.Fatalf("comment = %+v", comment)
	}
	runner.wantArgs(t, 0, "api", "repos/jerryfane/gitmoot/issues/2/comments", "-f", "body=done")
}

func TestCreateIssueUsesIssuesEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"number": 8, "title": "Review run-1", "state": "open", "html_url": "https://github.com/jerryfane/gitmoot/issues/8"}`,
		}},
	}
	client := GhClient{Runner: runner}

	issue, err := client.CreateIssue(context.Background(), CreateIssueInput{
		Repo:  Repository{Owner: "jerryfane", Name: "gitmoot"},
		Title: "Review run-1",
		Body:  "body",
	})

	if err != nil {
		t.Fatalf("CreateIssue returned error: %v", err)
	}
	if issue.Number != 8 || issue.URL != "https://github.com/jerryfane/gitmoot/issues/8" {
		t.Fatalf("issue = %+v", issue)
	}
	runner.wantArgs(t, 0, "api", "repos/jerryfane/gitmoot/issues", "-f", "title=Review run-1", "-f", "body=body")
}

func TestPreflightChecksGhAuthAndRepoAccess(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{
			{Stdout: "gh version 2.45.0"},
			{Stdout: "Logged in to github.com"},
			{Stdout: `{"nameWithOwner":"jerryfane/gitmoot"}`},
		},
	}
	client := GhClient{Runner: runner}

	if err := client.Preflight(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}); err != nil {
		t.Fatalf("Preflight returned error: %v", err)
	}

	runner.wantArgs(t, 0, "--version")
	runner.wantArgs(t, 1, "auth", "status", "--hostname", "github.com")
	runner.wantArgs(t, 2, "repo", "view", "jerryfane/gitmoot", "--json", "nameWithOwner")
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

	err := client.Preflight(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"})

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
			Stdout: `{"number": 8, "title": "Review run-1", "state": "closed", "html_url": "https://github.com/jerryfane/gitmoot/issues/8"}`,
		}},
	}
	client := GhClient{Runner: runner}

	issue, err := client.CloseIssue(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 8)

	if err != nil {
		t.Fatalf("CloseIssue returned error: %v", err)
	}
	if issue.Number != 8 || issue.State != "closed" {
		t.Fatalf("issue = %+v", issue)
	}
	runner.wantArgs(t, 0, "api", "-X", "PATCH", "repos/jerryfane/gitmoot/issues/8", "-f", "state=closed")
}

func TestGetUserPermissionUsesCollaboratorPermissionEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"permission": "write", "role_name": "write"}`,
		}},
	}
	client := GhClient{Runner: runner}

	permission, err := client.GetUserPermission(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, "alice")

	if err != nil {
		t.Fatalf("GetUserPermission returned error: %v", err)
	}
	if permission.Permission != "write" || permission.RoleName != "write" {
		t.Fatalf("permission = %+v", permission)
	}
	runner.wantArgs(t, 0, "api", "repos/jerryfane/gitmoot/collaborators/alice/permission")
}

func TestGetUserPermissionMapsNotFoundToNone(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "HTTP 404: Not Found"}},
		errs:    []error{errors.New("exit status 1")},
	}
	client := GhClient{Runner: runner}

	permission, err := client.GetUserPermission(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, "mallory")

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
		Repo:        Repository{Owner: "jerryfane", Name: "gitmoot"},
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
		"repos/jerryfane/gitmoot/statuses/abc123",
		"-f", "state=success",
		"-f", "context=gitmoot/task",
		"-f", "description=ok",
		"-f", "target_url=https://example.com",
	)
}

func TestGetPullRequestDecodesBaseSHA(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"number": 2, "title": "Task", "state": "open", "html_url": "https://github.com/jerryfane/gitmoot/pull/2", "head": {"ref": "task", "sha": "head123"}, "base": {"ref": "main", "sha": "base123"}}`,
		}},
	}
	client := GhClient{Runner: runner}

	pr, err := client.GetPullRequest(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.HeadSHA != "head123" || pr.BaseSHA != "base123" {
		t.Fatalf("pull request = %+v", pr)
	}
	runner.wantArgs(t, 0, "api", "repos/jerryfane/gitmoot/pulls/2")
}

func TestCompareCommitsUsesEscapedCompareEndpoint(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"status": "ahead", "ahead_by": 3, "behind_by": 0}`,
		}},
	}
	client := GhClient{Runner: runner}

	compare, err := client.CompareCommits(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, "release/1.0", "head123")

	if err != nil {
		t.Fatalf("CompareCommits returned error: %v", err)
	}
	if compare.Status != "ahead" || compare.AheadBy != 3 || compare.BehindBy != 0 {
		t.Fatalf("compare = %+v", compare)
	}
	runner.wantArgs(t, 0, "api", "repos/jerryfane/gitmoot/compare/release%2F1.0...head123")
}

func TestUpdatePullRequestBranchUsesExpectedHeadSHA(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `{"message":"Updating pull request branch.","url":"https://github.com/repos/jerryfane/gitmoot/pulls/2"}`,
		}},
	}
	client := GhClient{Runner: runner}

	result, err := client.UpdatePullRequestBranch(context.Background(), UpdatePullRequestBranchInput{
		Repo:            Repository{Owner: "jerryfane", Name: "gitmoot"},
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
		"repos/jerryfane/gitmoot/pulls/2/update-branch",
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
		Repo:            Repository{Owner: "jerryfane", Name: "gitmoot"},
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
		Repo:            Repository{Owner: "jerryfane", Name: "gitmoot"},
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
		Repo:            Repository{Owner: "jerryfane", Name: "gitmoot"},
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

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2)

	if err != nil {
		t.Fatalf("ListPullRequestChecks returned error: %v", err)
	}
	if len(checks) != 2 || checks[0].Bucket != "pass" || checks[1].Bucket != "pending" {
		t.Fatalf("checks = %+v", checks)
	}
	runner.wantArgs(t, 0,
		"pr", "checks", "2",
		"--repo", "jerryfane/gitmoot",
		"--json", "name,state,bucket,link,workflow,completedAt",
	)
}

func TestListPullRequestChecksAcceptsPendingExitWithJSON(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{
			Stdout: `[{"name": "test", "state": "PENDING", "bucket": "pending"}]`,
		}},
		errs: []error{errors.New("exit status 8")},
	}
	client := GhClient{Runner: runner}

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2)

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

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2)

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

	checks, err := client.ListPullRequestChecks(context.Background(), Repository{Owner: "jerryfane", Name: "gitmoot"}, 2)

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
		"--repo", "jerryfane/gitmoot",
		"--json", "name,state,bucket,link,workflow,completedAt",
	)
	runner.wantArgs(t, 1,
		"pr", "view", "2",
		"--repo", "jerryfane/gitmoot",
		"--json", "statusCheckRollup",
	)
}

func TestMergePullRequestUsesSafeHeadMatch(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{
		{Stdout: "merged"},
		{Stdout: `{"number": 2, "title": "Task", "state": "closed", "merged": true, "html_url": "https://github.com/jerryfane/gitmoot/pull/2", "merge_commit_sha": "merge123", "head": {"ref": "task", "sha": "abc123"}, "base": {"ref": "main"}}`},
	}}
	client := GhClient{Runner: runner}

	result, err := client.MergePullRequest(context.Background(), MergePullRequestInput{
		Repo:            Repository{Owner: "jerryfane", Name: "gitmoot"},
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
		"--repo", "jerryfane/gitmoot",
		"--squash",
		"--subject", "feat: task",
		"--match-head-commit", "abc123",
		"--delete-branch",
	)
	runner.wantArgs(t, 1, "api", "repos/jerryfane/gitmoot/pulls/2")
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
		Repo:            Repository{Owner: "jerryfane", Name: "gitmoot"},
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
		{Stdout: `{"number": 2, "title": "Task", "state": "open", "html_url": "https://github.com/jerryfane/gitmoot/pull/2", "merge_commit_sha": "synthetic", "head": {"ref": "task", "sha": "abc123"}, "base": {"ref": "main"}}`},
	}}
	client := GhClient{Runner: runner}

	result, err := client.MergePullRequest(context.Background(), MergePullRequestInput{
		Repo:            Repository{Owner: "jerryfane", Name: "gitmoot"},
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
		Repo:    Repository{Owner: "jerryfane", Name: "gitmoot"},
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
			{Stdout: "https://github.com/jerryfane/gitmoot/pull/7\n"},
			{Stdout: `{"number": 7, "title": "Task 7", "state": "open", "html_url": "https://github.com/jerryfane/gitmoot/pull/7", "head": {"ref": "task-7", "sha": "abc123"}, "base": {"ref": "main"}}`},
		},
	}
	client := GhClient{Runner: runner}

	pr, err := client.CreatePullRequest(context.Background(), CreatePullRequestInput{
		Repo:  Repository{Owner: "jerryfane", Name: "gitmoot"},
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
		"--repo", "jerryfane/gitmoot",
		"--title", "Task 7",
		"--body", "body",
		"--head", "task-7",
		"--base", "main",
	)
	runner.wantArgs(t, 1, "api", "repos/jerryfane/gitmoot/pulls/7")
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
