package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openPipelineStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func samplePipeline() Pipeline {
	return Pipeline{
		Name:     "deploy-flow",
		Repo:     "jerryfane/gitmoot",
		SpecYAML: "name: deploy-flow\nstages:\n  - {id: a, cmd: echo}\n",
		SpecHash: "abc123",
		Interval: "24h",
		Jitter:   "15m",
	}
}

// TestPipelineFreshCRUD proves the full CRUD round-trip on a fresh (fully
// migrated) DB: create, read back, list, toggle enabled, persist schedule state,
// and delete.
func TestPipelineFreshCRUD(t *testing.T) {
	store := openPipelineStore(t)
	ctx := context.Background()

	if _, ok, err := store.GetPipeline(ctx, "deploy-flow"); err != nil || ok {
		t.Fatalf("expected no pipeline yet, ok=%v err=%v", ok, err)
	}
	if err := store.CreateOrUpdatePipeline(ctx, samplePipeline()); err != nil {
		t.Fatalf("CreateOrUpdatePipeline: %v", err)
	}
	got, ok, err := store.GetPipeline(ctx, "deploy-flow")
	if err != nil || !ok {
		t.Fatalf("GetPipeline: ok=%v err=%v", ok, err)
	}
	if got.Repo != "jerryfane/gitmoot" || got.SpecHash != "abc123" || got.Interval != "24h" || got.Jitter != "15m" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Enabled {
		t.Fatalf("new pipeline should default disabled")
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not populated: %+v", got)
	}

	list, err := store.ListPipelines(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "deploy-flow" {
		t.Fatalf("ListPipelines = %+v err=%v", list, err)
	}

	if err := store.SetPipelineEnabled(ctx, "deploy-flow", true); err != nil {
		t.Fatalf("SetPipelineEnabled: %v", err)
	}
	if got, _, _ := store.GetPipeline(ctx, "deploy-flow"); !got.Enabled {
		t.Fatalf("expected enabled after SetPipelineEnabled(true)")
	}

	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	due := now.Add(24 * time.Hour)
	if err := store.UpdatePipelineScheduleState(ctx, PipelineScheduleState{
		Name: "deploy-flow", LastRunAt: now, NextDueAt: due, LastRunID: "run-1", LastStatus: "succeeded",
	}); err != nil {
		t.Fatalf("UpdatePipelineScheduleState: %v", err)
	}
	got, _, _ = store.GetPipeline(ctx, "deploy-flow")
	if !got.LastRunAt.Equal(now) || !got.NextDueAt.Equal(due) || got.LastRunID != "run-1" || got.LastStatus != "succeeded" {
		t.Fatalf("schedule state mismatch: %+v", got)
	}
	// Schedule-state update must not disturb the spec or enabled flag.
	if !got.Enabled || got.SpecHash != "abc123" {
		t.Fatalf("schedule update clobbered spec/enabled: %+v", got)
	}

	deleted, err := store.DeletePipeline(ctx, "deploy-flow")
	if err != nil || !deleted {
		t.Fatalf("DeletePipeline: deleted=%v err=%v", deleted, err)
	}
	if _, ok, _ := store.GetPipeline(ctx, "deploy-flow"); ok {
		t.Fatalf("pipeline still present after delete")
	}
	if deleted, _ := store.DeletePipeline(ctx, "deploy-flow"); deleted {
		t.Fatalf("second delete reported a row")
	}
}

// TestCreateOrUpdatePipelinePreservesEnabledAndState proves re-adding an edited
// spec replaces the spec fields but preserves the enabled flag and the durable
// schedule-state bookkeeping — the "a run executes its snapshot; toggling and
// last-run state survive an edit" contract.
func TestCreateOrUpdatePipelinePreservesEnabledAndState(t *testing.T) {
	store := openPipelineStore(t)
	ctx := context.Background()
	if err := store.CreateOrUpdatePipeline(ctx, samplePipeline()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetPipelineEnabled(ctx, "deploy-flow", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	now := time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)
	if err := store.UpdatePipelineScheduleState(ctx, PipelineScheduleState{
		Name: "deploy-flow", LastRunAt: now, LastRunID: "run-9", LastStatus: "blocked",
	}); err != nil {
		t.Fatalf("schedule state: %v", err)
	}

	edited := samplePipeline()
	edited.SpecYAML = "name: deploy-flow\nstages:\n  - {id: a, cmd: echo2}\n"
	edited.SpecHash = "def456"
	edited.Interval = "12h"
	if err := store.CreateOrUpdatePipeline(ctx, edited); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	got, _, _ := store.GetPipeline(ctx, "deploy-flow")
	if got.SpecHash != "def456" || got.Interval != "12h" {
		t.Fatalf("spec fields not updated on re-add: %+v", got)
	}
	if !got.Enabled {
		t.Fatalf("re-add clobbered the enabled flag")
	}
	if got.LastRunID != "run-9" || got.LastStatus != "blocked" || !got.LastRunAt.Equal(now) {
		t.Fatalf("re-add clobbered schedule state: %+v", got)
	}
}

func TestPipelineNotFoundErrors(t *testing.T) {
	store := openPipelineStore(t)
	ctx := context.Background()
	if err := store.SetPipelineEnabled(ctx, "ghost", true); err == nil {
		t.Fatalf("SetPipelineEnabled on missing pipeline should error")
	}
	if err := store.UpdatePipelineScheduleState(ctx, PipelineScheduleState{Name: "ghost"}); err == nil {
		t.Fatalf("UpdatePipelineScheduleState on missing pipeline should error")
	}
}

// TestMigrateAddsPipelinesToUpgradedDB proves the pipelines table is a clean
// additive append: an existing DB migrated up to (but not including) the
// pipelines migration, carrying a legacy row in a prior table, gains a working
// pipelines table on the next Open while the legacy row still reads back.
func TestMigrateAddsPipelinesToUpgradedDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	store := &Store{db: raw}
	// Apply every migration BEFORE the pipelines table. Located by content so this
	// stays correct as later steps append further migrations (the per-run/per-stage
	// tables) after the pipelines one.
	pipelinesIdx := -1
	for i, m := range migrations {
		if strings.Contains(m, "CREATE TABLE pipelines") {
			pipelinesIdx = i
			break
		}
	}
	if pipelinesIdx < 0 {
		t.Fatalf("pipelines migration not found")
	}
	for version, migration := range migrations[:pipelinesIdx] {
		if err := store.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	// The pipelines table must not exist yet.
	if _, err := raw.ExecContext(ctx, `SELECT name FROM pipelines`); err == nil {
		t.Fatalf("pipelines table exists before its migration")
	}
	// Seed a legacy row in a long-standing table.
	if _, err := raw.ExecContext(ctx, `INSERT INTO agents(name, role, runtime, runtime_ref, repo_scope)
		VALUES ('legacy', 'planner', 'codex', '', '')`); err != nil {
		t.Fatalf("insert legacy agent: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("Open upgraded DB: %v", err)
	}
	defer upgraded.Close()

	// The legacy row survives.
	if _, err := upgraded.GetAgent(ctx, "legacy"); err != nil {
		t.Fatalf("legacy agent lost after upgrade: %v", err)
	}
	// The freshly migrated pipelines table works.
	if err := upgraded.CreateOrUpdatePipeline(ctx, samplePipeline()); err != nil {
		t.Fatalf("CreateOrUpdatePipeline on upgraded DB: %v", err)
	}
	if _, ok, err := upgraded.GetPipeline(ctx, "deploy-flow"); err != nil || !ok {
		t.Fatalf("GetPipeline on upgraded DB: ok=%v err=%v", ok, err)
	}
}
