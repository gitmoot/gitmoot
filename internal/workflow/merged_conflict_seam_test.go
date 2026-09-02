package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// mergeInsideTheSeam lands a merge in the exact post-read/pre-write window the
// guarded block write is classified from. Nothing else in the tree can do this: the
// pre-read is the value the guard proves stale, so the merge must arrive between
// them or the test is only re-checking the already-merged case.
func mergeInsideTheSeam(t *testing.T, store *db.Store) {
	t.Helper()
	fired := false
	blockTaskPreWriteHook = func(ctx context.Context, taskID string) {
		if fired {
			return
		}
		fired = true
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Errorf("GetTask inside the seam: %v", err)
			return
		}
		task.State = string(TaskMerged)
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Errorf("merge inside the seam: %v", err)
		}
	}
	t.Cleanup(func() { blockTaskPreWriteHook = nil })
}

// TestBlockTaskClassifiesAMergeThatLandsInTheSeam is the P2 regression. The task is
// NOT merged when blockTask reads it, so classifying from the pre-read yields a hard
// poll error and strands the coordinator the failure policy has to release. The
// classification must read the WINNING state.
//
// MUTATION PROOF: classify from fromState instead of the post-conflict read and
// blockTask returns a wrapped store error rather than BlockedError.
func TestBlockTaskClassifiesAMergeThatLandsInTheSeam(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-seam", RepoFullName: "gitmoot/gitmoot", Branch: "task-seam", GoalID: "g1",
		Title: "impl", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	mergeInsideTheSeam(t, store)

	err := engine.blockTask(ctx, taskRef{ID: "task-seam", Repo: "gitmoot/gitmoot", Branch: "task-seam"}, "workflow_blocked", "dead leg", "workflow")
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("blockTask error = %v, want BlockedError: a merge in the seam hard-failed the poll", err)
	}
	task, err := store.GetTask(ctx, "task-seam")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(TaskMerged) {
		t.Fatalf("task state = %q, want the landed-work record kept", task.State)
	}
	refusals := 0
	events, err := store.ListTaskEvents(ctx, "task-seam")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == TaskEventMergedRegressionRefused {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("%s events = %d, want 1: the refusal left no durable trace", TaskEventMergedRegressionRefused, refusals)
	}
}

// TestBlockTaskStillHardErrorsOnANonMergedWinner is the control that keeps the fix
// from becoming "swallow every conflict". A winner that is not merged is a genuine
// incompatibility and must still fail loudly.
func TestBlockTaskStillHardErrorsOnANonMergedWinner(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-seam-other", RepoFullName: "gitmoot/gitmoot", Branch: "task-seam-other", GoalID: "g1",
		Title: "impl", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	fired := false
	blockTaskPreWriteHook = func(hookCtx context.Context, taskID string) {
		if fired {
			return
		}
		fired = true
		task, err := store.GetTask(hookCtx, taskID)
		if err != nil {
			t.Errorf("GetTask inside the seam: %v", err)
			return
		}
		task.State = string(TaskDismissed)
		if err := store.UpsertTask(hookCtx, task); err != nil {
			t.Errorf("dispose inside the seam: %v", err)
		}
	}
	t.Cleanup(func() { blockTaskPreWriteHook = nil })

	err := engine.blockTask(ctx, taskRef{ID: "task-seam-other", Repo: "gitmoot/gitmoot", Branch: "task-seam-other"}, "workflow_blocked", "dead leg", "workflow")
	var blocked BlockedError
	if errors.As(err, &blocked) {
		t.Fatalf("blockTask error = %v: a non-merged winner was folded into the landed-work refusal", err)
	}
	if err == nil {
		t.Fatal("blockTask returned nil for a refused write over a dismissed task")
	}
	events, err := store.ListTaskEvents(ctx, "task-seam-other")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == TaskEventMergedRegressionRefused {
			t.Fatal("a landed-work refusal was recorded for a task that never merged")
		}
	}
}

// TestBlockTaskSucceedsWithoutAnInterleave is the success-path control: the seam hook
// is what makes the other two tests interesting, and without it the ordinary block
// must still commit.
func TestBlockTaskSucceedsWithoutAnInterleave(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-plain", RepoFullName: "gitmoot/gitmoot", Branch: "task-plain", GoalID: "g1",
		Title: "impl", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	err := engine.blockTask(ctx, taskRef{ID: "task-plain", Repo: "gitmoot/gitmoot", Branch: "task-plain"}, "workflow_blocked", "dead leg", "workflow")
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("blockTask error = %v, want BlockedError", err)
	}
	task, err := store.GetTask(ctx, "task-plain")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
}

// TestDirtyWorktreeBlockClassifiesAMergeThatLandsInTheSeam is the same class on the
// allocation path, which carries its own copy of the classification.
//
// MUTATION PROOF: classify from fromState in blockTaskForDirtyWorktree and the
// allocation returns a raw store error instead of a BlockedError.
func TestDirtyWorktreeBlockClassifiesAMergeThatLandsInTheSeam(t *testing.T) {
	ctx, store, engine, manager, request, _, _, _ := setupOffLineageTaskWorktree(t, true)
	mergeInsideTheSeam(t, store)

	_, err := engine.AllocateTaskWorktree(ctx, request, manager)
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("AllocateTaskWorktree error = %v, want BlockedError: a merge in the seam hard-failed allocation", err)
	}
	task, err := store.GetTask(ctx, request.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(TaskMerged) {
		t.Fatalf("task state = %q, want the landed-work record kept", task.State)
	}
	events, err := store.ListTaskEvents(ctx, request.TaskID)
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	refusals := 0
	for _, event := range events {
		if event.Kind == TaskEventMergedRegressionRefused {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("%s events = %d, want 1", TaskEventMergedRegressionRefused, refusals)
	}
}
