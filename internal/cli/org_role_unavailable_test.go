package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
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
	if err != nil || !found || incident.EscalatedAt == "" {
		t.Fatalf("incident = %+v found=%v err=%v", incident, found, err)
	}

	if err := worker.captureQuotaRoleUnavailable(context.Background(), job, payload, agent, cause, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if wake.promptCalls != 1 {
		t.Fatalf("repeat quota failure woke %d times, want exactly once", wake.promptCalls)
	}

	if err := worker.clearQuotaRoleUnavailableOnSuccess(context.Background(), "review"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now); err != nil || found {
		t.Fatalf("success clear found=%v err=%v", found, err)
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

func TestSuccessfulJobClearsQuotaRoleUnavailable(t *testing.T) {
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
	if err := store.UpsertOrgRoleUnavailable(context.Background(), "review", "quota", now.Add(time.Hour), now); err != nil {
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
	if _, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", now); err != nil || found {
		t.Fatalf("incident after successful job found=%v err=%v", found, err)
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
