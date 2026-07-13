package workflow

import (
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

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
