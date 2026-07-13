package cli

import (
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestWebDataSourceChangeCursor(t *testing.T) {
	home := dashboardTestHome(t)
	ds := &webDataSource{home: home}
	ctx := context.Background()

	assertCursor := func(want string) {
		t.Helper()
		got, err := ds.ChangeCursor(ctx)
		if err != nil {
			t.Fatalf("ChangeCursor: %v", err)
		}
		if got != want {
			t.Fatalf("ChangeCursor = %q, want %q", got, want)
		}
	}
	assertCursor("0.0.0")
	assertCursor("0.0.0")

	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx,
		db.Job{ID: "job-1", Agent: "worker", Type: "ask", State: "queued"},
		db.JobEvent{Kind: "queued", Message: "created"}); err != nil {
		store.Close()
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	store.Close()
	assertCursor("1.0.0")

	store, err = db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{WorkflowID: "release/one", Body: "checkpoint"}); err != nil {
		store.Close()
		t.Fatalf("InsertWorkflowNote: %v", err)
	}
	store.Close()
	assertCursor("1.1.0")

	store, err = db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("reopen for task event: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", State: "implementing"}); err != nil {
		store.Close()
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{TaskID: "task-1", Kind: "task_dismissed_manual"}); err != nil {
		store.Close()
		t.Fatalf("AddTaskEvent: %v", err)
	}
	store.Close()
	assertCursor("1.1.1")
	assertCursor("1.1.1")
}
