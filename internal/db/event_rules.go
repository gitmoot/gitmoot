package db

import (
	"context"
	"errors"
	"strings"
	"time"
)

// EventRule binds one classified daemon event kind to an organization role.
// MatchFilter is v1 plain text, not JSON: the evaluator applies it as a
// case-insensitive substring against the event repo and job id.
type EventRule struct {
	ID          string
	OnKind      string
	MatchFilter string
	WakeRole    string
	Enabled     bool
	CreatedAt   string
}

// AddEventRule persists a new opt-in event rule.
func (s *Store) AddEventRule(ctx context.Context, rule EventRule) error {
	rule.ID = strings.TrimSpace(rule.ID)
	rule.OnKind = strings.TrimSpace(rule.OnKind)
	rule.WakeRole = strings.TrimSpace(rule.WakeRole)
	if rule.ID == "" {
		return errors.New("event rule id is required")
	}
	if rule.OnKind == "" {
		return errors.New("event rule on_kind is required")
	}
	if rule.WakeRole == "" {
		return errors.New("event rule wake_role is required")
	}
	if strings.TrimSpace(rule.CreatedAt) == "" {
		// Fixed-width nanoseconds (not RFC3339Nano, which trims trailing zeros) so
		// the lexical `ORDER BY created_at` in ListEventRules matches true
		// chronological order even for rules added within the same second.
		rule.CreatedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	enabled := 0
	if rule.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO event_rules(
		id, on_kind, match_filter, wake_role, enabled, created_at
	) VALUES (?, ?, ?, ?, ?, ?)`,
		rule.ID, rule.OnKind, strings.TrimSpace(rule.MatchFilter), rule.WakeRole,
		enabled, rule.CreatedAt)
	return err
}

// ListEventRules returns all rules in stable creation/id order.
func (s *Store) ListEventRules(ctx context.Context) ([]EventRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, on_kind, COALESCE(match_filter, ''), wake_role, enabled, created_at
		FROM event_rules ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []EventRule{}
	for rows.Next() {
		var rule EventRule
		var enabled int
		if err := rows.Scan(&rule.ID, &rule.OnKind, &rule.MatchFilter, &rule.WakeRole, &enabled, &rule.CreatedAt); err != nil {
			return nil, err
		}
		rule.Enabled = enabled != 0
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

// DeleteEventRule removes a rule by id. Removing an already-absent id is an
// idempotent no-op, which keeps best-effort operator cleanup simple.
func (s *Store) DeleteEventRule(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM event_rules WHERE id = ?`, strings.TrimSpace(id))
	return err
}
