package pipeline

import (
	"context"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
	"strings"
	"testing"
	"time"
)

// parkBlockedChainRun drives the linear a->b->c chain to a parked-blocked run: a
// succeeds, b returns blocked, c is skipped, run parks blocked. It returns the
// parked run for the resume/cancel tests to act on.
func parkBlockedChainRun(t *testing.T, store *db.Store, enqueue PipelineStageEnqueuer, rec db.Pipeline, spec Spec, now time.Time) db.PipelineRun {
	t.Helper()
	run := startTestRun(t, store, rec, spec, enqueue, now)
	settleStageJob(t, store, stageRow(t, store, run.ID, "a").JobID, "approved", "a ok", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	settleStageJob(t, store, stageRow(t, store, run.ID, "b").JobID, "blocked", "needs secret", []string{"R2 token"})
	run = advance(t, store, rec, spec, enqueue, run, now)
	if run.State != RunBlocked {
		t.Fatalf("precondition: run = %s, want blocked", run.State)
	}
	return run
}

// TestResumeResetsHaltedStageAndDependents proves resume of a parked-blocked run
// resets the halted stage AND its transitive dependents to pending (attempt bumped,
// job cleared), leaves the already-succeeded upstream stage untouched, clears the
// run's park fields, and lets the next advance re-enqueue the halted stage.
func TestResumeResetsHaltedStageAndDependents(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := parkBlockedChainRun(t, store, enqueue, rec, spec, now)
	bBlockedJob := stageRow(t, store, run.ID, "b").JobID
	aBefore := stageRow(t, store, run.ID, "a")

	resumed, err := ResumePipelineRun(ctx, store, run.ID, "")
	if err != nil {
		t.Fatalf("ResumePipelineRun: %v", err)
	}
	if resumed.State != RunRunning {
		t.Fatalf("resumed run = %s, want running", resumed.State)
	}
	if resumed.HaltStage != "" || resumed.HaltReason != "" || resumed.NeedsJSON != "" {
		t.Fatalf("resumed run park fields not cleared: %+v", resumed)
	}

	// a (succeeded) is untouched: same state, same attempt, same job.
	a := stageRow(t, store, run.ID, "a")
	if a.State != StageSucceeded || a.Attempt != aBefore.Attempt || a.JobID != aBefore.JobID {
		t.Fatalf("succeeded stage a changed on resume: before=%+v after=%+v", aBefore, a)
	}
	// b (halted) reset to pending, attempt bumped, job cleared, needs cleared.
	b := stageRow(t, store, run.ID, "b")
	if b.State != StagePending || b.Attempt != 1 || b.JobID != "" || b.NeedsJSON != "" {
		t.Fatalf("halted stage b not reset: %+v", b)
	}
	// c (skipped dependent) reset to pending, attempt bumped.
	c := stageRow(t, store, run.ID, "c")
	if c.State != StagePending || c.Attempt != 1 {
		t.Fatalf("dependent stage c not reset: %+v", c)
	}

	// The next advance re-enqueues b (a is still succeeded) under a FRESH job id.
	// Advance the RESUMED (running) run — the pre-resume handle is still blocked.
	advance(t, store, rec, spec, enqueue, resumed, now)
	b = stageRow(t, store, run.ID, "b")
	if b.State != StageQueued || b.JobID == "" || b.JobID == bBlockedJob {
		t.Fatalf("b not re-enqueued with a fresh job: %+v (was %q)", b, bBlockedJob)
	}
}

// TestResumeFromNeverTouchesSucceeded proves `--from <stage>` naming a SUCCEEDED
// stage leaves it succeeded (an already-landed stage is never re-run) while still
// resetting its non-succeeded dependents.
func TestResumeFromNeverTouchesSucceeded(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := parkBlockedChainRun(t, store, enqueue, rec, spec, now)
	aBefore := stageRow(t, store, run.ID, "a")

	// Resume FROM a (which succeeded): a stays succeeded, b and c reset.
	if _, err := ResumePipelineRun(ctx, store, run.ID, "a"); err != nil {
		t.Fatalf("ResumePipelineRun(from=a): %v", err)
	}
	a := stageRow(t, store, run.ID, "a")
	if a.State != StageSucceeded || a.Attempt != aBefore.Attempt {
		t.Fatalf("succeeded stage a must stay untouched even as resume point: %+v", a)
	}
	if b := stageRow(t, store, run.ID, "b"); b.State != StagePending {
		t.Fatalf("stage b = %s, want pending (dependent of resume point a)", b.State)
	}
	if c := stageRow(t, store, run.ID, "c"); c.State != StagePending {
		t.Fatalf("stage c = %s, want pending (transitive dependent)", c.State)
	}
}

// TestResumeRefusesNonParkedRuns proves resume only applies to a parked
// (blocked/failed) run: a running run, a succeeded run, and a missing run are all
// refused.
func TestResumeRefusesNonParkedRuns(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	// A running run: not parked.
	running := startTestRun(t, store, rec, spec, enqueue, now)
	if _, err := ResumePipelineRun(ctx, store, running.ID, ""); err == nil ||
		!strings.Contains(err.Error(), "requires a parked") {
		t.Fatalf("resume of running run: err=%v, want a parked-required refusal", err)
	}

	// A missing run.
	if _, err := ResumePipelineRun(ctx, store, "prun-nope", ""); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("resume of missing run: err=%v, want not-found", err)
	}

	// Drive the running run to succeeded, then prove a succeeded run is refused.
	settleStageJob(t, store, stageRow(t, store, running.ID, "a").JobID, "approved", "a", nil)
	running = advance(t, store, rec, spec, enqueue, running, now)
	settleStageJob(t, store, stageRow(t, store, running.ID, "b").JobID, "approved", "b", nil)
	running = advance(t, store, rec, spec, enqueue, running, now)
	settleStageJob(t, store, stageRow(t, store, running.ID, "c").JobID, "approved", "c", nil)
	running = advance(t, store, rec, spec, enqueue, running, now)
	if running.State != RunSucceeded {
		t.Fatalf("precondition: run = %s, want succeeded", running.State)
	}
	if _, err := ResumePipelineRun(ctx, store, running.ID, ""); err == nil ||
		!strings.Contains(err.Error(), "requires a parked") {
		t.Fatalf("resume of succeeded run: err=%v, want a parked-required refusal", err)
	}
}

// TestResumeRejectsUnknownFromStage proves a --from naming a stage not in the spec
// is refused.
func TestResumeRejectsUnknownFromStage(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := parkBlockedChainRun(t, store, enqueue, rec, spec, now)
	if _, err := ResumePipelineRun(ctx, store, run.ID, "ghost"); err == nil ||
		!strings.Contains(err.Error(), "not part of pipeline") {
		t.Fatalf("resume from unknown stage: err=%v, want not-part-of refusal", err)
	}
}

// TestCancelPipelineRunInFlight proves cancel of a running run cancels its in-flight
// stage job (via the shared CancelJob path) and moves the run + every non-terminal
// stage to cancelled.
func TestCancelPipelineRunInFlight(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	aJob := stageRow(t, store, run.ID, "a").JobID

	cancelled, err := CancelPipelineRun(ctx, store, run.ID, now)
	if err != nil {
		t.Fatalf("CancelPipelineRun: %v", err)
	}
	if cancelled.State != RunCancelled {
		t.Fatalf("run = %s, want cancelled", cancelled.State)
	}
	// The in-flight stage job was cancelled through the shared abandon verb.
	if job, _ := store.GetJob(ctx, aJob); job.State != string(workflow.JobCancelled) {
		t.Fatalf("stage a job = %s, want cancelled", job.State)
	}
	// Every stage moved to cancelled (a was queued; b, c pending).
	for _, id := range []string{"a", "b", "c"} {
		if got := stageRow(t, store, run.ID, id); got.State != StageCancelled {
			t.Fatalf("stage %s = %s, want cancelled", id, got.State)
		}
	}
	// The pipeline's last_status mirrors the cancel.
	if p, _, _ := store.GetPipeline(ctx, "chain"); p.LastStatus != RunCancelled {
		t.Fatalf("pipeline last_status = %q, want cancelled", p.LastStatus)
	}
}

// TestCancelPipelineRunPreservesSettledStages proves cancel of a PARKED run leaves
// the settled stages' recorded outcomes intact (so a cancelled run still shows WHY
// it halted) while still marking the run cancelled.
func TestCancelPipelineRunPreservesSettledStages(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := parkBlockedChainRun(t, store, enqueue, rec, spec, now)
	cancelled, err := CancelPipelineRun(ctx, store, run.ID, now)
	if err != nil {
		t.Fatalf("CancelPipelineRun: %v", err)
	}
	if cancelled.State != RunCancelled {
		t.Fatalf("run = %s, want cancelled", cancelled.State)
	}
	if a := stageRow(t, store, run.ID, "a"); a.State != StageSucceeded {
		t.Fatalf("stage a = %s, want succeeded (preserved)", a.State)
	}
	if b := stageRow(t, store, run.ID, "b"); b.State != StageBlocked {
		t.Fatalf("stage b = %s, want blocked (halt record preserved)", b.State)
	}
	if c := stageRow(t, store, run.ID, "c"); c.State != StageSkipped {
		t.Fatalf("stage c = %s, want skipped (preserved)", c.State)
	}
}

// TestCancelPipelineRunRefusesTerminal proves cancel refuses an already-terminal run.
func TestCancelPipelineRunRefusesTerminal(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	if _, err := CancelPipelineRun(ctx, store, run.ID, now); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	// A second cancel of the now-cancelled run is refused.
	if _, err := CancelPipelineRun(ctx, store, run.ID, now); err == nil ||
		!strings.Contains(err.Error(), "cancel requires") {
		t.Fatalf("re-cancel: err=%v, want a refusal", err)
	}
	if _, err := CancelPipelineRun(ctx, store, "prun-nope", now); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("cancel missing run: err=%v, want not-found", err)
	}
}
