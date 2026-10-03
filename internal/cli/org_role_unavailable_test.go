package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
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

func assertQuotaInboxNotice(t *testing.T, store *db.Store, jobID string) db.Message {
	t.Helper()
	messages, err := store.ListMessages(context.Background(), "owner", 0, 100)
	if err != nil || len(messages) != 1 {
		t.Fatalf("parent inbox = %+v, err=%v; want one durable quota notice", messages, err)
	}
	message := messages[0]
	if message.Kind != "notification" || message.Sender != db.MessageSystemSender ||
		message.SourceState != "blocked" || (jobID != "" && message.SourceJobID != jobID) {
		t.Fatalf("quota provenance = %+v", message)
	}
	rows, err := store.ListWakeOutbox(context.Background(), "pending")
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for _, row := range rows {
		if row.TargetRole == "owner" && row.SourceID == message.SourceID {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("pending parent delivery matches=%d: %+v", matches, rows)
	}
	return message
}

func TestQuotaNotificationFailureDoesNotConsumeIncidentClaim(t *testing.T) {
	home, paths := setupQuotaUnavailableOrgHome(t)
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER refuse_quota_mail BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'inbox unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	hooks := quotaRoleUnavailableHooks{store: store, home: home}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	capture := func() error {
		return hooks.captureFailure(context.Background(), db.Job{ID: "job-quota"},
			workflow.JobPayload{Repo: "gitmoot/gitmoot", ActingOrgRole: "review"},
			runtime.Agent{Runtime: runtime.ClaudeRuntime},
			workflow.DeliveryError{Err: errors.New("API error: You've hit your weekly limit - resets Jul 28, 1am (Europe/Berlin)")}, now)
	}
	if err := capture(); err == nil {
		t.Fatal("failed inbox write must report notification failure")
	}
	if incident, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now); err != nil || !found || incident.EscalatedAt != "" {
		t.Fatalf("quota guard must survive without consuming its claim: incident=%+v found=%v err=%v", incident, found, err)
	}
	if _, err := raw.Exec(`DROP TRIGGER refuse_quota_mail`); err != nil {
		t.Fatal(err)
	}
	if err := capture(); err != nil {
		t.Fatal(err)
	}
	assertQuotaInboxNotice(t, store, "job-quota")
}

func TestQuotaConfigurationFailureStillPausesRuntime(t *testing.T) {
	home, paths := setupQuotaUnavailableOrgHome(t)
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.bad]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	hooks := quotaRoleUnavailableHooks{store: store, home: home}
	err = hooks.captureFailure(context.Background(), db.Job{ID: "job-quota"},
		workflow.JobPayload{ActingOrgRole: "review"}, runtime.Agent{Runtime: runtime.ClaudeRuntime},
		workflow.DeliveryError{Err: errors.New("API error: You've hit your weekly limit - resets Jul 28, 1am (Europe/Berlin)")}, now)
	if err == nil {
		t.Fatal("invalid notification configuration must be reported")
	}
	incident, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now)
	if err != nil || !found || incident.Runtime != runtime.ClaudeRuntime || incident.EscalatedAt != "" {
		t.Fatalf("configuration failure lost the quota guard or consumed the notification: %+v found=%v err=%v", incident, found, err)
	}
}

func TestCaptureQuotaRoleUnavailableEscalatesOnceAndSuccessClears(t *testing.T) {
	store, sink, wake, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}, {"review", "w1:p2"}})
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "quota-parent", OnKind: "escalation", WakeRole: "owner", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	worker := jobWorker{Store: store, ConfigHome: home, ConfigHomeExplicit: true, Stdout: &output}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	job := db.Job{ID: "job-quota", Agent: "claude-agent"}
	payload := workflow.JobPayload{Repo: "gitmoot/gitmoot", ActingOrgRole: "review"}
	cause := workflow.DeliveryError{Err: errors.New("API error: You've hit your weekly limit - resets Jul 28, 1am (Europe/Berlin)")}
	agent := runtime.Agent{Name: "claude-agent", Runtime: runtime.ClaudeRuntime}

	if err := worker.quotaRoleUnavailableHooks().captureFailure(context.Background(), job, payload, agent, cause, now); err != nil {
		t.Fatal(err)
	}
	first := assertQuotaInboxNotice(t, store, "job-quota")
	incident, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now)
	if err != nil || !found || incident.Runtime != runtime.ClaudeRuntime || incident.EscalatedAt == "" {
		t.Fatalf("incident = %+v found=%v err=%v", incident, found, err)
	}

	if err := worker.quotaRoleUnavailableHooks().captureFailure(context.Background(), job, payload, agent, cause, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if next := assertQuotaInboxNotice(t, store, "job-quota"); next.ID != first.ID {
		t.Fatalf("repeat quota failure created another message: %d -> %d", first.ID, next.ID)
	}
	drainReplyWakeAfterAllRowsAreDue(t, store, sink)
	delivered, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 1 || delivered[0].SourceID != first.SourceID || wake.pane != "w1:p1" {
		t.Fatalf("parent notification did not reach its registered runtime: rows=%+v target=%q err=%v", delivered, wake.pane, err)
	}

	if err := worker.quotaRoleUnavailableHooks().clearOnSuccess(context.Background(), "review", runtime.ClaudeRuntime); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now); err != nil || found {
		t.Fatalf("success clear found=%v err=%v", found, err)
	}
}

func TestQuotaRoleUnavailableKeepsMailForUnresolvedParentBinding(t *testing.T) {
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
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hooks := quotaRoleUnavailableHooks{store: store, home: home}
	err = hooks.captureFailure(context.Background(), db.Job{ID: "job-quota"},
		workflow.JobPayload{Repo: "gitmoot/gitmoot", ActingOrgRole: "review"},
		runtime.Agent{Runtime: runtime.ClaudeRuntime},
		workflow.DeliveryError{Err: errors.New("API error: You've hit your weekly limit - resets Jul 28, 1am (Europe/Berlin)")}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	assertQuotaInboxNotice(t, store, "job-quota")
}

func TestCaptureQuotaRoleUnavailableClaudeOnly(t *testing.T) {
	store := daemonWorkerStore(t)
	worker := jobWorker{Store: store}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cause := workflow.DeliveryError{Err: errors.New("You've hit your weekly limit - resets Jul 28, 1am (Europe/Berlin)")}
	payload := workflow.JobPayload{ActingOrgRole: "review"}
	if err := worker.quotaRoleUnavailableHooks().captureFailure(context.Background(), db.Job{ID: "codex-job"}, payload, runtime.Agent{Runtime: runtime.CodexRuntime}, cause, now); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListActiveOrgRolesUnavailable(context.Background(), now); err != nil || len(rows) != 0 {
		t.Fatalf("non-Claude incidents = %+v err=%v", rows, err)
	}
}

func TestForegroundDispatchCapturesQuotaFailureAndClearsOnSuccess(t *testing.T) {
	home, paths := setupQuotaUnavailableOrgHome(t)
	store, err := dbtest.Open(t, paths.Database)
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
	assertQuotaInboxNotice(t, store, "")

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
	store, err := dbtest.Open(t, paths.Database)
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
	store, err := dbtest.Open(t, paths.Database)
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
	worker := defaultJobWorker(store, io.Discard, home)
	worker.StartAdapterFactory = func(execbackend.Backend, string, string) (runtime.Adapter, error) { return starter, nil }
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return delivery, nil }
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
	assertQuotaInboxNotice(t, store, "job-temp-quota")

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
	store, err := dbtest.Open(t, paths.Database)
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
