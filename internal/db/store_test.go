package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenMigratesSchema(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	for _, table := range []string{
		"repos",
		"agents",
		"goals",
		"tasks",
		"pull_requests",
		"seen_comments",
		"jobs",
		"job_events",
		"branch_locks",
		"merge_gates",
	} {
		ok, err := store.HasTable(ctx, table)
		if err != nil {
			t.Fatalf("HasTable(%s) returned error: %v", table, err)
		}
		if !ok {
			t.Fatalf("expected table %s to exist", table)
		}
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate returned error: %v", err)
	}
}

func TestRepositoryMethods(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertRepo(ctx, Repo{Owner: "jerryfane", Name: "gitmoot", DefaultBranch: "main"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, Agent{Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "session", RepoScope: "jerryfane/gitmoot", Capabilities: []string{"review"}, AutonomyPolicy: "auto", HealthStatus: "ok"}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	agent, err := store.GetAgent(ctx, "audit")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if agent.Name != "audit" || agent.Capabilities[0] != "review" {
		t.Fatalf("agent = %+v", agent)
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents returned error: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "audit" {
		t.Fatalf("agents = %+v", agents)
	}
	if err := store.InsertGoal(ctx, Goal{ID: "goal-1", Title: "Build Gitmoot", Source: "GOAL.md", Status: "planned"}); err != nil {
		t.Fatalf("InsertGoal returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-1", GoalID: "goal-1", Title: "Bootstrap", State: "planned"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.InsertGoal(ctx, Goal{ID: "goal-2", Title: "Corrected Goal", Source: "GOAL.md", Status: "planned"}); err != nil {
		t.Fatalf("second InsertGoal returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-1", GoalID: "goal-2", Title: "Bootstrap", State: "planned"}); err != nil {
		t.Fatalf("second UpsertTask returned error: %v", err)
	}
	var goalID string
	if err := store.db.QueryRowContext(ctx, `SELECT goal_id FROM tasks WHERE id = ?`, "task-1").Scan(&goalID); err != nil {
		t.Fatalf("query task goal_id: %v", err)
	}
	if goalID != "goal-2" {
		t.Fatalf("goal_id = %q, want goal-2", goalID)
	}
	if err := store.UpsertPullRequest(ctx, PullRequest{RepoFullName: "jerryfane/gitmoot", Number: 1, URL: "https://github.com/jerryfane/gitmoot/pull/1", HeadBranch: "task", BaseBranch: "main", State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	if err := store.MarkCommentSeen(ctx, Comment{RepoFullName: "jerryfane/gitmoot", CommentID: 100, PullRequest: 1, Body: "/gitmoot audit review"}); err != nil {
		t.Fatalf("MarkCommentSeen returned error: %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "job-1", Agent: "audit", Type: "review", State: "queued"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, JobEvent{JobID: "job-1", Kind: "queued", Message: "created"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	acquired, err := store.AcquireLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "lead"})
	if err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("first AcquireLock did not acquire lock")
	}
	acquired, err = store.AcquireLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "lead"})
	if err != nil {
		t.Fatalf("same-owner AcquireLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("same-owner AcquireLock did not return acquired")
	}
	acquired, err = store.AcquireLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "other"})
	if err != nil {
		t.Fatalf("second AcquireLock returned error: %v", err)
	}
	if acquired {
		t.Fatal("second AcquireLock unexpectedly acquired lock")
	}
	if err := store.UpsertMergeGate(ctx, MergeGate{RepoFullName: "jerryfane/gitmoot", PullRequest: 1, State: "pending", Reason: "waiting"}); err != nil {
		t.Fatalf("UpsertMergeGate returned error: %v", err)
	}
	removed, err := store.RemoveAgent(ctx, "audit")
	if err != nil {
		t.Fatalf("RemoveAgent returned error: %v", err)
	}
	if !removed {
		t.Fatal("RemoveAgent did not remove existing agent")
	}
	removed, err = store.RemoveAgent(ctx, "audit")
	if err != nil {
		t.Fatalf("second RemoveAgent returned error: %v", err)
	}
	if removed {
		t.Fatal("second RemoveAgent removed missing agent")
	}
}
