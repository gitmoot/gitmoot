package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	WakeOutboxStatePending   = "pending"
	WakeOutboxStateAttempted = "attempted"
	WakeOutboxStateDelivered = "delivered"
	WakeOutboxStateStalled   = "stalled"
	WakeOutboxStateFailed    = "failed"
	// WakeOutboxStateDeliveryUnknown means a prior process claimed the row but
	// disappeared before recording whether Herdr accepted the wake. It is
	// terminal and MUST NOT be retried blindly.
	WakeOutboxStateDeliveryUnknown = "delivery_unknown"
	wakeOutboxStateCount           = iota
)

const (
	WakeOutboxDeliveryUnknownEventKind = "wake_delivery_unknown"

	WakeOutboxKindReply      = "reply"
	WakeOutboxKindBlocked    = "blocked"
	WakeOutboxKindEscalation = "escalation"
	WakeOutboxKindDirective  = "directive"

	WakeOutboxSourceWorkflowNote = "workflow_note"
	WakeOutboxSourceChatMessage  = "chat_message"
	WakeOutboxSourceBlocked      = WakeOutboxKindBlocked
	WakeOutboxSourceEscalation   = WakeOutboxKindEscalation

	WakeOutboxReplyCoalescePrefix     = WakeOutboxKindReply + ":"
	WakeOutboxDirectiveCoalescePrefix = WakeOutboxKindDirective + ":"
)

type wakeOutboxStateInterpretation uint8

const (
	wakeOutboxStatePendingObligation wakeOutboxStateInterpretation = iota
	wakeOutboxStateAgedAttemptObligation
	wakeOutboxStateTerminal
	wakeOutboxStateDeliveryUnknown
)

type wakeOutboxStateDefinition struct {
	state          string
	interpretation wakeOutboxStateInterpretation
}

var wakeOutboxStateDefinitions = [...]wakeOutboxStateDefinition{
	{WakeOutboxStatePending, wakeOutboxStatePendingObligation},
	{WakeOutboxStateAttempted, wakeOutboxStateAgedAttemptObligation},
	{WakeOutboxStateDelivered, wakeOutboxStateTerminal},
	{WakeOutboxStateStalled, wakeOutboxStateTerminal},
	{WakeOutboxStateFailed, wakeOutboxStateTerminal},
	{WakeOutboxStateDeliveryUnknown, wakeOutboxStateDeliveryUnknown},
}

// Opposing lengths make an unclassified state or a stray definition fail compilation.
var (
	_ [wakeOutboxStateCount - len(wakeOutboxStateDefinitions)]struct{}
	_ [len(wakeOutboxStateDefinitions) - wakeOutboxStateCount]struct{}
)

// WakeOutboxEntry is one durable delivery item. A pending row is positive,
// queryable evidence that no delivery attempt has happened yet.
type WakeOutboxEntry struct {
	ID           int64  `json:"id"`
	SourceKind   string `json:"source_kind"`
	SourceID     string `json:"source_id"`
	TargetRole   string `json:"target_role"`
	CoalesceKey  string `json:"coalesce_key"`
	State        string `json:"state"`
	AttemptCount int    `json:"attempt_count"`
	LastError    string `json:"last_error,omitempty"`
	CreatedAt    string `json:"created_at"`
	AttemptedAt  string `json:"attempted_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

// WakeOutboxObligation is the row data needed to act on an outstanding wake.
// State is deliberately absent: ListWakeOutboxObligations classifies it before
// returning across the package boundary.
type WakeOutboxObligation struct {
	ID          int64
	SourceKind  string
	SourceID    string
	TargetRole  string
	CoalesceKey string
	CreatedAt   string
}

// WakeOutboxObligationProjection exposes decisions, not persisted states.
type WakeOutboxObligationProjection struct {
	Pending       []WakeOutboxObligation
	AgedAttempted []WakeOutboxObligation
}

func (p WakeOutboxObligationProjection) Len() int {
	return len(p.Pending) + len(p.AgedAttempted)
}

func insertWorkflowNoteWakeOutboxTx(ctx context.Context, tx *sql.Tx, noteID int64, targetRole, wakeKind string) error {
	if err := insertWakeOutboxTx(
		ctx, tx, WakeOutboxSourceWorkflowNote, strconv.FormatInt(noteID, 10),
		wakeKind, targetRole,
	); err != nil {
		return fmt.Errorf("insert workflow note wake outbox: %w", err)
	}
	return nil
}

func insertChatMessageWakeOutboxTx(ctx context.Context, tx *sql.Tx, messageID string, targetRoles []string) error {
	seen := make(map[string]struct{}, len(targetRoles))
	for _, targetRole := range targetRoles {
		role := strings.ToLower(strings.TrimSpace(targetRole))
		if role == "" {
			continue
		}
		if _, exists := seen[role]; exists {
			continue
		}
		seen[role] = struct{}{}
		if err := insertWakeOutboxTx(
			ctx, tx, WakeOutboxSourceChatMessage, messageID, WakeOutboxKindReply, role,
		); err != nil {
			return fmt.Errorf("insert chat message wake outbox: %w", err)
		}
	}
	return nil
}

// InsertWakeOutbox persists one event as a durable obligation for each target
// role. All rows share the event-kind coalescing namespace.
func (s *Store) InsertWakeOutbox(
	ctx context.Context,
	sourceKind, sourceID, wakeKind string,
	targetRoles []string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	seen := make(map[string]struct{}, len(targetRoles))
	for _, targetRole := range targetRoles {
		role := strings.ToLower(strings.TrimSpace(targetRole))
		if role == "" {
			continue
		}
		if _, exists := seen[role]; exists {
			continue
		}
		seen[role] = struct{}{}
		if err := insertWakeOutboxTx(ctx, tx, sourceKind, sourceID, wakeKind, role); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertWakeOutboxTx(ctx context.Context, tx *sql.Tx, sourceKind, sourceID, wakeKind, targetRole string) error {
	sourceKind = strings.TrimSpace(sourceKind)
	if sourceKind == "" {
		return errors.New("wake outbox source kind is required")
	}
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return errors.New("wake outbox source id is required")
	}
	role := strings.ToLower(strings.TrimSpace(targetRole))
	if role == "" {
		return errors.New("wake outbox target role is required")
	}
	coalesceKey, err := wakeOutboxCoalesceKey(wakeKind, role)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key)
VALUES (?, ?, ?, ?)`,
		sourceKind,
		sourceID,
		role,
		coalesceKey)
	if err != nil {
		return err
	}
	return nil
}

func wakeOutboxCoalesceKey(wakeKind, role string) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(wakeKind))
	switch kind {
	case WakeOutboxKindReply, WakeOutboxKindBlocked, WakeOutboxKindEscalation, WakeOutboxKindDirective:
	default:
		return "", fmt.Errorf("unsupported wake outbox kind %q", wakeKind)
	}
	return kind + ":" + strings.ToLower(strings.TrimSpace(role)), nil
}

// ListWakeOutbox returns durable delivery rows in insertion order. An empty
// state returns every row; otherwise it filters by the exact state enum.
func (s *Store) ListWakeOutbox(ctx context.Context, state string) ([]WakeOutboxEntry, error) {
	state = strings.TrimSpace(state)
	query := `
SELECT id, source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, last_error, created_at, COALESCE(attempted_at, ''),
	COALESCE(finished_at, ''), updated_at
FROM wake_outbox`
	args := []any{}
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, state)
	}
	query += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WakeOutboxEntry{}
	for rows.Next() {
		var entry WakeOutboxEntry
		if err := rows.Scan(
			&entry.ID, &entry.SourceKind, &entry.SourceID, &entry.TargetRole,
			&entry.CoalesceKey, &entry.State, &entry.AttemptCount,
			&entry.LastError, &entry.CreatedAt, &entry.AttemptedAt,
			&entry.FinishedAt, &entry.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// ListWakeOutboxObligations is the authoritative health projection for durable
// wake delivery. Pending rows and attempted rows older than attemptedBefore are
// the only non-terminal obligations; terminal outcomes never appear.
func (s *Store) ListWakeOutboxObligations(
	ctx context.Context,
	attemptedBefore time.Time,
) (WakeOutboxObligationProjection, error) {
	return listWakeOutboxObligations(ctx, s.db, attemptedBefore)
}

type wakeOutboxQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func listWakeOutboxObligations(
	ctx context.Context,
	queryer wakeOutboxQueryer,
	attemptedBefore time.Time,
) (WakeOutboxObligationProjection, error) {
	query, args := wakeOutboxObligationQuery(attemptedBefore)
	rows, err := queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return WakeOutboxObligationProjection{}, err
	}
	defer rows.Close()
	out := WakeOutboxObligationProjection{}
	for rows.Next() {
		var entry WakeOutboxEntry
		if err := rows.Scan(
			&entry.ID, &entry.SourceKind, &entry.SourceID, &entry.TargetRole,
			&entry.CoalesceKey, &entry.State, &entry.AttemptCount,
			&entry.LastError, &entry.CreatedAt, &entry.AttemptedAt,
			&entry.FinishedAt, &entry.UpdatedAt,
		); err != nil {
			return WakeOutboxObligationProjection{}, err
		}
		obligation := WakeOutboxObligation{
			ID: entry.ID, SourceKind: entry.SourceKind, SourceID: entry.SourceID,
			TargetRole: entry.TargetRole, CoalesceKey: entry.CoalesceKey,
			CreatedAt: entry.CreatedAt,
		}
		interpretation, ok := interpretWakeOutboxState(entry.State)
		if !ok {
			return WakeOutboxObligationProjection{}, fmt.Errorf(
				"wake outbox obligation row has unclassified state %q", entry.State,
			)
		}
		switch interpretation {
		case wakeOutboxStatePendingObligation:
			out.Pending = append(out.Pending, obligation)
		case wakeOutboxStateAgedAttemptObligation:
			out.AgedAttempted = append(out.AgedAttempted, obligation)
		default:
			return WakeOutboxObligationProjection{}, fmt.Errorf(
				"wake outbox obligation query returned non-obligation state %q", entry.State,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return WakeOutboxObligationProjection{}, err
	}
	return out, nil
}

func wakeOutboxObligationQuery(attemptedBefore time.Time) (string, []any) {
	predicate, args := wakeOutboxObligationPredicate(attemptedBefore)
	return `
SELECT id, source_kind, source_id, target_role, coalesce_key, state,
		attempt_count, last_error, created_at, COALESCE(attempted_at, ''),
		COALESCE(finished_at, ''), updated_at
FROM wake_outbox
WHERE ` + predicate + `
ORDER BY created_at, id`, args
}

// ExpireAgedWakeOutbox marks delivery-unknown rows terminal without re-emitting
// them. Each transition and its audit event share one transaction, so a crash
// cannot silently resolve the obligation without recording the policy outcome.
func (s *Store) ExpireAgedWakeOutbox(ctx context.Context, attemptedBefore, at time.Time) ([]WakeOutboxEntry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
SELECT id, source_kind, source_id, target_role, coalesce_key, state,
	attempt_count, last_error, created_at, COALESCE(attempted_at, ''),
	COALESCE(finished_at, ''), updated_at
FROM wake_outbox
WHERE state = 'attempted' AND attempted_at IS NOT NULL AND attempted_at <= ?
ORDER BY attempted_at, id`,
		attemptedBefore.UTC().Format(BlockedEpisodeTimeLayout))
	if err != nil {
		return nil, err
	}
	var entries []WakeOutboxEntry
	for rows.Next() {
		var entry WakeOutboxEntry
		if err := rows.Scan(
			&entry.ID, &entry.SourceKind, &entry.SourceID, &entry.TargetRole,
			&entry.CoalesceKey, &entry.State, &entry.AttemptCount,
			&entry.LastError, &entry.CreatedAt, &entry.AttemptedAt,
			&entry.FinishedAt, &entry.UpdatedAt,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, tx.Commit()
	}

	stamp := at.UTC().Format(BlockedEpisodeTimeLayout)
	const detail = "delivery outcome unknown after attempted wake aged out; not retried"
	for _, entry := range entries {
		result, err := tx.ExecContext(ctx, `
UPDATE wake_outbox
SET state = ?, last_error = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND state = 'attempted' AND attempted_at <= ?`,
			WakeOutboxStateDeliveryUnknown, detail, stamp, stamp, entry.ID,
			attemptedBefore.UTC().Format(BlockedEpisodeTimeLayout))
		if err != nil {
			return nil, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if affected != 1 {
			return nil, fmt.Errorf("expire wake outbox row %d updated %d rows, want 1", entry.ID, affected)
		}
		message := fmt.Sprintf(
			"source=%s:%s target_role=%s attempted_at=%s policy=expire_without_retry",
			entry.SourceKind, entry.SourceID, entry.TargetRole, entry.AttemptedAt,
		)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
			fmt.Sprintf("wake-outbox:%d", entry.ID),
			WakeOutboxDeliveryUnknownEventKind,
			message,
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return entries, nil
}

// ClaimWakeOutbox atomically marks an entire coalesced batch attempted. If any
// member is no longer pending, none are claimed; this is the cross-daemon
// mark-before-emit dedup guard.
func (s *Store) ClaimWakeOutbox(ctx context.Context, ids []int64, at time.Time) (bool, error) {
	if len(ids) == 0 {
		return false, errors.New("wake outbox claim requires at least one id")
	}
	query, args, err := wakeOutboxIDUpdate(`
UPDATE wake_outbox
SET state = 'attempted', attempt_count = attempt_count + 1,
	attempted_at = ?, finished_at = NULL, last_error = '', updated_at = ?
WHERE state = 'pending' AND id IN (`, ids, at)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != int64(len(ids)) {
		return false, tx.Rollback()
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// FinishWakeOutbox records the existing event-rule delivery classification for
// every row in one attempted batch.
func (s *Store) FinishWakeOutbox(ctx context.Context, ids []int64, state, detail string, at time.Time) error {
	interpretation, ok := interpretWakeOutboxState(state)
	if !ok || interpretation != wakeOutboxStateTerminal {
		return fmt.Errorf("invalid terminal wake outbox state %q", state)
	}
	query, args, err := wakeOutboxIDUpdate(`
UPDATE wake_outbox
SET state = ?, last_error = ?, finished_at = ?, updated_at = ?
WHERE state = 'attempted' AND id IN (`, ids, at, state, strings.TrimSpace(detail))
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != int64(len(ids)) {
		return fmt.Errorf("finish wake outbox updated %d rows, want %d", affected, len(ids))
	}
	return nil
}

func interpretWakeOutboxState(state string) (wakeOutboxStateInterpretation, bool) {
	for _, definition := range wakeOutboxStateDefinitions {
		if state == definition.state {
			return definition.interpretation, true
		}
	}
	return 0, false
}

func wakeOutboxObligationPredicate(attemptedBefore time.Time) (string, []any) {
	var clauses []string
	var args []any
	for _, definition := range wakeOutboxStateDefinitions {
		interpretation, ok := interpretWakeOutboxState(definition.state)
		if !ok {
			panic(fmt.Sprintf("wake outbox state %q has no interpretation", definition.state))
		}
		switch interpretation {
		case wakeOutboxStatePendingObligation:
			clauses = append(clauses, "state = ?")
			args = append(args, definition.state)
		case wakeOutboxStateAgedAttemptObligation:
			clauses = append(clauses, "(state = ? AND attempted_at IS NOT NULL AND attempted_at <= ?)")
			args = append(args, definition.state)
			args = append(args, attemptedBefore.UTC().Format(BlockedEpisodeTimeLayout))
		case wakeOutboxStateTerminal, wakeOutboxStateDeliveryUnknown:
			continue
		default:
			panic(fmt.Sprintf("wake outbox state %q has invalid interpretation %d", definition.state, interpretation))
		}
	}
	return strings.Join(clauses, " OR "), args
}

func wakeOutboxIDUpdate(prefix string, ids []int64, at time.Time, leading ...string) (string, []any, error) {
	if len(ids) == 0 {
		return "", nil, errors.New("wake outbox update requires at least one id")
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return "", nil, fmt.Errorf("invalid wake outbox id %d", id)
		}
		if _, exists := seen[id]; exists {
			return "", nil, fmt.Errorf("duplicate wake outbox id %d", id)
		}
		seen[id] = struct{}{}
	}
	stamp := at.UTC().Format(BlockedEpisodeTimeLayout)
	args := make([]any, 0, len(leading)+2+len(ids))
	for _, value := range leading {
		args = append(args, value)
	}
	args = append(args, stamp, stamp)
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	return prefix + strings.Join(placeholders, ",") + `)`, args, nil
}
