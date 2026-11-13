package presence

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
)

func TestBuildSnapshotCountsRepoLocalState(t *testing.T) {
	paths := testPaths(t)
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	for _, task := range []db.Task{
		{ID: "task-1", RepoFullName: "owner/repo", State: "planned"},
		{ID: "task-2", RepoFullName: "owner/repo", State: "blocked"},
		{ID: "task-3", RepoFullName: "other/repo", State: "planned"},
	} {
		if err := store.UpsertTask(context.Background(), task); err != nil {
			t.Fatalf("upsert task: %v", err)
		}
	}
	for _, job := range []db.Job{
		{ID: "job-1", State: "succeeded", Payload: jobPayload(t, "owner/repo")},
		{ID: "job-2", State: "blocked", Payload: jobPayload(t, "owner/repo")},
		{ID: "job-3", State: "failed", Payload: jobPayload(t, "other/repo")},
		{ID: "job-4", State: "failed", Payload: "{not json"},
	} {
		if err := store.CreateJob(context.Background(), job); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}
	if _, err := store.AcquireLock(context.Background(), db.BranchLock{RepoFullName: "owner/repo", Branch: "task/a", Owner: "agent-a"}); err != nil {
		t.Fatalf("acquire lock: %v", err)
	}

	snapshot, err := BuildSnapshot(context.Background(), paths, "owner/repo")
	if err != nil {
		t.Fatalf("BuildSnapshot returned error: %v", err)
	}
	if snapshot.Tasks != 2 {
		t.Fatalf("tasks = %d, want 2", snapshot.Tasks)
	}
	if snapshot.TaskStates["planned"] != 1 || snapshot.TaskStates["blocked"] != 1 {
		t.Fatalf("task states = %#v", snapshot.TaskStates)
	}
	if snapshot.Jobs != 2 {
		t.Fatalf("jobs = %d, want 2", snapshot.Jobs)
	}
	if snapshot.JobStates["succeeded"] != 1 || snapshot.JobStates["blocked"] != 1 {
		t.Fatalf("job states = %#v", snapshot.JobStates)
	}
	if len(snapshot.Locks) != 1 || snapshot.Locks[0].Branch != "task/a" || snapshot.Locks[0].Owner != "agent-a" {
		t.Fatalf("locks = %#v", snapshot.Locks)
	}
}

func TestInspectDaemonStoppedWithoutPIDFile(t *testing.T) {
	paths := testPaths(t)

	snapshot := InspectDaemon(paths)

	if snapshot.State != DaemonStopped {
		t.Fatalf("daemon state = %q, want stopped", snapshot.State)
	}
	if snapshot.PID != 0 {
		t.Fatalf("daemon pid = %d, want 0", snapshot.PID)
	}
}

func TestInspectDaemonDoesNotRemoveStalePIDFile(t *testing.T) {
	paths := testPaths(t)
	pidPath := filepath.Join(paths.Home, "daemon.pid")
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	snapshot := InspectDaemon(paths)

	if runtime.GOOS == "windows" {
		if snapshot.State != DaemonUnknown {
			t.Fatalf("daemon state = %q, want unknown on windows", snapshot.State)
		}
	} else if snapshot.State != DaemonStopped {
		t.Fatalf("daemon state = %q, want stopped", snapshot.State)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("pid file was removed or changed: %v", err)
	}
}

func TestInspectDaemonTreatsLiveNonDaemonPIDAsStale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows probe is conservative")
	}
	paths := testPaths(t)
	pidPath := filepath.Join(paths.Home, "daemon.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	snapshot := InspectDaemon(paths)

	if snapshot.State != DaemonStopped {
		t.Fatalf("daemon state = %q, want stopped for non-daemon pid", snapshot.State)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("pid file was removed or changed: %v", err)
	}
}

func TestFormatSnapshotQuotesLockMetadata(t *testing.T) {
	text := FormatSnapshot(Snapshot{
		Daemon:     DaemonSnapshot{State: DaemonRunning, PID: 42},
		Tasks:      2,
		TaskStates: map[string]int{"blocked": 1, "planned": 1},
		Jobs:       1,
		JobStates:  map[string]int{"succeeded": 1},
		Locks: []LockSnapshot{
			{Branch: "task/a\n- injected", Owner: "agent\n- injected"},
		},
	})

	for _, want := range []string{
		"Current snapshot",
		"- daemon: running (pid 42)",
		"- tasks: 2 (blocked: 1, planned: 1)",
		"- jobs: 1 (succeeded: 1)",
		"- locks: 1",
		strconv.Quote("task/a\n- injected") + " by " + strconv.Quote("agent\n- injected"),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted snapshot missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\n- injected") {
		t.Fatalf("formatted snapshot contains raw injection:\n%s", text)
	}
}

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".gitmoot")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create home: %v", err)
	}
	paths := config.Paths{
		Home:     home,
		Database: filepath.Join(home, config.DBName),
		Logs:     filepath.Join(home, config.LogsDir),
	}
	if err := os.MkdirAll(paths.Logs, 0o700); err != nil {
		t.Fatalf("create logs: %v", err)
	}
	return paths
}

func jobPayload(t *testing.T, repo string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"repo":         repo,
		"sender":       "tester",
		"instructions": fmt.Sprintf("work on %s", repo),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(payload)
}
