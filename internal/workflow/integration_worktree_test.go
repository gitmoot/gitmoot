package workflow

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestAllocateIntegrationWorktree(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	home := t.TempDir()
	checkout := t.TempDir()
	key, err := checkoutMutationLockKey(checkout)
	if err != nil {
		t.Fatalf("checkoutMutationLockKey returned error: %v", err)
	}
	manager := &fakeWorktreeManager{}

	path, err := engine.AllocateIntegrationWorktree(ctx, DelegationWorktreeRequest{
		Home:         home,
		Repo:         "owner/repo",
		ParentJobID:  "job-1",
		DelegationID: "verify",
		BaseBranch:   "main",
		Checkout:     checkout,
	}, []string{"branchA", "branchB"}, manager)
	if err != nil {
		t.Fatalf("AllocateIntegrationWorktree returned error: %v", err)
	}
	wantPath := filepath.Join(home, "worktrees", "owner--repo", "delegations", "job-1", "integration-verify")
	if path != wantPath {
		t.Fatalf("path = %q, want %q", path, wantPath)
	}
	if len(manager.detachedCalls) != 1 || manager.detachedCalls[0].path != wantPath || manager.detachedCalls[0].base != "main" {
		t.Fatalf("detached calls = %+v, want one detached worktree at %q off main", manager.detachedCalls, wantPath)
	}
	if len(manager.mergeCalls) != 1 || manager.mergeCalls[0].dir != wantPath || !reflect.DeepEqual(manager.mergeCalls[0].branches, []string{"branchA", "branchB"}) {
		t.Fatalf("merge calls = %+v, want one merge of [branchA branchB] in %q", manager.mergeCalls, wantPath)
	}
	// No branch lock is created (detached), and the checkout mutation lock is released.
	if _, err := store.GetResourceLock(ctx, key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("checkout lock after integration err = %v, want sql.ErrNoRows", err)
	}
}

func TestAllocateIntegrationWorktreeBlocksOnMergeConflict(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	manager := &fakeWorktreeManager{mergeErr: errors.New("CONFLICT (content): file.txt")}

	_, err := engine.AllocateIntegrationWorktree(ctx, DelegationWorktreeRequest{
		Home:         t.TempDir(),
		Repo:         "owner/repo",
		ParentJobID:  "job-1",
		DelegationID: "verify",
		BaseBranch:   "main",
		Checkout:     t.TempDir(),
	}, []string{"branchA", "branchB"}, manager)
	if err == nil {
		t.Fatal("AllocateIntegrationWorktree accepted a conflicting merge")
	}
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v, want BlockedError so the parent blocks rather than auto-resolving", err)
	}

	// Empty leg branches is a programming error, not a block.
	if _, err := engine.AllocateIntegrationWorktree(ctx, DelegationWorktreeRequest{
		Home: t.TempDir(), Repo: "owner/repo", ParentJobID: "job-1", DelegationID: "verify", Checkout: t.TempDir(),
	}, nil, manager); err == nil {
		t.Fatal("AllocateIntegrationWorktree accepted zero leg branches")
	}
}

func TestIntegrationDepBranches(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)

	// Two succeeded implement legs (distinct branches), one succeeded read-only dep
	// (no branch), and one implement leg that ran in the shared checkout (branch ==
	// base, so it is already on base and skipped).
	insertCompletedJob(t, store, db.Job{ID: "p/delegation/legA", Agent: "builder", Type: "implement"},
		JobPayload{Repo: "owner/repo", Branch: "branchA", DelegationID: "legA"})
	insertCompletedJob(t, store, db.Job{ID: "p/delegation/legB", Agent: "builder", Type: "implement"},
		JobPayload{Repo: "owner/repo", Branch: "branchB", DelegationID: "legB"})
	insertCompletedJob(t, store, db.Job{ID: "p/delegation/note", Agent: "noter", Type: "ask"},
		JobPayload{Repo: "owner/repo", Branch: "task-x", DelegationID: "note"})
	insertCompletedJob(t, store, db.Job{ID: "p/delegation/legBase", Agent: "builder", Type: "implement"},
		JobPayload{Repo: "owner/repo", Branch: "task-x", DelegationID: "legBase"})

	parentJob := db.Job{ID: "p", Agent: "coord", Type: "ask"}
	parentPayload := JobPayload{
		Repo:   "owner/repo",
		Branch: "task-x",
		Result: &AgentResult{Delegations: []Delegation{
			{ID: "legA", Action: "implement"},
			{ID: "legB", Action: "implement"},
			{ID: "note", Action: "ask"},
			{ID: "legBase", Action: "implement"},
		}},
	}
	verify := Delegation{ID: "verify", Action: "review", Deps: []string{"legA", "legB", "note", "legBase"}}

	branches, err := engine.integrationDepBranches(ctx, parentJob, parentPayload, verify)
	if err != nil {
		t.Fatalf("integrationDepBranches returned error: %v", err)
	}
	if !reflect.DeepEqual(branches, []string{"branchA", "branchB"}) {
		t.Fatalf("branches = %v, want [branchA branchB] (read-only and base-branch deps skipped)", branches)
	}

	// A delegation with no deps yields no integration.
	none, err := engine.integrationDepBranches(ctx, parentJob, parentPayload, Delegation{ID: "x", Action: "review"})
	if err != nil {
		t.Fatalf("integrationDepBranches(no deps) returned error: %v", err)
	}
	if none != nil {
		t.Fatalf("no-deps delegation = %v, want nil", none)
	}
}

func TestAllocateAndEnqueueDelegationRoutesVerifyToIntegrationWorktree(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "p/delegation/legA", Agent: "builder", Type: "implement"},
		JobPayload{Repo: "owner/repo", Branch: "branchA", DelegationID: "legA"})
	insertCompletedJob(t, store, db.Job{ID: "p/delegation/legB", Agent: "builder", Type: "implement"},
		JobPayload{Repo: "owner/repo", Branch: "branchB", DelegationID: "legB"})

	parentJob := db.Job{ID: "p", Agent: "coord", Type: "ask"}
	parentPayload := JobPayload{
		Repo:   "owner/repo",
		Branch: "task-x",
		Result: &AgentResult{Delegations: []Delegation{
			{ID: "legA", Action: "implement"},
			{ID: "legB", Action: "implement"},
			{ID: "verify", Action: "review", Deps: []string{"legA", "legB"}},
		}},
	}
	verify := Delegation{ID: "verify", Agent: "checker", Action: "review", Prompt: "verify the combined work", Deps: []string{"legA", "legB"}}
	request := engine.delegationRequest(parentJob, parentPayload, verify)

	if err := engine.allocateAndEnqueueDelegation(ctx, parentJob, parentPayload, verify, request, taskRefFromPayload(parentPayload)); err != nil {
		t.Fatalf("allocateAndEnqueueDelegation returned error: %v", err)
	}

	child, err := unmarshalPayload(mustJob(t, store, "p/delegation/verify").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	wantPath := filepath.Join(home, "worktrees", "owner--repo", "delegations", "p", "integration-verify")
	if child.WorktreePath != wantPath {
		t.Fatalf("verify worktree path = %q, want integration worktree %q", child.WorktreePath, wantPath)
	}
	if child.HeadSHA != "" {
		t.Fatalf("verify HeadSHA = %q, want cleared", child.HeadSHA)
	}
	if len(manager.mergeCalls) != 1 || !reflect.DeepEqual(manager.mergeCalls[0].branches, []string{"branchA", "branchB"}) {
		t.Fatalf("merge calls = %+v, want one merge of the two leg branches", manager.mergeCalls)
	}
}
