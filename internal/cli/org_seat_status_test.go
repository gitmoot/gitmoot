package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type fakeOrgSeatStatusGitHub struct {
	pulls    map[string]github.PullRequest
	pullErr  error
	checks   map[string][]github.PullRequestCheck
	checkErr error
	issues   map[string][]github.Issue
	issueErr error
	getCalls []string
}

func (f *fakeOrgSeatStatusGitHub) ListPullRequests(_ context.Context, repo github.Repository, state string) ([]github.PullRequest, error) {
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	if state != "open" {
		return nil, fmt.Errorf("unexpected state %q", state)
	}
	var open []github.PullRequest
	for key, pr := range f.pulls {
		if strings.HasPrefix(key, repo.FullName()+"#") && pr.State == "open" && !pr.Merged {
			open = append(open, pr)
		}
	}
	return open, nil
}

func (f *fakeOrgSeatStatusGitHub) GetPullRequest(_ context.Context, repo github.Repository, number int64) (github.PullRequest, error) {
	key := fmt.Sprintf("%s#%d", repo.FullName(), number)
	f.getCalls = append(f.getCalls, key)
	if f.pullErr != nil {
		return github.PullRequest{}, f.pullErr
	}
	pr, ok := f.pulls[key]
	if !ok {
		return github.PullRequest{}, fmt.Errorf("not found: %s", key)
	}
	return pr, nil
}

func (f *fakeOrgSeatStatusGitHub) ListPullRequestChecks(_ context.Context, repo github.Repository, number int64) ([]github.PullRequestCheck, error) {
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	return f.checks[fmt.Sprintf("%s#%d", repo.FullName(), number)], nil
}

func (f *fakeOrgSeatStatusGitHub) ListOpenIssuesWithLabels(_ context.Context, repo github.Repository, labels []string) ([]github.Issue, error) {
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	return f.issues[repo.FullName()+"|"+strings.Join(labels, ",")], nil
}

func stubOrgSeatStatus(t *testing.T, gh orgSeatStatusGitHubClient, now time.Time) {
	t.Helper()
	prevGH, prevNow := newOrgSeatStatusGitHubClient, orgSeatStatusNow
	newOrgSeatStatusGitHubClient = func() orgSeatStatusGitHubClient { return gh }
	orgSeatStatusNow = func() time.Time { return now }
	t.Cleanup(func() {
		newOrgSeatStatusGitHubClient = prevGH
		orgSeatStatusNow = prevNow
	})
}

func setupOrgSeatStatusFixture(t *testing.T, now time.Time) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	orgTOML := `
[org]
enforce = "warn"

[org.roles."owner"]
display_name = "Owner"
scope = ["*"]
merge_rule = "owner"
pane = "w1:p1"

[org.roles."builder"]
display_name = "Builder"
parent = "owner"
scope = ["acme/widget"]
merge_rule = "self"
pane = "w1:p2"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(strings.TrimSpace(orgTOML)+"\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	rawDB, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer rawDB.Close()

	mustCreateJob := func(job db.Job) {
		t.Helper()
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("create job %s: %v", job.ID, err)
		}
		if job.CreatedAt != "" || job.UpdatedAt != "" {
			if _, err := rawDB.ExecContext(ctx, `UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`,
				job.CreatedAt, job.UpdatedAt, job.ID); err != nil {
				t.Fatalf("update timestamps for %s: %v", job.ID, err)
			}
		}
	}
	mustEncodePayload := func(p workflow.JobPayload) string {
		t.Helper()
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return string(raw)
	}

	// Open PR #10 attributed to builder via implement run.
	impl10 := mustEncodePayload(workflow.JobPayload{
		Repo:          "acme/widget",
		PullRequest:   10,
		HeadSHA:       "1111111222222233333334444444555555566666",
		ActingOrgRole: "builder",
	})
	mustCreateJob(db.Job{
		ID: "job-impl-10", Agent: "builder", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: impl10, CreatedAt: now.Add(-4 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-4 * time.Hour).Format(time.RFC3339),
	})

	// Closed PR #11 attributed to builder — must be filtered out when GitHub reports closed.
	impl11 := mustEncodePayload(workflow.JobPayload{
		Repo:          "acme/widget",
		PullRequest:   11,
		HeadSHA:       "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeee",
		ActingOrgRole: "builder",
	})
	mustCreateJob(db.Job{
		ID: "job-impl-11", Agent: "builder", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: impl11, CreatedAt: now.Add(-5 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-5 * time.Hour).Format(time.RFC3339),
	})

	// Open PR #12 with no review-level log entry (should render "unrecorded").
	impl12 := mustEncodePayload(workflow.JobPayload{
		Repo:          "acme/widget",
		PullRequest:   12,
		HeadSHA:       "9999999888888877777776666666555555544444",
		ActingOrgRole: "builder",
	})
	mustCreateJob(db.Job{
		ID: "job-impl-12", Agent: "builder", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: impl12, CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
	})

	// Review run on PR #10 finished 2h ago: P1 finding is answered, P2 finding is open -> highest severity must be P2.
	rev10Recent := mustEncodePayload(workflow.JobPayload{
		Repo:        "acme/widget",
		PullRequest: 10,
		HeadSHA:     "1111111222222233333334444444555555566666",
		Result: &workflow.AgentResult{
			Decision: "changes_requested",
			Severity: "P1",
			Findings: []json.RawMessage{
				json.RawMessage(`{"id":"F1","severity":"P1","state":"answered","title":"fixed bug"}`),
				json.RawMessage(`{"id":"F2","severity":"P2","state":"open","title":"cleanup needed"}`),
				json.RawMessage(`{"id":"F3","severity":"P3","state":"open","title":"nit"}`),
			},
		},
	})
	mustCreateJob(db.Job{
		ID: "job-rev-10-recent", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded),
		Payload: rev10Recent, CreatedAt: now.Add(-3 * time.Hour).Format("2006-01-02 15:04:05"), UpdatedAt: now.Add(-2 * time.Hour).Format("2006-01-02 15:04:05"),
	})

	// Review run on PR #10 finished 30h ago — must be excluded by 24h window.
	rev10Old := mustEncodePayload(workflow.JobPayload{
		Repo:        "acme/widget",
		PullRequest: 10,
		HeadSHA:     "0000000111111122222223333333444444455555",
		Result:      &workflow.AgentResult{Decision: "approved"},
	})
	mustCreateJob(db.Job{
		ID: "job-rev-10-old", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded),
		Payload: rev10Old, CreatedAt: now.Add(-30 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-30 * time.Hour).Format(time.RFC3339),
	})

	// Running review on PR #12 started 26h ago — still active, must be included.
	rev12Running := mustEncodePayload(workflow.JobPayload{
		Repo:        "acme/widget",
		PullRequest: 12,
		HeadSHA:     "9999999888888877777776666666555555544444",
	})
	mustCreateJob(db.Job{
		ID: "job-rev-12-running", Agent: "reviewer", Type: "review", State: string(workflow.JobRunning),
		Payload: rev12Running, CreatedAt: now.Add(-26 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-26 * time.Hour).Format(time.RFC3339),
	})

	// Review on closed PR #11 finished 1h ago — excluded when GitHub confirms #11 is closed.
	rev11Closed := mustEncodePayload(workflow.JobPayload{
		Repo:        "acme/widget",
		PullRequest: 11,
		HeadSHA:     "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeee",
		Result:      &workflow.AgentResult{Decision: "approved"},
	})
	mustCreateJob(db.Job{
		ID: "job-rev-11-closed", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded),
		Payload: rev11Closed, CreatedAt: now.Add(-1 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
	})

	// Awaited fact in waiting state for builder.
	key, err := db.ReviewVerdictSubjectKey("acme/widget", 12, "9999999888888877777776666666555555544444")
	if err != nil {
		t.Fatalf("subject key: %v", err)
	}
	if _, _, err := store.SubscribeAwaitedFact(ctx, db.AwaitedFactSubscription{
		WaiterRole:  "builder",
		SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey:  key,
		Deadline:    now.Add(6 * time.Hour),
	}); err != nil {
		t.Fatalf("subscribe awaited fact: %v", err)
	}

	// Record review level history for PR #10 (two entries; last one wins).
	if err := appendReviewLevelLog(home, "acme/widget", 10, reviewlevel.Decision{
		Level:   reviewlevel.LevelBackground,
		HeadSHA: "0000000111111122222223333333444444455555",
	}, now.Add(-6*time.Hour)); err != nil {
		t.Fatalf("append review level 1: %v", err)
	}
	if err := appendReviewLevelLog(home, "acme/widget", 10, reviewlevel.Decision{
		Level:   reviewlevel.LevelRequired,
		HeadSHA: "1111111222222233333334444444555555566666",
	}, now.Add(-3*time.Hour)); err != nil {
		t.Fatalf("append review level 2: %v", err)
	}

	return home
}

func TestOrgSeatStatusJSONAndTable(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	home := setupOrgSeatStatusFixture(t, now)
	gh := &fakeOrgSeatStatusGitHub{
		pulls: map[string]github.PullRequest{
			"acme/widget#10": {
				Number:         10,
				State:          "open",
				HeadSHA:        "1111111222222233333334444444555555566666",
				MergeableState: "clean",
			},
			"acme/widget#11": {
				Number:         11,
				State:          "closed",
				Merged:         true,
				HeadSHA:        "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeee",
				MergeableState: "unknown",
			},
			"acme/widget#12": {
				Number:         12,
				State:          "open",
				HeadSHA:        "9999999888888877777776666666555555544444",
				MergeableState: "blocked",
			},
		},
		checks: map[string][]github.PullRequestCheck{
			"acme/widget#10": {
				{Name: "build", Bucket: "pass"},
				{Name: "test", Bucket: "pass"},
				{Name: "lint", Bucket: "fail"},
			},
			"acme/widget#12": {
				{Name: "ci", Bucket: "pending"},
			},
		},
		issues: map[string][]github.Issue{
			"acme/widget|review-p2,agent:builder": {
				{
					Number:    42,
					Title:     "Batch cleanup helper",
					State:     "open",
					CreatedAt: now.Add(-72 * time.Hour).Format(time.RFC3339),
				},
			},
		},
	}
	stubOrgSeatStatus(t, gh, now)

	// Test JSON output with explicit --role.
	var jsonOut, jsonErr bytes.Buffer
	if code := Run([]string{"org", "seat", "status", "--role", "builder", "--json", "--home", home}, &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("org seat status --json exit=%d stderr=%s", code, jsonErr.String())
	}

	var got orgSeatStatusOutput
	if err := json.Unmarshal(jsonOut.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal json: %v\n%s", err, jsonOut.String())
	}
	for _, key := range gh.getCalls {
		if key == "acme/widget#11" {
			t.Fatalf("closed PR #11 was fetched individually; GetPullRequest calls = %v", gh.getCalls)
		}
	}
	if got.Role != "builder" {
		t.Fatalf("role = %q, want builder", got.Role)
	}
	if got.GeneratedAt != "2026-11-28T12:00:00Z" {
		t.Fatalf("generated_at = %q, want 2026-11-28T12:00:00Z", got.GeneratedAt)
	}
	if got.PullRequests.Error != "" || len(got.PullRequests.Items) != 2 {
		t.Fatalf("pull_requests = %+v, want 2 open PRs without error", got.PullRequests)
	}
	if got.PullRequests.Items[0].PR != 10 ||
		got.PullRequests.Items[0].HeadSHA != "1111111222222233333334444444555555566666" ||
		got.PullRequests.Items[0].ReviewLevel != reviewlevel.LevelRequired ||
		got.PullRequests.Items[0].CI != (orgSeatStatusCI{Passed: 2, Pending: 0, Failed: 1}) ||
		got.PullRequests.Items[0].MergeStateStatus != "CLEAN" {
		t.Fatalf("PR #10 = %+v", got.PullRequests.Items[0])
	}
	if got.PullRequests.Items[1].PR != 12 ||
		got.PullRequests.Items[1].ReviewLevel != "unrecorded" ||
		got.PullRequests.Items[1].CI != (orgSeatStatusCI{Passed: 0, Pending: 1, Failed: 0}) ||
		got.PullRequests.Items[1].MergeStateStatus != "BLOCKED" {
		t.Fatalf("PR #12 = %+v", got.PullRequests.Items[1])
	}

	// Reviews: PR #10 recent (P1 answered -> highest P2) + PR #12 running. Old #10 and closed #11 excluded.
	if got.Reviews.Error != "" || len(got.Reviews.Items) != 2 {
		t.Fatalf("reviews = %+v, want 2", got.Reviews)
	}
	if got.Reviews.Items[0].RunID != "job-rev-10-recent" ||
		got.Reviews.Items[0].Decision != "changes_requested" ||
		got.Reviews.Items[0].HighestSeverity != "P2" {
		t.Fatalf("review[0] = %+v, want P2 (P1 answered excluded)", got.Reviews.Items[0])
	}
	if got.Reviews.Items[1].RunID != "job-rev-12-running" ||
		got.Reviews.Items[1].State != "running" {
		t.Fatalf("review[1] = %+v", got.Reviews.Items[1])
	}

	if got.AwaitedFacts.Error != "" || len(got.AwaitedFacts.Items) != 1 {
		t.Fatalf("awaited_facts = %+v, want 1", got.AwaitedFacts)
	}
	if got.ReviewP2Issues.Error != "" || len(got.ReviewP2Issues.Items) != 1 {
		t.Fatalf("review_p2_issues = %+v, want 1", got.ReviewP2Issues)
	}
	if got.ReviewP2Issues.Items[0].Number != 42 || got.ReviewP2Issues.Items[0].Age != "3d" {
		t.Fatalf("issue[0] = %+v", got.ReviewP2Issues.Items[0])
	}

	// Test table output via GITMOOT_ORG_ROLE env var.
	t.Setenv("GITMOOT_ORG_ROLE", "builder")
	var tableOut, tableErr bytes.Buffer
	if code := Run([]string{"org", "seat", "status", "--home", home}, &tableOut, &tableErr); code != 0 {
		t.Fatalf("org seat status table exit=%d stderr=%s", code, tableErr.String())
	}
	out := tableOut.String()
	for _, want := range []string{
		"Seat status: builder (generated 2026-11-28T12:00:00Z)",
		"Open Pull Requests",
		"acme/widget",
		"#10",
		"1111111",
		"level3_required",
		"2 pass/0 pend/1 fail",
		"CLEAN",
		"unrecorded",
		"Reviews (active or last 24h)",
		"job-rev-10-recent",
		"changes_requested",
		"P2",
		"Awaited Facts (waiting)",
		"acme/widget#12@9999999888888877777776666666555555544444",
		"Open review-p2 Issues",
		"#42",
		"3d",
		"Batch cleanup helper",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#11") {
		t.Errorf("closed PR #11 leaked into table output:\n%s", out)
	}
	t.Logf("sample table output:\n%s", out)
}

func TestOrgSeatStatusRoleResolution(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	home := setupOrgSeatStatusFixture(t, now)
	stubOrgSeatStatus(t, &fakeOrgSeatStatusGitHub{}, now)

	// 1. Missing role (no --role, no GITMOOT_ORG_ROLE, no HERDR_PANE_ID) -> exit 2 with clear error.
	t.Setenv("GITMOOT_ORG_ROLE", "")
	t.Setenv("HERDR_PANE_ID", "")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "seat", "status", "--home", home}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing role exit=%d want 2, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "use --role") {
		t.Fatalf("missing role stderr=%q", stderr.String())
	}

	// 2. Pane resolution via HERDR_PANE_ID.
	panes := []org.LivePane{{PaneID: "w1:p2", Label: "Builder"}}
	withOrgSeatFixtureProvider(t, &panes)
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"org", "seat", "status", "--json", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("pane-resolved role exit=%d stderr=%q", code, stderr.String())
	}
	var got orgSeatStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Role != "builder" {
		t.Fatalf("pane-resolved role = %q, want builder", got.Role)
	}
}

func TestOrgSeatStatusGitHubFailureStillRendersLocalSectionsAndExitsZero(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	home := setupOrgSeatStatusFixture(t, now)
	gh := &fakeOrgSeatStatusGitHub{
		pullErr:  errors.New("github API 503"),
		issueErr: errors.New("github search rate limited"),
	}
	stubOrgSeatStatus(t, gh, now)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "seat", "status", "--role", "builder", "--json", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("github failure exit=%d want 0, stderr=%q", code, stderr.String())
	}

	var got orgSeatStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal json: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(got.PullRequests.Error, "github API 503") {
		t.Fatalf("pull_requests.error = %q, want github API 503", got.PullRequests.Error)
	}
	if !strings.Contains(got.ReviewP2Issues.Error, "github search rate limited") {
		t.Fatalf("review_p2_issues.error = %q, want github search rate limited", got.ReviewP2Issues.Error)
	}
	// Local sections must still be populated!
	if len(got.Reviews.Items) < 2 {
		t.Fatalf("reviews = %+v, want local review runs even when GitHub fails", got.Reviews.Items)
	}
	if len(got.AwaitedFacts.Items) != 1 {
		t.Fatalf("awaited_facts = %+v, want 1 local awaited fact", got.AwaitedFacts.Items)
	}

	// Table output also exits 0 and shows errors + local sections.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"org", "seat", "status", "--role", "builder", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("github failure table exit=%d want 0, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "error: ") || !strings.Contains(out, "job-rev-10-recent") || !strings.Contains(out, "Awaited Facts (waiting)") {
		t.Fatalf("table output missing error or local sections:\n%s", out)
	}
}
