package workflow

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
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

func TestCommitDelegationLeg(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	manager := &fakeWorktreeManager{commitMade: true}
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "p/delegation/legA", ParentJobID: "p", DelegationID: "legA", Type: "implement"},
		JobPayload{Repo: "owner/repo", Branch: "branchA", DelegationID: "legA", WorktreePath: "/wt/legA"})
	job := mustJob(t, store, "p/delegation/legA")
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}

	if err := engine.commitDelegationLeg(ctx, job, payload); err != nil {
		t.Fatalf("commitDelegationLeg returned error: %v", err)
	}
	if len(manager.committedDirs) != 1 || manager.committedDirs[0] != "/wt/legA" {
		t.Fatalf("committedDirs = %v, want [/wt/legA]", manager.committedDirs)
	}
	if got := countJobEvents(t, store, "p/delegation/legA", "delegation_committed"); got != 1 {
		t.Fatalf("delegation_committed events = %d, want 1", got)
	}

	// No-op for a job without a delegation worktree.
	manager.committedDirs = nil
	if err := engine.commitDelegationLeg(ctx, db.Job{ID: "x"}, JobPayload{WorktreePath: "/wt/x"}); err != nil {
		t.Fatalf("commitDelegationLeg(non-delegation) returned error: %v", err)
	}
	if err := engine.commitDelegationLeg(ctx, db.Job{ID: "y", DelegationID: "y"}, JobPayload{DelegationID: "y"}); err != nil {
		t.Fatalf("commitDelegationLeg(no worktree) returned error: %v", err)
	}
	if len(manager.committedDirs) != 0 {
		t.Fatalf("commit must be a no-op without a delegation worktree: %v", manager.committedDirs)
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

	insertCompletedJob(t, store, db.Job{ID: "p/delegation/legA", Agent: "builder", Type: "implement", ParentJobID: "p", DelegationID: "legA"},
		JobPayload{Repo: "owner/repo", Branch: "branchA", DelegationID: "legA"})
	insertCompletedJob(t, store, db.Job{ID: "p/delegation/legB", Agent: "builder", Type: "implement", ParentJobID: "p", DelegationID: "legB"},
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

func TestAllocateAndEnqueueDelegationAllowsImplementDepAlreadyOnBase(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "p/delegation/legA", Agent: "builder", Type: "implement", ParentJobID: "p", DelegationID: "legA"},
		JobPayload{Repo: "owner/repo", Branch: "task-x", DelegationID: "legA"})

	parentJob := db.Job{ID: "p", Agent: "coord", Type: "ask"}
	parentPayload := JobPayload{
		Repo:   "owner/repo",
		Branch: "task-x",
		Result: &AgentResult{Delegations: []Delegation{
			{ID: "legA", Action: "implement"},
			{ID: "verify", Action: "review", Deps: []string{"legA"}},
		}},
	}
	verify := Delegation{ID: "verify", Agent: "checker", Action: "review", Prompt: "verify the base work", Deps: []string{"legA"}}
	request := engine.delegationRequest(parentJob, parentPayload, verify)

	if err := engine.allocateAndEnqueueDelegation(ctx, parentJob, parentPayload, verify, request, taskRefFromPayload(parentPayload)); err != nil {
		t.Fatalf("allocateAndEnqueueDelegation returned error: %v", err)
	}
	child, err := unmarshalPayload(mustJob(t, store, "p/delegation/verify").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if child.Branch != "task-x" {
		t.Fatalf("verify branch = %q, want parent branch task-x", child.Branch)
	}
	if child.WorktreePath != "" {
		t.Fatalf("verify worktree path = %q, want shared checkout", child.WorktreePath)
	}
	if len(manager.mergeCalls) != 0 {
		t.Fatalf("merge calls = %+v, want none for already-on-base implement dep", manager.mergeCalls)
	}
	if got := countJobEvents(t, store, "p", "delegation_integration_unresolved"); got != 0 {
		t.Fatalf("delegation_integration_unresolved events = %d, want 0", got)
	}
}

// TestAllocateAndEnqueueDelegationBlocksWhenImplementLegUnresolved covers the #19
// fix: a read-only delegation that DEPENDS on an implement leg must NOT silently
// fall through to the base checkout when no leg branch resolves (e.g. the leg's
// branch was not readable under store contention). Reviewing base would judge code
// without the implemented change. It must fail closed (block) instead.
func TestAllocateAndEnqueueDelegationBlocksWhenImplementLegUnresolved(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	// The implement leg "legA" is declared as a sibling and depended on, but NO
	// succeeded leg job exists for it, so dependency branch resolution yields 0 branches
	// while the delegation still declares an implement dep.
	parentJob := db.Job{ID: "p", Agent: "coord", Type: "ask"}
	parentPayload := JobPayload{
		Repo:   "owner/repo",
		Branch: "task-x",
		Result: &AgentResult{Delegations: []Delegation{
			{ID: "legA", Action: "implement"},
			{ID: "verify", Action: "review", Deps: []string{"legA"}},
		}},
	}
	verify := Delegation{ID: "verify", Agent: "checker", Action: "review", Prompt: "verify", Deps: []string{"legA"}}
	request := engine.delegationRequest(parentJob, parentPayload, verify)

	err := engine.allocateAndEnqueueDelegation(ctx, parentJob, parentPayload, verify, request, taskRefFromPayload(parentPayload))
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want BlockedError (must fail closed, not review the base checkout)", err)
	}
	if len(manager.mergeCalls) != 0 {
		t.Fatalf("mergeCalls = %d, want 0 (no leg branch to merge)", len(manager.mergeCalls))
	}
	if got := countJobEvents(t, store, "p", "delegation_integration_unresolved"); got != 1 {
		t.Fatalf("delegation_integration_unresolved events = %d, want 1", got)
	}
}

func TestAllocateAndEnqueueDelegationBlocksWhenAnyImplementLegUnresolved(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "p/delegation/legA", Agent: "builder", Type: "implement", ParentJobID: "p", DelegationID: "legA"},
		JobPayload{Repo: "owner/repo", Branch: "branchA", DelegationID: "legA"})

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

	err := engine.allocateAndEnqueueDelegation(ctx, parentJob, parentPayload, verify, request, taskRefFromPayload(parentPayload))
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want BlockedError (must not partially integrate while a dep is unresolved)", err)
	}
	if len(manager.mergeCalls) != 0 {
		t.Fatalf("mergeCalls = %d, want 0 when any implement dep is unresolved", len(manager.mergeCalls))
	}
	if jobExists(t, store, "p/delegation/verify") {
		t.Fatal("verify job was enqueued despite unresolved implement dep")
	}
	if got := countJobEvents(t, store, "p", "delegation_integration_unresolved"); got != 1 {
		t.Fatalf("delegation_integration_unresolved events = %d, want 1", got)
	}
}
