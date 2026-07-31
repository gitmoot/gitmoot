package pipeline

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const dreamingLiveCurrentSpec = `name: jarvis-dreaming
repo: gitmoot/jarvis
stages:
  - id: dream
    agent: jarvis-dreamer
    action: ask
    prompt: consolidate pending memories
`

// TestPipelineScanReconcilesLiveCancelledRunAcrossSpecDrift mirrors the wedged
// prun-jarvis-dreaming-18c601fe55793258 row family observed on 2026-07-31. The
// pipeline definition changed after the run started, but the terminal cancelled
// job remains authoritative evidence that its one running stage cannot progress.
func TestPipelineScanReconcilesLiveCancelledRunAcrossSpecDrift(t *testing.T) {
	const (
		pipelineName = "jarvis-dreaming"
		runID        = "prun-jarvis-dreaming-18c601fe55793258"
		jobID        = runID + "-dream-a0"
		currentHash  = "bbda059fca5a070c2fef99959bff3de237d49efe21060e88e5e7f6e0b7d47c1d"
		runHash      = "22537b6057e198f7b72ae75f0b3f9914b9f117d972cfb564b2adb80e88c654e8"
	)
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	runStarted := time.Date(2026, 7, 27, 1, 41, 34, 166307416, time.UTC)
	stageStarted := time.Date(2026, 7, 27, 1, 41, 35, 0, time.UTC)
	jobCancelled := time.Date(2026, 7, 31, 3, 31, 4, 0, time.UTC)
	scanAt := time.Date(2026, 7, 31, 8, 45, 6, 0, time.UTC)

	rec := db.Pipeline{
		Name:       pipelineName,
		Repo:       "gitmoot/jarvis",
		SpecYAML:   dreamingLiveCurrentSpec,
		SpecHash:   currentHash,
		Enabled:    true,
		Interval:   "24h",
		Jitter:     "30m",
		LastRunID:  runID,
		LastStatus: RunRunning,
	}
	if err := store.CreateOrUpdatePipeline(ctx, rec); err != nil {
		t.Fatalf("CreateOrUpdatePipeline: %v", err)
	}
	if err := store.UpdatePipelineLastRun(ctx, pipelineName, runID, RunRunning, runStarted); err != nil {
		t.Fatalf("UpdatePipelineLastRun: %v", err)
	}
	if err := store.AdvancePipelineNextDue(ctx, pipelineName, time.Date(2026, 7, 28, 1, 53, 48, 795871322, time.UTC)); err != nil {
		t.Fatalf("AdvancePipelineNextDue: %v", err)
	}
	run := db.PipelineRun{
		ID:          runID,
		Pipeline:    pipelineName,
		Trigger:     "schedule",
		PayloadJSON: "{}",
		SpecHash:    runHash,
		State:       RunRunning,
		StartedAt:   runStarted,
	}
	if err := store.CreatePipelineRun(ctx, run); err != nil {
		t.Fatalf("CreatePipelineRun: %v", err)
	}
	stage := db.PipelineRunStage{
		RunID:     runID,
		StageID:   "dream",
		State:     StageRunning,
		JobID:     jobID,
		Attempt:   0,
		StartedAt: stageStarted,
	}
	if err := store.CreatePipelineRunStage(ctx, stage); err != nil {
		t.Fatalf("CreatePipelineRunStage: %v", err)
	}
	payload := `{"repo":"gitmoot/jarvis","sender":"pipeline","root_job_id":"` + runID + `","job_timeout":"40m","result":{"decision":"blocked","summary":"pending memories cannot be retired"}}`
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      jobID,
		Agent:   "jarvis-dreamer",
		Type:    "ask",
		State:   string(workflow.JobCancelled),
		Payload: payload,
		Model:   "gpt-5.6-sol",
	}, db.JobEvent{JobID: jobID, Kind: string(workflow.JobCancelled), Message: "cancel requested from blocked"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	setDreamingLiveFixtureTimes(t, store, jobID, runStarted, jobCancelled)

	if age := scanAt.Sub(stageStarted); age < 103*time.Hour {
		t.Fatalf("fixture stage age = %s, want at least 103h", age)
	}
	if err := RunPipelineScanOnce(ctx, store, testStageEnqueuer(store), scanAt); err != nil {
		t.Fatalf("RunPipelineScanOnce: %v", err)
	}

	gotRun, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetPipelineRun(%s): ok=%v err=%v", runID, ok, err)
	}
	if gotRun.State != RunFailed {
		t.Fatalf("live-shaped reconciled run state = %q, want %q (pipeline hash=%s run hash=%s)", gotRun.State, RunFailed, currentHash, runHash)
	}
	gotStage := stageRow(t, store, runID, "dream")
	if gotStage.State != StageFailed || gotStage.Summary != "stage job cancelled" {
		t.Fatalf("live-shaped reconciled stage = {state=%q summary=%q}, want failed cancellation", gotStage.State, gotStage.Summary)
	}
	gotPipeline, ok, err := store.GetPipeline(ctx, pipelineName)
	if err != nil || !ok {
		t.Fatalf("GetPipeline(%s): ok=%v err=%v", pipelineName, ok, err)
	}
	if gotPipeline.LastStatus != RunFailed {
		t.Fatalf("live-shaped pipeline last_status = %q, want %q", gotPipeline.LastStatus, RunFailed)
	}
}

func setDreamingLiveFixtureTimes(t *testing.T, store *db.Store, jobID string, createdAt, cancelledAt time.Time) {
	t.Helper()
	conn, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`,
		createdAt.UTC().Format(time.RFC3339Nano), cancelledAt.UTC().Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatalf("UPDATE live fixture job times: %v", err)
	}
	if _, err := conn.Exec(`UPDATE job_events SET created_at = ? WHERE job_id = ? AND kind = ?`,
		cancelledAt.UTC().Format(time.RFC3339Nano), jobID, string(workflow.JobCancelled)); err != nil {
		t.Fatalf("UPDATE live fixture cancellation time: %v", err)
	}
}
