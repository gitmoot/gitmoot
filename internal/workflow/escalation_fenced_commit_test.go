package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

// pausedImplementEscalation seeds a paused escalation whose failing leg is an
// IMPLEMENT delegation. That distinction is the whole point: only an implement leg
// runs PRE-EFFECTS (worktree allocation, branch lock), so a fixture built on a review
// leg cannot observe them and any pre-effect assertion against it is vacuous (#1673).
func pausedImplementEscalation(t *testing.T, store *db.Store, engine *Engine) *fakeWorktreeManager {
	// engine is taken by pointer because the fixture WIRES it: Home, DelegationCheckout
	// and the worktree manager are what make an implement leg run pre-effects at all.
	t.Helper()
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "builder", []string{"implement"}, "gitmoot/gitmoot")
	manager := &fakeWorktreeManager{}
	engine.Home = t.TempDir()
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
				{ID: "impl", Agent: "builder", Action: "implement", Prompt: "build it", FailurePolicy: "escalate_human"},
			},
		},
	})
	if err := engine.AdvanceJob(context.Background(), "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent): %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/impl", JobFailed,
		AgentResult{Decision: "failed", Summary: "build broke"})
	if err := engine.AdvanceJob(context.Background(), "parent-job/delegation/impl"); err == nil {
		t.Fatal("expected the failing implement leg to pause on a human")
	}
	return manager
}

// distinctWorktrees counts the DISTINCT worktree paths a fixture allocated. The count
// that matters is one of RESOURCES, not one of invocations: re-running an allocation
// keyed by home/repo/parent/delegation re-uses the same worktree, and that re-use is
// exactly the property that makes a pre-effect safe to replay under the fence (#1673).
func distinctWorktrees(manager *fakeWorktreeManager) []string {
	seen := map[string]bool{}
	paths := []string{}
	for _, call := range manager.calls {
		if seen[call.path] {
			continue
		}
		seen[call.path] = true
		paths = append(paths, call.path)
	}
	return paths
}

// countJobs counts jobs whose id carries the given substring, which is how a
// duplicated enqueue becomes visible as a COUNT rather than as an eventual "at least
// one" assertion (#1673).
func countJobs(t *testing.T, store *db.Store, substr string) int {
	t.Helper()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	n := 0
	for _, job := range jobs {
		if strings.Contains(job.ID, substr) {
			n++
		}
	}
	return n
}

// TestFencedResolutionCrashKeepsOneOfEverythingAndReusesTheAllocation is the sequence
// directive 105303 named: crash inside the effect window AFTER the pre-effects, twice,
// then let a later pass succeed. Exactly one of everything must exist, and the
// surviving passes must RE-USE the recorded allocation rather than allocate again.
//
// It also pins that a failing pass RELEASES ITS FENCE: recovery proceeds on the next
// poll rather than waiting out the lease.
func TestFencedResolutionCrashKeepsOneOfEverythingAndReusesTheAllocation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	manager := pausedImplementEscalation(t, store, &engine)

	boom := errors.New("crash inside the effect window")
	crashes := 0
	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error {
		if crashes >= 2 {
			return nil
		}
		crashes++
		return boom
	}
	t.Cleanup(func() { resolutionEffectsHook = nil })

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); !errors.Is(err, boom) {
		t.Fatalf("ResolveEscalation error = %v, want the injected crash", err)
	}

	// NOTHING landed: the transaction is all-or-nothing and the claim is preserved.
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs after crash = %d, want 0", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts after crash = %d, want 0", got)
	}
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("the crash discarded the claim: no unsettled round survived")
	}
	if !round.Claimed() {
		t.Fatal("the preserved round lost its claim")
	}
	if task, err := store.GetTask(ctx, "task-5"); err != nil {
		t.Fatalf("GetTask: %v", err)
	} else if task.State != string(TaskAwaitingHuman) {
		t.Fatalf("task state after crash = %q, want awaiting_human: the task write escaped the transaction", task.State)
	}
	worktreesAfterCrash := distinctWorktrees(manager)
	if len(worktreesAfterCrash) == 0 {
		t.Fatal("no worktree was allocated: this fixture cannot observe pre-effects")
	}

	// NO MANUAL RELEASE: the failing pass must hand back its own fence, so this next
	// pass proceeds immediately instead of waiting out the lease.
	if _, err := engine.RecoverUnfinishedEscalationResolutions(ctx); !errors.Is(err, boom) {
		t.Fatalf("second pass error = %v, want the injected crash (a held fence would skip it)", err)
	}
	if got := distinctWorktrees(manager); len(got) != len(worktreesAfterCrash) || got[0] != worktreesAfterCrash[0] {
		t.Fatalf("worktrees = %v, want the replay to RE-USE %v", got, worktreesAfterCrash)
	}

	recovered, err := engine.RecoverUnfinishedEscalationResolutions(ctx)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}

	// EXACTLY ONE OF EVERYTHING.
	if got := countJobs(t, store, "/resume"); got != 1 {
		t.Fatalf("resume jobs = %d, want exactly 1", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 1 {
		t.Fatalf("receipts = %d, want exactly 1", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", "delegation_escalation_retry"); got != 1 {
		t.Fatalf("retry events = %d, want exactly 1", got)
	}
	if got := distinctWorktrees(manager); len(got) != 1 {
		t.Fatalf("distinct worktrees = %v, want exactly 1 across three passes", got)
	}
	if _, stillOpen := unsettledRound(t, store, "parent-job"); stillOpen {
		t.Fatal("the settled round still holds the slot")
	}
}

// TestFencedResolutionFenceLoserAppliesNoEffects is the second sequence: a pass whose
// fence is taken from under it mid-flight must apply ZERO effects - never a
// half-applied decision - and must not settle the round.
func TestFencedResolutionFenceLoserAppliesNoEffects(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	notifier := &recordingNotifier{}
	engine.EscalationNotifier = notifier
	sink := &recordingSink{}
	engine.EventSink = sink
	pausedImplementEscalation(t, store, &engine)
	// BASELINE: the fixture's own pause already announced once. Measuring from zero
	// here is what makes a later count a statement about the RESOLUTION rather than
	// about the fixture - the first version of this test measured the fixture.
	notifier.calls = nil
	sink.mu.Lock()
	sink.events = nil
	sink.mu.Unlock()
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no unsettled round")
	}

	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error {
		now := time.Now().UTC()
		if _, err := store.AcquireEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "thief",
			now.Add(time.Minute), now.Add(2*escalationRecoveryLeaseTTL)); err != nil {
			t.Fatalf("steal the fence: %v", err)
		}
		return nil
	}
	t.Cleanup(func() { resolutionEffectsHook = nil })

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0: a fence loser applies no effects", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0 for a fence loser", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", "delegation_escalation_retry"); got != 0 {
		t.Fatalf("verb events = %d, want 0 for a fence loser", got)
	}
	if _, stillOpen := unsettledRound(t, store, "parent-job"); !stillOpen {
		t.Fatal("a fence loser settled the round")
	}
	// EVERY ANNOUNCEMENT IS GATED ON WINNING. A loser that notifies has told a human
	// about a decision that was never applied - the failure mode the openers already
	// close (#1673).
	if got := len(notifier.calls); got != 0 {
		t.Fatalf("a fence loser sent %d escalation notifications, want 0", got)
	}
	if got := len(sink.byType(events.EventJobNeedsAttention)); got != 0 {
		t.Fatalf("a fence loser emitted %d needs-attention events, want 0: %+v", got, sink.byType(events.EventJobNeedsAttention))
	}
}

// TestFencedResolutionOnlyTheHolderRunsPreEffects is the third sequence, and the one
// that decides the ORDER: only the lease holder may allocate. An idempotent worktree
// key does not protect a branch lock that has an owner, which is why the fence is
// taken BEFORE the pre-effects rather than after them.
func TestFencedResolutionOnlyTheHolderRunsPreEffects(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	manager := pausedImplementEscalation(t, store, &engine)

	boom := errors.New("crash inside the effect window")
	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error { return boom }
	t.Cleanup(func() { resolutionEffectsHook = nil })
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); !errors.Is(err, boom) {
		t.Fatalf("ResolveEscalation error = %v, want the injected crash", err)
	}
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("the claim did not survive the crash")
	}
	allocations := len(distinctWorktrees(manager))
	if allocations == 0 {
		t.Fatal("no worktree was allocated: this fixture cannot observe pre-effects")
	}

	// Another pass holds the fence. Everything below is a NON-HOLDER.
	now := time.Now().UTC()
	held, err := store.AcquireEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "recoverer-a",
		now.Add(escalationRecoveryLeaseTTL), now)
	if err != nil {
		t.Fatalf("AcquireEscalationRecoveryLease(a): %v", err)
	}
	if !held {
		t.Fatal("the first recoverer did not take the fence")
	}
	second, err := store.AcquireEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "recoverer-b",
		now.Add(escalationRecoveryLeaseTTL), now)
	if err != nil {
		t.Fatalf("AcquireEscalationRecoveryLease(b): %v", err)
	}
	if second {
		t.Fatal("two passes hold the fence at once: both would run pre-effects")
	}

	recovered, err := engine.RecoverUnfinishedEscalationResolutions(ctx)
	if err != nil {
		t.Fatalf("non-holder recovery: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("a non-holder recovered %d rounds, want 0", recovered)
	}
	if got := len(distinctWorktrees(manager)); got != allocations {
		t.Fatalf("a non-holder allocated: distinct worktrees %d -> %d", allocations, got)
	}
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("a non-holder produced %d jobs, want 0", got)
	}
}
