package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUpsertLatestJobEventReplacesSingleRow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateJob(ctx, Job{ID: "job-progress", Agent: "worker", Type: "ask", State: "running"}); err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{`{"elapsed":"1m0s","activity":"one"}`, `{"elapsed":"1m30s","activity":"two"}`} {
		if err := store.UpsertLatestJobEvent(ctx, JobEvent{JobID: "job-progress", Kind: "progress", Message: message}); err != nil {
			t.Fatalf("UpsertLatestJobEvent: %v", err)
		}
	}
	events, err := store.ListJobEvents(ctx, "job-progress")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Message != `{"elapsed":"1m30s","activity":"two"}` {
		t.Fatalf("events = %+v, want one replaced progress row", events)
	}
}

func TestUpsertLatestJobEventTerminalGuard(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateJob(ctx, Job{ID: "job-terminal", Agent: "worker", Type: "ask", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertLatestJobEvent(ctx, JobEvent{JobID: "job-terminal", Kind: "progress", Message: "before"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.TransitionJobState(ctx, "job-terminal", "running", "succeeded"); err != nil || !ok {
		t.Fatalf("terminal transition: ok=%v err=%v", ok, err)
	}
	if err := store.UpsertLatestJobEvent(ctx, JobEvent{JobID: "job-terminal", Kind: "progress", Message: "late"}); err != nil {
		t.Fatal(err)
	}
	event, ok, err := store.GetLatestJobEventByKind(ctx, "job-terminal", "progress")
	if err != nil || !ok || event.Message != "before" {
		t.Fatalf("late terminal upsert changed event: ok=%v event=%+v err=%v", ok, event, err)
	}

	if err := store.CreateJob(ctx, Job{ID: "job-already-terminal", Agent: "worker", Type: "ask", State: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertLatestJobEvent(ctx, JobEvent{JobID: "job-already-terminal", Kind: "progress", Message: "late"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.GetLatestJobEventByKind(ctx, "job-already-terminal", "progress"); err != nil || ok {
		t.Fatalf("terminal insert guard: ok=%v err=%v", ok, err)
	}
}

func TestGetLatestJobEventByKindAndBulk(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []string{"job-a", "job-b", "job-c"} {
		if err := store.CreateJob(ctx, Job{ID: id, Agent: "worker", Type: "ask", State: "running"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddJobEvent(ctx, JobEvent{JobID: "job-a", Kind: "queued", Message: "ignore"}); err != nil {
		t.Fatal(err)
	}
	for id, message := range map[string]string{"job-a": "a", "job-b": "b", "job-c": "not-requested"} {
		if err := store.UpsertLatestJobEvent(ctx, JobEvent{JobID: id, Kind: "progress", Message: message}); err != nil {
			t.Fatal(err)
		}
	}

	event, ok, err := store.GetLatestJobEventByKind(ctx, "job-a", "progress")
	if err != nil || !ok || event.Message != "a" || event.CreatedAt == "" {
		t.Fatalf("by-kind event: ok=%v event=%+v err=%v", ok, event, err)
	}
	if _, ok, err := store.GetLatestJobEventByKind(ctx, "job-a", "missing"); err != nil || ok {
		t.Fatalf("missing by-kind: ok=%v err=%v", ok, err)
	}

	bulk, err := store.GetLatestJobEventsByKind(ctx, []string{"job-a", "job-b"}, "progress")
	if err != nil {
		t.Fatal(err)
	}
	if len(bulk) != 2 || bulk["job-a"].Message != "a" || bulk["job-b"].Message != "b" {
		t.Fatalf("bulk = %+v", bulk)
	}
	empty, err := store.GetLatestJobEventsByKind(ctx, nil, "progress")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty bulk = %+v err=%v", empty, err)
	}
}
