package workflow

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestEngineStartTaskBranchCreatesBranchAndLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	brancher := &fakeBranchCreator{}

	task, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:       "jerryfane/gitmoot",
		GoalID:     "goal-1",
		TaskID:     "task-8",
		TaskTitle:  "Branch Rules",
		Branch:     "task-8",
		BaseBranch: "main",
		Owner:      "lead",
	}, brancher)

	if err != nil {
		t.Fatalf("StartTaskBranch returned error: %v", err)
	}
	if task.ID != "task-8" || task.Branch != "task-8" || task.State != string(TaskImplementing) {
		t.Fatalf("task = %+v", task)
	}
	lock, err := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-8")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.Owner != "lead" {
		t.Fatalf("lock owner = %q, want lead", lock.Owner)
	}
	if len(brancher.calls) != 1 || brancher.calls[0].branch != "task-8" || brancher.calls[0].base != "main" {
		t.Fatalf("branch calls = %+v", brancher.calls)
	}
}

func TestEngineStartTaskBranchAcquiresCheckoutMutationLockBeforeBranchSetup(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	checkout := t.TempDir()
	key, err := checkoutMutationLockKey(checkout)
	if err != nil {
		t.Fatalf("checkoutMutationLockKey returned error: %v", err)
	}
	brancher := &fakeBranchCreator{onCreate: func() {
		lock, err := store.GetResourceLock(ctx, key)
		if err != nil {
			t.Fatalf("GetResourceLock during branch setup returned error: %v", err)
		}
		if lock.OwnerJobID != "task:task-8" {
			t.Fatalf("checkout lock owner = %q, want task:task-8", lock.OwnerJobID)
		}
	}}

	if _, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:     "jerryfane/gitmoot",
		TaskID:   "task-8",
		Branch:   "task-8",
		Owner:    "lead",
		Checkout: checkout,
	}, brancher); err != nil {
		t.Fatalf("StartTaskBranch returned error: %v", err)
	}
	if _, err := store.GetResourceLock(ctx, key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("checkout lock after branch setup error = %v, want sql.ErrNoRows", err)
	}
}

func TestEngineStartTaskBranchBlocksWhenCheckoutMutationLocked(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	checkout := t.TempDir()
	key, err := checkoutMutationLockKey(checkout)
	if err != nil {
		t.Fatalf("checkoutMutationLockKey returned error: %v", err)
	}
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  "task:other",
		OwnerToken:  "other-token",
		ExpiresAt:   "2099-01-01T00:00:00Z",
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	brancher := &fakeBranchCreator{}

	_, err = engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:     "jerryfane/gitmoot",
		TaskID:   "task-8",
		Branch:   "task-8",
		Owner:    "lead",
		Checkout: checkout,
	}, brancher)

	var blocked BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != checkoutMutationBusyMessage {
		t.Fatalf("error = %v, want checkout busy BlockedError", err)
	}
	if len(brancher.calls) != 0 {
		t.Fatalf("branch was created despite checkout lock: %+v", brancher.calls)
	}
}

func TestEngineStartTaskBranchReleasesLockOnBranchCreateFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	brancher := &fakeBranchCreator{err: errors.New("git failed")}

	_, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-8",
		Branch: "task-8",
		Owner:  "lead",
	}, brancher)

	if err == nil {
		t.Fatal("StartTaskBranch succeeded despite branch failure")
	}
	if _, lockErr := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-8"); !errors.Is(lockErr, sql.ErrNoRows) {
		t.Fatalf("lock after failure error = %v, want sql.ErrNoRows", lockErr)
	}
}

func TestEngineStartTaskBranchReleasesCheckoutMutationLockOnBranchCreateFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	checkout := t.TempDir()
	key, err := checkoutMutationLockKey(checkout)
	if err != nil {
		t.Fatalf("checkoutMutationLockKey returned error: %v", err)
	}
	brancher := &fakeBranchCreator{err: errors.New("git failed")}

	_, err = engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:     "jerryfane/gitmoot",
		TaskID:   "task-8",
		Branch:   "task-8",
		Owner:    "lead",
		Checkout: checkout,
	}, brancher)

	if err == nil {
		t.Fatal("StartTaskBranch succeeded despite branch failure")
	}
	if _, err := store.GetResourceLock(ctx, key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("checkout lock after branch setup failure error = %v, want sql.ErrNoRows", err)
	}
}

func TestEngineStartTaskBranchPreservesExistingTaskMetadata(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-8",
		RepoFullName: "jerryfane/gitmoot",
		GoalID:       "goal-1",
		Title:        "Branch Rules",
		State:        string(TaskPlanned),
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	engine := testEngine(store)

	task, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-8",
		Branch: "task-8",
		Owner:  "lead",
	}, &fakeBranchCreator{})

	if err != nil {
		t.Fatalf("StartTaskBranch returned error: %v", err)
	}
	if task.GoalID != "goal-1" || task.Title != "Branch Rules" {
		t.Fatalf("task metadata = goal %q title %q", task.GoalID, task.Title)
	}
}

func TestEngineStartTaskBranchPreservesExistingSameOwnerLockOnFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-8", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	brancher := &fakeBranchCreator{err: errors.New("git failed")}

	_, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-8",
		Branch: "task-8",
		Owner:  "lead",
	}, brancher)

	if err == nil {
		t.Fatal("StartTaskBranch succeeded despite branch failure")
	}
	lock, lockErr := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-8")
	if lockErr != nil {
		t.Fatalf("GetBranchLock returned error: %v", lockErr)
	}
	if lock.Owner != "lead" {
		t.Fatalf("lock owner = %q, want lead", lock.Owner)
	}
}

func TestEngineStartTaskBranchBlocksWhenBranchLocked(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-8", Owner: "other"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}

	_, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-8",
		Branch: "task-8",
		Owner:  "lead",
	}, &fakeBranchCreator{})

	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v, want BlockedError", err)
	}
}

func TestEngineStartTaskBranchRejectsBranchAssignedToOtherTask(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-existing", GoalID: "goal-1", Title: "Existing", State: string(TaskPlanned), Branch: "task-8"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	brancher := &fakeBranchCreator{}

	_, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:   "jerryfane/gitmoot",
		TaskID: "task-8",
		Branch: "task-8",
		Owner:  "lead",
	}, brancher)

	if err == nil || !strings.Contains(err.Error(), "another task") {
		t.Fatalf("error = %v, want branch assignment error", err)
	}
	if len(brancher.calls) != 0 {
		t.Fatalf("branch was created despite assignment conflict: %+v", brancher.calls)
	}
}

func TestEngineStartTaskBranchAllowsSameBranchInAnotherRepo(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-existing", RepoFullName: "jerryfane/other", GoalID: "goal-1", Title: "Existing", State: string(TaskPlanned), Branch: "task-8"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	engine := testEngine(store)
	brancher := &fakeBranchCreator{}

	task, err := engine.StartTaskBranch(ctx, TaskBranchRequest{
		Repo:      "jerryfane/gitmoot",
		GoalID:    "goal-1",
		TaskID:    "task-8",
		TaskTitle: "Task 8",
		Branch:    "task-8",
		Owner:     "lead",
	}, brancher)
	if err != nil {
		t.Fatalf("StartTaskBranch returned error: %v", err)
	}
	if task.RepoFullName != "jerryfane/gitmoot" {
		t.Fatalf("task repo = %q, want jerryfane/gitmoot", task.RepoFullName)
	}
}

func TestDelegationBranchNameAlwaysNamespaced(t *testing.T) {
	short := parentShort("parent-job")
	wantPrefix := "gitmoot-delegation-" + short + "-"

	// No worktree hint: the branch is the bare namespaced form.
	plain := delegationBranchName(Delegation{ID: "d1"}, "parent-job", "d1", 0)
	if plain != wantPrefix+"d1" {
		t.Fatalf("plain branch = %q, want %q", plain, wantPrefix+"d1")
	}

	// A worktree hint is appended only as a suffix, never replacing the namespace.
	hinted := delegationBranchName(Delegation{ID: "d1", Worktree: "Feature Login"}, "parent-job", "d1", 0)
	if hinted != wantPrefix+"d1-feature-login" {
		t.Fatalf("hinted branch = %q, want %q", hinted, wantPrefix+"d1-feature-login")
	}

	// Two sibling delegations that share an identical worktree hint must still get
	// distinct branches because each carries its own delegation id in the name.
	siblingA := delegationBranchName(Delegation{ID: "api", Worktree: "shared"}, "parent-job", "api", 0)
	siblingB := delegationBranchName(Delegation{ID: "ui", Worktree: "shared"}, "parent-job", "ui", 0)
	if siblingA == siblingB {
		t.Fatalf("siblings with identical worktree hint share branch %q", siblingA)
	}

	// Two siblings with empty worktree hints are likewise distinct.
	emptyA := delegationBranchName(Delegation{ID: "api"}, "parent-job", "api", 0)
	emptyB := delegationBranchName(Delegation{ID: "ui"}, "parent-job", "ui", 0)
	if emptyA == emptyB {
		t.Fatalf("siblings with empty worktree hint share branch %q", emptyA)
	}

	// A retry of the same delegation gets a distinct -retry-<n> branch so it never
	// reuses the failed original attempt's still-checked-out branch.
	original := delegationBranchName(Delegation{ID: "d1"}, "parent-job", "d1", 0)
	retry := delegationBranchName(Delegation{ID: "d1"}, "parent-job", "d1", 2)
	if retry != original+"-retry-2" {
		t.Fatalf("retry branch = %q, want %q", retry, original+"-retry-2")
	}
	if retry == original {
		t.Fatalf("retry branch %q collides with original attempt", retry)
	}

	// Determinism: the same inputs always produce the same branch.
	if again := delegationBranchName(Delegation{ID: "d1", Worktree: "Feature Login"}, "parent-job", "d1", 0); again != hinted {
		t.Fatalf("delegationBranchName not deterministic: %q != %q", again, hinted)
	}
}

type fakeBranchCreator struct {
	err      error
	onCreate func()
	calls    []branchCall
}

type branchCall struct {
	branch string
	base   string
}

func (f *fakeBranchCreator) CreateBranch(_ context.Context, branch string, base string) error {
	if f.onCreate != nil {
		f.onCreate()
	}
	f.calls = append(f.calls, branchCall{branch: branch, base: base})
	return f.err
}
