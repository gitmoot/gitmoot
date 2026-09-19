//go:build e2e

package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestRemoteExecutionBackendDispatchesReview is the regression for the
// unreachable remote backend. Remote code reviews are the epic's target
// workload and were refused BEFORE PROVISIONING by an allowlist naming only
// "implement" — a type #2203 made impossible to create.
//
// The assertion is that a review reaches the execution backend factory at all.
// It deliberately stops there: the factory errors, so this pins ADMISSION and
// nothing about whether a sandbox then succeeds.
//
// MUTATION: restore `job.Type != "implement"` on the gate and this goes red with
// factory calls = 0.
func TestRemoteExecutionBackendDispatchesReview(t *testing.T) {
	ctx := context.Background()
	home, paths, store := heartbeatLoopE2EHome(t)
	writeRemoteLifecycleConfig(t, paths, "")
	checkout := createDaemonWorkerGitCheckout(t, "remote-review")
	headSHA := daemonWorkerHeadSHA(t, checkout)
	makeReviewFixOriginFetchable(t, checkout, "remote-review")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "remote-review-agent", runtime.ShellRuntime, heartbeatShellResultScript, []string{"review"}, "owner/repo")
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo", Number: 2225, URL: "https://example.invalid/owner/repo/pull/2225",
		HeadBranch: "remote-review", BaseBranch: "remote-review", HeadSHA: headSHA, State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}

	remoteBackend := string(execbackend.Remote)
	mailbox := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("provisioning is not reached in this test"))
	job, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "remote-review-admitted", Agent: "remote-review-agent", Action: "review",
		Repo: "owner/repo", PullRequest: 2225, HeadSHA: headSHA, Branch: "remote-review",
		Instructions: "review the head",
		ExecBackend:  &remoteBackend,
	})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	factoryCalls := 0
	worker := defaultJobWorker(store, io.Discard, home)
	worker.ReviewAdmissionGitHubFactory = func(string) remoteReviewAdmissionGitHub {
		return remoteReviewAdmissionGitHubStub{
			pull: github.PullRequest{Number: 2225, HeadSHA: headSHA},
		}
	}
	worker.ExecutionBackendFactory = func(_ execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		factoryCalls++
		return nil, errors.New("stop here: admission is what this test measures")
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}

	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if factoryCalls != 1 {
		t.Fatalf("execution backend factory calls = %d, want 1: events=%+v", factoryCalls, events)
	}
	for _, event := range events {
		if strings.Contains(event.Message, "jobs are not supported on the") {
			t.Fatalf("review was refused by the job-type allowlist: %q", event.Message)
		}
	}
}

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
