package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EscalationRoundOutcome names the three ways an attempt to OPEN a human round
// ends. A caller that cannot tell them apart either announces for a loser or writes
// a landed-work audit event about a task it never touched — intent a boolean pair
// cannot carry (#1673).
type EscalationRoundOutcome int

const (
	// EscalationRoundBlocked: the coordinator already has an UNSETTLED round. That
	// covers a live pause AND a claimed round whose effects have not landed, because
	// the slot is held until settlement. Zero writes, nothing to announce.
	EscalationRoundBlocked EscalationRoundOutcome = iota
	// EscalationRoundOpened: this caller won the job-level slot and, unless taskless,
	// paused the task.
	EscalationRoundOpened
	// EscalationRoundRefused: this caller won the slot but its guarded task transition
	// was refused (a merged or disposed row), so the whole transaction rolled back and
	// the slot is free again. Only this outcome owes a classification of the row.
	EscalationRoundRefused
)

// EscalationRoundIntegrityNeedsRepair is the single terminal integrity state. A round
// in it PRESERVES its claim, keeps the slot (effects_completed_at stays NULL), and
// blocks both a new round and ordinary advance until an operator acts.
const EscalationRoundIntegrityNeedsRepair = "needs_repair"

// ErrEscalationRoundNotRepairable is returned by the operator repair arms when no
// needs_repair round matches — a wrong id, or one another operator already settled.
var ErrEscalationRoundNotRepairable = errors.New("no repairable escalation round")

// EscalationRound is one round's durable row.
type EscalationRound struct {
	JobID              string
	RoundID            string
	Kind               string
	OpenedAt           string
	ResolvedAt         string
	ClaimVerb          string
	ClaimGeneration    int64
	ClaimPayload       string
	EffectsCompletedAt string
	SettledReason      string
	SettledBy          string
	IntegrityState     string
	IntegrityCause     string
	RecoveryAttempts   int64
}

// Claimed reports whether this round's resolution has been claimed.
func (r EscalationRound) Claimed() bool { return strings.TrimSpace(r.ResolvedAt) != "" }

// NeedsRepair reports whether this round sits in the terminal integrity state.
func (r EscalationRound) NeedsRepair() bool {
	return r.IntegrityState == EscalationRoundIntegrityNeedsRepair
}

const escalationRoundColumns = `job_id, round_id, kind, opened_at, COALESCE(resolved_at, ''), claim_verb,
	COALESCE(claim_generation, 0), claim_payload, COALESCE(effects_completed_at, ''), settled_reason,
	settled_by, integrity_state, integrity_cause, recovery_attempts`

func scanEscalationRound(scan func(...any) error) (EscalationRound, error) {
	var round EscalationRound
	err := scan(&round.JobID, &round.RoundID, &round.Kind, &round.OpenedAt, &round.ResolvedAt,
		&round.ClaimVerb, &round.ClaimGeneration, &round.ClaimPayload, &round.EffectsCompletedAt,
		&round.SettledReason, &round.SettledBy, &round.IntegrityState, &round.IntegrityCause,
		&round.RecoveryAttempts)
	return round, err
}

// insertEscalationRoundTx takes the coordinator's ONLY unsettled-round slot. The
// partial unique index escalation_rounds_one_unsettled is the exclusion, so a second
// live round is a UNIQUE violation rather than a race the caller has to detect.
//
// It returns false when the slot is already taken; every other error is returned.
func insertEscalationRoundTx(ctx context.Context, tx *sql.Tx, jobID string, roundID string, kind string, now time.Time) (bool, error) {
	_, err := tx.ExecContext(ctx, `INSERT INTO escalation_rounds(job_id, round_id, kind, opened_at)
		VALUES (?, ?, ?, ?)`, strings.TrimSpace(jobID), strings.TrimSpace(roundID), strings.TrimSpace(kind), formatResourceLockTime(now))
	if err == nil {
		return true, nil
	}
	if isUniqueConstraintError(err) {
		return false, nil
	}
	return false, err
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "constraint failed: unique")
}

// GetEscalationRound reads one round by identity. Recovery reads ONLY by identity so
// it can never pair one round's request with another round's resolution.
func (s *Store) GetEscalationRound(ctx context.Context, jobID string, roundID string) (EscalationRound, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+escalationRoundColumns+` FROM escalation_rounds
		WHERE job_id = ? AND round_id = ?`, strings.TrimSpace(jobID), strings.TrimSpace(roundID))
	return scanEscalationRound(row.Scan)
}

// UnsettledEscalationRound returns the coordinator's current unsettled round, if any.
// It is the single source of truth for "is this job blocked", used by the opener's
// diagnostics, the advance block and the operator report.
func (s *Store) UnsettledEscalationRound(ctx context.Context, jobID string) (EscalationRound, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+escalationRoundColumns+` FROM escalation_rounds
		WHERE job_id = ? AND effects_completed_at IS NULL`, strings.TrimSpace(jobID))
	round, err := scanEscalationRound(row.Scan)

	if errors.Is(err, sql.ErrNoRows) {
		return EscalationRound{}, false, nil
	}
	if err != nil {
		return EscalationRound{}, false, err
	}
	return round, true, nil
}

// ClaimEscalationRound claims a round's resolution. rows=1 is the winner, and it is
// the ONLY caller allowed to run the verb's irreversible effects. It does NOT free
// the slot: settlement does.
func (s *Store) ClaimEscalationRound(ctx context.Context, jobID string, roundID string, verb string, generation int64, payload string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE escalation_rounds
		SET resolved_at = ?, claim_verb = ?, claim_generation = ?, claim_payload = ?
		WHERE job_id = ? AND round_id = ? AND resolved_at IS NULL AND effects_completed_at IS NULL`,
		formatResourceLockTime(now), strings.TrimSpace(verb), generation, payload,
		strings.TrimSpace(jobID), strings.TrimSpace(roundID))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// SettleEscalationRound writes the receipt and releases the slot. rows=1 settles;
// rows=0 means another recoverer already did, so concurrent recoverers cannot
// over-count — there is nothing to count.
//
// reason and by are recorded for settlements that did NOT apply effects (a Class I
// no-op release, or an operator supersede); an ordinary settlement leaves them empty.
func (s *Store) SettleEscalationRound(ctx context.Context, jobID string, roundID string, reason string, by string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE escalation_rounds
		SET effects_completed_at = ?, settled_reason = ?, settled_by = ?
		WHERE job_id = ? AND round_id = ? AND effects_completed_at IS NULL`,
		formatResourceLockTime(now), strings.TrimSpace(reason), strings.TrimSpace(by),
		strings.TrimSpace(jobID), strings.TrimSpace(roundID))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// RecordEscalationRoundAttempt counts one replay attempt. The count exists only to
// decide when to STOP RETRYING AND ASK A HUMAN; no value of it can discard a claim.
func (s *Store) RecordEscalationRoundAttempt(ctx context.Context, jobID string, roundID string) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE escalation_rounds
		SET recovery_attempts = recovery_attempts + 1
		WHERE job_id = ? AND round_id = ? AND effects_completed_at IS NULL`,
		strings.TrimSpace(jobID), strings.TrimSpace(roundID)); err != nil {
		return 0, err
	}
	var attempts int64
	if err := s.db.QueryRowContext(ctx, `SELECT recovery_attempts FROM escalation_rounds
		WHERE job_id = ? AND round_id = ?`, strings.TrimSpace(jobID), strings.TrimSpace(roundID)).Scan(&attempts); err != nil {
		return 0, err
	}
	return attempts, nil
}

// MarkEscalationRoundNeedsRepair enters the terminal integrity state and, in the SAME
// transaction, appends the single repair signal. The affected-row predicate is what
// makes that signal exactly-once across repeated sweeps.
//
// It never touches effects_completed_at: the claim is preserved and the slot stays
// held, which is what blocks a new round and ordinary advance.
func (s *Store) MarkEscalationRoundNeedsRepair(ctx context.Context, jobID string, roundID string, cause string, event JobEvent, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE escalation_rounds
		SET integrity_state = ?, integrity_cause = ?, integrity_at = ?
		WHERE job_id = ? AND round_id = ? AND integrity_state = '' AND effects_completed_at IS NULL`,
		EscalationRoundIntegrityNeedsRepair, strings.TrimSpace(cause), formatResourceLockTime(now),
		strings.TrimSpace(jobID), strings.TrimSpace(roundID))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != 1 {
		return false, tx.Commit()
	}
	if strings.TrimSpace(event.JobID) == "" {
		event.JobID = strings.TrimSpace(jobID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
		event.JobID, event.Kind, event.Message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// RepairRetryEscalationRound is the operator arm that PRESERVES the decision: it
// clears the integrity state and resets the attempt count so the next sweep replays
// the stored claim. The slot is held throughout, so a transient cause costs a poll
// rather than the human's instruction.
func (s *Store) RepairRetryEscalationRound(ctx context.Context, jobID string, roundID string, event JobEvent) (EscalationRound, error) {
	return s.repairEscalationRound(ctx, jobID, roundID, event, `UPDATE escalation_rounds
		SET integrity_state = '', integrity_cause = '', integrity_at = NULL, recovery_attempts = 0
		WHERE job_id = ? AND round_id = ? AND integrity_state = ? AND effects_completed_at IS NULL`, nil)
}

// RepairSupersedeEscalationRound is the ONLY path that discards a claimed human
// decision. It settles the round WITHOUT applying effects, records the operator and
// their reason, and releases the slot — so intent can be dropped only by a human
// saying so, on the record, never by machinery running out of attempts.
func (s *Store) RepairSupersedeEscalationRound(ctx context.Context, jobID string, roundID string, reason string, by string, now time.Time, event JobEvent) (EscalationRound, error) {
	return s.repairEscalationRound(ctx, jobID, roundID, event, `UPDATE escalation_rounds
		SET effects_completed_at = ?, settled_reason = ?, settled_by = ?
		WHERE job_id = ? AND round_id = ? AND integrity_state = ? AND effects_completed_at IS NULL`,
		[]any{formatResourceLockTime(now), strings.TrimSpace(reason), strings.TrimSpace(by)})
}

func (s *Store) repairEscalationRound(ctx context.Context, jobID string, roundID string, event JobEvent, statement string, leadingArgs []any) (EscalationRound, error) {
	jobID = strings.TrimSpace(jobID)
	roundID = strings.TrimSpace(roundID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EscalationRound{}, err
	}
	defer tx.Rollback()
	args := append(append([]any{}, leadingArgs...), jobID, roundID, EscalationRoundIntegrityNeedsRepair)
	result, err := tx.ExecContext(ctx, statement, args...)
	if err != nil {
		return EscalationRound{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return EscalationRound{}, err
	}
	if affected != 1 {
		return EscalationRound{}, fmt.Errorf("%w: job %s round %s", ErrEscalationRoundNotRepairable, jobID, roundID)
	}
	if strings.TrimSpace(event.JobID) == "" {
		event.JobID = jobID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
		event.JobID, event.Kind, event.Message); err != nil {
		return EscalationRound{}, err
	}
	row := tx.QueryRowContext(ctx, `SELECT `+escalationRoundColumns+` FROM escalation_rounds
		WHERE job_id = ? AND round_id = ?`, jobID, roundID)
	round, err := scanEscalationRound(row.Scan)
	if err != nil {
		return EscalationRound{}, err
	}
	return round, tx.Commit()
}

// UnfinishedEscalationRounds returns rounds whose resolution was CLAIMED but whose
// effects never landed, EXCLUDING those parked in needs_repair (terminal until an
// operator acts, so the sweep must not churn on them).
//
// Legacy rounds — every escalation resolved before this table existed — have NO ROW
// here, so they can never be candidates: zero replay by construction rather than by
// a predicate somebody could weaken.
func (s *Store) UnfinishedEscalationRounds(ctx context.Context) ([]EscalationRound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+escalationRoundColumns+` FROM escalation_rounds
		WHERE resolved_at IS NOT NULL AND effects_completed_at IS NULL AND integrity_state = ''
		ORDER BY job_id, round_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EscalationRound
	for rows.Next() {
		round, err := scanEscalationRound(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, round)
	}
	return out, rows.Err()
}

// EscalationRoundsNeedingRepair lists every parked round, for the operator report and
// the attention surface: a blocked coordinator must never be silent.
func (s *Store) EscalationRoundsNeedingRepair(ctx context.Context) ([]EscalationRound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+escalationRoundColumns+` FROM escalation_rounds
		WHERE integrity_state = ? AND effects_completed_at IS NULL
		ORDER BY job_id, round_id`, EscalationRoundIntegrityNeedsRepair)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EscalationRound
	for rows.Next() {
		round, err := scanEscalationRound(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, round)
	}
	return out, rows.Err()
}

// JobExists reports whether a coordinator row is still present. It is the ONE
// structural-impossibility test: with no coordinator there is no DAG to pause, no
// task to move and no continuation to enqueue, so a claim against it can never be
// replayed by anyone.
func (s *Store) JobExists(ctx context.Context, jobID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id = ?`, strings.TrimSpace(jobID)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AdoptLegacyEscalationRound gives a PRE-EXISTING open round a row so it can be
// resolved by the one mechanism instead of a second legacy code path (#1673).
//
// It is only ever called for a coordinator the caller has already established has an
// open legacy round in job_events, and it is idempotent because the partial unique
// index refuses a second unsettled row. Adoption is NOT a backfill: it happens on
// first touch, for one round, and never invents identities for historical claims
// that are already resolved - those have no row and can never enter recovery.
func (s *Store) AdoptLegacyEscalationRound(ctx context.Context, jobID string, roundID string, kind string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	took, err := insertEscalationRoundTx(ctx, tx, jobID, roundID, kind, now)
	if err != nil {
		return false, err
	}
	if !took {
		return false, tx.Commit()
	}
	return true, tx.Commit()
}

// ExecForTest runs a raw statement against this store. It exists so a test can put
// the database into a state no production code path creates - notably a round whose
// coordinator row is absent, which is the ONE structurally impossible case the
// recovery sweep must release. Production code must never call it.
func (s *Store) ExecForTest(ctx context.Context, statement string, args ...any) error {
	_, err := s.db.ExecContext(ctx, statement, args...)
	return err
}
