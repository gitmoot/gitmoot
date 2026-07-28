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
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
}

func noteSessionReviewHeartbeat(t *testing.T, home, workflowID, jobID, body string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"workflow", "note", workflowID, body,
		"--author", "lead", "--job", jobID, "--home", home, "--no-auto",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow note heartbeat for %s exit = %d, stderr=%s", jobID, code, stderr.String())
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
	if payload.HeadSHA != "open-head" {
		t.Fatalf("stored head SHA = %q, want open-head", payload.HeadSHA)
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
	if code := Run([]string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--json"}, &stdout, &stderr); code != 0 {
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
	if payload.HeadSHA != "close-head" {
		t.Fatalf("stored head SHA = %q, want close-head", payload.HeadSHA)
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
	if payload.HeadSHA != "record-head" {
		t.Fatalf("stored head SHA = %q, want record-head", payload.HeadSHA)
	}
}

func TestSessionReviewStatusDistinguishesProgressAndStall(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	open := func(typeName, workflowID, headSHA string) jobSessionOutput {
		t.Helper()
		args := []string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", typeName, "--json"}
		if typeName == "review" {
			args = append(args, "--pr", "7")
		}
		if workflowID != "" {
			args = append(args, "--workflow", workflowID)
		}
		if headSHA != "" {
			args = append(args, "--head-sha", headSHA)
		}
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
		}
		var out jobSessionOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("decode job open JSON: %v (%s)", err, stdout.String())
		}
		return out
	}

	fresh := open("review", "review/fresh", "fresh-head")
	stalled := open("review", "review/stalled", "stalled-head")
	unknown := open("review", "review/unknown", "unknown-head")
	otherType := open("ask", "review/ask", "")
	terminal := open("review", "review/terminal", "terminal-head")

	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID:      "dispatched-review",
		Agent:   "lead",
		Type:    "review",
		State:   string(workflow.JobRunning),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
	}, db.JobEvent{Kind: string(workflow.JobRunning), Message: "job started"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if code := Run([]string{"job", "close", terminal.JobID, "--home", home, "--decision", "approved"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("job close terminal fixture exit = %d", code)
	}

	old := time.Now().UTC().Add(-sessionReviewStaleGap - time.Minute).Format("2006-01-02 15:04:05")
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`, old, old, stalled.JobID); err != nil {
		t.Fatalf("backdate job %s: %v", stalled.JobID, err)
	}
	for _, note := range []struct {
		workflowID string
		jobID      string
		body       string
	}{
		{"review/fresh", fresh.JobID, "reviewed fresh tests"},
		{"review/stalled", stalled.JobID, "reviewed stale tests"},
	} {
		noteSessionReviewHeartbeat(t, home, note.workflowID, note.jobID, note.body)
	}
	if _, err := raw.Exec(
		`UPDATE job_events SET created_at = ? WHERE job_id = ? AND kind = ?`,
		old, stalled.JobID, sessionReviewHeartbeatEventKind,
	); err != nil {
		t.Fatalf("backdate stalled heartbeat: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --json exit = %d, stderr=%s", code, stderr.String())
	}
	var entries []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("decode job list JSON: %v (%s)", err, stdout.String())
	}
	var rawEntries []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &rawEntries); err != nil {
		t.Fatalf("decode raw job list JSON: %v (%s)", err, stdout.String())
	}
	byID := make(map[string]jobListEntry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	rawByID := make(map[string]map[string]any, len(rawEntries))
	for _, entry := range rawEntries {
		rawByID[entry["id"].(string)] = entry
	}
	if got := byID[fresh.JobID]; got.ReviewStatus != reviewStatusInProgress || got.HeadSHA != "fresh-head" {
		t.Fatalf("fresh review = %+v, want in_progress with head SHA", got)
	}
	if got := byID[stalled.JobID].ReviewStatus; got != reviewStatusStalled {
		t.Fatalf("stalled review status = %q, want %q", got, reviewStatusStalled)
	}
	if got := byID[unknown.JobID].ReviewStatus; got != reviewStatusUnknown {
		t.Fatalf("review without an observed heartbeat = %q, want %q", got, reviewStatusUnknown)
	}
	for _, id := range []string{otherType.JobID, "dispatched-review", terminal.JobID} {
		if got := byID[id].ReviewStatus; got != "" {
			t.Fatalf("ineligible job %s review status = %q, want omitted", id, got)
		}
		if _, present := rawByID[id]["review_status"]; present {
			t.Fatalf("ineligible job %s unexpectedly includes review_status: %+v", id, rawByID[id])
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "list", "--home", home, "--workflow", "review/fresh", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("workflow-filtered job list --json exit = %d, stderr=%s", code, stderr.String())
	}
	var workflowEntries []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &workflowEntries); err != nil {
		t.Fatalf("decode workflow-filtered job list JSON: %v (%s)", err, stdout.String())
	}
	if len(workflowEntries) != 1 ||
		workflowEntries[0].ID != fresh.JobID ||
		workflowEntries[0].ReviewStatus != reviewStatusInProgress ||
		workflowEntries[0].HeadSHA != "fresh-head" {
		t.Fatalf("workflow-filtered review = %+v, want same in_progress/head_sha fields as plain list", workflowEntries)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exit = %d, stderr=%s", code, stderr.String())
	}
	textRows := stdout.String()
	if !strings.Contains(textRows, fresh.JobID+"\trunning\treview") ||
		!strings.Contains(textRows, "\thead=fresh-head\tREVIEW: in_progress") ||
		!strings.Contains(textRows, stalled.JobID+"\trunning\treview") ||
		!strings.Contains(textRows, "\tREVIEW: stalled") ||
		!strings.Contains(textRows, unknown.JobID+"\trunning\treview") ||
		!strings.Contains(textRows, "\tREVIEW: unknown") {
		t.Fatalf("job list text missing review status/head badges:\n%s", textRows)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", stalled.JobID, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "review_status: stalled") {
		t.Fatalf("job show missing stalled review status:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", fresh.JobID, "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show --json exit = %d, stderr=%s", code, stderr.String())
	}
	var shown jobShowOutput
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatalf("decode job show JSON: %v (%s)", err, stdout.String())
	}
	if shown.ReviewStatus != reviewStatusInProgress || shown.Payload.HeadSHA != "fresh-head" {
		t.Fatalf("fresh job show = %+v, want in_progress with fresh-head", shown)
	}
}

func TestSessionReviewStatusReusedWorkflowLabel(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	openReview := func(headSHA string) jobSessionOutput {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := Run([]string{
			"job", "open", "--home", home, "--agent", "lead",
			"--repo", "owner/repo", "--type", "review",
			"--workflow", "review/reused", "--pr", "7", "--head-sha", headSHA, "--json",
		}, &stdout, &stderr); code != 0 {
			t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
		}
		var out jobSessionOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("decode job open JSON: %v (%s)", err, stdout.String())
		}
		return out
	}

	first := openReview("head-a")
	noteSessionReviewHeartbeat(t, home, "review/reused", first.JobID, "review A progress")
	if code := Run([]string{
		"job", "close", first.JobID, "--home", home,
		"--decision", "approved", "--summary", "review A done",
	}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("job close review A exit = %d", code)
	}
	second := openReview("head-b")

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	defer raw.Close()
	setSecondCreatedAt := func(at time.Time) db.Job {
		t.Helper()
		stamp := at.Format("2006-01-02 15:04:05")
		if _, err := raw.Exec(
			`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`,
			stamp, stamp, second.JobID,
		); err != nil {
			t.Fatalf("set review B timestamp: %v", err)
		}
		job, err := store.GetJob(context.Background(), second.JobID)
		if err != nil {
			t.Fatalf("GetJob(review B) returned error: %v", err)
		}
		return job
	}

	// Review A's job-bound note is not evidence that review B is active.
	freshSecond := setSecondCreatedAt(now.Add(-10 * time.Minute))
	if got := deriveReviewStatus(context.Background(), store, freshSecond, now); got != reviewStatusUnknown {
		t.Fatalf("fresh review B with review-A heartbeat = %q, want %q", got, reviewStatusUnknown)
	}

	// Aging the open record does not turn an unrelated job-bound note into review B
	// liveness evidence.
	staleSecond := setSecondCreatedAt(now.Add(-sessionReviewStaleGap - time.Second))
	if got := deriveReviewStatus(context.Background(), store, staleSecond, now); got != reviewStatusUnknown {
		t.Fatalf("stale review B with review-A heartbeat = %q, want %q", got, reviewStatusUnknown)
	}
}

func TestSessionReviewStatusExactStalenessBoundary(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"job", "open", "--home", home, "--agent", "lead",
		"--repo", "owner/repo", "--type", "review",
		"--workflow", "review/boundary", "--pr", "7", "--head-sha", "boundary-head", "--json",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var opened jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
		t.Fatalf("decode job open JSON: %v (%s)", err, stdout.String())
	}
	noteSessionReviewHeartbeat(t, home, "review/boundary", opened.JobID, "boundary heartbeat")

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	defer raw.Close()
	oldJobAt := now.Add(-time.Hour).Format("2006-01-02 15:04:05")
	if _, err := raw.Exec(
		`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`,
		oldJobAt, oldJobAt, opened.JobID,
	); err != nil {
		t.Fatalf("backdate boundary job: %v", err)
	}
	job, err := store.GetJob(context.Background(), opened.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}

	setHeartbeatAt := func(at time.Time) {
		t.Helper()
		if _, err := raw.Exec(
			`UPDATE job_events SET created_at = ? WHERE job_id = ? AND kind = ?`,
			at.Format("2006-01-02 15:04:05"), opened.JobID, sessionReviewHeartbeatEventKind,
		); err != nil {
			t.Fatalf("set boundary heartbeat timestamp: %v", err)
		}
	}

	setHeartbeatAt(now.Add(-sessionReviewStaleGap))
	if got := deriveReviewStatus(context.Background(), store, job, now); got != reviewStatusInProgress {
		t.Fatalf("review at exact stale gap = %q, want %q", got, reviewStatusInProgress)
	}

	setHeartbeatAt(now.Add(-sessionReviewStaleGap - time.Second))
	if got := deriveReviewStatus(context.Background(), store, job, now); got != reviewStatusStalled {
		t.Fatalf("review one second past stale gap = %q, want %q", got, reviewStatusStalled)
	}
}

func TestSessionReviewStatusIgnoresFreshDaemonLifecycleNote(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"job", "open", "--home", home, "--agent", "lead",
		"--repo", "owner/repo", "--type", "review",
		"--workflow", "review/daemon-noise", "--pr", "7", "--head-sha", "review-head", "--json",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var opened jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
		t.Fatalf("decode job open JSON: %v (%s)", err, stdout.String())
	}
	noteSessionReviewHeartbeat(t, home, "review/daemon-noise", opened.JobID, "reviewer checkpoint")
	daemonNote, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "review/daemon-noise",
		Author:     db.WorkflowAutoNoteAuthor,
		Body:       "[auto:pr:7:ready] unrelated PR lifecycle activity",
	})
	if err != nil {
		t.Fatalf("Insert daemon note: %v", err)
	}

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(
		`UPDATE job_events SET created_at = ? WHERE job_id = ? AND kind = ?`,
		now.Add(-sessionReviewStaleGap-time.Minute).Format("2006-01-02 15:04:05"),
		opened.JobID, sessionReviewHeartbeatEventKind,
	); err != nil {
		t.Fatalf("backdate reviewer heartbeat: %v", err)
	}
	if _, err := raw.Exec(
		`UPDATE workflow_notes SET created_at = ? WHERE id = ?`,
		now.Add(-time.Minute).Format("2006-01-02 15:04:05"), daemonNote.ID,
	); err != nil {
		t.Fatalf("freshen daemon note: %v", err)
	}
	if _, err := raw.Exec(
		`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`,
		now.Add(-time.Hour).Format("2006-01-02 15:04:05"),
		now.Add(-time.Hour).Format("2006-01-02 15:04:05"),
		opened.JobID,
	); err != nil {
		t.Fatalf("backdate review job: %v", err)
	}
	job, err := store.GetJob(context.Background(), opened.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}

	if got := deriveReviewStatus(context.Background(), store, job, now); got != reviewStatusStalled {
		t.Fatalf("stalled review with fresh daemon lifecycle note = %q, want %q", got, reviewStatusStalled)
	}
}

func TestSessionReviewStatusDoesNotCrossRefreshSameAgentConcurrentReviews(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	openReview := func(pr, head string) jobSessionOutput {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := Run([]string{
			"job", "open", "--home", home, "--agent", "lead",
			"--repo", "owner/repo", "--type", "review",
			"--workflow", "review/concurrent", "--pr", pr, "--head-sha", head, "--json",
		}, &stdout, &stderr); code != 0 {
			t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
		}
		var opened jobSessionOutput
		if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
			t.Fatalf("decode job open JSON: %v (%s)", err, stdout.String())
		}
		return opened
	}

	active := openReview("7", "active-head")
	stalled := openReview("8", "stalled-head")
	noteSessionReviewHeartbeat(t, home, "review/concurrent", stalled.JobID, "review 8 checkpoint")

	now := time.Now().UTC().Truncate(time.Second)
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	defer raw.Close()
	oldJobAt := now.Add(-time.Hour).Format("2006-01-02 15:04:05")
	if _, err := raw.Exec(
		`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id IN (?, ?)`,
		oldJobAt, oldJobAt, active.JobID, stalled.JobID,
	); err != nil {
		t.Fatalf("backdate concurrent review jobs: %v", err)
	}
	if _, err := raw.Exec(
		`UPDATE job_events SET created_at = ? WHERE job_id = ? AND kind = ?`,
		now.Add(-sessionReviewStaleGap-time.Minute).Format("2006-01-02 15:04:05"),
		stalled.JobID, sessionReviewHeartbeatEventKind,
	); err != nil {
		t.Fatalf("backdate stalled review heartbeat: %v", err)
	}

	// The same caller-controlled author now records fresh activity for the other
	// review under the same workflow. Only that exact job may become in_progress.
	noteSessionReviewHeartbeat(t, home, "review/concurrent", active.JobID, "review 7 checkpoint")
	heartbeat, ok, err := store.GetLatestJobEventByKind(
		context.Background(), active.JobID, sessionReviewHeartbeatEventKind,
	)
	if err != nil || !ok {
		t.Fatalf("active review heartbeat = (%+v, %v, %v)", heartbeat, ok, err)
	}
	if !strings.Contains(heartbeat.Message, "owner/repo#7 at active-head") {
		t.Fatalf("active review heartbeat lacks stored PR/HEAD target: %q", heartbeat.Message)
	}

	activeJob, err := store.GetJob(context.Background(), active.JobID)
	if err != nil {
		t.Fatalf("GetJob(active) returned error: %v", err)
	}
	stalledJob, err := store.GetJob(context.Background(), stalled.JobID)
	if err != nil {
		t.Fatalf("GetJob(stalled) returned error: %v", err)
	}
	observedAt := time.Now().UTC().Add(time.Second)
	if got := deriveReviewStatus(context.Background(), store, activeJob, observedAt); got != reviewStatusInProgress {
		t.Fatalf("active review status = %q, want %q", got, reviewStatusInProgress)
	}
	if got := deriveReviewStatus(context.Background(), store, stalledJob, observedAt); got != reviewStatusStalled {
		t.Fatalf("stalled same-agent review after other review heartbeat = %q, want %q", got, reviewStatusStalled)
	}
}

func TestSessionReviewStatusUnknownWhenHeartbeatCannotBeRead(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"job", "open", "--home", home, "--agent", "lead",
		"--repo", "owner/repo", "--type", "review",
		"--workflow", "review/unreadable", "--pr", "7", "--head-sha", "review-head", "--json",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var opened jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
		t.Fatalf("decode job open JSON: %v (%s)", err, stdout.String())
	}
	job, err := store.GetJob(context.Background(), opened.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if got := deriveReviewStatus(context.Background(), store, job, time.Now().UTC()); got != reviewStatusUnknown {
		t.Fatalf("review status after heartbeat-query failure = %q, want %q", got, reviewStatusUnknown)
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
		{"review missing pr", []string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "review", "--head-sha", "deadbeef"}, 2},
		{"review missing head", []string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "review", "--pr", "7"}, 2},
		{"record review missing pr", []string{"job", "record", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "review", "--decision", "approved", "--head-sha", "deadbeef"}, 2},
		{"record review missing head", []string{"job", "record", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "review", "--decision", "approved", "--pr", "7"}, 2},
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
	if code := Run([]string{"job", "close", opened.JobID, "--home", home, "--decision", "approved"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first close failed")
	}
	stderr.Reset()
	if code := Run([]string{"job", "close", opened.JobID, "--home", home, "--decision", "approved"}, &bytes.Buffer{}, &stderr); code != 1 {
		t.Fatalf("second close exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already been closed") {
		t.Fatalf("double-close stderr = %q, want already-been-closed", stderr.String())
	}
}
