package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type EventRuleScope string

const (
	EventRuleScopeAddressed EventRuleScope = "addressed"
	EventRuleScopeObserver  EventRuleScope = "observer"
)

// EventRule binds one classified daemon event kind to an organization role.
// MatchFilter is v1 plain text, not JSON: the evaluator applies it as a
// case-insensitive substring against the event repo and job id.
type EventRule struct {
	ID          string
	OnKind      string
	MatchFilter string
	WakeRole    string
	Scope       EventRuleScope
	Enabled     bool
	CreatedAt   string
}

// AddEventRule persists a new opt-in event rule.
func (s *Store) AddEventRule(ctx context.Context, rule EventRule) error {
	rule, err := normalizeEventRule(rule)
	if err != nil {
		return err
	}
	return insertEventRule(ctx, s.db, rule)
}

// AddEventRules persists a set of rules in one transaction. A conflict or
// invalid rule leaves the complete set unchanged.
func (s *Store) AddEventRules(ctx context.Context, rules []EventRule) error {
	if len(rules) == 0 {
		return nil
	}
	normalized := make([]EventRule, len(rules))
	for i, rule := range rules {
		var err error
		normalized[i], err = normalizeEventRule(rule)
		if err != nil {
			return fmt.Errorf("event rule %d: %w", i, err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, rule := range normalized {
		if err := insertEventRule(ctx, tx, rule); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func normalizeEventRule(rule EventRule) (EventRule, error) {
	rule.ID = strings.TrimSpace(rule.ID)
	rule.OnKind = strings.TrimSpace(rule.OnKind)
	rule.WakeRole = strings.TrimSpace(rule.WakeRole)
	rule.Scope = EventRuleScope(strings.ToLower(strings.TrimSpace(string(rule.Scope))))
	if rule.Scope == "" {
		rule.Scope = EventRuleScopeAddressed
	}
	if rule.ID == "" {
		return EventRule{}, errors.New("event rule id is required")
	}
	if rule.OnKind == "" {
		return EventRule{}, errors.New("event rule on_kind is required")
	}
	if rule.WakeRole == "" {
		return EventRule{}, errors.New("event rule wake_role is required")
	}
	if rule.Scope != EventRuleScopeAddressed && rule.Scope != EventRuleScopeObserver {
		return EventRule{}, fmt.Errorf("event rule scope %q is invalid; want addressed or observer", rule.Scope)
	}
	if strings.TrimSpace(rule.CreatedAt) == "" {
		// Fixed-width nanoseconds (not RFC3339Nano, which trims trailing zeros) so
		// the lexical `ORDER BY created_at` in ListEventRules matches true
		// chronological order even for rules added within the same second.
		rule.CreatedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	rule.MatchFilter = strings.TrimSpace(rule.MatchFilter)
	return rule, nil
}

func insertEventRule(ctx context.Context, execer sqlExecer, rule EventRule) error {
	enabled := 0
	if rule.Enabled {
		enabled = 1
	}
	_, err := execer.ExecContext(ctx, `INSERT INTO event_rules(
		id, on_kind, match_filter, wake_role, scope, enabled, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rule.ID, rule.OnKind, rule.MatchFilter, rule.WakeRole,
		rule.Scope, enabled, rule.CreatedAt)
	return err
}

// UpdateEventRuleScope changes only the routing scope of an existing rule.
func (s *Store) UpdateEventRuleScope(ctx context.Context, id string, scope EventRuleScope) error {
	id = strings.TrimSpace(id)
	scope = EventRuleScope(strings.ToLower(strings.TrimSpace(string(scope))))
	if id == "" {
		return errors.New("event rule id is required")
	}
	if scope != EventRuleScopeAddressed && scope != EventRuleScopeObserver {
		return fmt.Errorf("event rule scope %q is invalid; want addressed or observer", scope)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE event_rules SET scope = ? WHERE id = ?`, scope, id)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return fmt.Errorf("event rule %q not found", id)
	}
	return nil
}

// ListEventRules returns all rules in stable creation/id order.
func (s *Store) ListEventRules(ctx context.Context) ([]EventRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, on_kind, COALESCE(match_filter, ''), wake_role, scope, enabled, created_at
		FROM event_rules ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []EventRule{}
	for rows.Next() {
		var rule EventRule
		var enabled int
		if err := rows.Scan(&rule.ID, &rule.OnKind, &rule.MatchFilter, &rule.WakeRole, &rule.Scope, &enabled, &rule.CreatedAt); err != nil {
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

// DeleteEventRulesForRole removes all routes targeting role in one transaction
// and returns their complete rows so a coordinating caller can compensate.
func (s *Store) DeleteEventRulesForRole(ctx context.Context, role string) ([]EventRule, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return nil, errors.New("event rule wake_role is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, on_kind, COALESCE(match_filter, ''), wake_role, scope, enabled, created_at
		FROM event_rules
		WHERE LOWER(TRIM(wake_role)) = ?
		ORDER BY created_at, id`, role)
	if err != nil {
		return nil, err
	}
	rules := []EventRule{}
	for rows.Next() {
		var rule EventRule
		var enabled int
		if err := rows.Scan(&rule.ID, &rule.OnKind, &rule.MatchFilter, &rule.WakeRole, &rule.Scope, &enabled, &rule.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		rule.Enabled = enabled != 0
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_rules WHERE LOWER(TRIM(wake_role)) = ?`, role); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rules, nil
}
