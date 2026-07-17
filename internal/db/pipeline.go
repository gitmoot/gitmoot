package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Pipeline is one row of the pipelines registry (#681): a named pipeline's
// verbatim spec (YAML + content hash), its interval/jitter schedule, and the
// durable schedule state that makes an interval schedule restart-safe. A run
// snapshots SpecHash so it executes the spec content it was created from, not the
// row's later state.
type Pipeline struct {
	Name       string
	Repo       string
	SpecYAML   string
	SpecHash   string
	Enabled    bool
	Interval   string
	Jitter     string
	LastRunAt  time.Time
	NextDueAt  time.Time
	LastRunID  string
	LastStatus string
	// TriggerBinding is the JSON ownership record for an Activepieces flow.
	TriggerBinding string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PipelineScheduleState is the durable schedule state of one pipeline, mirroring
// HeartbeatState (#533): when it last ran, when it is next due, and the id/status
// of its most recent run. The next_due + last_status are what make an interval
// schedule restart-safe (a restart re-reads next_due rather than firing).
type PipelineScheduleState struct {
	Name       string
	LastRunAt  time.Time
	NextDueAt  time.Time
	LastRunID  string
	LastStatus string
}

// CreateOrUpdatePipeline inserts a pipeline or, on a name conflict, replaces its
// spec-defining fields (repo, spec_yaml, spec_hash, interval, jitter) in place.
// It deliberately does NOT touch enabled or the schedule-state columns on
// conflict: re-adding an edited spec preserves whether the pipeline was enabled
// and its last-run bookkeeping. enabled is set from the struct only on first
// insert; toggle it afterwards with SetPipelineEnabled.
func (s *Store) CreateOrUpdatePipeline(ctx context.Context, pipeline Pipeline) error {
	pipeline.Name = strings.TrimSpace(pipeline.Name)
	if pipeline.Name == "" {
		return errors.New("pipeline name is required")
	}
	enabled := 0
	if pipeline.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pipelines(name, repo, spec_yaml, spec_hash, enabled, interval, jitter, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(name) DO UPDATE SET
			repo = excluded.repo,
			spec_yaml = excluded.spec_yaml,
			spec_hash = excluded.spec_hash,
			interval = excluded.interval,
			jitter = excluded.jitter,
			updated_at = CURRENT_TIMESTAMP`,
		pipeline.Name, strings.TrimSpace(pipeline.Repo), pipeline.SpecYAML, strings.TrimSpace(pipeline.SpecHash),
		enabled, strings.TrimSpace(pipeline.Interval), strings.TrimSpace(pipeline.Jitter))
	return err
}

// GetPipeline returns one pipeline by name. A missing row is NOT an error: it
// returns ok=false with a zero Pipeline so callers can print a friendly
// "not found" and choose the exit code.
func (s *Store) GetPipeline(ctx context.Context, name string) (Pipeline, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Pipeline{}, false, errors.New("pipeline name is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT name, repo, spec_yaml, spec_hash, enabled, interval, jitter,
		last_run_at, next_due_at, last_run_id, last_status, trigger_binding, created_at, updated_at
		FROM pipelines WHERE name = ?`, name)
	pipeline, err := scanPipeline(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Pipeline{}, false, nil
	}
	if err != nil {
		return Pipeline{}, false, err
	}
	return pipeline, true, nil
}

// ListPipelines returns every registered pipeline ordered by name.
func (s *Store) ListPipelines(ctx context.Context) ([]Pipeline, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, repo, spec_yaml, spec_hash, enabled, interval, jitter,
		last_run_at, next_due_at, last_run_id, last_status, trigger_binding, created_at, updated_at
		FROM pipelines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pipelines []Pipeline
	for rows.Next() {
		pipeline, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		pipelines = append(pipelines, pipeline)
	}
	return pipelines, rows.Err()
}

// SetPipelineEnabled flips the enabled flag for a pipeline. It returns an error
// if no pipeline matched the name so the CLI can report "not found" rather than
// silently no-op'ing an enable/disable.
func (s *Store) SetPipelineEnabled(ctx context.Context, name string, enabled bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("pipeline name is required")
	}
	flag := 0
	if enabled {
		flag = 1
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE pipelines SET enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`, flag, name)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("pipeline " + name + " not found")
	}
	return nil
}

// SetPipelineTriggerBinding persists the complete JSON binding record. Keeping
// this as one opaque column makes ownership state atomic and forward-extensible.
func (s *Store) SetPipelineTriggerBinding(ctx context.Context, name, binding string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("pipeline name is required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE pipelines SET trigger_binding = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`, strings.TrimSpace(binding), name)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("pipeline " + name + " not found")
	}
	return nil
}

// DeletePipeline removes a pipeline row and its keychain grants atomically,
// returning whether a pipeline row was deleted. It does not touch the pipeline's
// runner agent or any run rows — the CLI removes the runner agent best-effort,
// and run/stage rows live in separate tables added by the run/advancer step.
func (s *Store) DeletePipeline(ctx context.Context, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errors.New("pipeline name is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Foreign keys are intentionally not enabled globally in this store. Mirror
	// pipeline_exposures' ON DELETE CASCADE explicitly so every connection gets the
	// same semantics, while pipeline_service_runs remain attached to retained run
	// rows and therefore survive exposure/pipeline removal.
	if _, err := tx.ExecContext(ctx, `DELETE FROM pipeline_exposures WHERE pipeline_name = ?`, name); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM keychain_grants WHERE consumer_kind = 'pipeline' AND consumer_id = ?`, name); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM pipelines WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return affected > 0, nil
}

// UpdatePipelineScheduleState persists a pipeline's durable schedule state — the
// last-run/next-due bookkeeping the interval scheduler reads on restart. It
// mirrors UpsertHeartbeatState's role (times stored as RFC3339Nano UTC text; a
// zero time becomes empty text) but updates the existing pipelines row in place
// (the row is created by CreateOrUpdatePipeline), so it returns an error if no
// pipeline matched the name. It leaves the spec and enabled fields untouched.
func (s *Store) UpdatePipelineScheduleState(ctx context.Context, state PipelineScheduleState) error {
	state.Name = strings.TrimSpace(state.Name)
	if state.Name == "" {
		return errors.New("pipeline name is required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE pipelines SET last_run_at = ?, next_due_at = ?, last_run_id = ?, last_status = ?, updated_at = CURRENT_TIMESTAMP
			WHERE name = ?`,
		formatHeartbeatTime(state.LastRunAt), formatHeartbeatTime(state.NextDueAt),
		strings.TrimSpace(state.LastRunID), strings.TrimSpace(state.LastStatus), state.Name)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("pipeline " + state.Name + " not found")
	}
	return nil
}

// AdvancePipelineNextDue advances ONLY a pipeline's schedule anchor (next_due_at),
// leaving the spec, enabled flag, and last-run bookkeeping (last_run_at / id /
// status) untouched. The scan-based scheduler calls it after creating a scheduled
// run (createPipelineRun already stamped the last-run columns) and when it skips a
// due-but-not-runnable pipeline (no repo / unparseable spec), so a misconfigured
// schedule advances rather than hot-looping and without clobbering the last-run
// display. It mirrors UpsertHeartbeatState's next_due role (time stored as
// RFC3339Nano UTC text). A missing pipeline row is an error so a lost row surfaces.
func (s *Store) AdvancePipelineNextDue(ctx context.Context, name string, nextDue time.Time) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("pipeline name is required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE pipelines SET next_due_at = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`,
		formatHeartbeatTime(nextDue), name)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("pipeline " + name + " not found")
	}
	return nil
}

// scanPipeline reads one pipelines row (from *sql.Row or *sql.Rows) into a
// Pipeline, decoding the integer enabled flag and the RFC3339 text timestamps.
func scanPipeline(row interface{ Scan(...any) error }) (Pipeline, error) {
	var (
		pipeline                               Pipeline
		enabled                                int
		lastRun, nextDue, createdAt, updatedAt string
	)
	if err := row.Scan(&pipeline.Name, &pipeline.Repo, &pipeline.SpecYAML, &pipeline.SpecHash, &enabled,
		&pipeline.Interval, &pipeline.Jitter, &lastRun, &nextDue, &pipeline.LastRunID, &pipeline.LastStatus,
		&pipeline.TriggerBinding, &createdAt, &updatedAt); err != nil {
		return Pipeline{}, err
	}
	pipeline.Enabled = enabled != 0
	pipeline.LastRunAt = parseHeartbeatTime(lastRun)
	pipeline.NextDueAt = parseHeartbeatTime(nextDue)
	pipeline.CreatedAt = parseStoredTimestamp(createdAt)
	pipeline.UpdatedAt = parseStoredTimestamp(updatedAt)
	return pipeline, nil
}
