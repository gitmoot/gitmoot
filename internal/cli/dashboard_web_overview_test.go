package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestWebDataSourceOverviewAndTasksEmpty(t *testing.T) {
	ds := &webDataSource{home: dashboardTestHome(t)}
	overview, err := ds.Overview(context.Background())
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if overview.NeedsYou == nil || overview.Activity.Workflows == nil || overview.Today.Notable == nil || overview.Scheduled == nil || overview.Fleet == nil {
		t.Fatalf("Overview contains nil collection: %+v", overview)
	}
	if len(overview.NeedsYou)+len(overview.Activity.Workflows)+len(overview.Today.Notable)+len(overview.Scheduled)+len(overview.Fleet) != 0 {
		t.Fatalf("Overview empty store = %+v", overview)
	}
	tasks, err := ds.Tasks(context.Background())
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if tasks == nil || len(tasks) != 0 {
		t.Fatalf("Tasks empty store = %#v, want non-nil empty", tasks)
	}
}

func TestDashboardOverviewTasksAndAutoWorkflows(t *testing.T) {
	home := dashboardTestHome(t)
	paths := config.PathsForHome(home)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	now := time.Date(2026, 7, 13, 12, 30, 0, 0, time.UTC)
	stamp := func(age time.Duration) string { return dashboardSQLiteTime(now.Add(-age)) }

	for _, agent := range []db.Agent{{Name: "alpha", Runtime: "codex"}, {Name: "beta", Runtime: "claude"}} {
		if err := store.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent(%s): %v", agent.Name, err)
		}
	}
	for _, task := range []db.Task{
		{ID: "task-plan", RepoFullName: "acme/app", Title: "Plan rollout", State: "planned", Branch: "plan"},
		{ID: "task-unknown", RepoFullName: "acme/app", Title: "Future lifecycle", State: "future_state", Branch: "future"},
		{ID: "task-pr", RepoFullName: "acme/app", Title: "Ship dashboard", State: "ready_to_merge", Branch: "ship-dashboard"},
		{ID: "task-blocked", RepoFullName: "acme/ops", Title: "Rotate key", State: "awaiting_human", Branch: "rotate"},
		{ID: "task-merged-recent", RepoFullName: "acme/app", Title: "Recent merge", State: "merged", Branch: "recent"},
		{ID: "task-merged-old", RepoFullName: "acme/app", Title: "Old merge", State: "merged", Branch: "old"},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatalf("UpsertTask(%s): %v", task.ID, err)
		}
	}
	if _, err := store.CreateLock(ctx, db.BranchLock{RepoFullName: "acme/app", Branch: "ship-dashboard", Owner: "alpha"}); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: "acme/app", Number: 42, URL: "https://github.com/acme/app/pull/42", HeadBranch: "ship-dashboard", BaseBranch: "main", State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}

	seedJob := func(id, agent, state, label, repo, title string, createdAge, updatedAge time.Duration, in, out int) {
		t.Helper()
		payload := workflow.JobPayload{WorkflowID: label, Repo: repo, TaskTitle: title}
		if err := store.CreateJob(ctx, db.Job{ID: id, Agent: agent, Type: "ask", State: state, Payload: mustJSON(t, payload)}); err != nil {
			t.Fatalf("CreateJob(%s): %v", id, err)
		}
		if err := store.UpdateJobUsage(ctx, id, in, out); err != nil {
			t.Fatalf("UpdateJobUsage(%s): %v", id, err)
		}
		setJobTimes(t, home, id, stamp(createdAge), stamp(updatedAge))
	}
	seedJob("live-running", "alpha", "running", "fable/redesign", "acme/app", "Live implementation", 2*time.Hour, 5*time.Minute, 1, 2)
	seedJob("live-queued", "beta", "queued", "fable/redesign", "acme/app", "Queued review", time.Hour, 10*time.Minute, 0, 0)
	seedJob("unlabelled-running", "beta", "running", "", "acme/misc", "Unattended work", 45*time.Minute, 5*time.Minute, 3, 5)
	seedJob("stalled-failed", "alpha", "failed", "ops/stalled", "acme/ops", "Deploy failed", 90*time.Minute, time.Hour, 7, 11)
	seedJob("finished-ok", "alpha", "succeeded", "release/done", "acme/app", "Release complete", 3*time.Hour, 2*time.Hour, 13, 17)
	seedJob("finished-cancelled", "beta", "cancelled", "release/cancelled", "acme/app", "Cancelled experiment", 4*time.Hour, 3*time.Hour, 19, 23)
	seedJob("finished-old", "alpha", "succeeded", "archive/old", "acme/app", "Old completion", 26*time.Hour, 25*time.Hour, 101, 103)
	seedJob("pipeline-job", "alpha", "succeeded", "", "acme/data", "Nightly ingest", 2*time.Hour, 90*time.Minute, 29, 31)

	blockedPayload := workflow.JobPayload{WorkflowID: "ops/recent", Repo: "acme/ops", TaskTitle: "Provision credentials"}
	if err := store.CreateJobWithEvent(ctx,
		db.Job{ID: "blocked-job", Agent: "beta", Type: "implement", State: "blocked", Payload: mustJSON(t, blockedPayload)},
		db.JobEvent{Kind: "blocked", Message: "waiting on deployment credentials"}); err != nil {
		t.Fatalf("Create blocked job: %v", err)
	}
	setJobTimes(t, home, "blocked-job", stamp(20*time.Minute), stamp(10*time.Minute))

	note, err := store.InsertWorkflowNoteWithMeta(ctx,
		db.WorkflowNote{WorkflowID: "ops/stalled", Author: "lead", Body: "last coordinator handoff"},
		db.WorkflowMeta{Author: "lead", Pane: "ops-pane", SessionID: "ops-session", WorkDir: "/work/ops"})
	if err != nil {
		t.Fatalf("InsertWorkflowNoteWithMeta: %v", err)
	}

	seedTestPipeline(t, store, db.Pipeline{
		Name: "nightly", Repo: "acme/data", SpecYAML: diamondSpecYAML, Enabled: true, Interval: "6h", Jitter: "30m",
	})
	if err := store.UpdatePipelineScheduleState(ctx, db.PipelineScheduleState{
		Name: "nightly", LastRunAt: now.Add(-time.Hour), NextDueAt: now.Add(5*time.Hour + 30*time.Minute),
		LastRunID: "pipeline-run", LastStatus: pipeline.RunSucceeded,
	}); err != nil {
		t.Fatalf("UpdatePipelineScheduleState: %v", err)
	}
	seedTestRun(t, store, db.PipelineRun{ID: "pipeline-run", Pipeline: "nightly", State: pipeline.RunSucceeded, StartedAt: now.Add(-2 * time.Hour), FinishedAt: now.Add(-90 * time.Minute)}, []db.PipelineRunStage{
		{StageID: "ingest", State: pipeline.StageSucceeded, JobID: "pipeline-job"},
	})
	store.Close()

	raw, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	for id, updated := range map[string]string{
		"task-plan":          stamp(2 * time.Hour),
		"task-unknown":       stamp(15 * time.Minute),
		"task-pr":            stamp(time.Hour),
		"task-blocked":       stamp(30 * time.Minute),
		"task-merged-recent": stamp(6 * 24 * time.Hour),
		"task-merged-old":    stamp(8 * 24 * time.Hour),
	} {
		if _, err := raw.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ?`, updated, id); err != nil {
			t.Fatalf("update task %s: %v", id, err)
		}
	}
	if _, err := raw.Exec(`UPDATE workflow_notes SET created_at = ? WHERE id = ?`, stamp(2*time.Hour), note.ID); err != nil {
		t.Fatalf("update workflow note: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw DB: %v", err)
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	tasks, err := dashboardTasks(ctx, store, now)
	if err != nil {
		t.Fatalf("dashboardTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("tasks len=%d want 3 (planned, unknown, old merged excluded): %+v", len(tasks), tasks)
	}
	if tasks[0].ID != "task-pr" || tasks[0].State != "pr_open" || tasks[0].Agent != "alpha" || tasks[0].PRNumber != 42 || tasks[1].ID != "task-blocked" || tasks[1].BlockedReason != "Awaiting human input" || tasks[2].ID != "task-merged-recent" {
		t.Fatalf("task projection/order = %+v", tasks)
	}

	overview, err := dashboardOverview(ctx, store, now)
	if err != nil {
		t.Fatalf("dashboardOverview: %v", err)
	}
	if len(overview.NeedsYou) != 3 || overview.NeedsYou[0].Kind != "stalled_workflow" || overview.NeedsYou[0].Label != "ops/stalled" || overview.NeedsYou[0].Pane != "ops-pane" || overview.NeedsYou[1].Kind != "pr_awaiting_merge" || overview.NeedsYou[1].Ref != "#42" || overview.NeedsYou[2].Kind != "blocked_job" || overview.NeedsYou[2].Title != "waiting on deployment credentials" {
		t.Fatalf("needs_you = %+v", overview.NeedsYou)
	}
	if len(overview.Activity.Workflows) != 1 || overview.Activity.Workflows[0].Label != "fable/redesign" || overview.Activity.Workflows[0].Running != 1 || !reflect.DeepEqual(overview.Activity.Workflows[0].Agents, []string{"alpha", "beta"}) || overview.Activity.Queued != 1 || overview.Activity.UnattendedNote != "1 active job without a workflow label" {
		t.Fatalf("activity = %+v", overview.Activity)
	}
	if overview.Today.Completed != 2 || overview.Today.Failed != 1 || overview.Today.Cancelled != 1 || overview.Today.TokensIn != 68 || overview.Today.TokensOut != 82 || len(overview.Today.Notable) != 4 {
		t.Fatalf("today = %+v", overview.Today)
	}
	hourTotal := 0
	for _, count := range overview.Today.PerHour {
		hourTotal += count
	}
	if hourTotal != 4 {
		t.Fatalf("per_hour total=%d want 4: %v", hourTotal, overview.Today.PerHour)
	}
	if len(overview.Scheduled) != 1 || overview.Scheduled[0].Name != "nightly" || overview.Scheduled[0].Schedule != "every 6h +30m" || overview.Scheduled[0].LastStatus != pipeline.RunSucceeded || overview.Scheduled[0].NextInS != 5*60*60 {
		t.Fatalf("scheduled = %+v", overview.Scheduled)
	}
	if len(overview.Fleet) != 2 || !overview.Fleet[0].Running || !overview.Fleet[1].Running {
		t.Fatalf("fleet = %+v", overview.Fleet)
	}

	workflows, err := dashboardWorkflowEntries(ctx, store, now)
	if err != nil {
		t.Fatalf("dashboardWorkflowEntries: %v", err)
	}
	pipelineGroup := workflowIndexEntryByLabel(t, workflows, "pipeline/nightly")
	if !pipelineGroup.Auto || pipelineGroup.Counts.Jobs != 1 || pipelineGroup.Counts.Succeeded != 1 || !reflect.DeepEqual(pipelineGroup.Repos, []string{"acme/data"}) {
		t.Fatalf("pipeline auto group = %+v", pipelineGroup)
	}
	adhocGroup := workflowIndexEntryByLabel(t, workflows, "adhoc/beta")
	if !adhocGroup.Auto || adhocGroup.State != "active" || adhocGroup.Counts.Running != 1 {
		t.Fatalf("adhoc auto group = %+v", adhocGroup)
	}

	second, err := dashboardOverview(ctx, store, now)
	if err != nil {
		t.Fatalf("second dashboardOverview: %v", err)
	}
	firstJSON, _ := json.Marshal(overview)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("overview not deterministic\nfirst  %s\nsecond %s", firstJSON, secondJSON)
	}
}

func TestWebDashboardTasksRoundTripAndMergedWindow(t *testing.T) {
	home := dashboardTestHome(t)
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, task := range []db.Task{
		{ID: "task-1", RepoFullName: "acme/app", Title: "Implement", State: "implementing"},
		{ID: "task-planned", RepoFullName: "acme/app", Title: "Plan", State: "planned"},
		{ID: "task-unknown", RepoFullName: "acme/app", Title: "Future", State: "future_state"},
	} {
		if err := store.UpsertTask(context.Background(), task); err != nil {
			t.Fatalf("UpsertTask(%s): %v", task.ID, err)
		}
	}
	store.Close()

	recorder := httptest.NewRecorder()
	dashboard.Serve(&webDataSource{home: home}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/tasks status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var tasks []dashboard.TaskSummary
	if err := json.Unmarshal(recorder.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "task-1" || tasks[0].State != "implementing" || tasks[0].UpdatedAt == 0 {
		t.Fatalf("tasks round trip = %+v", tasks)
	}
}

func TestDashboardTaskStateFiltersUnsupportedStates(t *testing.T) {
	tests := []struct {
		internal, state, reason string
		ok                      bool
	}{
		{internal: "planned"},
		{internal: "future_state"},
		{internal: "implementing", state: "implementing", ok: true},
		{internal: "pr_open", state: "pr_open", ok: true},
		{internal: "reviewing", state: "pr_open", ok: true},
		{internal: "changes_requested", state: "pr_open", ok: true},
		{internal: "ready_to_merge", state: "pr_open", ok: true},
		{internal: "blocked", state: "blocked", reason: "Task is blocked", ok: true},
		{internal: "awaiting_human", state: "blocked", reason: "Awaiting human input", ok: true},
		{internal: "merged", state: "merged", ok: true},
	}
	for _, test := range tests {
		t.Run(test.internal, func(t *testing.T) {
			state, reason, ok := dashboardTaskState(test.internal)
			if state != test.state || reason != test.reason || ok != test.ok {
				t.Fatalf("dashboardTaskState(%q) = (%q, %q, %v), want (%q, %q, %v)", test.internal, state, reason, ok, test.state, test.reason, test.ok)
			}
		})
	}
}
