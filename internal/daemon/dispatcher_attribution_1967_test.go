package daemon

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1967 READER 2 of 2 - the daemon PR-watcher fan-out.
//
// This is the path where the gap was total: the fan-out review's only recorded
// identity was Sender: "github", a channel, so a finding it produced could not
// be resolved to any seat at all. The dispatcher is the branch lock owner, read
// from the SAME lock row the watcher already fetches for ActingOrgRole, so the
// two readers cannot drift.
//
// The assertion deliberately rejects three specific wrong answers rather than
// only checking non-emptiness: "github" (the channel, which was the pre-fix
// value), and the reviewer agent name (the identity #1890 misroutes on).
func TestDaemonPRWatcherFanoutRecordsTheBranchLockOwnerAsDispatcher(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-1967",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1967",
		Title:        "Attribute the dispatcher",
		State:        string(workflow.TaskPullRequestOpen),
		Branch:       "task-1967",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	for _, agent := range []db.Agent{
		{Name: "builder", Role: "builder", Capabilities: []string{"implement"}},
		{Name: "reviewer", Role: "reviewer", Capabilities: []string{"review"}},
	} {
		agent.Runtime = "codex"
		agent.RuntimeRef = "last"
		agent.RepoScope = repo.FullName()
		agent.AutonomyPolicy = "workspace-write"
		agent.HealthStatus = "ok"
		if err := store.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent(%s) returned error: %v", agent.Name, err)
		}
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: repo.FullName(),
		Branch:       "task-1967",
		Owner:        "builder",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}

	engine := workflow.Engine{Store: store, RequiredReviewers: []string{"reviewer"}}
	daemon := Daemon{Repo: repo, Store: store, Workflow: &engine}
	pull := github.PullRequest{
		Number:  1967,
		Title:   "Attribute the dispatcher",
		State:   "open",
		HeadRef: "task-1967",
		BaseRef: "main",
		HeadSHA: "head1967",
	}

	if _, err := daemon.handlePullRequestWorkflowChange(ctx, pull, newReviewJobsMemo(store)); err != nil {
		t.Fatalf("handlePullRequestWorkflowChange returned error: %v", err)
	}

	jobs, err := store.ListJobsByType(ctx, "review")
	if err != nil {
		t.Fatalf("ListJobsByType returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("daemon fanout enqueued %d review jobs, want 1; cannot assert attribution", len(jobs))
	}
	switch jobs[0].DispatchedBy {
	case "builder":
	case "":
		t.Fatal("daemon fanout review has no dispatcher; a finding it records can only be routed by the reviewer's own name (#1890)")
	case "github":
		t.Fatal(`daemon fanout review dispatched_by = "github": that is the dispatch channel, not the seat whose work caused the review`)
	case "reviewer":
		t.Fatal(`daemon fanout review dispatched_by = "reviewer": attributing a finding to its own author is the defect #1967 exists to fix`)
	default:
		t.Fatalf("daemon fanout review dispatched_by = %q, want %q", jobs[0].DispatchedBy, "builder")
	}
}
