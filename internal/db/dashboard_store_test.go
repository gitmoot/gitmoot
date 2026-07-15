package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
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

func TestListDashboardJobSummariesProjectsLegacyAndMalformedPayloads(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, Agent{Name: "registered", Runtime: "codex"}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	jobs := []Job{
		{
			ID: "current", Agent: "registered", Type: "ask", State: "running",
			Payload:     `{"instructions":"  current title  ","repo":"acme/current","pull_request":42,"ephemeral":{"runtime":"kimi"}}`,
			InputTokens: 3, OutputTokens: 5,
		},
		{
			ID: "ephemeral", Agent: "temp-worker", Type: "implement", State: "queued",
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
		current.RegisteredRuntime != "codex" || current.EphemeralRuntime != "kimi" || current.InputTokens != 3 || current.OutputTokens != 5 {
		t.Fatalf("current projection = %+v", current)
	}
	inline := byID["ephemeral"]
	if inline.RegisteredRuntime != "" || inline.EphemeralRuntime != "claude" || inline.Repo != "acme/inline" {
		t.Fatalf("ephemeral projection = %+v", inline)
	}
	legacy := byID["legacy"]
	if legacy.Repo != "acme/legacy" || legacy.PullRequest != 7 || legacy.Instructions != "legacy" {
		t.Fatalf("legacy projection = %+v", legacy)
	}
	malformed := byID["malformed"]
	if malformed.Instructions != "" || malformed.Repo != "" || malformed.PullRequest != 0 || malformed.EphemeralRuntime != "" {
		t.Fatalf("malformed projection = %+v, want empty payload fields", malformed)
	}
}

var benchmarkDashboardJobRows any

// BenchmarkListDashboardJobSummaries compares the guarded SQL projection with
// the alternative one-pass partial Go decoder on the issue's measured 2,160 x
// ~28KB payload distribution. The SQL path avoids transferring the unused bytes.
func BenchmarkListDashboardJobSummaries(b *testing.B) {
	store, err := Open(b.TempDir() + "/gitmoot.db")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	filler := strings.Repeat("x", 28*1024)
	for i := 0; i < 2160; i++ {
		payload := fmt.Sprintf(`{"instructions":"job %d","repo":"acme/repo","pull_request":%d,"ephemeral":{"runtime":"codex"},"unused":"%s"}`, i, i%100, filler)
		if err := store.CreateJob(ctx, Job{ID: fmt.Sprintf("job-%04d", i), Agent: "worker", Type: "ask", State: "succeeded", Payload: payload}); err != nil {
			b.Fatal(err)
		}
	}

	b.Run("sql-projection", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			rows, err := store.ListDashboardJobSummaries(ctx)
			if err != nil {
				b.Fatal(err)
			}
			benchmarkDashboardJobRows = rows
		}
	})
	b.Run("full-payload-one-pass", func(b *testing.B) {
		type partialPayload struct {
			Instructions string `json:"instructions"`
			Repo         string `json:"repo"`
			PullRequest  int    `json:"pull_request"`
			Ephemeral    struct {
				Runtime string `json:"runtime"`
			} `json:"ephemeral"`
		}
		for i := 0; i < b.N; i++ {
			jobs, err := store.ListJobs(ctx)
			if err != nil {
				b.Fatal(err)
			}
			projected := make([]partialPayload, 0, len(jobs))
			for _, job := range jobs {
				var payload partialPayload
				_ = json.Unmarshal([]byte(job.Payload), &payload)
				projected = append(projected, payload)
			}
			benchmarkDashboardJobRows = projected
		}
	})
}
