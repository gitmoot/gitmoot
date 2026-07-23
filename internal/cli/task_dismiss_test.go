package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestTaskDismissPredicateFailsClosed(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{state: ""},
		{state: string(workflow.TaskPlanned)},
		{state: string(workflow.TaskImplementing), want: true},
		{state: string(workflow.TaskPullRequestOpen)},
		{state: string(workflow.TaskReviewing)},
		{state: string(workflow.TaskChangesRequested)},
		{state: string(workflow.TaskReadyToMerge)},
		{state: string(workflow.TaskMerged)},
		{state: string(workflow.TaskBlocked), want: true},
		{state: string(workflow.TaskAwaitingHumanMerge), want: true},
		{state: string(workflow.TaskAwaitingHuman)},
		{state: string(workflow.TaskDismissed)},
		{state: "future"},
	}
	for _, test := range tests {
		if got := taskDismissibleState(test.state); got != test.want {
			t.Fatalf("taskDismissibleState(%q) = %v, want %v", test.state, got, test.want)
		}
	}
}

func TestRunTaskDismissAllowedStatesJSONLockAndEvents(t *testing.T) {
	for _, state := range []workflow.TaskState{workflow.TaskImplementing, workflow.TaskBlocked, workflow.TaskAwaitingHumanMerge} {
		t.Run(string(state), func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			defer store.Close()
			worktree := filepath.Join(home, "preserved-worktree")
			task := db.Task{ID: "task-1", RepoFullName: "owner/repo", State: string(state), Branch: "feature/one", WorktreePath: worktree}
			if err := store.UpsertTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateLock(context.Background(), db.BranchLock{RepoFullName: task.RepoFullName, Branch: task.Branch, Owner: "worker"}); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := Run([]string{"task", "dismiss", task.ID, "--home", home, "--json"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			var output taskDismissOutput
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("decode %q: %v", stdout.String(), err)
			}
			if output.TaskID != task.ID || output.PreviousState != string(state) || output.State != string(workflow.TaskDismissed) || output.Source != "manual" || output.Reason != "dismissed by operator" || !output.Changed {
				t.Fatalf("output = %+v", output)
			}
			stored, _ := store.GetTask(context.Background(), task.ID)
			if stored.State != string(workflow.TaskDismissed) || stored.Branch != task.Branch || stored.WorktreePath != worktree {
				t.Fatalf("stored task = %+v", stored)
			}
			if _, err := store.GetBranchLock(context.Background(), task.RepoFullName, task.Branch); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("lock still present: %v", err)
			}
			events, _ := store.ListTaskEvents(context.Background(), task.ID)
			if len(events) != 1 || events[0].Kind != "task_dismissed_manual" || events[0].Reason != "dismissed by operator" {
				t.Fatalf("events = %+v", events)
			}

			stdout.Reset()
			stderr.Reset()
			code = Run([]string{"task", "dismiss", task.ID, "--home", home, "--reason", "ignored", "--json"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("idempotent code=%d stderr=%s", code, stderr.String())
			}
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || output.Changed {
				t.Fatalf("idempotent output=%+v err=%v", output, err)
			}
			events, _ = store.ListTaskEvents(context.Background(), task.ID)
			if len(events) != 1 {
				t.Fatalf("idempotent events = %+v", events)
			}

			stdout.Reset()
			stderr.Reset()
			if code := Run([]string{"task", "events", task.ID, "--home", home, "--json"}, &stdout, &stderr); code != 0 {
				t.Fatalf("events code=%d stderr=%s", code, stderr.String())
			}
			var listed []db.TaskEvent
			if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].Kind != "task_dismissed_manual" {
				t.Fatalf("listed=%+v err=%v", listed, err)
			}
			stdout.Reset()
			stderr.Reset()
			if code := Run([]string{"task", "events", task.ID, "--home", home}, &stdout, &stderr); code != 0 {
				t.Fatalf("plain events code=%d stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "task_dismissed_manual") || !strings.Contains(stdout.String(), "dismissed by operator") {
				t.Fatalf("plain events output=%q", stdout.String())
			}
		})
	}
}

func TestRunTaskDismissRefusesOwnedStates(t *testing.T) {
	states := []string{
		"", string(workflow.TaskPlanned), string(workflow.TaskPullRequestOpen),
		string(workflow.TaskReviewing), string(workflow.TaskChangesRequested),
		string(workflow.TaskReadyToMerge), string(workflow.TaskMerged),
		string(workflow.TaskAwaitingHuman), "future",
	}
	for _, state := range states {
		name := state
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			if err := store.UpsertTask(context.Background(), db.Task{ID: "task-1", State: state}); err != nil {
				t.Fatal(err)
			}
			store.Close()
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"task", "dismiss", "task-1", "--home", home}, &stdout, &stderr); code != 1 {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "only supports implementing or blocked") || !strings.Contains(stderr.String(), "owned by") {
				t.Fatalf("stderr = %s", stderr.String())
			}
		})
	}
}

func TestRunTaskDismissRefusesLiveJobAndWorktreeProcess(t *testing.T) {
	for _, test := range []struct {
		name        string
		liveJob     bool
		liveProcess bool
		want        string
	}{
		{name: "live job", liveJob: true, want: "live job job-1"},
		{name: "live process", liveProcess: true, want: "live process"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			worktree := filepath.Join(home, "worktree")
			task := db.Task{ID: "task-1", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "feature/one", WorktreePath: worktree}
			if err := store.UpsertTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			if test.liveJob {
				payload, _ := json.Marshal(workflow.JobPayload{TaskID: task.ID})
				if err := store.CreateJob(context.Background(), db.Job{ID: "job-1", Type: "ask", State: string(workflow.JobQueued), Payload: string(payload)}); err != nil {
					t.Fatal(err)
				}
			}
			store.Close()
			previous := taskWorktreeHasLiveProcess
			if test.liveProcess {
				taskWorktreeHasLiveProcess = func(path string) bool { return path == worktree }
			}
			defer func() { taskWorktreeHasLiveProcess = previous }()
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"task", "dismiss", task.ID, "--home", home}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunTaskRunRejectsDismissedTask(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	if err := store.UpsertTask(context.Background(), db.Task{ID: "task-1", RepoFullName: "owner/repo", State: string(workflow.TaskDismissed), Branch: "feature/one"}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"task", "run", "task-1", "--home", home, "--repo", "owner/repo", "--owner", "lead"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "dismissed") || !strings.Contains(stderr.String(), "task recover") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}
