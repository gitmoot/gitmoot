package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestReconcileTerminalDrivingJob(t *testing.T) {
	tests := []struct {
		name          string
		jobState      JobState
		decision      string
		parentJobID   string
		delegationID  string
		taskState     TaskState
		successor     bool
		wantTaskState TaskState
		wantEvent     string
	}{
		{name: "terminal failure blocks", jobState: JobFailed, decision: "failed", taskState: TaskImplementing, wantTaskState: TaskBlocked, wantEvent: TaskEventBlockedJobFailed},
		{name: "implemented success without PR blocks", jobState: JobSucceeded, decision: "implemented", taskState: TaskImplementing, wantTaskState: TaskBlocked, wantEvent: TaskEventBlockedTerminalNoPR},
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
			} else if len(events) != 1 || events[0].Kind != test.wantEvent {
				t.Fatalf("task events = %+v, want one %s", events, test.wantEvent)
			}
		})
	}
}

func TestJobKeepsTaskLiveTable(t *testing.T) {
	completionMarkers := []string{"advance_completed", "advance_retried", "advance_blocked", "advance_retry_skipped", "retry_queued"}
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
