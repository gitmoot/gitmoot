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

// TestSetTaskStateRefusesMergedToBlockedAndLeavesADurableTrace pins the #1673
// state-machine rule. A queued delegation child of an already-merged pull request
// dies in the daemon's closed-PR sweep; the child's advance ends in the parent's
// default block_parent policy, which calls setTaskState(TaskBlocked). Rewriting a
// `merged` task to `blocked` would undo the one record the sweep's own premise
// asserts — that the change shipped — so the move is refused.
//
// It differs from the disposed-state refusals above in the way that matters: it
// returns NO error. The advance that asked for the block still has to finish
// releasing the coordinator, so the refusal cannot be reported by failing it; the
// durable task event IS the report. Both identities setTaskState can resolve are
// covered: the payload task id, and the canonical task owning its branch.
func TestSetTaskStateRefusesMergedToBlockedAndLeavesADurableTrace(t *testing.T) {
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
		if err := engine.setTaskState(ctx, ref, TaskBlocked); err != nil {
			t.Fatalf("setTaskState(%+v, blocked) returned error %v; refusing the block must not fail the advance that asked for it", ref, err)
		}
		task, err := store.GetTask(ctx, "canonical")
		if err != nil {
			t.Fatalf("GetTask(canonical) returned error: %v", err)
		}
		if task.State != string(TaskMerged) {
			t.Fatalf("task state = %q after setTaskState(%+v, blocked), want merged (the landed-work record must survive)", task.State, ref)
		}
	}
	if _, err := store.GetTask(ctx, "late-review"); err == nil {
		t.Fatal("GetTask(late-review) succeeded; the refusal must not mint a second task on the merged branch")
	}

	// The refusal is silent to the caller, so the trace is the only way anybody
	// learns a block was dropped. One per refused call, on the canonical task.
	events, err := store.ListTaskEvents(ctx, "canonical")
	if err != nil {
		t.Fatalf("ListTaskEvents returned error: %v", err)
	}
	refusals := 0
	for _, event := range events {
		if event.Kind != TaskEventMergedBlockRefused {
			continue
		}
		refusals++
		if !strings.Contains(event.Reason, string(TaskMerged)) || !strings.Contains(event.Reason, string(TaskBlocked)) {
			t.Fatalf("%s reason = %q, want both states named", TaskEventMergedBlockRefused, event.Reason)
		}
	}
	if refusals != 2 {
		t.Fatalf("%s events = %d, want 2 (one per refused identity)", TaskEventMergedBlockRefused, refusals)
	}

	// `merged` is NOT frozen: the refusal is scoped to the blocked direction, so
	// `gitmoot task resume-work`'s merged -> implementing move still applies.
	if err := engine.setTaskState(ctx, taskRef{ID: "canonical", Repo: repo, Branch: branch}, TaskImplementing); err != nil {
		t.Fatalf("setTaskState(canonical, implementing) returned error: %v", err)
	}
	if task, _ := store.GetTask(ctx, "canonical"); task.State != string(TaskImplementing) {
		t.Fatalf("task state = %q, want implementing; the guard must not freeze merged outright", task.State)
	}
}
