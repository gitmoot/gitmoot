package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type PermissionPolicyObservationBaseline struct {
	AffectedCount int
	Configs       []string
	RecordedAt    string
	UpdatedAt     string
}

func normalizePermissionPolicyConfigs(configs []string) []string {
	set := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		if config = strings.TrimSpace(config); config != "" {
			set[config] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for config := range set {
		result = append(result, config)
	}
	sort.Strings(result)
	return result
}

func (s *Store) PermissionPolicyObservationBaseline(ctx context.Context) (PermissionPolicyObservationBaseline, bool, error) {
	var baseline PermissionPolicyObservationBaseline
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT affected_count, configs_json, recorded_at, updated_at
		FROM permission_policy_observation_baseline WHERE id = 1`).Scan(
		&baseline.AffectedCount, &raw, &baseline.RecordedAt, &baseline.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PermissionPolicyObservationBaseline{}, false, nil
	}
	if err != nil {
		return PermissionPolicyObservationBaseline{}, false, err
	}
	if err := json.Unmarshal([]byte(raw), &baseline.Configs); err != nil {
		return PermissionPolicyObservationBaseline{}, false, fmt.Errorf("decode permission-policy baseline: %w", err)
	}
	baseline.Configs = normalizePermissionPolicyConfigs(baseline.Configs)
	return baseline, true, nil
}

// InitializePermissionPolicyObservationBaseline records the first live-store
// measurement atomically. Concurrent repo pollers all observe the same winner.
func (s *Store) InitializePermissionPolicyObservationBaseline(ctx context.Context, configs []string) (PermissionPolicyObservationBaseline, bool, error) {
	configs = normalizePermissionPolicyConfigs(configs)
	raw, err := json.Marshal(configs)
	if err != nil {
		return PermissionPolicyObservationBaseline{}, false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO permission_policy_observation_baseline(id, affected_count, configs_json)
		VALUES (1, ?, ?)`, len(configs), string(raw))
	if err != nil {
		return PermissionPolicyObservationBaseline{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return PermissionPolicyObservationBaseline{}, false, err
	}
	baseline, ok, err := s.PermissionPolicyObservationBaseline(ctx)
	return baseline, ok && affected == 1, err
}

// LowerPermissionPolicyObservationBaseline is the one-way ratchet. A lower live
// count advances the baseline; equal or larger counts leave it untouched.
func (s *Store) LowerPermissionPolicyObservationBaseline(ctx context.Context, configs []string) (bool, error) {
	configs = normalizePermissionPolicyConfigs(configs)
	raw, err := json.Marshal(configs)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE permission_policy_observation_baseline
		SET affected_count = ?, configs_json = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = 1 AND affected_count > ?`, len(configs), string(raw), len(configs))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// ClaimPermissionPolicyWarning atomically coalesces a warning by agent-config
// and window and writes the winning job event in the same transaction.
func (s *Store) ClaimPermissionPolicyWarning(ctx context.Context, agent, runtimeName, policy, windowStart string, event JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO permission_policy_warning_claims(agent, runtime, policy, window_start, job_id)
		VALUES (?, ?, ?, ?, ?)`, agent, runtimeName, policy, windowStart, event.JobID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`, event.JobID, event.Kind, event.Message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListUnresolvedJobAgents returns durable job identities that no longer resolve
// through either the registered-agent or ephemeral-instance tables.
func (s *Store) ListUnresolvedJobAgents(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT j.agent
		FROM jobs j
		LEFT JOIN agents a ON a.name = j.agent
		LEFT JOIN agent_instances i ON i.name = j.agent
		WHERE TRIM(j.agent) <> '' AND a.name IS NULL AND i.name IS NULL
		ORDER BY j.agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result = append(result, name)
	}
	return result, rows.Err()
}
