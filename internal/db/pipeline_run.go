package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// PipelineRun is one row of pipeline_runs (#681): a single execution of a named
// pipeline. It snapshots SpecHash so the advancer executes the spec content the
// run was created from (resolved back through the pipelines row and hash-verified,
// not stored twice). State is the run-level advancement state; HaltStage/
// HaltReason name the stage that parked a blocked/failed run and NeedsJSON carries
// the aggregated blocked needs verbatim. Times are RFC3339Nano UTC (zero == empty
// text), mirroring the pipelines/heartbeat_state schedule columns.
type PipelineRun struct {
	ID          string
	Pipeline    string
	Trigger     string
	PayloadJSON string
	SpecHash    string
	State       string
	HaltStage   string
	HaltReason  string
	NeedsJSON   string
	StartedAt   time.Time
	FinishedAt  time.Time
}

// PipelineRunStage is one row of pipeline_run_stages (#681): the advancement state
// of a single stage within a run, keyed by (RunID, StageID). JobID is the stage
// job the advancer enqueued (empty until enqueued or after a retry reset), Attempt
// is the current attempt number (the deterministic stage job id embeds it),
// NeedsJSON persists a blocked stage's needs verbatim, and Summary is the settling
// result's short summary (or the failure reason).
type PipelineRunStage struct {
	RunID      string
	StageID    string
	State      string
	JobID      string
	Attempt    int
	NeedsJSON  string
	Summary    string
	StartedAt  time.Time
	FinishedAt time.Time
}

type pipelineRunExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// CreatePipelineRun inserts a new run row. The caller supplies the id (a
// deterministic manual/schedule run id); a duplicate id is an error so a
// double-create is caught rather than silently overwriting an in-flight run.
func (s *Store) CreatePipelineRun(ctx context.Context, run PipelineRun) error {
	return insertPipelineRun(ctx, s.db, run)
}

func insertPipelineRun(ctx context.Context, execer pipelineRunExecer, run PipelineRun) error {
	run.ID = strings.TrimSpace(run.ID)
	run.Pipeline = strings.TrimSpace(run.Pipeline)
	if run.ID == "" {
		return errors.New("pipeline run id is required")
	}
	if run.Pipeline == "" {
		return errors.New("pipeline run pipeline is required")
	}
	if strings.TrimSpace(run.State) == "" {
		run.State = "running"
	}
	if strings.TrimSpace(run.Trigger) == "" {
		run.Trigger = "manual"
	}
	if strings.TrimSpace(run.PayloadJSON) == "" {
		run.PayloadJSON = "{}"
	}
	_, err := execer.ExecContext(ctx, `INSERT INTO pipeline_runs(id, pipeline, trigger, payload_json, spec_hash, state, halt_stage, halt_reason, needs_json, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Pipeline, strings.TrimSpace(run.Trigger), run.PayloadJSON, strings.TrimSpace(run.SpecHash),
		strings.TrimSpace(run.State), strings.TrimSpace(run.HaltStage), run.HaltReason, run.NeedsJSON,
		formatHeartbeatTime(run.StartedAt), formatHeartbeatTime(run.FinishedAt))
	return err
}

// GetPipelineRun returns one run by id. A missing row is NOT an error: it returns
// ok=false so callers can print a friendly "not found".
func (s *Store) GetPipelineRun(ctx context.Context, id string) (PipelineRun, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return PipelineRun{}, false, errors.New("pipeline run id is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, pipeline, trigger, payload_json, spec_hash, state, halt_stage, halt_reason, needs_json, started_at, finished_at
		FROM pipeline_runs WHERE id = ?`, id)
	run, err := scanPipelineRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelineRun{}, false, nil
	}
	if err != nil {
		return PipelineRun{}, false, err
	}
	return run, true, nil
}

// ListPipelineRuns returns every run for a pipeline, newest first (by started_at
// then id). It is used by the run listing / overlap inspection.
func (s *Store) ListPipelineRuns(ctx context.Context, pipeline string) ([]PipelineRun, error) {
	pipeline = strings.TrimSpace(pipeline)
	if pipeline == "" {
		return nil, errors.New("pipeline name is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, pipeline, trigger, payload_json, spec_hash, state, halt_stage, halt_reason, needs_json, started_at, finished_at
		FROM pipeline_runs WHERE pipeline = ? ORDER BY started_at DESC, id DESC`, pipeline)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []PipelineRun
	for rows.Next() {
		run, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// ActivePipelineRun returns the single in-flight (state='running') run for a
// pipeline, if any. It backs the "one active run per pipeline" overlap guard (a
// run parks or finishes before another may start). If more than one ever exists
// (should not, given the guard), the newest is returned.
func (s *Store) ActivePipelineRun(ctx context.Context, pipeline string) (PipelineRun, bool, error) {
	pipeline = strings.TrimSpace(pipeline)
	if pipeline == "" {
		return PipelineRun{}, false, errors.New("pipeline name is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, pipeline, trigger, payload_json, spec_hash, state, halt_stage, halt_reason, needs_json, started_at, finished_at
		FROM pipeline_runs WHERE pipeline = ? AND state = 'running' ORDER BY started_at DESC, id DESC LIMIT 1`, pipeline)
	run, err := scanPipelineRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelineRun{}, false, nil
	}
	if err != nil {
		return PipelineRun{}, false, err
	}
	return run, true, nil
}

// ListActivePipelineRuns returns every in-flight (state='running') run across all
// pipelines. It is the scan-based advancer's entry query: only running runs are
// advanced, so a parked (blocked/failed) or terminal run consumes zero compute
// until it is resumed.
func (s *Store) ListActivePipelineRuns(ctx context.Context) ([]PipelineRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, pipeline, trigger, payload_json, spec_hash, state, halt_stage, halt_reason, needs_json, started_at, finished_at
		FROM pipeline_runs WHERE state = 'running' ORDER BY started_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []PipelineRun
	for rows.Next() {
		run, err := scanPipelineRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// UpdatePipelineRun writes a run's mutable advancement fields (state, halt
// stage/reason, needs, finished_at) in place. The immutable identity — id,
// pipeline, trigger, spec_hash, started_at — is never changed. It returns an error
// if no run matched the id so a lost row surfaces rather than silently no-op'ing.
func (s *Store) UpdatePipelineRun(ctx context.Context, run PipelineRun) error {
	run.ID = strings.TrimSpace(run.ID)
	if run.ID == "" {
		return errors.New("pipeline run id is required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE pipeline_runs SET state = ?, halt_stage = ?, halt_reason = ?, needs_json = ?, finished_at = ?
			WHERE id = ?`,
		strings.TrimSpace(run.State), strings.TrimSpace(run.HaltStage), run.HaltReason, run.NeedsJSON,
		formatHeartbeatTime(run.FinishedAt), run.ID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("pipeline run " + run.ID + " not found")
	}
	return nil
}

// CreatePipelineRunStage inserts one stage row for a run. The (run_id, stage_id)
// pair is the primary key, so a duplicate is an error (the run-creation path
// inserts each stage exactly once).
func (s *Store) CreatePipelineRunStage(ctx context.Context, stage PipelineRunStage) error {
	return insertPipelineRunStage(ctx, s.db, stage)
}

func insertPipelineRunStage(ctx context.Context, execer pipelineRunExecer, stage PipelineRunStage) error {
	stage.RunID = strings.TrimSpace(stage.RunID)
	stage.StageID = strings.TrimSpace(stage.StageID)
	if stage.RunID == "" || stage.StageID == "" {
		return errors.New("pipeline run stage run id and stage id are required")
	}
	if strings.TrimSpace(stage.State) == "" {
		stage.State = "pending"
	}
	_, err := execer.ExecContext(ctx, `INSERT INTO pipeline_run_stages(run_id, stage_id, state, job_id, attempt, needs_json, summary, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		stage.RunID, stage.StageID, strings.TrimSpace(stage.State), strings.TrimSpace(stage.JobID), stage.Attempt,
		stage.NeedsJSON, stage.Summary, formatHeartbeatTime(stage.StartedAt), formatHeartbeatTime(stage.FinishedAt))
	return err
}

// ListPipelineRunStages returns every stage row for a run, ordered by stage_id
// (the funnel view reorders them into spec/DAG order). It is the advancer's fold
// input and the funnel's data source.
func (s *Store) ListPipelineRunStages(ctx context.Context, runID string) ([]PipelineRunStage, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, errors.New("pipeline run id is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, stage_id, state, job_id, attempt, needs_json, summary, started_at, finished_at
		FROM pipeline_run_stages WHERE run_id = ? ORDER BY stage_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stages []PipelineRunStage
	for rows.Next() {
		stage, err := scanPipelineRunStage(rows)
		if err != nil {
			return nil, err
		}
		stages = append(stages, stage)
	}
	return stages, rows.Err()
}

// GetPipelineRunStage returns one stage row by (run_id, stage_id). A missing row
// is NOT an error (ok=false).
func (s *Store) GetPipelineRunStage(ctx context.Context, runID, stageID string) (PipelineRunStage, bool, error) {
	runID = strings.TrimSpace(runID)
	stageID = strings.TrimSpace(stageID)
	if runID == "" || stageID == "" {
		return PipelineRunStage{}, false, errors.New("pipeline run id and stage id are required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT run_id, stage_id, state, job_id, attempt, needs_json, summary, started_at, finished_at
		FROM pipeline_run_stages WHERE run_id = ? AND stage_id = ?`, runID, stageID)
	stage, err := scanPipelineRunStage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelineRunStage{}, false, nil
	}
	if err != nil {
		return PipelineRunStage{}, false, err
	}
	return stage, true, nil
}

// UpdatePipelineRunStage writes a stage's mutable fields (state, job id, attempt,
// needs, summary, started/finished) in place, keyed by (run_id, stage_id). It
// returns an error if no stage row matched so a lost row surfaces.
func (s *Store) UpdatePipelineRunStage(ctx context.Context, stage PipelineRunStage) error {
	stage.RunID = strings.TrimSpace(stage.RunID)
	stage.StageID = strings.TrimSpace(stage.StageID)
	if stage.RunID == "" || stage.StageID == "" {
		return errors.New("pipeline run stage run id and stage id are required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE pipeline_run_stages SET state = ?, job_id = ?, attempt = ?, needs_json = ?, summary = ?, started_at = ?, finished_at = ?
			WHERE run_id = ? AND stage_id = ?`,
		strings.TrimSpace(stage.State), strings.TrimSpace(stage.JobID), stage.Attempt, stage.NeedsJSON, stage.Summary,
		formatHeartbeatTime(stage.StartedAt), formatHeartbeatTime(stage.FinishedAt), stage.RunID, stage.StageID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("pipeline run stage " + stage.RunID + "/" + stage.StageID + " not found")
	}
	return nil
}

// UpdatePipelineLastRun records a pipeline's most recent run bookkeeping
// (last_run_id + last_status), and — when at is non-zero — last_run_at. It leaves
// the schedule's next_due_at and the spec/enabled fields untouched, so it is safe
// to call both when a run starts (`pipeline run`) and when the advancer settles a
// run to a terminal state. A missing pipeline row is NOT an error: a run outlives a
// removed pipeline, and the bookkeeping is best-effort observability.
func (s *Store) UpdatePipelineLastRun(ctx context.Context, name, runID, status string, at time.Time) error {
	return updatePipelineLastRun(ctx, s.db, name, runID, status, at)
}

func updatePipelineLastRun(ctx context.Context, execer pipelineRunExecer, name, runID, status string, at time.Time) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("pipeline name is required")
	}
	if at.IsZero() {
		_, err := execer.ExecContext(ctx,
			`UPDATE pipelines SET last_run_id = ?, last_status = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`,
			strings.TrimSpace(runID), strings.TrimSpace(status), name)
		return err
	}
	_, err := execer.ExecContext(ctx,
		`UPDATE pipelines SET last_run_at = ?, last_run_id = ?, last_status = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`,
		formatHeartbeatTime(at), strings.TrimSpace(runID), strings.TrimSpace(status), name)
	return err
}

func scanPipelineRun(row interface{ Scan(...any) error }) (PipelineRun, error) {
	var (
		run                 PipelineRun
		startedAt, finished string
	)
	if err := row.Scan(&run.ID, &run.Pipeline, &run.Trigger, &run.PayloadJSON, &run.SpecHash, &run.State,
		&run.HaltStage, &run.HaltReason, &run.NeedsJSON, &startedAt, &finished); err != nil {
		return PipelineRun{}, err
	}
	run.StartedAt = parseHeartbeatTime(startedAt)
	run.FinishedAt = parseHeartbeatTime(finished)
	return run, nil
}

func scanPipelineRunStage(row interface{ Scan(...any) error }) (PipelineRunStage, error) {
	var (
		stage               PipelineRunStage
		startedAt, finished string
	)
	if err := row.Scan(&stage.RunID, &stage.StageID, &stage.State, &stage.JobID, &stage.Attempt,
		&stage.NeedsJSON, &stage.Summary, &startedAt, &finished); err != nil {
		return PipelineRunStage{}, err
	}
	stage.StartedAt = parseHeartbeatTime(startedAt)
	stage.FinishedAt = parseHeartbeatTime(finished)
	return stage, nil
}
