package cli

import (
	"context"
	"encoding/json"
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

type timeoutCaptureAdapter struct {
	deadline    time.Time
	hasDeadline bool
}

func (a *timeoutCaptureAdapter) Deliver(ctx context.Context, _ runtime.Agent, _ runtime.Job) (runtime.Result, error) {
	a.deadline, a.hasDeadline = ctx.Deadline()
	return runtime.Result{Raw: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}`}, nil
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

func TestJobNeedsAdvanceRetryStopsAtReviewLoopDetected(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.CreateJob(ctx, db.Job{ID: "job-review-loop", Agent: "audit", Type: "review", State: string(workflow.JobSucceeded), Payload: `{"repo":"owner/repo"}`}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for _, kind := range []string{"advance_retry", workflow.ReviewLoopDetectedEventKind} {
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-review-loop", Kind: kind, Message: kind}); err != nil {
			t.Fatalf("AddJobEvent(%s): %v", kind, err)
		}
	}
	needsRetry, err := defaultJobWorker(store, io.Discard).jobNeedsAdvanceRetry(ctx, "job-review-loop")
	if err != nil {
		t.Fatalf("jobNeedsAdvanceRetry: %v", err)
	}
	if needsRetry {
		t.Fatal("jobNeedsAdvanceRetry returned true after review_loop_detected")
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
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "30s"}, managed, "ask"); got != 30*time.Second {
		t.Fatalf("effectiveJobTimeout(payload override) = %v, want 30s", got)
	}

	// An empty payload timeout falls back to the managed timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{}, managed, "ask"); got != 10*time.Minute {
		t.Fatalf("effectiveJobTimeout(empty payload) = %v, want 10m", got)
	}

	// An unparseable payload timeout falls back to the managed timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "banana"}, managed, "ask"); got != 10*time.Minute {
		t.Fatalf("effectiveJobTimeout(invalid payload) = %v, want 10m", got)
	}

	// A non-positive payload timeout falls back to the managed timeout.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "0s"}, managed, "ask"); got != 10*time.Minute {
		t.Fatalf("effectiveJobTimeout(zero payload) = %v, want 10m", got)
	}

	// With no agent-type config, the independent daemon kill default applies;
	// stale-running detection is never reused as the deadline.
	if got := effectiveJobTimeout(workflow.JobPayload{}, managedJobRuntimeConfig{}, "ask"); got != config.DefaultDaemonJobTimeoutDefault {
		t.Fatalf("effectiveJobTimeout(unmanaged, empty) = %v, want %v", got, config.DefaultDaemonJobTimeoutDefault)
	}

	// With no managed config, a valid payload timeout still applies.
	if got := effectiveJobTimeout(workflow.JobPayload{JobTimeout: "45s"}, managedJobRuntimeConfig{}, "ask"); got != 45*time.Second {
		t.Fatalf("effectiveJobTimeout(unmanaged, payload) = %v, want 45s", got)
	}

	// Configured daemon authority replaces both defaults, and its maximum is a
	// hard ceiling regardless of which higher-precedence source supplied the
	// candidate duration.
	authority := managedJobRuntimeConfig{JobTimeoutDefault: 3 * time.Hour, JobTimeoutMax: 6 * time.Hour}
	if got := effectiveJobTimeout(workflow.JobPayload{}, authority, "ask"); got != 3*time.Hour {
		t.Fatalf("effectiveJobTimeout(configured default) = %v, want 3h", got)
	}
	resolution := resolveEffectiveJobTimeout(workflow.JobPayload{JobTimeout: "7h"}, authority, "ask")
	if resolution.Timeout != 6*time.Hour || !resolution.Clamped || resolution.Source != "payload" {
		t.Fatalf("resolveEffectiveJobTimeout(configured max) = %+v, want payload clamped to 6h", resolution)
	}
}

func TestDaemonDispatchUsesRegisteredAgentTypeTimeoutWithoutInstance(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveAgentType(paths, config.AgentType{Name: "plain", Runtime: runtime.ShellRuntime, JobTimeout: "10m"}); err != nil {
		t.Fatal(err)
	}
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "plain", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-plain-timeout", Agent: "plain", Action: "ask", Repo: "owner/repo", Branch: "main"})
	job, err := store.GetJob(ctx, "job-plain-timeout")
	if err != nil {
		t.Fatal(err)
	}
	capture := &timeoutCaptureAdapter{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return capture, nil }
	started := time.Now()
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	assertCapturedTimeout(t, capture, started, 10*time.Minute)
}

func TestDaemonDispatchPlainAgentWithoutTypeConfigUsesDefaultTimeout(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir() // Intentionally has no config file or [agents.plain] block.
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "plain", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-plain-default-timeout", Agent: "plain", Action: "ask", Repo: "owner/repo", Branch: "main"})
	job, err := store.GetJob(ctx, "job-plain-default-timeout")
	if err != nil {
		t.Fatal(err)
	}
	capture := &timeoutCaptureAdapter{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return capture, nil }
	started := time.Now()
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobSucceeded) {
		t.Fatalf("plain agent job state = %q, want succeeded with daemon default timeout", stored.State)
	}
	assertCapturedTimeout(t, capture, started, config.DefaultDaemonJobTimeoutDefault)
}

func TestManagedJobConfigLoadsDaemonTimeoutAuthority(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[daemon]\njob_timeout_default = \"3h\"\njob_timeout_max = \"6h\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "plain", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	got, err := (jobWorker{Store: store, ConfigHome: home, ConfigHomeExplicit: true}).managedJobConfig(ctx, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if got.JobTimeoutDefault != 3*time.Hour || got.JobTimeoutMax != 6*time.Hour {
		t.Fatalf("managed job timeout authority = (%v, %v), want (3h, 6h)", got.JobTimeoutDefault, got.JobTimeoutMax)
	}
}

func TestDaemonDispatchClampsPayloadTimeoutAndRecordsEvent(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "plain", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-clamped-timeout", Agent: "plain", Action: "ask", Repo: "owner/repo", Branch: "main", JobTimeout: "12h"})
	job, err := store.GetJob(ctx, "job-clamped-timeout")
	if err != nil {
		t.Fatal(err)
	}
	capture := &timeoutCaptureAdapter{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return capture, nil }
	started := time.Now()
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	assertCapturedTimeout(t, capture, started, 8*time.Hour)
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var clampMessage string
	for _, event := range events {
		if event.Kind == "job_timeout_clamped" {
			clampMessage = event.Message
		}
	}
	if !strings.Contains(clampMessage, "12h") || !strings.Contains(clampMessage, "8h") {
		t.Fatalf("job_timeout_clamped event = %q, want requested 12h and max 8h", clampMessage)
	}
}

func TestDaemonJobTimeoutIsSeparateFromStaleDetection(t *testing.T) {
	if !(daemonRunningJobStaleAfter < config.DefaultDaemonJobTimeoutDefault) {
		t.Fatalf("stale_after = %v, kill default = %v; want stale_after < kill default", daemonRunningJobStaleAfter, config.DefaultDaemonJobTimeoutDefault)
	}
	if got := effectiveJobTimeout(workflow.JobPayload{}, managedJobRuntimeConfig{}, "ask"); got == daemonRunningJobStaleAfter {
		t.Fatalf("effectiveJobTimeout returned stale detector constant %v", got)
	}

	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "defaulted", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-default-timeout", Agent: "defaulted", Action: "ask", Repo: "owner/repo", Branch: "main"})
	job, err := store.GetJob(ctx, "job-default-timeout")
	if err != nil {
		t.Fatal(err)
	}
	capture := &timeoutCaptureAdapter{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return capture, nil }
	started := time.Now()
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	assertCapturedTimeout(t, capture, started, config.DefaultDaemonJobTimeoutDefault)
}

func assertCapturedTimeout(t *testing.T, capture *timeoutCaptureAdapter, started time.Time, want time.Duration) {
	t.Helper()
	if !capture.hasDeadline {
		t.Fatal("delivery context has no deadline")
	}
	got := capture.deadline.Sub(started)
	if got < want-time.Second || got > want+time.Second {
		t.Fatalf("delivery timeout = %v, want %v", got, want)
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

func TestPermissionBlockedJobWritesCurrentTaskOwnership(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-2", RepoFullName: "owner/repo",
		State: string(workflow.TaskImplementing), Branch: "feature/two",
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		TaskID: "task-2", Repo: "owner/repo", Branch: "feature/two",
		Result: &workflow.AgentResult{Decision: "blocked", Summary: "permission denied"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := blockTaskForPermissionBlockedJob(ctx, store, db.Job{ID: "job-2", Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(ctx, "task-2")
	if err != nil || task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task = %+v err %v, want blocked", task, err)
	}
	events, err := store.ListTaskEvents(ctx, "task-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "permission_job_blocked" ||
		events[0].FromState != string(workflow.TaskImplementing) ||
		events[0].ToState != string(workflow.TaskBlocked) {
		t.Fatalf("task events = %+v, want permission block ownership", events)
	}
}

// TestPermissionBlockedJobCannotOverwriteMergedTask keeps the #1673 guarantee on
// the permission-block path after it moved to the store's atomic
// BlockTaskWithEvent: a task whose pull request already MERGED must not be
// rewritten as blocked by a leg that never ran.
//
// The refusal shape belongs to the store now — a terminal-state conflict error
// instead of the engine's silent refusal event — so this asserts the invariant
// (the landed-work record survives, and no block is attributed to it) rather than
// the old mechanism.
func TestPermissionBlockedJobCannotOverwriteMergedTask(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-3", RepoFullName: "owner/repo", State: string(workflow.TaskMerged), Branch: "feature/three",
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		TaskID: "task-3", Repo: "owner/repo", Branch: "feature/three",
		Result: &workflow.AgentResult{Decision: "blocked", Summary: "permission denied"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := blockTaskForPermissionBlockedJob(ctx, store, db.Job{ID: "job-3", Payload: string(payload)}); err == nil {
		t.Fatal("a permission-blocked leg overwrote a merged task without complaint")
	}
	task, err := store.GetTask(ctx, "task-3")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != string(workflow.TaskMerged) {
		t.Fatalf("task state = %q, want the landed-work record preserved", task.State)
	}
	events, err := store.ListTaskEvents(ctx, "task-3")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "permission_job_blocked" {
			t.Fatalf("a block was attributed to a merged task: %+v", event)
		}
	}
}
