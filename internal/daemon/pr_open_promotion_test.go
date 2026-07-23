package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestReconcilePROpenTasks proves the #920 catch-up: implementing/blocked tasks
// whose branch carries an open same-repo PR are promoted to pr_open with an
// audit event, while fork heads, other states, and unrelated branches stay put.
func TestReconcilePROpenTasks(t *testing.T) {
	tests := []struct {
		name       string
		state      workflow.TaskState
		branch     string
		pull       github.PullRequest
		wantState  workflow.TaskState
		wantEvent  bool
		wantReason string
	}{
		{
			name: "implementing promoted", state: workflow.TaskImplementing, branch: "feat/x",
			pull:      github.PullRequest{Number: 470, HeadRef: "feat/x"},
			wantState: workflow.TaskPullRequestOpen, wantEvent: true, wantReason: "open PR #470 found for branch feat/x",
		},
		{
			name: "blocked promoted", state: workflow.TaskBlocked, branch: "feat/x",
			pull:      github.PullRequest{Number: 470, HeadRef: "feat/x", HeadRepoFullName: "owner/repo"},
			wantState: workflow.TaskPullRequestOpen, wantEvent: true, wantReason: "open PR #470",
		},
		{
			name: "fork head ignored", state: workflow.TaskBlocked, branch: "feat/x",
			pull:      github.PullRequest{Number: 470, HeadRef: "feat/x", HeadRepoFullName: "fork/repo"},
			wantState: workflow.TaskBlocked,
		},
		{
			name: "unrelated branch ignored", state: workflow.TaskBlocked, branch: "feat/other",
			pull:      github.PullRequest{Number: 470, HeadRef: "feat/x"},
			wantState: workflow.TaskBlocked,
		},
		{
			name: "planned untouched", state: workflow.TaskPlanned, branch: "feat/x",
			pull:      github.PullRequest{Number: 470, HeadRef: "feat/x"},
			wantState: workflow.TaskPlanned,
		},
		{
			name: "pr_open idempotent", state: workflow.TaskPullRequestOpen, branch: "feat/x",
			pull:      github.PullRequest{Number: 470, HeadRef: "feat/x"},
			wantState: workflow.TaskPullRequestOpen,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			repo := github.Repository{Owner: "owner", Name: "repo"}
			seedStaleRepo(t, store, repo)
			task := db.Task{ID: "task-1", RepoFullName: repo.FullName(), State: string(test.state), Branch: test.branch}
			if err := store.UpsertTask(ctx, task); err != nil {
				t.Fatal(err)
			}

			d := Daemon{Repo: repo, Store: store}
			if err := d.reconcilePROpenTasks(ctx, []github.PullRequest{test.pull}); err != nil {
				t.Fatalf("reconcilePROpenTasks: %v", err)
			}

			got, err := store.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != string(test.wantState) {
				t.Fatalf("state = %s, want %s", got.State, test.wantState)
			}
			events, err := store.ListTaskEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !test.wantEvent {
				if len(events) != 0 {
					t.Fatalf("events = %+v, want none", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1: %+v", len(events), events)
			}
			event := events[0]
			if event.Kind != "task_pr_open_auto" || event.FromState != string(test.state) || event.ToState != string(workflow.TaskPullRequestOpen) {
				t.Fatalf("event = %+v", event)
			}
			if !strings.Contains(event.Reason, test.wantReason) {
				t.Fatalf("event reason = %q, want contains %q", event.Reason, test.wantReason)
			}
		})
	}
}

// TestReconcilePROpenTasksNoPulls proves the zero-PR fast path never touches the store.
func TestReconcilePROpenTasksNoPulls(t *testing.T) {
	d := Daemon{Repo: github.Repository{Owner: "owner", Name: "repo"}}
	if err := d.reconcilePROpenTasks(context.Background(), nil); err != nil {
		t.Fatalf("reconcilePROpenTasks(nil pulls): %v", err)
	}
}

func TestReconcilePROpenTasksJournalsWorkflowOnce(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "owner", Name: "repo"}
	seedStaleRepo(t, store, repo)
	const (
		workflowID = "release/lifecycle"
		branch     = "feat/lifecycle"
	)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-lifecycle", RepoFullName: repo.FullName(), State: string(workflow.TaskImplementing), Branch: branch}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "job-lifecycle", Agent: "worker", Type: "implement", State: "succeeded",
		Payload: `{"workflow_id":"release/lifecycle","repo":"owner/repo","branch":"feat/lifecycle","pull_request":958}`}); err != nil {
		t.Fatal(err)
	}
	d := Daemon{Repo: repo, Store: store}
	pull := github.PullRequest{Number: 958, HeadRef: branch}
	for i := 0; i < 2; i++ {
		if err := d.reconcilePROpenTasks(ctx, []github.PullRequest{pull}); err != nil {
			t.Fatalf("reconcilePROpenTasks pass %d: %v", i+1, err)
		}
	}
	notes, err := store.ListWorkflowNotes(ctx, workflowID, 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes = %+v, err=%v; want one deduped breadcrumb", notes, err)
	}
	if notes[0].Author != db.WorkflowAutoNoteAuthor || notes[0].Body != "[auto:pr:958:opened] PR #958 opened (feat/lifecycle)" {
		t.Fatalf("auto note = %+v", notes[0])
	}
	meta, err := store.GetWorkflowMeta(ctx, workflowID)
	if err != nil || meta.Status != "active" || meta.Description != "lifecycle" {
		t.Fatalf("meta = %+v, err=%v", meta, err)
	}
}
