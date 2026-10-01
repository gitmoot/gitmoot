package db

import (
	"context"
	"errors"
	"path/filepath"
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
		first.GoalID != "local-review" || first.Title != "Review PR #46" || first.Branch != "feat/x" || first.WorktreePath != "" {
		t.Fatalf("successor = %+v", first)
	}
	again, created, err := store.CreateTaskSuccessor(ctx, "review-pr-46-06cf76fc", testDisposed, "planned", "")
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("repeat: task=%s created=%v err=%v, want the existing successor", again.ID, created, err)
	}
	root, _ := store.GetTask(ctx, "review-pr-46-06cf76fc")
	if root.State != "stranded" || root.Branch != "" {
		t.Fatalf("disposed root = state %q branch %q, want stranded and the branch handed over", root.State, root.Branch)
	}
	if owner, err := store.GetTaskByRepoBranch(ctx, "o/r", "feat/x"); err != nil || owner.ID != first.ID {
		t.Fatalf("branch owner = %s %v, want the successor", owner.ID, err)
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

// Concurrent calls must all get the one successor, not a UNIQUE error. Half
// go through a second Store on the same file, as the CLI and the daemon do
// from separate processes: one Store's single connection already serializes
// its own callers, so only the cross-store case tests the transaction's lock.
func TestConcurrentCreateTaskSuccessorYieldsOneSuccessor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	other, err := OpenAlreadyMigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	if err := store.UpsertTask(ctx, Task{ID: "t", RepoFullName: "o/r", State: "stranded", Branch: "b"}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		id      string
		created bool
		err     error
	}
	results := make(chan result, 8)
	for i := 0; i < 8; i++ {
		caller := store
		if i%2 == 1 {
			caller = other
		}
		go func() {
			task, created, err := caller.CreateTaskSuccessor(ctx, "t", testDisposed, "planned", "")
			results <- result{task.ID, created, err}
		}()
	}
	createdCount := 0
	for i := 0; i < 8; i++ {
		r := <-results
		if r.err != nil || r.id != "t-successor-2" {
			t.Fatalf("concurrent call: %+v", r)
		}
		if r.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("%d calls created a successor, want exactly 1", createdCount)
	}
}

// A task whose id merely ends in -successor-<n> is not part of another chain.
func TestForeignSuccessorLookingIDsAreNotChained(t *testing.T) {
	ctx := context.Background()
	store := openStoreOperationsTestStore(t)
	for _, task := range []Task{
		{ID: "x", RepoFullName: "o/r", State: "planned"},
		{ID: "x-successor-2", RepoFullName: "o/r", State: "stranded"},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if latest, err := store.LatestTaskInChain(ctx, "x-successor-2"); err != nil || latest.ID != "x-successor-2" {
		t.Fatalf("foreign id re-rooted: %s %v", latest.ID, err)
	}
	if latest, err := store.LatestTaskInChain(ctx, "x"); err != nil || latest.ID != "x" {
		t.Fatalf("foreign look-alike joined x's chain: %s %v", latest.ID, err)
	}
	successor, created, err := store.CreateTaskSuccessor(ctx, "x-successor-2", testDisposed, "planned", "")
	if err != nil || !created || successor.ID != "x-successor-2-successor-2" {
		t.Fatalf("successor of a foreign look-alike: %s %v %v", successor.ID, created, err)
	}
}
