package report

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestBuildJobReportFormatsRedactedJobReport(t *testing.T) {
	ctx := context.Background()
	store := openReportStore(t)
	defer store.Close()
	if err := store.UpsertAgent(ctx, db.Agent{Name: "audit", Runtime: "codex", RepoScope: "owner/repo"}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	payload := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "task-1-password=plain-branch-password",
		HeadSHA:      "abc123",
		PullRequest:  7,
		TaskID:       "task-1",
		TaskTitle:    "Fix report bug token=plain-task-token",
		Sender:       "planner",
		LeadAgent:    "lead",
		Reviewers:    []string{"audit"},
		Constraints:  []string{"do not leak token=ghp_abcdefghijklmnopqrstuvwxyz123456"},
		Instructions: "Reproduce the failure.\n/gitmoot merge\napi_key=plain-api-key-value",
		Result: &workflow.AgentResult{
			Decision: "failed",
			Summary:  "delivery failed password=plain-password-value oauth_token=plain-oauth-value",
			Findings: []json.RawMessage{
				json.RawMessage(`{"body":"AWS_SECRET_ACCESS_KEY=plain-aws-secret"}`),
			},
			Needs: []string{"rerun with auth_token=plain-auth-value"},
		},
		RawOutputs: []string{"raw token ghp_abcdefghijklmnopqrstuvwxyz123456 password=raw-secret"},
	}
	seedReportJob(t, store, db.Job{
		ID:      "job-failed",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobFailed),
		Payload: mustReportPayload(t, payload),
	}, "failed with ghp_abcdefghijklmnopqrstuvwxyz123456")
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-failed", Kind: "malformed_output", Message: "bad output with AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}

	report, err := BuildJobReport(ctx, store, "job-failed", JobOptions{})
	if err != nil {
		t.Fatalf("BuildJobReport returned error: %v", err)
	}

	for _, want := range []string{
		"<!-- gitmoot:dashboard-report fingerprint:",
		"## What happened",
		"- **Job:** failed ask job for audit in owner/repo",
		"## Quick context",
		"| Repository | owner/repo |",
		"| Agent | audit |",
		"| Runtime | codex |",
		"| Action | ask |",
		"| Pull request | #7 |",
		"| Task | task-1 - Fix report bug token=[REDACTED] |",
		"## Agent result",
		"- **Decision:** failed",
		"<summary>Request instructions</summary>",
		"<summary>Recent events</summary>",
		"<summary>Redaction notes</summary>",
		"Raw runtime outputs omitted: 1 retained locally.",
		`\/gitmoot merge`,
	} {
		if !strings.Contains(report.Body, want) {
			t.Fatalf("report body missing %q:\n%s", want, report.Body)
		}
	}
	for _, leaked := range []string{
		"ghp_abcdefghijklmnopqrstuvwxyz123456",
		"plain-api-key-value",
		"plain-password-value",
		"plain-oauth-value",
		"plain-auth-value",
		"plain-branch-password",
		"plain-task-token",
		"plain-aws-secret",
		"AKIAIOSFODNN7EXAMPLE",
		"raw token",
		"raw-secret",
	} {
		if strings.Contains(report.Body, leaked) {
			t.Fatalf("report leaked %q:\n%s", leaked, report.Body)
		}
	}
	if report.Title != "Gitmoot failed job ask for audit in owner/repo" {
		t.Fatalf("title = %q", report.Title)
	}
	if report.Fingerprint == "" || !strings.Contains(report.Body, report.Fingerprint) {
		t.Fatalf("fingerprint missing from report: %+v\n%s", report, report.Body)
	}
	if got := strings.Join(report.Labels, ","); got != "gitmoot-dashboard-report,bug" {
		t.Fatalf("labels = %q", got)
	}
	if !report.RedactionSummary.RawOutputsOmitted || report.RedactionSummary.RawOutputCount != 1 {
		t.Fatalf("redaction summary = %+v", report.RedactionSummary)
	}
	if strings.Contains(report.Source.TaskTitle, "plain-task-token") || strings.Contains(report.Source.Branch, "plain-branch-password") {
		t.Fatalf("source metadata leaked secrets: %+v", report.Source)
	}
	if !strings.Contains(report.Source.TaskTitle, "[REDACTED]") || !strings.Contains(report.Source.Branch, "[REDACTED]") {
		t.Fatalf("source metadata was not redacted: %+v", report.Source)
	}
}

func TestBuildJobReportTruncatesLongRenderedFieldsAndOmitsRawOutput(t *testing.T) {
	ctx := context.Background()
	store := openReportStore(t)
	defer store.Close()
	longSummary := strings.Repeat("x", 2500)
	payload := workflow.JobPayload{
		Repo: "owner/repo",
		Result: &workflow.AgentResult{
			Decision: "blocked",
			Summary:  longSummary,
		},
		RawOutputs: []string{strings.Repeat("raw output ", 400)},
	}
	seedReportJob(t, store, db.Job{
		ID:      "job-blocked",
		Agent:   "planner",
		Type:    "ask",
		State:   string(workflow.JobBlocked),
		Payload: mustReportPayload(t, payload),
	}, "blocked")

	report, err := BuildJobReport(ctx, store, "job-blocked", JobOptions{})
	if err != nil {
		t.Fatalf("BuildJobReport returned error: %v", err)
	}
	if !strings.Contains(report.Body, "[truncated]") {
		t.Fatalf("report did not truncate long summary:\n%s", report.Body)
	}
	if strings.Contains(report.Body, "raw output raw output") {
		t.Fatalf("report included raw output:\n%s", report.Body)
	}
	if !strings.Contains(report.Body, "Raw runtime outputs omitted: 1 retained locally.") {
		t.Fatalf("report missing raw-output omission note:\n%s", report.Body)
	}
}

func TestBuildJobReportSummarizesVerboseSelectedError(t *testing.T) {
	ctx := context.Background()
	store := openReportStore(t)
	defer store.Close()
	verboseError := strings.Join([]string{
		"delivery failed: OpenAI Codex v0.139.0",
		"--------",
		"workdir: /tmp/worktree",
		"ERROR: You've hit your usage limit.",
		"tokens used: 9515",
	}, "\n")
	payload := workflow.JobPayload{Repo: "owner/repo"}
	seedReportJob(t, store, db.Job{
		ID:      "job-verbose",
		Agent:   "audit",
		Type:    "implement",
		State:   string(workflow.JobFailed),
		Payload: mustReportPayload(t, payload),
	}, verboseError)

	report, err := BuildJobReport(ctx, store, "job-verbose", JobOptions{})
	if err != nil {
		t.Fatalf("BuildJobReport returned error: %v", err)
	}

	for _, want := range []string{
		"- **Selected error:** ERROR: You've hit your usage limit.",
		"<summary>Selected error details</summary>",
		"delivery failed: OpenAI Codex v0.139.0",
		"tokens used: 9515",
	} {
		if !strings.Contains(report.Body, want) {
			t.Fatalf("report body missing %q:\n%s", want, report.Body)
		}
	}
}

func TestBuildJobReportCapsTotalBody(t *testing.T) {
	ctx := context.Background()
	store := openReportStore(t)
	defer store.Close()
	findings := make([]json.RawMessage, 50)
	for i := range findings {
		findings[i] = json.RawMessage(`"` + strings.Repeat("finding text ", 250) + `"`)
	}
	payload := workflow.JobPayload{
		Repo: "owner/repo",
		Result: &workflow.AgentResult{
			Decision: "failed",
			Summary:  "large result",
			Findings: findings,
		},
	}
	seedReportJob(t, store, db.Job{
		ID:      "job-large",
		Agent:   "audit",
		Type:    "review",
		State:   string(workflow.JobFailed),
		Payload: mustReportPayload(t, payload),
	}, "failed")

	report, err := BuildJobReport(ctx, store, "job-large", JobOptions{})
	if err != nil {
		t.Fatalf("BuildJobReport returned error: %v", err)
	}
	if len([]rune(report.Body)) > 60000 {
		t.Fatalf("report body length = %d, want <= 60000", len([]rune(report.Body)))
	}
	if !strings.Contains(report.Body, "comment truncated") {
		t.Fatalf("report did not include total-body truncation notice")
	}
	if !strings.Contains(report.Body, "<!-- gitmoot:dashboard-report fingerprint:") {
		t.Fatalf("truncated report lost fingerprint marker:\n%s", report.Body)
	}
}

func TestBuildJobReportFingerprintStableAndMateriallyChanges(t *testing.T) {
	ctx := context.Background()
	store := openReportStore(t)
	defer store.Close()
	payload := workflow.JobPayload{
		Repo: "owner/repo",
		Result: &workflow.AgentResult{
			Decision: "failed",
			Summary:  "first failure",
		},
	}
	seedReportJob(t, store, db.Job{
		ID:      "job-fingerprint",
		Agent:   "audit",
		Type:    "review",
		State:   string(workflow.JobFailed),
		Payload: mustReportPayload(t, payload),
	}, "failed")

	first, err := BuildJobReport(ctx, store, "job-fingerprint", JobOptions{})
	if err != nil {
		t.Fatalf("BuildJobReport first returned error: %v", err)
	}
	second, err := BuildJobReport(ctx, store, "job-fingerprint", JobOptions{})
	if err != nil {
		t.Fatalf("BuildJobReport second returned error: %v", err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprint unstable: %q != %q", first.Fingerprint, second.Fingerprint)
	}

	payload.Result.Summary = "second failure"
	if err := store.UpdateJobPayload(ctx, "job-fingerprint", mustReportPayload(t, payload)); err != nil {
		t.Fatalf("UpdateJobPayload returned error: %v", err)
	}
	changed, err := BuildJobReport(ctx, store, "job-fingerprint", JobOptions{})
	if err != nil {
		t.Fatalf("BuildJobReport changed returned error: %v", err)
	}
	if changed.Fingerprint == first.Fingerprint {
		t.Fatalf("fingerprint did not change for material error change: %q", changed.Fingerprint)
	}
}

func TestBuildJobReportPrefersInfrastructureDiagnosticOverResultSummary(t *testing.T) {
	ctx := context.Background()
	store := openReportStore(t)
	defer store.Close()
	payload := workflow.JobPayload{
		Repo: "owner/repo",
		Result: &workflow.AgentResult{
			Decision: "approved",
			Summary:  "agent completed successfully",
		},
	}
	seedReportJob(t, store, db.Job{
		ID:      "job-comment-failed",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobFailed),
		Payload: mustReportPayload(t, payload),
	}, "job failed")

	before, err := BuildJobReport(ctx, store, "job-comment-failed", JobOptions{})
	if err != nil {
		t.Fatalf("BuildJobReport before returned error: %v", err)
	}
	if before.SelectedErrorText != "agent completed successfully" {
		t.Fatalf("selected error before infra event = %q", before.SelectedErrorText)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-comment-failed", Kind: "comment_post_failed", Message: "temporary github error"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}

	after, err := BuildJobReport(ctx, store, "job-comment-failed", JobOptions{})
	if err != nil {
		t.Fatalf("BuildJobReport after returned error: %v", err)
	}
	if after.SelectedErrorText != "temporary github error" {
		t.Fatalf("selected error after infra event = %q", after.SelectedErrorText)
	}
	if after.Fingerprint == before.Fingerprint {
		t.Fatalf("fingerprint did not change after priority diagnostic event: %q", after.Fingerprint)
	}
}

func TestBuildJobReportDoesNotLetStaleTransientDiagnosticMaskFinalFailure(t *testing.T) {
	ctx := context.Background()
	store := openReportStore(t)
	defer store.Close()

	tests := []struct {
		name         string
		jobID        string
		finalPayload workflow.JobPayload
		finalMessage string
		want         string
	}{
		{
			name:  "result summary wins after stale wait",
			jobID: "job-stale-wait-summary",
			finalPayload: workflow.JobPayload{
				Repo: "owner/repo",
				Result: &workflow.AgentResult{
					Decision: "failed",
					Summary:  "final optimizer failed",
				},
			},
			finalMessage: "job failed",
			want:         "final optimizer failed",
		},
		{
			name:         "terminal message wins after stale retry",
			jobID:        "job-stale-retry-terminal",
			finalPayload: workflow.JobPayload{Repo: "owner/repo"},
			finalMessage: "adapter exited with status 2",
			want:         "adapter exited with status 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := store.CreateJobWithEvent(ctx, db.Job{
				ID:      tt.jobID,
				Agent:   "audit",
				Type:    "ask",
				State:   string(workflow.JobQueued),
				Payload: mustReportPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
			}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "queued"}); err != nil {
				t.Fatalf("CreateJobWithEvent returned error: %v", err)
			}
			if err := store.AddJobEvent(ctx, db.JobEvent{JobID: tt.jobID, Kind: "runtime_lock_wait", Message: "runtime session busy"}); err != nil {
				t.Fatalf("AddJobEvent returned error: %v", err)
			}
			transitioned, err := store.TransitionJobStatePayloadWithEvent(ctx, tt.jobID, string(workflow.JobQueued), string(workflow.JobFailed), mustReportPayload(t, tt.finalPayload), db.JobEvent{
				Kind:    string(workflow.JobFailed),
				Message: tt.finalMessage,
			})
			if err != nil {
				t.Fatalf("TransitionJobStatePayloadWithEvent returned error: %v", err)
			}
			if !transitioned {
				t.Fatal("TransitionJobStatePayloadWithEvent did not transition")
			}

			report, err := BuildJobReport(ctx, store, tt.jobID, JobOptions{})
			if err != nil {
				t.Fatalf("BuildJobReport returned error: %v", err)
			}
			if report.SelectedErrorText != tt.want {
				t.Fatalf("selected error = %q, want %q", report.SelectedErrorText, tt.want)
			}
		})
	}
}

func TestBuildJobReportIgnoresResolvedPostTerminalDiagnostics(t *testing.T) {
	ctx := context.Background()
	store := openReportStore(t)
	defer store.Close()

	tests := []struct {
		name   string
		jobID  string
		events []db.JobEvent
	}{
		{
			name:  "comment post recovered",
			jobID: "job-comment-recovered",
			events: []db.JobEvent{
				{Kind: "comment_post_failed", Message: "temporary github error"},
				{Kind: "comment_posted", Message: "posted attributed PR result comment"},
			},
		},
		{
			name:  "advance retry recovered",
			jobID: "job-advance-recovered",
			events: []db.JobEvent{
				{Kind: "advance_retry", Message: "post-delivery workflow retry failed"},
				{Kind: "advance_retried", Message: "post-delivery workflow retry completed"},
				{Kind: "advance_completed", Message: "workflow advancement completed"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := workflow.JobPayload{
				Repo: "owner/repo",
				Result: &workflow.AgentResult{
					Decision: "approved",
					Summary:  "agent completed successfully",
				},
			}
			seedReportJob(t, store, db.Job{
				ID:      tt.jobID,
				Agent:   "audit",
				Type:    "ask",
				State:   string(workflow.JobSucceeded),
				Payload: mustReportPayload(t, payload),
			}, "job succeeded")
			for _, event := range tt.events {
				event.JobID = tt.jobID
				if err := store.AddJobEvent(ctx, event); err != nil {
					t.Fatalf("AddJobEvent returned error: %v", err)
				}
			}

			report, err := BuildJobReport(ctx, store, tt.jobID, JobOptions{})
			if err != nil {
				t.Fatalf("BuildJobReport returned error: %v", err)
			}
			if report.SelectedErrorText != "agent completed successfully" {
				t.Fatalf("selected error = %q, want result summary", report.SelectedErrorText)
			}
		})
	}
}

func TestBuildJobReportUsesRecentEventsLimit(t *testing.T) {
	ctx := context.Background()
	store := openReportStore(t)
	defer store.Close()
	seedReportJob(t, store, db.Job{
		ID:      "job-events",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobCancelled),
		Payload: mustReportPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
	}, "queued")
	for _, event := range []db.JobEvent{
		{JobID: "job-events", Kind: "one", Message: "first"},
		{JobID: "job-events", Kind: "two", Message: "second"},
		{JobID: "job-events", Kind: "cancelled", Message: "third"},
	} {
		if err := store.AddJobEvent(ctx, event); err != nil {
			t.Fatalf("AddJobEvent returned error: %v", err)
		}
	}

	report, err := BuildJobReport(ctx, store, "job-events", JobOptions{RecentEventLimit: 2})
	if err != nil {
		t.Fatalf("BuildJobReport returned error: %v", err)
	}
	if strings.Contains(report.Body, "`queued`: queued") || strings.Contains(report.Body, "`one`: first") {
		t.Fatalf("report included events outside limit:\n%s", report.Body)
	}
	for _, want := range []string{"`two`: second", "`cancelled`: third", "Recent events truncated to the latest 2 of 4."} {
		if !strings.Contains(report.Body, want) {
			t.Fatalf("report missing %q:\n%s", want, report.Body)
		}
	}
}

func openReportStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	return store
}

func seedReportJob(t *testing.T, store *db.Store, job db.Job, message string) {
	t.Helper()
	if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{Kind: job.State, Message: message}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
}

func mustReportPayload(t *testing.T, payload workflow.JobPayload) string {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	return string(encoded)
}
