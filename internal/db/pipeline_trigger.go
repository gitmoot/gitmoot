package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PipelineTriggerState is the durable once-per-upstream-run cursor for one
// downstream pipeline. Cursor is an upstream pipeline run id; ArmedAt is the
// no-backfill boundary used while Cursor is empty.
type PipelineTriggerState struct {
	Downstream string
	Upstream   string
	Cursor     string
	ArmedAt    time.Time
}

// ArmPipelineTrigger resets a downstream trigger at add/enable time. The cursor
// advances to the latest upstream run of any state so historical successes (and
// runs from a disabled period) can never backfill after arming.
func (s *Store) ArmPipelineTrigger(ctx context.Context, downstream, upstream string, armedAt time.Time) error {
	downstream = strings.TrimSpace(downstream)
	upstream = strings.TrimSpace(upstream)
	if downstream == "" || upstream == "" {
		return errors.New("pipeline trigger downstream and upstream names are required")
	}
	if armedAt.IsZero() {
		armedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	cursor := ""
	rows, err := tx.QueryContext(ctx, `SELECT id, started_at FROM pipeline_runs WHERE pipeline = ?`, upstream)
	if err != nil {
		return err
	}
	var latestAt time.Time
	for rows.Next() {
		var id, rawStarted string
		if err := rows.Scan(&id, &rawStarted); err != nil {
			rows.Close()
			return err
		}
		started := parseHeartbeatTime(rawStarted)
		if cursor == "" || started.After(latestAt) || (started.Equal(latestAt) && id > cursor) {
			cursor = id
			latestAt = started
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_trigger_states(downstream_pipeline, upstream_pipeline, cursor, armed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(downstream_pipeline) DO UPDATE SET
			upstream_pipeline = excluded.upstream_pipeline,
			cursor = excluded.cursor,
			armed_at = excluded.armed_at`,
		downstream, upstream, cursor, formatHeartbeatTime(armedAt.UTC())); err != nil {
		return err
	}
	return tx.Commit()
}

// GetPipelineTriggerState returns one downstream trigger state. A missing row is
// not an error.
func (s *Store) GetPipelineTriggerState(ctx context.Context, downstream string) (PipelineTriggerState, bool, error) {
	downstream = strings.TrimSpace(downstream)
	if downstream == "" {
		return PipelineTriggerState{}, false, errors.New("pipeline trigger downstream name is required")
	}
	var state PipelineTriggerState
	var armedAt string
	err := s.db.QueryRowContext(ctx, `SELECT downstream_pipeline, upstream_pipeline, cursor, armed_at
		FROM pipeline_trigger_states WHERE downstream_pipeline = ?`, downstream).
		Scan(&state.Downstream, &state.Upstream, &state.Cursor, &armedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelineTriggerState{}, false, nil
	}
	if err != nil {
		return PipelineTriggerState{}, false, err
	}
	state.ArmedAt = parseHeartbeatTime(armedAt)
	return state, true, nil
}

// DeletePipelineTriggerState removes the cursor owned by a downstream pipeline.
// It never removes rows merely because their upstream was removed: that is what
// keeps downstream pipelines dormant and ready if the upstream is re-added.
func (s *Store) DeletePipelineTriggerState(ctx context.Context, downstream string) error {
	downstream = strings.TrimSpace(downstream)
	if downstream == "" {
		return errors.New("pipeline trigger downstream name is required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM pipeline_trigger_states WHERE downstream_pipeline = ?`, downstream)
	return err
}

// NextSucceededPipelineTriggerRun returns the oldest successful upstream run
// newer than the state cursor (or ArmedAt when no cursor exists). Choosing the
// oldest lets a downstream drain multiple successes one-by-one without skipping
// any once it becomes idle.
func (s *Store) NextSucceededPipelineTriggerRun(ctx context.Context, state PipelineTriggerState) (PipelineRun, bool, error) {
	state.Downstream = strings.TrimSpace(state.Downstream)
	state.Upstream = strings.TrimSpace(state.Upstream)
	state.Cursor = strings.TrimSpace(state.Cursor)
	if state.Downstream == "" || state.Upstream == "" {
		return PipelineRun{}, false, errors.New("pipeline trigger downstream and upstream names are required")
	}
	boundary := state.ArmedAt.UTC()
	if state.Cursor != "" {
		cursorRun, ok, err := s.GetPipelineRun(ctx, state.Cursor)
		if err != nil {
			return PipelineRun{}, false, err
		}
		if !ok || cursorRun.Pipeline != state.Upstream {
			return PipelineRun{}, false, fmt.Errorf("pipeline trigger cursor %q for %s does not identify an upstream run", state.Cursor, state.Downstream)
		}
		boundary = cursorRun.StartedAt.UTC()
	}
	runs, err := s.ListPipelineRuns(ctx, state.Upstream)
	if err != nil {
		return PipelineRun{}, false, err
	}
	var candidate PipelineRun
	for _, run := range runs {
		if run.State != "succeeded" {
			continue
		}
		newer := run.StartedAt.After(boundary)
		if state.Cursor != "" && run.StartedAt.Equal(boundary) && run.ID > state.Cursor {
			newer = true
		}
		if !newer {
			continue
		}
		if candidate.ID == "" || run.StartedAt.Before(candidate.StartedAt) || (run.StartedAt.Equal(candidate.StartedAt) && run.ID < candidate.ID) {
			candidate = run
		}
	}
	return candidate, candidate.ID != "", nil
}

// FirePipelineTrigger atomically creates the downstream run and all of its stage
// rows, updates last-run bookkeeping, and advances the trigger cursor. If the
// downstream is disabled, already active, or the cursor changed since selection,
// it returns fired=false without advancing the cursor.
func (s *Store) FirePipelineTrigger(ctx context.Context, expected PipelineTriggerState, upstreamRun, downstreamRun PipelineRun, stages []PipelineRunStage) (bool, error) {
	expected.Downstream = strings.TrimSpace(expected.Downstream)
	expected.Upstream = strings.TrimSpace(expected.Upstream)
	expected.Cursor = strings.TrimSpace(expected.Cursor)
	if expected.Downstream == "" || expected.Upstream == "" {
		return false, errors.New("pipeline trigger downstream and upstream names are required")
	}
	if upstreamRun.ID == "" || upstreamRun.Pipeline != expected.Upstream || upstreamRun.State != "succeeded" {
		return false, errors.New("pipeline trigger fire requires a succeeded upstream run")
	}
	if downstreamRun.Pipeline != expected.Downstream {
		return false, errors.New("pipeline trigger downstream run does not match trigger state")
	}
	if strings.TrimSpace(downstreamRun.State) == "" {
		downstreamRun.State = "running"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var currentUpstream, currentCursor, rawArmedAt string
	err = tx.QueryRowContext(ctx, `SELECT upstream_pipeline, cursor, armed_at FROM pipeline_trigger_states WHERE downstream_pipeline = ?`, expected.Downstream).
		Scan(&currentUpstream, &currentCursor, &rawArmedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("pipeline trigger state for %s not found", expected.Downstream)
	}
	if err != nil {
		return false, err
	}
	if currentUpstream != expected.Upstream || currentCursor != expected.Cursor {
		return false, nil
	}
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT enabled FROM pipelines WHERE name = ?`, expected.Downstream).Scan(&enabled); err != nil {
		return false, err
	}
	if enabled == 0 {
		return false, nil
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_runs WHERE pipeline = ? AND state = 'running'`, expected.Downstream).Scan(&active); err != nil {
		return false, err
	}
	if active > 0 {
		return false, nil
	}
	var actualPipeline, actualState, rawStartedAt string
	if err := tx.QueryRowContext(ctx, `SELECT pipeline, state, started_at FROM pipeline_runs WHERE id = ?`, upstreamRun.ID).Scan(&actualPipeline, &actualState, &rawStartedAt); err != nil {
		return false, err
	}
	if actualPipeline != expected.Upstream || actualState != "succeeded" {
		return false, errors.New("upstream pipeline run is no longer succeeded")
	}
	boundary := parseHeartbeatTime(rawArmedAt)
	if currentCursor != "" {
		var cursorPipeline, rawCursorStartedAt string
		if err := tx.QueryRowContext(ctx, `SELECT pipeline, started_at FROM pipeline_runs WHERE id = ?`, currentCursor).Scan(&cursorPipeline, &rawCursorStartedAt); err != nil {
			return false, err
		}
		if cursorPipeline != expected.Upstream {
			return false, errors.New("pipeline trigger cursor does not belong to upstream pipeline")
		}
		boundary = parseHeartbeatTime(rawCursorStartedAt)
	}
	startedAt := parseHeartbeatTime(rawStartedAt)
	if !startedAt.After(boundary) && !(currentCursor != "" && startedAt.Equal(boundary) && upstreamRun.ID > currentCursor) {
		return false, errors.New("upstream pipeline run is not newer than trigger cursor")
	}
	if err := insertPipelineRun(ctx, tx, downstreamRun); err != nil {
		return false, err
	}
	for _, stage := range stages {
		if strings.TrimSpace(stage.RunID) == "" {
			stage.RunID = downstreamRun.ID
		}
		if stage.RunID != downstreamRun.ID {
			return false, errors.New("pipeline trigger stage run id does not match downstream run")
		}
		if err := insertPipelineRunStage(ctx, tx, stage); err != nil {
			return false, err
		}
	}
	if err := updatePipelineLastRun(ctx, tx, expected.Downstream, downstreamRun.ID, downstreamRun.State, downstreamRun.StartedAt.UTC()); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE pipeline_trigger_states SET cursor = ?
		WHERE downstream_pipeline = ? AND upstream_pipeline = ? AND cursor = ?`,
		upstreamRun.ID, expected.Downstream, expected.Upstream, expected.Cursor)
	if err != nil {
		return false, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return false, err
	} else if affected != 1 {
		return false, errors.New("pipeline trigger cursor changed during fire")
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
