package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestReconcileTerminalDrivingJob(t *testing.T) {
	tests := []struct {
		name             string
		jobState         JobState
		decision         string
		parentJobID      string
		delegationID     string
		taskState        TaskState
		successor        bool
		pullRequestState string
		wantTaskState    TaskState
		wantEvent        string
		wantReason       string
	}{
		{
			name:     "terminal failure without decision blocks",
			jobState: JobFailed, taskState: TaskImplementing,
			wantTaskState: TaskBlocked, wantEvent: TaskEventBlockedJobFailed,
			wantReason: "top-level implement job job-1 ended in failed without a pull request or live successor",
		},
		{
			name:     "terminal failure with decision blocks",
			jobState: JobFailed, decision: "failed", taskState: TaskImplementing,
			wantTaskState: TaskBlocked, wantEvent: TaskEventBlockedJobFailed,
			wantReason: "top-level implement job job-1 ended in failed with decision failed and no pull request or live successor",
		},
		{
			name:     "implemented success without PR blocks",
			jobState: JobSucceeded, decision: "implemented", taskState: TaskImplementing,
			wantTaskState: TaskBlocked, wantEvent: TaskEventBlockedTerminalNoPR,
			wantReason: `top-level implement job job-1 succeeded with decision implemented on branch "feature/one" at commit head123 but produced no open pull request or live successor`,
		},
		{
			name:     "implemented success with closed PR blocks",
			jobState: JobSucceeded, decision: "implemented", taskState: TaskImplementing, pullRequestState: "closed",
			wantTaskState: TaskBlocked, wantEvent: TaskEventBlockedTerminalNoPR,
			wantReason: `top-level implement job job-1 succeeded with decision implemented on branch "feature/one" at commit head123 but produced no open pull request or live successor`,
		},
		{name: "already advanced to pr_open is untouched", jobState: JobSucceeded, decision: "implemented", taskState: TaskPullRequestOpen, wantTaskState: TaskPullRequestOpen},
		{name: "delegation child is untouched", jobState: JobFailed, decision: "failed", parentJobID: "parent", delegationID: "child", taskState: TaskImplementing, wantTaskState: TaskImplementing},
		{name: "queued successor keeps task live", jobState: JobFailed, decision: "failed", taskState: TaskImplementing, successor: true, wantTaskState: TaskImplementing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", Branch: "feature/one", State: string(test.taskState)}); err != nil {
				t.Fatal(err)
			}
			payload := JobPayload{
				Repo: "owner/repo", Branch: "feature/one", TaskID: "task-1",
				HeadSHA:     "head123",
				ParentJobID: test.parentJobID, DelegationID: test.delegationID,
				Result: &AgentResult{Decision: test.decision, Summary: "done"},
			}
			encoded, _ := json.Marshal(payload)
			if err := store.CreateJob(ctx, db.Job{ID: "job-1", Type: "implement", State: string(test.jobState), Payload: string(encoded)}); err != nil {
				t.Fatal(err)
			}
			if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-1", Kind: "advance_completed"}); err != nil {
				t.Fatal(err)
			}
			if test.pullRequestState != "" {
				if err := store.UpsertPullRequest(ctx, db.PullRequest{
					RepoFullName: "owner/repo",
					Number:       41,
					HeadBranch:   "feature/one",
					HeadSHA:      "head123",
					State:        test.pullRequestState,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.successor {
				successorPayload, _ := json.Marshal(JobPayload{Repo: "owner/repo", Branch: "feature/one", TaskID: "task-1"})
				if err := store.CreateJob(ctx, db.Job{ID: "job-2", Type: "implement", State: string(JobQueued), Payload: string(successorPayload)}); err != nil {
					t.Fatal(err)
				}
			}
			engine := Engine{Store: store}
			for i := 0; i < 2; i++ {
				if err := engine.ReconcileTerminalDrivingJob(ctx, "job-1"); err != nil {
					t.Fatalf("ReconcileTerminalDrivingJob pass %d: %v", i+1, err)
				}
			}
			task, err := store.GetTask(ctx, "task-1")
			if err != nil || task.State != string(test.wantTaskState) {
				t.Fatalf("task = %+v, err=%v; want %s", task, err, test.wantTaskState)
			}
			events, err := store.ListTaskEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantEvent == "" {
				if len(events) != 0 {
					t.Fatalf("unexpected task events = %+v", events)
				}
			} else if len(events) != 1 || events[0].Kind != test.wantEvent || events[0].Reason != test.wantReason {
				t.Fatalf("task events = %+v, want one %s", events, test.wantEvent)
			}
		})
	}
}

func TestReconcileTerminalDrivingJobSplitsOpenPRFromStrand(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := Engine{Store: store}

	type fixture struct {
		taskID string
		jobID  string
		branch string
		head   string
	}
	healthy := fixture{taskID: "task-fix-pass", jobID: "job-fix-pass", branch: "feature/fix-pass", head: "abc123"}
	stranded := fixture{taskID: "task-stranded", jobID: "job-stranded", branch: "feature/stranded", head: "def456"}
	for _, item := range []fixture{healthy, stranded} {
		if err := store.UpsertTask(ctx, db.Task{
			ID:           item.taskID,
			RepoFullName: "owner/repo",
			Branch:       item.branch,
			State:        string(TaskImplementing),
		}); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(JobPayload{
			Repo:    "owner/repo",
			Branch:  item.branch,
			TaskID:  item.taskID,
			HeadSHA: item.head,
			Result:  &AgentResult{Decision: "implemented", Summary: "done"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateJob(ctx, db.Job{ID: item.jobID, Type: "implement", State: string(JobSucceeded), Payload: string(encoded)}); err != nil {
			t.Fatal(err)
		}
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: item.jobID, Kind: "advance_completed"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo",
		Number:       41,
		HeadBranch:   healthy.branch,
		HeadSHA:      healthy.head,
		State:        "open",
	}); err != nil {
		t.Fatal(err)
	}

	for _, item := range []fixture{healthy, stranded} {
		if err := engine.ReconcileTerminalDrivingJob(ctx, item.jobID); err != nil {
			t.Fatalf("ReconcileTerminalDrivingJob(%s): %v", item.jobID, err)
		}
	}

	healthyTask, err := store.GetTask(ctx, healthy.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if healthyTask.State != string(TaskPullRequestOpen) {
		t.Fatalf("healthy task state = %s, want %s", healthyTask.State, TaskPullRequestOpen)
	}
	strandedTask, err := store.GetTask(ctx, stranded.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if strandedTask.State != string(TaskBlocked) {
		t.Fatalf("stranded task state = %s, want %s", strandedTask.State, TaskBlocked)
	}

	healthyEvents, err := store.ListTaskEvents(ctx, healthy.taskID)
	if err != nil {
		t.Fatal(err)
	}
	strandedEvents, err := store.ListTaskEvents(ctx, stranded.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(healthyEvents) != 1 || healthyEvents[0].Kind != TaskEventTerminalPushedToOpenPR ||
		!strings.Contains(healthyEvents[0].Reason, "#41") || !strings.Contains(healthyEvents[0].Reason, healthy.branch) {
		t.Fatalf("healthy events = %+v, want open-PR informational transition", healthyEvents)
	}
	if len(strandedEvents) != 1 || strandedEvents[0].Kind != TaskEventBlockedTerminalNoPR ||
		!strings.Contains(strandedEvents[0].Reason, stranded.branch) || !strings.Contains(strandedEvents[0].Reason, stranded.head) {
		t.Fatalf("stranded events = %+v, want blocked event naming branch and SHA", strandedEvents)
	}
	if healthyEvents[0].Kind == strandedEvents[0].Kind || healthyEvents[0].Reason == strandedEvents[0].Reason {
		t.Fatalf("healthy and stranded outcomes must differ: healthy=%+v stranded=%+v", healthyEvents[0], strandedEvents[0])
	}

	blocked, err := store.ListTasksByRepoState(ctx, "owner/repo", string(TaskBlocked))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 || blocked[0].ID != stranded.taskID {
		t.Fatalf("blocked tasks = %+v, want only %s", blocked, stranded.taskID)
	}
}

func TestFindLiveTaskJobRepoScoped(t *testing.T) {
	tests := []struct {
		name      string
		taskRepo  string
		payload   JobPayload
		state     JobState
		events    []db.JobEvent
		wantJobID string
	}{
		{
			name:      "task id match",
			taskRepo:  "owner/repo",
			payload:   JobPayload{Repo: "owner/repo", Branch: "feature/other", TaskID: "task-1"},
			state:     JobQueued,
			wantJobID: "z-live",
		},
		{
			name:      "repo and branch match",
			taskRepo:  "owner/repo",
			payload:   JobPayload{Repo: "owner/repo", Branch: "feature/shared", TaskID: "other-task"},
			state:     JobRunning,
			wantJobID: "z-live",
		},
		{
			name:      "settled job with advancement pending",
			taskRepo:  "owner/repo",
			payload:   JobPayload{Repo: "owner/repo", Branch: "feature/shared"},
			state:     JobSucceeded,
			events:    []db.JobEvent{{Kind: "advance_started"}},
			wantJobID: "z-live",
		},
		{
			name:      "cancelled from running remains live",
			taskRepo:  "owner/repo",
			payload:   JobPayload{Repo: "owner/repo", Branch: "feature/shared"},
			state:     JobCancelled,
			events:    []db.JobEvent{{Kind: string(JobCancelled), Message: "cancel requested from running"}},
			wantJobID: "z-live",
		},
		{
			name:      "empty repo falls back to branch scan",
			payload:   JobPayload{Branch: "feature/shared"},
			state:     JobQueued,
			wantJobID: "z-live",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			task := db.Task{ID: "task-1", RepoFullName: test.taskRepo, Branch: "feature/shared"}

			for _, seed := range []struct {
				id      string
				payload JobPayload
			}{
				{id: "a-unrelated-same-branch", payload: JobPayload{Repo: "other/repo", Branch: task.Branch}},
				{id: "b-third-repo", payload: JobPayload{Repo: "third/repo", Branch: task.Branch}},
				{id: "c-same-repo-wrong-branch", payload: JobPayload{Repo: test.taskRepo, Branch: "feature/wrong", TaskID: "other-task"}},
			} {
				encoded, err := json.Marshal(seed.payload)
				if err != nil {
					t.Fatalf("Marshal(%s): %v", seed.id, err)
				}
				if err := store.CreateJob(ctx, db.Job{ID: seed.id, Type: "implement", State: string(JobQueued), Payload: string(encoded)}); err != nil {
					t.Fatalf("CreateJob(%s): %v", seed.id, err)
				}
			}

			encoded, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatalf("Marshal live payload: %v", err)
			}
			if err := store.CreateJob(ctx, db.Job{ID: "z-live", Type: "implement", State: string(test.state), Payload: string(encoded)}); err != nil {
				t.Fatalf("CreateJob(z-live): %v", err)
			}
			for _, event := range test.events {
				event.JobID = "z-live"
				if err := store.AddJobEvent(ctx, event); err != nil {
					t.Fatalf("AddJobEvent(%s): %v", event.Kind, err)
				}
			}

			got, live, err := FindLiveTaskJob(ctx, store, task)
			if err != nil {
				t.Fatalf("FindLiveTaskJob: %v", err)
			}
			if !live || got.ID != test.wantJobID {
				t.Fatalf("FindLiveTaskJob = job %+v live=%v, want %q live", got, live, test.wantJobID)
			}
		})
	}
}

func TestFindLiveTaskJobFindsLegacyJobAfterRepoBackfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	task := db.Task{ID: "task-1", RepoFullName: "owner/repo", Branch: "feature/shared"}
	encoded, err := json.Marshal(JobPayload{Repo: task.RepoFullName, Branch: task.Branch, TaskID: task.ID})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "legacy-live", Type: "implement", State: string(JobQueued), Payload: string(encoded)}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close before legacy seed: %v", err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE jobs SET repo = '' WHERE id = 'legacy-live'`); err != nil {
		raw.Close()
		t.Fatalf("clear projected repo: %v", err)
	}
	// Apply the #1066 repair shape directly. The db package owns a separate
	// content-addressed migration test; this workflow test only needs the
	// post-backfill row to prove the indexed liveness lookup.
	if _, err := raw.ExecContext(ctx, `UPDATE jobs SET repo = json_extract(payload, '$.repo')
		WHERE repo = '' AND json_valid(payload) AND COALESCE(json_extract(payload, '$.repo'), '') != ''`); err != nil {
		raw.Close()
		t.Fatalf("backfill projected repo: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw Close: %v", err)
	}

	store, err = db.Open(path)
	if err != nil {
		t.Fatalf("reopen with repo backfill migration: %v", err)
	}
	defer store.Close()

	got, live, err := FindLiveTaskJob(ctx, store, task)
	if err != nil {
		t.Fatalf("FindLiveTaskJob: %v", err)
	}
	if !live || got.ID != "legacy-live" {
		t.Fatalf("FindLiveTaskJob = job %+v live=%v, want legacy-live live", got, live)
	}
}

func TestJobKeepsTaskLiveTable(t *testing.T) {
	completionMarkers := []string{"advance_completed", "advance_retried", "advance_blocked", "advance_retry_skipped", "retry_queued", ReviewLoopDetectedEventKind}
	type testCase struct {
		name   string
		state  JobState
		type_  string
		events []db.JobEvent
		want   bool
	}
	tests := []testCase{
		{name: "succeeded pending advance", state: JobSucceeded, events: kinds("advance_started"), want: true},
		{name: "failed retry pending", state: JobFailed, events: kinds("advance_retry"), want: true},
		{name: "cancelled from running unsettled", state: JobCancelled, events: []db.JobEvent{{Kind: "cancelled", Message: "cancel requested from running"}}, want: true},
		{name: "cancelled from queued", state: JobCancelled, events: []db.JobEvent{{Kind: "cancelled", Message: "cancel requested from queued"}}},
		{name: "cancelled settled", state: JobCancelled, events: []db.JobEvent{{Kind: "cancelled", Message: "cancel requested from running"}, {Kind: "cancel_settled"}}},
	}
	for _, jobType := range []string{"ask", "review", "implement", "produce", "plan", "review-prep", "review-dispatch"} {
		tests = append(tests,
			testCase{name: "queued " + jobType, state: JobQueued, type_: jobType, want: true},
			testCase{name: "running " + jobType, state: JobRunning, type_: jobType, want: true},
		)
	}
	for _, marker := range completionMarkers {
		tests = append(tests, testCase{name: "settled by " + marker, state: JobSucceeded, events: kinds("advance_started", marker)})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := db.Job{ID: "job", Type: test.type_, State: string(test.state)}
			if got := jobKeepsTaskLive(job, test.events); got != test.want {
				t.Fatalf("jobKeepsTaskLive(%s, %v) = %v, want %v", test.state, test.events, got, test.want)
			}
		})
	}
}

func TestJobMatchesTaskIdentityTable(t *testing.T) {
	task := db.Task{ID: "task-1", RepoFullName: "owner/repo", Branch: "feature/one"}
	tests := []struct {
		name    string
		payload JobPayload
		task    db.Task
		want    bool
	}{
		{name: "task id", payload: JobPayload{TaskID: "task-1"}, task: task, want: true},
		{name: "repo branch", payload: JobPayload{Repo: "owner/repo", Branch: "feature/one"}, task: task, want: true},
		{name: "wrong repo", payload: JobPayload{Repo: "other/repo", Branch: "feature/one"}, task: task},
		{name: "wrong branch", payload: JobPayload{Repo: "owner/repo", Branch: "feature/two"}, task: task},
		{name: "empty task branch never matches", payload: JobPayload{Repo: "owner/repo", Branch: ""}, task: db.Task{ID: "other", RepoFullName: "owner/repo"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := jobMatchesTask(test.payload, test.task); got != test.want {
				t.Fatalf("jobMatchesTask(%+v, %+v) = %v, want %v", test.payload, test.task, got, test.want)
			}
		})
	}
}

func kinds(values ...string) []db.JobEvent {
	events := make([]db.JobEvent, 0, len(values))
	for _, value := range values {
		events = append(events, db.JobEvent{Kind: value})
	}
	return events
}
