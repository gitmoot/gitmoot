package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"database/sql"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
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
		t.Fatal("non-delegation job with no marker must not match")
	}
	if isReadOnlyDelegationWorktree("ask", JobPayload{DelegationID: "d1"}) {
		t.Fatal("delegation child without a worktree path must not match")
	}

	// #739: a TOP-LEVEL read-only (ask) worktree carries the explicit
	// ReadOnlyWorktree marker and NO DelegationID — it must match so the terminal
	// cleanup disposes it (it would otherwise be orphaned by the DelegationID gate).
	topLevel := JobPayload{WorktreePath: "/wt/seat", ReadOnlyWorktree: true}
	if !isReadOnlyDelegationWorktree("ask", topLevel) {
		t.Fatal("top-level marked read-only worktree (no delegation id) not detected (#739)")
	}
	// The marker without a worktree path is still not disposable (nothing to remove).
	if isReadOnlyDelegationWorktree("ask", JobPayload{ReadOnlyWorktree: true}) {
		t.Fatal("marker without a worktree path must not match")
	}
	// An implement/task worktree (Branch set, marker false) must NEVER match, even
	// under a DelegationID — it is torn down through the merge gate, not here.
	if isReadOnlyDelegationWorktree("implement", JobPayload{DelegationID: "d1", WorktreePath: "/wt/d1", Branch: "gitmoot-delegation-d1"}) {
		t.Fatal("implement delegation worktree must not match the read-only predicate")
	}
}

func TestCleanupDisposesTopLevelReadOnlyWorktree(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	if err := world.engine.cleanupReadOnlyDelegationWorktree(context.Background(), world.jobID, "ask", world.payload); err != nil {
		t.Fatal(err)
	}
	world.assertRemoved(t)
	if data, err := os.ReadFile(world.preserved); err != nil || string(data) != "producer result\n" {
		t.Fatalf("published result did not survive disposal: %q %v", data, err)
	}
}

func TestReadOnlyPrecleanupFailureRedactsDurableError(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	world.engine.BeforeReadOnlyWorktreeCleanup = func(context.Context, string, string, JobPayload) error {
		return errors.New("hostile helper failed with token=" + secret)
	}
	decision, err := world.engine.InspectProducerCleanup(ctx, world.jobID, CleanupPolicy{Mode: "ordinary"})
	if err != nil || !decision.Eligible {
		t.Fatalf("valid resource did not reach preservation: %+v %v", decision, err)
	}
	result, err := world.engine.ApplyProducerCleanup(ctx, world.jobID, decision.ResourceID, decision.EligibilityToken, CleanupPolicy{Mode: "ordinary"})
	if err == nil || result.State != "retryable" {
		t.Fatalf("failed preservation was not retryable: %+v %v", result, err)
	}
	world.assertPreserved(t)
	obligation, err := world.store.GetCleanupObligation(ctx, decision.ResourceID)
	if err != nil || obligation.State != db.CleanupObligationRetryable || obligation.AttemptCount != 1 {
		t.Fatalf("failed preservation was not retained durably: %+v %v", obligation, err)
	}
	if strings.Contains(obligation.LastError, secret) || !strings.Contains(obligation.LastError, "[REDACTED]") {
		t.Fatalf("durable preservation diagnostic exposed a credential: %q", obligation.LastError)
	}
}

func TestReclaimReadOnlyWorktreeRejectsUnownedPath(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	unowned := t.TempDir()
	sentinel := filepath.Join(unowned, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("not a producer resource"), 0600); err != nil {
		t.Fatal(err)
	}
	payload := world.payload
	payload.WorktreePath = unowned
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpdateJobPayload(ctx, world.jobID, encoded); err != nil {
		t.Fatal(err)
	}
	removed, err := world.engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.jobID)
	if err == nil || removed {
		t.Fatalf("unowned path was admitted: removed=%v err=%v", removed, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "not a producer resource" {
		t.Fatalf("unowned contents changed: %q %v", data, err)
	}
	world.assertPreserved(t)
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
	// #2203 removed the delegation branch namer along with implement-leg
	// allocation, so there is no single branch name to probe: assert the stronger
	// property directly — a read-only allocation leaves the repo with NO branch
	// lock at all.
	locks, err := store.ListBranchLocks(ctx, "owner/repo")
	if err != nil {
		t.Fatalf("ListBranchLocks: %v", err)
	}
	if len(locks) != 0 {
		t.Fatalf("read-only allocation must not create a branch lock; locks = %+v", locks)
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

func TestReadOnlyCleanupFailureIsReclaimable(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	manager := &failingReadOnlyCleanupManager{Client: world.manager, failure: errors.New("transient removal failure")}
	world.engine.DelegationWorktrees = manager
	if err := world.engine.cleanupReadOnlyDelegationWorktree(ctx, world.jobID, "ask", world.payload); err == nil {
		t.Fatal("removal failure was lost")
	}
	world.assertPreserved(t)
	resource := db.CleanupObligationResourceID(world.jobID, world.path)
	obligation, err := world.store.GetCleanupObligation(ctx, resource)
	if err != nil || obligation.State != db.CleanupObligationRetryable {
		t.Fatalf("failed removal is not reclaimable: %+v %v", obligation, err)
	}
	manager.failure = nil
	next, err := time.Parse(time.RFC3339Nano, obligation.NextAttemptAt)
	if err != nil {
		t.Fatal(err)
	}
	world.engine.Now = func() time.Time { return next.Add(time.Second) }
	removed, err := world.engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.jobID)
	if err != nil || !removed {
		t.Fatalf("retry did not reclaim the resource: removed=%v err=%v", removed, err)
	}
	world.assertRemoved(t)
	if data, err := os.ReadFile(world.preserved); err != nil || string(data) != "producer result\n" {
		t.Fatalf("retry lost the preserved result: %q %v", data, err)
	}
}

type failingReadOnlyCleanupManager struct {
	gitutil.Client
	failure error
}

func (m *failingReadOnlyCleanupManager) RemoveWorktreeForce(ctx context.Context, path string) error {
	if m.failure != nil {
		return m.failure
	}
	return m.Client.RemoveWorktreeForce(ctx, path)
}

// TestRecordReadOnlyWorktreeReclaimOnAbort pins the #739-review leak fix: a
// read-only ask aborted (cancel/kill/supersede) before it ever runs must mark its
// dispatch-allocated worktree for daemon reclaim, but only when the worktree is
// still on disk (so an already-disposed job is not turned into a permanent,
// never-reconciled reclaim candidate) and only for genuine read-only worktrees.
func TestRecordReadOnlyWorktreeReclaimOnAbort(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)

	// A queued read-only ask carrying a live dispatch worktree is marked for reclaim.
	wt := t.TempDir()
	recordReadOnlyWorktreeReclaimOnAbort(ctx, store,
		db.Job{ID: "local-ask-live", Type: "ask", State: string(JobQueued)},
		JobPayload{Repo: "owner/repo", WorktreePath: wt, ReadOnlyWorktree: true})
	if got := countJobEvents(t, store, "local-ask-live", "delegation_worktree_cleanup_skipped"); got != 1 {
		t.Fatalf("aborted read-only ask with a live worktree must be marked for reclaim, got %d", got)
	}

	// A worktree already gone from disk must NOT be marked (else it becomes a
	// permanent candidate the reclaim pass can never reconcile).
	gone := filepath.Join(t.TempDir(), "already-removed")
	recordReadOnlyWorktreeReclaimOnAbort(ctx, store,
		db.Job{ID: "local-ask-gone", Type: "ask", State: string(JobCancelled)},
		JobPayload{Repo: "owner/repo", WorktreePath: gone, ReadOnlyWorktree: true})
	if got := countJobEvents(t, store, "local-ask-gone", "delegation_worktree_cleanup_skipped"); got != 0 {
		t.Fatalf("already-disposed worktree must not be marked for reclaim, got %d", got)
	}

	// An implement leg (Branch set, no read-only marker) is left to its own merge-gate
	// cleanup and must not be touched by the read-only abort helper.
	recordReadOnlyWorktreeReclaimOnAbort(ctx, store,
		db.Job{ID: "impl-leg", Type: "implement", State: string(JobQueued)},
		JobPayload{DelegationID: "d1", WorktreePath: t.TempDir(), Branch: "gitmoot-delegation-d1"})
	if got := countJobEvents(t, store, "impl-leg", "delegation_worktree_cleanup_skipped"); got != 0 {
		t.Fatalf("implement worktree must not be marked by the read-only abort helper, got %d", got)
	}
}

func TestDispatchDelegationsTwoReadOnlySiblingsGetSeparateDetachedWorktrees(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit-a", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit-b", []string{"ask"}, "gitmoot/gitmoot")
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "gitmoot/gitmoot",
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

	wantPathOne := filepath.Join(home, "worktrees", "gitmoot--gitmoot", "delegations", "parent-job", "d1")
	wantPathTwo := filepath.Join(home, "worktrees", "gitmoot--gitmoot", "delegations", "parent-job", "d2")
	if payloadOne.WorktreePath != wantPathOne {
		t.Fatalf("d1 worktree path = %q, want %q", payloadOne.WorktreePath, wantPathOne)
	}
	if payloadTwo.WorktreePath != wantPathTwo {
		t.Fatalf("d2 worktree path = %q, want %q", payloadTwo.WorktreePath, wantPathTwo)
	}
	if payloadOne.WorktreePath == payloadTwo.WorktreePath {
		t.Fatalf("read-only siblings share worktree path %q -> would serialize on the same checkout key", payloadOne.WorktreePath)
	}
	if !payloadOne.ReadOnlySeat || !payloadTwo.ReadOnlySeat {
		t.Fatalf("detached read-only siblings lack hard seat markers: d1=%v d2=%v", payloadOne.ReadOnlySeat, payloadTwo.ReadOnlySeat)
	}
	// Read-only children are detached: no branch is created, so they keep the
	// inherited parent branch (unlike implement children, which are rebranded).
	if payloadOne.Branch != "task-005" || payloadTwo.Branch != "task-005" {
		t.Fatalf("read-only children must keep the parent branch: d1=%q d2=%q", payloadOne.Branch, payloadTwo.Branch)
	}
	// HeadSHA cleared so validateTargetCheckoutForRunner validates the fresh worktree HEAD.
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
	// #654: each read-only fan-out child keeps its original prompt AND gains a
	// worktree-context note pointing at the canonical base checkout, so a codex
	// (or absolute-path-aware) worker reads gitignored/uncommitted files there
	// instead of reporting them missing from the committed-tip worktree.
	for _, tc := range []struct {
		id      string
		payload JobPayload
		prompt  string
	}{
		{"d1", payloadOne, "audit one"},
		{"d2", payloadTwo, "audit two"},
	} {
		if !strings.HasPrefix(tc.payload.Instructions, tc.prompt) {
			t.Fatalf("%s instructions must start with the original prompt %q, got %q", tc.id, tc.prompt, tc.payload.Instructions)
		}
		if !strings.Contains(tc.payload.Instructions, engine.DelegationCheckout) {
			t.Fatalf("%s instructions must carry the base-checkout path %q, got %q", tc.id, engine.DelegationCheckout, tc.payload.Instructions)
		}
		if !strings.Contains(tc.payload.Instructions, "COMMITTED TIP") || !strings.Contains(tc.payload.Instructions, "gitignored") {
			t.Fatalf("%s instructions must warn about the committed-tip / gitignored worktree, got %q", tc.id, tc.payload.Instructions)
		}
	}
}

func TestDispatchDelegationsSingleReadOnlyDelegationStaysInSharedCheckout(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"ask"}, "gitmoot/gitmoot")
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "gitmoot/gitmoot",
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
	if payload.ReadOnlySeat {
		t.Fatal("shared-checkout single delegation unexpectedly marked as a detached read-only seat")
	}
	if len(manager.detachedCalls) != 0 {
		t.Fatalf("single read-only delegation must not allocate a worktree: %+v", manager.detachedCalls)
	}
	// #654: a single read-only delegation runs in the base checkout and already
	// sees gitignored/uncommitted files, so it must NOT carry the committed-tip
	// worktree note; its prompt stays byte-identical to the delegation prompt.
	if payload.Instructions != "audit one" {
		t.Fatalf("single read-only delegation instructions = %q, want the bare prompt with no worktree note", payload.Instructions)
	}
}

func TestDispatchDelegationsReadOnlyFanoutWithoutManagerEmitsSkippedEvent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit-a", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit-b", []string{"ask"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	// No engine.Home / engine.DelegationWorktrees: detached isolation unavailable.

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "gitmoot/gitmoot",
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
		// #654: the fallback children run serialized in the shared base checkout,
		// so they already see gitignored/uncommitted files and must NOT carry the
		// committed-tip worktree note (which is scoped to the worktree branch).
		if strings.Contains(payload.Instructions, "COMMITTED TIP") {
			t.Fatalf("fallback child %s must not carry the worktree-context note, got %q", id, payload.Instructions)
		}
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_worktree_skipped"); got != 2 {
		t.Fatalf("delegation_worktree_skipped event count = %d, want 2", got)
	}
}

func TestReadOnlyWorktreeContextNote(t *testing.T) {
	// Blank base checkout → "" so ask-path / test engines that never set
	// Engine.DelegationCheckout produce byte-identical prompts (#654).
	if got := readOnlyWorktreeContextNote(""); got != "" {
		t.Fatalf("readOnlyWorktreeContextNote(\"\") = %q, want empty", got)
	}
	if got := readOnlyWorktreeContextNote("   "); got != "" {
		t.Fatalf("readOnlyWorktreeContextNote(blank) = %q, want empty", got)
	}

	base := "/root/gitmoot"
	note := readOnlyWorktreeContextNote(base)
	if note == "" {
		t.Fatal("readOnlyWorktreeContextNote with a base checkout returned empty")
	}
	if !strings.Contains(note, base) {
		t.Fatalf("note must contain the base checkout path %q: %q", base, note)
	}
	for _, want := range []string{"COMMITTED TIP", "gitignored", "read-only"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note must mention %q: %q", want, note)
		}
	}
	// Deterministic: identical input → byte-identical output, so a re-dispatch or
	// retry recomputes the same payload for the idempotent-enqueue equality check.
	if again := readOnlyWorktreeContextNote(base); again != note {
		t.Fatalf("note is non-deterministic: %q != %q", again, note)
	}
	// The exported wrapper the daemon's top-level pool-isolation path uses (#696)
	// MUST return byte-identical text so top-level auto-isolated read-only jobs and
	// read-only delegation fan-out share one committed-tip note.
	if got := ReadOnlyWorktreeContextNote(base); got != note {
		t.Fatalf("ReadOnlyWorktreeContextNote diverged from readOnlyWorktreeContextNote: %q != %q", got, note)
	}
	if got := ReadOnlyWorktreeContextNote(""); got != "" {
		t.Fatalf("ReadOnlyWorktreeContextNote(\"\") = %q, want empty (byte-identical no-note path)", got)
	}
}
