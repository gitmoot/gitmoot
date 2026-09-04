package cli

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1834, the dispatch half. The branchless path wrote reviewing through
// UpsertTaskUnlessStates, whose exclusion list named only the DISPOSED states, so
// dispatching a review silently cleared a live changes_requested. The --branch
// path above it returns before any state write, so the two disagreed - which is
// why repeated manual review rounds never reset the wedged tasks and, worse, why
// a dispatch could erase an objection no approval had answered.
//
// Leaving changes_requested is now the merge gate's head-bound admission alone.
func TestReviewDispatchKeepsChangesRequested(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	if err := store.UpsertTask(ctx, db.Task{
		ID:           "review-pr-9-abcd1234",
		RepoFullName: "owner/repo",
		GoalID:       "local-review",
		Title:        "Review PR #9",
		State:        string(workflow.TaskChangesRequested),
	}); err != nil {
		t.Fatal(err)
	}

	request, err := prepareLocalReviewTask(ctx, store,
		github.Repository{Owner: "owner", Name: "repo"},
		localAgentDispatchRequest{
			Home: home, PullRequest: 9, HeadSHA: "head-new", TaskID: "review-pr-9-abcd1234",
		})
	if err != nil {
		t.Fatalf("dispatching a review against an objecting task must be allowed: %v", err)
	}
	if request.TaskID != "review-pr-9-abcd1234" {
		t.Fatalf("request bound task %q, want the existing task", request.TaskID)
	}
	// The dispatch must still carry the identity fields the review job needs.
	if request.GoalID == "" || request.TaskTitle == "" {
		t.Fatalf("dispatch dropped task identity: goal=%q title=%q", request.GoalID, request.TaskTitle)
	}
	task, err := store.GetTask(ctx, "review-pr-9-abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != string(workflow.TaskChangesRequested) {
		t.Fatalf("task state = %q, want changes_requested: dispatching a review must not clear an objection no approval has answered (#1834)", task.State)
	}
}

// The disposed states keep refusing. They share the exclusion list with
// changes_requested but mean the opposite thing, and collapsing the two would
// turn a refusal into a silent accept.
func TestReviewDispatchStillRefusesDisposedTask(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	for _, state := range []workflow.TaskState{workflow.TaskDismissed, workflow.TaskSuperseded, workflow.TaskStranded} {
		taskID := "review-pr-9-" + string(state)
		if err := store.UpsertTask(ctx, db.Task{
			ID: taskID, RepoFullName: "owner/repo", GoalID: "local-review",
			Title: "Review PR #9", State: string(state),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareLocalReviewTask(ctx, store,
			github.Repository{Owner: "owner", Name: "repo"},
			localAgentDispatchRequest{Home: home, PullRequest: 9, HeadSHA: "head-new", TaskID: taskID},
		); err == nil {
			t.Fatalf("dispatch against a %s task must be refused", state)
		}
	}
}

// A task that does not exist yet is still created in reviewing: the new
// exclusion must not turn first dispatch into a no-op.
func TestReviewDispatchCreatesReviewingTask(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	request, err := prepareLocalReviewTask(ctx, store,
		github.Repository{Owner: "owner", Name: "repo"},
		localAgentDispatchRequest{Home: home, PullRequest: 9, HeadSHA: "head-new"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(ctx, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != string(workflow.TaskReviewing) {
		t.Fatalf("new review task state = %q, want reviewing", task.State)
	}
}
