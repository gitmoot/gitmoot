package cli

import (
	"context"
	"io"
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
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "coder", runtime.ShellRuntime, pipelineStageResultCmd("implemented", "fixed", nil), []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, pipelineStageResultCmd("approved", "approved", nil), []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	rec, spec := newTestPipeline(t, store, "auto-merge", pipelineAutoMergeSpec)
	enqueue := newPipelineStageEnqueuer(store, home)
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
	if err := os.WriteFile(filepath.Join(implPayload.WorktreePath, "auto.txt"), []byte("auto merge\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runDaemonWorkerGit(t, implPayload.WorktreePath, "add", "auto.txt")
	runDaemonWorkerGit(t, implPayload.WorktreePath, "commit", "-m", "auto merge fixture")
	head, err := (gitutil.Client{Dir: implPayload.WorktreePath}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	settleBoundImplementStageJob(t, store, implJob.ID, "implemented", pipeline.PipelineStagePRBinding{PullRequest: 815, HeadSHA: head, Branch: implPayload.Branch, TaskID: implPayload.TaskID, LeadAgent: "coder"})
	executor := &stubPipelineAutoMerger{readiness: workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: head}, mergeResult: workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "merged-815"}}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second), executor)
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, defaultJobWorker(store, io.Discard, home), 1, "", io.Discard, now.Add(2*time.Second), nil, nil); err != nil {
		t.Fatalf("review worker tick: %v", err)
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(3*time.Second), executor)
	if run.State != pipeline.RunSucceeded || len(executor.mergeReqs) != 1 {
		t.Fatalf("shell E2E run=%+v merge calls=%d", run, len(executor.mergeReqs))
	}
	if got := executor.mergeReqs[0]; got.PullRequest != 815 || got.HeadSHA != head {
		t.Fatalf("shell E2E merge request = %+v, want PR 815 head %s", got, head)
	}
}
