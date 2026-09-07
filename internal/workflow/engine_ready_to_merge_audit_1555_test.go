package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1555, as re-scoped by its own 2026-09-03 verification: (a) a Deferred merge
// decision must not become ready_to_merge while a fix leg holds the branch, and
// (b) the ready_to_merge transition must emit a task_event.
//
// MEASURED ON PR #1546. A reviewer reproduced a host escape with a compiled
// probe ("a temporary implemented second backend executed locally and finished
// succeeded"), the engine dispatched the fix leg for it at 20:18:04, the verdict
// landed at 20:18:06, and at 20:21:12 two later approvals drove the task to
// ready_to_merge WHILE that leg was still running. `task_events` held ZERO rows
// for the transition. The only operation the engine would then accept was the
// merge, so a coordinator reading `gitmoot task list` plus an approval count
// would have shipped the escape.
//
// Two of the issue's four original asks were WITHDRAWN by its author after
// checking: the fix-pass refusal was correct and protective, and the escape
// hatch (`task dismiss`/`recover`/`resume-work`/`events`) exists. Neither is
// touched here.
func TestDeferredMergeDoesNotCallATaskMergeableWhileAWriterHoldsTheBranch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	ref := seedReadyToMergeTask(t, store, TaskChangesRequested)

	engine.MergeGate = stubMergeGate{decision: MergeDecision{
		Deferred:   true,
		BlockClass: MergeBlockTransient,
		Reason:     PlainReason("active implement job fix-leg-1 in flight on branch task-1546; holding merge until it settles"),
		HeldByJob:  "fix-leg-1",
	}}

	if _, err := engine.runMergeGate(ctx, "reviewer", readyToMergePayload(), ref, TaskChangesRequested); err != nil {
		t.Fatalf("runMergeGate: %v", err)
	}

	task, err := store.GetTask(ctx, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State == string(TaskReadyToMerge) {
		t.Fatal("the task was called ready_to_merge while a writer still owned the branch and an objection stood at this head; a task cannot be mergeable and under repair at once")
	}
	if task.State != string(TaskChangesRequested) {
		t.Fatalf("task state = %q, want the arriving %q preserved", task.State, TaskChangesRequested)
	}
	assertTaskEvent(t, store, ref.ID, "merge_deferred_branch_held", "fix-leg-1")
}

// (b), and the half that is pure addition: the transition that DOES happen must
// be auditable. Without this an operator sees the strongest merge signal the
// engine produces and can never learn what produced it.
func TestReadyToMergeTransitionsAreRecordedInTaskEvents(t *testing.T) {
	ctx := context.Background()

	t.Run("ready", func(t *testing.T) {
		store := openEngineStore(t)
		engine := testEngine(store)
		ref := seedReadyToMergeTask(t, store, TaskReviewing)
		engine.MergeGate = stubMergeGate{decision: MergeDecision{
			Ready:  true,
			Reason: PlainReason("every required reviewer approved at the evaluated head"),
		}}
		if _, err := engine.runMergeGate(ctx, "reviewer", readyToMergePayload(), ref, TaskReviewing); err != nil {
			t.Fatalf("runMergeGate: %v", err)
		}
		assertTaskState(t, store, ref.ID, TaskReadyToMerge)
		assertTaskEvent(t, store, ref.ID, "task_ready_to_merge", "every required reviewer approved")
	})

	// A deferral that is NOT a branch hold still parks in ready_to_merge, because
	// that is what the merge poll re-drives. It must say so rather than looking
	// like a clean approval: same state, different recorded reason.
	t.Run("deferred_without_a_branch_hold", func(t *testing.T) {
		store := openEngineStore(t)
		engine := testEngine(store)
		ref := seedReadyToMergeTask(t, store, TaskReviewing)
		engine.MergeGate = stubMergeGate{decision: MergeDecision{
			Deferred:   true,
			BlockClass: MergeBlockTransient,
			Reason:     PlainReason("pull request head is not observable yet"),
		}}
		if _, err := engine.runMergeGate(ctx, "reviewer", readyToMergePayload(), ref, TaskReviewing); err != nil {
			t.Fatalf("runMergeGate: %v", err)
		}
		assertTaskState(t, store, ref.ID, TaskReadyToMerge)
		assertTaskEvent(t, store, ref.ID, "task_ready_to_merge_deferred", "not observable yet")
	})
}

// The bound on (a), and the case that decides whether this is a scoped refusal
// or a liveness regression: a branch hold on a task that is NOT carrying an
// objection must still park in ready_to_merge, or a transient hold on an
// ordinary reviewing task would stop being re-driven by the merge poll.
func TestBranchHeldDeferralStillParksATaskThatCarriesNoObjection(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	ref := seedReadyToMergeTask(t, store, TaskReviewing)
	engine.MergeGate = stubMergeGate{decision: MergeDecision{
		Deferred:   true,
		BlockClass: MergeBlockTransient,
		Reason:     PlainReason("active ask job helper-1 in flight on branch task-1546; holding merge until it settles"),
		HeldByJob:  "helper-1",
	}}
	if _, err := engine.runMergeGate(ctx, "reviewer", readyToMergePayload(), ref, TaskReviewing); err != nil {
		t.Fatalf("runMergeGate: %v", err)
	}
	assertTaskState(t, store, ref.ID, TaskReadyToMerge)
	assertTaskEvent(t, store, ref.ID, "task_ready_to_merge_deferred", "helper-1")
}

type stubMergeGate struct {
	decision MergeDecision
}

func (s stubMergeGate) Evaluate(context.Context, MergeRequest) (MergeDecision, error) {
	return s.decision, nil
}

func seedReadyToMergeTask(t *testing.T, store *db.Store, state TaskState) taskRef {
	t.Helper()
	task := db.Task{
		ID:           "task-1546",
		RepoFullName: "gitmoot/gitmoot",
		GoalID:       "goal-1546",
		Title:        "E2B execution backend",
		Branch:       "task-1546",
		State:        string(state),
	}
	if err := store.UpsertTask(context.Background(), task); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	return taskRef{ID: task.ID, Repo: task.RepoFullName, GoalID: task.GoalID, Title: task.Title, Branch: task.Branch}
}

func readyToMergePayload() JobPayload {
	return JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1546",
		PullRequest: 1546,
		HeadSHA:     "36f7c70122b3",
		TaskID:      "task-1546",
		GoalID:      "goal-1546",
		LeadAgent:   "wave-impl",
	}
}

func assertTaskEvent(t *testing.T, store *db.Store, taskID string, kind string, mustContain string) {
	t.Helper()
	events, err := store.ListTaskEvents(context.Background(), taskID)
	if err != nil {
		t.Fatalf("ListTaskEvents(%s): %v", taskID, err)
	}
	for _, event := range events {
		if event.Kind == kind && strings.Contains(event.Reason, mustContain) {
			return
		}
	}
	t.Fatalf("task %s has no %q event whose reason contains %q; the transition is unauditable. events = %+v", taskID, kind, mustContain, events)
}
