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

// TestPipelineScanDoesNotSettleSpecDriftedLiveStages pins the destructive
// polarity of the terminal-only reconciler. A drifted run may be inspected for
// terminal evidence, but a queued or running stage backed by a live job must
// remain in flight rather than falling through to a false successful settlement.
func TestPipelineScanDoesNotSettleSpecDriftedLiveStages(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stageState string
		jobState   workflow.JobState
	}{
		{name: "running", stageState: StageRunning, jobState: workflow.JobRunning},
		{name: "queued", stageState: StageQueued, jobState: workflow.JobQueued},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := pipelineAdvanceStore(t)
			pipelineName := "drifted-live-" + tc.name
			runID := "prun-drifted-live-" + tc.name
			jobID := runID + "-work-a0"
			startedAt := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
			scanAt := startedAt.Add(5 * time.Minute)

			if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{
				Name:       pipelineName,
				Repo:       "owner/repo",
				SpecYAML:   dreamingLiveCurrentSpec,
				SpecHash:   "current-" + tc.name,
				Enabled:    false,
				LastRunID:  runID,
				LastStatus: RunRunning,
			}); err != nil {
				t.Fatalf("CreateOrUpdatePipeline: %v", err)
			}
			if err := store.UpdatePipelineLastRun(ctx, pipelineName, runID, RunRunning, startedAt); err != nil {
				t.Fatalf("UpdatePipelineLastRun: %v", err)
			}
			if err := store.CreatePipelineRun(ctx, db.PipelineRun{
				ID:          runID,
				Pipeline:    pipelineName,
				Trigger:     "schedule",
				PayloadJSON: "{}",
				SpecHash:    "snapshot-" + tc.name,
				State:       RunRunning,
				StartedAt:   startedAt,
			}); err != nil {
				t.Fatalf("CreatePipelineRun: %v", err)
			}
			if err := store.CreatePipelineRunStage(ctx, db.PipelineRunStage{
				RunID:     runID,
				StageID:   "work",
				State:     tc.stageState,
				JobID:     jobID,
				StartedAt: startedAt,
			}); err != nil {
				t.Fatalf("CreatePipelineRunStage: %v", err)
			}
			if err := store.CreateJobWithEvent(ctx, db.Job{
				ID:      jobID,
				Agent:   "worker",
				Type:    "ask",
				State:   string(tc.jobState),
				Payload: `{"repo":"owner/repo","sender":"pipeline","root_job_id":"` + runID + `"}`,
			}, db.JobEvent{JobID: jobID, Kind: string(tc.jobState), Message: "live job"}); err != nil {
				t.Fatalf("CreateJobWithEvent: %v", err)
			}

			if err := RunPipelineScanOnce(ctx, store, testStageEnqueuer(store), scanAt); err != nil {
				t.Fatalf("RunPipelineScanOnce: %v", err)
			}
			gotRun, ok, err := store.GetPipelineRun(ctx, runID)
			if err != nil || !ok {
				t.Fatalf("GetPipelineRun(%s): ok=%v err=%v", runID, ok, err)
			}
			if gotRun.State != RunRunning || !gotRun.FinishedAt.IsZero() {
				t.Fatalf("spec-drifted %s stage settled run = {state=%q finished_at=%s}, want running with no finish", tc.stageState, gotRun.State, gotRun.FinishedAt)
			}
			if gotStage := stageRow(t, store, runID, "work"); gotStage.State != tc.stageState {
				t.Fatalf("spec-drifted stage state = %q, want %q", gotStage.State, tc.stageState)
			}
			gotPipeline, ok, err := store.GetPipeline(ctx, pipelineName)
			if err != nil || !ok {
				t.Fatalf("GetPipeline(%s): ok=%v err=%v", pipelineName, ok, err)
			}
			if gotPipeline.LastStatus != RunRunning {
				t.Fatalf("spec-drifted pipeline last_status = %q, want %q", gotPipeline.LastStatus, RunRunning)
			}
		})
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
