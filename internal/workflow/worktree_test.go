package workflow

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TaskWorktreePath("/home/gitmoot", tc.repo, tc.task); err == nil {
				t.Fatal("TaskWorktreePath accepted invalid input")
			}
		})
	}

	// A task id that is not a plain path segment (e.g. "../task") is now sanitized
	// into a safe, traversal-safe segment rather than rejected.
	tp, err := TaskWorktreePath("/home/gitmoot", "owner/repo", "../task")
	if err != nil {
		t.Fatalf("TaskWorktreePath should sanitize unsafe task id, got error: %v", err)
	}
	troot := filepath.Join("/home/gitmoot", "worktrees", "owner--repo")
	if rel, err := filepath.Rel(troot, tp); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("sanitized task path %q escaped the worktrees root (rel=%q err=%v)", tp, rel, err)
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
		GoalID:     "goal-fallback",
		TaskID:     "task-1",
		TaskTitle:  "Fallback",
		Branch:     "task-1",
		BaseBranch: "main",
		Owner:      "lead",
		Checkout:   checkout,
	}, manager)

	if err != nil {
		t.Fatalf("AllocateTaskWorktree returned error: %v", err)
	}
	wantPath := filepath.Join(home, "worktrees", "owner--repo", "task-1")
	if task.WorktreePath != wantPath || task.Branch != "task-1" {
		t.Fatalf("task = %+v, want worktree path %q and branch task-1", task, wantPath)
	}
	if task.State != string(TaskImplementing) || task.GoalID != "goal-1" || task.Title != "First" {
		t.Fatalf("task metadata = %+v", task)
	}
	lock, err := store.GetBranchLock(ctx, "owner/repo", "task-1")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.Owner != "lead" {
		t.Fatalf("lock owner = %q, want lead", lock.Owner)
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

func TestEngineAllocateTaskWorktreeRejectsDismissedTask(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-dismissed", RepoFullName: "owner/repo", State: string(TaskDismissed), Branch: "feature/dismissed", WorktreePath: "/tmp/preserved"}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeWorktreeManager{}
	_, err := testEngine(store).AllocateTaskWorktree(ctx, TaskWorktreeRequest{Home: t.TempDir(), Repo: "owner/repo", TaskID: "task-dismissed", Branch: "feature/dismissed", Owner: "lead"}, manager)
	if err == nil || !strings.Contains(err.Error(), "dismissed") {
		t.Fatalf("AllocateTaskWorktree error = %v", err)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("manager calls = %+v", manager.calls)
	}
}

func TestEngineAllocateTaskWorktreeRejectsEvidenceDisposedTask(t *testing.T) {
	for _, terminal := range []TaskState{TaskSuperseded, TaskStranded} {
		t.Run(string(terminal), func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			branch := "feature/" + string(terminal)
			if err := store.UpsertTask(ctx, db.Task{ID: "task-terminal", RepoFullName: "owner/repo", State: string(terminal), Branch: branch, WorktreePath: "/tmp/preserved"}); err != nil {
				t.Fatal(err)
			}
			manager := &fakeWorktreeManager{}
			_, err := testEngine(store).AllocateTaskWorktree(ctx, TaskWorktreeRequest{Home: t.TempDir(), Repo: "owner/repo", TaskID: "task-terminal", Branch: branch, Owner: "lead"}, manager)
			if err == nil || !strings.Contains(err.Error(), string(terminal)) {
				t.Fatalf("AllocateTaskWorktree error = %v", err)
			}
			if len(manager.calls) != 0 {
				t.Fatalf("manager calls = %+v", manager.calls)
			}
		})
	}
}

func TestEngineAllocateTaskWorktreeRejectsAwaitingHumanMergeTask(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-awaiting-human-merge", RepoFullName: "owner/repo", State: string(TaskAwaitingHumanMerge), Branch: "feature/awaiting-human-merge", WorktreePath: "/tmp/preserved"}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeWorktreeManager{}
	_, err := testEngine(store).AllocateTaskWorktree(ctx, TaskWorktreeRequest{Home: t.TempDir(), Repo: "owner/repo", TaskID: "task-awaiting-human-merge", Branch: "feature/awaiting-human-merge", Owner: "lead"}, manager)
	if err == nil || !strings.Contains(err.Error(), "awaiting a human merge decision") {
		t.Fatalf("AllocateTaskWorktree error = %v", err)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("manager calls = %+v", manager.calls)
	}
	task, err := store.GetTask(ctx, "task-awaiting-human-merge")
	if err != nil || task.State != string(TaskAwaitingHumanMerge) {
		t.Fatalf("task after refusal = %+v, err=%v", task, err)
	}
}

func TestEngineAllocateTaskWorktreeDoesNotResurrectConcurrentPlannedTTLDismissal(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-race", RepoFullName: "owner/repo", State: string(TaskPlanned)}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeWorktreeManager{onAdd: func() {
		changed, _, err := store.TransitionTaskStateWithEventIfNoActiveJob(ctx, "task-race",
			[]string{string(TaskPlanned)}, string(TaskDismissed), "task_dismissed_planned_ttl", "test interleave")
		if err != nil || !changed {
			t.Fatalf("planned_ttl interleave changed=%v err=%v", changed, err)
		}
	}}
	_, err := testEngine(store).AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home: t.TempDir(), Repo: "owner/repo", TaskID: "task-race", Branch: "feature/race", Owner: "lead", Checkout: t.TempDir(),
	}, manager)
	if err == nil || !strings.Contains(err.Error(), "was dismissed") {
		t.Fatalf("AllocateTaskWorktree error = %v, want concurrent dismissal refusal", err)
	}
	task, getErr := store.GetTask(ctx, "task-race")
	if getErr != nil || task.State != string(TaskDismissed) {
		t.Fatalf("task = %+v, err=%v; dismissed task was resurrected", task, getErr)
	}
	events, eventErr := store.ListTaskEvents(ctx, task.ID)
	if eventErr != nil || len(events) != 1 || events[0].Kind != "task_dismissed_planned_ttl" {
		t.Fatalf("events = %+v, err=%v", events, eventErr)
	}
	if len(manager.removedForce) != 1 {
		t.Fatalf("worktree cleanup calls = %v, want one", manager.removedForce)
	}
}

func TestEngineAllocateTaskWorktreeWaitsForCheckoutMutationLock(t *testing.T) {
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
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(20 * time.Millisecond)
		_, _ = store.ReleaseResourceLock(context.Background(), key, "task:other", "other-token")
	}()

	task, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:     home,
		Repo:     "owner/repo",
		TaskID:   "task-1",
		Branch:   "task-1",
		Owner:    "lead",
		Checkout: checkout,
	}, manager)

	if err != nil {
		t.Fatalf("AllocateTaskWorktree returned error: %v", err)
	}
	<-released
	if task.WorktreePath == "" {
		t.Fatalf("task worktree path is empty: %+v", task)
	}
	if len(manager.calls) != 1 {
		t.Fatalf("AddWorktree calls = %+v, want one call after checkout lock release", manager.calls)
	}
}

func TestAcquireCheckoutMutationLockWithWaitBudgetTimesOutWhenLocked(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	checkout := t.TempDir()
	key, err := checkoutMutationLockKey(checkout)
	if err != nil {
		t.Fatalf("checkoutMutationLockKey returned error: %v", err)
	}
	if acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  "task:other",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}

	_, _, err = acquireCheckoutMutationLockWithWaitBudget(ctx, store, checkout, "worktree:task-1", time.Now().UTC(), 20*time.Millisecond, 5*time.Millisecond)

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "Waited up to") {
		t.Fatalf("error = %v, want checkout wait timeout BlockedError", err)
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
		Owner:    "lead",
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
		Owner:    "lead",
		Checkout: t.TempDir(),
	}, manager)

	if err == nil || !strings.Contains(err.Error(), "belongs to repo owner/other") {
		t.Fatalf("error = %v, want repo mismatch error", err)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("AddWorktree ran despite repo mismatch: %+v", manager.calls)
	}
}

func TestEngineAllocateTaskWorktreeBlocksWhenBranchLocked(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "other"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	manager := &fakeWorktreeManager{}

	_, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:     t.TempDir(),
		Repo:     "owner/repo",
		TaskID:   "task-1",
		Branch:   "task-1",
		Owner:    "lead",
		Checkout: t.TempDir(),
	}, manager)

	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v, want BlockedError", err)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("AddWorktree ran despite branch lock: %+v", manager.calls)
	}
}

func TestEngineAllocateTaskWorktreeReleasesCreatedBranchLockOnFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	manager := &fakeWorktreeManager{err: errors.New("git failed")}

	_, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:     t.TempDir(),
		Repo:     "owner/repo",
		TaskID:   "task-1",
		Branch:   "task-1",
		Owner:    "lead",
		Checkout: t.TempDir(),
	}, manager)

	if err == nil {
		t.Fatal("AllocateTaskWorktree succeeded despite worktree failure")
	}
	if _, lockErr := store.GetBranchLock(ctx, "owner/repo", "task-1"); !errors.Is(lockErr, sql.ErrNoRows) {
		t.Fatalf("branch lock after failure error = %v, want sql.ErrNoRows", lockErr)
	}
}

func TestEngineAllocateTaskWorktreeReusesExistingTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	home := t.TempDir()
	path, err := TaskWorktreePath(home, "owner/repo", "task-1")
	if err != nil {
		t.Fatalf("TaskWorktreePath returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-1",
		RepoFullName: "owner/repo",
		GoalID:       "goal-1",
		Title:        "First",
		State:        string(TaskPlanned),
		Branch:       "task-1",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	manager := &fakeWorktreeManager{
		pathHeads: map[string]string{path: "agent-head"},
		revHeads:  map[string]string{"HEAD": "base-head"},
		ancestor:  true,
	}

	task, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:     home,
		Repo:     "owner/repo",
		TaskID:   "task-1",
		Branch:   "task-1",
		Owner:    "lead",
		Checkout: t.TempDir(),
	}, manager)

	if err != nil {
		t.Fatalf("AllocateTaskWorktree returned error: %v", err)
	}
	if task.State != string(TaskImplementing) || task.WorktreePath != path {
		t.Fatalf("task = %+v, want implementing with existing path %q", task, path)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("AddWorktree ran despite existing task worktree: %+v", manager.calls)
	}
	if len(manager.removed) != 0 || len(manager.deletedBranches) != 0 || len(manager.cleanCalls) != 0 {
		t.Fatalf("fresh worktree was mutated or dirty-checked: removed=%v deleted=%v clean_checks=%v", manager.removed, manager.deletedBranches, manager.cleanCalls)
	}
	if len(manager.ancestorCalls) != 1 || manager.ancestorCalls[0] != [2]string{"base-head", "agent-head"} {
		t.Fatalf("ancestor calls = %v, want base-head -> agent-head", manager.ancestorCalls)
	}
}

func TestEngineAllocateTaskWorktreeRecutsCleanOffLineageWorktree(t *testing.T) {
	ctx, store, engine, manager, request, path, oldHead, baseHead := setupOffLineageTaskWorktree(t, false)

	task, err := engine.AllocateTaskWorktree(ctx, request, manager)
	if err != nil {
		t.Fatalf("AllocateTaskWorktree: %v", err)
	}
	if task.WorktreePath != path {
		t.Fatalf("task worktree = %q, want %q", task.WorktreePath, path)
	}
	newHead, err := (gitutil.NewHostClient(path)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA re-cut worktree: %v", err)
	}
	if newHead != baseHead || newHead == oldHead {
		t.Fatalf("re-cut head = %s, want base %s and not old %s", newHead, baseHead, oldHead)
	}
	events, err := store.ListTaskEvents(ctx, request.TaskID)
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "stale_worktree_recut" ||
		!strings.Contains(events[0].Reason, "old_head="+oldHead) ||
		!strings.Contains(events[0].Reason, "new_head="+newHead) {
		t.Fatalf("task events = %+v", events)
	}
}

func TestEngineAllocateTaskWorktreeBlocksDirtyOffLineageWorktree(t *testing.T) {
	ctx, store, engine, manager, request, path, oldHead, baseHead := setupOffLineageTaskWorktree(t, true)

	_, err := engine.AllocateTaskWorktree(ctx, request, manager)
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("AllocateTaskWorktree error = %v, want BlockedError", err)
	}
	for _, want := range []string{path, oldHead, baseHead, "uncommitted changes", "manually salvage"} {
		if !strings.Contains(blocked.Reason, want) {
			t.Fatalf("blocked reason %q missing %q", blocked.Reason, want)
		}
	}
	headAfter, err := (gitutil.NewHostClient(path)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA preserved worktree: %v", err)
	}
	if headAfter != oldHead {
		t.Fatalf("dirty worktree head = %s, want preserved %s", headAfter, oldHead)
	}
	dirtyPath := filepath.Join(path, "salvage.txt")
	content, err := os.ReadFile(dirtyPath)
	if err != nil {
		t.Fatalf("read preserved dirty file: %v", err)
	}
	if string(content) != "salvage me\n" {
		t.Fatalf("preserved dirty content = %q", content)
	}
	task, err := store.GetTask(ctx, request.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(TaskBlocked) || task.WorktreePath != path {
		t.Fatalf("blocked task = %+v, want blocked with preserved worktree", task)
	}
	events, err := store.ListTaskEvents(ctx, request.TaskID)
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "stale_worktree_dirty_blocked" ||
		events[0].ToState != string(TaskBlocked) ||
		!strings.Contains(events[0].Reason, "uncommitted changes") {
		t.Fatalf("task events = %+v", events)
	}
}

func TestEngineAllocateTaskWorktreeUsesExistingBranchWhenBranchAlreadyExists(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	home := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-1",
		RepoFullName: "owner/repo",
		GoalID:       "goal-1",
		Title:        "First",
		State:        string(TaskImplementing),
		Branch:       "task-1",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	manager := &fakeWorktreeManager{existingBranches: map[string]bool{"task-1": true}}

	task, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:     home,
		Repo:     "owner/repo",
		TaskID:   "task-1",
		Branch:   "task-1",
		Owner:    "lead",
		Checkout: t.TempDir(),
	}, manager)

	if err != nil {
		t.Fatalf("AllocateTaskWorktree returned error: %v", err)
	}
	wantPath := filepath.Join(home, "worktrees", "owner--repo", "task-1")
	if task.WorktreePath != wantPath {
		t.Fatalf("worktree path = %q, want %q", task.WorktreePath, wantPath)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("AddWorktree ran for existing branch: %+v", manager.calls)
	}
	if len(manager.existingCalls) != 1 || manager.existingCalls[0].branch != "task-1" || manager.existingCalls[0].path != wantPath {
		t.Fatalf("existing branch worktree calls = %+v", manager.existingCalls)
	}
}

func TestDelegationWorktreePath(t *testing.T) {
	path, err := DelegationWorktreePath("/home/gitmoot", "owner/repo", "job-1", "d1", 0)
	if err != nil {
		t.Fatalf("DelegationWorktreePath returned error: %v", err)
	}
	want := filepath.Join("/home/gitmoot", "worktrees", "owner--repo", "delegations", "job-1", "d1")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	// A retry attempt gets an isolated /retry/<n> subdirectory so it never
	// collides with the failed original attempt's worktree.
	retryPath, err := DelegationWorktreePath("/home/gitmoot", "owner/repo", "job-1", "d1", 2)
	if err != nil {
		t.Fatalf("DelegationWorktreePath (retry) returned error: %v", err)
	}
	wantRetry := filepath.Join("/home/gitmoot", "worktrees", "owner--repo", "delegations", "job-1", "d1", "retry", "2")
	if retryPath != wantRetry {
		t.Fatalf("retry path = %q, want %q", retryPath, wantRetry)
	}
	if retryPath == want {
		t.Fatalf("retry path %q collides with original attempt path", retryPath)
	}
	for _, tc := range []struct {
		name       string
		home       string
		repo       string
		parentJob  string
		delegation string
	}{
		{name: "empty home", home: "", repo: "owner/repo", parentJob: "job-1", delegation: "d1"},
		{name: "empty repo", home: "/home/gitmoot", repo: "", parentJob: "job-1", delegation: "d1"},
		{name: "nested repo", home: "/home/gitmoot", repo: "owner/repo/extra", parentJob: "job-1", delegation: "d1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DelegationWorktreePath(tc.home, tc.repo, tc.parentJob, tc.delegation, 0); err == nil {
				t.Fatal("DelegationWorktreePath accepted invalid input")
			}
		})
	}

	// Parent/delegation ids that are not a plain path segment -- a "/"-bearing
	// continuation id, or a "../" attempt -- are now SANITIZED into a safe segment
	// rather than rejected, so the multi-round coordinator can dispatch an
	// implement delegation from a continuation. The result must stay traversal-safe
	// (never escape the delegations root).
	for _, tc := range []struct {
		name       string
		parentJob  string
		delegation string
	}{
		{name: "slashed parent (continuation)", parentJob: "job-1/continuation/continuation", delegation: "d1"},
		{name: "dotdot parent", parentJob: "../job", delegation: "d1"},
		{name: "dotdot delegation", parentJob: "job-1", delegation: "../d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := DelegationWorktreePath("/home/gitmoot", "owner/repo", tc.parentJob, tc.delegation, 0)
			if err != nil {
				t.Fatalf("DelegationWorktreePath should sanitize, got error: %v", err)
			}
			root := filepath.Join("/home/gitmoot", "worktrees", "owner--repo", "delegations")
			rel, err := filepath.Rel(root, p)
			if err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("sanitized path %q escaped the delegations root (rel=%q err=%v)", p, rel, err)
			}
		})
	}
}

func TestAllocateDelegationWorktreeAddsGitWorktreeAndReturnsPathBranch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
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
		if lock.OwnerJobID != "worktree:job-1/d1" {
			t.Fatalf("checkout lock owner = %q, want worktree:job-1/d1", lock.OwnerJobID)
		}
	}}

	result, err := engine.AllocateDelegationWorktree(ctx, DelegationWorktreeRequest{
		Home:         home,
		Repo:         "owner/repo",
		ParentJobID:  "job-1",
		DelegationID: "d1",
		Delegation:   Delegation{ID: "d1", Agent: "helper", Action: "implement"},
		BaseBranch:   "main",
		Owner:        "helper",
		Checkout:     checkout,
	}, manager)
	if err != nil {
		t.Fatalf("AllocateDelegationWorktree returned error: %v", err)
	}
	wantPath := filepath.Join(home, "worktrees", "owner--repo", "delegations", "job-1", "d1")
	if result.Path != wantPath {
		t.Fatalf("path = %q, want %q", result.Path, wantPath)
	}
	wantBranch := delegationBranchName(Delegation{ID: "d1"}, "job-1", "d1", 0)
	if result.Branch != wantBranch {
		t.Fatalf("branch = %q, want %q", result.Branch, wantBranch)
	}
	if len(manager.calls) != 1 || manager.calls[0].branch != wantBranch || manager.calls[0].path != wantPath || manager.calls[0].base != "main" {
		t.Fatalf("worktree calls = %+v", manager.calls)
	}
	// The tasks table must not be touched by delegation allocation.
	if _, err := store.GetTask(ctx, "d1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetTask after delegation allocation error = %v, want sql.ErrNoRows", err)
	}
	lock, err := store.GetBranchLock(ctx, "owner/repo", wantBranch)
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.Owner != "helper" {
		t.Fatalf("lock owner = %q, want helper", lock.Owner)
	}
	if _, err := store.GetResourceLock(ctx, key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("checkout lock after AddWorktree error = %v, want sql.ErrNoRows", err)
	}
}

func TestAllocateDelegationWorktreeUsesCheckoutMutationLock(t *testing.T) {
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
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(20 * time.Millisecond)
		_, _ = store.ReleaseResourceLock(context.Background(), key, "task:other", "other-token")
	}()

	result, err := engine.AllocateDelegationWorktree(ctx, DelegationWorktreeRequest{
		Home:         home,
		Repo:         "owner/repo",
		ParentJobID:  "job-1",
		DelegationID: "d1",
		Owner:        "helper",
		Checkout:     checkout,
	}, manager)
	if err != nil {
		t.Fatalf("AllocateDelegationWorktree returned error: %v", err)
	}
	<-released
	if result.Path == "" {
		t.Fatalf("delegation worktree path is empty: %+v", result)
	}
	if len(manager.calls) != 1 {
		t.Fatalf("AddWorktree calls = %+v, want one call after checkout lock release", manager.calls)
	}
	if _, err := store.GetResourceLock(ctx, key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("checkout lock after AddWorktree error = %v, want sql.ErrNoRows", err)
	}
}

func TestAllocateDelegationWorktreeBranchNaming(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)

	hinted, err := engine.AllocateDelegationWorktree(ctx, DelegationWorktreeRequest{
		Home:         t.TempDir(),
		Repo:         "owner/repo",
		ParentJobID:  "job-1",
		DelegationID: "d1",
		Delegation:   Delegation{ID: "d1", Worktree: "Feature Login"},
		Owner:        "helper",
		Checkout:     t.TempDir(),
	}, &fakeWorktreeManager{})
	if err != nil {
		t.Fatalf("AllocateDelegationWorktree (hinted) returned error: %v", err)
	}
	// The worktree hint is appended only as a human-readable suffix; the branch is
	// always namespaced with the parent-short and delegation id so it stays unique
	// across siblings regardless of the hint.
	wantHinted := "gitmoot-delegation-" + parentShort("job-1") + "-d1-feature-login"
	if hinted.Branch != wantHinted {
		t.Fatalf("hinted branch = %q, want %q", hinted.Branch, wantHinted)
	}

	fallback, err := engine.AllocateDelegationWorktree(ctx, DelegationWorktreeRequest{
		Home:         t.TempDir(),
		Repo:         "owner/repo",
		ParentJobID:  "job-1",
		DelegationID: "d2",
		Delegation:   Delegation{ID: "d2"},
		Owner:        "helper",
		Checkout:     t.TempDir(),
	}, &fakeWorktreeManager{})
	if err != nil {
		t.Fatalf("AllocateDelegationWorktree (fallback) returned error: %v", err)
	}
	want := "gitmoot-delegation-" + parentShort("job-1") + "-d2"
	if fallback.Branch != want {
		t.Fatalf("fallback branch = %q, want %q", fallback.Branch, want)
	}
}

func TestAllocateDelegationWorktreeReusesExistingBranch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	home := t.TempDir()
	branch := delegationBranchName(Delegation{ID: "d1"}, "job-1", "d1", 0)
	manager := &fakeWorktreeManager{existingBranches: map[string]bool{branch: true}}

	result, err := engine.AllocateDelegationWorktree(ctx, DelegationWorktreeRequest{
		Home:         home,
		Repo:         "owner/repo",
		ParentJobID:  "job-1",
		DelegationID: "d1",
		Owner:        "helper",
		Checkout:     t.TempDir(),
	}, manager)
	if err != nil {
		t.Fatalf("AllocateDelegationWorktree returned error: %v", err)
	}
	wantPath := filepath.Join(home, "worktrees", "owner--repo", "delegations", "job-1", "d1")
	if result.Path != wantPath || result.Branch != branch {
		t.Fatalf("result = %+v, want path %q branch %q", result, wantPath, branch)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("AddWorktree ran for existing branch: %+v", manager.calls)
	}
	if len(manager.existingCalls) != 1 || manager.existingCalls[0].branch != branch || manager.existingCalls[0].path != wantPath {
		t.Fatalf("existing branch worktree calls = %+v", manager.existingCalls)
	}
}

func TestAllocateDelegationWorktreeExistingBranchLineageOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		clean     bool
		wantBlock bool
		wantEvent string
	}{
		{name: "clean stale worktree is re-cut", clean: true, wantEvent: "stale_worktree_recut"},
		{name: "dirty stale worktree is preserved and blocked", clean: false, wantBlock: true, wantEvent: "stale_worktree_dirty_blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			engine := testEngine(store)
			home := t.TempDir()
			branch := delegationBranchName(Delegation{ID: "d1"}, "job-1", "d1", 0)
			path, err := DelegationWorktreePath(home, "owner/repo", "job-1", "d1", 0)
			if err != nil {
				t.Fatalf("DelegationWorktreePath: %v", err)
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("MkdirAll delegation worktree: %v", err)
			}
			manager := &fakeWorktreeManager{
				existingBranches: map[string]bool{branch: true},
				pathHeads:        map[string]string{path: "stale-head"},
				revHeads:         map[string]string{"parent": "parent-head"},
				ancestor:         false,
				ancestorSet:      true,
				clean:            tc.clean,
				cleanSet:         true,
			}

			result, err := engine.AllocateDelegationWorktree(ctx, DelegationWorktreeRequest{
				Home:         home,
				Repo:         "owner/repo",
				ParentJobID:  "job-1",
				DelegationID: "d1",
				BaseBranch:   "parent",
				Owner:        "helper",
				Checkout:     t.TempDir(),
			}, manager)
			var blocked BlockedError
			if tc.wantBlock {
				if !errors.As(err, &blocked) {
					t.Fatalf("AllocateDelegationWorktree error = %v, want BlockedError", err)
				}
				if len(manager.removed) != 0 || len(manager.deletedBranches) != 0 || len(manager.calls) != 0 {
					t.Fatalf("dirty delegation worktree was mutated: removed=%v deleted=%v add=%v", manager.removed, manager.deletedBranches, manager.calls)
				}
				if !strings.Contains(blocked.Reason, "uncommitted changes") {
					t.Fatalf("blocked reason = %q", blocked.Reason)
				}
			} else {
				if err != nil {
					t.Fatalf("AllocateDelegationWorktree: %v", err)
				}
				if result.Path != path {
					t.Fatalf("result path = %q, want %q", result.Path, path)
				}
				if len(manager.removed) != 1 || len(manager.deletedBranches) != 1 || len(manager.calls) != 1 {
					t.Fatalf("clean delegation recut calls: removed=%v deleted=%v add=%v", manager.removed, manager.deletedBranches, manager.calls)
				}
			}
			events, err := store.ListJobEvents(ctx, "job-1")
			if err != nil {
				t.Fatalf("ListJobEvents: %v", err)
			}
			if len(events) != 1 || events[0].Kind != tc.wantEvent {
				t.Fatalf("job events = %+v, want kind %q", events, tc.wantEvent)
			}
		})
	}
}

func TestAllocateDelegationWorktreeReleasesBranchLockOnFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	manager := &fakeWorktreeManager{err: errors.New("git failed")}
	branch := delegationBranchName(Delegation{ID: "d1"}, "job-1", "d1", 0)

	_, err := engine.AllocateDelegationWorktree(ctx, DelegationWorktreeRequest{
		Home:         t.TempDir(),
		Repo:         "owner/repo",
		ParentJobID:  "job-1",
		DelegationID: "d1",
		Owner:        "helper",
		Checkout:     t.TempDir(),
	}, manager)
	if err == nil {
		t.Fatal("AllocateDelegationWorktree succeeded despite worktree failure")
	}
	if _, lockErr := store.GetBranchLock(ctx, "owner/repo", branch); !errors.Is(lockErr, sql.ErrNoRows) {
		t.Fatalf("branch lock after failure error = %v, want sql.ErrNoRows", lockErr)
	}
}

func TestEngineReclaimTerminalTaskWorktreeKeepsLiveDirtyAdhocAtAnyAge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("MkdirAll checkout: %v", err)
	}
	runWorktreeGit(t, checkout, "init", "-b", "main")
	runWorktreeGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runWorktreeGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".gitignore"), []byte("GOALS/\nCLAUDE.local.md\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .gitignore: %v", err)
	}
	runWorktreeGit(t, checkout, "add", "base.txt", ".gitignore")
	runWorktreeGit(t, checkout, "commit", "-m", "base")

	home := filepath.Join(root, "home")
	path, err := TaskWorktreePath(home, "owner/repo", "adhoc-old-live")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll worktree parent: %v", err)
	}
	runWorktreeGit(t, checkout, "worktree", "add", "-b", "adhoc-old-live", path, "HEAD")
	untrackedPath := filepath.Join(path, "uncommitted.txt")
	if err := os.WriteFile(untrackedPath, []byte("preserve me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile uncommitted change: %v", err)
	}

	liveProcess := exec.Command("sleep", "30")
	liveProcess.Dir = path
	if err := liveProcess.Start(); err != nil {
		t.Fatalf("start live worktree process: %v", err)
	}
	t.Cleanup(func() {
		_ = liveProcess.Process.Kill()
		_ = liveProcess.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		live, known := WorktreeLiveness(path)
		if live && known {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live worktree process was not observable: live=%v known=%v", live, known)
		}
		time.Sleep(10 * time.Millisecond)
	}

	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "adhoc-old-live",
		RepoFullName: "owner/repo",
		State:        string(TaskDismissed),
		Branch:       "adhoc-old-live",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	manager := gitutil.NewHostClient(checkout)
	engine := testEngine(store)
	engine.WorktreeHasLiveProcess = nil

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, "adhoc-old-live", manager)
	if err != nil {
		t.Fatalf("live ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimLiveProcess {
		t.Fatalf("live outcome = %+v, want retained live-process task", outcome)
	}
	if live, known := WorktreeLiveness(path); !live || !known {
		t.Fatalf("KEEP fixture process after reclaim: live=%v known=%v", live, known)
	}
	if content, err := os.ReadFile(untrackedPath); err != nil || string(content) != "preserve me\n" {
		t.Fatalf("uncommitted work after live reclaim = %q, err=%v", content, err)
	}

	if err := liveProcess.Process.Kill(); err != nil {
		t.Fatalf("kill live worktree process: %v", err)
	}
	_ = liveProcess.Wait()
	// The live-process arm above uses the real /proc scan. After the child exits,
	// make the no-live premise deterministic so an unrelated unreadable host PID
	// cannot prevent this same test from reaching the content guards.
	engine.WorktreeLiveness = func(string) (bool, bool) { return false, true }
	outcome, err = engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, "adhoc-old-live", manager)
	if err != nil {
		t.Fatalf("dirty ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimDirty {
		t.Fatalf("dirty outcome = %+v, want retained dirty task", outcome)
	}
	if clean, err := manager.WorktreeCleanAt(ctx, path); err != nil || clean {
		t.Fatalf("dirty WorktreeCleanAt = %v, err=%v, want false nil", clean, err)
	}

	if err := os.Remove(untrackedPath); err != nil {
		t.Fatalf("remove untracked fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, "GOALS"), 0o755); err != nil {
		t.Fatalf("MkdirAll ignored content: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "GOALS", "plan.md"), []byte("preserve ignored plan\n"), 0o644); err != nil {
		t.Fatalf("WriteFile ignored plan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "CLAUDE.local.md"), []byte("preserve ignored instructions\n"), 0o644); err != nil {
		t.Fatalf("WriteFile ignored instructions: %v", err)
	}
	runWorktreeGit(t, path, "check-ignore", "GOALS/plan.md", "CLAUDE.local.md")
	outcome, err = engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, "adhoc-old-live", manager)
	if err != nil {
		t.Fatalf("ignored-content ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimDirty {
		t.Fatalf("ignored-content outcome = %+v, want retained dirty task", outcome)
	}
	if pristine, err := manager.WorktreePristineAt(ctx, path); err != nil || pristine {
		t.Fatalf("ignored-content WorktreePristineAt = %v, err=%v, want false nil", pristine, err)
	}
	task, err := store.GetTask(ctx, "adhoc-old-live")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.WorktreePath != path {
		t.Fatalf("worktree path = %q, want preserved %q", task.WorktreePath, path)
	}
	for _, ignored := range []string{"GOALS/plan.md", "CLAUDE.local.md"} {
		if _, err := os.Stat(filepath.Join(path, ignored)); err != nil {
			t.Fatalf("ignored content %s was removed: %v", ignored, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(path, "GOALS")); err != nil {
		t.Fatalf("RemoveAll ignored GOALS: %v", err)
	}
	if err := os.Remove(filepath.Join(path, "CLAUDE.local.md")); err != nil {
		t.Fatalf("remove ignored instructions: %v", err)
	}
	runWorktreeGit(t, path, "checkout", "--detach")
	if err := os.WriteFile(filepath.Join(path, "detached.txt"), []byte("detached commit\n"), 0o644); err != nil {
		t.Fatalf("WriteFile detached commit: %v", err)
	}
	runWorktreeGit(t, path, "add", "detached.txt")
	runWorktreeGit(t, path, "commit", "-m", "detached local commit")
	if pristine, err := manager.WorktreePristineAt(ctx, path); err != nil || !pristine {
		t.Fatalf("detached committed WorktreePristineAt = %v, err=%v, want true nil", pristine, err)
	}
	detachedHead, err := manager.HeadSHAAt(ctx, path)
	if err != nil {
		t.Fatalf("detached HeadSHAAt: %v", err)
	}
	branchHead, err := manager.RevParse(ctx, "refs/heads/adhoc-old-live")
	if err != nil {
		t.Fatalf("task branch RevParse: %v", err)
	}
	if reachable, err := manager.IsAncestor(ctx, detachedHead, branchHead); err != nil || reachable {
		t.Fatalf("detached head reachable from task branch = %v, err=%v, want false nil", reachable, err)
	}
	outcome, err = engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, "adhoc-old-live", manager)
	if err != nil {
		t.Fatalf("detached-head ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimHeadUnreachable {
		t.Fatalf("detached-head outcome = %+v, want retained unreachable head", outcome)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("detached-head worktree was removed: %v", err)
	}
	task, err = store.GetTask(ctx, "adhoc-old-live")
	if err != nil || task.WorktreePath != path {
		t.Fatalf("detached-head task = %+v, err=%v, want preserved path %q", task, err, path)
	}
}

func TestEngineReclaimTerminalTaskWorktreeReclaimsTaskKinds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    string
		state TaskState
	}{
		{name: "adhoc dismissed", id: "adhoc-clean", state: TaskDismissed},
		{name: "review pr merged", id: "review-pr-17-clean", state: TaskMerged},
		{name: "task superseded", id: "task-clean", state: TaskSuperseded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			home := t.TempDir()
			checkout := t.TempDir()
			path, err := TaskWorktreePath(home, "owner/repo", tc.id)
			if err != nil {
				t.Fatalf("TaskWorktreePath: %v", err)
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("MkdirAll worktree: %v", err)
			}
			if err := store.UpsertTask(ctx, db.Task{
				ID:           tc.id,
				RepoFullName: "owner/repo",
				State:        string(tc.state),
				Branch:       tc.id,
				WorktreePath: path,
			}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			manager := &fakeWorktreeManager{existingBranches: map[string]bool{tc.id: true}}
			engine := testEngine(store)

			outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, tc.id, manager)
			if err != nil {
				t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
			}
			if !outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimReclaimed {
				t.Fatalf("outcome = %+v, want reclaimed", outcome)
			}
			if len(manager.cleanCalls) != 2 || len(manager.removed) != 1 || manager.removed[0] != path {
				t.Fatalf("manager safety/removal calls: clean=%v removed=%v", manager.cleanCalls, manager.removed)
			}
			if len(manager.deletedBranches) != 0 {
				t.Fatalf("terminal task branch was deleted: %v", manager.deletedBranches)
			}
			task, err := store.GetTask(ctx, tc.id)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.WorktreePath != "" || task.Branch != tc.id {
				t.Fatalf("task after reclaim = %+v, want empty path and preserved branch", task)
			}
			events, err := store.ListTaskEvents(ctx, tc.id)
			if err != nil {
				t.Fatalf("ListTaskEvents: %v", err)
			}
			if len(events) != 1 || events[0].Kind != "terminal_worktree_reclaimed" || events[0].Reason != path {
				t.Fatalf("task events = %+v", events)
			}
		})
	}
}

func TestEngineReclaimTerminalTaskWorktreeKeepsUnsafeCandidates(t *testing.T) {
	for _, tc := range []struct {
		name           string
		live           bool
		known          bool
		clean          bool
		want           TaskWorktreeReclaimClassification
		wantCleanCalls int
	}{
		{name: "live process", live: true, known: true, clean: true, want: TaskWorktreeReclaimLiveProcess},
		{name: "unknown process table", known: false, clean: true, want: TaskWorktreeReclaimLivenessUnknown},
		{name: "dirty worktree", known: true, clean: false, want: TaskWorktreeReclaimDirty, wantCleanCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			home := t.TempDir()
			path, err := TaskWorktreePath(home, "owner/repo", "adhoc-terminal")
			if err != nil {
				t.Fatalf("TaskWorktreePath: %v", err)
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("MkdirAll worktree: %v", err)
			}
			if err := store.UpsertTask(ctx, db.Task{
				ID:           "adhoc-terminal",
				RepoFullName: "owner/repo",
				State:        string(TaskDismissed),
				WorktreePath: path,
			}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			manager := &fakeWorktreeManager{cleanSet: true, clean: tc.clean}
			engine := testEngine(store)
			engine.WorktreeHasLiveProcess = nil
			engine.WorktreeLiveness = func(gotPath string) (bool, bool) {
				if gotPath != path {
					t.Fatalf("liveness path = %q, want %q", gotPath, path)
				}
				return tc.live, tc.known
			}

			outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), "adhoc-terminal", manager)
			if err != nil {
				t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
			}
			if outcome.Reclaimed || outcome.Classification != tc.want {
				t.Fatalf("outcome = %+v, want retained %s", outcome, tc.want)
			}
			if len(manager.cleanCalls) != tc.wantCleanCalls || len(manager.removed) != 0 {
				t.Fatalf("unsafe worktree was over-inspected or removed: clean=%v removed=%v", manager.cleanCalls, manager.removed)
			}
			task, err := store.GetTask(ctx, "adhoc-terminal")
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.WorktreePath != path {
				t.Fatalf("worktree path = %q, want preserved %q", task.WorktreePath, path)
			}
		})
	}
}

// A preserved task branch that no longer exists means safety is unprovable, not
// that the pass is broken: it classifies instead of erroring on every tick.
func TestEngineReclaimTerminalTaskWorktreeClassifiesMissingBranch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	const taskID = "review-pr-42-missing-branch"
	path, err := TaskWorktreePath(home, "owner/repo", taskID)
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           taskID,
		RepoFullName: "owner/repo",
		State:        string(TaskMerged),
		Branch:       taskID,
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	manager := &fakeWorktreeManager{existingBranches: map[string]bool{}}
	engine := testEngine(store)

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), taskID, manager)
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimHeadUnreachable {
		t.Fatalf("outcome = %+v, want head_unreachable retention", outcome)
	}
	if len(manager.removed) != 0 {
		t.Fatalf("worktree was removed without a reachable branch: %v", manager.removed)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("worktree was deleted: %v", statErr)
	}
}

type fakeTerminalWorktreeRemovalError struct{}

func (fakeTerminalWorktreeRemovalError) Error() string {
	return "registered root does not own worktree"
}

func (fakeTerminalWorktreeRemovalError) TerminalWorktreeRemoval() bool {
	return true
}

func TestEngineReclaimTerminalTaskWorktreeClassifiesUnremovableOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	path, err := TaskWorktreePath(home, "owner/repo", "review-pr-unremovable")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "review-pr-unremovable",
		RepoFullName: "owner/repo",
		State:        string(TaskMerged),
		Branch:       "review-pr-unremovable",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	manager := &fakeWorktreeManager{
		removeErr:        fakeTerminalWorktreeRemovalError{},
		existingBranches: map[string]bool{"review-pr-unremovable": true},
	}
	engine := testEngine(store)

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), "review-pr-unremovable", manager)
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimUnremovable {
		t.Fatalf("outcome = %+v, want terminal-unremovable", outcome)
	}
	ids, err := store.TaskIDsWithTerminalWorktree(ctx)
	if err != nil {
		t.Fatalf("TaskIDsWithTerminalWorktree: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("classified candidate remained retryable: %v", ids)
	}
	events, err := store.ListTaskEvents(ctx, "review-pr-unremovable")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "terminal_worktree_unremovable" || events[0].Reason != path {
		t.Fatalf("task events = %+v", events)
	}
}

func TestEngineReclaimTerminalTaskWorktreeClassifiesMissingGitAdmin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("MkdirAll checkout: %v", err)
	}
	runWorktreeGit(t, checkout, "init", "-b", "main")
	runWorktreeGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runWorktreeGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	runWorktreeGit(t, checkout, "add", "base.txt")
	runWorktreeGit(t, checkout, "commit", "-m", "base")

	home := filepath.Join(root, "home")
	path, err := TaskWorktreePath(home, "owner/repo", "adhoc-missing-admin")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll worktree parent: %v", err)
	}
	runWorktreeGit(t, checkout, "worktree", "add", "-b", "adhoc-missing-admin", path, "HEAD")
	gitFile, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		t.Fatalf("ReadFile .git: %v", err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(gitFile)), "\n")
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		t.Fatalf("worktree .git pointer = %q", line)
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	if err := os.RemoveAll(filepath.Clean(gitDir)); err != nil {
		t.Fatalf("remove isolated worktree admin: %v", err)
	}

	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "adhoc-missing-admin",
		RepoFullName: "owner/repo",
		State:        string(TaskDismissed),
		Branch:       "adhoc-missing-admin",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	engine := testEngine(store)
	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, "adhoc-missing-admin", gitutil.NewHostClient(checkout))
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimUnremovable {
		t.Fatalf("outcome = %+v, want terminal-unremovable", outcome)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unprovable worktree was removed: %v", err)
	}
	ids, err := store.TaskIDsWithTerminalWorktree(ctx)
	if err != nil {
		t.Fatalf("TaskIDsWithTerminalWorktree: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("missing-admin candidate remained retryable: %v", ids)
	}
}

func TestEngineReclaimTerminalTaskWorktreeKeepsBranchLockOwner(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	path, err := TaskWorktreePath(home, "owner/repo", "task-locked")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-locked",
		RepoFullName: "owner/repo",
		State:        string(TaskMerged),
		Branch:       "task-locked",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	created, err := store.CreateLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-locked", Owner: "live-owner"})
	if err != nil || !created {
		t.Fatalf("CreateLock created=%v err=%v", created, err)
	}
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), "task-locked", manager)
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimActiveOwner {
		t.Fatalf("outcome = %+v, want active owner keep", outcome)
	}
	if len(manager.cleanCalls) != 0 || len(manager.removed) != 0 {
		t.Fatalf("owned worktree was inspected or removed: clean=%v removed=%v", manager.cleanCalls, manager.removed)
	}
}

func TestEngineReclaimTerminalTaskWorktreeRejectsUnmanagedPath(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	unmanaged := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-unmanaged",
		RepoFullName: "owner/repo",
		State:        string(TaskDismissed),
		WorktreePath: unmanaged,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), "task-unmanaged", manager)
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimPathMismatch {
		t.Fatalf("outcome = %+v, want path mismatch keep", outcome)
	}
	if len(manager.cleanCalls) != 0 || len(manager.removed) != 0 {
		t.Fatalf("unmanaged path was inspected or removed: clean=%v removed=%v", manager.cleanCalls, manager.removed)
	}
	if _, err := os.Stat(unmanaged); err != nil {
		t.Fatalf("unmanaged path changed: %v", err)
	}
}

func TestEngineReclaimTerminalTaskWorktreeKeepsBlockedJobOwner(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	path, err := TaskWorktreePath(home, "owner/repo", "review-pr-active")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "review-pr-active",
		RepoFullName: "owner/repo",
		State:        string(TaskMerged),
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	payload, err := marshalPayload(JobPayload{TaskID: "review-pr-active", WorktreePath: path})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "active-owner", Agent: "reviewer", Type: "review", State: string(JobBlocked), Payload: payload,
	}, db.JobEvent{Kind: string(JobBlocked), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), "review-pr-active", manager)
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimActiveOwner {
		t.Fatalf("outcome = %+v, want active owner keep", outcome)
	}
	if len(manager.cleanCalls) != 0 || len(manager.removed) != 0 {
		t.Fatalf("active job worktree was inspected or removed: clean=%v removed=%v", manager.cleanCalls, manager.removed)
	}
}

func TestEngineReclaimAgedFixWorktreeKeepsLiveProcess(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	const jobID = "fix-live"
	path, err := FixWorktreePath(home, "owner/repo", jobID)
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll fix worktree: %v", err)
	}
	payload, err := marshalPayload(JobPayload{
		Repo:         "owner/repo",
		WorktreePath: path,
		FixWorktree:  true,
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      jobID,
		Agent:   "fixer",
		Type:    "implement",
		State:   string(JobSucceeded),
		Repo:    "owner/repo",
		Payload: payload,
	}, db.JobEvent{Kind: string(JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	engine := testEngine(store)
	engine.Home = home
	engine.WorktreeHasLiveProcess = nil
	engine.WorktreeLiveness = func(gotPath string) (bool, bool) {
		if gotPath != path {
			t.Fatalf("liveness path = %q, want %q", gotPath, path)
		}
		return true, true
	}

	reclaimed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ReclaimAgedTerminalDelegationWorktreeOutcome: %v", err)
	}
	if reclaimed {
		t.Fatal("live fix worktree was reclaimed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live fix worktree was removed: %v", err)
	}
}

func TestEngineReclaimAgedFixWorktreeCompletesAlreadyAbsentPath(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	const jobID = "fix-already-absent"
	path, err := FixWorktreePath(home, "owner/repo", jobID)
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	payload, err := marshalPayload(JobPayload{
		Repo: "owner/repo", Branch: "feature/fix", WorktreePath: path, FixWorktree: true,
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: jobID, Agent: "fixer", Type: "implement", State: string(JobSucceeded), Repo: "owner/repo", Payload: payload,
	}, db.JobEvent{Kind: string(JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationWorktrees = &fakeWorktreeManager{err: errors.New("missing path must not reach Git manager")}

	reclaimed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ReclaimAgedTerminalDelegationWorktreeOutcome: %v", err)
	}
	if !reclaimed {
		t.Fatal("already-absent fix worktree did not complete reclaim")
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "delegation_worktree_reclaimed_ttl" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing reclaim event: %+v", events)
	}
}

func TestEngineReclaimAgedFixCloneRequiresPublishedObjectDatabase(t *testing.T) {
	for _, tc := range []struct {
		name            string
		clean           bool
		unpublished     string
		cleanErr        error
		remoteURLErr    error
		refreshErr      error
		cloneOnlyErr    error
		requireDeadline bool
		want            bool
		wantErr         string
		wantRetained    bool
		wantProved      bool
	}{
		{name: "dirty", clean: false},
		{name: "unpublished side branch", clean: true, unpublished: "deadbeef", wantRetained: true},
		{name: "clean probe error", cleanErr: errors.New("clean probe failed"), wantErr: "prove aged terminal fix worktree clean"},
		{name: "trusted remote url error", clean: true, remoteURLErr: errors.New("no origin"), wantErr: "resolve trusted remote url"},
		{name: "proof refresh error", clean: true, refreshErr: errors.New("fetch failed"), wantErr: "refresh trusted remote refs"},
		{name: "clone-only probe error", clean: true, cloneOnlyErr: errors.New("rev-list failed"), wantErr: "holds no unpublished commits"},
		// A fully proved clone is no longer deleted: it is handed to an operator, so
		// the pass reports "not reclaimed" and leaves the directory in place.
		{name: "fully published clone", clean: true, requireDeadline: true, wantProved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			home := t.TempDir()
			jobID := "fix-" + strings.ReplaceAll(tc.name, " ", "-")
			path, err := FixWorktreePath(home, "owner/repo", jobID)
			if err != nil {
				t.Fatalf("FixWorktreePath: %v", err)
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("MkdirAll fix worktree: %v", err)
			}
			payload, err := marshalPayload(JobPayload{
				Repo:         "owner/repo",
				Branch:       "feature/fix",
				WorktreePath: path,
				FixWorktree:  true,
			})
			if err != nil {
				t.Fatalf("marshalPayload: %v", err)
			}
			if err := store.CreateJobWithEvent(ctx, db.Job{
				ID:      jobID,
				Agent:   "fixer",
				Type:    "implement",
				State:   string(JobSucceeded),
				Repo:    "owner/repo",
				Payload: payload,
			}, db.JobEvent{Kind: string(JobSucceeded), Message: "seed"}); err != nil {
				t.Fatalf("CreateJobWithEvent: %v", err)
			}
			manager := &fakeWorktreeManager{
				cleanSet:         true,
				clean:            tc.clean,
				cleanErr:         tc.cleanErr,
				remoteURL:        "https://example.invalid/owner/repo.git",
				remoteURLErr:     tc.remoteURLErr,
				refreshErr:       tc.refreshErr,
				cloneOnlyErr:     tc.cloneOnlyErr,
				requireDeadline:  tc.requireDeadline,
				cloneOnlyDefault: tc.unpublished,
			}
			engine := testEngine(store)
			engine.Home = home
			engine.DelegationCheckout = t.TempDir()
			engine.DelegationWorktrees = manager

			reclaimed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, time.Now().Add(time.Hour))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ReclaimAgedTerminalDelegationWorktreeOutcome error = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("ReclaimAgedTerminalDelegationWorktreeOutcome: %v", err)
			}
			if reclaimed != tc.want {
				t.Fatalf("reclaimed = %v, want %v", reclaimed, tc.want)
			}
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("fix clone was removed; automatic removal is disabled: %v", statErr)
			}
			if tc.wantProved {
				events, eventsErr := store.ListJobEvents(ctx, jobID)
				if eventsErr != nil {
					t.Fatalf("ListJobEvents: %v", eventsErr)
				}
				proved := false
				for _, event := range events {
					if event.Kind == "delegation_worktree_reclaimable_manual" {
						proved = true
					}
				}
				if !proved {
					t.Fatalf("a fully proved clone did not reach the operator handoff: %+v", events)
				}
			}
			leftovers, quarantineErr := FixCloneQuarantines(path)
			if quarantineErr != nil || len(leftovers) != 0 {
				t.Fatalf("quarantines survived the pass: %v (err %v)", leftovers, quarantineErr)
			}
			if tc.wantProved && len(manager.refreshedURLs) == 0 {
				t.Fatal("removal proof never refreshed the trusted remote refs")
			}
			for _, url := range manager.refreshedURLs {
				if url != "https://example.invalid/owner/repo.git" {
					t.Fatalf("proof refreshed from %q, want the trusted checkout remote", url)
				}
			}
			obligation, obligationErr := store.GetCleanupObligation(ctx, db.CleanupObligationResourceID(jobID, path))
			if tc.wantRetained {
				if obligationErr != nil {
					t.Fatalf("GetCleanupObligation: %v", obligationErr)
				}
				if obligation.Reason != db.CleanupReasonUnpublishedCommits {
					t.Fatalf("obligation reason = %q, want %q", obligation.Reason, db.CleanupReasonUnpublishedCommits)
				}
				events, eventsErr := store.ListJobEvents(ctx, jobID)
				if eventsErr != nil {
					t.Fatalf("ListJobEvents: %v", eventsErr)
				}
				retained := 0
				for _, event := range events {
					if event.Kind == "delegation_worktree_retained_unpublished" {
						retained++
					}
				}
				if retained != 1 {
					t.Fatalf("retained events = %d, want exactly 1: %+v", retained, events)
				}
				// A second pass must not re-announce a reason that already holds.
				if _, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, time.Now().Add(time.Hour)); err != nil {
					t.Fatalf("second reclaim pass: %v", err)
				}
				events, eventsErr = store.ListJobEvents(ctx, jobID)
				if eventsErr != nil {
					t.Fatalf("ListJobEvents after second pass: %v", eventsErr)
				}
				retained = 0
				for _, event := range events {
					if event.Kind == "delegation_worktree_retained_unpublished" {
						retained++
					}
				}
				if retained != 1 {
					t.Fatalf("retained events after second pass = %d, want 1", retained)
				}
			}
		})
	}
}

// These regressions enter through the destructive production path. Git status
// can hide every input completely, but each carries a separate object database
// that the outer clone's reachability proof cannot inspect.
func TestEngineReclaimAgedFixCloneRetainsIgnoredNestedRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string) string
	}{
		{
			name: "global excludes nested repository",
			setup: func(t *testing.T, path string) string {
				excludes := filepath.Join(t.TempDir(), "global-excludes")
				if err := os.WriteFile(excludes, []byte("/repos/\n"), 0o644); err != nil {
					t.Fatalf("write global excludes: %v", err)
				}
				runWorktreeGit(t, path, "config", "core.excludesFile", excludes)
				nested := filepath.Join(path, "repos", "nested")
				if err := os.MkdirAll(nested, 0o755); err != nil {
					t.Fatalf("MkdirAll nested repository: %v", err)
				}
				runWorktreeGit(t, nested, "init", "-q", "-b", "main")
				runWorktreeGit(t, nested, "config", "user.email", "gitmoot@example.com")
				runWorktreeGit(t, nested, "config", "user.name", "Gitmoot")
				if err := os.WriteFile(filepath.Join(nested, "unique.txt"), []byte("only copy\n"), 0o644); err != nil {
					t.Fatalf("write nested work: %v", err)
				}
				runWorktreeGit(t, nested, "add", "unique.txt")
				runWorktreeGit(t, nested, "commit", "-q", "-m", "unique nested commit")
				return filepath.Join("repos", "nested", ".git")
			},
		},
		{
			name: "global excludes bare object database",
			setup: func(t *testing.T, path string) string {
				excludes := filepath.Join(t.TempDir(), "global-excludes")
				if err := os.WriteFile(excludes, []byte("/cache/\n"), 0o644); err != nil {
					t.Fatalf("write global excludes: %v", err)
				}
				runWorktreeGit(t, path, "config", "core.excludesFile", excludes)
				bare := filepath.Join(path, "cache", "objects.git")
				if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
					t.Fatalf("MkdirAll bare repository parent: %v", err)
				}
				runWorktreeGit(t, filepath.Dir(bare), "init", "-q", "--bare", bare)
				return filepath.Join("cache", "objects.git", "objects")
			},
		},
		{
			name: "global excludes loose-only custom object database",
			setup: func(t *testing.T, path string) string {
				excludes := filepath.Join(t.TempDir(), "global-excludes")
				if err := os.WriteFile(excludes, []byte("/cache/\n"), 0o644); err != nil {
					t.Fatalf("write global excludes: %v", err)
				}
				runWorktreeGit(t, path, "config", "core.excludesFile", excludes)
				objects := filepath.Join(path, "cache", "objects")
				if err := os.MkdirAll(objects, 0o755); err != nil {
					t.Fatalf("MkdirAll custom object database: %v", err)
				}
				gitEnv := []string{"GIT_OBJECT_DIRECTORY=" + objects}
				tree := runWorktreeGitEnvOutput(t, path, gitEnv, "", "mktree")
				commit := runWorktreeGitEnvOutput(t, path, gitEnv, "", "commit-tree", tree, "-m", "unique custom commit")
				if commit == "" {
					t.Fatal("commit-tree returned an empty object id")
				}
				if _, err := os.Stat(filepath.Join(objects, commit[:2], commit[2:])); err != nil {
					t.Fatalf("custom commit is not loose in the ignored object database: %v", err)
				}
				return filepath.Join("cache", "objects")
			},
		},
		{
			name: "global excludes packed custom object database",
			setup: func(t *testing.T, path string) string {
				excludes := filepath.Join(t.TempDir(), "global-excludes")
				if err := os.WriteFile(excludes, []byte("/vendor-cache/\n"), 0o644); err != nil {
					t.Fatalf("write global excludes: %v", err)
				}
				runWorktreeGit(t, path, "config", "core.excludesFile", excludes)
				objects := filepath.Join(path, "vendor-cache", "objects")
				if err := os.MkdirAll(objects, 0o755); err != nil {
					t.Fatalf("MkdirAll custom object database: %v", err)
				}
				gitEnv := []string{"GIT_OBJECT_DIRECTORY=" + objects}
				tree := runWorktreeGitEnvOutput(t, path, gitEnv, "", "mktree")
				commit := runWorktreeGitEnvOutput(t, path, gitEnv, "", "commit-tree", tree, "-m", "packed custom commit")
				runWorktreeGitEnvOutput(t, path, gitEnv, commit+"\n", "pack-objects", "--quiet", filepath.Join(objects, "pack", "pack"))
				// Every LOOSE object must go, or this case is also detectable by the
				// loose arm and stops discriminating the pack path. `pack-objects`
				// leaves the empty tree loose beside the packed commit.
				entries, err := os.ReadDir(objects)
				if err != nil {
					t.Fatalf("ReadDir custom object database: %v", err)
				}
				for _, entry := range entries {
					if entry.Name() == "pack" {
						continue
					}
					if err := os.RemoveAll(filepath.Join(objects, entry.Name())); err != nil {
						t.Fatalf("drop loose copy: %v", err)
					}
				}
				if remaining, err := os.ReadDir(objects); err != nil || len(remaining) != 1 {
					t.Fatalf("object database entries = %v (err %v), want only pack", remaining, err)
				}
				packs, err := filepath.Glob(filepath.Join(objects, "pack", "pack-*.pack"))
				if err != nil || len(packs) != 1 {
					t.Fatalf("packs = %v (err %v), want exactly one", packs, err)
				}
				return filepath.Join("vendor-cache", "objects")
			},
		},
		{
			name: "submodule ignore all",
			setup: func(t *testing.T, path string) string {
				submodule := filepath.Join(t.TempDir(), "submodule")
				runWorktreeGit(t, filepath.Dir(submodule), "init", "-q", "-b", "main", submodule)
				runWorktreeGit(t, submodule, "config", "user.email", "gitmoot@example.com")
				runWorktreeGit(t, submodule, "config", "user.name", "Gitmoot")
				if err := os.WriteFile(filepath.Join(submodule, "base.txt"), []byte("base\n"), 0o644); err != nil {
					t.Fatalf("write submodule base: %v", err)
				}
				runWorktreeGit(t, submodule, "add", "base.txt")
				runWorktreeGit(t, submodule, "commit", "-q", "-m", "submodule base")
				runWorktreeGit(t, path, "-c", "protocol.file.allow=always", "submodule", "add", "-q", submodule, "libs/sub")
				runWorktreeGit(t, path, "commit", "-q", "-am", "add submodule")
				runWorktreeGit(t, filepath.Join(path, "libs", "sub"), "config", "user.email", "gitmoot@example.com")
				runWorktreeGit(t, filepath.Join(path, "libs", "sub"), "config", "user.name", "Gitmoot")
				if err := os.WriteFile(filepath.Join(path, "libs", "sub", "local.txt"), []byte("only copy\n"), 0o644); err != nil {
					t.Fatalf("write submodule work: %v", err)
				}
				runWorktreeGit(t, filepath.Join(path, "libs", "sub"), "add", "local.txt")
				runWorktreeGit(t, filepath.Join(path, "libs", "sub"), "commit", "-q", "-m", "local-only submodule commit")
				runWorktreeGit(t, path, "config", "submodule.libs/sub.ignore", "all")
				return filepath.Join(".git", "modules", "libs", "sub", "objects")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			home := t.TempDir()
			jobID := "fix-ignored-" + strings.ReplaceAll(tc.name, " ", "-")
			path, err := FixWorktreePath(home, "owner/repo", jobID)
			if err != nil {
				t.Fatalf("FixWorktreePath: %v", err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("MkdirAll fix parent: %v", err)
			}
			runWorktreeGit(t, filepath.Dir(path), "init", "-q", "-b", "main", path)
			runWorktreeGit(t, path, "config", "user.email", "gitmoot@example.com")
			runWorktreeGit(t, path, "config", "user.name", "Gitmoot")
			if err := os.WriteFile(filepath.Join(path, "base.txt"), []byte("base\n"), 0o644); err != nil {
				t.Fatalf("write outer base: %v", err)
			}
			runWorktreeGit(t, path, "add", "base.txt")
			runWorktreeGit(t, path, "commit", "-q", "-m", "outer base")
			wantNested := tc.setup(t, path)
			remote := filepath.Join(home, jobID+"-origin.git")
			runWorktreeGit(t, home, "init", "-q", "--bare", remote)
			runWorktreeGit(t, path, "remote", "add", "origin", remote)
			runWorktreeGit(t, path, "push", "-q", "-u", "origin", "main")

			manager := gitutil.NewHostClient(path)
			clean, err := manager.WorktreeCleanAt(ctx, path)
			if err != nil || !clean {
				t.Fatalf("trigger is not hidden from outer status: clean=%v err=%v", clean, err)
			}
			nested, err := nestedGitObjectDatabase(ctx, path, gitPackVerifierForTest(t))
			if err != nil {
				t.Fatalf("nestedGitObjectDatabase: %v", err)
			}
			if nested != wantNested {
				t.Fatalf("nested object database = %q, want %q", nested, wantNested)
			}

			payload, err := marshalPayload(JobPayload{
				Repo: "owner/repo", Branch: "feature/fix", WorktreePath: path, FixWorktree: true,
			})
			if err != nil {
				t.Fatalf("marshalPayload: %v", err)
			}
			if err := store.CreateJobWithEvent(ctx, db.Job{
				ID: jobID, Agent: "fixer", Type: "implement", State: string(JobSucceeded),
				Repo: "owner/repo", Payload: payload,
			}, db.JobEvent{Kind: string(JobSucceeded), Message: "seed"}); err != nil {
				t.Fatalf("CreateJobWithEvent: %v", err)
			}
			engine := testEngine(store)
			engine.Home = home
			engine.DelegationCheckout = path
			engine.DelegationWorktrees = manager
			reclaimed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatalf("ReclaimAgedTerminalDelegationWorktreeOutcome: %v", err)
			}
			if reclaimed {
				t.Fatal("ignored nested repository was reclaimed")
			}
			if _, err := os.Stat(filepath.Join(path, wantNested)); err != nil {
				t.Fatalf("nested repository was not retained: %v", err)
			}
			obligation, err := store.GetCleanupObligation(ctx, db.CleanupObligationResourceID(jobID, path))
			if err != nil {
				t.Fatalf("GetCleanupObligation: %v", err)
			}
			if obligation.Reason != db.CleanupReasonUnpublishedCommits {
				t.Fatalf("obligation reason = %q, want %q", obligation.Reason, db.CleanupReasonUnpublishedCommits)
			}
		})
	}
}

// Git-SHAPED is not Git: a cache entry can carry a well-formed object header or
// the PACK magic and still hold nothing recoverable. Retaining on those keeps
// every clone with such a cache forever, which is the inert-pass failure the
// content check exists to avoid.
func TestNestedGitObjectDatabaseRejectsCorruptGitShapedCaches(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{
			name: "loose object declaring more bytes than it carries",
			setup: func(t *testing.T, root string) {
				writeZlibFile(t, filepath.Join(root, "cache", "ab", strings.Repeat("c", 38)), "blob 999999\x00x")
			},
		},
		{
			name: "loose object whose bytes do not hash to its name",
			setup: func(t *testing.T, root string) {
				writeZlibFile(t, filepath.Join(root, "cache", "ab", strings.Repeat("c", 38)), "blob 5\x00hello")
			},
		},
		{
			name: "pack file holding only the magic",
			setup: func(t *testing.T, root string) {
				name := filepath.Join(root, "cache", "pack", "pack-"+strings.Repeat("a", 40)+".pack")
				if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
					t.Fatalf("MkdirAll pack directory: %v", err)
				}
				if err := os.WriteFile(name, []byte("PACK"), 0o644); err != nil {
					t.Fatalf("write pack: %v", err)
				}
			},
		},
		{
			name: "pack file with no index beside it",
			setup: func(t *testing.T, root string) {
				name := filepath.Join(root, "cache", "pack", "pack-"+strings.Repeat("b", 40)+".pack")
				if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
					t.Fatalf("MkdirAll pack directory: %v", err)
				}
				body := append([]byte("PACK"), 0, 0, 0, 2, 0, 0, 0, 1)
				body = append(body, make([]byte, 32)...)
				if err := os.WriteFile(name, body, 0o644); err != nil {
					t.Fatalf("write pack: %v", err)
				}
			},
		},
		{
			name: "indexed pack with an unsupported version",
			setup: func(t *testing.T, root string) {
				writePackFile(t, root, strings.Repeat("c", 40), 7, 1)
			},
		},
		{
			name: "indexed pack carrying no objects",
			setup: func(t *testing.T, root string) {
				writePackFile(t, root, strings.Repeat("d", 40), 2, 0)
			},
		},
		{
			name: "pack whose index names a different pack",
			setup: func(t *testing.T, root string) {
				// Both files verify on their own; only the cross-check catches this.
				writeVerifiablePack(t, root, strings.Repeat("e", 40), false)
			},
		},
		{
			name: "pack with a corrupt trailing checksum",
			setup: func(t *testing.T, root string) {
				writeVerifiablePack(t, root, strings.Repeat("0", 40), true)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			nested, err := nestedGitObjectDatabase(ctx, root, gitPackVerifierForTest(t))
			if err != nil {
				t.Fatalf("nestedGitObjectDatabase: %v", err)
			}
			if nested != "" {
				t.Fatalf("corrupt Git-shaped cache classified as object database %q", nested)
			}
		})
	}
}

// writePackFile lays down an INDEXED pack of the requested version and object
// count, sized past the header-plus-checksum floor, so the only thing under test
// is the structural check rather than the size or index preconditions.
func writePackFile(t *testing.T, root, hashName string, version, objects uint32) {
	t.Helper()
	name := filepath.Join(root, "cache", "pack", "pack-"+hashName+".pack")
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatalf("MkdirAll pack directory: %v", err)
	}
	body := make([]byte, 0, 12+1+20)
	body = append(body, "PACK"...)
	body = binary.BigEndian.AppendUint32(body, version)
	body = binary.BigEndian.AppendUint32(body, objects)
	body = append(body, make([]byte, 1+20)...)
	if err := os.WriteFile(name, body, 0o644); err != nil {
		t.Fatalf("write pack: %v", err)
	}
	if err := os.WriteFile(strings.TrimSuffix(name, ".pack")+".idx", []byte("idx"), 0o644); err != nil {
		t.Fatalf("write pack index: %v", err)
	}
}

// writeVerifiablePack lays down a pack and index whose OWN checksums verify, so
// the only thing under test is the cross-check between them (and, with
// corruptPack, the pack self-check).
func writeVerifiablePack(t *testing.T, root, hashName string, corruptPack bool) {
	t.Helper()
	name := filepath.Join(root, "cache", "pack", "pack-"+hashName+".pack")
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatalf("MkdirAll pack directory: %v", err)
	}
	body := make([]byte, 0, 12+1)
	body = append(body, "PACK"...)
	body = binary.BigEndian.AppendUint32(body, 2)
	body = binary.BigEndian.AppendUint32(body, 1)
	body = append(body, 0x00)
	packDigest := sha1.Sum(body)
	pack := append(append([]byte{}, body...), packDigest[:]...)
	if corruptPack {
		pack[len(pack)-1] ^= 0xff
	}
	if err := os.WriteFile(name, pack, 0o644); err != nil {
		t.Fatalf("write pack: %v", err)
	}
	// A v2 index that verifies but records a DIFFERENT pack digest.
	index := make([]byte, 0, 8+1024+40)
	index = append(index, 0xff, 0x74, 0x4f, 0x63)
	index = binary.BigEndian.AppendUint32(index, 2)
	index = append(index, make([]byte, 1024)...)
	recorded := sha1.Sum([]byte("a different pack"))
	if corruptPack {
		// Record the pack's ACTUAL (corrupted) trailer, so the cross-check agrees
		// and only the pack's own checksum can reject the file.
		copy(recorded[:], pack[len(pack)-sha1.Size:])
	}
	index = append(index, recorded[:]...)
	indexDigest := sha1.Sum(index)
	index = append(index, indexDigest[:]...)
	if err := os.WriteFile(strings.TrimSuffix(name, ".pack")+".idx", index, 0o644); err != nil {
		t.Fatalf("write pack index: %v", err)
	}
}

func writeZlibFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	var deflated bytes.Buffer
	writer := zlib.NewWriter(&deflated)
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatalf("deflate %s: %v", path, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close deflater: %v", err)
	}
	if err := os.WriteFile(path, deflated.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A malformed loose-object candidate must not be able to size the daemon's
// allocation: the header read is bounded, while a VALID object of any size still
// streams through the hash.
func TestLooseObjectHeaderReadIsBounded(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	// 1 MiB of NUL-free content behind a hex-fanout name: no header terminator
	// inside the bound, so recognition rejects it without buffering the stream.
	writeZlibFile(t, filepath.Join(root, "cache", "ab", strings.Repeat("c", 38)), strings.Repeat("x", 1<<20))
	nested, err := nestedGitObjectDatabase(ctx, root, gitPackVerifierForTest(t))
	if err != nil {
		t.Fatalf("nestedGitObjectDatabase: %v", err)
	}
	if nested != "" {
		t.Fatalf("unterminated header classified as object database %q", nested)
	}
	// The bound is a RESOURCE guard, so the observable is how much it consumed: a
	// reader that drains a NUL-free stream is exactly the failure mode.
	stream := strings.NewReader(strings.Repeat("x", 1<<20))
	if _, err := readBoundedGitObjectHeader(bufio.NewReader(stream)); err == nil {
		t.Fatal("readBoundedGitObjectHeader accepted an unterminated header")
	}
	// One bufio refill (4 KiB) is the floor any buffered reader pays; draining the
	// whole megabyte is the defect. 64 KiB separates the two without pinning the
	// buffer size.
	const consumedLimit = 64 << 10
	if consumed := int64(1<<20) - int64(stream.Len()); consumed > consumedLimit {
		t.Fatalf("header read consumed %d bytes of a NUL-free stream, want at most %d", consumed, consumedLimit)
	}

	// A large VALID blob still streams: the bound applies to the header only.
	body := strings.Repeat("payload\n", 1<<14)
	header := fmt.Sprintf("blob %d\x00", len(body))
	digest := sha1.Sum([]byte(header + body))
	name := hex.EncodeToString(digest[:])
	writeZlibFile(t, filepath.Join(root, "objects", name[:2], name[2:]), header+body)
	nested, err = nestedGitObjectDatabase(ctx, root, gitPackVerifierForTest(t))
	if err != nil {
		t.Fatalf("nestedGitObjectDatabase after the valid object: %v", err)
	}
	if nested != "objects" {
		t.Fatalf("valid streamed object database = %q, want objects", nested)
	}
}

// The inverse of the nested-object-database retention: ordinary ignored
// content-addressed build output uses exactly the same hex-fanout and pack
// NAMING as a Git object database. Classifying it as Git data retains every
// clone with a content-addressed cache, which makes the whole pass inert, so
// recognition reads the candidate's bytes instead of trusting its name.
func TestEngineReclaimAgedFixCloneReclaimsIgnoredHexFanoutOutput(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	jobID := "fix-hex-fanout-output"
	path, err := FixWorktreePath(home, "owner/repo", jobID)
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll fix parent: %v", err)
	}
	runWorktreeGit(t, filepath.Dir(path), "init", "-q", "-b", "main", path)
	runWorktreeGit(t, path, "config", "user.email", "gitmoot@example.com")
	runWorktreeGit(t, path, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(path, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write outer base: %v", err)
	}
	runWorktreeGit(t, path, "add", "base.txt")
	runWorktreeGit(t, path, "commit", "-q", "-m", "outer base")
	excludes := filepath.Join(t.TempDir(), "global-excludes")
	if err := os.WriteFile(excludes, []byte("/build/\n"), 0o644); err != nil {
		t.Fatalf("write global excludes: %v", err)
	}
	runWorktreeGit(t, path, "config", "core.excludesFile", excludes)
	// A cache keyed by content hash: SHA-1-shaped and SHA-256-shaped loose names
	// under a two-hex fanout, plus a file using the pack naming convention.
	for _, cached := range []string{
		filepath.Join("build", "ab", strings.Repeat("c", 38)),
		filepath.Join("build", "de", strings.Repeat("f", 62)),
		filepath.Join("build", "pack", "pack-"+strings.Repeat("a", 40)+".pack"),
	} {
		absolute := filepath.Join(path, cached)
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatalf("MkdirAll cache directory: %v", err)
		}
		if err := os.WriteFile(absolute, []byte("ordinary build output\n"), 0o644); err != nil {
			t.Fatalf("write cached artifact: %v", err)
		}
	}
	// The discriminating input: a ZLIB-COMPRESSED cache entry in the same layout.
	// Rejecting it needs the decompressed header, not the deflate framing that
	// every Git object also has.
	compressed := filepath.Join(path, "build", "0a", strings.Repeat("1", 38))
	if err := os.MkdirAll(filepath.Dir(compressed), 0o755); err != nil {
		t.Fatalf("MkdirAll compressed cache directory: %v", err)
	}
	var deflated bytes.Buffer
	deflater := zlib.NewWriter(&deflated)
	if _, err := deflater.Write([]byte("cachev2 payload not a git object")); err != nil {
		t.Fatalf("deflate cached artifact: %v", err)
	}
	if err := deflater.Close(); err != nil {
		t.Fatalf("close deflater: %v", err)
	}
	if err := os.WriteFile(compressed, deflated.Bytes(), 0o644); err != nil {
		t.Fatalf("write compressed cached artifact: %v", err)
	}
	remote := filepath.Join(home, jobID+"-origin.git")
	runWorktreeGit(t, home, "init", "-q", "--bare", remote)
	runWorktreeGit(t, path, "remote", "add", "origin", remote)
	runWorktreeGit(t, path, "push", "-q", "-u", "origin", "main")

	manager := gitutil.NewHostClient(path)
	if clean, err := manager.WorktreeCleanAt(ctx, path); err != nil || !clean {
		t.Fatalf("ignored cache is not hidden from status: clean=%v err=%v", clean, err)
	}
	nested, err := nestedGitObjectDatabase(ctx, path, gitPackVerifierForTest(t))
	if err != nil {
		t.Fatalf("nestedGitObjectDatabase: %v", err)
	}
	if nested != "" {
		t.Fatalf("ordinary build output classified as Git object database %q", nested)
	}

	payload, err := marshalPayload(JobPayload{
		Repo: "owner/repo", Branch: "feature/fix", WorktreePath: path, FixWorktree: true,
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: jobID, Agent: "fixer", Type: "implement", State: string(JobSucceeded),
		Repo: "owner/repo", Payload: payload,
	}, db.JobEvent{Kind: string(JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = path
	engine.DelegationWorktrees = manager
	reclaimed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ReclaimAgedTerminalDelegationWorktreeOutcome: %v", err)
	}
	// Automatic removal is disabled, so "not inert" now means the pass reaches the
	// PROVED-DISPOSABLE outcome instead of retaining for a content reason: the
	// clone is handed to an operator, and nothing is deleted.
	if reclaimed {
		t.Fatal("the pass deleted a fix clone; automatic removal is disabled")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("proved clone was removed: %v", err)
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	proved := false
	for _, event := range events {
		if event.Kind == "delegation_worktree_reclaimable_manual" {
			proved = true
		}
		if event.Kind == "delegation_worktree_retained_unpublished" {
			t.Fatalf("ordinary build output was classified as unpublished Git data: %s", event.Message)
		}
	}
	if !proved {
		t.Fatalf("clone holding only ignored build output never reached the proved-disposable outcome: %+v", events)
	}
}

// A managed path may contain glob metacharacters (a home or repo directory named
// with brackets). Discovery must not silently return "no quarantine" there: every
// caller reads that as a completed removal.
func TestFixCloneQuarantinesFindsPathsWithGlobMetacharacters(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "home[a]", "worktrees", "owner--repo", "fixes")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("MkdirAll parent: %v", err)
	}
	path := filepath.Join(parent, "job-1")
	quarantine := path + fixCloneQuarantinePrefix + "0123456789abcdef"
	if err := os.MkdirAll(quarantine, 0o755); err != nil {
		t.Fatalf("MkdirAll quarantine: %v", err)
	}
	// A sibling clone must not be mistaken for this clone's quarantine.
	if err := os.MkdirAll(filepath.Join(parent, "job-2"+fixCloneQuarantinePrefix+"ff"), 0o755); err != nil {
		t.Fatalf("MkdirAll sibling quarantine: %v", err)
	}
	quarantines, err := FixCloneQuarantines(path)
	if err != nil {
		t.Fatalf("FixCloneQuarantines: %v", err)
	}
	if len(quarantines) != 1 || quarantines[0] != quarantine {
		t.Fatalf("quarantines = %v, want [%s]", quarantines, quarantine)
	}
	if found, err := FixCloneQuarantines(filepath.Join(parent, "job-3")); err != nil || len(found) != 0 {
		t.Fatalf("quarantines for an untouched clone = %v (err %v), want none", found, err)
	}
}

// The pristine check includes ignored files, so a clone holding build output is
// the retention most hosts will actually hit. It must record why, like the other
// retention branches: a silent keep is indistinguishable from a live worker.
func TestEngineReclaimAgedFixCloneRecordsDirtyRetention(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	const jobID = "fix-dirty-retention"
	path, err := FixWorktreePath(home, "owner/repo", jobID)
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll fix worktree: %v", err)
	}
	payload, err := marshalPayload(JobPayload{
		Repo:         "owner/repo",
		Branch:       "feature/fix",
		WorktreePath: path,
		FixWorktree:  true,
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      jobID,
		Agent:   "fixer",
		Type:    "implement",
		State:   string(JobSucceeded),
		Repo:    "owner/repo",
		Payload: payload,
	}, db.JobEvent{Kind: string(JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = &fakeWorktreeManager{cleanSet: true, clean: false}

	for attempt := 0; attempt < 2; attempt++ {
		reclaimed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("ReclaimAgedTerminalDelegationWorktreeOutcome: %v", err)
		}
		if reclaimed {
			t.Fatal("dirty fix clone was reclaimed")
		}
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	dirty := 0
	for _, event := range events {
		if event.Kind == "delegation_worktree_retained_dirty" {
			dirty++
		}
	}
	if dirty != 1 {
		t.Fatalf("dirty retention events = %d, want exactly 1: %+v", dirty, events)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("dirty fix clone was removed: %v", statErr)
	}
}

// Every outcome of the pass should be attributable from the job log alone, and a
// retention reason is recorded once per job even when the offending detail changes.
func TestEngineReclaimAgedFixCloneRecordsLiveRetentionOncePerReason(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	const jobID = "fix-live-retention"
	path, err := FixWorktreePath(home, "owner/repo", jobID)
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll fix worktree: %v", err)
	}
	payload, err := marshalPayload(JobPayload{
		Repo:         "owner/repo",
		Branch:       "feature/fix",
		WorktreePath: path,
		FixWorktree:  true,
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      jobID,
		Agent:   "fixer",
		Type:    "implement",
		State:   string(JobSucceeded),
		Repo:    "owner/repo",
		Payload: payload,
	}, db.JobEvent{Kind: string(JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = &fakeWorktreeManager{cleanSet: true, clean: true}
	engine.WorktreeHasLiveProcess = func(string) bool { return true }

	for attempt := 0; attempt < 3; attempt++ {
		reclaimed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("ReclaimAgedTerminalDelegationWorktreeOutcome: %v", err)
		}
		if reclaimed {
			t.Fatal("live fix clone was reclaimed")
		}
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	live := 0
	for _, event := range events {
		if event.Kind == "delegation_worktree_retained_live" {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("live retention events = %d, want exactly 1: %+v", live, events)
	}
}

// The ordinary (non-TTL) fix-clone cleanup runs on every terminal advance and on
// the skipped-cleanup pass, so it must make the same inference as the TTL pass:
// an absent path is not a completed removal while a quarantine of it survives.
func TestEngineCleanupFixWorktreeKeepsObligationOpenWhileQuarantined(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	const jobID = "fix-cleanup-quarantined"
	path, err := FixWorktreePath(home, "owner/repo", jobID)
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	quarantine := path + fixCloneQuarantinePrefix + "00112233445566ff"
	if err := os.MkdirAll(quarantine, 0o755); err != nil {
		t.Fatalf("MkdirAll quarantine: %v", err)
	}
	payload, err := marshalPayload(JobPayload{
		Repo:         "owner/repo",
		Branch:       "feature/fix",
		WorktreePath: path,
		FixWorktree:  true,
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      jobID,
		Agent:   "fixer",
		Type:    "implement",
		State:   string(JobSucceeded),
		Repo:    "owner/repo",
		Payload: payload,
	}, db.JobEvent{Kind: string(JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	engine := testEngine(store)
	engine.Home = home

	if _, err := engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, jobID); err != nil {
		t.Fatalf("ReclaimTerminalDelegationWorktreeOutcome: %v", err)
	}
	obligation, err := store.GetCleanupObligation(ctx, db.CleanupObligationResourceID(jobID, path))
	if err != nil {
		t.Fatalf("GetCleanupObligation: %v", err)
	}
	if obligation.State == db.CleanupObligationRemoved {
		t.Fatalf("obligation = %+v, want it open while %s holds the clone", obligation, quarantine)
	}
	if _, statErr := os.Stat(quarantine); statErr != nil {
		t.Fatalf("quarantined clone was removed: %v", statErr)
	}
}

// An unreadable process table retains every clone forever. That must be visible:
// a silent keep is indistinguishable from a worker that is genuinely running.
func TestEngineReclaimAgedFixCloneRecordsInconclusiveLiveness(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	const jobID = "fix-liveness-unknown"
	path, err := FixWorktreePath(home, "owner/repo", jobID)
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll fix worktree: %v", err)
	}
	payload, err := marshalPayload(JobPayload{
		Repo:         "owner/repo",
		Branch:       "feature/fix",
		WorktreePath: path,
		FixWorktree:  true,
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      jobID,
		Agent:   "fixer",
		Type:    "implement",
		State:   string(JobSucceeded),
		Repo:    "owner/repo",
		Payload: payload,
	}, db.JobEvent{Kind: string(JobSucceeded), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = &fakeWorktreeManager{cleanSet: true, clean: true}
	engine.WorktreeHasLiveProcess = nil
	engine.WorktreeLiveness = func(string) (bool, bool) { return false, false }

	for attempt := 0; attempt < 2; attempt++ {
		reclaimed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("ReclaimAgedTerminalDelegationWorktreeOutcome: %v", err)
		}
		if reclaimed {
			t.Fatal("clone with unprovable liveness was reclaimed")
		}
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	unknown := 0
	for _, event := range events {
		if event.Kind == "delegation_worktree_liveness_unknown" {
			unknown++
		}
	}
	if unknown != 1 {
		t.Fatalf("liveness-unknown events = %d, want exactly 1: %+v", unknown, events)
	}
}

func setupOffLineageTaskWorktree(t *testing.T, dirty bool) (context.Context, *db.Store, Engine, gitutil.Client, TaskWorktreeRequest, string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("MkdirAll checkout: %v", err)
	}
	runWorktreeGit(t, root, "init", "--bare", remote)
	runWorktreeGit(t, checkout, "init", "-b", "main")
	runWorktreeGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runWorktreeGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	runWorktreeGit(t, checkout, "add", "base.txt")
	runWorktreeGit(t, checkout, "commit", "-m", "base")
	runWorktreeGit(t, checkout, "remote", "add", "origin", remote)
	runWorktreeGit(t, checkout, "push", "-u", "origin", "main")

	home := filepath.Join(root, "home")
	path, err := TaskWorktreePath(home, "owner/repo", "task-1")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll worktree parent: %v", err)
	}
	runWorktreeGit(t, checkout, "worktree", "add", "-b", "task-1", path, "HEAD")
	if err := os.WriteFile(filepath.Join(path, "stale.txt"), []byte("committed stale work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile stale: %v", err)
	}
	runWorktreeGit(t, path, "add", "stale.txt")
	runWorktreeGit(t, path, "commit", "-m", "stale branch")
	oldHead, err := (gitutil.NewHostClient(path)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA stale worktree: %v", err)
	}

	if err := os.WriteFile(filepath.Join(checkout, "current.txt"), []byte("current base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile current: %v", err)
	}
	runWorktreeGit(t, checkout, "add", "current.txt")
	runWorktreeGit(t, checkout, "commit", "-m", "advance base")
	runWorktreeGit(t, checkout, "push", "origin", "main")
	baseHead, err := (gitutil.NewHostClient(checkout)).RevParse(ctx, "origin/main")
	if err != nil {
		t.Fatalf("RevParse origin/main: %v", err)
	}
	if dirty {
		if err := os.WriteFile(filepath.Join(path, "salvage.txt"), []byte("salvage me\n"), 0o644); err != nil {
			t.Fatalf("WriteFile salvage: %v", err)
		}
	}

	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-1",
		RepoFullName: "owner/repo",
		State:        string(TaskImplementing),
		Branch:       "task-1",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	request := TaskWorktreeRequest{
		Home:       home,
		Repo:       "owner/repo",
		TaskID:     "task-1",
		Branch:     "task-1",
		BaseBranch: "origin/main",
		Owner:      "lead",
		Checkout:   checkout,
	}
	return ctx, store, testEngine(store), gitutil.NewHostClient(checkout), request, path, oldHead, baseHead
}

func runWorktreeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func runWorktreeGitEnvOutput(t *testing.T, dir string, env []string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

type fakeWorktreeManager struct {
	err               error
	onAdd             func()
	existingBranches  map[string]bool
	fetchedRemotes    []string
	pathHeads         map[string]string
	revHeads          map[string]string
	ancestor          bool
	ancestorSet       bool
	clean             bool
	cleanSet          bool
	cleanErr          error
	remoteURL         string
	remoteURLErr      error
	refreshErr        error
	requireDeadline   bool
	refreshedPaths    []string
	refreshedURLs     []string
	cloneOnly         map[string]string // path -> unpublished sha ("" proves published)
	cloneOnlyDefault  string            // answer for paths absent from cloneOnly
	cloneOnlyErr      error
	verifiedPacks     []string
	verifyPackErr     error
	verifyPackInvalid bool
	cloneOnlyCalls    []string
	cloneOnlyHook     func(path string) // mutate the clone between proof rounds
	cleanCalls        []string
	ancestorCalls     [][2]string
	calls             []worktreeCall
	existingCalls     []worktreeCall
	detachedCalls     []worktreeCall // AddDetachedWorktree: path in .path, ref in .base
	removed           []string       // RemoveWorktree paths
	removedForce      []string       // RemoveWorktreeForce paths
	removeErr         error
	deletedBranches   []string // DeleteBranch branches
	deleteErr         error
	mergeCalls        []mergeCall // MergeBranches calls
	mergeErr          error
	committedDirs     []string // CommitWorktree dirs
	commitMade        bool     // value CommitWorktree returns for "committed"
	commitErr         error
}

type mergeCall struct {
	dir      string
	branches []string
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
	if f.pathHeads != nil {
		f.pathHeads[path] = base
	}
	return f.err
}

func (f *fakeWorktreeManager) AddExistingBranchWorktree(_ context.Context, branch string, path string) error {
	if f.onAdd != nil {
		f.onAdd()
	}
	f.existingCalls = append(f.existingCalls, worktreeCall{branch: branch, path: path})
	return f.err
}

func (f *fakeWorktreeManager) BranchExists(_ context.Context, branch string) (bool, error) {
	return f.existingBranches[branch], nil
}

func (f *fakeWorktreeManager) FetchRemote(_ context.Context, remote string) error {
	f.fetchedRemotes = append(f.fetchedRemotes, remote)
	return nil
}

func (f *fakeWorktreeManager) HeadSHAAt(_ context.Context, path string) (string, error) {
	if head := f.pathHeads[path]; head != "" {
		return head, nil
	}
	return "existing-head", nil
}

func (f *fakeWorktreeManager) RevParse(_ context.Context, rev string) (string, error) {
	if head := f.revHeads[rev]; head != "" {
		return head, nil
	}
	return rev + "-head", nil
}

func (f *fakeWorktreeManager) IsAncestor(_ context.Context, ancestor, descendant string) (bool, error) {
	f.ancestorCalls = append(f.ancestorCalls, [2]string{ancestor, descendant})
	if f.ancestorSet {
		return f.ancestor, nil
	}
	return true, nil
}

func (f *fakeWorktreeManager) WorktreeCleanAt(_ context.Context, path string) (bool, error) {
	f.cleanCalls = append(f.cleanCalls, path)
	if f.cleanErr != nil {
		return false, f.cleanErr
	}
	if f.cleanSet {
		return f.clean, nil
	}
	return true, nil
}

func (f *fakeWorktreeManager) WorktreePristineAt(ctx context.Context, path string) (bool, error) {
	return f.WorktreeCleanAt(ctx, path)
}

func (f *fakeWorktreeManager) RemoteURL(_ context.Context, remote string) (string, error) {
	if f.remoteURLErr != nil {
		return "", f.remoteURLErr
	}
	if f.remoteURL != "" {
		return f.remoteURL, nil
	}
	return "https://example.invalid/" + remote + ".git", nil
}

func (f *fakeWorktreeManager) RefreshCloneProofRefs(ctx context.Context, path string, remoteURL string) error {
	if f.requireDeadline {
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("trusted remote refresh has no deadline")
		}
	}
	f.refreshedPaths = append(f.refreshedPaths, path)
	f.refreshedURLs = append(f.refreshedURLs, remoteURL)
	return f.refreshErr
}

// VerifyPackIndex mirrors Git's pack validation. The fake accepts by default and
// records what it was asked to verify, so a test can prove the production path
// consults it and can make it refuse.
func (f *fakeWorktreeManager) VerifyPackIndex(_ context.Context, indexPath string, objectFormat string) (bool, error) {
	f.verifiedPacks = append(f.verifiedPacks, indexPath+" "+objectFormat)
	if f.verifyPackErr != nil {
		return false, f.verifyPackErr
	}
	return !f.verifyPackInvalid, nil
}

func (f *fakeWorktreeManager) CloneOnlyCommit(_ context.Context, path string) (string, error) {
	f.cloneOnlyCalls = append(f.cloneOnlyCalls, path)
	if f.cloneOnlyHook != nil {
		f.cloneOnlyHook(path)
	}
	if f.cloneOnlyErr != nil {
		return "", f.cloneOnlyErr
	}
	if sha, ok := f.cloneOnly[path]; ok {
		return sha, nil
	}
	return f.cloneOnlyDefault, nil
}

func (f *fakeWorktreeManager) AddDetachedWorktree(_ context.Context, path string, ref string) error {
	if f.onAdd != nil {
		f.onAdd()
	}
	f.detachedCalls = append(f.detachedCalls, worktreeCall{path: path, base: ref})
	return f.err
}

func (f *fakeWorktreeManager) MergeBranches(_ context.Context, dir string, branches []string, _ string) error {
	f.mergeCalls = append(f.mergeCalls, mergeCall{dir: dir, branches: branches})
	return f.mergeErr
}

func (f *fakeWorktreeManager) CommitWorktree(_ context.Context, dir string, _ string) (bool, error) {
	f.committedDirs = append(f.committedDirs, dir)
	return f.commitMade, f.commitErr
}

func (f *fakeWorktreeManager) RemoveWorktreeForce(_ context.Context, path string) error {
	f.removedForce = append(f.removedForce, path)
	return f.removeErr
}

func (f *fakeWorktreeManager) RemoveWorktree(_ context.Context, path string) error {
	f.removed = append(f.removed, path)
	return f.removeErr
}

func (f *fakeWorktreeManager) DeleteBranch(_ context.Context, branch string) error {
	f.deletedBranches = append(f.deletedBranches, branch)
	return f.deleteErr
}

func TestTaskWorktreePathSegmentSanitizesSlashedIDs(t *testing.T) {
	// Backward-compatible: already-safe values are returned unchanged so existing
	// worktree paths never move.
	for _, v := range []string{"local-ask-lead-abc123", "task-1", "owner_repo", "a.b-c_d"} {
		got, err := taskWorktreePathSegment(v, "x")
		if err != nil {
			t.Fatalf("safe value %q errored: %v", v, err)
		}
		if got != v {
			t.Fatalf("safe value %q changed to %q (must be byte-identical)", v, got)
		}
	}

	// A coordinator continuation parent id (contains '/') must no longer error;
	// it sanitizes to a single, path-safe, traversal-safe, deterministic segment.
	id := "local-ask-lead-abc123/continuation/continuation"
	got, err := taskWorktreePathSegment(id, "parent job id")
	if err != nil {
		t.Fatalf("slashed continuation id errored (the bug this fixes): %v", err)
	}
	if strings.ContainsAny(got, `/\`) {
		t.Fatalf("sanitized segment %q still contains a path separator", got)
	}
	if got == "." || got == ".." || strings.HasPrefix(got, ".") {
		t.Fatalf("sanitized segment %q is not traversal-safe", got)
	}
	if again, _ := taskWorktreePathSegment(id, "parent job id"); got != again {
		t.Fatalf("not deterministic: %q vs %q", got, again)
	}

	// DelegationWorktreePath (the real caller) now succeeds for a slashed parent.
	p, err := DelegationWorktreePath("/h", "o/r", id, "impl", 0)
	if err != nil {
		t.Fatalf("DelegationWorktreePath rejected slashed parent (the bug): %v", err)
	}
	if !strings.Contains(p, got) {
		t.Fatalf("path %q missing sanitized parent segment %q", p, got)
	}

	// Distinct unsafe ids that collapse to the same prefix must NOT collide.
	a, _ := taskWorktreePathSegment("x/y", "p")
	b, _ := taskWorktreePathSegment("x:y", "p")
	if a == b {
		t.Fatalf("distinct unsafe ids collided: both -> %q", a)
	}

	// "." and ".." sanitize to a safe segment rather than being usable as traversal.
	for _, dotted := range []string{".", ".."} {
		seg, err := taskWorktreePathSegment(dotted, "p")
		if err != nil {
			t.Fatalf("%q errored: %v", dotted, err)
		}
		if seg == "." || seg == ".." || strings.ContainsAny(seg, `/\`) {
			t.Fatalf("%q produced unsafe segment %q", dotted, seg)
		}
	}

	// Empty / whitespace still errors.
	if _, err := taskWorktreePathSegment("   ", "p"); err == nil {
		t.Fatalf("blank value should still error")
	}
}

// gitPackVerifierForTest runs the REAL git verification the production path uses,
// so a recognition test is not silently weaker than the daemon. Without git on the
// host it returns nil, which is the conservative fallback production also takes.
func gitPackVerifierForTest(t *testing.T) packIndexVerifier {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	client := gitutil.NewHostClient(t.TempDir())
	return client.VerifyPackIndex
}
