package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// The #681 pipeline RUN engine: create a run, enqueue ready stages, and advance it
// by a scan (mirroring runHeartbeatScanOnce). The advancer is decision-based — it
// folds each settled stage job by its gitmoot_result DECISION, never jobs.state,
// because changes_requested is a SUCCEEDED job that must NOT advance (the
// stateForDecision trap, workflow/result.go). Stage jobs are ordinary queued jobs
// run through the SHELL runtime via a per-job runtime override; the normal worker
// tick claims + runs them, and this advancer folds the results. The daemon-loop
// wiring of runPipelineScanOnce lands in a later step — here it is driven by
// `pipeline run` and the tests.

// pipelineStageEnqueuer enqueues one pipeline stage job. In production it wraps
// workflow.Mailbox.Enqueue (matching newHeartbeatEnqueuer); tests inject a fake to
// assert the request shape without a real worker.
type pipelineStageEnqueuer func(ctx context.Context, request workflow.JobRequest) (db.Job, error)

// newPipelineStageEnqueuer builds the production enqueuer: a Mailbox bound to the
// store and the daemon's canary-routing policy, so a pipeline stage job is
// indistinguishable from a normal background job once enqueued (the runner agent
// carries no template, so canary never actually samples).
func newPipelineStageEnqueuer(store *db.Store, home string) pipelineStageEnqueuer {
	mailbox := workflow.Mailbox{Store: store, CanaryEnabled: canaryRoutingEnabled(home)}
	return func(ctx context.Context, request workflow.JobRequest) (db.Job, error) {
		return mailbox.Enqueue(ctx, request)
	}
}

// pipelineRunID derives a deterministic-per-invocation run id: a recognizable
// "prun-" marker, the pipeline name (a validated name-safe token), and the
// nanosecond clock so distinct runs never collide. The marker also lets
// `pipeline show` tell a run id from a pipeline name.
func pipelineRunID(pipelineName string, now time.Time) string {
	return fmt.Sprintf("prun-%s-%x", strings.TrimSpace(pipelineName), now.UTC().UnixNano())
}

// pipelineStageJobID is the DETERMINISTIC stage job id: run id + stage id +
// attempt. It is deterministic so a re-scan re-derives the same id and the
// idempotent enqueue (enqueuePipelineStageJob) never double-creates a job; the
// attempt suffix mints a fresh id for each retry.
func pipelineStageJobID(runID, stageID string, attempt int) string {
	return fmt.Sprintf("%s-%s-a%d", runID, stageID, attempt)
}

// pipelineStageFingerprint is the stage job fingerprint `pipeline:<name>:<run>:
// <stage>:<attempt>` (decision 2), unique per stage attempt.
func pipelineStageFingerprint(pipelineName, runID, stageID string, attempt int) string {
	return fmt.Sprintf("pipeline:%s:%s:%s:%d", pipelineName, runID, stageID, attempt)
}

// pipelineStageJobRequest builds the JobRequest for one stage attempt. The stage
// command travels in RuntimeOverrideRef (the shell runtime runs it verbatim as
// `sh -c <cmd> gitmoot <prompt>`), NOT on the runner agent's runtime_ref, so one
// runner serves every stage. ParentJobID and DelegationID stay EMPTY (any
// ParentJobID would enter delegation advancement, engine.go); RootJobID is the run
// id so all of a run's stage jobs share a root. The per-stage timeout is plumbed
// as JobTimeout.
func pipelineStageJobRequest(rec db.Pipeline, stage pipeline.Stage, run db.PipelineRun, attempt int) workflow.JobRequest {
	return workflow.JobRequest{
		ID:                 pipelineStageJobID(run.ID, stage.ID, attempt),
		Agent:              pipelineRunnerAgentName(rec.Name),
		Action:             "ask",
		Repo:               rec.Repo,
		Sender:             workflow.PipelineJobSender,
		Instructions:       fmt.Sprintf("pipeline %s run %s stage %s", rec.Name, run.ID, stage.ID),
		Fingerprint:        pipelineStageFingerprint(rec.Name, run.ID, stage.ID, attempt),
		RootJobID:          run.ID,
		JobTimeout:         stage.Timeout,
		RuntimeOverride:    runtime.ShellRuntime,
		RuntimeOverrideRef: stage.Cmd,
	}
}

// enqueuePipelineStageJob enqueues a stage job idempotently: on an enqueue error
// (e.g. a duplicate id from a re-scan that raced the stage-row write) it adopts an
// already-created job with the same deterministic id, mirroring the engine's
// enqueue idiom (engine.go). A genuinely new error with no matching job propagates.
func enqueuePipelineStageJob(ctx context.Context, store *db.Store, enqueue pipelineStageEnqueuer, request workflow.JobRequest) (db.Job, error) {
	job, err := enqueue(ctx, request)
	if err == nil {
		return job, nil
	}
	if existing, getErr := store.GetJob(ctx, request.ID); getErr == nil {
		return existing, nil
	}
	return db.Job{}, err
}

// createPipelineRun creates a fresh manual/scheduled run: the pipeline_runs row
// (snapshotting the pipeline's current spec_hash) plus one pending
// pipeline_run_stages row per spec stage, and records the run on the pipeline's
// last-run bookkeeping. It does NOT enqueue anything — the caller advances the run
// once to enqueue the ready root stages, so creation and the first advance are the
// same idempotent code path a re-scan uses.
func createPipelineRun(ctx context.Context, store *db.Store, rec db.Pipeline, spec pipeline.Spec, trigger string, now time.Time) (db.PipelineRun, error) {
	run := db.PipelineRun{
		ID:        pipelineRunID(rec.Name, now),
		Pipeline:  rec.Name,
		Trigger:   trigger,
		SpecHash:  rec.SpecHash,
		State:     pipeline.RunRunning,
		StartedAt: now.UTC(),
	}
	if err := store.CreatePipelineRun(ctx, run); err != nil {
		return db.PipelineRun{}, err
	}
	for _, stage := range spec.Stages {
		if err := store.CreatePipelineRunStage(ctx, db.PipelineRunStage{
			RunID:   run.ID,
			StageID: stage.ID,
			State:   pipeline.StagePending,
		}); err != nil {
			return db.PipelineRun{}, err
		}
	}
	if err := store.UpdatePipelineLastRun(ctx, rec.Name, run.ID, pipeline.RunRunning, now.UTC()); err != nil {
		return db.PipelineRun{}, err
	}
	return run, nil
}

// runPipelineScanOnce is the pipeline scan wired into BOTH daemon supervisor loops
// next to runHeartbeatScanOnce (#681). Each tick is two passes, mirroring the
// heartbeat idiom:
//
//	SCHEDULE pass — for each enabled pipeline whose interval is due and that has no
//	  active run, create a fresh scheduled run and advance next_due anchored to now
//	  (missed ticks coalesce into one run; a durable next_due makes it restart-safe).
//	ADVANCE  pass — advance every in-flight (state='running') run once.
//
// Ordering matters: the schedule pass creates the run rows, then the advance pass
// enqueues their ready root stages, so a scheduled run and a manual `pipeline run`
// reach the worker by the identical code path. Off-by-default and zero-cost when
// idle: a pipeline with no interval is skipped before any state touch, and a parked
// or terminal run is never advanced. A per-pipeline / per-run error is collected
// (first wins) but never stops the rest; the daemon caller logs it and never aborts
// the loop.
func runPipelineScanOnce(ctx context.Context, store *db.Store, enqueue pipelineStageEnqueuer, now time.Time) error {
	now = now.UTC()
	var firstErr error
	if err := schedulePipelineRuns(ctx, store, now); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := advancePipelineRuns(ctx, store, enqueue, now); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// pipelineRunsInFlight reports whether any pipeline run is still in flight
// (state='running'). The registered-repo supervisor calls it once per cycle,
// AFTER runPipelineScanOnce, to decide whether the pipeline advancer needs a
// prompt next tick (#697). Off-by-default cheap: with no pipelines / no active
// runs the active-run query returns an empty slice before any further work, so
// an idle daemon pays only one indexed SELECT per cycle.
func pipelineRunsInFlight(ctx context.Context, store *db.Store) (bool, error) {
	runs, err := store.ListActivePipelineRuns(ctx)
	if err != nil {
		return false, err
	}
	return len(runs) > 0, nil
}

// pipelineAdvanceWait decouples the pipeline-advance cadence from the repo-poll
// backoff (#697). The registered-repo supervisor sleeps for the wait the poller
// returns, which grows to minutes when repo polling backs off (persistent repo
// errors / a 404 repo / a GitHub secondary rate-limit; base 1m, max 5m). That
// backoff must not throttle pipeline-run advancement, or a settled stage folds
// only once per (now minutes-long) tick. When a run is in flight this caps the
// sleep to the configured, NON-backed-off poll interval (pollFloor) so the next
// pipeline scan runs promptly; otherwise (no run in flight, or a non-positive
// floor, or a poll wait already shorter than the floor) it returns the poll wait
// unchanged, so an idle daemon still sleeps out the full backoff exactly as
// before. The repo poller is unaffected: it re-checks each repo's own NextPoll
// due time and skips repos not yet due, so an early wake never re-polls a
// backed-off repo.
func pipelineAdvanceWait(pollWait, pollFloor time.Duration, runsInFlight bool) time.Duration {
	if !runsInFlight {
		return pollWait
	}
	if pollFloor > 0 && pollFloor < pollWait {
		return pollFloor
	}
	return pollWait
}

// schedulePipelineRuns is runPipelineScanOnce's SCHEDULE pass (#681): it creates a
// run for each enabled pipeline whose interval schedule is due and has no active
// run. A per-pipeline error is collected (first wins) but never stops the rest.
func schedulePipelineRuns(ctx context.Context, store *db.Store, now time.Time) error {
	pipelines, err := store.ListPipelines(ctx)
	if err != nil {
		return err
	}
	now = now.UTC()
	var firstErr error
	for _, rec := range pipelines {
		if err := scheduleOnePipeline(ctx, store, rec, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// scheduleOnePipeline fires (or skips) one pipeline's interval schedule, mirroring
// runOneHeartbeat. Only an ENABLED pipeline with an interval that is DUE and has NO
// active run creates a run; the guards otherwise skip it. next_due is advanced
// ANCHORED TO NOW (not the old due time) so a long-idle scheduler that missed many
// intervals fires exactly ONE run and schedules the next one interval out — never a
// backlog replay.
func scheduleOnePipeline(ctx context.Context, store *db.Store, rec db.Pipeline, now time.Time) error {
	// Only an enabled, scheduled (interval set) pipeline auto-runs; a disabled or
	// manual-only pipeline is never fired by the scheduler.
	if !rec.Enabled || strings.TrimSpace(rec.Interval) == "" {
		return nil
	}
	interval, err := time.ParseDuration(strings.TrimSpace(rec.Interval))
	if err != nil {
		return fmt.Errorf("pipeline %s interval: %w", rec.Name, err)
	}
	if interval <= 0 {
		return fmt.Errorf("pipeline %s interval must be positive", rec.Name)
	}
	jitter := time.Duration(0)
	if trimmed := strings.TrimSpace(rec.Jitter); trimmed != "" {
		if jitter, err = time.ParseDuration(trimmed); err != nil {
			return fmt.Errorf("pipeline %s jitter: %w", rec.Name, err)
		}
	}
	// Not yet due: a zero next_due (the first scan after enable) is in the past, so a
	// freshly enabled pipeline fires immediately; a future next_due is skipped WITHOUT
	// advancing (mirrors the heartbeat due check).
	if !rec.NextDueAt.IsZero() && now.Before(rec.NextDueAt) {
		return nil
	}
	nextDue := now.Add(interval + heartbeatJitter(jitter))
	// A scheduled pipeline needs a managed repo: its stage jobs need one for the
	// worker to claim them (the same precondition `pipeline run` enforces). Without a
	// repo the run would wedge (queued jobs no worker claims keep the overlap guard
	// tripped forever), so skip the run but ADVANCE next_due so a misconfigured
	// schedule does not hot-loop and self-recovers once a repo is set (the heartbeat
	// repo-unmanaged idiom). AdvancePipelineNextDue touches only next_due, so the
	// last-run display is preserved.
	if strings.TrimSpace(rec.Repo) == "" {
		return store.AdvancePipelineNextDue(ctx, rec.Name, nextDue)
	}
	// Overlap guard: one active (state='running') run per pipeline. A run in flight
	// means skip WITHOUT advancing next_due, so the next scheduled run fires as soon
	// as this one settles. A parked (blocked/failed) run does NOT count as active,
	// mirroring `pipeline run`'s ActivePipelineRun guard.
	if _, active, err := store.ActivePipelineRun(ctx, rec.Name); err != nil {
		return err
	} else if active {
		return nil
	}
	spec, err := pipeline.Load([]byte(rec.SpecYAML))
	if err != nil {
		// A stored spec that no longer parses can't be run (`pipeline add` validates,
		// so this is defensive); advance next_due so a broken spec does not hot-loop.
		return store.AdvancePipelineNextDue(ctx, rec.Name, nextDue)
	}
	if _, err := createPipelineRun(ctx, store, rec, spec, "schedule", now); err != nil {
		return err
	}
	// createPipelineRun stamped last_run_*; advance ONLY next_due (anchored to now).
	// The ADVANCE pass that follows enqueues this run's ready root stages.
	return store.AdvancePipelineNextDue(ctx, rec.Name, nextDue)
}

// advancePipelineRuns is runPipelineScanOnce's ADVANCE pass (#681): it advances
// every in-flight (state='running') run once, so parked (blocked/failed) and
// terminal runs consume zero compute. A per-run error is collected (first wins) but
// never stops the remaining runs. Runs whose pipeline was removed, whose stored
// spec no longer parses, or whose spec drifted (hash no longer matches the run's
// snapshot) are skipped rather than executed against a changed spec.
func advancePipelineRuns(ctx context.Context, store *db.Store, enqueue pipelineStageEnqueuer, now time.Time) error {
	runs, err := store.ListActivePipelineRuns(ctx)
	if err != nil {
		return err
	}
	now = now.UTC()
	var firstErr error
	for _, run := range runs {
		rec, ok, err := store.GetPipeline(ctx, run.Pipeline)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !ok {
			continue
		}
		spec, err := pipeline.Load([]byte(rec.SpecYAML))
		if err != nil {
			continue
		}
		// A run executes its SNAPSHOT: if the pipeline's spec was edited since the run
		// was created, its hash no longer matches and we do NOT advance against the
		// changed DAG/commands. Parking/repairing spec drift is a follow-up; skipping
		// is safe (the run stays running and is left untouched).
		if strings.TrimSpace(rec.SpecHash) != strings.TrimSpace(run.SpecHash) {
			continue
		}
		if _, err := advancePipelineRun(ctx, store, enqueue, rec, spec, run, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// advancePipelineRun is the per-run advancer (decision 6). It is a single
// idempotent pass: FOLD every settled stage job into its stage row by DECISION,
// ENQUEUE every newly-ready stage (deps all succeeded — a blocked/failed branch
// halts only ITSELF, so independent branches keep running), then, if
// nothing is in flight, SETTLE the run — succeeded, or parked blocked/failed with
// the halt stage + reason + (for blocked) the aggregated needs persisted verbatim.
// It writes only rows that actually change, so a re-scan on an unchanged run makes
// no writes and no enqueues. It assumes run.State == running; a parked/terminal run
// is a no-op. The updated run is returned for callers/tests.
func advancePipelineRun(ctx context.Context, store *db.Store, enqueue pipelineStageEnqueuer, rec db.Pipeline, spec pipeline.Spec, run db.PipelineRun, now time.Time) (db.PipelineRun, error) {
	now = now.UTC()
	if run.State != pipeline.RunRunning {
		return run, nil
	}
	specByID := make(map[string]pipeline.Stage, len(spec.Stages))
	for _, stage := range spec.Stages {
		specByID[stage.ID] = stage
	}
	rows, err := store.ListPipelineRunStages(ctx, run.ID)
	if err != nil {
		return run, err
	}
	byID := make(map[string]db.PipelineRunStage, len(rows))
	for _, row := range rows {
		byID[row.StageID] = row
	}

	// --- FOLD: settle stage jobs by decision ---------------------------------
	for _, stage := range spec.Stages {
		row, ok := byID[stage.ID]
		if !ok {
			continue
		}
		if row.State != pipeline.StageQueued && row.State != pipeline.StageRunning {
			continue
		}
		if strings.TrimSpace(row.JobID) == "" {
			continue
		}
		job, err := store.GetJob(ctx, row.JobID)
		if err != nil {
			return run, err
		}
		if !workflow.IsSettledJobState(job.State) {
			// Not terminal yet: reflect a queued->running transition so the funnel
			// tracks the live job, but otherwise leave it in flight.
			if job.State == string(workflow.JobRunning) && row.State == pipeline.StageQueued {
				row.State = pipeline.StageRunning
				if err := persistPipelineStage(ctx, store, byID[stage.ID], row); err != nil {
					return run, err
				}
				byID[stage.ID] = row
			}
			continue
		}
		state, summary, needs := foldPipelineStageOutcome(spec.EffectiveSuccessDecisions(stage), job)
		if state == pipeline.StageFailed && row.Attempt < stage.Retry {
			// Retry budget remains: bump the attempt and reset to pending so the
			// enqueue phase re-launches it under a fresh deterministic id/fingerprint.
			row.Attempt++
			row.State = pipeline.StagePending
			row.JobID = ""
			row.Summary = summary
			row.NeedsJSON = ""
			row.FinishedAt = time.Time{}
		} else {
			row.State = state
			row.Summary = summary
			row.NeedsJSON = marshalPipelineNeeds(needs)
			row.FinishedAt = now
		}
		if err := persistPipelineStage(ctx, store, byID[stage.ID], row); err != nil {
			return run, err
		}
		byID[stage.ID] = row
	}

	// --- ENQUEUE: launch newly-ready stages ----------------------------------
	// Per-branch reachability, NOT a run-wide fail-fast: a stage whose needs have
	// ALL succeeded is enqueued even when an INDEPENDENT branch has already blocked
	// or failed. Only a stage that (transitively) depends on the halted stage never
	// becomes ready — its dep never reaches succeeded — so a blocked/failed branch
	// halts itself while its siblings run to completion (decision 6: "dependents
	// never ready"). The run does not park until nothing is in flight below.
	for _, stage := range spec.Stages {
		row := byID[stage.ID]
		if row.State != pipeline.StagePending || !pipelineStageDepsSucceeded(stage, byID) {
			continue
		}
		job, err := enqueuePipelineStageJob(ctx, store, enqueue, pipelineStageJobRequest(rec, stage, run, row.Attempt))
		if err != nil {
			return run, err
		}
		row.State = pipeline.StageQueued
		row.JobID = job.ID
		row.StartedAt = now
		if err := persistPipelineStage(ctx, store, byID[stage.ID], row); err != nil {
			return run, err
		}
		byID[stage.ID] = row
	}

	// --- SETTLE: park or finish once nothing is in flight --------------------
	if anyPipelineStageInState(byID, pipeline.StageQueued, pipeline.StageRunning) {
		return run, nil
	}

	// Nothing is in flight, so no stage will ever reach succeeded again. Any stage
	// still pending here therefore has a dep that can NEVER all-succeed — the ENQUEUE
	// pass above already launched every pending stage whose deps had all succeeded,
	// so a leftover pending stage is transitively downstream of a blocked/failed (or
	// itself-skipped) stage. Mark it skipped for a clean terminal picture; this is
	// exactly why a blocked/failed branch's downstream stages are never enqueued,
	// while a reachable independent branch was already run above.
	for _, stage := range spec.Stages {
		row := byID[stage.ID]
		if row.State == pipeline.StagePending {
			row.State = pipeline.StageSkipped
			row.FinishedAt = now
			if err := persistPipelineStage(ctx, store, byID[stage.ID], row); err != nil {
				return run, err
			}
			byID[stage.ID] = row
		}
	}

	settled := computePipelineRunSettlement(spec, byID, now)
	return applyPipelineRunSettlement(ctx, store, rec, run, settled)
}

// pipelineRunSettlement is the computed run-level outcome of an advance pass once
// nothing is in flight: the terminal state plus the halt stage/reason/needs.
type pipelineRunSettlement struct {
	State      string
	HaltStage  string
	HaltReason string
	NeedsJSON  string
	FinishedAt time.Time
}

// computePipelineRunSettlement derives the run's terminal state from the settled
// stage rows (pure). Failed takes precedence over blocked (a hard failure is the
// more urgent halt); otherwise blocked parks with the aggregated needs; otherwise
// every stage is succeeded/skipped and the run succeeded. The halt stage is the
// first blocked/failed stage in spec (topological) order for a stable funnel.
func computePipelineRunSettlement(spec pipeline.Spec, byID map[string]db.PipelineRunStage, now time.Time) pipelineRunSettlement {
	if stage, ok := firstPipelineStageInState(spec, byID, pipeline.StageFailed); ok {
		return pipelineRunSettlement{
			State:      pipeline.RunFailed,
			HaltStage:  stage.StageID,
			HaltReason: stage.Summary,
			FinishedAt: now,
		}
	}
	if stage, ok := firstPipelineStageInState(spec, byID, pipeline.StageBlocked); ok {
		return pipelineRunSettlement{
			State:      pipeline.RunBlocked,
			HaltStage:  stage.StageID,
			HaltReason: stage.Summary,
			NeedsJSON:  marshalPipelineNeeds(aggregatePipelineBlockedNeeds(spec, byID)),
			FinishedAt: now,
		}
	}
	return pipelineRunSettlement{State: pipeline.RunSucceeded, FinishedAt: now}
}

// applyPipelineRunSettlement persists the run's terminal state (only when it
// actually changed) and mirrors it onto the pipeline's last_status.
func applyPipelineRunSettlement(ctx context.Context, store *db.Store, rec db.Pipeline, run db.PipelineRun, settled pipelineRunSettlement) (db.PipelineRun, error) {
	updated := run
	updated.State = settled.State
	updated.HaltStage = settled.HaltStage
	updated.HaltReason = settled.HaltReason
	updated.NeedsJSON = settled.NeedsJSON
	updated.FinishedAt = settled.FinishedAt
	if pipelineRunEqual(run, updated) {
		return run, nil
	}
	if err := store.UpdatePipelineRun(ctx, updated); err != nil {
		return run, err
	}
	if err := store.UpdatePipelineLastRun(ctx, rec.Name, run.ID, updated.State, time.Time{}); err != nil {
		return run, err
	}
	return updated, nil
}

// foldPipelineStageOutcome maps a settled stage job to a stage outcome BY DECISION
// (decision 6). It reads payload.Result.Decision, never jobs.state: a
// changes_requested job is SUCCEEDED but must NOT advance, so any decision outside
// the stage's success_decisions (and not "blocked") is a failure. A cancelled job
// or a job with no result (errored before parse / unparseable) is a failure too.
func foldPipelineStageOutcome(successDecisions []string, job db.Job) (state, summary string, needs []string) {
	if job.State == string(workflow.JobCancelled) {
		return pipeline.StageFailed, "stage job cancelled", nil
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil || payload.Result == nil {
		return pipeline.StageFailed, "stage job produced no gitmoot_result", nil
	}
	decision := strings.TrimSpace(payload.Result.Decision)
	summary = strings.TrimSpace(payload.Result.Summary)
	switch {
	case containsPipelineDecision(successDecisions, decision):
		return pipeline.StageSucceeded, summary, nil
	case decision == "blocked":
		if summary == "" {
			summary = "stage blocked"
		}
		return pipeline.StageBlocked, summary, append([]string(nil), payload.Result.Needs...)
	default:
		if summary == "" {
			summary = fmt.Sprintf("stage returned decision %q", decision)
		}
		return pipeline.StageFailed, summary, nil
	}
}

// pipelineStageDepsSucceeded reports whether every stage this one needs has
// reached StageSucceeded (so it is ready to enqueue). A missing dep row is treated
// as not-succeeded (defensive; validation already rejects unknown needs).
func pipelineStageDepsSucceeded(stage pipeline.Stage, byID map[string]db.PipelineRunStage) bool {
	for _, dep := range stage.Needs {
		if dep == "" {
			continue
		}
		if byID[dep].State != pipeline.StageSucceeded {
			return false
		}
	}
	return true
}

func anyPipelineStageInState(byID map[string]db.PipelineRunStage, states ...string) bool {
	for _, row := range byID {
		for _, want := range states {
			if row.State == want {
				return true
			}
		}
	}
	return false
}

// firstPipelineStageInState returns the first stage row (in spec/topological
// order) that is in the wanted state, for a stable halt-stage / funnel ordering.
func firstPipelineStageInState(spec pipeline.Spec, byID map[string]db.PipelineRunStage, want string) (db.PipelineRunStage, bool) {
	for _, stage := range spec.Stages {
		if row, ok := byID[stage.ID]; ok && row.State == want {
			return row, true
		}
	}
	return db.PipelineRunStage{}, false
}

// aggregatePipelineBlockedNeeds collects the persisted needs of every blocked
// stage, in spec order, deduped, so the run-level needs_json is the union of what
// every parked stage is waiting on.
func aggregatePipelineBlockedNeeds(spec pipeline.Spec, byID map[string]db.PipelineRunStage) []string {
	seen := make(map[string]struct{})
	var needs []string
	for _, stage := range spec.Stages {
		row, ok := byID[stage.ID]
		if !ok || row.State != pipeline.StageBlocked {
			continue
		}
		for _, need := range decodePipelineNeeds(row.NeedsJSON) {
			if _, dup := seen[need]; dup {
				continue
			}
			seen[need] = struct{}{}
			needs = append(needs, need)
		}
	}
	return needs
}

// persistPipelineStage writes a stage row only when a field the advancer owns
// actually changed, keeping a re-scan write-free.
func persistPipelineStage(ctx context.Context, store *db.Store, old, updated db.PipelineRunStage) error {
	if pipelineStageEqual(old, updated) {
		return nil
	}
	return store.UpdatePipelineRunStage(ctx, updated)
}

func pipelineStageEqual(a, b db.PipelineRunStage) bool {
	return a.State == b.State && a.JobID == b.JobID && a.Attempt == b.Attempt &&
		a.NeedsJSON == b.NeedsJSON && a.Summary == b.Summary &&
		a.StartedAt.Equal(b.StartedAt) && a.FinishedAt.Equal(b.FinishedAt)
}

func pipelineRunEqual(a, b db.PipelineRun) bool {
	return a.State == b.State && a.HaltStage == b.HaltStage && a.HaltReason == b.HaltReason &&
		a.NeedsJSON == b.NeedsJSON && a.FinishedAt.Equal(b.FinishedAt)
}

func containsPipelineDecision(decisions []string, decision string) bool {
	for _, d := range decisions {
		if d == decision {
			return true
		}
	}
	return false
}

// marshalPipelineNeeds encodes a needs slice as a compact JSON array for
// needs_json (empty slice => empty string, so no needs stores as "").
func marshalPipelineNeeds(needs []string) string {
	needs = compactPipelineNeeds(needs)
	if len(needs) == 0 {
		return ""
	}
	encoded, err := json.Marshal(needs)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// decodePipelineNeeds parses a needs_json array back into a slice; a blank or
// malformed value decodes to no needs.
func decodePipelineNeeds(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var needs []string
	if err := json.Unmarshal([]byte(value), &needs); err != nil {
		return nil
	}
	return compactPipelineNeeds(needs)
}

func compactPipelineNeeds(needs []string) []string {
	out := make([]string, 0, len(needs))
	for _, need := range needs {
		if trimmed := strings.TrimSpace(need); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
