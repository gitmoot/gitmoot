package workflow

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestTaskWorktreePath(t *testing.T) {
	path, err := TaskWorktreePath("/home/gitmoot", "owner/repo", "task-1")
	if err != nil {
		t.Fatalf("TaskWorktreePath returned error: %v", err)
	}
	want := filepath.Join("/home/gitmoot", "worktrees", "owner--repo", "task-1")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	for _, tc := range []struct {
		name string
		repo string
		task string
	}{
		{name: "empty repo", repo: "", task: "task-1"},
		{name: "nested repo", repo: "owner/repo/extra", task: "task-1"},
		{name: "unsafe task", repo: "owner/repo", task: "../task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TaskWorktreePath("/home/gitmoot", tc.repo, tc.task); err == nil {
				t.Fatal("TaskWorktreePath accepted invalid input")
			}
		})
	}
}

func TestEngineAllocateTaskWorktreeAddsGitWorktreeAndStoresPath(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "First", State: string(TaskPlanned)}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	home := t.TempDir()
	checkout := t.TempDir()
	key, err := checkoutMutationLockKey(checkout)
	if err != nil {
		t.Fatalf("checkoutMutationLockKey returned error: %v", err)
	}
	manager := &fakeWorktreeManager{onAdd: func() {
		lock, err := store.GetResourceLock(ctx, key)
		if err != nil {
			t.Fatalf("GetResourceLock during AddWorktree returned error: %v", err)
		}
		if lock.OwnerJobID != "worktree:task-1" {
			t.Fatalf("checkout lock owner = %q, want worktree:task-1", lock.OwnerJobID)
		}
	}}

	task, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:       home,
		Repo:       "owner/repo",
		TaskID:     "task-1",
		Branch:     "task-1",
		BaseBranch: "main",
		Checkout:   checkout,
	}, manager)

	if err != nil {
		t.Fatalf("AllocateTaskWorktree returned error: %v", err)
	}
	wantPath := filepath.Join(home, "worktrees", "owner--repo", "task-1")
	if task.WorktreePath != wantPath || task.Branch != "task-1" {
		t.Fatalf("task = %+v, want worktree path %q and branch task-1", task, wantPath)
	}
	if len(manager.calls) != 1 || manager.calls[0].branch != "task-1" || manager.calls[0].path != wantPath || manager.calls[0].base != "main" {
		t.Fatalf("worktree calls = %+v", manager.calls)
	}
	if _, err := store.GetResourceLock(ctx, key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("checkout lock after AddWorktree error = %v, want sql.ErrNoRows", err)
	}
	reloaded, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if reloaded.WorktreePath != wantPath {
		t.Fatalf("reloaded worktree path = %q, want %q", reloaded.WorktreePath, wantPath)
	}
}

func TestEngineAllocateTaskWorktreeBlocksWhenCheckoutMutationLocked(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	home := t.TempDir()
	checkout := t.TempDir()
	key, err := checkoutMutationLockKey(checkout)
	if err != nil {
		t.Fatalf("checkoutMutationLockKey returned error: %v", err)
	}
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  "task:other",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	manager := &fakeWorktreeManager{}

	_, err = engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:     home,
		Repo:     "owner/repo",
		TaskID:   "task-1",
		Branch:   "task-1",
		Checkout: checkout,
	}, manager)

	var blocked BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != checkoutMutationBusyMessage {
		t.Fatalf("error = %v, want checkout busy BlockedError", err)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("AddWorktree ran despite checkout lock: %+v", manager.calls)
	}
}

func TestEngineAllocateTaskWorktreeRejectsBranchAssignedToOtherTask(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-existing", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Existing", State: string(TaskPlanned), Branch: "task-1"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	manager := &fakeWorktreeManager{}

	_, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:     t.TempDir(),
		Repo:     "owner/repo",
		TaskID:   "task-2",
		Branch:   "task-1",
		Checkout: t.TempDir(),
	}, manager)

	if err == nil || !strings.Contains(err.Error(), "another task") {
		t.Fatalf("error = %v, want branch assignment error", err)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("AddWorktree ran despite assignment conflict: %+v", manager.calls)
	}
}

func TestEngineAllocateTaskWorktreeRejectsTaskInAnotherRepo(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/other", GoalID: "goal-1", Title: "First", State: string(TaskPlanned)}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	manager := &fakeWorktreeManager{}

	_, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:     t.TempDir(),
		Repo:     "owner/repo",
		TaskID:   "task-1",
		Branch:   "task-1",
		Checkout: t.TempDir(),
	}, manager)

	if err == nil || !strings.Contains(err.Error(), "belongs to repo owner/other") {
		t.Fatalf("error = %v, want repo mismatch error", err)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("AddWorktree ran despite repo mismatch: %+v", manager.calls)
	}
}

type fakeWorktreeManager struct {
	err   error
	onAdd func()
	calls []worktreeCall
}

type worktreeCall struct {
	branch string
	path   string
	base   string
}

func (f *fakeWorktreeManager) AddWorktree(_ context.Context, branch string, path string, base string) error {
	if f.onAdd != nil {
		f.onAdd()
	}
	f.calls = append(f.calls, worktreeCall{branch: branch, path: path, base: base})
	return f.err
}
