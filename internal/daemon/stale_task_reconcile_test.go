package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

type fakeRemoteBranchChecker struct {
	present map[string]struct{}
	err     error
	calls   [][]string
}

func (f *fakeRemoteBranchChecker) RemoteBranches(_ context.Context, _ string, branches []string) (map[string]struct{}, error) {
	f.calls = append(f.calls, append([]string(nil), branches...))
	if f.err != nil {
		return nil, f.err
	}
	return f.present, nil
}

func TestStaleTaskReconcilerDecisions(t *testing.T) {
	tests := []struct {
		name        string
		state       workflow.TaskState
		branch      string
		open        bool
		remote      bool
		live        bool
		belowTTL    bool
		wantDismiss bool
		wantCalls   int
		wantReason  string
	}{
		{name: "below threshold", state: workflow.TaskImplementing, branch: "feature/one", belowTTL: true},
		{name: "open PR branch", state: workflow.TaskImplementing, branch: "feature/one", open: true},
		{name: "remote present", state: workflow.TaskImplementing, branch: "feature/one", remote: true, wantCalls: 1},
		{name: "remote absent", state: workflow.TaskImplementing, branch: "feature/one", wantDismiss: true, wantCalls: 1, wantReason: "refs/heads/feature/one absent"},
		{name: "blocked remote absent", state: workflow.TaskBlocked, branch: "feature/blocked", wantDismiss: true, wantCalls: 1, wantReason: "refs/heads/feature/blocked absent"},
		{name: "empty branch", state: workflow.TaskImplementing, wantDismiss: true, wantReason: "empty branch"},
		{name: "live job", state: workflow.TaskImplementing, branch: "feature/live", live: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			repo := github.Repository{Owner: "owner", Name: "repo"}
			seedStaleRepo(t, store, repo)
			writeStaleTaskConfig(t, store, "168h")
			task := db.Task{ID: "task-1", RepoFullName: repo.FullName(), State: string(test.state), Branch: test.branch}
			if err := store.UpsertTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if test.live {
				payload, _ := json.Marshal(workflow.JobPayload{Repo: repo.FullName(), Branch: test.branch, TaskID: task.ID})
				if err := store.CreateJob(ctx, db.Job{ID: "job-live", Type: "review", State: string(workflow.JobQueued), Payload: string(payload)}); err != nil {
					t.Fatal(err)
				}
			}
			remote := &fakeRemoteBranchChecker{present: map[string]struct{}{}}
			if test.remote {
				remote.present[test.branch] = struct{}{}
			}
			now := time.Now().UTC().Add(365 * 24 * time.Hour)
			if test.belowTTL {
				now = time.Now().UTC()
			}
			d := Daemon{Repo: repo, Store: store, RemoteBranches: remote, Now: func() time.Time { return now }}
			open := map[string]struct{}{}
			if test.open {
				open[test.branch] = struct{}{}
			}
			if err := d.reconcileStaleTasks(ctx, open); err != nil {
				t.Fatalf("reconcileStaleTasks: %v", err)
			}
			got, err := store.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if dismissed := got.State == string(workflow.TaskDismissed); dismissed != test.wantDismiss {
				t.Fatalf("task state = %s, wantDismiss %v", got.State, test.wantDismiss)
			}
			if len(remote.calls) != test.wantCalls {
				t.Fatalf("remote calls = %v, want %d", remote.calls, test.wantCalls)
			}
			events, err := store.ListTaskEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantDismiss {
				if len(events) != 1 || events[0].Kind != "task_dismissed_auto" || !strings.Contains(events[0].Reason, test.wantReason) || !strings.Contains(events[0].Reason, "ttl=168h0m0s") || !strings.Contains(events[0].Reason, "updated_at=") {
					t.Fatalf("events = %+v", events)
				}
			} else if len(events) != 0 {
				t.Fatalf("unexpected events = %+v", events)
			}
		})
	}
}

func TestStaleTaskReconcilerRemoteErrorMutatesNothing(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "owner", Name: "repo"}
	seedStaleRepo(t, store, repo)
	writeStaleTaskConfig(t, store, "1h")
	tasks := []db.Task{
		{ID: "task-a", RepoFullName: repo.FullName(), State: string(workflow.TaskImplementing), Branch: "feature/task-a"},
		{ID: "task-b", RepoFullName: repo.FullName(), State: string(workflow.TaskImplementing), Branch: "feature/task-b"},
		{ID: "task-empty", RepoFullName: repo.FullName(), State: string(workflow.TaskImplementing)},
	}
	for _, task := range tasks {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	remote := &fakeRemoteBranchChecker{err: errors.New("network down")}
	logs := []string{}
	d := Daemon{Repo: repo, Store: store, RemoteBranches: remote, Now: futureClock, Logf: func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}}
	if err := d.reconcileStaleTasks(ctx, map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}
	if len(remote.calls) != 1 || len(logs) != 1 {
		t.Fatalf("remote calls=%v logs=%v, want one each", remote.calls, logs)
	}
	for _, seeded := range tasks {
		task, _ := store.GetTask(ctx, seeded.ID)
		events, _ := store.ListTaskEvents(ctx, seeded.ID)
		if task.State != string(workflow.TaskImplementing) || len(events) != 0 {
			t.Fatalf("%s mutated: task=%+v events=%+v", seeded.ID, task, events)
		}
	}
}

func TestStaleTaskReconcilerCapDrainAndRepeatIdempotence(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "owner", Name: "repo"}
	seedStaleRepo(t, store, repo)
	writeStaleTaskConfig(t, store, "1h")
	for i := 0; i < staleTaskReconcileLimit+5; i++ {
		id := fmt.Sprintf("task-%02d", i)
		if err := store.UpsertTask(ctx, db.Task{ID: id, RepoFullName: repo.FullName(), State: string(workflow.TaskImplementing), Branch: "feature/" + id}); err != nil {
			t.Fatal(err)
		}
	}
	remote := &fakeRemoteBranchChecker{present: map[string]struct{}{}}
	d := Daemon{Repo: repo, Store: store, RemoteBranches: remote, Now: futureClock}
	if err := d.reconcileStaleTasks(ctx, map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}
	assertDismissedCount(t, store, staleTaskReconcileLimit)
	if len(remote.calls) != 1 || len(remote.calls[0]) != staleTaskReconcileLimit {
		t.Fatalf("first remote batch = %v", remote.calls)
	}
	if err := d.reconcileStaleTasks(ctx, map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}
	assertDismissedCount(t, store, staleTaskReconcileLimit+5)
	if len(remote.calls) != 2 || len(remote.calls[1]) != 5 {
		t.Fatalf("remote batches = %v", remote.calls)
	}
	if err := d.reconcileStaleTasks(ctx, map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}
	if len(remote.calls) != 2 {
		t.Fatalf("repeat remote calls = %v", remote.calls)
	}
	events, _ := store.ListTaskEvents(ctx, "task-00")
	if len(events) != 1 {
		t.Fatalf("repeat events = %+v", events)
	}
}

func TestStaleTaskReconcilerDisabledAndInvalidConfig(t *testing.T) {
	for _, test := range []struct {
		name, value string
		wantLogs    int
	}{
		{name: "disabled", value: "0"},
		{name: "invalid", value: "bad", wantLogs: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			repo := github.Repository{Owner: "owner", Name: "repo"}
			seedStaleRepo(t, store, repo)
			writeStaleTaskConfig(t, store, test.value)
			if err := store.UpsertTask(context.Background(), db.Task{ID: "task-1", RepoFullName: repo.FullName(), State: string(workflow.TaskImplementing), Branch: "feature/one"}); err != nil {
				t.Fatal(err)
			}
			remote := &fakeRemoteBranchChecker{present: map[string]struct{}{}}
			logs := []string{}
			d := Daemon{Repo: repo, Store: store, RemoteBranches: remote, Now: futureClock, Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }}
			if err := d.reconcileStaleTasks(context.Background(), map[string]struct{}{}); err != nil {
				t.Fatal(err)
			}
			task, _ := store.GetTask(context.Background(), "task-1")
			if task.State != string(workflow.TaskImplementing) || len(remote.calls) != 0 || len(logs) != test.wantLogs {
				t.Fatalf("task=%+v calls=%v logs=%v", task, remote.calls, logs)
			}
		})
	}
}

func TestPollOnceRunsStaleTaskReconciler(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "owner", Name: "repo"}
	seedStaleRepo(t, store, repo)
	writeStaleTaskConfig(t, store, "1h")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: repo.FullName(), State: string(workflow.TaskImplementing)}); err != nil {
		t.Fatal(err)
	}
	client := &fakeGitHub{pullsByState: map[string][]github.PullRequest{"open": nil}, comments: map[int64][]github.IssueComment{}}
	d := Daemon{Repo: repo, Store: store, GitHub: client, Now: futureClock, RemoteBranches: &fakeRemoteBranchChecker{}}
	if err := d.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := store.GetTask(ctx, "task-1")
	if task.State != string(workflow.TaskDismissed) {
		t.Fatalf("PollOnce task state = %s", task.State)
	}
}

func seedStaleRepo(t *testing.T, store *db.Store, repo github.Repository) {
	t.Helper()
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: repo.Owner, Name: repo.Name, CheckoutPath: "/repo", PrimaryCheckoutPath: "/repo"}); err != nil {
		t.Fatal(err)
	}
}

func writeStaleTaskConfig(t *testing.T, store *db.Store, value string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(store.DatabasePath()), "config.toml")
	if err := os.WriteFile(path, []byte("[workflow]\nstale_task_ttl = \""+value+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func futureClock() time.Time { return time.Now().UTC().Add(365 * 24 * time.Hour) }

func assertDismissedCount(t *testing.T, store *db.Store, want int) {
	t.Helper()
	tasks, err := store.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, task := range tasks {
		if task.State == string(workflow.TaskDismissed) {
			got++
		}
	}
	if got != want {
		t.Fatalf("dismissed count = %d, want %d", got, want)
	}
}
