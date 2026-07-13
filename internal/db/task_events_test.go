package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTaskEventsMigrationAppliesToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	legacy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`DROP TABLE task_events; DELETE FROM schema_migrations WHERE version = ?`, len(migrations)); err != nil {
		t.Fatalf("rewind newest migration: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open existing db: %v", err)
	}
	defer store.Close()
	if ok, err := store.HasTable(context.Background(), "task_events"); err != nil || !ok {
		t.Fatalf("task_events table ok=%v err=%v", ok, err)
	}
}

func TestTransitionTaskStateWithEventAtomicCASAndOrdering(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, Task{ID: "task-1", State: "implementing"}); err != nil {
		t.Fatal(err)
	}
	changed, current, err := store.TransitionTaskStateWithEvent(ctx, "task-1", []string{"implementing", "blocked"}, "dismissed", "task_dismissed_manual", "done")
	if err != nil || !changed || current != "dismissed" {
		t.Fatalf("first transition = changed %v current %q err %v", changed, current, err)
	}
	changed, current, err = store.TransitionTaskStateWithEvent(ctx, "task-1", []string{"implementing", "blocked"}, "dismissed", "task_dismissed_manual", "again")
	if err != nil || changed || current != "dismissed" {
		t.Fatalf("idempotent transition = changed %v current %q err %v", changed, current, err)
	}
	changed, current, err = store.TransitionTaskStateWithEvent(ctx, "task-1", []string{"blocked"}, "planned", "should_not_exist", "bad cas")
	if err != nil || changed || current != "dismissed" {
		t.Fatalf("failed CAS = changed %v current %q err %v", changed, current, err)
	}
	if err := store.AddTaskEvent(ctx, TaskEvent{TaskID: "task-1", Kind: "note", Reason: "second"}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListTaskEvents(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "task_dismissed_manual" || events[0].FromState != "implementing" || events[0].ToState != "dismissed" || events[1].Kind != "note" || events[0].ID >= events[1].ID {
		t.Fatalf("events = %+v", events)
	}
}
