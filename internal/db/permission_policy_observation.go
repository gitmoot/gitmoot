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

type PermissionPolicyWarningClaim struct {
	Agent       string
	Runtime     string
	Policy      string
	Capability  string
	WindowStart string
	JobID       string
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

// LowerPermissionPolicyObservationBaseline is the one-way ratchet. The caller
// supplies the baseline it compared so a concurrent update cannot absorb a
// config that was not in that recorded set.
func (s *Store) LowerPermissionPolicyObservationBaseline(ctx context.Context, previous, configs []string) (bool, error) {
	previous = normalizePermissionPolicyConfigs(previous)
	configs = normalizePermissionPolicyConfigs(configs)
	raw, err := json.Marshal(configs)
	if err != nil {
		return false, err
	}
	previousRaw, err := json.Marshal(previous)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE permission_policy_observation_baseline
		SET affected_count = ?, configs_json = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = 1 AND configs_json = ?`, len(configs), string(raw), string(previousRaw))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// ClaimPermissionPolicyWarning atomically coalesces a warning by agent-config,
// capability, and window and writes the winning job event in the same transaction.
func (s *Store) ClaimPermissionPolicyWarning(ctx context.Context, agent, runtimeName, policy, capability, windowStart string, event JobEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO permission_policy_warning_claims(agent, runtime, policy, capability, window_start, job_id)
		VALUES (?, ?, ?, ?, ?, ?)`, agent, runtimeName, policy, capability, windowStart, event.JobID)
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

// UpdateClaimedPermissionPolicyWarning updates the existing coalesced warning
// event in place. The claim join prevents a caller from attaching effects to an
// unrelated event that merely shares the same job id and kind.
func (s *Store) UpdateClaimedPermissionPolicyWarning(ctx context.Context, jobID, kind, message string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE job_events
		SET message = ?
		WHERE id = (
			SELECT e.id
			FROM job_events e
			JOIN permission_policy_warning_claims c ON c.job_id = e.job_id
			WHERE e.job_id = ? AND e.kind = ?
			ORDER BY e.id ASC
			LIMIT 1
		)`, message, jobID, kind)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// LatestPermissionPolicyWarningClaim resolves the newest coalesced observation
// for an agent configuration. It is the query seam for effect measurements;
// effects remain attached to the R1 claim/event rather than a parallel store.
func (s *Store) LatestPermissionPolicyWarningClaim(ctx context.Context, agent, runtimeName, policy string) (PermissionPolicyWarningClaim, bool, error) {
	var claim PermissionPolicyWarningClaim
	err := s.db.QueryRowContext(ctx, `SELECT agent, runtime, policy, capability, window_start, job_id
		FROM permission_policy_warning_claims
		WHERE agent = ? AND runtime = ? AND policy = ?
		ORDER BY window_start DESC, rowid DESC
		LIMIT 1`, agent, runtimeName, policy).Scan(
		&claim.Agent, &claim.Runtime, &claim.Policy, &claim.Capability, &claim.WindowStart, &claim.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return PermissionPolicyWarningClaim{}, false, nil
	}
	return claim, err == nil, err
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
