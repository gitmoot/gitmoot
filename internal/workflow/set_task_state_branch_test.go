package workflow

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
)

// TestSetTaskStateBranchConflict pins the fix for the reported
// "UNIQUE constraint failed: tasks.repo_full_name, tasks.branch" during workflow
// advancement. When advancement calls setTaskState with a ref whose branch is already
// owned by a DIFFERENT task (a fresh task id re-running a phase on the same branch after
// a transient failure), it must advance the branch's canonical task in place rather than
// crash on the tasks(repo_full_name, branch) partial-unique index.
func TestSetTaskStateBranchConflict(t *testing.T) {
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	e := Engine{Store: store}

	const repo = "owner/repo"
	const branch = "council/slug-v1"

	// Seed the branch's canonical task.
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: repo, GoalID: "g1", Title: "impl", State: string(TaskImplementing), Branch: branch}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Advancement with a DIFFERENT (fresh) id on the same branch must NOT crash.
	if err := e.setTaskState(ctx, taskRef{ID: "task-fresh", Repo: repo, GoalID: "g1", Title: "review", Branch: branch}, TaskReviewing); err != nil {
		t.Fatalf("setTaskState onto a branch owned by another task must not error, got: %v", err)
	}

	// The branch's canonical task advanced in place; no duplicate was created.
	got, err := store.GetTaskByRepoBranch(ctx, repo, branch)
	if err != nil {
		t.Fatalf("GetTaskByRepoBranch: %v", err)
	}
	if got.ID != "task-a" {
		t.Errorf("branch task id = %q, want task-a (canonical id preserved)", got.ID)
	}
	if got.State != string(TaskReviewing) {
		t.Errorf("branch task state = %q, want %q (advanced in place)", got.State, string(TaskReviewing))
	}
	if _, err := store.GetTask(ctx, "task-fresh"); err == nil {
		t.Errorf("GetTask(task-fresh) unexpectedly succeeded; no duplicate should be created on the taken branch")
	}

	// Sanity: the normal same-id path still advances the task directly.
	if err := e.setTaskState(ctx, taskRef{ID: "task-a", Repo: repo, GoalID: "g1", Title: "impl", Branch: branch}, TaskBlocked); err != nil {
		t.Fatalf("setTaskState same-id: %v", err)
	}
	if got, _ := store.GetTask(ctx, "task-a"); got.State != string(TaskBlocked) {
		t.Errorf("same-id advance state = %q, want %q", got.State, string(TaskBlocked))
	}
}

func TestSetTaskStateCannotResurrectDismissedTask(t *testing.T) {
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	const repo, branch = "owner/repo", "feature/dismissed"
	if err := store.UpsertTask(ctx, db.Task{ID: "canonical", RepoFullName: repo, State: string(TaskDismissed), Branch: branch}); err != nil {
		t.Fatal(err)
	}
	engine := Engine{Store: store}
	for _, ref := range []taskRef{
		{ID: "canonical", Repo: repo, Branch: branch},
		{ID: "late-review", Repo: repo, Branch: branch},
	} {
		if err := engine.setTaskState(ctx, ref, TaskReviewing); err == nil || !strings.Contains(err.Error(), "dismissed") {
			t.Fatalf("setTaskState(%+v) error = %v", ref, err)
		}
	}
	task, _ := store.GetTask(ctx, "canonical")
	if task.State != string(TaskDismissed) {
		t.Fatalf("task resurrected to %s", task.State)
	}
}

func TestSetTaskStateCannotResurrectEvidenceDisposedTask(t *testing.T) {
	for _, terminal := range []TaskState{TaskSuperseded, TaskStranded} {
		t.Run(string(terminal), func(t *testing.T) {
			store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			branch := "feature/" + string(terminal)
			if err := store.UpsertTask(ctx, db.Task{ID: "canonical", RepoFullName: "owner/repo", State: string(terminal), Branch: branch}); err != nil {
				t.Fatal(err)
			}
			engine := Engine{Store: store}
			for _, ref := range []taskRef{
				{ID: "canonical", Repo: "owner/repo", Branch: branch},
				{ID: "late-review", Repo: "owner/repo", Branch: branch},
			} {
				if err := engine.setTaskState(ctx, ref, TaskReviewing); err == nil || !strings.Contains(err.Error(), string(terminal)) {
					t.Fatalf("setTaskState(%+v) error = %v", ref, err)
				}
			}
			stored, _ := store.GetTask(ctx, "canonical")
			if stored.State != string(terminal) {
				t.Fatalf("task resurrected to %s", stored.State)
			}
		})
	}
}

// TestSetTaskStateRefusesMergedRegressionAndLeavesADurableTrace pins the #1673
// state-machine rule. A queued delegation child of an already-merged pull request
// dies in the daemon's closed-PR sweep; the child's advance ends in the parent's
// failure_policy, which writes a state asserting the work is NOT done. Rewriting a
// `merged` task that way would undo the one record the sweep's own premise asserts
// — that the change shipped — so the move is refused.
//
// All three refused targets are covered, because all three are reachable from the
// same dead child: `blocked` (block_parent), `awaiting_human` (escalate_human), and
// `planned` (the escalation resume / TTL sweep that CLEARS an awaiting_human pause —
// the escalation round is recorded even when the pause itself is refused, so
// permitting `planned` would hand the same child the same regression one step later).
//
// It differs from the disposed-state refusals above in the way that matters: it
// returns NO error. The advance that asked for the write still has to finish
// releasing the coordinator, so the refusal cannot be reported by failing it; the
// durable task event IS the report. Both identities setTaskState can resolve are
// covered: the payload task id, and the canonical task owning its branch.
func TestSetTaskStateRefusesMergedRegressionAndLeavesADurableTrace(t *testing.T) {
	for _, refused := range []TaskState{TaskPlanned, TaskBlocked, TaskAwaitingHuman} {
		t.Run(string(refused), func(t *testing.T) {
			store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			const repo, branch = "owner/repo", "feature/merged"
			if err := store.UpsertTask(ctx, db.Task{ID: "canonical", RepoFullName: repo, State: string(TaskMerged), Branch: branch}); err != nil {
				t.Fatal(err)
			}
			engine := Engine{Store: store}
			for _, ref := range []taskRef{
				{ID: "canonical", Repo: repo, Branch: branch},   // payload task id
				{ID: "late-review", Repo: repo, Branch: branch}, // fresh id on the taken branch
			} {
				if err := engine.setTaskState(ctx, ref, refused); err != nil {
					t.Fatalf("setTaskState(%+v, %s) returned error %v; refusing the write must not fail the advance that asked for it", ref, refused, err)
				}
				task, err := store.GetTask(ctx, "canonical")
				if err != nil {
					t.Fatalf("GetTask(canonical) returned error: %v", err)
				}
				if task.State != string(TaskMerged) {
					t.Fatalf("task state = %q after setTaskState(%+v, %s), want merged (the landed-work record must survive)", task.State, ref, refused)
				}
			}
			if _, err := store.GetTask(ctx, "late-review"); err == nil {
				t.Fatal("GetTask(late-review) succeeded; the refusal must not mint a second task on the merged branch")
			}

			// The refusal is silent to the caller, so the trace is the only way anybody
			// learns a write was dropped. One per refused call, on the canonical task.
			events, err := store.ListTaskEvents(ctx, "canonical")
			if err != nil {
				t.Fatalf("ListTaskEvents returned error: %v", err)
			}
			refusals := 0
			for _, event := range events {
				if event.Kind != TaskEventMergedRegressionRefused {
					continue
				}
				refusals++
				if !strings.Contains(event.Reason, string(TaskMerged)) || !strings.Contains(event.Reason, string(refused)) {
					t.Fatalf("%s reason = %q, want both states named", TaskEventMergedRegressionRefused, event.Reason)
				}
			}
			if refusals != 2 {
				t.Fatalf("%s events = %d, want 2 (one per refused identity)", TaskEventMergedRegressionRefused, refusals)
			}
		})
	}
}

// TestSetTaskStateAllowsMergedToNonRegressionStates is the other half of the rule,
// and the one a widened refusal would break. The guard is deliberately limited to
// the three states a dead leg's failure policy can write; it does not freeze every
// other state transition out of merged. The CLI resume-work contract is tested at
// its own entry point and separately rejects merged.
func TestSetTaskStateAllowsMergedToNonRegressionStates(t *testing.T) {
	for _, allowed := range []TaskState{
		TaskImplementing, TaskPullRequestOpen, TaskReviewing, TaskChangesRequested,
		TaskReadyToMerge, TaskAwaitingHumanMerge, TaskMerged,
	} {
		t.Run(string(allowed), func(t *testing.T) {
			store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			const repo, branch = "owner/repo", "feature/merged"
			if err := store.UpsertTask(ctx, db.Task{ID: "canonical", RepoFullName: repo, State: string(TaskMerged), Branch: branch}); err != nil {
				t.Fatal(err)
			}
			engine := Engine{Store: store}
			ref := taskRef{ID: "canonical", Repo: repo, Branch: branch}
			if err := engine.setTaskState(ctx, ref, allowed); err != nil {
				t.Fatalf("setTaskState(canonical, %s) returned error: %v", allowed, err)
			}
			task, err := store.GetTask(ctx, "canonical")
			if err != nil {
				t.Fatalf("GetTask(canonical) returned error: %v", err)
			}
			if task.State != string(allowed) {
				t.Fatalf("task state = %q, want %s; the guard must not freeze merged outright", task.State, allowed)
			}
			events, err := store.ListTaskEvents(ctx, "canonical")
			if err != nil {
				t.Fatalf("ListTaskEvents returned error: %v", err)
			}
			for _, event := range events {
				if event.Kind == TaskEventMergedRegressionRefused {
					t.Fatalf("%s event written for the permitted move merged -> %s", TaskEventMergedRegressionRefused, allowed)
				}
			}
		})
	}
}

// TestWriteTaskStateRefusesMergedWrittenAfterTheRead drives the interleaving the
// first version of this guard lost. setTaskState reads the task, then writes it;
// between those two points a SECOND writer — another daemon, or the same daemon's
// reconcileExternallyMergedTasks earlier in the same PollOnce — can land `merged`.
// An advisory check on the pre-read sees `reviewing`, approves, and the stale
// `blocked` write then overwrites the merge.
//
// The test stands exactly in that window: it takes the pre-read itself (observing a
// non-merged state), lets the second writer win, and only then calls the guarded
// write with the stale snapshot. Nothing in writeTaskState consults the snapshot's
// state, so the refusal has to come from the UPDATE's own WHERE clause; against a
// plain UpsertTask, or against a guard reading the snapshot, `merged` is lost.
func TestWriteTaskStateRefusesMergedWrittenAfterTheRead(t *testing.T) {
	for _, refused := range []TaskState{TaskPlanned, TaskBlocked, TaskAwaitingHuman} {
		t.Run(string(refused), func(t *testing.T) {
			store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			const repo, branch = "owner/repo", "feature/raced"
			seed := db.Task{ID: "canonical", RepoFullName: repo, GoalID: "goal-1", Title: "Parent", State: string(TaskReviewing), Branch: branch}
			if err := store.UpsertTask(ctx, seed); err != nil {
				t.Fatal(err)
			}

			// 1. The read setTaskState takes. It observes `reviewing`: no advisory check
			//    on this value can ever refuse.
			stale, err := store.GetTask(ctx, "canonical")
			if err != nil {
				t.Fatalf("GetTask(canonical) returned error: %v", err)
			}
			if stale.State != string(TaskReviewing) {
				t.Fatalf("pre-read state = %q, want reviewing (the window must open on a permitted state)", stale.State)
			}

			// 2. The other writer wins the race: the pull request merged.
			merged := seed
			merged.State = string(TaskMerged)
			if err := store.UpsertTask(ctx, merged); err != nil {
				t.Fatalf("UpsertTask(merged) returned error: %v", err)
			}

			// 3. The guarded write runs, still holding the stale snapshot. `blocked` now
			//    reaches the store through BlockTaskWithEvent, but `planned` and
			//    `awaiting_human` still come through PersistTaskState, which is the write
			//    point this window is about.
			stale.State = string(refused)
			if _, err := PersistTaskState(ctx, store, stale, refused); err != nil {
				t.Fatalf("PersistTaskState(%s) returned error: %v", refused, err)
			}

			task, err := store.GetTask(ctx, "canonical")
			if err != nil {
				t.Fatalf("GetTask(canonical) returned error: %v", err)
			}
			if task.State != string(TaskMerged) {
				t.Fatalf("task state = %q, want merged: the write raced the merge and won", task.State)
			}
			events, err := store.ListTaskEvents(ctx, "canonical")
			if err != nil {
				t.Fatalf("ListTaskEvents returned error: %v", err)
			}
			refusals := 0
			for _, event := range events {
				if event.Kind == TaskEventMergedRegressionRefused {
					refusals++
				}
			}
			if refusals != 1 {
				t.Fatalf("%s events = %d, want 1: a write refused by the store must still leave the trace", TaskEventMergedRegressionRefused, refusals)
			}
		})
	}
}
