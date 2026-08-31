package db

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRevalidateTaskStateDoesNotRefreshTaskAge(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, Task{ID: "task-ready", State: "ready_to_merge"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET updated_at = '2000-01-01 00:00:00' WHERE id = ?`, "task-ready"); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM tasks WHERE id = ?`, "task-ready").Scan(&before); err != nil {
		t.Fatal(err)
	}
	matched, current, err := store.RevalidateTaskState(ctx, "task-ready", "ready_to_merge")
	if err != nil || !matched || current != "ready_to_merge" {
		t.Fatalf("matching revalidation = matched %v current %q err %v", matched, current, err)
	}
	var after string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM tasks WHERE id = ?`, "task-ready").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("updated_at changed from %q to %q during revalidation", before, after)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET state = 'blocked' WHERE id = ?`, "task-ready"); err != nil {
		t.Fatal(err)
	}
	matched, current, err = store.RevalidateTaskState(ctx, "task-ready", "ready_to_merge")
	if err != nil || matched || current != "blocked" {
		t.Fatalf("conflicting revalidation = matched %v current %q err %v", matched, current, err)
	}
}

func TestBlockTaskWithEventRollsBackStateWhenOwnershipInsertFails(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, Task{ID: "task-block", State: "implementing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER reject_task_block_event
BEFORE INSERT ON task_events
WHEN NEW.task_id = 'task-block'
BEGIN
	SELECT RAISE(ABORT, 'forced task block attribution failure');
END`); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.BlockTaskWithEvent(ctx,
		Task{ID: "task-block", State: "blocked"},
		TaskEvent{Kind: "workflow_blocked", FromState: "implementing", Reason: "failed"},
	)
	if blocked || err == nil || !strings.Contains(err.Error(), "forced task block attribution failure") {
		t.Fatalf("BlockTaskWithEvent = blocked %v err %v", blocked, err)
	}
	task, getErr := store.GetTask(ctx, "task-block")
	if getErr != nil || task.State != "implementing" {
		t.Fatalf("task = %+v err %v, want original implementing state", task, getErr)
	}
	events, listErr := store.ListTaskEvents(ctx, "task-block")
	if listErr != nil || len(events) != 0 {
		t.Fatalf("events = %+v err %v, want atomic rollback", events, listErr)
	}
}

func TestBlockTaskWithEventPreservesPriorOwnerWhenReplacementInsertFails(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, Task{ID: "task-block", State: "blocked"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddTaskEvent(ctx, TaskEvent{
		TaskID: "task-block", Kind: "merge_gate_blocked", FromState: "ready_to_merge",
		ToState: "blocked", Reason: "dirty worktree",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER reject_replacement_block_owner
BEFORE INSERT ON task_events
WHEN NEW.kind = 'workflow_blocked'
BEGIN
	SELECT RAISE(ABORT, 'forced replacement ownership failure');
END`); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.BlockTaskWithEvent(ctx,
		Task{ID: "task-block", State: "blocked"},
		TaskEvent{Kind: "workflow_blocked", FromState: "blocked", Reason: "new workflow failure"},
	)
	if blocked || err == nil || !strings.Contains(err.Error(), "forced replacement ownership failure") {
		t.Fatalf("BlockTaskWithEvent = blocked %v err %v", blocked, err)
	}
	events, listErr := store.ListTaskEvents(ctx, "task-block")
	if listErr != nil || len(events) != 1 || events[0].Kind != "merge_gate_blocked" {
		t.Fatalf("events = %+v err %v, want only prior owner", events, listErr)
	}
}

func TestBlockTaskWithEventRejectsStaleAndTerminalStateObservations(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, Task{ID: "task-block", State: "ready_to_merge"}); err != nil {
		t.Fatal(err)
	}
	if changed, _, err := store.CompareAndSwapTaskState(ctx, "task-block", "ready_to_merge", "merged"); err != nil || !changed {
		t.Fatalf("terminal transition = changed %v err %v", changed, err)
	}
	blocked, err := store.BlockTaskWithEvent(ctx,
		Task{ID: "task-block", State: "blocked"},
		TaskEvent{Kind: "workflow_blocked", FromState: "ready_to_merge", Reason: "late failure"},
	)
	if blocked || !errors.Is(err, ErrTaskStateConflict) {
		t.Fatalf("BlockTaskWithEvent = blocked %v err %v, want guarded conflict", blocked, err)
	}
	task, getErr := store.GetTask(ctx, "task-block")
	events, listErr := store.ListTaskEvents(ctx, "task-block")
	if getErr != nil || listErr != nil || task.State != "merged" || len(events) != 0 {
		t.Fatalf("task=%+v events=%+v getErr=%v listErr=%v", task, events, getErr, listErr)
	}
}

func TestTaskStateClaimFencesIndependentStoreStateWriters(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, Task{ID: "task-claim", State: "ready_to_merge"}); err != nil {
		t.Fatal(err)
	}
	writer, err := OpenAlreadyMigrated(store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	token, claimed, current, err := store.ClaimTaskState(ctx, "task-claim", "ready_to_merge", "external_merge", time.Minute)
	if err != nil || !claimed || current != "ready_to_merge" {
		t.Fatalf("claim = token %q claimed %v current %q err %v", token, claimed, current, err)
	}
	assertRejected := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s unexpectedly changed a task with an active durable claim", name)
		}
		task, getErr := writer.GetTask(ctx, "task-claim")
		if getErr != nil || task.State != "ready_to_merge" {
			t.Fatalf("%s left task %+v, err %v", name, task, getErr)
		}
	}

	_, err = writer.BlockTaskWithEvent(ctx,
		Task{ID: "task-claim", State: "blocked"},
		TaskEvent{Kind: "workflow_blocked", FromState: "ready_to_merge", Reason: "late failure"})
	assertRejected("BlockTaskWithEvent", err)
	_, _, err = writer.TransitionTaskStateWithEvent(ctx, "task-claim",
		[]string{"ready_to_merge"}, "blocked", "workflow_blocked", "late transition")
	assertRejected("TransitionTaskStateWithEvent", err)
	_, _, err = writer.CompareAndSwapTaskState(ctx, "task-claim", "ready_to_merge", "blocked")
	assertRejected("CompareAndSwapTaskState", err)
	_, _, err = writer.DisposeTask(ctx, "task-claim", []string{"ready_to_merge"}, "dismissed",
		"stale", "late disposal", "", "task_disposed", time.Now())
	assertRejected("DisposeTask", err)
	assertRejected("UpsertTask", writer.UpsertTask(ctx, Task{ID: "task-claim", State: "blocked"}))

	changed, current, err := store.CompleteTaskStateClaim(ctx, "task-claim", token,
		"merged", "pull_request_merged", "external merge completed")
	if err != nil || !changed || current != "merged" {
		t.Fatalf("complete claim = changed %v current %q err %v", changed, current, err)
	}
	events, err := writer.ListTaskEvents(ctx, "task-claim")
	if err != nil || len(events) != 1 || events[0].Kind != "pull_request_merged" {
		t.Fatalf("events = %+v, err %v", events, err)
	}
}

func TestTaskStateClaimCollisionReleasesLosingTransaction(t *testing.T) {
	ctx := context.Background()
	winner := openWorkflowTestStore(t)
	if err := winner.UpsertTask(ctx, Task{ID: "task-collision", State: "ready_to_merge"}); err != nil {
		t.Fatal(err)
	}
	loser, err := OpenAlreadyMigrated(winner.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer loser.Close()

	token, claimed, _, err := winner.ClaimTaskState(ctx, "task-collision", "ready_to_merge",
		TaskStateClaimKindExternalMerge, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("winning claim = claimed %v err %v", claimed, err)
	}
	collisionCtx, cancelCollision := context.WithTimeout(ctx, 2*time.Second)
	defer cancelCollision()
	if _, claimed, _, err := loser.ClaimTaskState(collisionCtx, "task-collision", "ready_to_merge",
		TaskStateClaimKindExternalMerge, time.Minute); !errors.Is(err, ErrTaskStateClaimed) || claimed {
		t.Fatalf("losing claim = claimed %v err %v, want prompt ErrTaskStateClaimed", claimed, err)
	}

	usableCtx, cancelUsable := context.WithTimeout(ctx, 2*time.Second)
	defer cancelUsable()
	if task, err := loser.GetTask(usableCtx, "task-collision"); err != nil || task.State != "ready_to_merge" {
		t.Fatalf("losing Store after collision = task %+v err %v, want usable connection", task, err)
	}
	if renewed, err := winner.RenewTaskStateClaim(usableCtx, "task-collision", token, time.Minute); err != nil || !renewed {
		t.Fatalf("winning claim renewal after collision = renewed %v err %v", renewed, err)
	}
	if changed, current, err := winner.CompleteTaskStateClaim(usableCtx, "task-collision", token,
		"merged", "pull_request_merged", "external merge completed after collision"); err != nil || !changed || current != "merged" {
		t.Fatalf("winning claim completion = changed %v current %q err %v", changed, current, err)
	}
	if task, err := loser.GetTask(usableCtx, "task-collision"); err != nil || task.State != "merged" {
		t.Fatalf("losing Store final read = task %+v err %v, want merged", task, err)
	}
}

func TestTaskStateClaimFencesCrossProcessWriter(t *testing.T) {
	const helperPathEnv = "GITMOOT_TASK_STATE_CLAIM_HELPER_PATH"
	ctx := context.Background()
	if path := os.Getenv(helperPathEnv); path != "" {
		writer, err := OpenAlreadyMigrated(path)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		changed, current, err := writer.CompareAndSwapTaskState(ctx, "task-cross-process", "ready_to_merge", "blocked")
		if err == nil || changed || current != "" {
			t.Fatalf("cross-process writer = changed %v current %q err %v, want schema-trigger rejection", changed, current, err)
		}
		return
	}

	store := openWorkflowTestStore(t)
	if err := store.UpsertTask(ctx, Task{ID: "task-cross-process", State: "ready_to_merge"}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, _, err := store.ClaimTaskState(ctx, "task-cross-process", "ready_to_merge", "external_merge", time.Minute); err != nil || !claimed {
		t.Fatalf("ClaimTaskState = claimed %v err %v", claimed, err)
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTaskStateClaimFencesCrossProcessWriter$")
	cmd.Env = append(os.Environ(), helperPathEnv+"="+store.DatabasePath())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-process writer helper: %v\n%s", err, output)
	}
	task, err := store.GetTask(ctx, "task-cross-process")
	if err != nil || task.State != "ready_to_merge" {
		t.Fatalf("task = %+v err %v, want claimed ready_to_merge state", task, err)
	}
}

func TestRetainedAmbiguousTaskClaimDoesNotExpireBeforeAuthoritativeResolution(t *testing.T) {
	ctx := context.Background()
	store := openWorkflowTestStore(t)
	if err := store.UpsertTask(ctx, Task{ID: "task-uncertain", State: "ready_to_merge"}); err != nil {
		t.Fatal(err)
	}
	writer, err := OpenAlreadyMigrated(store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	token, claimed, _, err := store.ClaimTaskState(ctx, "task-uncertain", "ready_to_merge",
		TaskStateClaimKindExternalMerge, time.Second)
	if err != nil || !claimed {
		t.Fatalf("ClaimTaskState = claimed %v err %v", claimed, err)
	}
	retained, err := store.RetainTaskStateClaim(ctx, "task-uncertain", token,
		"ready_to_merge", TaskStateClaimKindExternalMergeUncertain)
	if err != nil || !retained {
		t.Fatalf("RetainTaskStateClaim = retained %v err %v", retained, err)
	}
	time.Sleep(1100 * time.Millisecond)
	changed, _, err := writer.CompareAndSwapTaskState(ctx, "task-uncertain", "ready_to_merge", "dismissed")
	if err == nil || changed || !strings.Contains(err.Error(), "claimed for an external merge") {
		t.Fatalf("expired retained claim writer = changed %v err %v, want durable rejection", changed, err)
	}
	released, current, err := store.ReleaseRetainedTaskStateClaim(ctx, "task-uncertain",
		"ready_to_merge", TaskStateClaimKindExternalMergeUncertain)
	if err != nil || !released || current != "ready_to_merge" {
		t.Fatalf("ReleaseRetainedTaskStateClaim = released %v current %q err %v", released, current, err)
	}
	changed, current, err = writer.CompareAndSwapTaskState(ctx, "task-uncertain", "ready_to_merge", "dismissed")
	if err != nil || !changed || current != "dismissed" {
		t.Fatalf("resolved claim writer = changed %v current %q err %v", changed, current, err)
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
