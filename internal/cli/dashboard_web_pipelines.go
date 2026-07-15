package cli

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

// This file implements the two Pipelines-page DataSource methods — Pipelines (the
// declared #681 pipelines with their schedule state + recent run outcomes) and
// PipelineRun (the full stage detail of one run) — over the same read-only store
// paths the rest of dashboard_web.go uses (withStore). Both are deterministic: the
// Pipelines UI polls them with a change-signature skip, so ordering must be stable
// across calls. Pipeline run/stage states are a DIFFERENT vocabulary from job
// NodeState, so the raw pipeline state strings pass straight through (never
// mapNodeState); the times are already time.Time, so they convert via
// UnixMilli guarded by !IsZero (never parseJobTimeMillis, which is for raw string
// columns).

// Pipelines returns every declared pipeline with its schedule state and recent run
// outcomes. It is a read-only pass over ListPipelines (already ORDER BY name, so
// deterministic) plus, per pipeline, ListPipelineRuns (already newest-first) capped
// at the 10 most recent. StageCount is read fail-open from the stored spec (a
// broken spec yields 0 rather than failing the endpoint), matching the CLI's
// pipeline-list projection. Each summary's Recent slice is always non-nil.
func (d *webDataSource) Pipelines(ctx context.Context) ([]dashboard.PipelineSummary, error) {
	out := []dashboard.PipelineSummary{}
	err := withStore(d.home, func(store *db.Store) error {
		rows, err := store.ListPipelines(ctx)
		if err != nil {
			return err
		}
		out = make([]dashboard.PipelineSummary, 0, len(rows))
		knownPipelines := pipelineNameSet(rows)
		for _, p := range rows {
			group, _ := resolvedPipelineGroup(p)
			summary := dashboard.PipelineSummary{
				Name:       p.Name,
				Repo:       p.Repo,
				Group:      group,
				Enabled:    p.Enabled,
				Mode:       pipelineDisplayMode(p, pipelineUpstreamMissing(p, knownPipelines)),
				Interval:   p.Interval,
				Jitter:     p.Jitter,
				StageCount: pipelineStageCount(p.SpecYAML),
				LastRunID:  p.LastRunID,
				LastStatus: p.LastStatus,
				LastRunAt:  pipelineTimeMillis(p.LastRunAt),
				NextDueAt:  pipelineTimeMillis(p.NextDueAt),
				Recent:     []dashboard.PipelineRunSummary{},
			}
			runs, err := store.ListPipelineRuns(ctx, p.Name)
			if err != nil {
				return err
			}
			if len(runs) > 10 {
				runs = runs[:10]
			}
			summary.Recent = make([]dashboard.PipelineRunSummary, 0, len(runs))
			for _, run := range runs {
				summary.Recent = append(summary.Recent, pipelineRunSummary(run))
			}
			out = append(out, summary)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PipelineRun returns the full detail for a single run by run id, its stages in
// spec (topological) order — the same order the CLI funnel prints. An unknown id
// maps to dashboard.ErrPipelineRunNotFound (the API layer serves that as a 404).
// It replicates orderPipelineRunStages: when the run's pipeline and a matching spec
// snapshot are available it reorders the stage rows into the spec's declared order
// and merges each spec stage's metadata onto the row; otherwise it keeps the
// store's stable stage_id order with no spec-derived metadata (the same fall-back
// the funnel takes on a missing pipeline or a SpecHash mismatch). Deps prefer the
// persisted stage needs_json snapshot. Empty snapshots may fall back to the
// current spec under either the strict hash gate or an exact stage-ID-set gate:
// dependency edges drive layout, so a metadata-only spec re-add must not flatten
// run history, while a set mismatch keeps the honest vertical fallback. Stages
// and every stage's Deps are always non-nil.
func (d *webDataSource) PipelineRun(ctx context.Context, id string) (dashboard.PipelineRun, error) {
	out := dashboard.PipelineRun{Stages: []dashboard.PipelineStage{}}
	err := withStore(d.home, func(store *db.Store) error {
		run, ok, err := store.GetPipelineRun(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			return dashboard.ErrPipelineRunNotFound
		}
		stageRows, err := store.ListPipelineRunStages(ctx, run.ID)
		if err != nil {
			return err
		}

		// Load the pipeline's current spec (repo is a pipeline-level attribute, so it
		// resolves even on a spec-hash mismatch) then order the stage rows with the
		// shared spec-order-or-stage_id fallback. specByID is populated only when the
		// spec applies (parsed + hash match), so spec-derived cmd/deps/retry are merged
		// under exactly the same gate as the ordering (matching the CLI funnel).
		var repo string
		spec, specParsed := pipeline.Spec{}, false
		var specHash string
		if rec, found, gerr := store.GetPipeline(ctx, run.Pipeline); gerr == nil && found {
			repo = rec.Repo
			specHash = rec.SpecHash
			if loaded, lerr := pipeline.Load([]byte(rec.SpecYAML)); lerr == nil {
				spec, specParsed = loaded, true
			}
		}
		ordered, specOK := orderRunStages(spec, specParsed, specHash, run.SpecHash, stageRows)
		depsSpecOK := specOK || (specParsed && pipelineRunStageIDsMatchSpec(spec, stageRows))
		stageIDs := make(map[string]struct{}, len(ordered))
		jobIDs := make([]string, 0, len(ordered))
		for _, row := range ordered {
			stageIDs[row.StageID] = struct{}{}
			if row.JobID != "" {
				jobIDs = append(jobIDs, row.JobID)
			}
		}
		progressByJobID, err := store.GetLatestJobEventsByKind(ctx, jobIDs, "progress")
		if err != nil {
			return err
		}
		var specByID map[string]pipeline.Stage
		if specOK {
			specByID = make(map[string]pipeline.Stage, len(spec.Stages))
			for _, s := range spec.Stages {
				specByID[s.ID] = s
			}
		}
		var depsSpecByID map[string]pipeline.Stage
		if depsSpecOK {
			depsSpecByID = make(map[string]pipeline.Stage, len(spec.Stages))
			for _, s := range spec.Stages {
				depsSpecByID[s.ID] = s
			}
		}

		out = dashboard.PipelineRun{
			ID:         run.ID,
			Pipeline:   run.Pipeline,
			Repo:       repo,
			Trigger:    run.Trigger,
			State:      run.State,
			SpecHash:   run.SpecHash,
			HaltStage:  run.HaltStage,
			HaltReason: run.HaltReason,
			Needs:      decodePipelineNeeds(run.NeedsJSON),
			StartedAt:  pipelineTimeMillis(run.StartedAt),
			FinishedAt: pipelineTimeMillis(run.FinishedAt),
			Stages:     make([]dashboard.PipelineStage, 0, len(ordered)),
		}
		for _, row := range ordered {
			deps, persisted := decodePipelineRunStageDeps(row.NeedsJSON, stageIDs)
			if !persisted {
				if spec, ok := depsSpecByID[row.StageID]; ok {
					deps = append([]string{}, spec.Needs...)
				}
			}
			stage := dashboard.PipelineStage{
				ID:         row.StageID,
				State:      row.State,
				Deps:       deps,
				JobID:      row.JobID,
				Attempt:    row.Attempt,
				Needs:      decodePipelineNeeds(row.NeedsJSON),
				Summary:    row.Summary,
				StartedAt:  pipelineTimeMillis(row.StartedAt),
				FinishedAt: pipelineTimeMillis(row.FinishedAt),
			}
			if spec, ok := specByID[row.StageID]; ok {
				stage.Cmd = spec.Cmd
				stage.Retry = spec.Retry
				// #873 display metadata stays under the same strict hash gate as
				// cmd/retry: the structural dependency fallback must never leak the
				// stage kind or agent runtime from a different spec snapshot.
				stage.Kind = pipelineStageKindName(spec)
				if name := strings.TrimSpace(spec.Agent); name != "" {
					if agent, aerr := store.GetAgent(ctx, name); aerr == nil {
						stage.AgentRuntime = strings.TrimSpace(agent.Runtime)
					}
				}
			}
			if row.State == pipeline.StageRunning {
				if event, ok := progressByJobID[row.JobID]; ok {
					var payload pipelineProgressEventPayload
					if json.Unmarshal([]byte(event.Message), &payload) == nil {
						stage.ProgressActivity = payload.Activity
						stage.ProgressAt = parseJobTimeMillis(event.CreatedAt)
					}
				}
			}
			out.Stages = append(out.Stages, stage)
		}
		return nil
	})
	if err != nil {
		return dashboard.PipelineRun{}, err
	}
	return out, nil
}

// decodePipelineRunStageDeps decodes a persisted stage dependency list without
// trusting arbitrary needs_json strings as graph edges. The stage row field also
// carries human-action needs when a stage blocks, so only references to ids in the
// same run become edges. Any non-empty persisted value is authoritative: malformed
// JSON and non-stage values deliberately decode to an empty graph rather than
// falling through to a possibly stale spec. The returned slice is always non-nil
// and keeps the stored order.
func decodePipelineRunStageDeps(value string, stageIDs map[string]struct{}) (deps []string, present bool) {
	deps = []string{}
	if strings.TrimSpace(value) == "" {
		return deps, false
	}
	if err := json.Unmarshal([]byte(value), &deps); err != nil || deps == nil {
		return []string{}, true
	}
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		dep = strings.TrimSpace(dep)
		if _, ok := stageIDs[dep]; ok {
			out = append(out, dep)
		}
	}
	return out, true
}

// pipelineRunStageIDsMatchSpec is the structural dependency-fallback gate. It
// compares exact sets (not order) so metadata-only spec re-adds can restore the
// historical DAG without attaching edges from a genuinely different pipeline.
func pipelineRunStageIDsMatchSpec(spec pipeline.Spec, rows []db.PipelineRunStage) bool {
	if len(spec.Stages) != len(rows) {
		return false
	}
	ids := make(map[string]struct{}, len(spec.Stages))
	for _, stage := range spec.Stages {
		id := strings.TrimSpace(stage.ID)
		if _, duplicate := ids[id]; duplicate {
			return false
		}
		ids[id] = struct{}{}
	}
	for _, row := range rows {
		id := strings.TrimSpace(row.StageID)
		if _, ok := ids[id]; !ok {
			return false
		}
		delete(ids, id)
	}
	return len(ids) == 0
}

// PipelineDetail returns one pipeline's currently declared stage DAG plus its run
// history (newest-first, capped at 100), each history row carrying its per-stage
// marks. An unknown name maps to dashboard.ErrPipelineNotFound (the API layer
// serves that as a 404). The declared DAG is read fail-open from the stored spec
// (a broken spec yields an empty, non-nil declared list rather than failing the
// endpoint) with every stage in spec order and state StagePending. Each run's
// marks are ordered with the SAME spec-order-or-stage_id-fallback semantics as
// PipelineRun (via orderRunStages), so a run whose snapshot still matches the
// current spec reads in spec order and a stale/mismatched one falls back to the
// store's stage_id order. Every slice is non-nil.
func (d *webDataSource) PipelineDetail(ctx context.Context, name string) (dashboard.PipelineDetail, error) {
	out := dashboard.PipelineDetail{
		Name:     name,
		Declared: []dashboard.PipelineStage{},
		Runs:     []dashboard.PipelineRunHistoryEntry{},
	}
	err := withStore(d.home, func(store *db.Store) error {
		rec, ok, err := store.GetPipeline(ctx, name)
		if err != nil {
			return err
		}
		if !ok {
			return dashboard.ErrPipelineNotFound
		}

		// Declared DAG from the current spec (fail-open: a parse failure leaves the
		// non-nil empty Declared in place). Every declared stage is StagePending with
		// its spec-derived cmd/deps/retry — the shape the UI previews for a pipeline
		// that has never run.
		spec, specParsed := pipeline.Spec{}, false
		if loaded, lerr := pipeline.Load([]byte(rec.SpecYAML)); lerr == nil {
			spec, specParsed = loaded, true
			out.Description = spec.Description
			// Agent runtimes resolve best-effort so the declared preview can label
			// agent stages even for a pipeline that has never run (#873).
			agents := pipelineStageAgents(ctx, store, rec)
			out.Declared = make([]dashboard.PipelineStage, 0, len(spec.Stages))
			for _, s := range spec.Stages {
				stage := dashboard.PipelineStage{
					ID:    s.ID,
					State: pipeline.StagePending,
					Kind:  pipelineStageKindName(s),
					Cmd:   s.Cmd,
					Retry: s.Retry,
				}
				if agent, ok := agents[strings.TrimSpace(s.Agent)]; ok {
					stage.AgentRuntime = strings.TrimSpace(agent.Runtime)
				}
				if len(s.Needs) > 0 {
					stage.Deps = append([]string(nil), s.Needs...)
				}
				out.Declared = append(out.Declared, stage)
			}
		}

		// Run history: ListPipelineRuns is already newest-first (started_at DESC, id
		// DESC); cap at 100. Per run, order its stage rows with the shared fallback and
		// project each to a minimal mark (id + state) for the history matrix.
		runs, err := store.ListPipelineRuns(ctx, name)
		if err != nil {
			return err
		}
		if len(runs) > 100 {
			runs = runs[:100]
		}
		out.Runs = make([]dashboard.PipelineRunHistoryEntry, 0, len(runs))
		for _, run := range runs {
			stageRows, err := store.ListPipelineRunStages(ctx, run.ID)
			if err != nil {
				return err
			}
			ordered, _ := orderRunStages(spec, specParsed, rec.SpecHash, run.SpecHash, stageRows)
			marks := make([]dashboard.PipelineStageMark, 0, len(ordered))
			for _, row := range ordered {
				marks = append(marks, dashboard.PipelineStageMark{ID: row.StageID, State: row.State})
			}
			started := pipelineTimeMillis(run.StartedAt)
			finished := pipelineTimeMillis(run.FinishedAt)
			out.Runs = append(out.Runs, dashboard.PipelineRunHistoryEntry{
				ID:         run.ID,
				Trigger:    run.Trigger,
				State:      run.State,
				HaltStage:  run.HaltStage,
				StartedAt:  started,
				FinishedAt: finished,
				Duration:   pipelineRunDurationMillis(started, finished),
				Stages:     marks,
			})
		}
		return nil
	})
	if err != nil {
		return dashboard.PipelineDetail{}, err
	}
	return out, nil
}

// pipelineRunSummary maps one store run row into its lightweight listing entry.
// Duration is finished-started in milliseconds, only when both bounds are set (0
// while a run is still in flight).
func pipelineRunSummary(run db.PipelineRun) dashboard.PipelineRunSummary {
	started := pipelineTimeMillis(run.StartedAt)
	finished := pipelineTimeMillis(run.FinishedAt)
	return dashboard.PipelineRunSummary{
		ID:         run.ID,
		Trigger:    run.Trigger,
		State:      run.State,
		HaltStage:  run.HaltStage,
		StartedAt:  started,
		FinishedAt: finished,
		Duration:   pipelineRunDurationMillis(started, finished),
	}
}

// pipelineRunDurationMillis is the v1.5 run-duration rule: finished-started in
// milliseconds only when both bounds are set and finished is after started (0
// while a run is still in flight or has no timestamps). Shared by the run-listing
// summary and the run-history entry so both agree.
func pipelineRunDurationMillis(started, finished int64) int64 {
	if started > 0 && finished > started {
		return finished - started
	}
	return 0
}

// pipelineStageCount returns the number of declared stages in a stored spec,
// fail-open to 0 when the spec is absent or unparseable (a broken spec degrades
// this one row's stage count, never the endpoint).
func pipelineStageCount(specYAML string) int {
	spec, err := pipeline.Load([]byte(specYAML))
	if err != nil {
		return 0
	}
	return len(spec.Stages)
}

// orderRunStages applies the run-detail ordering decision in one place so the
// full run view (PipelineRun) and the history matrix (PipelineDetail) never
// diverge. When the current spec parsed and its hash matches the run's snapshot,
// the rows are reordered into spec (topological) order and specOK is true (the
// caller may then merge spec-derived cmd/deps/retry). Any weaker condition — no
// spec, parse failure, or a spec-hash mismatch — keeps the store's stage_id order
// with specOK false, mirroring orderPipelineRunStages' fallback (the CLI funnel).
// The caller may independently apply the narrower stage-ID-set fallback to Deps;
// this strict result remains the only gate for ordering and display metadata.
func orderRunStages(spec pipeline.Spec, specParsed bool, specHash, runSpecHash string, rows []db.PipelineRunStage) (ordered []db.PipelineRunStage, specOK bool) {
	if specParsed && strings.TrimSpace(specHash) == strings.TrimSpace(runSpecHash) {
		return orderPipelineStagesBySpec(spec, rows), true
	}
	return rows, false
}

// orderPipelineStagesBySpec reorders stage rows into the spec's declared
// (topological) order, appending any rows not present in the spec last so no data
// is dropped. It mirrors orderPipelineRunStages' reordering, given an
// already-loaded, hash-verified spec.
func orderPipelineStagesBySpec(spec pipeline.Spec, stages []db.PipelineRunStage) []db.PipelineRunStage {
	byID := make(map[string]db.PipelineRunStage, len(stages))
	for _, stage := range stages {
		byID[stage.StageID] = stage
	}
	ordered := make([]db.PipelineRunStage, 0, len(stages))
	seen := make(map[string]struct{}, len(stages))
	for _, s := range spec.Stages {
		if row, ok := byID[s.ID]; ok {
			ordered = append(ordered, row)
			seen[s.ID] = struct{}{}
		}
	}
	for _, stage := range stages {
		if _, ok := seen[stage.StageID]; !ok {
			ordered = append(ordered, stage)
		}
	}
	return ordered
}

// pipelineTimeMillis converts a store time.Time to epoch milliseconds, 0 for the
// zero time (the on-disk empty-text sentinel). Pipeline schedule/run/stage columns
// are time.Time, so this is the right converter — parseJobTimeMillis is for raw
// string timestamp columns.
func pipelineTimeMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}
