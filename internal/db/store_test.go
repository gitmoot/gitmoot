package db

import (
	"context"
	"database/sql"
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
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.GoalID != "goal-2" {
		t.Fatalf("task goal_id = %q, want goal-2", task.GoalID)
	}
	if err := store.UpsertPullRequest(ctx, PullRequest{RepoFullName: "jerryfane/gitmoot", Number: 1, URL: "https://github.com/jerryfane/gitmoot/pull/1", HeadBranch: "task", BaseBranch: "main", HeadSHA: "abc123", State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	pr, err := store.GetPullRequest(ctx, "jerryfane/gitmoot", 1)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.HeadSHA != "abc123" {
		t.Fatalf("pull request head sha = %q, want abc123", pr.HeadSHA)
	}
	if err := store.MarkCommentSeen(ctx, Comment{RepoFullName: "jerryfane/gitmoot", CommentID: 100, PullRequest: 1, Body: "/gitmoot audit review"}); err != nil {
		t.Fatalf("MarkCommentSeen returned error: %v", err)
	}
	seen, err := store.HasCommentSeen(ctx, "jerryfane/gitmoot", 100)
	if err != nil {
		t.Fatalf("HasCommentSeen returned error: %v", err)
	}
	if !seen {
		t.Fatal("HasCommentSeen did not find marked comment")
	}
	isNew, err := store.MarkCommentSeenIfNew(ctx, Comment{RepoFullName: "jerryfane/gitmoot", CommentID: 101, PullRequest: 1, Body: "/gitmoot audit review again"})
	if err != nil {
		t.Fatalf("MarkCommentSeenIfNew returned error: %v", err)
	}
	if !isNew {
		t.Fatal("MarkCommentSeenIfNew did not report new comment")
	}
	isNew, err = store.MarkCommentSeenIfNew(ctx, Comment{RepoFullName: "jerryfane/gitmoot", CommentID: 101, PullRequest: 1, Body: "/gitmoot audit review again"})
	if err != nil {
		t.Fatalf("duplicate MarkCommentSeenIfNew returned error: %v", err)
	}
	if isNew {
		t.Fatal("MarkCommentSeenIfNew reported duplicate comment as new")
	}
	if err := store.CreateJob(ctx, Job{ID: "job-1", Agent: "audit", Type: "review", State: "queued"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != "queued" {
		t.Fatalf("job state = %q, want queued", job.State)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "job-1" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if err := store.UpdateJobState(ctx, "job-1", "running"); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	transitioned, err := store.TransitionJobState(ctx, "job-1", "queued", "running")
	if err != nil {
		t.Fatalf("TransitionJobState stale returned error: %v", err)
	}
	if transitioned {
		t.Fatal("TransitionJobState unexpectedly changed a non-matching state")
	}
	transitioned, err = store.TransitionJobState(ctx, "job-1", "running", "succeeded")
	if err != nil {
		t.Fatalf("TransitionJobState returned error: %v", err)
	}
	if !transitioned {
		t.Fatal("TransitionJobState did not change matching state")
	}
	if err := store.CreateJob(ctx, Job{ID: "job-2", Agent: "audit", Type: "review", State: "queued"}); err != nil {
		t.Fatalf("second CreateJob returned error: %v", err)
	}
	transitioned, err = store.TransitionJobStateWithEvent(ctx, "job-2", "queued", "running", JobEvent{Kind: "running", Message: "started"})
	if err != nil {
		t.Fatalf("TransitionJobStateWithEvent returned error: %v", err)
	}
	if !transitioned {
		t.Fatal("TransitionJobStateWithEvent did not change matching state")
	}
	jobEvents, err := store.ListJobEvents(ctx, "job-2")
	if err != nil {
		t.Fatalf("ListJobEvents for job-2 returned error: %v", err)
	}
	if len(jobEvents) != 1 || jobEvents[0].Kind != "running" {
		t.Fatalf("job-2 events = %+v", jobEvents)
	}
	if err := store.CreateJobWithEvent(ctx, Job{ID: "job-3", Agent: "audit", Type: "review", State: "queued"}, JobEvent{Kind: "queued", Message: "created"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	jobEvents, err = store.ListJobEvents(ctx, "job-3")
	if err != nil {
		t.Fatalf("ListJobEvents for job-3 returned error: %v", err)
	}
	if len(jobEvents) != 1 || jobEvents[0].Kind != "queued" {
		t.Fatalf("job-3 events = %+v", jobEvents)
	}
	transitioned, err = store.TransitionJobStatePayloadWithEvent(ctx, "job-3", "queued", "succeeded", `{"result":{"summary":"ok"}}`, JobEvent{Kind: "succeeded", Message: "done"})
	if err != nil {
		t.Fatalf("TransitionJobStatePayloadWithEvent returned error: %v", err)
	}
	if !transitioned {
		t.Fatal("TransitionJobStatePayloadWithEvent did not change matching state")
	}
	job, err = store.GetJob(ctx, "job-3")
	if err != nil {
		t.Fatalf("GetJob for job-3 returned error: %v", err)
	}
	if job.State != "succeeded" || job.Payload != `{"result":{"summary":"ok"}}` {
		t.Fatalf("job-3 = %+v", job)
	}
	if err := store.UpdateJobPayload(ctx, "job-1", `{"raw_outputs":["ok"]}`); err != nil {
		t.Fatalf("UpdateJobPayload returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, JobEvent{JobID: "job-1", Kind: "queued", Message: "created"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "queued" {
		t.Fatalf("events = %+v", events)
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
	lock, err := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.Owner != "lead" {
		t.Fatalf("lock owner = %q, want lead", lock.Owner)
	}
	created, err := store.CreateLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "lead"})
	if err != nil {
		t.Fatalf("CreateLock existing returned error: %v", err)
	}
	if created {
		t.Fatal("CreateLock reported existing lock as newly created")
	}
	acquired, err = store.AcquireLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "other"})
	if err != nil {
		t.Fatalf("second AcquireLock returned error: %v", err)
	}
	if acquired {
		t.Fatal("second AcquireLock unexpectedly acquired lock")
	}
	released, err := store.ReleaseLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "other"})
	if err != nil {
		t.Fatalf("wrong-owner ReleaseLock returned error: %v", err)
	}
	if released {
		t.Fatal("wrong-owner ReleaseLock released lock")
	}
	released, err = store.ReleaseLock(ctx, BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task", Owner: "lead"})
	if err != nil {
		t.Fatalf("ReleaseLock returned error: %v", err)
	}
	if !released {
		t.Fatal("ReleaseLock did not release owned lock")
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

func TestTasksRequireUniqueNonEmptyBranches(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertTask(ctx, Task{ID: "task-1", GoalID: "goal-1", Title: "First", State: "planned", Branch: "task-branch"}); err != nil {
		t.Fatalf("UpsertTask first returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-2", GoalID: "goal-1", Title: "Second", State: "planned", Branch: "task-branch"}); err == nil {
		t.Fatal("UpsertTask allowed two tasks to share one branch")
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-empty-1", GoalID: "goal-1", Title: "Empty 1", State: "planned"}); err != nil {
		t.Fatalf("UpsertTask empty first returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, Task{ID: "task-empty-2", GoalID: "goal-1", Title: "Empty 2", State: "planned"}); err != nil {
		t.Fatalf("UpsertTask empty second returned error: %v", err)
	}
}

func TestTasksAllowSameBranchAcrossRepos(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	first := Task{ID: "task-1", RepoFullName: "jerryfane/gitmoot", GoalID: "goal-1", Title: "First", State: "planned", Branch: "task-branch"}
	second := Task{ID: "task-2", RepoFullName: "jerryfane/other", GoalID: "goal-1", Title: "Second", State: "planned", Branch: "task-branch"}
	if err := store.UpsertTask(ctx, first); err != nil {
		t.Fatalf("UpsertTask first returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, second); err != nil {
		t.Fatalf("UpsertTask second repo returned error: %v", err)
	}
	got, err := store.GetTaskByRepoBranch(ctx, "jerryfane/other", "task-branch")
	if err != nil {
		t.Fatalf("GetTaskByRepoBranch returned error: %v", err)
	}
	if got.ID != "task-2" {
		t.Fatalf("repo scoped task = %q, want task-2", got.ID)
	}
}

func TestMigrationDeduplicatesExistingTaskBranches(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations returned error: %v", err)
	}
	for version, migration := range migrations[:2] {
		if _, err := raw.ExecContext(ctx, migration); err != nil {
			t.Fatalf("apply seed migration %d returned error: %v", version+1, err)
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, 'test')`, version+1); err != nil {
			t.Fatalf("record seed migration %d returned error: %v", version+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO tasks(id, goal_id, title, state, branch, updated_at) VALUES
		('task-old', 'goal-1', 'Old', 'planned', 'task-branch', '2026-01-01T00:00:00Z'),
		('task-new', 'goal-1', 'New', 'planned', 'task-branch', '2026-01-02T00:00:00Z')`); err != nil {
		t.Fatalf("insert duplicate tasks returned error: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw Close returned error: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	kept, err := store.GetTaskByBranch(ctx, "task-branch")
	if err != nil {
		t.Fatalf("GetTaskByBranch returned error: %v", err)
	}
	if kept.ID != "task-new" {
		t.Fatalf("kept task = %q, want latest task-new", kept.ID)
	}
	old, err := store.GetTask(ctx, "task-old")
	if err != nil {
		t.Fatalf("GetTask old returned error: %v", err)
	}
	if old.Branch != "" {
		t.Fatalf("duplicate task branch = %q, want cleared", old.Branch)
	}
}
