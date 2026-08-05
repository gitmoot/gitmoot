package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/evidence"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func seedSessionAgentRepo(t *testing.T, store *db.Store) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", Enabled: true}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "lead",
		Role:         "agent",
		Runtime:      runtime.ShellRuntime,
		RuntimeRef:   "printf ok",
		RepoScope:    "owner/repo",
		Capabilities: []string{"ask", "review", "implement"},
		HealthStatus: "ok",
		Model:        "configured-default-must-not-be-recorded",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
}

func TestJobOpenAndRecordPersistParentJobID(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(home string) []string
	}{
		{
			name: "open",
			args: func(home string) []string {
				return []string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--parent-job-id", "parent-job", "--json"}
			},
		},
		{
			name: "record",
			args: func(home string) []string {
				return []string{"job", "record", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--decision", "approved", "--parent-job-id", "parent-job", "--json"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			defer store.Close()
			seedSessionAgentRepo(t, store)
			if err := store.CreateJob(context.Background(), db.Job{ID: "parent-job", Agent: "lead", Type: "ask", State: string(workflow.JobSucceeded)}); err != nil {
				t.Fatalf("CreateJob(parent) returned error: %v", err)
			}

			var stdout, stderr bytes.Buffer
			if code := Run(tc.args(home), &stdout, &stderr); code != 0 {
				t.Fatalf("%s exit = %d, stderr=%s", tc.name, code, stderr.String())
			}
			var out jobSessionOutput
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("decode %s JSON: %v (%s)", tc.name, err, stdout.String())
			}
			stored, err := store.GetJob(context.Background(), out.JobID)
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			if stored.ParentJobID != "parent-job" {
				t.Fatalf("parent_job_id = %q, want parent-job", stored.ParentJobID)
			}
			payload, err := workflow.ParseJobPayload(stored.Payload)
			if err != nil {
				t.Fatalf("ParseJobPayload returned error: %v", err)
			}
			if payload.ParentJobID != "parent-job" {
				t.Fatalf("payload parent_job_id = %q, want parent-job", payload.ParentJobID)
			}
		})
	}
}

// TestJobOpenCreatesRunningSessionJob proves `job open` creates a running,
// externally_driven job with no queued row (no dispatch).
func TestJobOpenCreatesRunningSessionJob(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--title", "lead session", "--head-sha", "open-head", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var out jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode job open JSON: %v (%s)", err, stdout.String())
	}
	if out.State != string(workflow.JobRunning) || !out.ExternallyDriven || out.Type != "ask" || out.Repo != "owner/repo" {
		t.Fatalf("job open output = %+v", out)
	}
	if out.HeadSHA != "open-head" {
		t.Fatalf("job open head SHA = %q, want open-head", out.HeadSHA)
	}

	stored, err := store.GetJob(context.Background(), out.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(workflow.JobRunning) || !stored.ExternallyDriven {
		t.Fatalf("stored job = %+v, want running externally_driven", stored)
	}
	payload, err := workflow.ParseJobPayload(stored.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.HeadSHA != "" {
		t.Fatalf("gate-visible payload head SHA = %q, want empty (display plane must stay separate)", payload.HeadSHA)
	}
	if got := loadSessionJobDisplayHeadSHA(context.Background(), store, out.JobID); got != "open-head" {
		t.Fatalf("display head SHA = %q, want open-head", got)
	}
	queued, err := store.ListQueuedJobs(context.Background())
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	if len(queued) != 0 {
		t.Fatalf("queued = %d, want 0", len(queued))
	}
}

// TestJobCloseAppliesDecision proves open+close moves the job to its terminal state
// with the session's result.
func TestJobCloseAppliesDecision(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "review", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var opened jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
		t.Fatalf("decode open JSON: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "close", opened.JobID, "--home", home, "--decision", "changes_requested", "--summary", "needs work", "--pr", "9", "--head-sha", "close-head", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job close exit = %d, stderr=%s", code, stderr.String())
	}
	var closed jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &closed); err != nil {
		t.Fatalf("decode close JSON: %v", err)
	}
	if closed.State != string(workflow.JobSucceeded) || closed.Decision != "changes_requested" || closed.PullRequest != 9 || closed.HeadSHA != "close-head" {
		t.Fatalf("close output = %+v", closed)
	}
	stored, err := store.GetJob(context.Background(), opened.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(workflow.JobSucceeded) {
		t.Fatalf("stored state = %q, want succeeded", stored.State)
	}
	payload, err := workflow.ParseJobPayload(stored.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.HeadSHA != "" {
		t.Fatalf("gate-visible payload head SHA = %q, want empty (display plane must stay separate)", payload.HeadSHA)
	}
	if payload.ReviewStatusGrade != evidence.GradeReported {
		t.Fatalf("stored review status grade = %q, want reported", payload.ReviewStatusGrade)
	}
	if got := loadSessionJobDisplayHeadSHA(context.Background(), store, opened.JobID); got != "close-head" {
		t.Fatalf("display head SHA = %q, want close-head", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", opened.JobID, "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show --json exit = %d, stderr=%s", code, stderr.String())
	}
	var shown jobShowOutput
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatalf("decode job show JSON: %v (%s)", err, stdout.String())
	}
	if shown.ReviewStatusGrade != evidence.GradeReported {
		t.Fatalf("job show review status grade = %q, want reported", shown.ReviewStatusGrade)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --json exit = %d, stderr=%s", code, stderr.String())
	}
	var listed []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("decode job list JSON: %v (%s)", err, stdout.String())
	}
	if len(listed) != 1 || listed[0].ID != opened.JobID || listed[0].ReviewStatusGrade != evidence.GradeReported {
		t.Fatalf("job list review = %+v, want closed review with reported grade", listed)
	}
}

func TestJobCloseAndRecordPersistUsageEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, home string) string
	}{
		{
			name: "close",
			run: func(t *testing.T, home string) string {
				t.Helper()
				var stdout, stderr bytes.Buffer
				if code := Run([]string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--json"}, &stdout, &stderr); code != 0 {
					t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
				}
				var opened jobSessionOutput
				if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
					t.Fatalf("decode open JSON: %v", err)
				}
				if code := Run([]string{"job", "close", opened.JobID, "--home", home, "--decision", "approved", "--model", "reported-model", "--input-tokens", "123", "--output-tokens", "45"}, &bytes.Buffer{}, &stderr); code != 0 {
					t.Fatalf("job close exit = %d, stderr=%s", code, stderr.String())
				}
				return opened.JobID
			},
		},
		{
			name: "record",
			run: func(t *testing.T, home string) string {
				t.Helper()
				var stdout, stderr bytes.Buffer
				if code := Run([]string{"job", "record", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--decision", "approved", "--model", "reported-model", "--input-tokens", "123", "--output-tokens", "45", "--json"}, &stdout, &stderr); code != 0 {
					t.Fatalf("job record exit = %d, stderr=%s", code, stderr.String())
				}
				var recorded jobSessionOutput
				if err := json.Unmarshal(stdout.Bytes(), &recorded); err != nil {
					t.Fatalf("decode record JSON: %v", err)
				}
				return recorded.JobID
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			defer store.Close()
			seedSessionAgentRepo(t, store)
			jobID := tc.run(t, home)
			stored, err := store.GetJob(context.Background(), jobID)
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			if stored.Model != "reported-model" || stored.InputTokens != 123 || stored.OutputTokens != 45 {
				t.Fatalf("usage evidence = model %q tokens %d/%d, want reported-model 123/45", stored.Model, stored.InputTokens, stored.OutputTokens)
			}
		})
	}
}

func TestJobSessionOmittedEvidencePreservesEmptyDefaultsAndEvents(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var opened jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
		t.Fatalf("decode open JSON: %v", err)
	}
	if code := Run([]string{"job", "close", opened.JobID, "--home", home, "--decision", "approved"}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("job close exit = %d, stderr=%s", code, stderr.String())
	}
	stored, err := store.GetJob(context.Background(), opened.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.Model != "" || stored.InputTokens != 0 || stored.OutputTokens != 0 || stored.ParentJobID != "" {
		t.Fatalf("omitted evidence changed row: model=%q tokens=%d/%d parent=%q", stored.Model, stored.InputTokens, stored.OutputTokens, stored.ParentJobID)
	}
	events, err := store.ListJobEvents(context.Background(), opened.JobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 2 || events[0].Kind != "running" || events[0].Message != "job started (externally driven session)" || events[1].Kind != "succeeded" || events[1].Message != "job succeeded" {
		t.Fatalf("omitted evidence changed events: %+v", events)
	}
}

func TestJobSessionUnknownParentRefusedWithoutCreatingRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(home string) []string
	}{
		{"open", func(home string) []string {
			return []string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--parent-job-id", "missing-parent"}
		}},
		{"record", func(home string) []string {
			return []string{"job", "record", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--decision", "approved", "--parent-job-id", "missing-parent"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			defer store.Close()
			seedSessionAgentRepo(t, store)
			var stdout, stderr bytes.Buffer
			if code := Run(tc.args(home), &stdout, &stderr); code != 1 {
				t.Fatalf("%s exit = %d, want 1; stderr=%s", tc.name, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), `parent job "missing-parent" not found`) {
				t.Fatalf("%s stderr = %q, want clear unknown-parent refusal", tc.name, stderr.String())
			}
			jobs, err := store.ListJobs(context.Background())
			if err != nil {
				t.Fatalf("ListJobs returned error: %v", err)
			}
			if len(jobs) != 0 {
				t.Fatalf("unknown parent created jobs: %+v", jobs)
			}
		})
	}
}

// TestJobRecordOneShotTerminal proves `job record` creates an already-terminal job.
func TestJobRecordOneShotTerminal(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "record", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "implement", "--decision", "implemented", "--summary", "done", "--pr", "12", "--head-sha", "record-head", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job record exit = %d, stderr=%s", code, stderr.String())
	}
	var out jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode record JSON: %v", err)
	}
	if out.State != string(workflow.JobSucceeded) || out.Decision != "implemented" || !out.ExternallyDriven || out.HeadSHA != "record-head" {
		t.Fatalf("record output = %+v", out)
	}
	stored, err := store.GetJob(context.Background(), out.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(workflow.JobSucceeded) || !stored.ExternallyDriven {
		t.Fatalf("stored job = %+v", stored)
	}
	payload, err := workflow.ParseJobPayload(stored.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.HeadSHA != "" {
		t.Fatalf("gate-visible payload head SHA = %q, want empty (display plane must stay separate)", payload.HeadSHA)
	}
	if got := loadSessionJobDisplayHeadSHA(context.Background(), store, out.JobID); got != "record-head" {
		t.Fatalf("display head SHA = %q, want record-head", got)
	}
}

func TestSessionReviewStatusIsReportedNonAuthoritativeDisplay(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	openReview := func(workflowID, headSHA string) jobSessionOutput {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := Run([]string{
			"job", "open", "--home", home, "--agent", "lead",
			"--repo", "owner/repo", "--type", "review",
			"--workflow", workflowID, "--pr", "12", "--head-sha", headSHA, "--json",
		}, &stdout, &stderr); code != 0 {
			t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
		}
		var out jobSessionOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("decode job open JSON: %v (%s)", err, stdout.String())
		}
		return out
	}

	fresh := openReview("review/fresh", "fresh-head")
	stalled := openReview("review/stalled", "stalled-head")
	noted := openReview("review/noted", "noted-head")

	old := time.Now().UTC().Add(-sessionReviewStaleGap - time.Minute).Format("2006-01-02 15:04:05")
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	defer raw.Close()
	for _, id := range []string{stalled.JobID, noted.JobID} {
		if _, err := raw.Exec(`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`, old, old, id); err != nil {
			t.Fatalf("backdate job %s: %v", id, err)
		}
	}
	// This note is deliberately a caller assertion with no trusted reviewer
	// attribution. It may refresh the display, but the grade/authority labels
	// below must prevent the hint from masquerading as merge evidence.
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "review/noted",
		Author:     "unrelated-caller",
		Body:       "reported progress",
	}); err != nil {
		t.Fatalf("InsertWorkflowNote returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --json exit = %d, stderr=%s", code, stderr.String())
	}
	var entries []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("decode job list JSON: %v (%s)", err, stdout.String())
	}
	byID := make(map[string]jobListEntry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	for id, wantStatus := range map[string]string{
		fresh.JobID:   reviewStatusInProgress,
		stalled.JobID: reviewStatusStalled,
		noted.JobID:   reviewStatusInProgress,
	} {
		got := byID[id]
		if got.ReviewStatus != wantStatus ||
			got.ReviewStatusGrade != evidence.GradeReported ||
			got.ReviewStatusAuthority != reviewStatusAuthorityNonAuthoritative {
			t.Fatalf("review %s display = %+v, want %s/reported/non_authoritative", id, got, wantStatus)
		}
	}
	if got := byID[fresh.JobID].HeadSHA; got != "fresh-head" {
		t.Fatalf("fresh review head SHA = %q, want fresh-head", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "list", "--home", home, "--workflow", "review/fresh", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow-filtered job list --json exit = %d, stderr=%s", code, stderr.String())
	}
	var filtered []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &filtered); err != nil {
		t.Fatalf("decode workflow-filtered list JSON: %v (%s)", err, stdout.String())
	}
	if len(filtered) != 1 ||
		filtered[0].ID != fresh.JobID ||
		filtered[0].ReviewStatus != reviewStatusInProgress ||
		filtered[0].ReviewStatusGrade != evidence.GradeReported ||
		filtered[0].ReviewStatusAuthority != reviewStatusAuthorityNonAuthoritative ||
		filtered[0].HeadSHA != "fresh-head" {
		t.Fatalf("workflow-filtered review = %+v", filtered)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "\thead=fresh-head\tREVIEW (reported; non-authoritative): in_progress") {
		t.Fatalf("job list text lacks explicit non-authoritative label:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", stalled.JobID, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show exit = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"review_status: stalled",
		"review_status_grade: reported",
		"review_status_authority: non_authoritative",
		"head_sha: stalled-head",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("job show missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSessionReviewDisplayNeverWritesTaskState(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)
	if err := store.UpsertTask(context.Background(), db.Task{
		ID:           "task-display-only",
		RepoFullName: "owner/repo",
		State:        string(workflow.TaskImplementing),
		Branch:       "display-only",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`
CREATE TRIGGER forbid_session_review_task_state
BEFORE UPDATE OF state ON tasks
BEGIN
	SELECT RAISE(ABORT, 'display-only review attempted to write tasks.state');
END`); err != nil {
		t.Fatalf("create tasks.state guard trigger: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"job", "open", "--home", home, "--agent", "lead",
		"--repo", "owner/repo", "--type", "review",
		"--task", "task-display-only", "--workflow", "review/display-only",
		"--pr", "12", "--head-sha", "display-head", "--json",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var opened jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
		t.Fatalf("decode job open JSON: %v (%s)", err, stdout.String())
	}
	if code := Run([]string{
		"workflow", "note", "review/display-only", "reported progress",
		"--home", home, "--no-auto",
	}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("workflow note exit = %d, stderr=%s", code, stderr.String())
	}
	if code := Run([]string{"job", "list", "--home", home, "--state", "running", "--json"}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("job list exit = %d, stderr=%s", code, stderr.String())
	}
	if code := Run([]string{"job", "show", opened.JobID, "--home", home, "--json"}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("job show exit = %d, stderr=%s", code, stderr.String())
	}
	if code := Run([]string{
		"job", "close", opened.JobID, "--home", home,
		"--decision", "approved", "--summary", "reported verdict",
	}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("job close exit = %d, stderr=%s", code, stderr.String())
	}

	task, err := store.GetTask(context.Background(), "task-display-only")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskImplementing) {
		t.Fatalf("task state = %q, want unchanged %q", task.State, workflow.TaskImplementing)
	}
}

// TestJobSessionValidationErrors proves the CLI rejects bad input cleanly.
func TestJobSessionValidationErrors(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"invalid type", []string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "bogus"}, 2},
		{"invalid decision", []string{"job", "record", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--decision", "bogus"}, 2},
		{"missing agent", []string{"job", "open", "--home", home, "--repo", "owner/repo", "--type", "ask"}, 2},
		{"unknown agent", []string{"job", "open", "--home", home, "--agent", "ghost", "--repo", "owner/repo", "--type", "ask"}, 1},
		{"untracked repo", []string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/nope", "--type", "ask"}, 1},
		{"close unknown id", []string{"job", "close", "no-such-job", "--home", home, "--decision", "approved"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(tc.args, &stdout, &stderr); code != tc.want {
				t.Fatalf("exit = %d, want %d; stderr=%s", code, tc.want, stderr.String())
			}
		})
	}
}

// TestJobCloseDoubleCloseFails proves a session job can be closed exactly once.
func TestJobCloseDoubleCloseFails(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var opened jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
		t.Fatalf("decode open JSON: %v", err)
	}
	if code := Run([]string{"job", "close", opened.JobID, "--home", home, "--decision", "approved", "--model", "first-model", "--input-tokens", "1", "--output-tokens", "2"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first close failed")
	}
	stderr.Reset()
	if code := Run([]string{"job", "close", opened.JobID, "--home", home, "--decision", "approved", "--model", "second-model", "--input-tokens", "3", "--output-tokens", "4"}, &bytes.Buffer{}, &stderr); code != 1 {
		t.Fatalf("second close exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already been closed") {
		t.Fatalf("double-close stderr = %q, want already-been-closed", stderr.String())
	}
}
