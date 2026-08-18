package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func setupQuotaUnavailableOrgHome(t *testing.T) (string, config.Paths) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`
[org.roles."owner"]
scope = ["*"]
pane = "w1:p1"
[org.roles."review"]
parent = "owner"
scope = ["gitmoot/*"]
pane = "w1:p2"
`)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return home, paths
}

func TestCaptureQuotaRoleUnavailableEscalatesOnceAndSuccessClears(t *testing.T) {
	home, paths := setupQuotaUnavailableOrgHome(t)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wake := &fakeEventWake{}
	var output bytes.Buffer
	worker := jobWorker{Store: store, ConfigHome: home, ConfigHomeExplicit: true, Stdout: &output, QuotaWake: wake}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	job := db.Job{ID: "job-quota", Agent: "claude-agent"}
	payload := workflow.JobPayload{Repo: "gitmoot/gitmoot", ActingOrgRole: "review"}
	cause := workflow.DeliveryError{Err: errors.New("API error: You've hit your weekly limit - resets Jul 28, 1am (Europe/Berlin)")}
	agent := runtime.Agent{Name: "claude-agent", Runtime: runtime.ClaudeRuntime}

	if err := worker.captureQuotaRoleUnavailable(context.Background(), job, payload, agent, cause, now); err != nil {
		t.Fatal(err)
	}
	if wake.promptCalls != 1 || wake.pane != "w1:p1" {
		t.Fatalf("wake = calls=%d pane=%q prompt=%q", wake.promptCalls, wake.pane, wake.prompt)
	}
	if !strings.Contains(wake.prompt, "review is UNAVAILABLE") || !strings.Contains(wake.prompt, "reason=quota") {
		t.Fatalf("wake prompt = %q", wake.prompt)
	}
	incident, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now)
	if err != nil || !found || incident.Runtime != runtime.ClaudeRuntime || incident.EscalatedAt == "" {
		t.Fatalf("incident = %+v found=%v err=%v", incident, found, err)
	}

	if err := worker.captureQuotaRoleUnavailable(context.Background(), job, payload, agent, cause, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if wake.promptCalls != 1 {
		t.Fatalf("repeat quota failure woke %d times, want exactly once", wake.promptCalls)
	}

	if err := worker.clearQuotaRoleUnavailableOnSuccess(context.Background(), "review", runtime.ClaudeRuntime); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now); err != nil || found {
		t.Fatalf("success clear found=%v err=%v", found, err)
	}
}

func TestQuotaRoleUnavailableNudgeCountsUnresolvedParentBinding(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[org.roles."owner"]
scope = ["*"]
pane = "missing-label"
[org.roles."review"]
parent = "owner"
scope = ["gitmoot/*"]
pane = "w1:p2"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var output bytes.Buffer
	wake := &fakeEventWake{}
	hooks := quotaRoleUnavailableHooks{store: store, home: home, stdout: &output, wake: wake}
	hooks.wakeParent(
		context.Background(),
		db.Job{ID: "job-quota"},
		workflow.JobPayload{Repo: "gitmoot/gitmoot", ActingOrgRole: "review"},
		db.OrgRoleUnavailable{Role: "review", Until: time.Now().Add(time.Hour).Format(db.BlockedEpisodeTimeLayout)},
		true,
		true,
	)
	missed, err := store.ListRoleMissedWakes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(missed) != 1 || missed[0].Role != "owner" || missed[0].Consecutive != 1 {
		t.Fatalf("unresolved parent missed wakes = %+v, want owner count 1", missed)
	}
	if !strings.Contains(output.String(), "parent role owner has no pane") {
		t.Fatalf("output = %q", output.String())
	}
	if wake.promptCalls != 0 {
		t.Fatalf("unresolved parent prompted %d times", wake.promptCalls)
	}
}

func TestCaptureQuotaRoleUnavailableClaudeOnly(t *testing.T) {
	store := daemonWorkerStore(t)
	worker := jobWorker{Store: store}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cause := workflow.DeliveryError{Err: errors.New("You've hit your weekly limit - resets Jul 28, 1am (Europe/Berlin)")}
	payload := workflow.JobPayload{ActingOrgRole: "review"}
	if err := worker.captureQuotaRoleUnavailable(context.Background(), db.Job{ID: "codex-job"}, payload, runtime.Agent{Runtime: runtime.CodexRuntime}, cause, now); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListActiveOrgRolesUnavailable(context.Background(), now); err != nil || len(rows) != 0 {
		t.Fatalf("non-Claude incidents = %+v err=%v", rows, err)
	}
}

func TestForegroundDispatchCapturesQuotaFailureAndClearsOnSuccess(t *testing.T) {
	home, paths := setupQuotaUnavailableOrgHome(t)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/gitmoot/gitmoot.git")
	seedDaemonWorkerRepo(t, store, "gitmoot/gitmoot", checkout)
	seedDaemonWorkerAgent(t, store, "claude-reviewer", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440002", []string{"ask"}, "gitmoot/gitmoot")

	adapter := &cliWorkerFakeAdapter{err: errors.New("API error: You've hit your weekly limit - resets Jul 28, 1am (Europe/Berlin)")}
	previousAdapterFactory := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, runtime.Agent, string) (runtime.Adapter, error) {
		return adapter, nil
	}
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previousAdapterFactory })
	wake := &fakeEventWake{}
	previousWakeFactory := newQuotaRoleUnavailableWakeClient
	newQuotaRoleUnavailableWakeClient = func() eventWakeClient { return wake }
	t.Cleanup(func() { newQuotaRoleUnavailableWakeClient = previousWakeFactory })

	request := localAgentDispatchRequest{
		RepoFlag:       "gitmoot/gitmoot",
		Agent:          "claude-reviewer",
		Action:         "ask",
		Instructions:   "Review the change.",
		ActingOrgRole:  "review",
		OperatorOrigin: true,
		ExecutionPath:  "agent_ask",
		Home:           home,
	}
	if _, err := dispatchLocalAgentJob(context.Background(), store, request); err == nil {
		t.Fatal("foreground quota failure returned nil error")
	}
	now := time.Now().UTC()
	if incident, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now); err != nil || !found {
		t.Fatalf("foreground quota incident = %+v found=%v err=%v", incident, found, err)
	}
	if wake.promptCalls != 1 {
		t.Fatalf("foreground quota escalation calls = %d, want 1", wake.promptCalls)
	}

	if err := store.ClearOrgRoleUnavailable(context.Background(), "review"); err != nil {
		t.Fatal(err)
	}
	adapter.err = nil
	adapter.output = `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	adapter.onDeliver = func() {
		seedNow := time.Now().UTC()
		if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", runtime.ClaudeRuntime, "quota", seedNow.Add(time.Hour), seedNow); err != nil {
			t.Errorf("seed in-flight unavailability: %v", err)
		}
	}
	if _, err := dispatchLocalAgentJob(context.Background(), store, request); err != nil {
		t.Fatalf("foreground success: %v", err)
	}
	if incident, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", time.Now().UTC()); err != nil || found {
		t.Fatalf("foreground success left incident = %+v found=%v err=%v", incident, found, err)
	}
}

func TestSuccessfulJobOnlyClearsQuotaRoleUnavailableForSameRuntime(t *testing.T) {
	home, paths := setupQuotaUnavailableOrgHome(t)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "gitmoot/gitmoot", checkout)
	seedDaemonWorkerAgent(t, store, "success-worker", runtime.ShellRuntime,
		`printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`,
		[]string{"ask"}, "gitmoot/gitmoot")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-success-clear", Agent: "success-worker", Action: "ask", Repo: "gitmoot/gitmoot",
		ActingOrgRole: "review",
	})
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", runtime.ClaudeRuntime, "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	worker := blockerE2EWorker(store, home, checkout)
	worker.QuotaWake = nil
	job, err := store.GetJob(context.Background(), "job-success-clear")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(context.Background(), job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	job, err = store.GetJob(context.Background(), "job-success-clear")
	if err != nil || job.State != string(workflow.JobSucceeded) {
		t.Fatalf("successful job = %+v err=%v", job, err)
	}
	if incident, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now); err != nil || !found {
		t.Fatalf("shell success cleared Claude incident = %+v found=%v err=%v", incident, found, err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.quotaRoleUnavailableHooks().recordRuntimeOutcome(
		context.Background(), job, payload, runtime.Agent{Runtime: runtime.ClaudeRuntime}, nil, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now); err != nil || found {
		t.Fatalf("Claude success left Claude incident found=%v err=%v", found, err)
	}
}

func TestTempWorkerDispatchCapturesQuotaFailureAndClearsOnSuccess(t *testing.T) {
	home, paths := setupQuotaUnavailableOrgHome(t)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/gitmoot/gitmoot.git")
	seedDaemonWorkerRepo(t, store, "gitmoot/gitmoot", checkout)
	original := runtime.Agent{
		Name: "claude-reviewer", Role: "worker", Runtime: runtime.ClaudeRuntime,
		RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "gitmoot/gitmoot",
		Capabilities: []string{"ask"}, AutonomyPolicy: runtime.AutonomyPolicyAuto,
	}
	seedDaemonWorkerAgent(t, store, original.Name, original.Runtime, original.RuntimeRef, original.Capabilities, original.RepoScope)

	starter := &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655440003"}
	delivery := &cliWorkerFakeAdapter{err: errors.New("API error: You've hit your weekly limit - resets Jul 28, 1am (Europe/Berlin)")}
	wake := &fakeEventWake{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.StartAdapterFactory = func(execbackend.Backend, string, string) (runtime.Adapter, error) { return starter, nil }
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return delivery, nil }
	worker.QuotaWake = wake
	policy := config.DefaultParallelSessionPolicy()
	policy.MergeBack = config.ParallelSessionMergeBackOff

	runTemp := func(jobID string) {
		t.Helper()
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: jobID, Agent: original.Name, Action: "ask", Repo: "gitmoot/gitmoot",
			ActingOrgRole: "review",
		})
		job, err := store.GetJob(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatal(err)
		}
		if err := worker.runWithTempWorker(context.Background(), job, payload, execbackend.Local, original, checkout, policy, "test contention", false); err != nil {
			t.Fatalf("runWithTempWorker(%s): %v", jobID, err)
		}
	}

	runTemp("job-temp-quota")
	now := time.Now().UTC()
	if incident, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now); err != nil || !found {
		t.Fatalf("temp-worker quota incident = %+v found=%v err=%v", incident, found, err)
	}
	if wake.promptCalls != 1 {
		t.Fatalf("temp-worker escalation calls = %d, want 1", wake.promptCalls)
	}

	if err := store.ClearOrgRoleUnavailable(context.Background(), "review"); err != nil {
		t.Fatal(err)
	}
	delivery.err = nil
	delivery.output = `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	delivery.onDeliver = func() {
		seedNow := time.Now().UTC()
		if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", runtime.ClaudeRuntime, "quota", seedNow.Add(time.Hour), seedNow); err != nil {
			t.Errorf("seed in-flight unavailability: %v", err)
		}
	}
	runTemp("job-temp-success")
	if incident, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", time.Now().UTC()); err != nil || found {
		t.Fatalf("temp-worker success left incident = %+v found=%v err=%v", incident, found, err)
	}
}

func TestValidateAndTouchActingOrgRoleRefusesUnavailableAndClearsExpired(t *testing.T) {
	home, paths := setupQuotaUnavailableOrgHome(t)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := validateAndTouchActingOrgRole(ctx, store, home, "review", "agent_run"); err != nil {
		t.Fatalf("available role refused: %v", err)
	}
	if err := store.UpsertOrgRoleUnavailable(ctx, "review", "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	err = validateAndTouchActingOrgRole(ctx, store, home, "REVIEW", "agent_run")
	if err == nil || !strings.Contains(err.Error(), `org role "review" is unavailable`) ||
		!strings.Contains(err.Error(), "reason=quota") || !strings.Contains(err.Error(), "dispatch refused") {
		t.Fatalf("unavailable refusal = %v", err)
	}

	if err := store.ClearOrgRoleUnavailable(ctx, "review"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertOrgRoleUnavailable(ctx, "review", "quota", now.Add(-time.Minute), now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := validateAndTouchActingOrgRole(ctx, store, home, "review", "agent_run"); err != nil {
		t.Fatalf("expired role refused: %v", err)
	}
	if _, found, err := store.GetActiveOrgRoleUnavailable(ctx, "review", now); err != nil || found {
		t.Fatalf("expired row found=%v err=%v", found, err)
	}
}

func TestRunTaskRunRefusesUnavailableRoleBeforeWorktreeAllocation(t *testing.T) {
	home, paths := setupQuotaUnavailableOrgHome(t)
	goalPath := filepath.Join(t.TempDir(), "GOAL.md")
	if err := os.WriteFile(goalPath, []byte("# Build Gitmoot\n\n### Task 1: Bootstrap\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "gitmoot/gitmoot"}, &stdout, &stderr); code != 0 {
		t.Fatalf("goal import code=%d stderr=%q", code, stderr.String())
	}
	subscribeShellImplementAgent(t, home, "lead", "gitmoot/gitmoot")
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/gitmoot/gitmoot.git")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "-c", "user.name=Gitmoot Test", "-c", "user.email=gitmoot@example.com", "commit", "-m", "initial")
	withWorkingDirectory(t, checkout)

	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailable(context.Background(), "review", "quota", now.Add(time.Hour), now); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{
		"task", "run", "task-001", "--home", home, "--repo", "gitmoot/gitmoot",
		"--owner", "lead", "--org-role", "review",
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `org role "review" is unavailable`) ||
		!strings.Contains(stderr.String(), "dispatch refused") {
		t.Fatalf("task run unavailable: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.GetTask(context.Background(), "task-001")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != string(workflow.TaskPlanned) || strings.TrimSpace(task.WorktreePath) != "" {
		t.Fatalf("task mutated before refusal: %+v", task)
	}
	if jobs, err := store.ListJobs(context.Background()); err != nil || len(jobs) != 0 {
		t.Fatalf("jobs after refused task run = %+v err=%v", jobs, err)
	}
	if _, err := os.Stat(filepath.Join(paths.Home, "worktrees", "gitmoot--gitmoot", "task-001")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree exists after refused task run: %v", err)
	}
}

func TestListPendingQueuedJobsHoldsUnavailableRole(t *testing.T) {
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "worker", runtime.ShellRuntime, "true", []string{"ask"}, "gitmoot/gitmoot")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-review", Agent: "worker", Action: "ask", Repo: "gitmoot/gitmoot", ActingOrgRole: "review",
	})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-owner", Agent: "worker", Action: "ask", Repo: "gitmoot/gitmoot", ActingOrgRole: "owner",
	})
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailable(context.Background(), "review", "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	pending, err := listPendingQueuedJobs(context.Background(), jobWorker{Store: store}, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "job-owner" {
		t.Fatalf("pending with unavailable review = %+v", pending)
	}

	if err := store.ClearOrgRoleUnavailable(context.Background(), "review"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertOrgRoleUnavailable(context.Background(), "review", "quota", now.Add(-time.Minute), now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	pending, err = listPendingQueuedJobs(context.Background(), jobWorker{Store: store}, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending after expiry = %+v", pending)
	}
}

func TestOrgStatusUnavailableOverlay(t *testing.T) {
	_, paths := setupQuotaUnavailableOrgHome(t)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	if err := store.UpsertOrgRoleUnavailable(context.Background(), "review", "quota", until, now); err != nil {
		t.Fatal(err)
	}
	shared, err := loadOrgSharedState(context.Background(), paths, store, now)
	if err != nil {
		t.Fatal(err)
	}
	source := func(context.Context, config.OrgConfig) (map[string]org.RoleLiveState, time.Time, string, error) {
		return map[string]org.RoleLiveState{
			"owner":  {State: org.StateIdle},
			"review": {State: org.StateWorking},
		}, now, "fixture", nil
	}
	rows, err := buildOrgStatusRows(context.Background(), &shared, source, "status", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Role != "review" {
			continue
		}
		if row.ProviderState != org.StateUnavailable || row.UnavailableReason != "quota" ||
			row.UnavailableUntil != until.Format(time.RFC3339) ||
			!strings.Contains(row.ProviderDetail, "⚠ UNAVAILABLE") {
			t.Fatalf("review row = %+v", row)
		}
		return
	}
	t.Fatal("review row missing")
}

func TestBlockedRoleWakeLoopClearsExpiredQuotaUnavailableWithoutHerdr(t *testing.T) {
	store := daemonWorkerStore(t)
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailable(context.Background(), "review", "quota", now.Add(-time.Minute), now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	missingHome := filepath.Join(t.TempDir(), "no-config")
	runBlockedRoleWakeOnce(context.Background(), store, missingHome, &bytes.Buffer{}, now, blockedRoleWakeDependencies{})
	if cleared, err := store.ClearExpiredOrgRolesUnavailable(context.Background(), now); err != nil || cleared != 0 {
		t.Fatalf("post-sweep clear count = %d err=%v, want already cleared", cleared, err)
	}
}
