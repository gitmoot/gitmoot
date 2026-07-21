package pipeline

import (
	"context"
	"errors"
	"fmt"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
	"strings"
	"time"
)

// The #681 pipeline RESUME and CANCEL verbs. Resume re-runs a parked (blocked or
// failed) run from its halted stage; cancel abandons a run, cancelling its in-flight
// stage jobs through the shared workflow.CancelJob path. ResumePipelineRun is the
// #682 seam — the approval gates that follow-up will call it to restart a run once a
// block is cleared, so it is exported and side-effect-scoped to the run + stage rows
// (no gates / secret stores / approval UI here).

// ResumePipelineRun re-runs a PARKED pipeline run (#681, decision 7). It resets the
// halted stage (or the explicit fromStage) and its transitive DEPENDENTS back to
// pending — bumping each reset stage's attempt so the next scan re-enqueues a fresh
// stage job under a new deterministic id/fingerprint — clears the run's park fields,
// and returns the run to running so the advance pass picks it up. It NEVER touches a
// stage that already succeeded (an already-landed stage is not re-run, even if it is
// downstream of the resume point) and it REFUSES a non-parked run (only a
// blocked/failed run can be resumed).
//
// A run executes the spec it was created from, so the spec is resolved from the
// pipeline row and hash-verified against the run's snapshot: a resume against a spec
// that drifted since the run was created is refused rather than re-running a changed
// DAG. The updated run is returned.
func ResumePipelineRun(ctx context.Context, store *db.Store, runID, fromStage string) (db.PipelineRun, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return db.PipelineRun{}, errors.New("run id is required")
	}
	run, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil {
		return db.PipelineRun{}, err
	}
	if !ok {
		return db.PipelineRun{}, fmt.Errorf("run %s not found", runID)
	}
	if !isParkedPipelineRun(run.State) {
		return db.PipelineRun{}, fmt.Errorf("run %s is %s; resume requires a parked (blocked or failed) run", runID, run.State)
	}
	rec, ok, err := store.GetPipeline(ctx, run.Pipeline)
	if err != nil {
		return db.PipelineRun{}, err
	}
	if !ok {
		return db.PipelineRun{}, fmt.Errorf("pipeline %s not found", run.Pipeline)
	}
	if strings.TrimSpace(rec.SpecHash) != strings.TrimSpace(run.SpecHash) {
		return db.PipelineRun{}, fmt.Errorf("pipeline %s spec changed since run %s was created; cannot resume", run.Pipeline, runID)
	}
	spec, err := Load([]byte(rec.SpecYAML))
	if err != nil {
		return db.PipelineRun{}, fmt.Errorf("stored spec is invalid: %w", err)
	}
	// Default the resume point to the halted stage; --from overrides it to re-run from
	// an explicit stage (and everything downstream of it).
	from := strings.TrimSpace(fromStage)
	if from == "" {
		from = strings.TrimSpace(run.HaltStage)
	}
	if from == "" {
		return db.PipelineRun{}, fmt.Errorf("run %s has no halt stage to resume from; pass --from <stage>", runID)
	}
	if _, ok := pipelineStageByID(spec, from); !ok {
		return db.PipelineRun{}, fmt.Errorf("stage %s is not part of pipeline %s", from, run.Pipeline)
	}
	reset := pipelineStageAndDependents(spec, from)
	rows, err := store.ListPipelineRunStages(ctx, runID)
	if err != nil {
		return db.PipelineRun{}, err
	}
	for _, row := range rows {
		if !reset[row.StageID] {
			continue
		}
		// Never re-run a stage that already succeeded — an already-landed stage stays
		// succeeded even when it is downstream of the resume point.
		if row.State == StageSucceeded {
			continue
		}
		updated := row
		updated.State = StagePending
		updated.JobID = ""
		updated.Attempt = row.Attempt + 1
		updated.NeedsJSON = ""
		updated.Summary = ""
		updated.StartedAt = time.Time{}
		updated.FinishedAt = time.Time{}
		if err := store.UpdatePipelineRunStage(ctx, updated); err != nil {
			return db.PipelineRun{}, err
		}
	}
	// Clear the run's park fields and return it to running so the advance pass
	// re-enqueues the reset stages on the next scan.
	run.State = RunRunning
	run.HaltStage = ""
	run.HaltReason = ""
	run.NeedsJSON = ""
	run.FinishedAt = time.Time{}
	if err := store.UpdatePipelineRun(ctx, run); err != nil {
		return db.PipelineRun{}, err
	}
	// Mirror the pipeline's last_status back to running WHEN this run is the
	// pipeline's most recent one, so `pipeline list` / `show <name>` reflect the
	// resume without clobbering a newer run's bookkeeping.
	if strings.TrimSpace(rec.LastRunID) == runID {
		if err := store.UpdatePipelineLastRun(ctx, rec.Name, runID, RunRunning, time.Time{}); err != nil {
			return db.PipelineRun{}, err
		}
	}
	return run, nil
}

// CancelPipelineRun abandons a run (#681, decision 9): it cancels every in-flight
// (queued/running) stage job through the shared workflow.CancelJob path — the same
// single-job abandon verb `job cancel` uses, which also best-effort releases the
// job's locks — marks the run cancelled, and moves every not-yet-terminal stage to
// cancelled for a clean terminal picture. An already-settled stage
// (succeeded/blocked/failed/skipped) keeps its recorded outcome, so a cancelled
// run still shows WHY it halted. It refuses an already-terminal run
// (succeeded/cancelled): there is nothing to abandon. The updated run is returned.
func CancelPipelineRun(ctx context.Context, store *db.Store, runID string, now time.Time) (db.PipelineRun, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return db.PipelineRun{}, errors.New("run id is required")
	}
	now = now.UTC()
	run, ok, err := store.GetPipelineRun(ctx, runID)
	if err != nil {
		return db.PipelineRun{}, err
	}
	if !ok {
		return db.PipelineRun{}, fmt.Errorf("run %s not found", runID)
	}
	if run.State == RunSucceeded || run.State == RunCancelled {
		return db.PipelineRun{}, fmt.Errorf("run %s is %s; cancel requires a running or parked run", runID, run.State)
	}
	rows, err := store.ListPipelineRunStages(ctx, runID)
	if err != nil {
		return db.PipelineRun{}, err
	}
	for _, row := range rows {
		// Cancel the in-flight stage job through the shared abandon verb (which also
		// releases the job's locks best-effort). A stage with no job, or a job that
		// already settled, is skipped; CancelJob's own state guard makes a lost race a
		// no-op, and lock cleanup is incidental, so the error is intentionally swallowed.
		if job := strings.TrimSpace(row.JobID); job != "" && (row.State == StageQueued || row.State == StageRunning) {
			_, _ = workflow.CancelJob(ctx, store, job)
		}
		if isTerminalPipelineStage(row.State) {
			continue
		}
		updated := row
		updated.State = StageCancelled
		updated.FinishedAt = now
		if err := store.UpdatePipelineRunStage(ctx, updated); err != nil {
			return db.PipelineRun{}, err
		}
	}
	run.State = RunCancelled
	run.FinishedAt = now
	if err := store.UpdatePipelineRun(ctx, run); err != nil {
		return db.PipelineRun{}, err
	}
	// Mirror the pipeline's last_status to cancelled WHEN this run is its most recent
	// one (same non-clobber guard as resume).
	if rec, ok, gerr := store.GetPipeline(ctx, run.Pipeline); gerr == nil && ok && strings.TrimSpace(rec.LastRunID) == runID {
		if err := store.UpdatePipelineLastRun(ctx, run.Pipeline, runID, RunCancelled, time.Time{}); err != nil {
			return db.PipelineRun{}, err
		}
	}
	return run, nil
}

// isParkedPipelineRun reports whether a run state is a parked (resumable) one: a
// blocked or failed run awaits a human, and resume is that human's re-run verb.
func isParkedPipelineRun(state string) bool {
	return state == RunBlocked || state == RunFailed
}

// isTerminalPipelineStage reports whether a stage row is already in a settled state
// (so cancel leaves it intact rather than overwriting its recorded outcome).
func isTerminalPipelineStage(state string) bool {
	switch state {
	case StageSucceeded, StageBlocked, StageFailed,
		StageSkipped, StageCancelled:
		return true
	default:
		return false
	}
}

// pipelineStageByID looks a stage up in a spec by id.
func pipelineStageByID(spec Spec, id string) (Stage, bool) {
	for _, stage := range spec.Stages {
		if stage.ID == id {
			return stage, true
		}
	}
	return Stage{}, false
}

// pipelineStageAndDependents returns the closure of `from` plus every stage that
// transitively DEPENDS on it (following needs edges forward). Resume resets this set
// so the resume point AND everything downstream of it re-runs; the spec's validated
// acyclicity bounds the walk.
func pipelineStageAndDependents(spec Spec, from string) map[string]bool {
	dependents := make(map[string][]string, len(spec.Stages))
	for _, stage := range spec.Stages {
		for _, dep := range stage.Needs {
			dependents[dep] = append(dependents[dep], stage.ID)
		}
	}
	out := make(map[string]bool)
	var visit func(id string)
	visit = func(id string) {
		if out[id] {
			return
		}
		out[id] = true
		for _, child := range dependents[id] {
			visit(child)
		}
	}
	visit(from)
	return out
}
