package db

import (
	"context"
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
