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
	WakeOutboxStatePending    = "pending"
	WakeOutboxStateAttempted  = "attempted"
	WakeOutboxStateDelivered  = "delivered"
	WakeOutboxStateStalled    = "stalled"
	WakeOutboxStateFailed     = "failed"
	WakeOutboxStateSuperseded = "superseded"
	// WakeOutboxStateDeliveryUnknown means a prior process claimed the row but
	// disappeared before recording whether Herdr accepted the wake. It is
	// terminal and MUST NOT be retried blindly.
	WakeOutboxStateDeliveryUnknown = "delivery_unknown"
	wakeOutboxStateCount           = iota
)

const (
	WakeOutboxDeliveryUnknownEventKind = "wake_delivery_unknown"
	// WakeOutboxDeliveryFailedEventKind records a wake that will NOT be retried
	// again: either its cause is not transient or its attempt budget is spent
	// (#1982). It exists so an undelivered obligation is attributable from the
	// store, naming role, cause and attempts, rather than only from a daemon
	// log line nobody reads.
	WakeOutboxDeliveryFailedEventKind = "wake_delivery_failed"

	// WakeOutboxUnroutableEventKind records a wake addressed to a role that
	// CANNOT RECEIVE IT, because no enabled event rule exists for that role and
	// kind. Such a row is not wrong and is not retried: it waits, correctly, for
	// a rule that may never come. What was missing is that it waited SILENTLY,
	// so 49 awaited-fact obligations sat pending from 2026-08-02 and ten
	// escalations addressed to a retired coordinator dead-ended unobserved.
	//
	// Owner decision of 2026-09-08, relayed by phobos, chose making the
	// dead-end LOUD over rerouting it: rerouting would rebuild the coordinator
	// layer that was just retired. This event is the loudness, and it wakes
	// nobody.
	WakeOutboxUnroutableEventKind = "wake_unroutable"
	// WakeOutboxUnroutableRouteRemoved marks a role whose route once existed and
	// was deleted after the row was created: a RETIRED seat, which is usually
	// correct and needs no remedy.
	WakeOutboxUnroutableRouteRemoved = "route_removed"
	// WakeOutboxUnroutableNeverConfigured marks a role with no recorded route
	// history for this kind at all: the 2026-08-02 gap, where the remedy is a
	// route. The two are separated because the remedy differs.
	//
	// LIMIT OF THE DISTINCTION, stated because it is not visible from the
	// record: it rests on the event_rule_deletions tombstones, and that table
	// has no rows before 2026-08-30. A role retired before then reads as
	// never-configured.
	WakeOutboxUnroutableNeverConfigured = "never_configured"

	WakeOutboxKindReply      = "reply"
	WakeOutboxKindBlocked    = "blocked"
	WakeOutboxKindEscalation = "escalation"
	WakeOutboxKindDirective  = "directive"
	WakeOutboxKindFact       = "fact"

	WakeOutboxSourceWorkflowNote = "workflow_note"
	WakeOutboxSourceBlocked      = WakeOutboxKindBlocked
	WakeOutboxSourceEscalation   = WakeOutboxKindEscalation
	WakeOutboxSourceAwaitedFact  = "awaited_fact"

	WakeOutboxReplyCoalescePrefix     = WakeOutboxKindReply + ":"
	WakeOutboxDirectiveCoalescePrefix = WakeOutboxKindDirective + ":"
	WakeOutboxFactCoalescePrefix      = WakeOutboxKindFact + ":"

	WakeOutboxDirectivePhaseAcknowledgment = "acknowledgment"
	WakeOutboxDirectivePhaseCompletion     = "completion"
	WakeOutboxDirectivePhaseTerminal       = "terminal"
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
	{WakeOutboxStateSuperseded, wakeOutboxStateTerminal},
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
	ID             int64
	SourceKind     string
	SourceID       string
	TargetRole     string
	CoalesceKey    string
	CreatedAt      string
	DirectivePhase string
	// DirectiveBody is the directive's own actionable text, projected from the
	// same query that derives the phase (#1981). A prompt that names a row
	// instead of carrying its body forces the seat to fetch, and the fetch is
	// another turn; carrying it is the whole point of the delivery.
	DirectiveBody string
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

// supersedeDirectiveWakeOutboxTx retires a receipt wake in the same transaction
// that records the receipt. A later completion nudge revives the same stable row.
func supersedeDirectiveWakeOutboxTx(ctx context.Context, tx *sql.Tx, directiveID int64, receiptKind string) error {
	_, err := tx.ExecContext(ctx, `
UPDATE wake_outbox
SET state = 'superseded',
	last_error = ?,
	finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
	updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE source_kind = ? AND source_id = ?
	AND coalesce_key LIKE 'directive:%' AND state = 'pending'`,
		"directive "+strings.TrimSpace(receiptKind)+" recorded before wake delivery",
		WakeOutboxSourceWorkflowNote,
		strconv.FormatInt(directiveID, 10),
	)
	if err != nil {
		return fmt.Errorf("supersede directive wake outbox: %w", err)
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

// insertWakeOutboxTx writes one obligation row.
//
// INVARIANT ASYMMETRY, DOCUMENTED BECAUSE IT LOOKS LIKE AN INCONSISTENCY AND IS
// NOT (#1352): DIRECTIVE ROWS KEY A PER-OBLIGATION STATE MACHINE — pending,
// revivable from delivered — WHILE EVERY OTHER KIND KEYS AN EVENT.
//
// The table's implicit assumption is that source_id is event-unique. AT
// event_rule_sink.go — and only there — the non-directive kinds get that free
// from serialized payloads; this is NOT a property of the table, since the
// workflow-note writer passes its own source id. Directives
// alone carry a LITERAL directive id, because the decoder (wakeOutboxEvent)
// renders source_id straight into the command an operator reads back:
// `gitmoot org directive ack <id>`. Storing a payload there instead produced
// `ack {"schema_version":1,...}`.
//
// That literal id is STABLE ACROSS NAGS, so repeated nags are the SAME
// obligation, not new ones — therefore REVIVE rather than insert. Do NOT "tidy"
// the directive branch back to serialized payloads for uniformity: it would
// restore uniqueness and silently recreate the malformed-command defect, which
// cost three review rounds to find.
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
	// #1352: REVIVE SEMANTICS BIND TO THE DIRECTIVE BRANCH ONLY. The other writer
	// on this generic seam — workflow-note insertion — keeps HARD-INSERT
	// semantics, untouched.
	//
	// The reasoning is obligation-specific: directive rows key a PER-OBLIGATION
	// STATE MACHINE, while every other kind keys an EVENT. Extending revive to
	// workflow notes would apply an obligation semantic to
	// things that are not obligations.
	//
	// SCOPE OF THE SAFETY ARGUMENT, stated precisely because a looser version was
	// wrong: Store.InsertWakeOutbox has ONE production caller (Emit in
	// event_rule_sink.go). This generic seam has THREE production paths, so the
	// single-caller argument covers the OUTER function only — which is why the
	// upsert is scoped to the directive kind here rather than applied to the seam.
	if strings.EqualFold(strings.TrimSpace(wakeKind), WakeOutboxKindDirective) {
		// A DELIVERED row REVIVES to pending — the obligation still stands. An
		// ALREADY-PENDING row is LEFT UNTOUCHED (nothing new to say), which gives
		// nag suppression for free. A second row is never created.
		_, err = tx.ExecContext(ctx, `
INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key)
VALUES (?, ?, ?, ?)
ON CONFLICT(source_kind, source_id, target_role) DO UPDATE SET
	state = 'pending',
	coalesce_key = excluded.coalesce_key,
	attempt_count = 0,
	last_error = '',
	attempted_at = NULL,
	finished_at = NULL,
	updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE wake_outbox.state <> 'pending'`,
			sourceKind,
			sourceID,
			role,
			coalesceKey)
	} else {
		_, err = tx.ExecContext(ctx, `
INSERT INTO wake_outbox(source_kind, source_id, target_role, coalesce_key)
VALUES (?, ?, ?, ?)`,
			sourceKind,
			sourceID,
			role,
			coalesceKey)
	}
	if err != nil {
		return err
	}
	return nil
}

func wakeOutboxCoalesceKey(wakeKind, role string) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(wakeKind))
	switch kind {
	case WakeOutboxKindReply, WakeOutboxKindBlocked, WakeOutboxKindEscalation, WakeOutboxKindDirective, WakeOutboxKindFact:
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
		var directivePhase string
		var directiveBody string
		if err := rows.Scan(
			&entry.ID, &entry.SourceKind, &entry.SourceID, &entry.TargetRole,
			&entry.CoalesceKey, &entry.State, &entry.AttemptCount,
			&entry.LastError, &entry.CreatedAt, &entry.AttemptedAt,
			&entry.FinishedAt, &entry.UpdatedAt, &directivePhase, &directiveBody,
		); err != nil {
			return WakeOutboxObligationProjection{}, err
		}
		obligation := WakeOutboxObligation{
			ID: entry.ID, SourceKind: entry.SourceKind, SourceID: entry.SourceID,
			TargetRole: entry.TargetRole, CoalesceKey: entry.CoalesceKey,
			CreatedAt: entry.CreatedAt, DirectivePhase: directivePhase,
			DirectiveBody: directiveBody,
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
		COALESCE(finished_at, ''), updated_at,
		CASE
			WHEN source_kind != 'workflow_note' OR coalesce_key NOT LIKE 'directive:%' THEN ''
			WHEN EXISTS (
				SELECT 1
				FROM workflow_notes d
				JOIN workflow_notes r ON r.workflow_id = d.workflow_id
				WHERE d.id = CAST(wake_outbox.source_id AS INTEGER)
					AND (
						substr(r.body, 1, length('[org:directive-cancel id=' || wake_outbox.source_id || ' ')) = '[org:directive-cancel id=' || wake_outbox.source_id || ' '
						OR substr(r.body, 1, length('[org:directive-done id=' || wake_outbox.source_id || ' ')) = '[org:directive-done id=' || wake_outbox.source_id || ' '
					)
			) THEN 'terminal'
			WHEN EXISTS (
				SELECT 1
				FROM workflow_notes d
				JOIN workflow_notes r ON r.workflow_id = d.workflow_id
				WHERE d.id = CAST(wake_outbox.source_id AS INTEGER)
					AND (
						substr(r.body, 1, length('[org:directive-ack id=' || wake_outbox.source_id || ' ')) = '[org:directive-ack id=' || wake_outbox.source_id || ' '
						OR substr(r.body, 1, length('[org:directive-delivered id=' || wake_outbox.source_id || ' ')) = '[org:directive-delivered id=' || wake_outbox.source_id || ' '
					)
			) THEN 'completion'
			ELSE 'acknowledgment'
		END,
		CASE
			WHEN source_kind != 'workflow_note' OR coalesce_key NOT LIKE 'directive:%' THEN ''
			ELSE COALESCE((
				SELECT d.body FROM workflow_notes d
				WHERE d.id = CAST(wake_outbox.source_id AS INTEGER)
			), '')
		END
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
	// A CLAIMED BATCH IS ONE WAKE, AND THIS SWEEP IS THE SIBLING SEAM THAT DID
	// NOT KNOW IT. #1982 moved the coalescing collapse from claim time to
	// outcome time, which left a whole batch `attempted` until its outcome; this
	// function is the other writer of a terminal state, so a crashed batch aged
	// out as N independent `delivery_unknown` rows. One undelivered wake then
	// counted as N unproven obligations and the record that they were a single
	// wake was lost.
	//
	// The surviving row takes the unknown outcome, because the wake was its own,
	// and the rows it collapsed are superseded into it: the same accounting a
	// delivered or failed batch already uses. Rows are ordered by attempted_at
	// then id, so the first row of each claim group is its survivor.
	seenSurvivor := map[string]int64{}
	for _, entry := range entries {
		group := strings.ToLower(strings.TrimSpace(entry.TargetRole)) + "\x00" +
			entry.CoalesceKey + "\x00" + entry.AttemptedAt
		state, rowDetail := WakeOutboxStateDeliveryUnknown, detail
		survivor, collapsed := seenSurvivor[group]
		if collapsed {
			state, rowDetail = WakeOutboxStateSuperseded, WakeOutboxCoalescedDetail(survivor)
		} else {
			seenSurvivor[group] = entry.ID
		}
		result, err := tx.ExecContext(ctx, `
UPDATE wake_outbox
SET state = ?, last_error = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND state = 'attempted' AND attempted_at <= ?`,
			state, rowDetail, stamp, stamp, entry.ID,
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
		if collapsed {
			// The audit event belongs to the obligation, not to every row it
			// carried: N events for one undelivered wake is the same
			// over-counting in the job-event stream.
			continue
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

// ClaimWakeOutbox atomically claims one coalesced batch. If any member is no
// longer pending, none are claimed; this is the cross-daemon mark-before-emit
// dedup guard.
//
// THE BATCH LEAVES THIS CALL AS ONE OBLIGATION, NOT AS N (#1978), but the
// collapse is recorded at DELIVERY time, not here (#1982). I originally
// superseded the coalesced rows inside this claim and argued it cost nothing
// because every terminal state was terminal and no row was ever re-emitted.
// Retry made that argument false: a stalled batch is now re-attempted, and a
// row already marked `superseded` could not rejoin the retry, so the notes it
// carried would vanish from the next wake. The rows therefore stay `attempted`
// together and FinishWakeOutbox decides their fate from the observed outcome.
func (s *Store) ClaimWakeOutbox(ctx context.Context, surviving int64, coalesced []int64, at time.Time) (bool, error) {
	ids := make([]int64, 0, len(coalesced)+1)
	ids = append(ids, surviving)
	ids = append(ids, coalesced...)
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

// FinishOrRetryWakeOutbox records a NON-delivered outcome for one attempted
// batch (#1982). While `retryBudget` attempts remain and the cause is one the
// caller judged transient, the batch returns to `pending` with its cause
// recorded; otherwise it becomes terminal in `state` and the failure is
// written as a durable, attributable job event naming the target role, the
// cause and the attempts spent.
//
// The decision reads attempt_count inside the same transaction as the write,
// so two daemons cannot both see budget remaining and re-pend the same batch.
func (s *Store) FinishOrRetryWakeOutbox(
	ctx context.Context,
	ids []int64,
	state, cause string,
	retryBudget int,
	at time.Time,
) (retried bool, attempts int, err error) {
	interpretation, ok := interpretWakeOutboxState(state)
	if !ok || interpretation != wakeOutboxStateTerminal {
		return false, 0, fmt.Errorf("invalid terminal wake outbox state %q", state)
	}
	if len(ids) == 0 {
		return false, 0, errors.New("wake outbox outcome requires at least one id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var role string
	if err := tx.QueryRowContext(ctx,
		`SELECT attempt_count, target_role FROM wake_outbox WHERE id = ?`, ids[0],
	).Scan(&attempts, &role); err != nil {
		return false, 0, err
	}
	if attempts < retryBudget {
		query, args, err := wakeOutboxIDUpdateStamps(`
UPDATE wake_outbox
SET state = 'pending', last_error = ?, attempted_at = NULL, finished_at = NULL,
	updated_at = ?
WHERE state = 'attempted' AND id IN (`, ids, at, 1, strings.TrimSpace(cause))
		if err != nil {
			return false, attempts, err
		}
		if err := execWakeOutboxRowUpdate(ctx, tx, query, args, len(ids), "requeue"); err != nil {
			return false, attempts, err
		}
		if err := tx.Commit(); err != nil {
			return false, attempts, err
		}
		return true, attempts, nil
	}
	query, args, err := wakeOutboxIDUpdate(`
UPDATE wake_outbox
SET state = ?, last_error = ?, finished_at = ?, updated_at = ?
WHERE state = 'attempted' AND id IN (`, ids, at, state, strings.TrimSpace(cause))
	if err != nil {
		return false, attempts, err
	}
	if err := execWakeOutboxRowUpdate(ctx, tx, query, args, len(ids), "finish"); err != nil {
		return false, attempts, err
	}
	message := fmt.Sprintf(
		"wake delivery failed for %s: %s (attempts=%d, state=%s)",
		role, strings.TrimSpace(cause), attempts, state,
	)
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
			fmt.Sprintf("wake-outbox:%d", id),
			WakeOutboxDeliveryFailedEventKind,
			message,
		); err != nil {
			return false, attempts, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, attempts, err
	}
	return false, attempts, nil
}

func execWakeOutboxRowUpdate(ctx context.Context, tx *sql.Tx, query string, args []any, want int, op string) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != int64(want) {
		return fmt.Errorf("%s wake outbox updated %d rows, want %d", op, affected, want)
	}
	return nil
}

// WakeOutboxCoalescedDetail is the audit text a suppressed wake carries. It
// names the row that delivered on its behalf, so the saving is countable:
// `SELECT count(*) FROM wake_outbox WHERE last_error LIKE 'coalesced into%'`.
func WakeOutboxCoalescedDetail(surviving int64) string {
	return wakeOutboxCoalescedPrefix + strconv.FormatInt(surviving, 10)
}

// wakeOutboxCoalescedPrefix is the stable prefix a report matches on to count
// suppressed wakes without re-deriving the sentence (#1983).
const wakeOutboxCoalescedPrefix = "coalesced into wake outbox row "

// FinishWakeOutbox records the observed delivery outcome for one attempted
// batch. ids[0] is the SURVIVING row, the one the emitted wake identifies.
//
// On a DELIVERED outcome the survivor is delivered and every row it collapsed
// is recorded `superseded` naming it, so one delivered wake reports exactly
// one delivery and the coalescing saving stays countable (#1978). On any other
// terminal outcome the whole batch carries that outcome, because nothing was
// delivered on anyone's behalf (#1982).
func (s *Store) FinishWakeOutbox(ctx context.Context, ids []int64, state, detail string, at time.Time) error {
	interpretation, ok := interpretWakeOutboxState(state)
	if !ok || interpretation != wakeOutboxStateTerminal {
		return fmt.Errorf("invalid terminal wake outbox state %q", state)
	}
	if state == WakeOutboxStateDelivered && len(ids) > 1 {
		if err := s.finishWakeOutboxRows(ctx, ids[1:], WakeOutboxStateSuperseded, WakeOutboxCoalescedDetail(ids[0]), at); err != nil {
			return err
		}
		return s.finishWakeOutboxRows(ctx, ids[:1], state, detail, at)
	}
	return s.finishWakeOutboxRows(ctx, ids, state, detail, at)
}

func (s *Store) finishWakeOutboxRows(ctx context.Context, ids []int64, state, detail string, at time.Time) error {
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

// wakeOutboxIDUpdate builds an `id IN (...)` update whose SET clause takes the
// `leading` values followed by exactly TWO timestamp placeholders.
func wakeOutboxIDUpdate(prefix string, ids []int64, at time.Time, leading ...string) (string, []any, error) {
	return wakeOutboxIDUpdateStamps(prefix, ids, at, 2, leading...)
}

// wakeOutboxIDUpdateStamps is the same builder with an EXPLICIT number of
// timestamp placeholders. The count is a parameter because it silently has to
// match the statement: a SET clause with one stamp and a builder supplying two
// shifts the id arguments by one, so `id IN (?)` binds a timestamp, the update
// matches zero rows, and the caller reports "updated 0 rows" for a row that is
// sitting in exactly the state it asked for (#1982, found by that symptom).
func wakeOutboxIDUpdateStamps(prefix string, ids []int64, at time.Time, stamps int, leading ...string) (string, []any, error) {
	if len(ids) == 0 {
		return "", nil, errors.New("wake outbox update requires at least one id")
	}
	if stamps < 0 {
		return "", nil, fmt.Errorf("invalid wake outbox stamp count %d", stamps)
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
	args := make([]any, 0, len(leading)+stamps+len(ids))
	for _, value := range leading {
		args = append(args, value)
	}
	for range stamps {
		args = append(args, stamp)
	}
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	return prefix + strings.Join(placeholders, ",") + `)`, args, nil
}
