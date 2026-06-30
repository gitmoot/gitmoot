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

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	gmruntime "github.com/jerryfane/gitmoot/internal/runtime"
)

// TestClaudeCredEnvFromLookupNeutralizesAbsentVars is the #486 credential-isolation
// regression: the daemon-token probe env must carry the daemon's set credential
// AND an empty NAME= for every Claude credential the daemon does NOT set, so when
// the entries are appended onto the doctor process's own environment a competing
// doctor-shell credential (e.g. a valid ANTHROPIC_API_KEY the daemon lacks, which
// outranks OAuth) is neutralized rather than leaking in and validating the wrong
// credential. Without the empty-neutralization this asserts fails: a missing var
// would be omitted, letting the doctor's value survive.
func TestClaudeCredEnvFromLookupNeutralizesAbsentVars(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == gmruntime.ClaudeOAuthTokenEnv {
			return "daemon-secret", true
		}
		return "", false
	}
	env, ok := claudeCredEnvFromLookup(lookup)
	if !ok {
		t.Fatal("claudeCredEnvFromLookup ok=false for a daemon with an OAuth token")
	}
	got := make(map[string]string, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		got[k] = v
	}
	if got[gmruntime.ClaudeOAuthTokenEnv] != "daemon-secret" {
		t.Fatalf("OAuth token = %q, want the daemon's value", got[gmruntime.ClaudeOAuthTokenEnv])
	}
	for _, name := range []string{gmruntime.AnthropicAPIKeyEnv, gmruntime.AnthropicAuthTokenEnv} {
		v, present := got[name]
		if !present {
			t.Fatalf("%s missing from probe env; an absent daemon var must be neutralized with NAME=, not omitted (it would let the doctor's value leak in)", name)
		}
		if v != "" {
			t.Fatalf("%s = %q, want empty (neutralized)", name, v)
		}
	}
}

// TestClaudeCredEnvFromLookupNoCredentialSkips guards the fail-open contract: a
// daemon with no Claude credential at all yields (nil, false) so the caller falls
// back to the presence-only report instead of probing an all-empty env.
func TestClaudeCredEnvFromLookupNoCredentialSkips(t *testing.T) {
	if env, ok := claudeCredEnvFromLookup(func(string) (string, bool) { return "", false }); ok || env != nil {
		t.Fatalf("claudeCredEnvFromLookup = (%v, %t), want (nil, false) for a credential-less daemon", env, ok)
	}
	if env, ok := claudeCredEnvFromLookup(nil); ok || env != nil {
		t.Fatalf("claudeCredEnvFromLookup(nil) = (%v, %t), want (nil, false)", env, ok)
	}
}

func TestBuildSnapshotCountsRepoLocalState(t *testing.T) {
	paths := testPaths(t)
	store, err := db.Open(paths.Database)
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

// TestInspectDaemonClaudeAuthFailsOpenWithoutDaemon guards the #427 fail-open
// contract: with no running daemon, the auth snapshot reports neither running nor
// detected so doctor/dashboard/daemon-status fall back to the shell-local check
// instead of implying the daemon is unauthenticated.
func TestInspectDaemonClaudeAuthFailsOpenWithoutDaemon(t *testing.T) {
	paths := testPaths(t)

	snapshot := InspectDaemonClaudeAuth(paths)

	if snapshot.Running {
		t.Fatalf("daemon auth Running = true, want false without a daemon")
	}
	if snapshot.Detected {
		t.Fatalf("daemon auth Detected = true, want false without a readable daemon env")
	}
	if snapshot.Auth.Ready() {
		t.Fatalf("daemon auth Ready = true, want false when env is undetected")
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
