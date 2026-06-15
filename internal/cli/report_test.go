package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/report"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestReportBugJobPreviewDefaultsToPreview(t *testing.T) {
	home := seedCLIReportJob(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"report", "bug", "--home", home, "--job", "job-failed"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("report bug preview exit code = %d, stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"Title: Gitmoot failed job ask for audit in owner/repo",
		"Labels: gitmoot-dashboard-report, bug",
		"Fingerprint: ",
		"<!-- gitmoot:dashboard-report fingerprint:",
		"## What happened",
		"## Quick context",
		"<summary>Recent events</summary>",
		"failed during delivery",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("preview output missing %q:\n%s", want, output)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestReportBugJobPreviewCreateSmoke(t *testing.T) {
	home := seedCLIReportJob(t)
	var previewOut, previewErr bytes.Buffer

	code := Run([]string{"report", "bug", "--home", home, "--job", "job-failed", "--preview"}, &previewOut, &previewErr)

	if code != 0 {
		t.Fatalf("report bug preview exit code = %d, stderr=%s", code, previewErr.String())
	}
	preview := previewOut.String()
	fingerprint := previewFingerprint(t, preview)
	for _, want := range []string{
		"Title: Gitmoot failed job ask for audit in owner/repo",
		"Labels: gitmoot-dashboard-report, bug",
		"## Quick context",
		"<summary>Job context</summary>",
		"<summary>Redaction notes</summary>",
	} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview output missing %q:\n%s", want, preview)
		}
	}

	fake := &reportFakeGitHub{
		createdIssue: github.Issue{Number: 291, URL: "https://github.com/jerryfane/gitmoot/issues/291"},
	}
	restore := replaceReportGitHubClient(fake)
	defer restore()
	var createOut, createErr bytes.Buffer

	code = Run([]string{"report", "bug", "--home", home, "--job", "job-failed", "--create", "--yes"}, &createOut, &createErr)

	if code != 0 {
		t.Fatalf("report bug create exit code = %d, stderr=%s", code, createErr.String())
	}
	if createOut.String() != "created issue: https://github.com/jerryfane/gitmoot/issues/291\n" {
		t.Fatalf("stdout = %q", createOut.String())
	}
	if createErr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", createErr.String())
	}
	if len(fake.searches) != 1 || !strings.Contains(fake.searches[0], fingerprint) {
		t.Fatalf("searches = %+v, want fingerprint %q", fake.searches, fingerprint)
	}
	if fake.createInput.Repo.FullName() != "jerryfane/gitmoot" ||
		fake.createInput.Title != "Gitmoot failed job ask for audit in owner/repo" ||
		strings.Join(fake.createInput.Labels, ",") != "gitmoot-dashboard-report,bug" ||
		!strings.Contains(fake.createInput.Body, report.FingerprintMarker(fingerprint)) ||
		!strings.Contains(fake.createInput.Body, "<summary>Recent events</summary>") {
		t.Fatalf("create input = %+v", fake.createInput)
	}
}

func TestReportBugCreateRequiresYes(t *testing.T) {
	home := seedCLIReportJob(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"report", "bug", "--home", home, "--job", "job-failed", "--create"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("report bug --create exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--create requires --yes") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestReportBugUnsupportedSourcesNameSource(t *testing.T) {
	for _, args := range [][]string{
		{"report", "bug", "--source", "daemon", "--preview"},
		{"report", "bug", "--source", "dashboard", "--preview"},
		{"report", "bug", "--train", "train-1", "--preview"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := Run(args, &stdout, &stderr)

			if code != 1 {
				t.Fatalf("exit code = %d, stderr=%s", code, stderr.String())
			}
			output := stderr.String()
			if !strings.Contains(output, "source daemon") && !strings.Contains(output, "source dashboard") && !strings.Contains(output, "source train") {
				t.Fatalf("stderr does not name source: %q", output)
			}
		})
	}
}

func TestReportBugJobCreateCreatesIssueWithLabels(t *testing.T) {
	home := seedCLIReportJob(t)
	fake := &reportFakeGitHub{
		createdIssue: github.Issue{Number: 289, URL: "https://github.com/jerryfane/gitmoot/issues/289"},
	}
	restore := replaceReportGitHubClient(fake)
	defer restore()
	var stdout, stderr bytes.Buffer

	code := Run([]string{"report", "bug", "--home", home, "--job", "job-failed", "--create", "--yes"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("report bug create exit code = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != "created issue: https://github.com/jerryfane/gitmoot/issues/289\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if len(fake.preflightRepos) != 1 || fake.preflightRepos[0].FullName() != "jerryfane/gitmoot" {
		t.Fatalf("preflight repos = %+v", fake.preflightRepos)
	}
	if len(fake.searches) != 1 || !strings.Contains(fake.searches[0], "gitmoot:dashboard-report fingerprint:") {
		t.Fatalf("searches = %+v", fake.searches)
	}
	if fake.createInput.Repo.FullName() != "jerryfane/gitmoot" ||
		!strings.Contains(fake.createInput.Body, "<!-- gitmoot:dashboard-report fingerprint:") ||
		strings.Join(fake.createInput.Labels, ",") != "gitmoot-dashboard-report,bug" {
		t.Fatalf("create input = %+v", fake.createInput)
	}
}

func TestReportBugJobCreateReturnsExistingIssueForDuplicate(t *testing.T) {
	home := seedCLIReportJob(t)
	fake := &reportFakeGitHub{
		searchIssues: []github.Issue{{
			Number: 289,
			URL:    "https://github.com/jerryfane/gitmoot/issues/289",
		}},
	}
	restore := replaceReportGitHubClient(fake)
	defer restore()
	var stdout, stderr bytes.Buffer

	code := Run([]string{"report", "bug", "--home", home, "--job", "job-failed", "--create", "--yes"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("report bug duplicate exit code = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != "existing issue: https://github.com/jerryfane/gitmoot/issues/289\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if fake.createCalled {
		t.Fatal("duplicate report should not create a new issue")
	}
}

func seedCLIReportJob(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 7,
		TaskID:      "task-1",
		TaskTitle:   "Fix bug reporting",
		Result: &workflow.AgentResult{
			Decision: "failed",
			Summary:  "failed during delivery",
		},
	})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID:      "job-failed",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobFailed),
		Payload: string(payload),
	}, db.JobEvent{Kind: string(workflow.JobFailed), Message: "failed during delivery"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	return home
}

func previewFingerprint(t *testing.T, output string) string {
	t.Helper()
	const prefix = "Fingerprint: "
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			fingerprint := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if fingerprint == "" {
				t.Fatal("preview fingerprint was empty")
			}
			return fingerprint
		}
	}
	t.Fatalf("preview fingerprint missing:\n%s", output)
	return ""
}

func replaceReportGitHubClient(client reportGitHubClient) func() {
	prev := newReportGitHubClient
	newReportGitHubClient = func() reportGitHubClient { return client }
	return func() { newReportGitHubClient = prev }
}

type reportFakeGitHub struct {
	github.NoopClient

	preflightRepos []github.Repository
	searches       []string
	searchIssues   []github.Issue
	searchErr      error
	createInput    github.CreateIssueInput
	createdIssue   github.Issue
	createErr      error
	createCalled   bool
}

func (f *reportFakeGitHub) Preflight(_ context.Context, repo github.Repository) error {
	f.preflightRepos = append(f.preflightRepos, repo)
	return nil
}

func (f *reportFakeGitHub) SearchOpenIssues(_ context.Context, _ github.Repository, text string) ([]github.Issue, error) {
	f.searches = append(f.searches, text)
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return append([]github.Issue(nil), f.searchIssues...), nil
}

func (f *reportFakeGitHub) CreateIssue(_ context.Context, input github.CreateIssueInput) (github.Issue, error) {
	f.createCalled = true
	f.createInput = input
	if f.createErr != nil {
		return github.Issue{}, f.createErr
	}
	if f.createdIssue.URL == "" {
		return github.Issue{Number: 1, URL: "https://github.com/jerryfane/gitmoot/issues/1"}, nil
	}
	return f.createdIssue, nil
}

var _ reportGitHubClient = (*reportFakeGitHub)(nil)

func TestReportBugContinuesWhenDuplicateSearchFails(t *testing.T) {
	home := seedCLIReportJob(t)
	fake := &reportFakeGitHub{
		searchErr:    errors.New("search unavailable"),
		createdIssue: github.Issue{Number: 290, URL: "https://github.com/jerryfane/gitmoot/issues/290"},
	}
	restore := replaceReportGitHubClient(fake)
	defer restore()
	var stdout, stderr bytes.Buffer

	code := Run([]string{"report", "bug", "--home", home, "--job", "job-failed", "--create", "--yes"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("report bug search failure exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning: duplicate search failed: search unavailable") {
		t.Fatalf("stderr missing warning: %q", stderr.String())
	}
	if stdout.String() != "created issue: https://github.com/jerryfane/gitmoot/issues/290\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestCreateBugReportIssueResultRequiresVisibleDuplicateSearchWarning(t *testing.T) {
	fake := &reportFakeGitHub{
		searchErr: errors.New("search unavailable"),
	}
	draft := report.Report{
		Title:       "Gitmoot failed job ask for planner",
		Body:        "<!-- gitmoot:dashboard-report fingerprint:abc123 -->\n\nbody",
		Labels:      []string{"gitmoot-dashboard-report", "bug"},
		Fingerprint: "abc123",
	}

	_, err := createBugReportIssueResult(context.Background(), fake, draft, nil)

	if err == nil || !strings.Contains(err.Error(), "duplicate search failed: search unavailable") {
		t.Fatalf("expected duplicate search error, got %v", err)
	}
	if fake.createCalled {
		t.Fatal("must not create an issue when duplicate search failed without a warning sink")
	}
}
