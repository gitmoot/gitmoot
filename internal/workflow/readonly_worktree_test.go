package workflow

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"database/sql"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestReadOnlyFanoutNeedsWorktree(t *testing.T) {
	ask := Delegation{ID: "a", Action: "ask", Prompt: "p"}
	ask2 := Delegation{ID: "b", Action: "ask", Prompt: "p"}
	review := Delegation{ID: "c", Action: "review", Prompt: "p"}
	impl := Delegation{ID: "d", Action: "implement", Prompt: "p"}

	cases := []struct {
		name        string
		delegations []Delegation
		target      Delegation
		want        bool
	}{
		{"two ask siblings", []Delegation{ask, ask2}, ask, true},
		{"ask + review (both read-only)", []Delegation{ask, review}, review, true},
		{"single ask", []Delegation{ask}, ask, false},
		{"implement target never isolated", []Delegation{impl, ask, ask2}, impl, false},
		{"one ask among implements", []Delegation{impl, ask}, ask, false},
		{"two ask plus implement", []Delegation{ask, ask2, impl}, ask, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := JobPayload{Result: &AgentResult{Delegations: tc.delegations}}
			if got := readOnlyFanoutNeedsWorktree(payload, tc.target); got != tc.want {
				t.Fatalf("readOnlyFanoutNeedsWorktree = %v, want %v", got, tc.want)
			}
		})
	}

	// Nil result must not panic and must report no fan-out.
	if readOnlyFanoutNeedsWorktree(JobPayload{}, ask) {
		t.Fatal("readOnlyFanoutNeedsWorktree with nil result = true, want false")
	}
}

func TestIsReadOnlyDelegationWorktree(t *testing.T) {
	base := JobPayload{DelegationID: "d1", WorktreePath: "/wt/d1"}
	if !isReadOnlyDelegationWorktree("ask", base) {
		t.Fatal("ask delegation worktree child not detected")
	}
	if !isReadOnlyDelegationWorktree("review", base) {
		t.Fatal("review delegation worktree child not detected")
	}
	if isReadOnlyDelegationWorktree("implement", base) {
		t.Fatal("implement child must not be treated as a read-only worktree")
	}
	if isReadOnlyDelegationWorktree("ask", JobPayload{WorktreePath: "/wt/d1"}) {
		t.Fatal("non-delegation job (no delegation id) must not match")
	}
	if isReadOnlyDelegationWorktree("ask", JobPayload{DelegationID: "d1"}) {
		t.Fatal("delegation child without a worktree path must not match")
	}
}

func TestAllocateReadOnlyDelegationWorktreeDetachedNoBranchLock(t *testing.T) {
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
			t.Fatalf("GetResourceLock during AddDetachedWorktree returned error: %v", err)
		}
		if lock.OwnerJobID != "worktree:job-1/d1" {
			t.Fatalf("checkout lock owner = %q, want worktree:job-1/d1", lock.OwnerJobID)
		}
	}}

	path, err := engine.AllocateReadOnlyDelegationWorktree(ctx, DelegationWorktreeRequest{
		Home:         home,
		Repo:         "owner/repo",
		ParentJobID:  "job-1",
		DelegationID: "d1",
		Delegation:   Delegation{ID: "d1", Agent: "audit", Action: "ask"},
		BaseBranch:   "main",
		Checkout:     checkout,
	}, manager)
	if err != nil {
		t.Fatalf("AllocateReadOnlyDelegationWorktree returned error: %v", err)
	}
	wantPath := filepath.Join(home, "worktrees", "owner--repo", "delegations", "job-1", "d1")
	if path != wantPath {
		t.Fatalf("path = %q, want %q", path, wantPath)
	}
	if len(manager.detachedCalls) != 1 || manager.detachedCalls[0].path != wantPath || manager.detachedCalls[0].base != "main" {
		t.Fatalf("detached calls = %+v, want one at %q ref main", manager.detachedCalls, wantPath)
	}
	// A read-only worktree must create no branch and take no branch lock.
	if len(manager.calls) != 0 {
		t.Fatalf("read-only allocation must not call AddWorktree (branch path): %+v", manager.calls)
	}
	wantBranch := delegationBranchName(Delegation{ID: "d1"}, "job-1", "d1", 0)
	if _, err := store.GetBranchLock(ctx, "owner/repo", wantBranch); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read-only allocation must not create a branch lock; GetBranchLock err = %v", err)
	}
	// The checkout mutation lock must be released after allocation.
	if _, err := store.GetResourceLock(ctx, key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("checkout lock after AddDetachedWorktree err = %v, want sql.ErrNoRows", err)
	}
}

func TestAllocateReadOnlyDelegationWorktreeDefaultsRefToHEAD(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	manager := &fakeWorktreeManager{}
	if _, err := engine.AllocateReadOnlyDelegationWorktree(ctx, DelegationWorktreeRequest{
		Home:         t.TempDir(),
		Repo:         "owner/repo",
		ParentJobID:  "job-1",
		DelegationID: "d1",
		Delegation:   Delegation{ID: "d1", Agent: "audit", Action: "ask"},
		BaseBranch:   "",
		Checkout:     t.TempDir(),
	}, manager); err != nil {
		t.Fatalf("AllocateReadOnlyDelegationWorktree returned error: %v", err)
	}
	if len(manager.detachedCalls) != 1 || manager.detachedCalls[0].base != "HEAD" {
		t.Fatalf("detached calls = %+v, want ref HEAD when base empty", manager.detachedCalls)
	}
}

func TestCleanupReadOnlyDelegationWorktreeForceRemoves(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	manager := &fakeWorktreeManager{}
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	payload := JobPayload{DelegationID: "d1", WorktreePath: "/wt/d1"}
	engine.cleanupReadOnlyDelegationWorktree(ctx, "job-1/delegation/d1", "ask", payload)
	if len(manager.removedForce) != 1 || manager.removedForce[0] != "/wt/d1" {
		t.Fatalf("removedForce = %+v, want one force-remove of /wt/d1", manager.removedForce)
	}
	if got := countJobEvents(t, store, "job-1/delegation/d1", "delegation_worktree_removed"); got != 1 {
		t.Fatalf("delegation_worktree_removed event count = %d, want 1", got)
	}

	// No-op for an implement child (cleaned via the merge gate, not here).
	manager.removedForce = nil
	engine.cleanupReadOnlyDelegationWorktree(ctx, "job-2", "implement", payload)
	// No-op for a non-delegation job and for a child without a worktree path.
	engine.cleanupReadOnlyDelegationWorktree(ctx, "job-3", "ask", JobPayload{WorktreePath: "/wt/x"})
	engine.cleanupReadOnlyDelegationWorktree(ctx, "job-4", "ask", JobPayload{DelegationID: "d2"})
	if len(manager.removedForce) != 0 {
		t.Fatalf("cleanup must be a no-op for non read-only worktree children: %+v", manager.removedForce)
	}
}

func TestDispatchDelegationsTwoReadOnlySiblingsGetSeparateDetachedWorktrees(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit-a", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit-b", []string{"ask"}, "jerryfane/gitmoot")
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "d1", Agent: "audit-a", Action: "ask", Prompt: "audit one"},
				{ID: "d2", Agent: "audit-b", Action: "ask", Prompt: "audit two"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	payloadOne, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/d1").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(d1) returned error: %v", err)
	}
	payloadTwo, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/d2").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(d2) returned error: %v", err)
	}

	wantPathOne := filepath.Join(home, "worktrees", "jerryfane--gitmoot", "delegations", "parent-job", "d1")
	wantPathTwo := filepath.Join(home, "worktrees", "jerryfane--gitmoot", "delegations", "parent-job", "d2")
	if payloadOne.WorktreePath != wantPathOne {
		t.Fatalf("d1 worktree path = %q, want %q", payloadOne.WorktreePath, wantPathOne)
	}
	if payloadTwo.WorktreePath != wantPathTwo {
		t.Fatalf("d2 worktree path = %q, want %q", payloadTwo.WorktreePath, wantPathTwo)
	}
	if payloadOne.WorktreePath == payloadTwo.WorktreePath {
		t.Fatalf("read-only siblings share worktree path %q -> would serialize on the same checkout key", payloadOne.WorktreePath)
	}
	// Read-only children are detached: no branch is created, so they keep the
	// inherited parent branch (unlike implement children, which are rebranded).
	if payloadOne.Branch != "task-005" || payloadTwo.Branch != "task-005" {
		t.Fatalf("read-only children must keep the parent branch: d1=%q d2=%q", payloadOne.Branch, payloadTwo.Branch)
	}
	// HeadSHA cleared so validateTargetCheckout validates the fresh worktree HEAD.
	if payloadOne.HeadSHA != "" || payloadTwo.HeadSHA != "" {
		t.Fatalf("read-only worktree children must not inherit parent HeadSHA: d1=%q d2=%q", payloadOne.HeadSHA, payloadTwo.HeadSHA)
	}
	// Two detached worktrees, no branch-creating AddWorktree calls.
	if len(manager.detachedCalls) != 2 {
		t.Fatalf("detached worktree calls = %+v, want two", manager.detachedCalls)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("read-only fan-out must not call AddWorktree (branch path): %+v", manager.calls)
	}
	for _, c := range manager.detachedCalls {
		if c.base != "task-005" {
			t.Fatalf("detached worktree ref = %q, want parent branch task-005", c.base)
		}
	}
}

func TestDispatchDelegationsSingleReadOnlyDelegationStaysInSharedCheckout(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "d1", Agent: "audit", Action: "ask", Prompt: "audit one"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	payload, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/d1").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if strings.TrimSpace(payload.WorktreePath) != "" {
		t.Fatalf("single read-only delegation must stay in the shared checkout, got worktree %q", payload.WorktreePath)
	}
	if len(manager.detachedCalls) != 0 {
		t.Fatalf("single read-only delegation must not allocate a worktree: %+v", manager.detachedCalls)
	}
}

func TestDispatchDelegationsReadOnlyFanoutWithoutManagerEmitsSkippedEvent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit-a", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit-b", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	// No engine.Home / engine.DelegationWorktrees: detached isolation unavailable.

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "d1", Agent: "audit-a", Action: "ask", Prompt: "audit one"},
				{ID: "d2", Agent: "audit-b", Action: "ask", Prompt: "audit two"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	for _, id := range []string{"parent-job/delegation/d1", "parent-job/delegation/d2"} {
		payload, err := unmarshalPayload(mustJob(t, store, id).Payload)
		if err != nil {
			t.Fatalf("unmarshalPayload(%s) returned error: %v", id, err)
		}
		if strings.TrimSpace(payload.WorktreePath) != "" {
			t.Fatalf("fallback child %s unexpectedly got worktree path %q", id, payload.WorktreePath)
		}
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_worktree_skipped"); got != 2 {
		t.Fatalf("delegation_worktree_skipped event count = %d, want 2", got)
	}
}
