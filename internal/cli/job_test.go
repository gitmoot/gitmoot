package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestRunJobListShowEventsRetryCancel(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedCLIJob(t, store, db.Job{
		ID:      "job-failed",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobFailed),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 7, RawOutputs: []string{"raw"}}),
	}, "failed")
	seedCLIJob(t, store, db.Job{
		ID:      "job-queued",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 7}),
	}, "queued")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "list", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "job-failed") || !strings.Contains(stdout.String(), "job-queued") {
		t.Fatalf("job list output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "show", "job-failed", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"id: job-failed", "state: failed", "repo: owner/repo", "raw_outputs: 1 retained locally"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("job show missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "events", "job-failed", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job events exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "failed\tfailed") {
		t.Fatalf("job events output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "retry", "job-failed", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job retry exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "queued retry for job job-failed") {
		t.Fatalf("job retry output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "cancel", "job-queued", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job cancel exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cancelled job job-queued") {
		t.Fatalf("job cancel output = %q", stdout.String())
	}
}

func TestRunJobWatchPrintsEventsUntilTerminal(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedCLIJob(t, store, db.Job{
		ID:      "job-watch",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main"}),
	}, "succeeded")
	if err := store.AddJobEvent(context.Background(), db.JobEvent{JobID: "job-watch", Kind: "advance_completed", Message: "done"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "watch", "job-watch", "--home", home, "--poll", "1ms"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job watch exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"succeeded\tsucceeded", "advance_completed\tdone", "state: succeeded"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("job watch output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunJobWatchJSON(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedCLIJob(t, store, db.Job{
		ID:      "job-watch-json",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobFailed),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main"}),
	}, "failed")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "watch", "job-watch-json", "--home", home, "--json", "--poll", "1ms"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job watch --json exit code = %d, stderr=%s", code, stderr.String())
	}
	var decoded jobWatchOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json output did not decode: %v\n%s", err, stdout.String())
	}
	if decoded.Job.ID != "job-watch-json" || decoded.Job.State != string(workflow.JobFailed) || len(decoded.Events) != 1 || decoded.Events[0].Kind != string(workflow.JobFailed) {
		t.Fatalf("decoded watch output = %+v", decoded)
	}
}

func TestRunJobRunUsesDaemonWorkerInternals(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", "shell", `printf '%s\n' '{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`, []string{"ask"}, "owner/repo")
	seedCLIJob(t, store, db.Job{
		ID:      "job-run",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 0}),
	}, "queued")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "run", "job-run", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job run exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ran job job-run: succeeded") {
		t.Fatalf("job run output = %q", stdout.String())
	}
	job, err := store.GetJob(context.Background(), "job-run")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobSucceeded) || !strings.Contains(job.Payload, `"summary":"done"`) {
		t.Fatalf("job after run = %+v", job)
	}
}

func openCLIJobStore(t *testing.T, home string) *db.Store {
	t.Helper()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	return store
}

func seedCLIJob(t *testing.T, store *db.Store, job db.Job, message string) {
	t.Helper()
	if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{Kind: job.State, Message: message}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
}

func mustJobPayload(t *testing.T, payload workflow.JobPayload) string {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	return string(encoded)
}
