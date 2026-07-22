package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestTempWorkerEligibleAllowsWritableImplementWithTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worktree := t.TempDir()
	seedDaemonWorkerAgentWithPolicy(t, store, "lead", runtime.CodexRuntime, "session-1", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-a", RepoFullName: "owner/repo", State: string(workflow.TaskImplementing), Branch: "task-a", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: "task-a", TaskID: "task-a"})
	job, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}

	got := tempWorkerEligible(ctx, store, job, payload, runtime.Agent{Name: "lead", Runtime: runtime.CodexRuntime, AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite}, config.DefaultParallelSessionPolicy(), time.Now().UTC())

	if !got.Eligible {
		t.Fatalf("tempWorkerEligible = %+v, want eligible", got)
	}
}

func TestTempWorkerEligibleRejectsQueuePolicy(t *testing.T) {
	policy := config.DefaultParallelSessionPolicy()
	policy.SameSession = config.ParallelSessionQueue

	got := tempWorkerEligible(context.Background(), nil, db.Job{ID: "job-a", Type: "ask"}, workflow.JobPayload{}, runtime.Agent{Name: "lead", Runtime: runtime.CodexRuntime}, policy, time.Now().UTC())

	if got.Eligible || !strings.Contains(got.Reason, "same_session is queue") {
		t.Fatalf("tempWorkerEligible = %+v, want queue policy rejection", got)
	}
}

func TestTempWorkerEligibleRejectsAlreadyDelegatedJob(t *testing.T) {
	got := tempWorkerEligible(context.Background(), nil, db.Job{ID: "job-a", Type: "ask"}, workflow.JobPayload{DelegationReason: "runtime_session_busy"}, runtime.Agent{Name: "lead-temp-job-a", Runtime: runtime.CodexRuntime}, config.DefaultParallelSessionPolicy(), time.Now().UTC())

	if got.Eligible || !strings.Contains(got.Reason, "delegated temp worker waits") {
		t.Fatalf("tempWorkerEligible = %+v, want delegated job rejection", got)
	}
}

func TestJobWorkerParallelSessionPolicyUsesDefaultWhenHomeNotExplicit(t *testing.T) {
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)

	got, err := worker.parallelSessionPolicy()
	if err != nil {
		t.Fatalf("parallelSessionPolicy returned error: %v", err)
	}

	if got.SameSession != config.ParallelSessionForkTempSession {
		t.Fatalf("same_session = %q, want default fork_temp_session", got.SameSession)
	}
}

func TestJobWorkerParallelSessionPolicyLoadsExplicitHome(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	content := strings.Replace(config.DefaultConfig(paths), `same_session = "fork_temp_session"`, `same_session = "queue"`, 1)
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard, home)

	got, err := worker.parallelSessionPolicy()
	if err != nil {
		t.Fatalf("parallelSessionPolicy returned error: %v", err)
	}

	if got.SameSession != config.ParallelSessionQueue {
		t.Fatalf("same_session = %q, want explicit queue config", got.SameSession)
	}
}

func TestTempWorkerEligibleRejectsReadOnlyImplementation(t *testing.T) {
	got := tempWorkerEligible(context.Background(), nil, db.Job{ID: "job-a", Type: "implement"}, workflow.JobPayload{}, runtime.Agent{Name: "lead", Runtime: runtime.CodexRuntime, AutonomyPolicy: runtime.AutonomyPolicyReadOnly}, config.DefaultParallelSessionPolicy(), time.Now().UTC())

	if got.Eligible || !strings.Contains(got.Reason, "writable agent policy") {
		t.Fatalf("tempWorkerEligible = %+v, want read-only implementation rejection", got)
	}
}

func TestTempWorkerEligibleRejectsImplementationWithoutTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	got := tempWorkerEligible(ctx, store, db.Job{ID: "job-a", Type: "implement"}, workflow.JobPayload{Repo: "owner/repo", TaskID: "task-a"}, runtime.Agent{Name: "lead", Runtime: runtime.CodexRuntime, AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite}, config.DefaultParallelSessionPolicy(), time.Now().UTC())

	if got.Eligible || !strings.Contains(got.Reason, "task worktree") {
		t.Fatalf("tempWorkerEligible = %+v, want missing worktree rejection", got)
	}
}

func TestTempWorkerEligibleRejectsMaxTempWorkers(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		if err := store.UpsertAgentInstance(ctx, db.AgentInstance{
			Name:           fmt.Sprintf("lead-temp-%d", i),
			Type:           tempWorkerAgentType("lead"),
			Runtime:        runtime.CodexRuntime,
			RuntimeRef:     fmt.Sprintf("session-%d", i),
			RepoFullName:   "owner/repo",
			Role:           "worker",
			AutonomyPolicy: runtime.AutonomyPolicyAuto,
			State:          "idle",
			CreatedAt:      now.Format(time.RFC3339),
			LastUsedAt:     now.Format(time.RFC3339),
			ExpiresAt:      now.Add(time.Hour).Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("UpsertAgentInstance %d returned error: %v", i, err)
		}
	}

	got := tempWorkerEligible(ctx, store, db.Job{ID: "job-a", Type: "ask"}, workflow.JobPayload{}, runtime.Agent{Name: "lead", Runtime: runtime.CodexRuntime, AutonomyPolicy: runtime.AutonomyPolicyAuto}, config.DefaultParallelSessionPolicy(), now)

	if got.Eligible || !strings.Contains(got.Reason, "max temp workers reached") {
		t.Fatalf("tempWorkerEligible = %+v, want cap rejection", got)
	}
}

// TestPerJobAdmissionEstimate maps each session runtime to its configured RAM
// estimate (and marks it session-counted), and a non-session runtime (shell, or a
// session runtime with no ref) to 0 RAM AND not session-counted.
func TestPerJobAdmissionEstimate(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "codex-agent", runtime.CodexRuntime, "ref-1", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "claude-agent", runtime.ClaudeRuntime, "ref-2", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "kimi-agent", runtime.KimiRuntime, "ref-3", []string{"ask"}, "owner/repo")
	// A shell agent has no resumable runtime session key ⇒ contributes 0 and is not
	// session-counted (matches its exemption from the runtime session lock).
	seedDaemonWorkerAgent(t, store, "shell-agent", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	// A codex agent with an empty runtime ref also has no session key ⇒ 0 / not counted.
	seedDaemonWorkerAgent(t, store, "codex-no-ref", runtime.CodexRuntime, "", []string{"ask"}, "owner/repo")

	policy := config.AdmissionPolicy{CodexMemoryGB: 0.2, ClaudeMemoryGB: 0.85, KimiMemoryGB: 0.5, DefaultMemoryGB: 0.7}
	cases := []struct {
		agent       string
		wantMemGB   float64
		wantSession bool
	}{
		{"codex-agent", 0.2, true},
		{"claude-agent", 0.85, true},
		{"kimi-agent", 0.5, true},
		{"shell-agent", 0, false},
		{"codex-no-ref", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			job := db.Job{ID: "job-" + tc.agent, Agent: tc.agent}
			got := perJobAdmissionEstimate(ctx, store, job, policy)
			if got.memGB != tc.wantMemGB {
				t.Fatalf("perJobAdmissionEstimate(%s).memGB = %v, want %v", tc.agent, got.memGB, tc.wantMemGB)
			}
			if got.session != tc.wantSession {
				t.Fatalf("perJobAdmissionEstimate(%s).session = %v, want %v", tc.agent, got.session, tc.wantSession)
			}
		})
	}
}

func TestJobNeedsAdvanceRetryResetsAfterJobRetry(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-retry-reset", Agent: "audit", Type: "ask", State: string(workflow.JobFailed), Payload: `{"repo":"owner/repo"}`}, db.JobEvent{
		JobID:   "job-retry-reset",
		Kind:    string(workflow.JobFailed),
		Message: "failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-retry-reset", Kind: "advance_retry", Message: "old advance retry"}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	if _, err := workflow.RetryJob(ctx, store, "job-retry-reset"); err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard)
	needsRetry, err := worker.jobNeedsAdvanceRetry(ctx, "job-retry-reset")
	if err != nil {
		t.Fatalf("jobNeedsAdvanceRetry returned error: %v", err)
	}
	if needsRetry {
		t.Fatal("jobNeedsAdvanceRetry returned true after retry_queued reset")
	}
}

// TestJobWorkerResolveRepoSchedulerFallsBackToGlobal pins the resolver's
// fail-safe defaults (#576): an implicit config home, an empty repo filter, and
// an unmatched repo all return the global limit and the worker's UsePool
// unchanged, so the run path is byte-identical to today when the feature is off.
func TestJobWorkerResolveRepoSchedulerFallsBackToGlobal(t *testing.T) {
	// Implicit config home (no explicit --home) ⇒ no overrides possible.
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)
	worker.UsePool = true
	if limit, usePool := worker.resolveRepoScheduler("owner/repo-a", 3); limit != 3 || !usePool {
		t.Fatalf("implicit home: got (%d, %v), want (3, true)", limit, usePool)
	}
	// Explicit home with a section for a DIFFERENT repo ⇒ unmatched ⇒ global.
	configHome := writeRepoConcurrencyConfigHome(t, `
[repos."owner/repo-a"]
max_parallel = 1
scheduler = "barrier"
`)
	worker2 := defaultJobWorker(daemonWorkerStore(t), io.Discard, configHome)
	worker2.UsePool = true
	if limit, usePool := worker2.resolveRepoScheduler("owner/repo-b", 4); limit != 4 || !usePool {
		t.Fatalf("unmatched repo: got (%d, %v), want (4, true)", limit, usePool)
	}
	// Empty repo filter ⇒ global default (never reads config).
	if limit, usePool := worker2.resolveRepoScheduler("", 4); limit != 4 || !usePool {
		t.Fatalf("empty repo: got (%d, %v), want (4, true)", limit, usePool)
	}
	// Matched repo ⇒ capped limit and scheduler flip (pool -> barrier).
	if limit, usePool := worker2.resolveRepoScheduler("owner/repo-a", 4); limit != 1 || usePool {
		t.Fatalf("matched repo: got (%d, %v), want (1, false)", limit, usePool)
	}
}

func TestEffectiveJobTimeout(t *testing.T) {
	managed := managedJobRuntimeConfig{OK: true, JobTimeout: 10 * time.Minute}

	// A valid per-delegation timeout overrides the agent-type timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "30s"}, managed); got != 30*time.Second {
		t.Fatalf("effectiveJobTimeout(payload override) = %v, want 30s", got)
	}

	// An empty payload timeout falls back to the managed timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{}, managed); got != 10*time.Minute {
		t.Fatalf("effectiveJobTimeout(empty payload) = %v, want 10m", got)
	}

	// An unparseable payload timeout falls back to the managed timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "banana"}, managed); got != 10*time.Minute {
		t.Fatalf("effectiveJobTimeout(invalid payload) = %v, want 10m", got)
	}

	// A non-positive payload timeout falls back to the managed timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "0s"}, managed); got != 10*time.Minute {
		t.Fatalf("effectiveJobTimeout(zero payload) = %v, want 10m", got)
	}

	// With no managed config, an empty payload timeout falls back to the daemon
	// stale window so an unmanaged job still has a watchdog.
	if got := effectiveJobTimeout(workflow.JobPayload{}, managedJobRuntimeConfig{}); got != daemonRunningJobStaleAfter {
		t.Fatalf("effectiveJobTimeout(unmanaged, empty) = %v, want %v", got, daemonRunningJobStaleAfter)
	}

	// With no managed config, a valid payload timeout still applies.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "45s"}, managedJobRuntimeConfig{}); got != 45*time.Second {
		t.Fatalf("effectiveJobTimeout(unmanaged, payload) = %v, want 45s", got)
	}
}

func TestPermissionBlockedJobCannotResurrectDismissedTask(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", State: string(workflow.TaskDismissed), Branch: "feature/one"}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(workflow.JobPayload{TaskID: "task-1", Repo: "owner/repo", Branch: "feature/one"})
	if err != nil {
		t.Fatal(err)
	}
	err = blockTaskForPermissionBlockedJob(ctx, store, db.Job{ID: "job-1", Payload: string(payload)})
	if err == nil || !strings.Contains(err.Error(), "dismissed") {
		t.Fatalf("blockTaskForPermissionBlockedJob error = %v", err)
	}
	task, _ := store.GetTask(ctx, "task-1")
	if task.State != string(workflow.TaskDismissed) {
		t.Fatalf("task resurrected to %s", task.State)
	}
}
