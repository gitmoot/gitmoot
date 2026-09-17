//go:build e2e

package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const pipelineAutoMergeSpec = `name: auto-merge
repo: owner/repo
allow_auto_merge: true
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: review
    agent: reviewer
    prompt: Review the implementation PR.
    action: review
    source: impl
    needs: [impl]
    success_decisions: [approved]
  - id: merge
    gate: pr_merged
    merge: auto
    source: impl
    needs: [impl]
`

type stubPipelineAutoMerger struct {
	readiness    workflow.PipelineAutoMergeReadiness
	mergeResult  workflow.PipelineAutoMergeResult
	evaluateReqs []workflow.PipelineAutoMergeRequest
	mergeReqs    []workflow.PipelineAutoMergeRequest
}

func (s *stubPipelineAutoMerger) Evaluate(_ context.Context, request workflow.PipelineAutoMergeRequest) (workflow.PipelineAutoMergeReadiness, error) {
	s.evaluateReqs = append(s.evaluateReqs, request)
	return s.readiness, nil
}

func (s *stubPipelineAutoMerger) Merge(_ context.Context, request workflow.PipelineAutoMergeRequest) (workflow.PipelineAutoMergeResult, error) {
	s.mergeReqs = append(s.mergeReqs, request)
	return s.mergeResult, nil
}

func advanceWithAutoMerge(t *testing.T, store *db.Store, enqueue pipeline.PipelineStageEnqueuer, rec db.Pipeline, spec pipeline.Spec, run db.PipelineRun, now time.Time, executor pipeline.PipelineAutoMergeExecutor) db.PipelineRun {
	t.Helper()
	updated, err := pipeline.AdvancePipelineRunWithAutoMerge(context.Background(), store, enqueue, rec, spec, run, now, executor)
	if err != nil {
		t.Fatalf("AdvancePipelineRunWithAutoMerge: %v", err)
	}
	return updated
}

func TestPipelineAutoMergeShellRuntimeE2E(t *testing.T) {
	ctx := context.Background()
	_, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "coder", runtime.ShellRuntime, pipelineStageResultCmd("implemented", "fixed", nil), []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, pipelineStageResultCmd("approved", "approved", nil), []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	rec, spec := newTestPipeline(t, store, "auto-merge", pipelineAutoMergeSpec)
	// #2203: the real enqueuer now REFUSES an implement stage, and a pr_merged gate
	// requires an implement source, so this test drives the stub stage enqueuer. What
	// it covers is AUTO-MERGE - the executor request and the gate fold - not the
	// enqueue seam, which TestPipelineImplementStageIsRefusedAtEnqueueE2E owns.
	enqueue := testStageEnqueuer(store)
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, now)
	implRow := stageRow(t, store, run.ID, "impl")
	implJob, err := store.GetJob(ctx, implRow.JobID)
	if err != nil {
		t.Fatalf("GetJob(impl): %v", err)
	}
	implPayload, err := workflow.ParseJobPayload(implJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(impl): %v", err)
	}
	// The head SHA is produced in a PLAIN temporary repo. It used to be committed
	// inside the implement stage's writable worktree, which #2203 removed - but the
	// auto-merge contract only needs a real 40-char SHA to bind the PR to, and where
	// that SHA came from was never part of what this test proves. Owner-authorized
	// re-homing (workflow note 173859) so auto-merge keeps end-to-end coverage.
	headRepo := createDaemonWorkerGitCheckout(t, "main")
	if err := os.WriteFile(filepath.Join(headRepo, "auto.txt"), []byte("auto merge\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runDaemonWorkerGit(t, headRepo, "add", "auto.txt")
	runDaemonWorkerGit(t, headRepo, "commit", "-m", "auto merge fixture")
	head, err := (gitutil.NewHostClient(headRepo)).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	settleBoundImplementStageJob(t, store, implJob.ID, "implemented", pipeline.PipelineStagePRBinding{PullRequest: 815, HeadSHA: head, Branch: implPayload.Branch, TaskID: implPayload.TaskID, LeadAgent: "coder"})
	executor := &stubPipelineAutoMerger{readiness: workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: head}, mergeResult: workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "merged-815"}}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second), executor)
	// The review stage is settled directly rather than by a worker tick: the stub stage
	// enqueuer does not create runnable shell jobs. The review's DECISION is what the
	// gate depends on (success_decisions: [approved]), and that is asserted below via
	// the auto-merge request, so nothing this test proves is lost.
	reviewRow := stageRow(t, store, run.ID, "review")
	if reviewRow.JobID == "" {
		t.Fatalf("review stage has no job: %+v", reviewRow)
	}
	settleStageJob(t, store, reviewRow.JobID, "approved", "approved", nil)
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(3*time.Second), executor)
	if run.State != pipeline.RunSucceeded || len(executor.mergeReqs) != 1 {
		t.Fatalf("shell E2E run=%+v merge calls=%d", run, len(executor.mergeReqs))
	}
	if got := executor.mergeReqs[0]; got.PullRequest != 815 || got.HeadSHA != head {
		t.Fatalf("shell E2E merge request = %+v, want PR 815 head %s", got, head)
	}
}
