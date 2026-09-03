package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// End to end through jobWorker.run: an expired staged credential must be
// RECORDED and the job must still run. The first version of this change failed
// the job here instead, and the #1810 review showed that converted a deferrable
// runtime-auth blocker into a terminal failure with a PR comment, while the auth
// overlay in the same commit means the staged snapshot no longer decides whether
// the seat can authenticate.
func TestWorkerRecordsExpiredSeatCredentialAndStillRuns(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"a","expiresAt":0,"refreshToken":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgentWithPolicy(t, store, "claude-reviewer", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440000", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "cred-review", Agent: "claude-reviewer", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, RuntimeConfigDir: sourceDir,
	})

	runner := &repairStateRunner{}
	launched := false
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		launched = true
		return runtime.ClaudeAdapter{Runner: runner}, nil
	}
	job, err := store.GetJob(ctx, "cred-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}

	if !launched {
		t.Fatal("an expired staged credential must not stop the runtime from launching")
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var recorded string
	for _, event := range events {
		if event.Kind == "readonly_seat_credential_expired" {
			recorded = event.Message
		}
	}
	if recorded == "" {
		t.Fatalf("expected a readonly_seat_credential_expired event; events=%+v", events)
	}
	if strings.Contains(recorded, sourceDir) {
		t.Fatalf("event %q publishes the absolute host credential path", recorded)
	}
	// No terminal failure was minted BY THE PREFLIGHT: whatever the runtime does
	// next owns the outcome.
	for _, event := range events {
		if event.Kind == string(workflow.JobFailed) && strings.Contains(event.Message, "credential") {
			t.Fatalf("preflight minted a terminal credential failure: %q", event.Message)
		}
	}
}

// The refreshable case must NOT be refused: refreshing is the runtime's job and
// normally works, so refusing would break every host whose credential refreshes.
// It records the fact, so a later auth failure is not read as a fresh OAuth
// problem.
func TestWorkerRecordsExpiredButRefreshableSeatCredential(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	sourceDir := t.TempDir()
	expiry := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := os.WriteFile(filepath.Join(sourceDir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"a","expiresAt":`+
			strconv.FormatInt(expiry.UnixMilli(), 10)+`,"refreshToken":"r"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgentWithPolicy(t, store, "claude-reviewer", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440000", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "cred-review-refreshable", Agent: "claude-reviewer", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, RuntimeConfigDir: sourceDir,
	})

	runner := &repairStateRunner{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return runtime.ClaudeAdapter{Runner: runner}, nil
	}
	job, err := store.GetJob(ctx, "cred-review-refreshable")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}

	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var recorded string
	for _, event := range events {
		if event.Kind == "readonly_seat_credential_expired" {
			recorded = event.Message
		}
	}
	if recorded == "" {
		t.Fatalf("expected a readonly_seat_credential_expired event; events=%+v", events)
	}
	if !strings.Contains(recorded, expiry.Format(time.RFC3339)) {
		t.Fatalf("event %q must name the expiry", recorded)
	}
	// The absolute host path is deliberately NOT in the message: this text used to
	// reach a PR comment, and #1810's review flagged publishing it.
	if strings.Contains(recorded, sourceDir) {
		t.Fatalf("event %q publishes the absolute host credential path", recorded)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State == string(workflow.JobFailed) && runner.calls == 0 {
		t.Fatalf("a refreshable credential must still reach the runtime; state=%q calls=%d", stored.State, runner.calls)
	}
}
