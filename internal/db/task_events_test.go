package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskEventsMigrationAppliesToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	legacy, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	taskEventsVersion := 0
	for i, migration := range migrations {
		if strings.Contains(migration, "CREATE TABLE IF NOT EXISTS task_events") {
			taskEventsVersion = i + 1
			break
		}
	}
	if taskEventsVersion == 0 {
		t.Fatal("task_events migration not found")
	}
	if _, err := legacy.db.Exec(`DROP TABLE task_events; DELETE FROM schema_migrations WHERE version = ?`, taskEventsVersion); err != nil {
		t.Fatalf("rewind task_events migration: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("Open existing db: %v", err)
	}
	defer store.Close()
	if ok, err := store.HasTable(context.Background(), "task_events"); err != nil || !ok {
		t.Fatalf("task_events table ok=%v err=%v", ok, err)
	}
}

func TestTransitionTaskStateWithEventAtomicCASAndOrdering(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, Task{ID: "task-1", State: "implementing"}); err != nil {
		t.Fatal(err)
	}
	changed, current, err := store.TransitionTaskStateWithEvent(ctx, "task-1", []string{"implementing", "blocked"}, "dismissed", "task_dismissed_manual", "done")
	if err != nil || !changed || current != "dismissed" {
		t.Fatalf("first transition = changed %v current %q err %v", changed, current, err)
	}
	changed, current, err = store.TransitionTaskStateWithEvent(ctx, "task-1", []string{"implementing", "blocked"}, "dismissed", "task_dismissed_manual", "again")
	if err != nil || changed || current != "dismissed" {
		t.Fatalf("idempotent transition = changed %v current %q err %v", changed, current, err)
	}
	changed, current, err = store.TransitionTaskStateWithEvent(ctx, "task-1", []string{"blocked"}, "planned", "should_not_exist", "bad cas")
	if err != nil || changed || current != "dismissed" {
		t.Fatalf("failed CAS = changed %v current %q err %v", changed, current, err)
	}
	if err := store.AddTaskEvent(ctx, TaskEvent{TaskID: "task-1", Kind: "note", Reason: "second"}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListTaskEvents(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "task_dismissed_manual" || events[0].FromState != "implementing" || events[0].ToState != "dismissed" || events[1].Kind != "note" || events[0].ID >= events[1].ID {
		t.Fatalf("events = %+v", events)
	}
}

func TestTransitionTaskStateWithEventObservedReturnsCASSourceState(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, Task{ID: "task-observed", State: "awaiting_human_merge"}); err != nil {
		t.Fatal(err)
	}
	changed, observed, current, err := store.TransitionTaskStateWithEventObserved(ctx, "task-observed", []string{"pr_open", "awaiting_human_merge"}, "blocked", "pr_closed_unmerged", "closed without merge")
	if err != nil || !changed || observed != "awaiting_human_merge" || current != "blocked" {
		t.Fatalf("transition = changed %v observed %q current %q err %v", changed, observed, current, err)
	}
	events, err := store.ListTaskEvents(ctx, "task-observed")
	if err != nil || len(events) != 1 || events[0].FromState != "awaiting_human_merge" || events[0].ToState != "blocked" {
		t.Fatalf("events = %+v, err=%v", events, err)
	}
}

func TestGuardedTaskTransitionRefusesMatchingActiveJob(t *testing.T) {
	tests := []struct {
		name    string
		state   string
		payload string
	}{
		{name: "task id queued", state: "queued", payload: `{"repo":"other/repo","branch":"other","task_id":"task-1"}`},
		{name: "repo branch running", state: "running", payload: `{"repo":"owner/repo","branch":"feature/one","task_id":"other"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openWorkflowTestStore(t)
			ctx := context.Background()
			if err := store.UpsertTask(ctx, Task{ID: "task-1", RepoFullName: "owner/repo", Branch: "feature/one", State: "implementing"}); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateJob(ctx, Job{ID: "job-1", Type: "review", State: test.state, Payload: test.payload}); err != nil {
				t.Fatal(err)
			}
			changed, current, err := store.TransitionTaskStateWithEventIfNoActiveJob(ctx, "task-1", []string{"implementing"}, "dismissed", "task_dismissed_manual", "test")
			if changed || current != "implementing" || !errors.Is(err, ErrTaskHasActiveJob) {
				t.Fatalf("transition = changed %v current %q err %v", changed, current, err)
			}
			task, getErr := store.GetTask(ctx, "task-1")
			events, listErr := store.ListTaskEvents(ctx, "task-1")
			if getErr != nil || listErr != nil || task.State != "implementing" || len(events) != 0 {
				t.Fatalf("task=%+v events=%+v getErr=%v listErr=%v", task, events, getErr, listErr)
			}
		})
	}
}

func TestTaskIDsWithTerminalWorktreeUsesLifecycleStateAndClassification(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	for _, task := range []Task{
		{ID: "merged", State: "merged", WorktreePath: "/worktrees/merged"},
		{ID: "dismissed", State: "dismissed", WorktreePath: "/worktrees/dismissed"},
		{ID: "superseded", State: "superseded", WorktreePath: "/worktrees/superseded"},
		{ID: "stranded", State: "stranded", WorktreePath: " /worktrees/stranded "},
		{ID: "blocked", State: "blocked", WorktreePath: "/worktrees/blocked"},
		{ID: "implementing", State: "implementing", WorktreePath: "/worktrees/implementing"},
		{ID: "empty-path", State: "merged"},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatalf("UpsertTask %s: %v", task.ID, err)
		}
	}
	classified, err := store.ClassifyTerminalTaskWorktreeUnremovable(ctx, "stranded", "/worktrees/stranded")
	if err != nil || !classified {
		t.Fatalf("ClassifyTerminalTaskWorktreeUnremovable classified=%v err=%v", classified, err)
	}
	events, err := store.ListTaskEvents(ctx, "stranded")
	if err != nil {
		t.Fatalf("ListTaskEvents after classification: %v", err)
	}
	if len(events) != 1 || events[0].FromState != "" || events[0].ToState != "" || events[0].Reason != "/worktrees/stranded" {
		t.Fatalf("classification event = %+v, want normalized informational event", events)
	}

	ids, err := store.TaskIDsWithTerminalWorktree(ctx)
	if err != nil {
		t.Fatalf("TaskIDsWithTerminalWorktree: %v", err)
	}
	want := []string{"dismissed", "merged", "superseded"}
	if len(ids) != len(want) {
		t.Fatalf("terminal worktree ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("terminal worktree ids = %v, want %v", ids, want)
		}
	}
	if err := store.AddTaskEvent(ctx, TaskEvent{TaskID: "stranded", Kind: "task_rechecked", Reason: "new lifecycle evidence"}); err != nil {
		t.Fatalf("AddTaskEvent after classification: %v", err)
	}
	ids, err = store.TaskIDsWithTerminalWorktree(ctx)
	if err != nil {
		t.Fatalf("TaskIDsWithTerminalWorktree after later event: %v", err)
	}
	want = []string{"dismissed", "merged", "stranded", "superseded"}
	if len(ids) != len(want) {
		t.Fatalf("terminal worktree ids after later event = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("terminal worktree ids after later event = %v, want %v", ids, want)
		}
	}
}

func TestTaskHasActiveWorktreeOwnerMatchesTaskOrPath(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	for _, job := range []Job{
		{ID: "by-task", Agent: "agent", Type: "implement", State: "queued", Payload: `{"task_id":"task-1"}`},
		{ID: "by-path", Agent: "agent", Type: "review", State: "running", Payload: `{"worktree_path":"/worktrees/task-2"}`},
		{ID: "blocked-owner", Agent: "agent", Type: "implement", State: "blocked", Payload: `{"task_id":"task-4","worktree_path":"/worktrees/task-4"}`},
		{ID: "finished", Agent: "agent", Type: "implement", State: "succeeded", Payload: `{"task_id":"task-3","worktree_path":"/worktrees/task-3"}`},
		{ID: "malformed-finished", Agent: "agent", Type: "implement", State: "failed", Payload: `not json at all`},
	} {
		if err := store.CreateJobWithEvent(ctx, job, JobEvent{Kind: job.State, Message: "seed"}); err != nil {
			t.Fatalf("CreateJobWithEvent %s: %v", job.ID, err)
		}
	}
	for _, tc := range []struct {
		taskID string
		path   string
		want   bool
	}{
		{taskID: "task-1", path: "/other", want: true},
		{taskID: "other", path: "/worktrees/task-2", want: true},
		{taskID: "task-3", path: "/worktrees/task-3", want: false},
		{taskID: "task-4", path: "/worktrees/task-4", want: true},
		{taskID: "unowned", path: "/worktrees/unowned", want: false},
	} {
		got, err := store.TaskHasActiveWorktreeOwner(ctx, tc.taskID, tc.path)
		if err != nil {
			t.Fatalf("TaskHasActiveWorktreeOwner(%q, %q): %v", tc.taskID, tc.path, err)
		}
		if got != tc.want {
			t.Fatalf("TaskHasActiveWorktreeOwner(%q, %q) = %v, want %v", tc.taskID, tc.path, got, tc.want)
		}
	}
	malformedJobID, err := store.FirstMalformedNonFinalJob(ctx)
	if err != nil {
		t.Fatalf("FirstMalformedNonFinalJob without active malformed payload: %v", err)
	}
	if malformedJobID != "" {
		t.Fatalf("FirstMalformedNonFinalJob = %q before active malformed payload, want empty", malformedJobID)
	}
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "malformed-active", Agent: "agent", Type: "implement", State: "queued", Payload: `not json at all`,
	}, JobEvent{Kind: "queued", Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent malformed-active: %v", err)
	}
	got, err := store.TaskHasActiveWorktreeOwner(ctx, "unowned", "/worktrees/unowned")
	if err != nil {
		t.Fatalf("TaskHasActiveWorktreeOwner with malformed active payload: %v", err)
	}
	if !got {
		t.Fatal("malformed non-final payload did not fail closed as an active owner")
	}
	malformedJobID, err = store.FirstMalformedNonFinalJob(ctx)
	if err != nil {
		t.Fatalf("FirstMalformedNonFinalJob: %v", err)
	}
	if malformedJobID != "malformed-active" {
		t.Fatalf("FirstMalformedNonFinalJob = %q, want malformed-active", malformedJobID)
	}
}

func TestCompleteTerminalTaskWorktreeReclaimClearsRemovedPathAfterStateTransition(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const taskID = "task-reclaim-race"
	const removedPath = "/worktrees/task-reclaim-race"
	if err := store.UpsertTask(ctx, Task{
		ID: taskID, State: "reviewing", WorktreePath: removedPath,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	changed, err := store.CompleteTerminalTaskWorktreeReclaim(ctx, taskID, removedPath)
	if err != nil {
		t.Fatalf("CompleteTerminalTaskWorktreeReclaim: %v", err)
	}
	if !changed {
		t.Fatal("removed path was not cleared after lifecycle transition")
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask after completion: %v", err)
	}
	if task.WorktreePath != "" || task.State != "reviewing" {
		t.Fatalf("task after completion = %+v, want reviewing with empty path", task)
	}

	const replacementPath = "/worktrees/task-reclaim-race-new"
	task.WorktreePath = replacementPath
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatalf("UpsertTask replacement: %v", err)
	}
	changed, err = store.CompleteTerminalTaskWorktreeReclaim(ctx, taskID, removedPath)
	if err != nil {
		t.Fatalf("CompleteTerminalTaskWorktreeReclaim replacement: %v", err)
	}
	if changed {
		t.Fatal("completion cleared a concurrent path replacement")
	}
	task, err = store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask after replacement: %v", err)
	}
	if task.WorktreePath != replacementPath {
		t.Fatalf("replacement path = %q, want %q", task.WorktreePath, replacementPath)
	}
}
