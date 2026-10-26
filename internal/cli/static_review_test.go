package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestWorkerEnforcesRepositoryNoExecutionPolicy: a repository declaring
// execution = "none" makes EVERY review of it a no-execution review, whatever
// queued the job, so a review that cannot run under that contract (here a
// claude reviewer) is failed before any runtime launches. A review of the same
// shape with no declaration, or with an unreadable one, shows both sides of
// the decision.
func TestWorkerEnforcesRepositoryNoExecutionPolicy(t *testing.T) {
	cases := []struct {
		name         string
		config       string
		wantLaunched bool
		wantFailure  string
	}{
		{name: "undeclared runs", config: "", wantLaunched: true},
		{name: "declared none refuses a non-omp reviewer", config: "[repos.\"owner/repo\".review]\nexecution = \"none\"\n", wantFailure: "not supported on runtime \"claude\""},
		{name: "unreadable declaration refuses", config: "[repos.\"owner/repo\".review]\nexecution = \"sometimes\"\n", wantFailure: "review execution policy is unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, home := blockerE2EHome(t)
			if tc.config != "" {
				paths, err := pathsFromFlag(home)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
					t.Fatal(err)
				}
				existing, _ := os.ReadFile(paths.ConfigFile)
				if err := os.WriteFile(paths.ConfigFile, append(existing, []byte("\n"+tc.config)...), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
			seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
			seedDaemonWorkerAgentWithPolicy(t, store, "claude-reviewer", runtime.ClaudeRuntime,
				"550e8400-e29b-41d4-a716-446655440000", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
			enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
				ID: "policy-review", Agent: "claude-reviewer", Action: "review", Repo: "owner/repo",
				WorktreePath: checkout, ReadOnlySeat: true,
			})
			launched := false
			worker := defaultJobWorker(store, io.Discard, home)
			worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
				return checkout, nil
			}
			worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
				launched = true
				return runtime.ClaudeAdapter{Runner: &repairStateRunner{}}, nil
			}
			job, err := store.GetJob(ctx, "policy-review")
			if err != nil {
				t.Fatal(err)
			}
			if err := worker.run(ctx, job); err != nil {
				t.Fatal(err)
			}
			if launched != tc.wantLaunched {
				t.Fatalf("runtime launched = %v, want %v", launched, tc.wantLaunched)
			}
			if tc.wantFailure == "" {
				return
			}
			job, err = store.GetJob(ctx, "policy-review")
			if err != nil {
				t.Fatal(err)
			}
			if job.State != string(workflow.JobFailed) {
				t.Fatalf("state = %q, want failed", job.State)
			}
			events, err := store.ListJobEvents(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == string(workflow.JobFailed) && strings.Contains(event.Message, tc.wantFailure) {
					return
				}
			}
			t.Fatalf("no failure naming %q; events=%+v", tc.wantFailure, events)
		})
	}
}
