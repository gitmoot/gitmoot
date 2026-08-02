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
	newHead, err := (gitutil.Client{Dir: path}).HeadSHA(ctx)
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
	headAfter, err := (gitutil.Client{Dir: path}).HeadSHA(ctx)
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
	oldHead, err := (gitutil.Client{Dir: path}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA stale worktree: %v", err)
	}

	if err := os.WriteFile(filepath.Join(checkout, "current.txt"), []byte("current base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile current: %v", err)
	}
	runWorktreeGit(t, checkout, "add", "current.txt")
	runWorktreeGit(t, checkout, "commit", "-m", "advance base")
	runWorktreeGit(t, checkout, "push", "origin", "main")
	baseHead, err := (gitutil.Client{Dir: checkout}).RevParse(ctx, "origin/main")
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
	return ctx, store, testEngine(store), gitutil.Client{Dir: checkout}, request, path, oldHead, baseHead
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
	if f.cleanSet {
		return f.clean, nil
	}
	return true, nil
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
