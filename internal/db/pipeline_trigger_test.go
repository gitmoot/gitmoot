package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPipelineTriggerStateMigrationAndRoundTrip(t *testing.T) {
	found := false
	for _, migration := range migrations {
		if strings.Contains(migration, "CREATE TABLE pipeline_trigger_states") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("pipeline_trigger_states migration not found")
	}

	store := openPipelineStore(t)
	ctx := context.Background()
	armed := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	if err := store.ArmPipelineTrigger(ctx, "downstream", "upstream", armed); err != nil {
		t.Fatalf("ArmPipelineTrigger: %v", err)
	}
	state, ok, err := store.GetPipelineTriggerState(ctx, "downstream")
	if err != nil || !ok {
		t.Fatalf("GetPipelineTriggerState: ok=%v err=%v", ok, err)
	}
	if state.Downstream != "downstream" || state.Upstream != "upstream" || state.Cursor != "" || !state.ArmedAt.Equal(armed) {
		t.Fatalf("state round-trip = %+v", state)
	}
	if err := store.DeletePipelineTriggerState(ctx, "downstream"); err != nil {
		t.Fatalf("DeletePipelineTriggerState: %v", err)
	}
	if _, ok, err := store.GetPipelineTriggerState(ctx, "downstream"); err != nil || ok {
		t.Fatalf("state remained after delete: ok=%v err=%v", ok, err)
	}
}

func TestFirePipelineTriggerIsAtomicWithCursor(t *testing.T) {
	store := openPipelineStore(t)
	ctx := context.Background()
	upstreamSpec := "name: upstream\nstages: [{id: run, cmd: echo}]\n"
	downstreamSpec := "name: downstream\nrepo: owner/downstream\ntrigger: {kind: pipeline, pipeline: upstream}\nstages: [{id: one, cmd: echo}, {id: two, cmd: echo}]\n"
	for _, rec := range []Pipeline{
		{Name: "upstream", SpecYAML: upstreamSpec, SpecHash: "up-hash", Enabled: true},
		{Name: "downstream", Repo: "owner/downstream", SpecYAML: downstreamSpec, SpecHash: "down-hash", Enabled: true},
	} {
		if err := store.CreateOrUpdatePipeline(ctx, rec); err != nil {
			t.Fatalf("CreateOrUpdatePipeline %s: %v", rec.Name, err)
		}
	}
	base := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	old := PipelineRun{ID: "prun-up-old", Pipeline: "upstream", State: "succeeded", StartedAt: base}
	if err := store.CreatePipelineRun(ctx, old); err != nil {
		t.Fatalf("create old upstream run: %v", err)
	}
	if err := store.ArmPipelineTrigger(ctx, "downstream", "upstream", base.Add(time.Minute)); err != nil {
		t.Fatalf("arm: %v", err)
	}
	state, _, _ := store.GetPipelineTriggerState(ctx, "downstream")
	if state.Cursor != old.ID {
		t.Fatalf("arm cursor = %q, want %q", state.Cursor, old.ID)
	}
	upstream := PipelineRun{ID: "prun-up-new", Pipeline: "upstream", State: "succeeded", StartedAt: base.Add(2 * time.Minute)}
	if err := store.CreatePipelineRun(ctx, upstream); err != nil {
		t.Fatalf("create new upstream run: %v", err)
	}
	next, ok, err := store.NextSucceededPipelineTriggerRun(ctx, state)
	if err != nil || !ok || next.ID != upstream.ID {
		t.Fatalf("NextSucceededPipelineTriggerRun = %+v ok=%v err=%v", next, ok, err)
	}
	downstream := PipelineRun{ID: "prun-down-ok", Pipeline: "downstream", Trigger: "pipeline", PayloadJSON: `{"upstream_pipeline":"upstream","upstream_run_id":"prun-up-new"}`, SpecHash: "down-hash", State: "running", StartedAt: base.Add(3 * time.Minute)}
	fired, err := store.FirePipelineTrigger(ctx, state, upstream, downstream, []PipelineRunStage{{StageID: "one", State: "pending"}, {StageID: "two", State: "pending"}})
	if err != nil || !fired {
		t.Fatalf("FirePipelineTrigger: fired=%v err=%v", fired, err)
	}
	state, _, _ = store.GetPipelineTriggerState(ctx, "downstream")
	if state.Cursor != upstream.ID {
		t.Fatalf("cursor = %q, want %q", state.Cursor, upstream.ID)
	}
	created, ok, err := store.GetPipelineRun(ctx, downstream.ID)
	if err != nil || !ok || created.Trigger != "pipeline" || created.PayloadJSON != downstream.PayloadJSON {
		t.Fatalf("created downstream run = %+v ok=%v err=%v", created, ok, err)
	}
	stages, err := store.ListPipelineRunStages(ctx, downstream.ID)
	if err != nil || len(stages) != 2 {
		t.Fatalf("downstream stages = %+v err=%v", stages, err)
	}
	rec, _, _ := store.GetPipeline(ctx, "downstream")
	if rec.LastRunID != downstream.ID || rec.LastStatus != "running" {
		t.Fatalf("last-run bookkeeping = %+v", rec)
	}

	// Re-arm past the successful fire, then prove a stage insert failure rolls
	// back both the run insert and cursor movement (the crash-window invariant).
	if err := store.UpdatePipelineRun(ctx, PipelineRun{ID: downstream.ID, State: "succeeded", FinishedAt: base.Add(4 * time.Minute)}); err != nil {
		t.Fatalf("settle downstream: %v", err)
	}
	state, _, _ = store.GetPipelineTriggerState(ctx, "downstream")
	upstream2 := PipelineRun{ID: "prun-up-next", Pipeline: "upstream", State: "succeeded", StartedAt: base.Add(5 * time.Minute)}
	if err := store.CreatePipelineRun(ctx, upstream2); err != nil {
		t.Fatalf("create second upstream run: %v", err)
	}
	badRun := PipelineRun{ID: "prun-down-rollback", Pipeline: "downstream", Trigger: "pipeline", State: "running", StartedAt: base.Add(6 * time.Minute)}
	fired, err = store.FirePipelineTrigger(ctx, state, upstream2, badRun, []PipelineRunStage{{StageID: "duplicate"}, {StageID: "duplicate"}})
	if err == nil || fired {
		t.Fatalf("duplicate-stage fire = fired=%v err=%v, want rollback error", fired, err)
	}
	if _, ok, err := store.GetPipelineRun(ctx, badRun.ID); err != nil || ok {
		t.Fatalf("rolled-back run exists: ok=%v err=%v", ok, err)
	}
	after, _, _ := store.GetPipelineTriggerState(ctx, "downstream")
	if after.Cursor != state.Cursor {
		t.Fatalf("cursor advanced on rolled-back fire: before=%q after=%q", state.Cursor, after.Cursor)
	}
}
