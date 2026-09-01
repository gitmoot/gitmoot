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
	RecoveryOwner      string
	RecoveryLeaseUntil string
	// PreEffect* record the resources this round's replay allocated OUTSIDE the effect
	// transaction (a delegation worktree, a branch lock). They exist so a supersede or
	// a Class I release can hand them back rather than orphaning them.
	PreEffectRepo         string
	PreEffectBranch       string
	PreEffectWorktreePath string
	PreEffectLockOwner    string
}

// Claimed reports whether this round's resolution has been claimed.
func (r EscalationRound) Claimed() bool { return strings.TrimSpace(r.ResolvedAt) != "" }

// NeedsRepair reports whether this round sits in the terminal integrity state.
func (r EscalationRound) NeedsRepair() bool {
	return r.IntegrityState == EscalationRoundIntegrityNeedsRepair
}

const escalationRoundColumns = `job_id, round_id, kind, opened_at, COALESCE(resolved_at, ''), claim_verb,
	COALESCE(claim_generation, 0), claim_payload, COALESCE(effects_completed_at, ''), settled_reason,
	settled_by, integrity_state, integrity_cause, recovery_attempts, recovery_owner,
	COALESCE(recovery_lease_until, ''), preeffect_repo, preeffect_branch, preeffect_worktree_path,
	preeffect_lock_owner`

func scanEscalationRound(scan func(...any) error) (EscalationRound, error) {
	var round EscalationRound
	err := scan(&round.JobID, &round.RoundID, &round.Kind, &round.OpenedAt, &round.ResolvedAt,
		&round.ClaimVerb, &round.ClaimGeneration, &round.ClaimPayload, &round.EffectsCompletedAt,
		&round.SettledReason, &round.SettledBy, &round.IntegrityState, &round.IntegrityCause,
		&round.RecoveryAttempts, &round.RecoveryOwner, &round.RecoveryLeaseUntil,
		&round.PreEffectRepo, &round.PreEffectBranch, &round.PreEffectWorktreePath,
		&round.PreEffectLockOwner)
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

// AcquireEscalationRecoveryLease is THE FENCE. Recovery is exclusively owned through
// effect commit: only the holder may run pre-effects, apply effects, park the round or
// settle it (#1673).
//
// ORDER MATTERS AND IS PART OF THE DESIGN: the fence is taken BEFORE the pre-effects,
// so two recoverers can never both allocate a worktree or both try to take a branch
// lock - an idempotent worktree key does not protect a lock that has an owner.
//
// A lease is taken only on an UNPARKED, UNSETTLED round, and an expired lease is
// reclaimable so a crashed owner cannot wedge the round. Expiry transfers ownership
// ONLY: it never settles, never discards, and never touches the preserved claim.
func (s *Store) AcquireEscalationRecoveryLease(ctx context.Context, jobID string, roundID string, owner string, until time.Time, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return false, errors.New("recovery lease owner is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE escalation_rounds
		SET recovery_owner = ?, recovery_lease_until = ?
		WHERE job_id = ? AND round_id = ?
		  AND effects_completed_at IS NULL
		  AND integrity_state = ''
		  AND (recovery_owner = '' OR recovery_lease_until IS NULL OR recovery_lease_until <= ?)`,
		owner, formatResourceLockTime(until), strings.TrimSpace(jobID), strings.TrimSpace(roundID),
		formatResourceLockTime(now))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// ReleaseEscalationRecoveryLease drops a lease this owner still holds so a finished
// pass does not make the round wait out its lease.
func (s *Store) ReleaseEscalationRecoveryLease(ctx context.Context, jobID string, roundID string, owner string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE escalation_rounds
		SET recovery_owner = '', recovery_lease_until = NULL
		WHERE job_id = ? AND round_id = ? AND recovery_owner = ? AND effects_completed_at IS NULL`,
		strings.TrimSpace(jobID), strings.TrimSpace(roundID), strings.TrimSpace(owner))
	return err
}

// RecordEscalationRoundPreEffects durably records the resources a replay allocated
// outside the effect transaction, under the held fence. It is what lets a later
// supersede or Class I release hand those resources back instead of orphaning them.
func (s *Store) RecordEscalationRoundPreEffects(ctx context.Context, jobID string, roundID string, owner string, repo string, branch string, worktreePath string, lockOwner string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE escalation_rounds
		SET preeffect_repo = ?, preeffect_branch = ?, preeffect_worktree_path = ?, preeffect_lock_owner = ?
		WHERE job_id = ? AND round_id = ? AND effects_completed_at IS NULL
		  AND recovery_owner = ? AND recovery_lease_until > ?`,
		strings.TrimSpace(repo), strings.TrimSpace(branch), strings.TrimSpace(worktreePath),
		strings.TrimSpace(lockOwner), strings.TrimSpace(jobID), strings.TrimSpace(roundID),
		strings.TrimSpace(owner), formatResourceLockTime(now))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// ResolutionCommit is EVERY durable write one resolution performs. It exists so they
// commit in ONE lease-guarded transaction: there is then no "after effect N, before
// receipt" state for a crash to land in, which is what a per-effect boundary could not
// give (#1673).
type ResolutionCommit struct {
	JobID   string
	RoundID string
	Owner   string
	// Jobs are prepared rows: PREPARE did every non-write step, so nothing here needs
	// computation, network, git or policy resolution inside the transaction.
	Jobs []PreparedJob
	// Task, when TaskID is set, is the task-state transition plus its event.
	Task           Task
	TaskForbidden  []string
	TaskEvent      TaskEvent
	TaskEventValid bool
	// Events are the verb's own job events, including the isolation-skipped note, which
	// is a job_events row and therefore belongs INSIDE the transaction.
	Events []JobEvent
	// Receipt settles the round and releases its slot. Omitted (ReceiptValid false)
	// when the transaction's outcome is the ALTERNATIVE one - a blocked allocation -
	// where the claim must stay preserved so a crash-replay cannot double-block.
	Receipt      JobEvent
	ReceiptValid bool
}

// PreparedJob is one job row plus the events that must land with it.
type PreparedJob struct {
	Job    Job
	Events []JobEvent
}

// CommitResolutionEffects commits a resolution in ONE transaction, with the fence
// validated inside it. Zero affected rows on the fence check ABORTS EVERYTHING: no
// job, no task write, no event, no receipt, and the claim preserved.
//
// Every write goes through tx-scoped statements taking this transaction handle; no
// effect opens its own transaction or connection.
func (s *Store) CommitResolutionEffects(ctx context.Context, commit ResolutionCommit, now time.Time) (bool, error) {
	jobID := strings.TrimSpace(commit.JobID)
	roundID := strings.TrimSpace(commit.RoundID)
	owner := strings.TrimSpace(commit.Owner)
	if jobID == "" || roundID == "" || owner == "" {
		return false, errors.New("resolution commit requires job id, round id and fence owner")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// THE FENCE, validated inside the transaction that commits the effects.
	fence, err := tx.ExecContext(ctx, `UPDATE escalation_rounds
		SET recovery_lease_until = ?
		WHERE job_id = ? AND round_id = ? AND effects_completed_at IS NULL
		  AND recovery_owner = ? AND recovery_lease_until > ?`,
		formatResourceLockTime(now.Add(2*time.Minute)), jobID, roundID, owner, formatResourceLockTime(now))
	if err != nil {
		return false, err
	}
	held, err := fence.RowsAffected()
	if err != nil {
		return false, err
	}
	if held != 1 {
		// Lost the fence: an operator superseded the round, it was parked, or the lease
		// lapsed. Nothing may land.
		return false, nil
	}

	if strings.TrimSpace(commit.Task.ID) != "" {
		// THE GUARD DECIDES THE WHOLE TRANSACTION. A refused task write means another
		// worker moved the task to a terminal state (merged, dismissed, superseded)
		// between capture and commit. Committing anyway would dispatch stale work
		// against a finished lifecycle AND record the human decision as applied, so a
		// zero-row guard aborts everything - jobs, events and receipt included (#1673).
		written, err := s.upsertTaskUnlessStates(ctx, tx, commit.Task, commit.TaskForbidden)
		if err != nil {
			return false, err
		}
		if !written {
			// Rolled back by the deferred Rollback: nothing landed. Reported as a lost
			// fence rather than an error, because the claim is intact and the next pass
			// re-classifies against the winning state.
			return false, nil
		}
		if commit.TaskEventValid {
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_events(task_id, kind, from_state, to_state, reason)
				VALUES (?, ?, ?, ?, ?)`, commit.Task.ID, commit.TaskEvent.Kind, commit.TaskEvent.FromState,
				commit.TaskEvent.ToState, commit.TaskEvent.Reason); err != nil {
				return false, err
			}
		}
	}

	for _, prepared := range commit.Jobs {
		if err := createJobWithEventTx(ctx, tx, s, prepared.Job, JobEvent{
			JobID: prepared.Job.ID, Kind: prepared.Job.State, Message: "job queued",
		}); err != nil {
			return false, err
		}
		for _, event := range prepared.Events {
			if strings.TrimSpace(event.JobID) == "" {
				event.JobID = prepared.Job.ID
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
				event.JobID, event.Kind, event.Message); err != nil {
				return false, err
			}
		}
	}

	for _, event := range commit.Events {
		if strings.TrimSpace(event.JobID) == "" {
			event.JobID = jobID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
			event.JobID, event.Kind, event.Message); err != nil {
			return false, err
		}
	}

	if commit.ReceiptValid {
		if _, err := tx.ExecContext(ctx, `UPDATE escalation_rounds
			SET effects_completed_at = ?, recovery_owner = '', recovery_lease_until = NULL
			WHERE job_id = ? AND round_id = ? AND effects_completed_at IS NULL`,
			formatResourceLockTime(now), jobID, roundID); err != nil {
			return false, err
		}
		event := commit.Receipt
		if strings.TrimSpace(event.JobID) == "" {
			event.JobID = jobID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(job_id, kind, message) VALUES (?, ?, ?)`,
			event.JobID, event.Kind, event.Message); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// MarkEscalationRoundNeedsRepairAsOwner parks a round only for its FENCE HOLDER. That
// restriction is what pairs with supersede's needs_repair precondition to make an
// operator discard and an in-flight replay mutually exclusive rather than racing
// (#1673). It clears the fence as it parks, so the parked round is by construction
// unfenced and therefore supersedable.
func (s *Store) MarkEscalationRoundNeedsRepairAsOwner(ctx context.Context, jobID string, roundID string, owner string, cause string, event JobEvent, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE escalation_rounds
		SET integrity_state = ?, integrity_cause = ?, integrity_at = ?,
		    recovery_owner = '', recovery_lease_until = NULL
		WHERE job_id = ? AND round_id = ? AND integrity_state = '' AND effects_completed_at IS NULL
		  AND recovery_owner = ? AND recovery_lease_until > ?`,
		EscalationRoundIntegrityNeedsRepair, strings.TrimSpace(cause), formatResourceLockTime(now),
		strings.TrimSpace(jobID), strings.TrimSpace(roundID), strings.TrimSpace(owner),
		formatResourceLockTime(now))
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

// RenewEscalationRecoveryLease extends a fence its CURRENT holder still owns. It is a
// separate primitive from AcquireEscalationRecoveryLease on purpose: acquire requires
// the lease to be free or lapsed, so a holder calling it gets false and would wrongly
// conclude it had lost ownership (#1673).
//
// It returns false ONLY on genuine ownership loss - superseded, parked, settled, or
// lapsed and taken by someone else - which is exactly the signal a long external
// pre-effect needs.
func (s *Store) RenewEscalationRecoveryLease(ctx context.Context, jobID string, roundID string, owner string, until time.Time, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return false, errors.New("recovery lease owner is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE escalation_rounds
		SET recovery_lease_until = ?
		WHERE job_id = ? AND round_id = ?
		  AND effects_completed_at IS NULL
		  AND integrity_state = ''
		  AND recovery_owner = ?
		  AND recovery_lease_until > ?`,
		formatResourceLockTime(until), strings.TrimSpace(jobID), strings.TrimSpace(roundID),
		owner, formatResourceLockTime(now))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}
