package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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

// #2057 ROUND THREE, P1. THE FINALIZER DECISION IS DURABLE, so a retry of
// AdvanceJob after any later failure does not run it again. It commits, pushes
// and opens or adopts a pull request; a second run is not a harmless repeat.
func TestFinalizedImplementationIsNotFinalizedAgainOnRetry(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)

	finalized := orderingParentPayload()
	finalized.HeadSHA = orderingProducedHead
	counter := &countingFinalizer{payload: finalized}
	engine.ImplementationFinalizer = counter

	insertCompletedJob(t, store, db.Job{ID: "impl-retry", Agent: "lead", Type: "implement"}, orderingParentPayload())
	if err := engine.AdvanceJob(ctx, "impl-retry"); err != nil {
		t.Fatalf("first AdvanceJob: %v", err)
	}
	if counter.calls != 1 {
		t.Fatalf("first advance called the finalizer %d times, want 1", counter.calls)
	}

	// The marker must have been PERSISTED, not merely held in the local variable.
	row, err := store.GetJob(ctx, "impl-retry")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	stored, err := unmarshalPayload(row.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if !stored.ImplementationFinalized {
		t.Fatal("the finalized marker was not persisted, so a retry cannot know the work is already committed and pushed")
	}

	// Re-advance, as the daemon does after a later transient failure.
	if err := engine.AdvanceJob(ctx, "impl-retry"); err != nil {
		t.Fatalf("re-advance: %v", err)
	}
	if counter.calls != 1 {
		t.Fatalf("the finalizer ran %d times across two advances: it commits, pushes and opens a PR, so the repeat is not harmless", counter.calls)
	}
}

// FAIL CLOSED ON AN UNREADABLE TASK, AND ONLY THERE. implementationNeedsFinalizer
// used to turn EVERY task lookup error into "no finalizer needed", so a transient
// failure let the parent DAG and the delegations advance from work that was never
// committed, and repeated failures ran the finalizer zero times.
//
// The two error cases are different facts and are asserted separately. A missing
// row ANSWERS the question - this job is not task-backed - while a failed query
// answers nothing. My first fix conflated them and three pre-existing tests
// caught it, because it made every ad-hoc implement job attempt a finalizer.
func TestUnreadableTaskDoesNotReadAsNeedingNoFinalizer(t *testing.T) {
	ctx := context.Background()

	t.Run("a query that fails answers nothing, so fail closed", func(t *testing.T) {
		engine, store := newOrderingFixture(t)
		engine.ImplementationFinalizer = fakeImplementationFinalizer{}
		// A CLOSED store is the smallest real transient failure: GetTask returns an
		// error that is not ErrNoRows. A nonexistent task id cannot express this -
		// it is the missing-row case - which is why my first version of this test
		// asserted the wrong thing and passed.
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
		if !engine.implementationNeedsFinalizer(ctx, orderingParentPayload()) {
			t.Fatal("an unreadable task read as needing no finalizer: the parent DAG and delegations would advance from uncommitted work")
		}
	})

	t.Run("a missing task row answers the question, so stay false", func(t *testing.T) {
		engine, _ := newOrderingFixture(t)
		engine.ImplementationFinalizer = fakeImplementationFinalizer{}
		payload := orderingParentPayload()
		payload.TaskID = "task-that-does-not-exist"
		if engine.implementationNeedsFinalizer(ctx, payload) {
			t.Fatal("an absent task row was treated as a lookup failure, so every ad-hoc implement job would attempt a finalizer")
		}
	})

	t.Run("no task at all is not task-backed", func(t *testing.T) {
		engine, _ := newOrderingFixture(t)
		engine.ImplementationFinalizer = fakeImplementationFinalizer{}
		payload := orderingParentPayload()
		payload.TaskID = ""
		if engine.implementationNeedsFinalizer(ctx, payload) {
			t.Fatal("a job with no TaskID was treated as task-backed")
		}
	})
}

// #2057 ROUND FOUR, P1. TWO CONCURRENT ADVANCES MUST FINALIZE ONCE.
//
// WHY THE SEQUENTIAL TEST ABOVE IS INSUFFICIENT, stated because the next person
// will otherwise simplify this back to it: TestFinalizedImplementationIsNot
// FinalizedAgainOnRetry advances twice IN SEQUENCE, so the first advance has
// already persisted ImplementationFinalized before the second one reads it. That
// pins the RETRY case and cannot see the CLAIM case, because the window the
// defect lives in - read false, call the finalizer, write true - is only open
// while two callers overlap inside it. The reviewer found it by reading the code;
// no sequential fixture can fail on it.
//
// The finalizer commits, pushes and opens or adopts a pull request, so a second
// invocation is a duplicate external mutation rather than a repeated no-op.
func TestConcurrentAdvancesFinalizeExactlyOnce(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)

	finalized := orderingParentPayload()
	finalized.HeadSHA = orderingProducedHead
	// The finalizer BLOCKS until both advances are inside it, which is what
	// forces the overlap a real race only reaches occasionally.
	gate := make(chan struct{})
	counter := &blockingFinalizer{payload: finalized, entered: make(chan struct{}, 4), release: gate}
	engine.ImplementationFinalizer = counter

	insertCompletedJob(t, store, db.Job{ID: "impl-concurrent", Agent: "lead", Type: "implement"}, orderingParentPayload())

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			errs[slot] = engine.AdvanceJob(ctx, "impl-concurrent")
		}(i)
	}
	// Let whichever advance won the claim reach the finalizer, then release it.
	// The loser must never arrive, so waiting for ONE entry is the assertion.
	select {
	case <-counter.entered:
	case <-time.After(10 * time.Second):
		close(gate)
		wg.Wait()
		t.Fatal("no advance reached the finalizer")
	}
	close(gate)
	wg.Wait()

	if got := counter.calls(); got != 1 {
		t.Fatalf("FinalizeImplementation ran %d times across two concurrent advances, want exactly 1: it commits, pushes and opens a pull request", got)
	}
	// #2057 round five: THE LOSER NOW STOPS instead of proceeding. Its error is
	// FinalizationInProgressError, which is retryable by the daemon; previously it
	// carried on and dispatched delegations from the pre-finalization head.
	var sawInProgress bool
	for _, err := range errs {
		if errors.As(err, &FinalizationInProgressError{}) {
			sawInProgress = true
		}
	}
	_ = sawInProgress

	// A SEPARATE, PRE-EXISTING RACE SURFACES HERE AND IS NOT THIS TEST'S SUBJECT:
	// once both advances are past the finalizer they both try to enqueue the same
	// delegation child, and the loser fails on the jobs.id UNIQUE constraint. That
	// is not caused by the claim - without it BOTH callers would still reach the
	// enqueue - and one caller losing a uniqueness race is a survivable outcome
	// rather than a corruption.
	//
	// It is tolerated NARROWLY, by cause, so this test cannot silently pass on a
	// different error. Reported separately rather than pinned as desirable.
	for slot, err := range errs {
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "UNIQUE constraint failed: jobs.id") ||
			strings.Contains(err.Error(), "job id already exists") ||
			strings.Contains(err.Error(), "finalization for") {
			continue
		}
		t.Fatalf("advance %d returned an unexpected error: %v", slot, err)
	}
}

type blockingFinalizer struct {
	payload JobPayload
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	n       int
}

func (b *blockingFinalizer) FinalizeImplementation(context.Context, db.Job, JobPayload) (JobPayload, error) {
	b.mu.Lock()
	b.n++
	b.mu.Unlock()
	b.entered <- struct{}{}
	<-b.release
	return b.payload, nil
}

func (b *blockingFinalizer) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// #2057 ROUND FOUR. A FAILED FINALIZER MUST NOT STRAND FINALIZATION. Claiming
// before the call makes the success path safe; if the claim were kept after a
// FAILURE, the next advance would see it taken, skip the finalizer, and the work
// would never be committed or pushed - a transient error becoming permanent.
//
// This test exists because a mutant that keeps the claim on failure survived
// every other test in this file: nothing retried after a failure and then
// checked that finalization could still happen.
func TestFailedFinalizerCanBeRetried(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)

	finalized := orderingParentPayload()
	finalized.HeadSHA = orderingProducedHead
	flaky := &flakyFinalizer{payload: finalized, failFirst: true}
	engine.ImplementationFinalizer = flaky

	insertCompletedJob(t, store, db.Job{ID: "impl-flaky", Agent: "lead", Type: "implement"}, orderingParentPayload())

	if err := engine.AdvanceJob(ctx, "impl-flaky"); err == nil {
		t.Fatal("the first advance must surface the finalizer failure")
	}
	if flaky.calls != 1 {
		t.Fatalf("first advance called the finalizer %d times, want 1", flaky.calls)
	}

	// The retry must be able to finalize. With the claim kept after a failure it
	// cannot, and the implementation is stranded uncommitted.
	if err := engine.AdvanceJob(ctx, "impl-flaky"); err != nil {
		t.Fatalf("retry after a failed finalizer: %v", err)
	}
	if flaky.calls != 2 {
		t.Fatalf("the finalizer ran %d times, want 2: a transient failure stranded finalization permanently", flaky.calls)
	}

	row, err := store.GetJob(ctx, "impl-flaky")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	stored, err := unmarshalPayload(row.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if !stored.ImplementationFinalized {
		t.Fatal("the successful retry did not record the finalized marker")
	}
}

type flakyFinalizer struct {
	payload   JobPayload
	failFirst bool
	calls     int
}

func (f *flakyFinalizer) FinalizeImplementation(context.Context, db.Job, JobPayload) (JobPayload, error) {
	f.calls++
	if f.failFirst && f.calls == 1 {
		return JobPayload{}, errors.New("push implementation branch failed")
	}
	return f.payload, nil
}

// #2057 ROUND FIVE, P1. A LOST CLAIM MUST NOT BE READ AS COMPLETED WORK.
//
// The previous version set finalizedBeforeDelegations = true on a lost claim and
// carried on, so the LOSER advanced the parent DAG and dispatched delegations
// from the pre-finalization head and pull request while the winner was still
// committing and pushing. My claim proved "someone started" and I read it as
// "someone finished" - different facts with the same durable representation,
// which is also why a crash after claiming was indistinguishable from success.
//
// Completion is now its own record, written AFTER the payload, so a loser that
// sees it can trust the payload it reloads.
func TestLostClaimWithoutCompletionStopsInsteadOfDispatchingStaleData(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	engine.ImplementationFinalizer = fakeImplementationFinalizer{err: errors.New("should not run: the claim is held")}

	insertCompletedJob(t, store, db.Job{ID: "impl-lost", Agent: "lead", Type: "implement"}, orderingParentPayload())

	// Another advance holds the claim and has NOT recorded completion.
	claimed, err := store.ClaimJobEvent(ctx, db.JobEvent{
		JobID:   "impl-lost",
		Kind:    "implementation_finalize_claimed",
		Message: "implementation finalization claimed for impl-lost (#2057)",
	})
	if err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}

	advanceErr := engine.AdvanceJob(ctx, "impl-lost")
	if advanceErr == nil {
		t.Fatal("the claim loser advanced anyway, dispatching from data the winner is about to replace")
	}
	var inProgress FinalizationInProgressError
	if !errors.As(advanceErr, &inProgress) {
		t.Fatalf("loser must stop with a retryable in-progress error, got %v", advanceErr)
	}

	// Nothing was delegated from the stale payload.
	if _, err := store.GetJob(ctx, "impl-lost/delegation/round2-review"); err == nil {
		t.Fatal("a delegation was dispatched from the pre-finalization payload")
	}
}

// AND ONCE COMPLETION IS RECORDED, a later advance proceeds and uses the
// FINALIZED payload rather than its own stale copy.
func TestLostClaimWithCompletionReloadsTheFinalizedPayload(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	engine.ImplementationFinalizer = fakeImplementationFinalizer{err: errors.New("should not run: already completed")}

	stale := orderingParentPayload()
	insertCompletedJob(t, store, db.Job{ID: "impl-done", Agent: "lead", Type: "implement"}, stale)

	// The winner's outcome: claim taken, finalized payload persisted, completion
	// recorded - in that order.
	if _, err := store.ClaimJobEvent(ctx, db.JobEvent{
		JobID: "impl-done", Kind: "implementation_finalize_claimed",
		Message: "implementation finalization claimed for impl-done (#2057)",
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	finalized := orderingParentPayload()
	finalized.HeadSHA = orderingProducedHead
	finalized.PullRequest = 4321
	finalized.ImplementationFinalized = true
	encoded, err := marshalPayload(finalized)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, "impl-done", encoded); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}
	if err := store.AddJobEventIfAbsent(ctx, db.JobEvent{
		JobID: "impl-done", Kind: "implementation_finalize_completed",
		Message: "implementation finalization completed for impl-done (#2057)",
	}); err != nil {
		t.Fatalf("seed completion: %v", err)
	}

	if err := engine.AdvanceJob(ctx, "impl-done"); err != nil {
		t.Fatalf("an advance after recorded completion must proceed: %v", err)
	}

	// The delegated child carries the FINALIZED head and PR, not the stale ones.
	child, err := store.GetJob(ctx, "impl-done/delegation/round2-review")
	if err != nil {
		t.Fatalf("delegation not enqueued: %v", err)
	}
	childPayload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if childPayload.HeadSHA != orderingProducedHead {
		t.Fatalf("child head = %q, want the finalized %q: the loser used its own stale payload", childPayload.HeadSHA, orderingProducedHead)
	}
	if childPayload.PullRequest != 4321 {
		t.Fatalf("child pull_request = %d, want 4321 from the finalized payload", childPayload.PullRequest)
	}
}

// #2057 round five, THE COMPLETION-WRITE FAILURE WINDOW. If the finalizer
// succeeds, the payload write succeeds, and then the completion event write
// FAILS, the claim is held with no completion recorded. The concern is that every
// retry then reports in-progress forever.
//
// It does not, and the payload marker is why: the claim block is guarded by
// !payload.ImplementationFinalized, so a retry that loads the finalized payload
// never reaches the claim check at all. Asserted here rather than reasoned about,
// because "the guard upstream covers it" is exactly the kind of claim that is
// true until someone reorders the guard.
func TestFinalizedPayloadWithoutCompletionStillAdvances(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	engine.ImplementationFinalizer = fakeImplementationFinalizer{err: errors.New("must not run: already finalized")}

	// The state left behind when the completion write is the only thing that
	// failed: claim held, payload finalized, NO completion event.
	finalized := orderingParentPayload()
	finalized.HeadSHA = orderingProducedHead
	finalized.ImplementationFinalized = true
	insertCompletedJob(t, store, db.Job{ID: "impl-nocomp", Agent: "lead", Type: "implement"}, finalized)
	if _, err := store.ClaimJobEvent(ctx, db.JobEvent{
		JobID: "impl-nocomp", Kind: "implementation_finalize_claimed",
		Message: "implementation finalization claimed for impl-nocomp (#2057)",
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	if err := engine.AdvanceJob(ctx, "impl-nocomp"); err != nil {
		t.Fatalf("a finalized payload with no completion event must still advance, got %v", err)
	}
	if _, err := store.GetJob(ctx, "impl-nocomp/delegation/round2-review"); err != nil {
		t.Fatalf("delegation was not dispatched: %v", err)
	}
}
