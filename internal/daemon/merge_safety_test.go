package daemon

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRearmAutoMergeDisabledTasksOnlyOnceAndOnlyForPolicyReason(t *testing.T) {
	ctx := context.Background()
	repo := github.Repository{Owner: "owner", Name: "repo"}
	store := testStore(t)
	for _, task := range []db.Task{
		{ID: "policy-parked", RepoFullName: repo.FullName(), State: string(workflow.TaskAwaitingHumanMerge), Branch: "policy"},
		{ID: "other-parked", RepoFullName: repo.FullName(), State: string(workflow.TaskAwaitingHumanMerge), Branch: "other"},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatalf("UpsertTask(%s): %v", task.ID, err)
		}
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{TaskID: "policy-parked", Kind: "task_awaiting_human_merge", ToState: string(workflow.TaskAwaitingHumanMerge), Reason: workflow.MergeLeaveOpenAutoMergeDisabledReason}); err != nil {
		t.Fatalf("AddTaskEvent policy: %v", err)
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{TaskID: "other-parked", Kind: "task_awaiting_human_merge", ToState: string(workflow.TaskAwaitingHumanMerge), Reason: "branch protection requires human merge"}); err != nil {
		t.Fatalf("AddTaskEvent other: %v", err)
	}

	enabled := false
	d := Daemon{Repo: repo, Store: store, AutoMergeEnabled: func(string) bool { return enabled }}
	if err := d.rearmAutoMergeDisabledTasks(ctx); err != nil {
		t.Fatalf("disabled rearm: %v", err)
	}
	if task, err := store.GetTask(ctx, "policy-parked"); err != nil || task.State != string(workflow.TaskAwaitingHumanMerge) {
		t.Fatalf("disabled policy task = %+v, err=%v; want parked", task, err)
	}
	enabled = true
	for pass := 0; pass < 2; pass++ {
		if err := d.rearmAutoMergeDisabledTasks(ctx); err != nil {
			t.Fatalf("rearm pass %d: %v", pass+1, err)
		}
	}

	policyTask, err := store.GetTask(ctx, "policy-parked")
	if err != nil || policyTask.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("policy task = %+v, err=%v; want ready_to_merge", policyTask, err)
	}
	otherTask, err := store.GetTask(ctx, "other-parked")
	if err != nil || otherTask.State != string(workflow.TaskAwaitingHumanMerge) {
		t.Fatalf("other task = %+v, err=%v; want awaiting_human_merge", otherTask, err)
	}
	events, err := store.ListTaskEvents(ctx, "policy-parked")
	if err != nil || len(events) != 2 || events[1].Kind != "task_awaiting_human_merge_rearmed" {
		t.Fatalf("policy events = %+v, err=%v; want exactly one re-arm", events, err)
	}
}
