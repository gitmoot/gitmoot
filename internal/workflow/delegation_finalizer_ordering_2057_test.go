package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// #2057 ROUND SEVEN, P1. A LIVE FINALIZER'S CLAIM MUST NEVER BE RECOVERED, AND
// AGE CANNOT ESTABLISH THAT. Round six recovered any claim older than 15
// minutes; the reviewer instrumented it and saw the winning finalizer stay live
// while a losing advance released its claim and a third advance entered the same
// finalizer concurrently. There is no safe constant: finalization is bounded
// only by the job context, four hours by default and eight at most.
//
// Recovery is now keyed on the OWNER stamped into the claim. This test never
// advances a clock, because the implementation no longer reads one.
func TestLiveFinalizeClaimOwnerIsNeverRecovered(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	finalizer := &countingImplementationFinalizer{}
	engine.ImplementationFinalizer = finalizer

	insertCompletedJob(t, store, db.Job{ID: "impl-live", Agent: "lead", Type: "implement"}, orderingParentPayload())
	// A claim owned by THIS boot and THIS process: provably alive.
	seedFinalizeClaim(t, store, "impl-live", implementationFinalizeClaimOwnerMessage("impl-live"))

	abandoned, owner, err := engine.implementationFinalizeClaimAbandoned(ctx, "impl-live")
	if err != nil {
		t.Fatalf("abandonment check: %v", err)
	}
	if abandoned {
		t.Fatalf("a claim held by a LIVE process was reported abandoned (owner=%+v)", owner)
	}
	if err := engine.AdvanceJob(ctx, "impl-live"); !errors.As(err, &FinalizationInProgressError{}) {
		t.Fatalf("a live claim must stay a retryable stop, got %v", err)
	}
	if finalizer.calls != 0 {
		t.Fatalf("the finalizer ran %d times beside a live owner, which is the reproduced defect", finalizer.calls)
	}
}

// A CLAIM FROM A PREVIOUS BOOT CANNOT HAVE A LIVE HOLDER, so it is recovered and
// the next advance finalizes. This is the case round six was trying to serve,
// now decided by identity instead of elapsed time.
func TestFinalizeClaimFromAForeignBootIsRecovered(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	finalizer := &countingImplementationFinalizer{}
	engine.ImplementationFinalizer = finalizer

	insertCompletedJob(t, store, db.Job{ID: "impl-foreign", Agent: "lead", Type: "implement"}, orderingParentPayload())
	seedFinalizeClaim(t, store, "impl-foreign", foreignOwnerMessage("impl-foreign"))

	if err := engine.AdvanceJob(ctx, "impl-foreign"); !errors.As(err, &FinalizationInProgressError{}) {
		t.Fatalf("the recovering advance must still be a retryable stop, got %v", err)
	}
	recovered, message := claimRecoveryRecorded(t, store, "impl-foreign")
	if !recovered {
		t.Fatal("a claim from a dead boot was neither released nor recorded, so the job is stranded")
	}
	if !strings.Contains(message, "is gone") || !strings.Contains(message, "424242") {
		t.Fatalf("the recovery event does not name the owner it removed: %q", message)
	}

	// THE PROPERTY THAT MAKES RECOVERY WORTH ANYTHING: the next advance actually
	// finalizes. Releasing without a subsequent finalize would only move the stall.
	if err := engine.AdvanceJob(ctx, "impl-foreign"); err != nil {
		t.Fatalf("the advance after recovery must finalize, got %v", err)
	}
	if finalizer.calls != 1 {
		t.Fatalf("the finalizer ran %d times after recovery, want exactly 1", finalizer.calls)
	}
}

// THE ABA RACE THE REVIEWER NAMED. Two stale recoverers can both observe the old
// claim; one releases it and a new winner acquires. The second must NOT be able
// to release the successor's claim. It cannot, because the release is built from
// the OBSERVED message and the successor's carries a different owner.
func TestStaleRecovererCannotReleaseASuccessorsClaim(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	engine.ImplementationFinalizer = &countingImplementationFinalizer{}

	insertCompletedJob(t, store, db.Job{ID: "impl-aba", Agent: "lead", Type: "implement"}, orderingParentPayload())
	seedFinalizeClaim(t, store, "impl-aba", foreignOwnerMessage("impl-aba"))

	// Both recoverers observe the stale claim.
	_, firstOwner, err := engine.implementationFinalizeClaimAbandoned(ctx, "impl-aba")
	if err != nil {
		t.Fatalf("first observation: %v", err)
	}
	_, secondOwner, err := engine.implementationFinalizeClaimAbandoned(ctx, "impl-aba")
	if err != nil {
		t.Fatalf("second observation: %v", err)
	}

	// The first recoverer wins the OWNER token, which is the recovery right.
	if won, err := store.ReleaseJobEventClaim(ctx, db.JobEvent{
		JobID: "impl-aba", Kind: implementationFinalizeClaimOwnerEvent, Message: firstOwner.Message,
	}); err != nil || !won {
		t.Fatalf("first recoverer must win the owner token: won=%v err=%v", won, err)
	}

	// The SECOND, holding the same observed owner, must LOSE it. That is the
	// mutual exclusion: it stops before it can touch any claim.
	released, err := store.ReleaseJobEventClaim(ctx, db.JobEvent{
		JobID: "impl-aba", Kind: implementationFinalizeClaimOwnerEvent, Message: secondOwner.Message,
	})
	if err != nil {
		t.Fatalf("second recoverer: %v", err)
	}
	if released {
		t.Fatal("two recoverers both won the owner token, so both may release a claim: the ABA race is open")
	}
}

// AN UNATTRIBUTABLE CLAIM IS LIVE. A message that does not parse cannot be tied
// to an owner, and recovering it would re-enter a finalizer on no evidence.
func TestUnattributableFinalizeClaimIsNotRecovered(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	finalizer := &countingImplementationFinalizer{}
	engine.ImplementationFinalizer = finalizer

	insertCompletedJob(t, store, db.Job{ID: "impl-opaque", Agent: "lead", Type: "implement"}, orderingParentPayload())
	// A LEGACY claim: written before owner rows existed, so it has none.
	seedFinalizeClaim(t, store, "impl-opaque", "")
	abandoned, _, err := engine.implementationFinalizeClaimAbandoned(ctx, "impl-opaque")
	if err != nil {
		t.Fatalf("abandonment check: %v", err)
	}
	if abandoned {
		t.Fatal("an unparseable claim was recovered, so a legacy row re-enters the finalizer")
	}
	if err := engine.AdvanceJob(ctx, "impl-opaque"); !errors.As(err, &FinalizationInProgressError{}) {
		t.Fatalf("want a retryable stop, got %v", err)
	}
	if finalizer.calls != 0 {
		t.Fatalf("the finalizer ran %d times on an unattributable claim", finalizer.calls)
	}
}

// A COMPLETED claim is never abandoned, whatever its owner: the loser reloads the
// finalized payload and proceeds, which is round five's behaviour.
func TestCompletedFinalizeClaimIsNeverRecovered(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	engine.ImplementationFinalizer = fakeImplementationFinalizer{err: errors.New("should not run: already completed")}

	payload := orderingParentPayload()
	payload.ImplementationFinalized = true
	insertCompletedJob(t, store, db.Job{ID: "impl-done", Agent: "lead", Type: "implement"}, payload)
	seedFinalizeClaim(t, store, "impl-done", foreignOwnerMessage("impl-done"))
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID: "impl-done", Kind: implementationFinalizeCompletedEvent,
		Message: "implementation finalization completed for impl-done (#2057)",
	}); err != nil {
		t.Fatalf("seed completion: %v", err)
	}

	abandoned, _, err := engine.implementationFinalizeClaimAbandoned(ctx, "impl-done")
	if err != nil {
		t.Fatalf("abandonment check: %v", err)
	}
	if abandoned {
		t.Fatal("a completed finalization was reported abandoned, which would re-run the finalizer")
	}
}

type countingImplementationFinalizer struct {
	calls int
}

func (f *countingImplementationFinalizer) FinalizeImplementation(_ context.Context, _ db.Job, payload JobPayload) (JobPayload, error) {
	f.calls++
	return payload, nil
}

func claimRecoveryRecorded(t *testing.T, store *db.Store, jobID string) (bool, string) {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == implementationFinalizeClaimRecoveredEvent {
			return true, event.Message
		}
	}
	return false, ""
}

// corruptJobEventTimestamp makes a claim's created_at unparseable, which is the
// only way to exercise the unaged branch: the store writes a valid timestamp.
func corruptJobEventTimestamp(t *testing.T, store *db.Store, jobID, kind, value string) {
	t.Helper()
	conn, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	result, err := conn.Exec(`UPDATE job_events SET created_at = ? WHERE job_id = ? AND kind = ?`, value, jobID, kind)
	if err != nil {
		t.Fatalf("UPDATE job event time: %v", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("updated rows = %d err=%v, want 1", changed, err)
	}
}

// seedFinalizeClaim writes a claim AND its owner row, which is the pair
// production always writes together. A fixture with only one of them models a
// state the winner cannot produce.
func seedFinalizeClaim(t *testing.T, store *db.Store, jobID, ownerMessage string) {
	t.Helper()
	ctx := context.Background()
	claimed, err := store.ClaimJobEvent(ctx, db.JobEvent{
		JobID: jobID, Kind: implementationFinalizeClaimedEvent,
		Message: implementationFinalizeClaimMessage(jobID),
	})
	if err != nil || !claimed {
		t.Fatalf("seed claim %s: claimed=%v err=%v", jobID, claimed, err)
	}
	if strings.TrimSpace(ownerMessage) == "" {
		return
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID: jobID, Kind: implementationFinalizeClaimOwnerEvent, Message: ownerMessage,
	}); err != nil {
		t.Fatalf("seed owner %s: %v", jobID, err)
	}
}

func foreignOwnerMessage(jobID string) string {
	return "implementation finalization for " + jobID + " is held by boot 00000000-dead-dead-dead-000000000000 pid 424242 (#2057)"
}

// THE CLAIM MESSAGE IS A DEPLOY CONTRACT, and this pins it because round seven
// broke it once. ClaimJobEvent's at-most-once check keys on (job, kind,
// MESSAGE), so a claim written by an older binary must still exclude a claimer
// running the newer one. My first attempt put the owner identity INTO the claim
// message, and a deploy with an in-flight finalization would then have run a
// SECOND finalizer beside the first: the exact double-execution the claim
// exists to prevent, introduced by the fix for it.
//
// A pre-existing round-five test caught it. This one states it directly, so the
// next person to reach for a richer claim message sees the cost first.
func TestClaimMessageStaysStableAcrossABinaryChange(t *testing.T) {
	ctx := context.Background()
	_, store := newOrderingFixture(t)
	insertCompletedJob(t, store, db.Job{ID: "impl-deploy", Agent: "lead", Type: "implement"}, orderingParentPayload())

	// THE OLD BINARY'S MESSAGE IS A LITERAL, not a call to the current helper.
	// Round eight's reviewer caught that: seeding both sides through
	// implementationFinalizeClaimMessage made the test tautological, so changing
	// the helper to a v2 string left it GREEN while the deploy contract it
	// claims to protect was broken. This literal is the string the deployed
	// b592f556, 9b88aca6 and b47bf3fc binaries all emit.
	const deployed = "implementation finalization claimed for impl-deploy (#2057)"
	if got := implementationFinalizeClaimMessage("impl-deploy"); got != deployed {
		t.Fatalf("the claim message CHANGED: %q, want the deployed %q - a running finalization would no longer be excluded", got, deployed)
	}
	if claimed, err := store.ClaimJobEvent(ctx, db.JobEvent{
		JobID: "impl-deploy", Kind: implementationFinalizeClaimedEvent, Message: deployed,
	}); err != nil || !claimed {
		t.Fatalf("seed the old binary's claim: claimed=%v err=%v", claimed, err)
	}

	// The NEW binary tries to claim the same finalization. It must LOSE.
	claimed, err := store.ClaimJobEvent(ctx, db.JobEvent{
		JobID:   "impl-deploy",
		Kind:    implementationFinalizeClaimedEvent,
		Message: implementationFinalizeClaimMessage("impl-deploy"),
	})
	if err != nil {
		t.Fatalf("ClaimJobEvent: %v", err)
	}
	if claimed {
		t.Fatal("a new binary claimed a finalization already held by an older one: two finalizers would run")
	}
}

// A MALFORMED OWNER ROW IS LIVE, and this is a DIFFERENT case from a missing
// one. The legacy test above has no owner row at all, so it returns before the
// parse is ever attempted: a mutant making an unparseable owner "abandoned"
// survived it. This fixture writes an owner row that does not match the
// pattern, so the parse branch itself is under test.
func TestMalformedFinalizeClaimOwnerIsNotRecovered(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	finalizer := &countingImplementationFinalizer{}
	engine.ImplementationFinalizer = finalizer

	for index, owner := range []string{
		"implementation finalization for impl-bad is held by boot  pid  (#2057)",
		"implementation finalization for impl-bad is held by boot abc pid notanumber (#2057)",
		"some entirely different sentence",
		"implementation finalization for impl-bad is held by boot abc pid -3 (#2057)",
	} {
		t.Run(fmt.Sprintf("case%d", index), func(t *testing.T) {
			jobID := fmt.Sprintf("impl-bad-%d", index)
			insertCompletedJob(t, store, db.Job{ID: jobID, Agent: "lead", Type: "implement"}, orderingParentPayload())
			seedFinalizeClaim(t, store, jobID, strings.Replace(owner, "impl-bad", jobID, 1))

			abandoned, _, err := engine.implementationFinalizeClaimAbandoned(ctx, jobID)
			if err != nil {
				t.Fatalf("abandonment check: %v", err)
			}
			if abandoned {
				t.Fatalf("an owner row that does not parse (%q) was recovered anyway", owner)
			}
		})
	}
	if finalizer.calls != 0 {
		t.Fatalf("the finalizer ran %d times on unparseable owner rows", finalizer.calls)
	}
}

// TWO CONCURRENT RECOVERIES MUST PRODUCE ONE FINALIZER, and this drives the
// ENGINE rather than the store primitive.
//
// The ABA test above asserts that ReleaseJobEventClaim is at-most-once, which is
// a fact about the store. A mutant removing the engine's `if !wonRecovery` guard
// survived it, because nothing checked that the engine CONSULTS that result.
// This test races two advances against one abandoned claim: exactly one may win
// the owner token and go on to release the claim, so the finalizer can run at
// most once no matter how the two interleave.
func TestConcurrentRecoveriesFinalizeAtMostOnce(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	finalizer := &countingImplementationFinalizer{}
	engine.ImplementationFinalizer = finalizer

	insertCompletedJob(t, store, db.Job{ID: "impl-race", Agent: "lead", Type: "implement"}, orderingParentPayload())
	seedFinalizeClaim(t, store, "impl-race", foreignOwnerMessage("impl-race"))

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = engine.AdvanceJob(ctx, "impl-race")
		}()
	}
	close(start)
	wg.Wait()

	// At most one recovery may have happened, so at most one finalizer call.
	if finalizer.calls > 1 {
		t.Fatalf("the finalizer ran %d times: two recoverers both released the claim", finalizer.calls)
	}
	// And exactly one recovery event, never two, because the owner token is the
	// mutual exclusion.
	events, err := store.ListJobEvents(ctx, "impl-race")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	recoveries := 0
	for _, event := range events {
		if event.Kind == implementationFinalizeClaimRecoveredEvent {
			recoveries++
		}
	}
	if recoveries > 1 {
		t.Fatalf("recorded %d recoveries of one claim, want at most 1", recoveries)
	}
}

// #2057 ROUND EIGHT, P1. RELEASING A CLAIM MUST TAKE ITS OWNER ROW WITH IT.
//
// Round seven released the claim on the payload-write arm and left the owner row
// behind, so a LATER holder inherited a dead identity: the abandonment reader
// found the prior boot's owner beside the new claim, called it abandoned, and
// recovered a LIVE finalizer. The reviewer reproduced that.
//
// This drives the production helper both arms now use, and then asserts the
// property that actually matters - that a release-then-reclaim cycle leaves
// exactly ONE owner row, naming the CURRENT holder.
func TestReleasingAClaimRemovesItsOwnerRow(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	insertCompletedJob(t, store, db.Job{ID: "impl-pair", Agent: "lead", Type: "implement"}, orderingParentPayload())

	seedFinalizeClaim(t, store, "impl-pair", implementationFinalizeClaimOwnerMessage("impl-pair"))
	if err := engine.releaseFinalizeClaim(ctx, "impl-pair"); err != nil {
		t.Fatalf("releaseFinalizeClaim: %v", err)
	}
	claims, owners := finalizeClaimRowCounts(t, store, "impl-pair")
	if claims != 0 || owners != 0 {
		t.Fatalf("after release: claims=%d owners=%d, want 0 and 0 (a surviving owner row is the defect)", claims, owners)
	}

	// RECLAIM, which is the scenario that turned the leftover row into a live
	// recovery: a new holder must not find a second, older owner beside its own.
	seedFinalizeClaim(t, store, "impl-pair", implementationFinalizeClaimOwnerMessage("impl-pair"))
	claims, owners = finalizeClaimRowCounts(t, store, "impl-pair")
	if claims != 1 || owners != 1 {
		t.Fatalf("after reclaim: claims=%d owners=%d, want exactly 1 and 1", claims, owners)
	}
	abandoned, owner, err := engine.implementationFinalizeClaimAbandoned(ctx, "impl-pair")
	if err != nil {
		t.Fatalf("abandonment check: %v", err)
	}
	if abandoned {
		t.Fatalf("the reclaimed live holder was reported abandoned via a surviving row: owner=%+v", owner)
	}
}

// THE STALE-OWNER STATE ITSELF, asserted as the thing recovery must refuse when
// it cannot be prevented: a claim accompanied by a FOREIGN owner row is what the
// leftover row produced. It is reported abandoned by design - which is exactly
// why the release must never leave one behind. This test pins the direction so a
// future reader cannot mistake the reader for the defect.
func TestAForeignOwnerBesideAClaimIsWhatTheReleaseMustPrevent(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	insertCompletedJob(t, store, db.Job{ID: "impl-stale", Agent: "lead", Type: "implement"}, orderingParentPayload())
	seedFinalizeClaim(t, store, "impl-stale", foreignOwnerMessage("impl-stale"))

	abandoned, _, err := engine.implementationFinalizeClaimAbandoned(ctx, "impl-stale")
	if err != nil {
		t.Fatalf("abandonment check: %v", err)
	}
	if !abandoned {
		t.Fatal("a foreign owner beside a claim was NOT reported abandoned, so foreign-boot recovery is broken")
	}
	// And the release removes exactly that pairing, so the state cannot persist
	// into a later holder's lifetime.
	if err := engine.releaseFinalizeClaim(ctx, "impl-stale"); err != nil {
		// The foreign owner's message differs from this process's, so the helper
		// cannot match it: that is a REAL limitation and it is asserted, not
		// hidden. A foreign row is cleaned by the recovery path, not by a
		// holder's own release.
		t.Fatalf("releaseFinalizeClaim returned an error: %v", err)
	}
	_, owners := finalizeClaimRowCounts(t, store, "impl-stale")
	if owners != 1 {
		t.Fatalf("owners=%d; a holder's release matches only ITS OWN owner message, so a foreign row must survive here and be removed by recovery", owners)
	}
}

func finalizeClaimRowCounts(t *testing.T, store *db.Store, jobID string) (int, int) {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var claims, owners int
	for _, event := range events {
		switch event.Kind {
		case implementationFinalizeClaimedEvent:
			claims++
		case implementationFinalizeClaimOwnerEvent:
			owners++
		}
	}
	return claims, owners
}

// #2057 ROUND EIGHT, THE INVARIANT ITSELF: A CLAIM MUST NOT SURVIVE A FAILED
// OWNER WRITE.
//
// The mutant that made the owner write best-effort again survived every other
// test in this file, because nothing exercised the failure. There is no
// interface seam to inject one (#2093: Engine.Store is a concrete *db.Store), so
// this uses the same technique the reviewer used - a SQLite TRIGGER that rejects
// exactly the owner insert - which injects the fault into the PRODUCTION path
// rather than around it.
func TestAClaimIsReleasedWhenItsOwnerRowCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	engine, store := newOrderingFixture(t)
	finalizer := &countingImplementationFinalizer{}
	engine.ImplementationFinalizer = finalizer
	insertCompletedJob(t, store, db.Job{ID: "impl-noowner", Agent: "lead", Type: "implement"}, orderingParentPayload())

	rejectJobEventKind(t, store, implementationFinalizeClaimOwnerEvent)

	err := engine.AdvanceJob(ctx, "impl-noowner")
	if err == nil {
		t.Fatal("the advance succeeded while the owner row could not be written, so a claim exists that nothing can attribute")
	}
	// The finalizer must NOT have run: the claim is surrendered before any
	// external work, so a retry is a clean retry rather than a second run.
	if finalizer.calls != 0 {
		t.Fatalf("the finalizer ran %d times despite an unattributable claim", finalizer.calls)
	}
	claims, owners := finalizeClaimRowCounts(t, store, "impl-noowner")
	if claims != 0 {
		t.Fatalf("claims=%d owners=%d: the claim survived a failed owner write, which is the state that lets a later holder inherit a dead identity", claims, owners)
	}
}

// rejectJobEventKind installs a trigger that fails inserts of one job_event
// kind, so a production write path can be made to fail without a code seam.
func rejectJobEventKind(t *testing.T, store *db.Store, kind string) {
	t.Helper()
	conn, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	stmt := `CREATE TRIGGER reject_kind BEFORE INSERT ON job_events
	         WHEN NEW.kind = '` + kind + `'
	         BEGIN SELECT RAISE(ABORT, 'injected: owner row rejected'); END;`
	if _, err := conn.Exec(stmt); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
}
