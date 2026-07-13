package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/cockpit"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
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

func TestRunJobListSurfacesDelegationPreflightFailure(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	// A coordinator whose delegation fan-out could not be routed. Its job state is
	// not "blocked" (it took a corrective continuation), so the reason must surface
	// from the delegation_preflight_failed event, not the job state.
	seedCLIJob(t, store, db.Job{
		ID:      "coordinator-job",
		Agent:   "coordinator",
		Type:    "ask",
		State:   string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main"}),
	}, "succeeded")
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   "coordinator-job",
		Kind:    "delegation_preflight_failed",
		Message: `delegation "impl": "claude" is a runtime, not a registered agent`,
	}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	// A continuation event arrives later (becomes the latest overall event), so a
	// latest-event-only surface would miss the preflight reason.
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   "coordinator-job",
		Kind:    "delegation_continuation_enqueued",
		Message: "preflight corrective continuation",
	}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	// An ordinary job with no preflight failure must print unchanged.
	seedCLIJob(t, store, db.Job{
		ID:      "plain-job",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 3}),
	}, "queued")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "list", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job list exit code = %d, stderr=%s", code, stderr.String())
	}
	var coordinatorLine, plainLine string
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if strings.HasPrefix(line, "coordinator-job\t") {
			coordinatorLine = line
		}
		if strings.HasPrefix(line, "plain-job\t") {
			plainLine = line
		}
	}
	if coordinatorLine == "" || plainLine == "" {
		t.Fatalf("job list output = %q", stdout.String())
	}
	if !strings.Contains(coordinatorLine, "PREFLIGHT_FAILED: ") || !strings.Contains(coordinatorLine, "is a runtime, not a registered agent") {
		t.Fatalf("coordinator line missing the preflight reason column: %q", coordinatorLine)
	}
	// The existing six columns stay byte-stable: a plain job has exactly six.
	if got := strings.Count(plainLine, "\t"); got != 5 {
		t.Fatalf("plain job line changed shape (%d tabs, want 5): %q", got, plainLine)
	}
	if strings.Contains(plainLine, "PREFLIGHT_FAILED") {
		t.Fatalf("a job without a preflight failure must not print the column: %q", plainLine)
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

// TestPinRunJobWatchStopsOnBlocked pins `job watch`'s terminal semantics (#632):
// a `blocked` job is treated as SETTLED, so the watch loop stops tailing it and
// returns exit 0 immediately rather than polling forever. If blocked were not
// terminal here, this test would hang. This "settled includes blocked" behavior
// must hold identically before and after the predicate refactor.
func TestPinRunJobWatchStopsOnBlocked(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedCLIJob(t, store, db.Job{
		ID:      "job-watch-blocked",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobBlocked),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main"}),
	}, "blocked")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "watch", "job-watch-blocked", "--home", home, "--poll", "1ms"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job watch on blocked job exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state: blocked") {
		t.Fatalf("job watch on blocked job did not report terminal state:\n%s", stdout.String())
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

func TestRunJobWatchTranscriptRejectsJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "watch", "job", "--transcript", "--json"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--transcript is incompatible with --json") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunJobWatchTranscriptFallsBackWhenDerivedLogMissing(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedCLIJob(t, store, db.Job{
		ID:      "job-no-transcript",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
	}, "succeeded")
	store.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "watch", "job-no-transcript", "--home", home, "--transcript", "--poll", "1ms"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"transcript unavailable; showing job events", "succeeded\tsucceeded", "state: succeeded"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("fallback output missing %q: %q", want, stdout.String())
		}
	}
}

func TestRunJobWatchTranscriptRendersExplicitShellLog(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedCLIJob(t, store, db.Job{
		ID:      "job-transcript",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
	}, "succeeded")
	store.Close()
	logPath := filepath.Join(home, "cockpit shell.log")
	if err := os.WriteFile(logPath, []byte("working\ndone without newline"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "watch", "job-transcript", "--home", home, "--transcript", "--log-path", logPath, "--runtime", runtime.ShellRuntime, "--poll", "1ms"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "\u25b6 ask \u00b7 audit \u00b7 shell\n\nworking\ndone without newline\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestResolveTranscriptRuntimeOrder(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertAgent(context.Background(), db.Agent{Name: "audit", Runtime: runtime.ClaudeRuntime}); err != nil {
		t.Fatal(err)
	}
	job := db.Job{ID: "runtime-order", Agent: "audit"}
	payload := workflow.JobPayload{RuntimeOverride: runtime.CodexRuntime}
	got, err := resolveTranscriptRuntime(context.Background(), store, job, payload, "")
	if err != nil || got != runtime.CodexRuntime {
		t.Fatalf("payload override runtime = %q, err=%v", got, err)
	}
	got, err = resolveTranscriptRuntime(context.Background(), store, job, payload, runtime.KimiRuntime)
	if err != nil || got != runtime.KimiRuntime {
		t.Fatalf("explicit runtime = %q, err=%v", got, err)
	}
	got, err = resolveTranscriptRuntime(context.Background(), store, job, workflow.JobPayload{}, "")
	if err != nil || got != runtime.ClaudeRuntime {
		t.Fatalf("agent runtime = %q, err=%v", got, err)
	}
	ephJob := db.Job{ID: "ephemeral-runtime", Agent: "unregistered-ephemeral"}
	ephPayload := workflow.JobPayload{Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.KimiRuntime}}
	got, err = resolveTranscriptRuntime(context.Background(), store, ephJob, ephPayload, "")
	if err != nil || got != runtime.KimiRuntime {
		t.Fatalf("ephemeral runtime = %q, err=%v", got, err)
	}
}

func TestJobWatchTranscriptShellNoLLME2E(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "absent-herdr.sock"))
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertAgent(context.Background(), db.Agent{Name: "shell-seat", Runtime: runtime.ShellRuntime}); err != nil {
		t.Fatal(err)
	}
	payload := workflow.JobPayload{Repo: "owner/repo", RawOutputs: []string{"sentinel-result"}}
	seedCLIJob(t, store, db.Job{
		ID:      "job-shell-e2e",
		Agent:   "shell-seat",
		Type:    "ask",
		State:   string(workflow.JobRunning),
		Payload: mustJobPayload(t, payload),
	}, "running")
	logPath := filepath.Join(config.PathsForHome(home).Logs, "jobs", cockpit.SafeLogName("job-shell-e2e")+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("shell started\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	writerDone := make(chan error, 1)
	go func() {
		time.Sleep(5 * time.Millisecond)
		file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, err = file.WriteString("{corrupted runtime bytes\nshell done")
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
		if err == nil {
			err = store.UpdateJobState(context.Background(), "job-shell-e2e", string(workflow.JobSucceeded))
		}
		writerDone <- err
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "watch", "job-shell-e2e", "--home", home, "--transcript", "--poll", "1ms"}, &stdout, &stderr)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"shell started", "{corrupted runtime bytes", "shell done"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("live transcript missing %q: %q", want, stdout.String())
		}
	}
	job, err := store.GetJob(context.Background(), "job-shell-e2e")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := daemonJobPayload(job)
	if err != nil || len(decoded.RawOutputs) != 1 || decoded.RawOutputs[0] != "sentinel-result" {
		t.Fatalf("job result payload changed: payload=%+v err=%v", decoded, err)
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

// TestRunJobKill covers the #341 operator kill switch CLI: `job kill <root>`
// marks the root as killed and reports it, and errors on a missing root id.
func TestRunJobKill(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedCLIJob(t, store, db.Job{
		ID:      "root-job",
		Agent:   "coord",
		Type:    "ask",
		State:   string(workflow.JobRunning),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main"}),
	}, "running")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "kill", "root-job", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job kill exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "killed delegation tree rooted at root-job") {
		t.Fatalf("job kill output = %q", stdout.String())
	}
	killed, err := store.IsRootJobKilled(context.Background(), "root-job")
	if err != nil {
		t.Fatalf("IsRootJobKilled returned error: %v", err)
	}
	if !killed {
		t.Fatal("job kill should mark the root as killed")
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "kill", "no-such-root", "--home", home}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("job kill on missing root exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "job kill:") {
		t.Fatalf("job kill missing-root stderr = %q", stderr.String())
	}
}

// seedJobCancelBulkFixtures seeds the standard #631 bulk-cancel fixture set: a
// spread of blocked jobs (assorted ages, repos, agents) plus queued/running/
// failed decoys that must never be selected. Ages are stamped onto updated_at so
// the age filter has deterministic values. The seeding store is closed so the
// later setJobTimes (and the CLI commands) own the file.
func seedJobCancelBulkFixtures(t *testing.T, home string) {
	t.Helper()
	store := openCLIJobStore(t, home)
	now := time.Now().UTC()
	const layout = "2006-01-02 15:04:05"
	fixtures := []struct {
		id, state, repo, agent string
		age                    time.Duration
	}{
		{"blocked-old-a", string(workflow.JobBlocked), "owner/repo", "audit", 10 * 24 * time.Hour},
		{"blocked-old-b", string(workflow.JobBlocked), "owner/repo", "builder", 30 * 24 * time.Hour},
		{"blocked-old-c", string(workflow.JobBlocked), "other/repo", "audit", 12 * 24 * time.Hour},
		{"blocked-recent", string(workflow.JobBlocked), "owner/repo", "audit", time.Hour},
		{"queued-old", string(workflow.JobQueued), "owner/repo", "audit", 20 * 24 * time.Hour},
		{"running-old", string(workflow.JobRunning), "owner/repo", "audit", 20 * 24 * time.Hour},
		{"failed-old", string(workflow.JobFailed), "owner/repo", "audit", 20 * 24 * time.Hour},
	}
	for _, f := range fixtures {
		seedCLIJob(t, store, db.Job{
			ID:      f.id,
			Agent:   f.agent,
			Type:    "ask",
			State:   f.state,
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: f.repo, Branch: "main"}),
		}, f.state)
	}
	store.Close()
	for _, f := range fixtures {
		ts := now.Add(-f.age).Format(layout)
		setJobTimes(t, home, f.id, ts, ts)
	}
}

func jobStateForTest(t *testing.T, home, id string) string {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	job, err := store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", id, err)
	}
	return job.State
}

func TestRunJobCancelBulkDryRunSelectsBlockedAndOlder(t *testing.T) {
	home := t.TempDir()
	seedJobCancelBulkFixtures(t, home)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "cancel", "--home", home, "--state", "blocked", "--older-than", "7d"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("bulk dry-run exit code = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"blocked-old-a", "blocked-old-b", "blocked-old-c", "run again with --yes to cancel 3 jobs"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{"blocked-recent", "queued-old", "running-old", "failed-old"} {
		if strings.Contains(out, absent) {
			t.Fatalf("dry-run selected a job it should not have (%q):\n%s", absent, out)
		}
	}
	// Oldest first with an id tie-break: b (30d) before c (12d) before a (10d).
	ib, ic, ia := strings.Index(out, "blocked-old-b"), strings.Index(out, "blocked-old-c"), strings.Index(out, "blocked-old-a")
	if !(ib < ic && ic < ia) {
		t.Fatalf("dry-run order not oldest-first (b=%d c=%d a=%d):\n%s", ib, ic, ia, out)
	}
	// A dry-run must not mutate anything.
	for _, id := range []string{"blocked-old-a", "blocked-old-b", "blocked-old-c", "blocked-recent"} {
		if got := jobStateForTest(t, home, id); got != string(workflow.JobBlocked) {
			t.Fatalf("dry-run mutated %s to %q, want still blocked", id, got)
		}
	}
}

func TestRunJobCancelBulkYesCancelsSelection(t *testing.T) {
	home := t.TempDir()
	seedJobCancelBulkFixtures(t, home)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "cancel", "--home", home, "--state", "blocked", "--older-than", "7d", "--yes"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("bulk --yes exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cancelled 3 of 3") {
		t.Fatalf("bulk --yes summary missing:\n%s", stdout.String())
	}
	for _, id := range []string{"blocked-old-a", "blocked-old-b", "blocked-old-c"} {
		if got := jobStateForTest(t, home, id); got != string(workflow.JobCancelled) {
			t.Fatalf("job %s state = %q, want cancelled", id, got)
		}
	}
	// The too-new blocked job and the non-blocked decoys are untouched.
	if got := jobStateForTest(t, home, "blocked-recent"); got != string(workflow.JobBlocked) {
		t.Fatalf("blocked-recent state = %q, want still blocked", got)
	}
	for id, want := range map[string]string{
		"queued-old":  string(workflow.JobQueued),
		"running-old": string(workflow.JobRunning),
		"failed-old":  string(workflow.JobFailed),
	} {
		if got := jobStateForTest(t, home, id); got != want {
			t.Fatalf("decoy %s state = %q, want %q", id, got, want)
		}
	}
}

func TestRunJobCancelBulkFiltersCompose(t *testing.T) {
	home := t.TempDir()
	seedJobCancelBulkFixtures(t, home)

	// --older-than in Go-duration form (168h == 7d) composed with repo + agent:
	// only blocked-old-a is owner/repo + audit + old enough.
	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "cancel", "--home", home, "--state", "blocked", "--older-than", "168h", "--repo", "owner/repo", "--agent", "audit"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("bulk compose exit code = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "blocked-old-a") || !strings.Contains(out, "run again with --yes to cancel 1 jobs") {
		t.Fatalf("compose selection = %q, want only blocked-old-a", out)
	}
	for _, absent := range []string{"blocked-old-b", "blocked-old-c", "blocked-recent"} {
		if strings.Contains(out, absent) {
			t.Fatalf("compose selected %q it should have filtered out:\n%s", absent, out)
		}
	}
}

func TestRunJobCancelBulkStateValidation(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "cancel", "--home", home, "--state", "failed"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("bulk --state failed exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `only "blocked"`) {
		t.Fatalf("bulk --state failed stderr = %q, want a blocked-only message", stderr.String())
	}
}

func TestRunJobCancelIDStateMutuallyExclusive(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "cancel", "some-id", "--home", home, "--state", "blocked"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("id + --state exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("id + --state stderr = %q, want a mutual-exclusion message", stderr.String())
	}
}

func TestRunJobCancelBulkFiltersRequireState(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "cancel", "--home", home, "--older-than", "7d"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("--older-than without --state exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "require --state") {
		t.Fatalf("--older-than without --state stderr = %q, want a require-state message", stderr.String())
	}
}

func TestParseOlderThanDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"168h", 168 * time.Hour, false},
		{"0", 0, false},
		{"90m", 90 * time.Minute, false},
		{"-3d", 0, true},
		{"-1h", 0, true},
		{"bogus", 0, true},
		{"5x", 0, true},
	}
	for _, tc := range cases {
		got, err := parseOlderThanDuration(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("parseOlderThanDuration(%q) err = nil, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseOlderThanDuration(%q) returned error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("parseOlderThanDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
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

func TestPrintJobRendersFailureDiagnosticsBlock(t *testing.T) {
	var buf bytes.Buffer
	exitCode := 7
	payload := workflow.JobPayload{
		Repo: "owner/repo",
		FailureDiagnostics: &workflow.FailureDiagnostics{
			Phase:      workflow.FailurePhaseLaunched,
			ExitCode:   &exitCode,
			StderrTail: "first crash line\nsecond crash line",
			SessionID:  "sess-1234",
		},
	}

	printJob(&buf, db.Job{ID: "job-x", State: "failed", Type: "review", Agent: "audit"}, payload, stuckReason{})

	out := buf.String()
	for _, want := range []string{
		"failure_diagnostics:",
		"  phase: launched",
		"  exit_code: 7",
		"  runtime_session: sess-1234",
		"  stderr_tail:",
		"    first crash line",
		"    second crash line",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("job show output missing %q:\n%s", want, out)
		}
	}
}

func TestPrintJobOmitsFailureDiagnosticsWhenAbsent(t *testing.T) {
	var buf bytes.Buffer

	printJob(&buf, db.Job{ID: "job-y", State: "succeeded", Type: "review", Agent: "audit"}, workflow.JobPayload{Repo: "owner/repo"}, stuckReason{})

	if strings.Contains(buf.String(), "failure_diagnostics:") {
		t.Fatalf("job show output has a failure diagnostics block for a healthy job:\n%s", buf.String())
	}
}

func TestTranscriptStyleTerminalStaysPlainOffTTY(t *testing.T) {
	if transcriptStyleTerminal(&bytes.Buffer{}) {
		t.Fatal("buffer writer must not be styled")
	}
	f, err := os.CreateTemp(t.TempDir(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if transcriptStyleTerminal(f) {
		t.Fatal("regular file must not be styled")
	}
	t.Setenv("NO_COLOR", "1")
	if transcriptStyleTerminal(os.Stdout) {
		t.Fatal("NO_COLOR must force plain output")
	}
}

func TestTranscriptHeaderModelResolution(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertAgent(context.Background(), db.Agent{Name: "modeled", Runtime: runtime.CodexRuntime, Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	job := db.Job{ID: "hdr", Type: "ask", Agent: "modeled", WorkflowID: "wf"}
	h := transcriptHeader(context.Background(), store, job, workflow.JobPayload{Model: "gpt-5.4", Instructions: "prompt text"}, runtime.CodexRuntime)
	if h.Model != "gpt-5.4" || h.Action != "ask" || h.Workflow != "wf" || h.Prompt != "prompt text" {
		t.Fatalf("override header = %+v", h)
	}
	h = transcriptHeader(context.Background(), store, job, workflow.JobPayload{}, runtime.CodexRuntime)
	if h.Model != "gpt-5.5" {
		t.Fatalf("registry model = %q, want gpt-5.5", h.Model)
	}
	eph := db.Job{ID: "hdr2", Type: "ask", Agent: "no-row"}
	h = transcriptHeader(context.Background(), store, eph, workflow.JobPayload{}, runtime.KimiRuntime)
	if h.Model != "" {
		t.Fatalf("ephemeral model = %q, want empty", h.Model)
	}
}
