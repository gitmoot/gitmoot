package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// exactHeadReviewWorktreeManager is a WorktreeManager + ReadOnlyWorktreeManager
// that mirrors the ONE property of `git worktree add` these tests depend on: it
// REFUSES an already-occupied path. Without that refusal a re-poll that
// re-allocates the same deterministic path looks like a success in the fake and
// hides the exact wedge the pre-allocation idempotence check exists to prevent.
type exactHeadReviewWorktreeManager struct {
	occupied map[string]bool
	added    []worktreeCall
	removed  []string
	fetched  []int
	addErr   error
	fetchErr error
	// fetchClears makes the post-fetch retry succeed, modelling a cold checkout
	// that only lacked the PR commit object.
	fetchClears bool
}

func newExactHeadReviewWorktreeManager() *exactHeadReviewWorktreeManager {
	return &exactHeadReviewWorktreeManager{occupied: map[string]bool{}}
}

func (m *exactHeadReviewWorktreeManager) AddWorktree(context.Context, string, string, string) error {
	return errors.New("exactHeadReviewWorktreeManager allocates no branch worktrees")
}

func (m *exactHeadReviewWorktreeManager) AddDetachedWorktree(_ context.Context, path string, ref string) error {
	if m.addErr != nil {
		return m.addErr
	}
	if m.occupied[path] {
		return fmt.Errorf("fatal: '%s' already exists", path)
	}
	m.occupied[path] = true
	m.added = append(m.added, worktreeCall{path: path, base: ref})
	return nil
}

func (m *exactHeadReviewWorktreeManager) RemoveWorktreeForce(_ context.Context, path string) error {
	delete(m.occupied, path)
	m.removed = append(m.removed, path)
	return nil
}

func (m *exactHeadReviewWorktreeManager) FetchPullRequest(_ context.Context, _ string, number int) error {
	m.fetched = append(m.fetched, number)
	if m.fetchErr != nil {
		return m.fetchErr
	}
	if m.fetchClears {
		m.addErr = nil
	}
	return nil
}

func nativeReviewFanoutEngine(t *testing.T, store *db.Store, manager *exactHeadReviewWorktreeManager) Engine {
	t.Helper()
	engine := testEngine(store)
	engine.Home = t.TempDir()
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager
	return engine
}

func routineReviewFanoutEvent() PullRequestEvent {
	return PullRequestEvent{
		Repo:              "gitmoot/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		HeadSHA:           "head123",
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		RequiredReviewers: []string{"audit"},
	}
}

// TestRoutineReviewLegIsEnqueuedWithExactHeadWorktree pins F1's precondition and
// F2's mechanism at once: a ROUTINE native review leg must be BORN carrying its
// exact-head WorktreePath, the ReadOnlyWorktree marker and the read-only context
// note, because queuedJobCheckoutKey reads payload.WorktreePath at scheduler
// admission and payloadMatchesRequest compares Instructions + WorktreePath on
// every re-derivation.
//
// MUTATION PROOF: make prepareNativeReviewWorktree return the request untouched
// (the pre-fix worker-allocates-after-enqueue shape) and every assertion below
// flips RED — no worktree is allocated and the payload carries neither the path
// nor the note.
func TestRoutineReviewLegIsEnqueuedWithExactHeadWorktree(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	manager := newExactHeadReviewWorktreeManager()
	engine := nativeReviewFanoutEngine(t, store, manager)

	if err := engine.HandlePullRequestOpened(ctx, routineReviewFanoutEvent()); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	job := mustJob(t, store, "review-audit-task-7-review-1")
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if len(manager.added) != 1 {
		t.Fatalf("detached worktree allocations = %+v, want exactly one", manager.added)
	}
	if payload.WorktreePath != manager.added[0].path || payload.WorktreePath == "" {
		t.Fatalf("payload worktree = %q, want the allocated %q", payload.WorktreePath, manager.added[0].path)
	}
	// The worktree must be pinned to the EXACT review head, not the branch tip: a
	// branch-tip worktree reviews a commit other than the one the verdict claims.
	if manager.added[0].base != "head123" {
		t.Fatalf("worktree ref = %q, want the exact review head head123", manager.added[0].base)
	}
	if !payload.ReadOnlyWorktree {
		t.Fatal("payload is missing the ReadOnlyWorktree marker; terminal cleanup would orphan the worktree")
	}
	if want := filepath.Join(engine.Home, "worktrees"); !strings.HasPrefix(payload.WorktreePath, want) {
		t.Fatalf("worktree %q is not under the managed root %q", payload.WorktreePath, want)
	}
	note := ReadOnlyWorktreeContextNote(engine.DelegationCheckout)
	if note == "" || !strings.HasSuffix(payload.Instructions, note) {
		t.Fatalf("payload instructions do not end with the read-only worktree note: %q", payload.Instructions)
	}
	if got := countJobEvents(t, store, job.ID, "review_worktree_allocated_exact_head"); got != 1 {
		t.Fatalf("review_worktree_allocated_exact_head events = %d, want 1", got)
	}
}

// TestRoutineReviewRepollAtSameHeadIsIdempotent is F1 itself. The round is STABLE
// across re-polls at one head (nextReviewRound returns the existing head's
// round), so the deterministic job id is re-derived identically and the second
// HandlePullRequestOpened must be a NO-OP — no second job, no second worktree, and
// above all no raw "UNIQUE constraint failed: jobs.id" escaping to the caller.
//
// MUTATION PROOF: delete the existingJobMatchesRequest pre-check in
// prepareNativeReviewWorktree (so the allocation runs before idempotence is
// decided) and the second call fails with
// `fatal: '<path>' already exists` — the fake refuses an occupied path exactly as
// git does.
func TestRoutineReviewRepollAtSameHeadIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	manager := newExactHeadReviewWorktreeManager()
	engine := nativeReviewFanoutEngine(t, store, manager)
	event := routineReviewFanoutEvent()

	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("first HandlePullRequestOpened: %v", err)
	}
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("second HandlePullRequestOpened at the same head: %v", err)
	}
	if got := reviewJobCount(t, store); got != 1 {
		t.Fatalf("review jobs = %d, want 1 after two polls at the same head", got)
	}
	if len(manager.added) != 1 {
		t.Fatalf("detached worktree allocations = %+v, want exactly one across two polls", manager.added)
	}
	if got := countJobEvents(t, store, "review-audit-task-7-review-1", "review_worktree_allocated_exact_head"); got != 1 {
		t.Fatalf("review_worktree_allocated_exact_head events = %d, want exactly one per worktree", got)
	}
}

// TestRoutineReviewLegsGetDistinctWorktreesPerReviewer is F2's engine-side half:
// N reviewers must be born with N DISTINCT paths, because a shared path would
// collapse them back onto one checkout key at admission.
//
// MUTATION PROOF: derive the path from the repo instead of the job id (drop
// request.ID from the DelegationWorktreePath call) and the second reviewer's
// allocation collides on the occupied path.
func TestRoutineReviewLegsGetDistinctWorktreesPerReviewer(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "second-audit", []string{"review"}, "gitmoot/gitmoot")
	manager := newExactHeadReviewWorktreeManager()
	engine := nativeReviewFanoutEngine(t, store, manager)
	event := routineReviewFanoutEvent()
	event.RequiredReviewers = []string{"audit", "second-audit"}

	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	if len(manager.added) != 2 {
		t.Fatalf("detached worktree allocations = %+v, want one per reviewer", manager.added)
	}
	if manager.added[0].path == manager.added[1].path {
		t.Fatalf("both reviewer legs share worktree %q", manager.added[0].path)
	}
	for _, jobID := range []string{"review-audit-task-7-review-1", "review-second-audit-task-7-review-1"} {
		job := mustJob(t, store, jobID)
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			t.Fatalf("unmarshalPayload %s: %v", jobID, err)
		}
		if payload.WorktreePath == "" || !payload.ReadOnlyWorktree {
			t.Fatalf("reviewer leg %s payload = %+v, want its own read-only worktree", jobID, payload)
		}
	}
}

// TestRoutineReviewAllocationRetriesAfterFetchingPullRequestRef pins the cold
// checkout recovery: the forge supplies the head SHA but the local checkout may
// not carry that commit object, and nothing on the poll path fetches it. Without
// the retry the fan-out returns on the FIRST reviewer and dispatches nothing, at a
// head that would retry to the same result.
//
// MUTATION PROOF: drop the fetch+retry arm from allocateNativeReviewWorktree and
// HandlePullRequestOpened returns the invalid-reference error with zero jobs.
func TestRoutineReviewAllocationRetriesAfterFetchingPullRequestRef(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	manager := newExactHeadReviewWorktreeManager()
	manager.addErr = errors.New("fatal: invalid reference: head123")
	manager.fetchClears = true
	engine := nativeReviewFanoutEngine(t, store, manager)

	if err := engine.HandlePullRequestOpened(ctx, routineReviewFanoutEvent()); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	if len(manager.fetched) != 1 || manager.fetched[0] != 7 {
		t.Fatalf("pull/<n>/head fetches = %v, want exactly [7]", manager.fetched)
	}
	job := mustJob(t, store, "review-audit-task-7-review-1")
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.WorktreePath == "" || !payload.ReadOnlyWorktree {
		t.Fatalf("payload after fetch retry = %+v, want an allocated read-only worktree", payload)
	}
}

// TestRoutineReviewLockContentionFailsThePollWithoutFetching pins the transient
// arm: a spent checkout-mutation-lock budget is contention, so the poll fails and
// the daemon re-fires HandlePullRequestOpened (which re-derives the same id and
// re-attempts). No job is created, and no pointless network fetch is spent on a
// failure a fetch cannot fix.
//
// MUTATION PROOF: remove the BlockedError arm from allocateNativeReviewWorktree
// and the fetch assertion flips RED — the contention path spends a fetch and then
// wraps a second lock failure.
func TestRoutineReviewLockContentionFailsThePollWithoutFetching(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	manager := newExactHeadReviewWorktreeManager()
	engine := nativeReviewFanoutEngine(t, store, manager)
	release, err := AcquireCheckoutMutationLock(ctx, store, engine.DelegationCheckout, "other-worker", time.Now().UTC())
	if err != nil {
		t.Fatalf("AcquireCheckoutMutationLock: %v", err)
	}
	defer func() {
		if err := release(context.Background()); err != nil {
			t.Fatalf("release checkout mutation lock: %v", err)
		}
	}()

	err = engine.HandlePullRequestOpened(ctx, routineReviewFanoutEvent())
	if err == nil {
		t.Fatal("HandlePullRequestOpened succeeded while the checkout mutation lock was held")
	}
	if !CheckoutMutationLockContention(err) {
		t.Fatalf("error = %v, want a checkout-mutation-lock BlockedError the daemon can defer on", err)
	}
	if len(manager.fetched) != 0 {
		t.Fatalf("fetches = %v, want none: a fetch cannot clear lock contention", manager.fetched)
	}
	if got := reviewJobCount(t, store); got != 0 {
		t.Fatalf("review jobs = %d, want 0: the leg must be re-derivable, not half-created", got)
	}
}

// TestRoutineReviewLegKeepsWorkerAllocationWhenEngineHasNoWorktreeManager pins the
// deliberate configuration split. An engine with no read-only worktree manager
// must be left BYTE-IDENTICAL — the leg is enqueued path-less and the daemon
// worker's prepareNativeReviewWorktreeForRunner keeps covering it. That is what
// makes the two allocation paths disjoint rather than two live paths for one
// configuration.
//
// MUTATION PROOF: drop the manager/Home/DelegationCheckout guard from
// prepareNativeReviewWorktree and this leg is either born with a path it has no
// manager to create or the poll fails outright.
func TestRoutineReviewLegKeepsWorkerAllocationWhenEngineHasNoWorktreeManager(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	if err := engine.HandlePullRequestOpened(ctx, routineReviewFanoutEvent()); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	job := mustJob(t, store, "review-audit-task-7-review-1")
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.WorktreePath != "" || payload.ReadOnlyWorktree {
		t.Fatalf("unwired engine payload = %+v, want no worktree so the worker still allocates it", payload)
	}
	// The worker helper's gate is what keeps covering this shape; the note must be
	// absent too, or the worker's own append would double it.
	if strings.Contains(payload.Instructions, "Worktree context (read-only)") {
		t.Fatalf("unwired engine appended the worktree note: %q", payload.Instructions)
	}
}

// TestRoutineReviewWorktreeIsReleasedWhenEnqueueFails closes the window the
// pre-enqueue allocation opens: the worktree path is DETERMINISTIC, so a worktree
// allocated for a job that then fails to enqueue would make every later poll's
// allocation fail with "already exists" — a permanently wedged head, worse than
// the transient enqueue error that started it.
//
// MUTATION PROOF: drop the releaseNativeReviewWorktree call from the enqueue-error
// branch and the path stays occupied, so the retry below fails.
func TestRoutineReviewWorktreeIsReleasedWhenEnqueueFails(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	manager := newExactHeadReviewWorktreeManager()
	engine := nativeReviewFanoutEngine(t, store, manager)
	// Collide the derived job id with an unrelated existing job, so the insert
	// violates jobs.id and the comparator legitimately refuses to treat it as the
	// same request.
	engine.JobID = func(JobRequest) string { return "occupied-job" }
	insertCompletedJob(t, store, db.Job{ID: "occupied-job", Agent: "other", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7",
	})

	if err := engine.HandlePullRequestOpened(ctx, routineReviewFanoutEvent()); err == nil {
		t.Fatal("HandlePullRequestOpened succeeded despite the colliding job id")
	}
	if len(manager.added) != 1 {
		t.Fatalf("allocations = %+v, want the one this poll made", manager.added)
	}
	path := manager.added[0].path
	if len(manager.removed) != 1 || manager.removed[0] != path {
		t.Fatalf("removals = %v, want the orphaned worktree %q reclaimed", manager.removed, path)
	}
	// The proof that matters: the deterministic path is free, so a later poll can
	// still allocate it once the collision is resolved.
	if err := manager.AddDetachedWorktree(ctx, path, "head123"); err != nil {
		t.Fatalf("deterministic path is still occupied after a failed enqueue: %v", err)
	}
}
