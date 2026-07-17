package cli

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestServiceShellStageGetsDetachedWorktreeAndLeavesCheckoutUntouched(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "service-kit", Repo: "owner/repo", SpecYAML: "name: service-kit\n", SpecHash: "sha256:spec"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC)
	if err := store.CreatePipelineRun(ctx, db.PipelineRun{ID: "prun-service-kit", Pipeline: "service-kit", Trigger: "service", State: "running", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	beforeHead := gitOutput(t, checkout, "rev-parse", "HEAD")
	beforeStatus := gitOutput(t, checkout, "status", "--porcelain")

	request := workflow.JobRequest{
		ID: "prun-service-kit-build-a0", Agent: "pipeline-service-kit-runner", Action: "ask", Repo: "owner/repo",
		Sender: workflow.PipelineJobSender, Instructions: "pipeline service-kit run prun-service-kit stage build",
		RootJobID: "prun-service-kit", RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: "printf ok",
	}
	enqueue := newPipelineStageEnqueuer(store, home)
	job, err := enqueue(ctx, request)
	if err != nil {
		t.Fatalf("enqueue service shell stage: %v", err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(payload.WorktreePath) == "" || !payload.ReadOnlyWorktree {
		t.Fatalf("service shell payload lacks detached cleanup-marked worktree: %+v", payload)
	}
	if payload.WorktreePath == checkout {
		t.Fatalf("service shell stage fell back to registered checkout %s", checkout)
	}
	if info, err := os.Stat(payload.WorktreePath); err != nil || !info.IsDir() {
		t.Fatalf("detached worktree %q unavailable: info=%v err=%v", payload.WorktreePath, info, err)
	}
	if afterHead := gitOutput(t, checkout, "rev-parse", "HEAD"); afterHead != beforeHead {
		t.Fatalf("registered checkout HEAD changed: before=%q after=%q", beforeHead, afterHead)
	}
	if afterStatus := gitOutput(t, checkout, "status", "--porcelain"); afterStatus != beforeStatus {
		t.Fatalf("registered checkout changed: before=%q after=%q", beforeStatus, afterStatus)
	}
	adopted, err := enqueue(ctx, request)
	if err != nil || adopted.ID != job.ID {
		t.Fatalf("idempotent re-enqueue did not adopt isolated job: id=%q err=%v", adopted.ID, err)
	}
}

func TestServiceShellStageIsolationFailsClosedWithoutCheckout(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	if err := store.CreatePipelineRun(ctx, db.PipelineRun{ID: "prun-service-missing", Pipeline: "missing", Trigger: "service", State: "running", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	request := workflow.JobRequest{
		ID: "prun-service-missing-build-a0", Agent: "runner", Action: "ask", Repo: "owner/missing",
		Sender: workflow.PipelineJobSender, RootJobID: "prun-service-missing",
		RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: "printf ok",
	}
	if _, err := newPipelineStageEnqueuer(store, home)(ctx, request); err == nil || !strings.Contains(err.Error(), "requires a detached worktree") {
		t.Fatalf("service shell allocation err=%v, want fail-closed error", err)
	}
	if _, err := store.GetJob(ctx, request.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("fail-closed service stage created a job: %v", err)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
