package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

const (
	TaskHoldSetManualEventKind      = "task_hold_set_manual"
	TaskHoldClearedManualEventKind  = "task_hold_cleared_manual"
	AutomaticFixDispatchedEventKind = "automatic_fix_dispatched"
	AdvancementSkippedHeldEventKind = "advancement_skipped_held"
	AdvancementStoppedRoundCapKind  = "advancement_stopped_round_cap"

	maxAutoFixRounds = 3
)

// taskAdvancementHeld derives the current coordinator hold from the append-only
// task event trail. This safety property depends on task_events, or at minimum
// task_hold_set_manual and task_hold_cleared_manual, remaining permanently
// unpruned: only under that retention contract does "no hold event" unambiguously
// mean "never held". Any future task-event retention mechanism must preserve or
// materialize these events before it can safely prune them.
func (e Engine) taskAdvancementHeld(ctx context.Context, taskID string) (bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false, nil
	}
	if e.TaskHoldStatus != nil {
		return e.TaskHoldStatus(ctx, taskID)
	}
	event, found, err := e.Store.LatestTaskEventByKinds(ctx, taskID,
		TaskHoldSetManualEventKind, TaskHoldClearedManualEventKind)
	if err != nil {
		return false, err
	}
	return found && event.Kind == TaskHoldSetManualEventKind, nil
}

func (e Engine) recordAutomaticFixHeldSkip(ctx context.Context, ref taskRef, reason string) error {
	if strings.TrimSpace(ref.ID) == "" {
		return nil
	}
	return e.Store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID: ref.ID,
		Kind:   AdvancementSkippedHeldEventKind,
		Reason: strings.TrimSpace(reason),
	})
}

func (e Engine) automaticFixRoundCapEvent(ctx context.Context, taskID string) (db.TaskEvent, bool, error) {
	return e.Store.LatestTaskEventByKinds(ctx, taskID, AdvancementStoppedRoundCapKind)
}

// automaticFixDispatchCount derives the cap counter from the append-only task
// event trail. This safety property depends on task_events, or at minimum
// automatic_fix_dispatched, remaining permanently unpruned: pruning those
// events would lower the observed count and silently reopen automatic dispatch.
// Any future retention mechanism must preserve or materialize this count first.
func (e Engine) automaticFixDispatchCount(ctx context.Context, taskID string) (int, error) {
	return e.Store.CountTaskEventsByKind(ctx, taskID, AutomaticFixDispatchedEventKind)
}

func automaticFixRoundCapReason(dispatchCount int) string {
	return fmt.Sprintf("automatic review-fix advancement stopped after %d automatic fix dispatches: maximum automatic fix rounds is %d; coordinator action is required", dispatchCount, maxAutoFixRounds)
}

func (e Engine) parkTaskForAutomaticFixCap(ctx context.Context, ref taskRef, reason string) error {
	if strings.TrimSpace(ref.ID) == "" {
		return BlockedError{Reason: reason}
	}
	task, err := e.Store.GetTask(ctx, ref.ID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := e.setTaskState(ctx, ref, TaskChangesRequested); err != nil {
			return err
		}
		task, err = e.Store.GetTask(ctx, ref.ID)
	}
	if err != nil {
		return err
	}
	if task.State == string(TaskDismissed) {
		return fmt.Errorf("task %s is dismissed; workflow advancement cannot move it to %s", task.ID, TaskBlocked)
	}
	if task.State == string(TaskBlocked) {
		return BlockedError{Reason: reason}
	}
	changed, current, err := e.Store.TransitionTaskStateWithEvent(ctx, ref.ID,
		[]string{task.State}, string(TaskBlocked),
		AdvancementStoppedRoundCapKind, reason)
	if err != nil {
		return err
	}
	if !changed && current != string(TaskBlocked) {
		return fmt.Errorf("task %s changed to %s while applying automatic fix round cap", ref.ID, current)
	}
	return BlockedError{Reason: reason}
}

func (e Engine) canonicalAdvancementTaskRef(ctx context.Context, ref taskRef) (taskRef, error) {
	if strings.TrimSpace(ref.Repo) == "" || strings.TrimSpace(ref.Branch) == "" {
		return ref, nil
	}
	task, err := e.Store.GetTaskByRepoBranch(ctx, ref.Repo, ref.Branch)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, nil
	}
	if err != nil {
		return taskRef{}, err
	}
	ref.ID = task.ID
	if ref.GoalID == "" {
		ref.GoalID = task.GoalID
	}
	if ref.Title == "" {
		ref.Title = task.Title
	}
	return ref, nil
}
