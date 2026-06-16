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
	manager := &fakeWorktreeManager{}

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
	path, err := DelegationWorktreePath("/home/gitmoot", "owner/repo", "job-1", "d1")
	if err != nil {
		t.Fatalf("DelegationWorktreePath returned error: %v", err)
	}
	want := filepath.Join("/home/gitmoot", "worktrees", "owner--repo", "delegations", "job-1", "d1")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
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
		{name: "unsafe parent", home: "/home/gitmoot", repo: "owner/repo", parentJob: "../job", delegation: "d1"},
		{name: "unsafe delegation", home: "/home/gitmoot", repo: "owner/repo", parentJob: "job-1", delegation: "../d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DelegationWorktreePath(tc.home, tc.repo, tc.parentJob, tc.delegation); err == nil {
				t.Fatal("DelegationWorktreePath accepted invalid input")
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
	wantBranch := delegationBranchName(Delegation{ID: "d1"}, "job-1", "d1")
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
	if hinted.Branch != "feature-login" {
		t.Fatalf("hinted branch = %q, want feature-login", hinted.Branch)
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
	branch := delegationBranchName(Delegation{ID: "d1"}, "job-1", "d1")
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

func TestAllocateDelegationWorktreeReleasesBranchLockOnFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	manager := &fakeWorktreeManager{err: errors.New("git failed")}
	branch := delegationBranchName(Delegation{ID: "d1"}, "job-1", "d1")

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

type fakeWorktreeManager struct {
	err              error
	onAdd            func()
	existingBranches map[string]bool
	calls            []worktreeCall
	existingCalls    []worktreeCall
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
