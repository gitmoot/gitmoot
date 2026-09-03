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

// End to end through jobWorker.run: an unusable staged credential must fail the
// job BEFORE any runtime launch. Measured cause: three seat reviews failed at
// 06:31-06:36Z relaying "OAuth session expired and could not be refreshed",
// which named OAuth rather than the snapshot gitmoot had staged.
func TestWorkerRefusesSeatWithUnusableStagedCredential(t *testing.T) {
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

	launched := false
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		launched = true
		t.Error("the runtime must not be launched on a credential that cannot authenticate")
		return nil, nil
	}
	job, err := store.GetJob(ctx, "cred-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}

	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobFailed) || launched {
		t.Fatalf("job state=%q launched=%v, want failed without a launch", stored.State, launched)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	for _, event := range events {
		if strings.Contains(event.Message, ".credentials.json") {
			reason = event.Message
		}
	}
	if reason == "" {
		t.Fatalf("no event names the staged credential; events=%+v", events)
	}
	if !strings.Contains(reason, "no refresh token") || !strings.Contains(reason, "re-login") {
		t.Fatalf("failure reason %q must say what is wrong and what fixes it", reason)
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
	if !strings.Contains(recorded, expiry.Format(time.RFC3339)) || !strings.Contains(recorded, sourceDir) {
		t.Fatalf("event %q must name the expiry and the source dir", recorded)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State == string(workflow.JobFailed) && runner.calls == 0 {
		t.Fatalf("a refreshable credential must still reach the runtime; state=%q calls=%d", stored.State, runner.calls)
	}
}
