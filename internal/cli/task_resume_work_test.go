package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestTaskResumeWorkPredicateFailsClosed(t *testing.T) {
	eligible := map[workflow.TaskState]bool{
		workflow.TaskReviewing:          true,
		workflow.TaskReadyToMerge:       true,
		workflow.TaskAwaitingHumanMerge: true,
	}
	for _, state := range []workflow.TaskState{
		"", workflow.TaskPlanned, workflow.TaskImplementing,
		workflow.TaskPullRequestOpen, workflow.TaskReviewing,
		workflow.TaskChangesRequested, workflow.TaskReadyToMerge,
		workflow.TaskMerged, workflow.TaskBlocked,
		workflow.TaskAwaitingHumanMerge, workflow.TaskAwaitingHuman,
		workflow.TaskDismissed, "future",
	} {
		if got := taskResumeWorkEligibleState(string(state)); got != eligible[state] {
			t.Fatalf("taskResumeWorkEligibleState(%q) = %v, want %v", state, got, eligible[state])
		}
	}
}

func TestRunTaskResumeWorkTransitionsAndReenablesFixPass(t *testing.T) {
	for _, state := range []workflow.TaskState{
		workflow.TaskReviewing,
		workflow.TaskReadyToMerge,
		workflow.TaskAwaitingHumanMerge,
	} {
		t.Run(string(state), func(t *testing.T) {
			previousLiveness := taskWorktreeLiveness
			taskWorktreeLiveness = func(string) (bool, bool) { return false, true }
			t.Cleanup(func() { taskWorktreeLiveness = previousLiveness })
			fixture := newFixPassFixture(t, state)
			installFixPassPullRequestClient(t, fixture.pullRequest())
			args := []string{"task", "resume-work", fixture.task.ID, "--home", fixture.home, "--reason", "coordinator requested fixes", "--json"}
			wantEventReason := "coordinator requested fixes"
			if state == workflow.TaskAwaitingHumanMerge {
				args = append(args, "--override-pending-human-decision")
				wantEventReason += "; override_pending_human_decision=true"
			}

			var stdout, stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("resume-work code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			var output taskResumeWorkOutput
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("decode output %q: %v", stdout.String(), err)
			}
			if !output.Changed || output.PreviousState != string(state) ||
				output.State != string(workflow.TaskImplementing) ||
				output.OverridePendingHumanDecision != (state == workflow.TaskAwaitingHumanMerge) {
				t.Fatalf("output = %+v", output)
			}
			task, err := fixture.store.GetTask(context.Background(), fixture.task.ID)
			if err != nil || task.State != string(workflow.TaskImplementing) {
				t.Fatalf("task = %+v, err=%v", task, err)
			}
			lock, err := fixture.store.GetBranchLock(context.Background(), fixture.repo.FullName(), fixture.branch)
			if err != nil || lock.Owner != fixture.owner {
				t.Fatalf("branch lock = %+v, err=%v; want preserved owner %q", lock, err, fixture.owner)
			}
			events, err := fixture.store.ListTaskEvents(context.Background(), fixture.task.ID)
			if err != nil || len(events) != 1 ||
				events[0].Kind != "task_resume_work_manual" ||
				events[0].Reason != wantEventReason {
				t.Fatalf("events = %+v, err=%v", events, err)
			}

			started, request, err := prepareLocalImplementDispatchRequest(
				context.Background(), fixture.store, fixture.record, fixture.repo,
				localAgentDispatchRequest{
					Home: fixture.home, Agent: fixture.owner, Action: "implement",
					Instructions: "address review findings", PullRequest: fixture.pullNumber,
					ImplementBase: "HEAD",
				},
			)
			if err != nil {
				t.Fatalf("fix-pass after resume-work: %v", err)
			}
			if started.ID != fixture.task.ID || started.WorktreePath != fixture.task.WorktreePath ||
				request.TaskID != fixture.task.ID || request.PullRequest != fixture.pullNumber {
				t.Fatalf("fix-pass binding = task %+v request %+v", started, request)
			}
		})
	}
}

func TestRunTaskResumeWorkAwaitingHumanMergeRequiresReasonAndOverride(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing override",
			args: []string{"--reason", "more work needed"},
			want: "--override-pending-human-decision",
		},
		{
			name: "missing reason",
			args: []string{"--override-pending-human-decision"},
			want: "requires --reason",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			task := db.Task{ID: "task-1", State: string(workflow.TaskAwaitingHumanMerge)}
			if err := store.UpsertTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			store.Close()
			args := append([]string{"task", "resume-work", task.ID, "--home", home}, test.args...)
			var stdout, stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("code=%d stdout=%s stderr=%s; want %q", code, stdout.String(), stderr.String(), test.want)
			}
			store = openCLIJobStore(t, home)
			defer store.Close()
			stored, err := store.GetTask(context.Background(), task.ID)
			if err != nil || stored.State != string(workflow.TaskAwaitingHumanMerge) {
				t.Fatalf("task = %+v, err=%v", stored, err)
			}
			events, err := store.ListTaskEvents(context.Background(), task.ID)
			if err != nil || len(events) != 0 {
				t.Fatalf("events = %+v, err=%v", events, err)
			}
		})
	}
}

func TestRunTaskResumeWorkRefusesLiveJobAndProcessForEveryEligibleState(t *testing.T) {
	for _, state := range []workflow.TaskState{
		workflow.TaskReviewing,
		workflow.TaskReadyToMerge,
		workflow.TaskAwaitingHumanMerge,
	} {
		for _, test := range []struct {
			name        string
			liveJob     bool
			liveProcess bool
			unknown     bool
			want        string
		}{
			{name: "live job", liveJob: true, want: "live job job-1"},
			{name: "live process", liveProcess: true, want: "live process"},
			{name: "unknown process liveness", unknown: true, want: "process liveness could not be determined"},
		} {
			t.Run(string(state)+"/"+test.name, func(t *testing.T) {
				home := t.TempDir()
				store := openCLIJobStore(t, home)
				task := db.Task{
					ID: "task-1", RepoFullName: "owner/repo", State: string(state),
					Branch: "feature/one", WorktreePath: home + "/worktree",
				}
				if err := store.UpsertTask(context.Background(), task); err != nil {
					t.Fatal(err)
				}
				if test.liveJob {
					payload, _ := json.Marshal(workflow.JobPayload{
						Repo: task.RepoFullName, Branch: task.Branch, TaskID: task.ID,
					})
					if err := store.CreateJob(context.Background(), db.Job{
						ID: "job-1", Type: "review", State: string(workflow.JobQueued), Payload: string(payload),
					}); err != nil {
						t.Fatal(err)
					}
				}
				store.Close()

				previous := taskWorktreeLiveness
				if test.liveProcess || test.unknown {
					taskWorktreeLiveness = func(path string) (bool, bool) {
						return test.liveProcess && path == task.WorktreePath, !test.unknown
					}
				}
				t.Cleanup(func() { taskWorktreeLiveness = previous })
				args := []string{"task", "resume-work", task.ID, "--home", home, "--reason", "resume"}
				if state == workflow.TaskAwaitingHumanMerge {
					args = append(args, "--override-pending-human-decision")
				}
				var stdout, stderr bytes.Buffer
				if code := Run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), test.want) {
					t.Fatalf("code=%d stdout=%s stderr=%s; want %q", code, stdout.String(), stderr.String(), test.want)
				}
			})
		}
	}
}

func TestRunTaskResumeWorkRefusesEveryOtherStateWithOwner(t *testing.T) {
	tests := []struct {
		state string
		owner string
	}{
		{"", "other workflow machinery"},
		{string(workflow.TaskPlanned), "task run/implement dispatch"},
		{string(workflow.TaskImplementing), "active implementation and recovery machinery"},
		{string(workflow.TaskPullRequestOpen), "pull-request review and merge machinery"},
		{string(workflow.TaskChangesRequested), "implement fix-pass dispatch"},
		{string(workflow.TaskMerged), "the terminal merge record"},
		{string(workflow.TaskBlocked), "active implementation and recovery machinery"},
		{string(workflow.TaskAwaitingHuman), "the explicit human-resume machinery"},
		{string(workflow.TaskDismissed), "explicit task recovery"},
		{"future", "other workflow machinery"},
	}
	for _, test := range tests {
		name := test.state
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			if err := store.UpsertTask(context.Background(), db.Task{ID: "task-1", State: test.state}); err != nil {
				t.Fatal(err)
			}
			store.Close()
			var stdout, stderr bytes.Buffer
			code := Run([]string{
				"task", "resume-work", "task-1", "--home", home, "--reason", "resume",
			}, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), "owned by "+test.owner) {
				t.Fatalf("code=%d stdout=%s stderr=%s; want owner %q", code, stdout.String(), stderr.String(), test.owner)
			}
		})
	}
}
