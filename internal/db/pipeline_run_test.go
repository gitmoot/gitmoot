package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleRun(id string) PipelineRun {
	return PipelineRun{
		ID:          id,
		Pipeline:    "deploy-flow",
		Trigger:     "manual",
		PayloadJSON: `{"body":"first line\n第二行 ☃"}`,
		SpecHash:    "hash-1",
		State:       "running",
		StartedAt:   time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC),
	}
}

// TestPipelineRunCRUD proves the run row round-trips and its mutable advancement
// fields update in place while identity stays fixed.
func TestPipelineRunCRUD(t *testing.T) {
	store := openPipelineStore(t)
	ctx := context.Background()

	if _, ok, err := store.GetPipelineRun(ctx, "prun-x"); err != nil || ok {
		t.Fatalf("expected no run yet: ok=%v err=%v", ok, err)
	}
	run := sampleRun("prun-1")
	if err := store.CreatePipelineRun(ctx, run); err != nil {
		t.Fatalf("CreatePipelineRun: %v", err)
	}
	// Duplicate id is rejected.
	if err := store.CreatePipelineRun(ctx, run); err == nil {
		t.Fatalf("expected duplicate run id to error")
	}

	got, ok, err := store.GetPipelineRun(ctx, "prun-1")
	if err != nil || !ok {
		t.Fatalf("GetPipelineRun: ok=%v err=%v", ok, err)
	}
	if got.Pipeline != "deploy-flow" || got.Trigger != "manual" || got.PayloadJSON != run.PayloadJSON || got.SpecHash != "hash-1" || got.State != "running" {
		t.Fatalf("run roundtrip mismatch: %+v", got)
	}
	if !got.StartedAt.Equal(run.StartedAt) || !got.FinishedAt.IsZero() {
		t.Fatalf("run times mismatch: %+v", got)
	}

	// Park it: state + halt + needs + finished update; identity is untouched.
	finished := time.Date(2026, 7, 6, 9, 5, 0, 0, time.UTC)
	got.State = "blocked"
	got.HaltStage = "score"
	got.HaltReason = "needs a secret"
	got.NeedsJSON = `["R2 token"]`
	got.PayloadJSON = `{"body":"must not replace snapshot"}`
	got.FinishedAt = finished
	if err := store.UpdatePipelineRun(ctx, got); err != nil {
		t.Fatalf("UpdatePipelineRun: %v", err)
	}
	reloaded, _, _ := store.GetPipelineRun(ctx, "prun-1")
	if reloaded.State != "blocked" || reloaded.HaltStage != "score" || reloaded.NeedsJSON != `["R2 token"]` {
		t.Fatalf("park state mismatch: %+v", reloaded)
	}
	if !reloaded.FinishedAt.Equal(finished) {
		t.Fatalf("finished_at mismatch: %+v", reloaded)
	}
	if reloaded.Pipeline != "deploy-flow" || reloaded.SpecHash != "hash-1" || !reloaded.StartedAt.Equal(run.StartedAt) {
		t.Fatalf("update disturbed run identity: %+v", reloaded)
	}
	if reloaded.PayloadJSON != run.PayloadJSON {
		t.Fatalf("update disturbed immutable payload snapshot: %q", reloaded.PayloadJSON)
	}

	if err := store.UpdatePipelineRun(ctx, PipelineRun{ID: "prun-missing", State: "failed"}); err == nil {
		t.Fatalf("expected UpdatePipelineRun on missing id to error")
	}
}

func TestPipelineRunPayloadMigrationUpgrade(t *testing.T) {
	ctx := context.Background()
	payloadMigration := -1
	for i, migration := range migrations {
		if strings.Contains(migration, "ADD COLUMN payload_json") {
			payloadMigration = i
			break
		}
	}
	if payloadMigration < 0 {
		t.Fatal("payload_json migration not found")
	}
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db: raw, path: path}
	t.Cleanup(func() { _ = raw.Close() })
	for version, migration := range migrations[:payloadMigration] {
		if err := store.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("apply pre-payload migration %d: %v", version+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO pipeline_runs(id, pipeline, trigger, spec_hash, state) VALUES ('prun-legacy', 'mail', 'manual', 'hash', 'running')`); err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}
	if err := store.applyMigration(ctx, payloadMigration+1, migrations[payloadMigration]); err != nil {
		t.Fatalf("apply payload migration: %v", err)
	}
	legacy, ok, err := store.GetPipelineRun(ctx, "prun-legacy")
	if err != nil || !ok || legacy.PayloadJSON != "{}" {
		t.Fatalf("legacy payload = %q ok=%v err=%v", legacy.PayloadJSON, ok, err)
	}
	mapped := sampleRun("prun-mapped")
	mapped.Pipeline = "mail"
	mapped.PayloadJSON = `{"body":"hello\n世界","subject":"☃ status"}`
	if err := store.CreatePipelineRun(ctx, mapped); err != nil {
		t.Fatalf("create upgraded mapped run: %v", err)
	}
	got, ok, err := store.GetPipelineRun(ctx, mapped.ID)
	if err != nil || !ok || got.PayloadJSON != mapped.PayloadJSON {
		t.Fatalf("upgraded mapped payload = %q ok=%v err=%v", got.PayloadJSON, ok, err)
	}
}

// TestPipelineRunListingAndActive proves the listing / overlap-guard queries: runs
// list newest-first per pipeline, ActivePipelineRun returns only a running run, and
// ListActivePipelineRuns is the advancer's cross-pipeline running-run scan.
func TestPipelineRunListingAndActive(t *testing.T) {
	store := openPipelineStore(t)
	ctx := context.Background()

	older := sampleRun("prun-old")
	older.StartedAt = time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	older.State = "succeeded"
	newer := sampleRun("prun-new")
	newer.StartedAt = time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	newer.PayloadJSON = `{"which":"new"}`
	other := sampleRun("prun-other")
	other.Pipeline = "other-flow"
	other.PayloadJSON = `{"which":"other"}`
	for _, r := range []PipelineRun{older, newer, other} {
		if err := store.CreatePipelineRun(ctx, r); err != nil {
			t.Fatalf("CreatePipelineRun(%s): %v", r.ID, err)
		}
	}

	runs, err := store.ListPipelineRuns(ctx, "deploy-flow")
	if err != nil || len(runs) != 2 {
		t.Fatalf("ListPipelineRuns = %+v err=%v", runs, err)
	}
	if runs[0].ID != "prun-new" || runs[0].PayloadJSON != newer.PayloadJSON {
		t.Fatalf("ListPipelineRuns not newest-first: %+v", runs)
	}

	active, ok, err := store.ActivePipelineRun(ctx, "deploy-flow")
	if err != nil || !ok || active.ID != "prun-new" || active.PayloadJSON != newer.PayloadJSON {
		t.Fatalf("ActivePipelineRun = %+v ok=%v err=%v (want prun-new)", active, ok, err)
	}

	all, err := store.ListActivePipelineRuns(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListActivePipelineRuns = %+v err=%v (want the 2 running runs)", all, err)
	}
	for _, run := range all {
		if run.ID == other.ID && run.PayloadJSON != other.PayloadJSON {
			t.Fatalf("ListActivePipelineRuns lost payload snapshot: %+v", run)
		}
	}

	// Once the running run settles, the overlap guard clears.
	newer.State = "failed"
	if err := store.UpdatePipelineRun(ctx, newer); err != nil {
		t.Fatalf("UpdatePipelineRun: %v", err)
	}
	if _, ok, _ := store.ActivePipelineRun(ctx, "deploy-flow"); ok {
		t.Fatalf("ActivePipelineRun should be empty after settle")
	}
}

// TestPipelineRunStageCRUD proves stage rows round-trip, update in place by
// (run_id, stage_id), and list scoped to the run.
func TestPipelineRunStageCRUD(t *testing.T) {
	store := openPipelineStore(t)
	ctx := context.Background()
	if err := store.CreatePipelineRun(ctx, sampleRun("prun-1")); err != nil {
		t.Fatalf("CreatePipelineRun: %v", err)
	}

	for _, id := range []string{"source", "score", "deploy"} {
		if err := store.CreatePipelineRunStage(ctx, PipelineRunStage{RunID: "prun-1", StageID: id, State: "pending"}); err != nil {
			t.Fatalf("CreatePipelineRunStage(%s): %v", id, err)
		}
	}
	// A run's stages are scoped to it; a decoy run's stage must not leak.
	if err := store.CreatePipelineRun(ctx, sampleRun("prun-2")); err != nil {
		t.Fatalf("CreatePipelineRun 2: %v", err)
	}
	if err := store.CreatePipelineRunStage(ctx, PipelineRunStage{RunID: "prun-2", StageID: "source", State: "pending"}); err != nil {
		t.Fatalf("CreatePipelineRunStage decoy: %v", err)
	}

	stages, err := store.ListPipelineRunStages(ctx, "prun-1")
	if err != nil || len(stages) != 3 {
		t.Fatalf("ListPipelineRunStages = %+v err=%v", stages, err)
	}
	// Ordered by stage_id.
	if stages[0].StageID != "deploy" || stages[1].StageID != "score" || stages[2].StageID != "source" {
		t.Fatalf("stages not ordered by id: %+v", stages)
	}

	started := time.Date(2026, 7, 6, 9, 1, 0, 0, time.UTC)
	update := PipelineRunStage{RunID: "prun-1", StageID: "score", State: "blocked", JobID: "prun-1-score-a0", Attempt: 2, NeedsJSON: `["R2 token"]`, Summary: "blocked on secret", StartedAt: started}
	if err := store.UpdatePipelineRunStage(ctx, update); err != nil {
		t.Fatalf("UpdatePipelineRunStage: %v", err)
	}
	got, ok, err := store.GetPipelineRunStage(ctx, "prun-1", "score")
	if err != nil || !ok {
		t.Fatalf("GetPipelineRunStage: ok=%v err=%v", ok, err)
	}
	if got.State != "blocked" || got.JobID != "prun-1-score-a0" || got.Attempt != 2 || got.NeedsJSON != `["R2 token"]` || got.Summary != "blocked on secret" {
		t.Fatalf("stage update mismatch: %+v", got)
	}
	if !got.StartedAt.Equal(started) {
		t.Fatalf("stage started_at mismatch: %+v", got)
	}

	if err := store.UpdatePipelineRunStage(ctx, PipelineRunStage{RunID: "prun-1", StageID: "missing", State: "failed"}); err == nil {
		t.Fatalf("expected UpdatePipelineRunStage on missing stage to error")
	}
}

// TestUpdatePipelineLastRun proves the last-run bookkeeping updates without
// disturbing the schedule's next_due or the spec, and tolerates a missing pipeline.
func TestUpdatePipelineLastRun(t *testing.T) {
	store := openPipelineStore(t)
	ctx := context.Background()
	if err := store.CreateOrUpdatePipeline(ctx, samplePipeline()); err != nil {
		t.Fatalf("CreateOrUpdatePipeline: %v", err)
	}
	// Seed a next_due so we can prove it is preserved.
	due := time.Date(2026, 7, 7, 9, 0, 0, 0, time.UTC)
	if err := store.UpdatePipelineScheduleState(ctx, PipelineScheduleState{Name: "deploy-flow", NextDueAt: due, LastStatus: "old"}); err != nil {
		t.Fatalf("UpdatePipelineScheduleState: %v", err)
	}

	started := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	if err := store.UpdatePipelineLastRun(ctx, "deploy-flow", "prun-1", "running", started); err != nil {
		t.Fatalf("UpdatePipelineLastRun (start): %v", err)
	}
	got, _, _ := store.GetPipeline(ctx, "deploy-flow")
	if got.LastRunID != "prun-1" || got.LastStatus != "running" || !got.LastRunAt.Equal(started) {
		t.Fatalf("start bookkeeping mismatch: %+v", got)
	}
	if !got.NextDueAt.Equal(due) || got.SpecHash != "abc123" {
		t.Fatalf("last-run update clobbered schedule/spec: %+v", got)
	}

	// Terminal update (zero at) leaves last_run_at untouched.
	if err := store.UpdatePipelineLastRun(ctx, "deploy-flow", "prun-1", "blocked", time.Time{}); err != nil {
		t.Fatalf("UpdatePipelineLastRun (terminal): %v", err)
	}
	got, _, _ = store.GetPipeline(ctx, "deploy-flow")
	if got.LastStatus != "blocked" || !got.LastRunAt.Equal(started) || !got.NextDueAt.Equal(due) {
		t.Fatalf("terminal bookkeeping mismatch: %+v", got)
	}

	// A run outlives a removed pipeline: no error on a missing row.
	if err := store.UpdatePipelineLastRun(ctx, "gone", "prun-1", "failed", time.Time{}); err != nil {
		t.Fatalf("UpdatePipelineLastRun on missing pipeline should be a no-op nil: %v", err)
	}
}
