package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var testDisposed = []string{"dismissed", "superseded", "stranded"}

func TestCreateTaskSuccessorContinuesADisposedChain(t *testing.T) {
	ctx := context.Background()
	store := openStoreOperationsTestStore(t)
	if err := store.UpsertTask(ctx, Task{ID: "review-pr-46-06cf76fc", RepoFullName: "o/r", GoalID: "local-review", Title: "Review PR #46", State: "reviewing", Branch: "feat/x", WorktreePath: "/gone"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateTaskSuccessor(ctx, "review-pr-46-06cf76fc", testDisposed, "planned", ""); !errors.Is(err, ErrTaskNotDisposed) {
		t.Fatalf("successor for a live task: err=%v, want ErrTaskNotDisposed", err)
	}
	if _, _, err := store.DisposeTask(ctx, "review-pr-46-06cf76fc", []string{"reviewing"}, "stranded", "tier4_stranded", "no disposal evidence", "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	first, created, err := store.CreateTaskSuccessor(ctx, "review-pr-46-06cf76fc", testDisposed, "planned", "owner decision 245002")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if first.ID != "review-pr-46-06cf76fc-successor-2" || first.State != "planned" || first.RepoFullName != "o/r" ||
		first.GoalID != "local-review" || first.Title != "Review PR #46" || first.Branch != "" || first.WorktreePath != "" {
		t.Fatalf("successor = %+v", first)
	}
	again, created, err := store.CreateTaskSuccessor(ctx, "review-pr-46-06cf76fc", testDisposed, "planned", "")
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("repeat: task=%s created=%v err=%v, want the existing successor", again.ID, created, err)
	}
	root, _ := store.GetTask(ctx, "review-pr-46-06cf76fc")
	if root.State != "stranded" {
		t.Fatalf("disposed root moved to %q", root.State)
	}
	rootEvents, _ := store.ListTaskEvents(ctx, root.ID)
	succEvents, _ := store.ListTaskEvents(ctx, first.ID)
	if !hasTaskEvent(rootEvents, TaskEventSuccessorCreated, first.ID) || !hasTaskEvent(succEvents, TaskEventCreatedAsSuccessor, "review-pr-46-06cf76fc on branch feat/x (stranded") {
		t.Fatalf("events root=%+v successor=%+v", rootEvents, succEvents)
	}

	// The successor itself gets disposed: the chain continues from it, by any member's id.
	if _, _, err := store.DisposeTask(ctx, first.ID, []string{"planned"}, "stranded", "tier4_stranded", "again", "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	third, created, err := store.CreateTaskSuccessor(ctx, "review-pr-46-06cf76fc", testDisposed, "planned", "")
	if err != nil || !created || third.ID != "review-pr-46-06cf76fc-successor-3" {
		t.Fatalf("third: %s created=%v err=%v", third.ID, created, err)
	}
	latest, err := store.LatestTaskInChain(ctx, first.ID)
	if err != nil || latest.ID != third.ID {
		t.Fatalf("LatestTaskInChain = %s, %v", latest.ID, err)
	}
}

func hasTaskEvent(events []TaskEvent, kind, reasonFragment string) bool {
	for _, event := range events {
		if event.Kind == kind && strings.Contains(event.Reason, reasonFragment) {
			return true
		}
	}
	return false
}
