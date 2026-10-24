package db

import "context"

// CleanupPipelineConsumer binds each stage to the run and attempt that may still
// consume a producer's workspace. A missing run is represented by an empty state.
type CleanupPipelineConsumer struct {
	RunID    string `json:"run_id"`
	RunState string `json:"run_state"`
	StageID  string `json:"stage_id"`
	State    string `json:"state"`
	JobID    string `json:"job_id"`
	Attempt  int    `json:"attempt"`
}

func (s *Store) ListCleanupPipelineConsumers(ctx context.Context, jobID, rootID string) ([]CleanupPipelineConsumer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stage.run_id, COALESCE(run.state, ''),
		stage.stage_id, stage.state, stage.job_id, stage.attempt
		FROM pipeline_run_stages stage LEFT JOIN pipeline_runs run ON run.id=stage.run_id
		WHERE stage.run_id IN (
			SELECT source.run_id FROM pipeline_run_stages source
			WHERE source.job_id=? OR (?<>'' AND (source.job_id=? OR source.run_id=?))
		) ORDER BY stage.run_id, stage.stage_id`, jobID, rootID, rootID, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var consumers []CleanupPipelineConsumer
	for rows.Next() {
		var consumer CleanupPipelineConsumer
		if err := rows.Scan(&consumer.RunID, &consumer.RunState, &consumer.StageID,
			&consumer.State, &consumer.JobID, &consumer.Attempt); err != nil {
			return nil, err
		}
		consumers = append(consumers, consumer)
	}
	return consumers, rows.Err()
}
