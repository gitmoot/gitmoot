//go:build e2e

package cli

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestRemoteExecutionBackendStillRefusesAsk pins the boundary. Lifting the gate
// for review must not open it for everything: "ask" has no worktree contract and
// no acceptance in #1529, and it must still be refused BEFORE a billable sandbox
// exists.
func TestRemoteExecutionBackendStillRefusesAsk(t *testing.T) {
	ctx := context.Background()
	home, paths, store := heartbeatLoopE2EHome(t)
	writeRemoteLifecycleConfig(t, paths, "")
	checkout := createDaemonWorkerGitCheckout(t, "remote-ask-boundary")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "remote-ask-agent", runtime.ShellRuntime, heartbeatShellResultScript, []string{"ask"}, "owner/repo")

	mailbox := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("must refuse before checkout"))
	job, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "remote-ask-still-refused", Agent: "remote-ask-agent", Action: "ask",
		Repo: "owner/repo", Instructions: "must not provision",
	})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	factoryCalls := 0
	worker := defaultJobWorker(store, io.Discard, home)
	worker.ExecutionBackendFactory = func(_ execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		factoryCalls++
		return nil, errors.New("factory must not be called for ask")
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("execution backend factory calls = %d, want 0: ask must be refused before provisioning", factoryCalls)
	}
	completed, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if completed.State != string(workflow.JobFailed) {
		t.Fatalf("state = %q, want failed", completed.State)
	}
}
