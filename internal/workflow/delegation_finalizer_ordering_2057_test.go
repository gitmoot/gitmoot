package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2057, the accepted P1 on the #1730 fix.
//
// Cited against durable trees, not this checkout: in `origin/main` at 6b95d4cf
// AND at the reviewed head fbde492a, `engine_run_budgets.go:698` dispatched
// delegations and `:710` finalized the implementation - dispatch BEFORE
// finalize. Until the finalizer runs, a task-backed implementation's work is not
// committed, so the worktree HEAD is still the inherited pre-change commit and
// there is nothing for a dispatch-time resolver to resolve.
//
// THE FIRST FIX COULD NOT HAVE CAUGHT THIS AND NEITHER COULD ITS TESTS. They
// preloaded the fake resolver with the produced head before enqueue, which
// supplies the postcondition of a step that has not run. Every value was right;
// only their availability was wrong. Filed as a class in #2076.
//
// So these tests drive Engine.AdvanceJob - the production advancement path - and
// deliberately install NO resolver. The finalizer's returned payload is then the
// only possible source of the produced head, which is exactly the dependency
// under test.

const (
	orderingInheritedHead = "1111111111111111111111111111111111111111"
	orderingProducedHead  = "2222222222222222222222222222222222222222"
)

func newOrderingFixture(t *testing.T) (Engine, *db.Store) {
	t.Helper()
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-2057",
		RepoFullName: "gitmoot/gitmoot",
		GoalID:       "goal-2057",
		Title:        "Ordering",
		State:        string(TaskImplementing),
		Branch:       "task-2057",
		// A worktree path is what makes implementationNeedsFinalizer true, which
		// is the whole population this defect lives in.
		WorktreePath: "/tmp/gitmoot-task-2057",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	return engine, store
}

func orderingParentPayload() JobPayload {
	return JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-2057",
		GoalID: "goal-2057", TaskID: "task-2057", TaskTitle: "Ordering",
		LeadAgent: "lead",
		// The pre-change commit the parent was dispatched against.
		HeadSHA: orderingInheritedHead,
		Result: &AgentResult{
			Decision: "implemented", Summary: "done",
			Delegations: []Delegation{{
				ID: "round2-review", Action: "review", Agent: "reviewer",
			}},
		},
	}
}

// THE REGRESSION. A delegated review must inherit the head the FINALIZER
// produced, which requires the finalizer to have run first. With the production
// ordering reversed this records orderingInheritedHead - a commit that carries
// none of the parent's work - and the row attests a verdict against an ancestor
// of the tree the reviewer read.
func TestDelegatedReviewInheritsTheFinalizedProducedHead(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)

	finalized := orderingParentPayload()
	finalized.HeadSHA = orderingProducedHead
	finalized.PullRequest = 2057
	engine.ImplementationFinalizer = fakeImplementationFinalizer{payload: finalized}
	// No DelegationWorktrees, so no resolver exists. If the child still carries
	// the produced head, it can only have come from the finalized payload.
	engine.DelegationWorktrees = nil

	insertCompletedJob(t, store, db.Job{ID: "impl-2057", Agent: "lead", Type: "implement"}, orderingParentPayload())
	if err := engine.AdvanceJob(ctx, "impl-2057"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}

	child, err := store.GetJob(ctx, "impl-2057/delegation/round2-review")
	if err != nil {
		t.Fatalf("delegated review not enqueued: %v", err)
	}
	childPayload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if childPayload.HeadSHA == orderingInheritedHead {
		t.Fatal("the delegated review inherited the parent's PRE-CHANGE head: delegations were dispatched before the implementation was finalized, so the verdict would name a commit carrying none of the work")
	}
	if childPayload.HeadSHA != orderingProducedHead {
		t.Fatalf("child head = %q, want the finalizer's produced head %q", childPayload.HeadSHA, orderingProducedHead)
	}
	// The PR number is written by the same finalizer paths as the head. If the
	// child sees the head but not the number, the dispatch read a half-updated
	// payload rather than the finalized one.
	if childPayload.PullRequest != 2057 {
		t.Fatalf("child pull_request = %d, want 2057 from the finalized payload", childPayload.PullRequest)
	}
}

// THE FINALIZER RUNS EXACTLY ONCE. It commits, pushes, and opens or adopts a
// pull request, so a second call is not a harmless repeat - moving it earlier
// must not leave the original call site running it again.
func TestImplementationFinalizerRunsOncePerAdvance(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)

	finalized := orderingParentPayload()
	finalized.HeadSHA = orderingProducedHead
	counter := &countingFinalizer{payload: finalized}
	engine.ImplementationFinalizer = counter

	insertCompletedJob(t, store, db.Job{ID: "impl-2057b", Agent: "lead", Type: "implement"}, orderingParentPayload())
	if err := engine.AdvanceJob(ctx, "impl-2057b"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}

	if counter.calls != 1 {
		t.Fatalf("FinalizeImplementation called %d times, want exactly 1: it commits, pushes and opens a PR", counter.calls)
	}
}

// A FAILING FINALIZER MUST PREVENT THE DELEGATIONS, which is the ordering
// consequence of the fix and the safer direction: the work whose review was
// being delegated does not exist. Before the reorder the child was already
// enqueued by the time the finalizer failed.
func TestFailedFinalizerDispatchesNoDelegations(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	engine.ImplementationFinalizer = fakeImplementationFinalizer{err: errors.New("push implementation branch failed")}

	insertCompletedJob(t, store, db.Job{ID: "impl-2057c", Agent: "lead", Type: "implement"}, orderingParentPayload())
	if err := engine.AdvanceJob(ctx, "impl-2057c"); err == nil {
		t.Fatal("AdvanceJob must surface the finalizer failure")
	}

	if _, err := store.GetJob(ctx, "impl-2057c/delegation/round2-review"); err == nil {
		t.Fatal("a review was delegated for work the finalizer failed to commit or push")
	}
}

type countingFinalizer struct {
	payload JobPayload
	calls   int
}

func (c *countingFinalizer) FinalizeImplementation(context.Context, db.Job, JobPayload) (JobPayload, error) {
	c.calls++
	return c.payload, nil
}

// #2057 ROUND TWO, P1-A. An implement job that is ITSELF a delegation child
// advances its parent's DAG, which can enqueue a ready dependent sibling or the
// coordinator continuation. Finalizing after that left those jobs enqueued from
// work that was never committed. Round one's failing-finalizer test used a
// TOP-LEVEL job, so it could not reach this path - the finalizer must sit above
// the parent advance, not merely above dispatchDelegations.
func TestFailedFinalizerOnADelegationChildAdvancesNoParent(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	engine.ImplementationFinalizer = fakeImplementationFinalizer{err: errors.New("push implementation branch failed")}

	parentPayload := orderingParentPayload()
	insertCompletedJob(t, store, db.Job{ID: "coordinator-2057", Agent: "lead", Type: "implement"}, parentPayload)

	// The child is an implement job WITH a parent, which is the shape round one
	// could not express.
	childPayload := orderingParentPayload()
	childPayload.ParentJobID = "coordinator-2057"
	insertCompletedJob(t, store, db.Job{ID: "child-2057", Agent: "lead", Type: "implement"}, childPayload)

	before, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}

	if err := engine.AdvanceJob(ctx, "child-2057"); err == nil {
		t.Fatal("AdvanceJob must surface the finalizer failure")
	}

	after, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("the failed finalizer still enqueued %d job(s) through the parent DAG: before=%d after=%d", len(after)-len(before), len(before), len(after))
	}
}
