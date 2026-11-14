package db

import (
	"context"
	"testing"
	"time"
)

func TestDashboardChangeCursor(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()

	events, notes, taskEvents, err := store.DashboardChangeCursor(ctx)
	if err != nil {
		t.Fatalf("DashboardChangeCursor(empty): %v", err)
	}
	if events != 0 || notes != 0 || taskEvents != 0 {
		t.Fatalf("empty cursor = %d.%d.%d, want 0.0.0", events, notes, taskEvents)
	}

	if err := store.CreateJobWithEvent(ctx,
		Job{ID: "job-1", Agent: "worker", Type: "ask", State: "queued"},
		JobEvent{Kind: "queued", Message: "created"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	events, notes, taskEvents, err = store.DashboardChangeCursor(ctx)
	if err != nil || events != 1 || notes != 0 || taskEvents != 0 {
		t.Fatalf("event cursor = %d.%d.%d, err=%v, want 1.0.0", events, notes, taskEvents, err)
	}

	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: "release/one", Body: "checkpoint"}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}
	events, notes, taskEvents, err = store.DashboardChangeCursor(ctx)
	if err != nil || events != 1 || notes != 1 || taskEvents != 0 {
		t.Fatalf("note cursor = %d.%d.%d, err=%v, want 1.1.0", events, notes, taskEvents, err)
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-1", State: "implementing"}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.AddTaskEvent(ctx, TaskEvent{TaskID: "task-1", Kind: "task_dismissed_manual", FromState: "implementing", ToState: "dismissed"}); err != nil {
		t.Fatalf("AddTaskEvent: %v", err)
	}
	events, notes, taskEvents, err = store.DashboardChangeCursor(ctx)
	if err != nil || events != 1 || notes != 1 || taskEvents != 1 {
		t.Fatalf("task event cursor = %d.%d.%d, err=%v, want 1.1.1", events, notes, taskEvents, err)
	}

	events2, notes2, taskEvents2, err := store.DashboardChangeCursor(ctx)
	if err != nil || events2 != events || notes2 != notes || taskEvents2 != taskEvents {
		t.Fatalf("repeat cursor = %d.%d.%d, err=%v, want %d.%d.%d", events2, notes2, taskEvents2, err, events, notes, taskEvents)
	}
}

func TestListDashboardTasksExcludesDismissed(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	for _, task := range []Task{
		{ID: "visible", State: "implementing"},
		{ID: "hidden", State: "dismissed"},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatalf("UpsertTask(%s): %v", task.ID, err)
		}
	}
	tasks, err := store.ListDashboardTasks(ctx, "2000-01-01 00:00:00")
	if err != nil {
		t.Fatalf("ListDashboardTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "visible" {
		t.Fatalf("dashboard tasks = %+v, want only visible", tasks)
	}
}

// TestListDashboardTasksOwnerAgreesWithPullRequestActingRole pins the shared
// pullRequestActingRoleSelector: the dashboard's bulk task owner and
// PullRequestActingRole pick the same implement job (earliest created_at, not
// lowest id), ignore other job types, and leave a PR-less task unattributed.
func TestListDashboardTasksOwnerAgreesWithPullRequestActingRole(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const repo = "acme/widget"
	for _, task := range []Task{
		{ID: "with-pr", RepoFullName: repo, Branch: "feat", State: "pr_open"},
		{ID: "no-pr", RepoFullName: repo, Branch: "other", State: "implementing"},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatalf("UpsertTask(%s): %v", task.ID, err)
		}
	}
	if err := store.UpsertPullRequest(ctx, PullRequest{RepoFullName: repo, Number: 12, HeadBranch: "feat", State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	jobs := []struct {
		id, typ, payload, createdAt string
	}{
		{"impl-a", "implement", `{"repo":"acme/widget","pull_request":12,"acting_org_role":"second"}`, "2026-11-01 10:05:00"},
		{"impl-b", "implement", `{"repo":"acme/widget","pull_request":12,"acting_org_role":"first"}`, "2026-11-01 10:00:00"},
		{"review-0", "review", `{"repo":"acme/widget","pull_request":12,"acting_org_role":"reviewer"}`, "2026-11-01 09:00:00"},
		{"impl-nopr", "implement", `{"repo":"acme/widget","acting_org_role":"stray"}`, "2026-11-01 08:00:00"},
	}
	for _, j := range jobs {
		if err := store.CreateJob(ctx, Job{ID: j.id, Agent: "worker", Type: j.typ, State: "succeeded", Payload: j.payload}); err != nil {
			t.Fatalf("CreateJob(%s): %v", j.id, err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`, j.createdAt, j.id); err != nil {
			t.Fatal(err)
		}
	}

	role, err := store.PullRequestActingRole(ctx, repo, 12)
	if err != nil || role != "first" {
		t.Fatalf("PullRequestActingRole = %q, err=%v, want first (earliest implement job)", role, err)
	}
	tasks, err := store.ListDashboardTasks(ctx, "2000-01-01 00:00:00")
	if err != nil {
		t.Fatalf("ListDashboardTasks: %v", err)
	}
	byID := map[string]DashboardTaskRow{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	if got := byID["with-pr"]; got.Agent != role || got.PRNumber != 12 {
		t.Fatalf("with-pr row = %+v, want owner %q on PR 12", got, role)
	}
	if got := byID["no-pr"]; got.Agent != "" || got.PRNumber != 0 {
		t.Fatalf("no-pr row = %+v, want no owner and no PR", got)
	}
}

func TestListDashboardUnlabeledJobsIsWindowedAndLabelFiltered(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	for _, job := range []Job{
		{ID: "recent-empty", Type: "ask", State: "queued", Payload: `{"workflow_id":""}`},
		{ID: "old-empty", Type: "ask", State: "queued", Payload: `{"workflow_id":""}`},
		{ID: "recent-labeled", Type: "ask", State: "queued", Payload: `{"workflow_id":"team/campaign"}`},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s): %v", job.ID, err)
		}
	}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = 'recent-empty'`, now.Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = 'old-empty'`, now.Add(-25*time.Hour).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = 'recent-labeled'`, now.Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListDashboardUnlabeledJobs(ctx, now.Add(-24*time.Hour).Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "recent-empty" || rows[0].WorkflowID != "" {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestListDashboardJobSummariesProjectsLegacyAndMalformedPayloads(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, Agent{Name: "registered", Runtime: "codex"}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	jobs := []Job{
		{
			ID: "current", Agent: "registered", Type: "ask", State: "running",
			Model:       "gpt-5.6-sol",
			Payload:     `{"instructions":"  current title  ","repo":"acme/current","pull_request":42,"ephemeral":{"runtime":"kimi"}}`,
			InputTokens: 3, OutputTokens: 5,
		},
		{
			ID: "ephemeral", Agent: "temp-worker", Type: "implement", State: "queued",
			Model:   "claude-fable-5",
			Payload: `{"instructions":"inline","repo":"acme/inline","ephemeral":{"runtime":"claude"}}`,
		},
		{
			ID: "legacy", Agent: "registered", Type: "review", State: "succeeded",
			Payload: `{"instructions":"legacy","repo":"acme/legacy","pull_request":7}`,
		},
		{ID: "malformed", Agent: "unknown", Type: "ask", State: "failed", Payload: `{"instructions":`},
	}
	for _, job := range jobs {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s): %v", job.ID, err)
		}
		if job.InputTokens != 0 || job.OutputTokens != 0 {
			if err := store.UpdateJobUsage(ctx, job.ID, job.InputTokens, job.OutputTokens); err != nil {
				t.Fatalf("UpdateJobUsage(%s): %v", job.ID, err)
			}
		}
	}
	// Simulate a row written before repo/pull_request were denormalized.
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET repo = '', pull_request = 0 WHERE id = 'legacy'`); err != nil {
		t.Fatalf("clear legacy projections: %v", err)
	}

	rows, err := store.ListDashboardJobSummaries(ctx)
	if err != nil {
		t.Fatalf("ListDashboardJobSummaries: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	byID := make(map[string]DashboardJobSummaryRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	current := byID["current"]
	if current.Instructions != "  current title  " || current.Repo != "acme/current" || current.PullRequest != 42 ||
		current.RegisteredRuntime != "codex" || current.EphemeralRuntime != "kimi" || current.Model != "gpt-5.6-sol" || current.InputTokens != 3 || current.OutputTokens != 5 {
		t.Fatalf("current projection = %+v", current)
	}
	inline := byID["ephemeral"]
	if inline.RegisteredRuntime != "" || inline.EphemeralRuntime != "claude" || inline.Model != "claude-fable-5" || inline.Repo != "acme/inline" {
		t.Fatalf("ephemeral projection = %+v", inline)
	}
	legacy := byID["legacy"]
	if legacy.Repo != "acme/legacy" || legacy.PullRequest != 7 || legacy.Instructions != "legacy" || legacy.Model != "" {
		t.Fatalf("legacy projection = %+v", legacy)
	}
	malformed := byID["malformed"]
	if malformed.Instructions != "" || malformed.Repo != "" || malformed.PullRequest != 0 || malformed.EphemeralRuntime != "" || malformed.Model != "" {
		t.Fatalf("malformed projection = %+v, want empty payload fields", malformed)
	}
}

var benchmarkDashboardJobRows any
