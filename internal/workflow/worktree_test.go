package workflow

import (
	"context"
	"database/sql"
	"errors"
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
	if clean, err := manager.WorktreeCleanAt(ctx, path); err != nil || clean {
		t.Fatalf("ignored-content WorktreeCleanAt = %v, err=%v, want false nil", clean, err)
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
			manager := &fakeWorktreeManager{}
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
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	manager := &fakeWorktreeManager{removeErr: fakeTerminalWorktreeRemovalError{}}
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

func TestEngineReclaimAgedFixWorktreeRequiresCleanRemoteReachableHead(t *testing.T) {
	for _, tc := range []struct {
		name         string
		clean        bool
		reachable    bool
		cleanErr     error
		reachableErr error
		want         bool
		wantErr      string
	}{
		{name: "dirty", clean: false, reachable: true},
		{name: "unpushed head", clean: true, reachable: false},
		{name: "clean probe error", cleanErr: errors.New("clean probe failed"), wantErr: "prove aged terminal fix worktree clean"},
		{name: "reachability probe error", clean: true, reachableErr: errors.New("reachability probe failed"), wantErr: "head reachable from remote"},
		{name: "clean pushed head", clean: true, reachable: true, want: true},
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
				cleanSet:     true,
				clean:        tc.clean,
				cleanErr:     tc.cleanErr,
				reachableErr: tc.reachableErr,
				ancestorSet:  true,
				ancestor:     tc.reachable,
			}
			engine := testEngine(store)
			engine.Home = home
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
			_, statErr := os.Stat(path)
			if tc.want && !os.IsNotExist(statErr) {
				t.Fatalf("eligible fix worktree remains: %v", statErr)
			}
			if !tc.want && statErr != nil {
				t.Fatalf("ineligible fix worktree was removed: %v", statErr)
			}
		})
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

type fakeWorktreeManager struct {
	err              error
	onAdd            func()
	existingBranches map[string]bool
	fetchedRemotes   []string
	pathHeads        map[string]string
	revHeads         map[string]string
	ancestor         bool
	ancestorSet      bool
	clean            bool
	cleanSet         bool
	cleanErr         error
	reachableErr     error
	cleanCalls       []string
	ancestorCalls    [][2]string
	calls            []worktreeCall
	existingCalls    []worktreeCall
	detachedCalls    []worktreeCall // AddDetachedWorktree: path in .path, ref in .base
	removed          []string       // RemoveWorktree paths
	removedForce     []string       // RemoveWorktreeForce paths
	removeErr        error
	deletedBranches  []string // DeleteBranch branches
	deleteErr        error
	mergeCalls       []mergeCall // MergeBranches calls
	mergeErr         error
	committedDirs    []string // CommitWorktree dirs
	commitMade       bool     // value CommitWorktree returns for "committed"
	commitErr        error
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

func (f *fakeWorktreeManager) WorktreeHeadReachableFromRemote(ctx context.Context, path string, branch string) (bool, error) {
	if f.reachableErr != nil {
		return false, f.reachableErr
	}
	head, err := f.HeadSHAAt(ctx, path)
	if err != nil {
		return false, err
	}
	remoteHead, err := f.RevParse(ctx, "origin/"+branch)
	if err != nil {
		return false, err
	}
	return f.IsAncestor(ctx, head, remoteHead)
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
