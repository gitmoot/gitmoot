package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// blockingWorktreeManager refuses allocation with a BlockedError, which is the
// production trigger for the resolution transaction's ALTERNATIVE OUTCOME.
type blockingWorktreeManager struct {
	fakeWorktreeManager
}

func (b *blockingWorktreeManager) AddWorktree(_ context.Context, _ string, _ string, _ string) error {
	return BlockedError{Reason: "worktree allocation refused for the test"}
}

// TestRefusedAllocationBlocksUnderTheFenceAndNeverRepeats covers P1-1
// (engine.go:464). The refused-allocation branch must be REACHABLE from production:
// the block and its task_event commit under the fence with NO receipt and NO prepared
// jobs, a pass that lost the fence lands nothing at all, and a recovery pass cannot
// repeat the block.
func TestRefusedAllocationBlocksUnderTheFenceAndNeverRepeats(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedImplementEscalation(t, store, &engine)
	// Swap in a manager that refuses, so the refusal comes from the production path
	// rather than from a test hook.
	engine.DelegationWorktrees = &blockingWorktreeManager{}

	blockedErr := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, "")
	var blocked BlockedError
	if !errors.As(blockedErr, &blocked) {
		t.Fatalf("ResolveEscalation error = %v, want BlockedError from the refused allocation", blockedErr)
	}

	// THE BLOCK LANDED, under the fence, with its event.
	task, err := store.GetTask(ctx, "task-5")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(TaskBlocked) {
		t.Fatalf("task state = %q, want blocked", task.State)
	}
	blockEvents := 0
	events, err := store.ListTaskEvents(ctx, "task-5")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == "workflow_blocked" {
			blockEvents++
		}
	}
	if blockEvents != 1 {
		t.Fatalf("workflow_blocked task events = %d, want exactly 1", blockEvents)
	}

	// NO RECEIPT and NO DISPATCH: a refused decision keeps its claim and queues nothing.
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0: a refused decision must not be recorded as applied", got)
	}
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0: a refused decision must dispatch nothing", got)
	}
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("the refusal settled the round: the human decision is gone")
	}
	if !round.Claimed() {
		t.Fatal("the refusal discarded the claim")
	}

	// AND IT CANNOT REPEAT. Recovery re-drives the same refusal; the block must not
	// be written a second time.
	if _, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil && !errors.As(err, &blocked) {
		t.Fatalf("recovery pass: %v", err)
	}
	events, err = store.ListTaskEvents(ctx, "task-5")
	if err != nil {
		t.Fatalf("ListTaskEvents after recovery: %v", err)
	}
	repeat := 0
	for _, event := range events {
		if event.Kind == "workflow_blocked" {
			repeat++
		}
	}
	if repeat != 1 {
		t.Fatalf("workflow_blocked task events after recovery = %d, want still exactly 1", repeat)
	}
}

// TestTerminalTaskWinnerRefusesTheWholeResolution covers P1-2
// (store_escalation_rounds.go:544). If another worker moves the task to a terminal
// state between capture and commit, the guarded task write affects zero rows - and
// that single boolean must abort EVERYTHING: no prepared job, no event, no receipt.
func TestTerminalTaskWinnerRefusesTheWholeResolution(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedImplementEscalation(t, store, &engine)

	// The concurrent winner lands in the capture/commit seam, through the same hook
	// the crash tests use, so the race is deterministic.
	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error {
		task, err := store.GetTask(hookCtx, "task-5")
		if err != nil {
			t.Fatalf("hook GetTask: %v", err)
		}
		task.State = string(TaskMerged)
		if err := store.UpsertTask(hookCtx, task); err != nil {
			t.Fatalf("hook UpsertTask: %v", err)
		}
		return nil
	}
	t.Cleanup(func() { resolutionEffectsHook = nil })

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}

	// THE WINNER KEPT THE TASK.
	task, err := store.GetTask(ctx, "task-5")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(TaskMerged) {
		t.Fatalf("task state = %q, want merged: the resolution overwrote a terminal winner", task.State)
	}
	// AND NOTHING ELSE LANDED.
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0: stale work was dispatched against a terminal lifecycle", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0: the decision was recorded as applied when it was not", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", "delegation_escalation_retry"); got != 0 {
		t.Fatalf("verb events = %d, want 0", got)
	}
	if _, ok := unsettledRound(t, store, "parent-job"); !ok {
		t.Fatal("the refused commit settled the round anyway")
	}
}

// TestPreEffectOwnershipLossHandsBackResources covers P1-3
// (engine_escalation_resume.go:733). Long external pre-effects must hold ownership:
// a lease that lapses while git work runs means RecordEscalationRoundPreEffects
// refuses, and that boolean must be treated as OWNERSHIP LOSS - no effects, and the
// branch lock handed back rather than stranded.
func TestPreEffectOwnershipLossHandsBackResources(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedImplementEscalation(t, store, &engine)
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no unsettled round")
	}

	// Ownership is taken away DURING the pre-effects, exactly as a lapsed lease
	// followed by another recoverer would do.
	resolutionEffectsHook = func(hookCtx context.Context, jobID string) error {
		now := time.Now().UTC()
		taken, err := store.AcquireEscalationRecoveryLease(hookCtx, "parent-job", round.RoundID, "other-recoverer",
			now.Add(time.Minute), now.Add(2*escalationRecoveryLeaseTTL))
		if err != nil {
			t.Fatalf("hook acquire: %v", err)
		}
		if !taken {
			t.Fatal("the hook could not take ownership: the test cannot observe loss")
		}
		return nil
	}
	t.Cleanup(func() { resolutionEffectsHook = nil })

	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeRetry, ""); err != nil {
		t.Fatalf("ResolveEscalation: %v", err)
	}

	// NO EFFECTS from a pass that lost ownership.
	if got := countJobs(t, store, "/resume"); got != 0 {
		t.Fatalf("resume jobs = %d, want 0 after ownership loss", got)
	}
	if got := countWorkflowJobEvents(t, store, "parent-job", escalationEffectsCompletedEvent); got != 0 {
		t.Fatalf("receipts = %d, want 0 after ownership loss", got)
	}
	// THE LOCK WAS HANDED BACK: the new owner must be able to take the same branch.
	acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot",
		Branch:       round.PreEffectBranch,
		Owner:        "other-recoverer",
	})
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if !acquired && strings.TrimSpace(round.PreEffectBranch) != "" {
		t.Fatal("the lost pass stranded its branch lock: the new owner cannot proceed")
	}
}

// TestReleasingAFenceYouDoNotOwnReportsFalse covers the tenth guarded write,
// ReleaseEscalationRecoveryLease, whose affected-row count was previously discarded
// inside the store. Zero rows is legitimate - but it must be REPORTED, so a caller can
// tell "I released it" from "it was never mine".
func TestReleasingAFenceYouDoNotOwnReportsFalse(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	pausedImplementEscalation(t, store, &engine)
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok {
		t.Fatal("no unsettled round")
	}

	now := time.Now().UTC()
	held, err := store.AcquireEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "owner-a",
		now.Add(escalationRecoveryLeaseTTL), now)
	if err != nil {
		t.Fatalf("AcquireEscalationRecoveryLease: %v", err)
	}
	if !held {
		t.Fatal("owner-a did not take the fence")
	}

	// A NON-OWNER releases nothing and is told so.
	released, err := store.ReleaseEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "owner-b")
	if err != nil {
		t.Fatalf("release by non-owner: %v", err)
	}
	if released {
		t.Fatal("a non-owner released someone else's fence")
	}
	// AND THE FENCE IS STILL HELD: owner-b cannot take it.
	stolen, err := store.AcquireEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "owner-b",
		now.Add(escalationRecoveryLeaseTTL), now)
	if err != nil {
		t.Fatalf("AcquireEscalationRecoveryLease(b): %v", err)
	}
	if stolen {
		t.Fatal("the fence was taken after a failed release")
	}

	// THE OWNER releases, and is told it happened.
	released, err = store.ReleaseEscalationRecoveryLease(ctx, "parent-job", round.RoundID, "owner-a")
	if err != nil {
		t.Fatalf("release by owner: %v", err)
	}
	if !released {
		t.Fatal("the owner's release reported false")
	}
}
