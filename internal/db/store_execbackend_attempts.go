package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	ExecBackendAttemptStateReserved     = "reserved"
	ExecBackendAttemptStateProvisioning = "provisioning"
	ExecBackendAttemptStateRunning      = "running"
	ExecBackendAttemptStateCollecting   = "collecting"
	ExecBackendAttemptStateDestroying   = "destroying"
	ExecBackendAttemptStateDestroyed    = "destroyed"
	ExecBackendAttemptStateOrphaned     = "orphaned"
	ExecBackendAttemptStateFailed       = "failed"
)

// ExecBackendAttemptKey identifies one execution-backend allocation attempt.
// lifecycle_generation separates repeated runs of the same durable job, while
// attempt separates Mailbox re-deliveries within that lifecycle.
type ExecBackendAttemptKey struct {
	JobID               string
	Attempt             int
	LifecycleGeneration int64
}

// ExecBackendAttemptReservation is the information known before provider
// provisioning begins. CostReservedUSD is intentionally part of this shape:
// ReserveExecBackendAttempt persists it atomically with the reserved row before
// any provider call, giving parallel children one primary-keyed reservation to
// contend on rather than an advisory after-the-fact cost record.
type ExecBackendAttemptReservation struct {
	ExecBackendAttemptKey
	Provider           string
	DaemonFencingToken string
	BootID             string
	TTLExpiresAt       time.Time
	CostReservedUSD    float64
}

// ExecBackendAttempt is one durable execution-backend lifecycle row. SandboxID
// remains a pointer because NULL is evidence: it identifies the crash window in
// which the ledger reservation exists but no provider handle was persisted.
type ExecBackendAttempt struct {
	ExecBackendAttemptKey
	Provider           string
	SandboxID          *string
	DaemonFencingToken string
	BootID             string
	TTLExpiresAt       string
	State              string
	CostReservedUSD    *float64
	CostActualUSD      *float64
	CreatedAt          string
	UpdatedAt          string
}

const execBackendAttemptColumns = `job_id, attempt, lifecycle_generation, provider, sandbox_id,
	daemon_fencing_token, boot_id, ttl_expires_at, state, cost_reserved_usd, cost_actual_usd,
	created_at, updated_at`

// ReserveExecBackendAttempt writes the cost reservation and reserved lifecycle
// row in one INSERT before provisioning. sandbox_id and cost_actual_usd are
// explicitly NULL at this point. Both daemon_fencing_token and boot_id are
// recorded because a daemon restart within one host boot reuses boot_id and must
// still fence the previous daemon incarnation.
//
// THE COMPUTE-DOLLAR CAP IS ENFORCED IN THIS STATEMENT (#1540). The predicate
// rides the INSERT rather than preceding it, because a read-then-insert lets N
// parallel legs each sum the same total, each see room and each insert - the
// exact contention this reservation exists to serialise. Zero rows affected IS
// the refusal. This is also why the gate lives here rather than at a Provision
// call site: a caller cannot spend without a reserved row and cannot obtain one
// without passing this predicate, so the population it guards is every future
// provisioning path rather than the ones that exist today.
func (s *Store) ReserveExecBackendAttempt(ctx context.Context, reservation ExecBackendAttemptReservation, policy ExecBackendCostCap) error {
	reservation.JobID = strings.TrimSpace(reservation.JobID)
	reservation.Provider = strings.TrimSpace(reservation.Provider)
	reservation.DaemonFencingToken = strings.TrimSpace(reservation.DaemonFencingToken)
	reservation.BootID = strings.TrimSpace(reservation.BootID)
	if reservation.JobID == "" {
		return errors.New("execution backend attempt job id is required")
	}
	if reservation.Attempt < 1 {
		return errors.New("execution backend attempt number must be positive")
	}
	if reservation.LifecycleGeneration < 0 {
		return errors.New("execution backend lifecycle generation must be non-negative")
	}
	if reservation.Provider == "" {
		return errors.New("execution backend provider is required")
	}
	if reservation.DaemonFencingToken == "" {
		return errors.New("execution backend daemon fencing token is required")
	}
	if reservation.BootID == "" {
		return errors.New("execution backend boot id is required")
	}
	if reservation.TTLExpiresAt.IsZero() {
		return errors.New("execution backend TTL expiry is required")
	}
	if reservation.CostReservedUSD < 0 {
		return errors.New("execution backend reserved cost must be non-negative")
	}

	// No configured cap denies, and does so before any statement runs.
	// THE UNSET ARM IS THE SHIPPING STATE, not an edge case. Each of the four
	// ways a cap can be missing - absent, malformed, partially set, and
	// explicitly zero - denies here, and zero denies EXPLICITLY rather than by
	// falling through, because zero is exactly the value this codebase's other
	// budgets read as "unlimited".
	if !policy.Configured {
		return &ExecBackendCapRefusal{Clause: "unconfigured", RequestUSD: reservation.CostReservedUSD, DenyReason: policy.DenyReason}
	}
	if policy.MaxReservedUSD == 0 || policy.PerAttemptUSD == 0 {
		return &ExecBackendCapRefusal{Clause: "unconfigured", RequestUSD: reservation.CostReservedUSD,
			DenyReason: "an execution backend cost cap of exactly 0 denies. Unlike the model-token budget, 0 here does not mean unlimited. Set [remote_exec].cost_max_reserved_usd and [remote_exec].cost_per_attempt_usd to positive values in config.toml."}
	}
	if policy.MaxReservedUSD < 0 || policy.PerAttemptUSD < 0 {
		return &ExecBackendCapRefusal{Clause: "unconfigured", RequestUSD: reservation.CostReservedUSD,
			DenyReason: "a negative execution backend cost cap denies. Set [remote_exec].cost_max_reserved_usd and [remote_exec].cost_per_attempt_usd to positive values in config.toml."}
	}

	marks, stateArgs := billingStatePlaceholders()
	// One statement: both cap clauses and the insert. The concurrency clause is
	// skipped only when MaxConcurrent <= 0; the dollar clause never is.
	concurrency := "1=1"
	args := []any{
		reservation.JobID, reservation.Attempt, reservation.LifecycleGeneration,
		reservation.Provider, reservation.DaemonFencingToken, reservation.BootID,
		reservation.TTLExpiresAt.UTC().Format(time.RFC3339Nano),
		ExecBackendAttemptStateReserved, reservation.CostReservedUSD,
	}
	args = append(args, stateArgs...)
	args = append(args, reservation.CostReservedUSD, policy.MaxReservedUSD)
	if policy.MaxConcurrent > 0 {
		concurrency = `(SELECT COUNT(*) FROM execbackend_attempts WHERE state IN (` + marks + `)) + 1 <= ?`
		args = append(args, stateArgs...)
		args = append(args, policy.MaxConcurrent)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO execbackend_attempts(
		job_id, attempt, lifecycle_generation, provider, sandbox_id,
		daemon_fencing_token, boot_id, ttl_expires_at, state,
		cost_reserved_usd, cost_actual_usd
	) SELECT ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, NULL
	WHERE (SELECT COALESCE(SUM(cost_reserved_usd), 0) FROM execbackend_attempts
		WHERE state IN (`+marks+`)) + ? <= ?
	  AND `+concurrency, args...)
	if err != nil {
		// An unreadable meter refuses. The statement that reads the meter is the
		// statement that writes the row, so a failure here cannot have admitted.
		return fmt.Errorf("execution backend cost cap could not be evaluated, refusing provision: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("execution backend cost cap could not confirm admission, refusing provision: %w", err)
	}
	if affected == 1 {
		return nil
	}
	refusal := &ExecBackendCapRefusal{
		Clause:         "dollar",
		RequestUSD:     reservation.CostReservedUSD,
		MaxReservedUSD: policy.MaxReservedUSD,
		MaxConcurrent:  policy.MaxConcurrent,
	}
	s.describeBillingLoad(ctx, refusal)
	for _, n := range refusal.ByState {
		refusal.LiveCount += n
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_reserved_usd), 0)
		FROM execbackend_attempts WHERE state IN (`+marks+`)`, stateArgs...).Scan(&refusal.ReservedUSD); err != nil && refusal.OperandsErr == nil {
		refusal.OperandsErr = err
	}
	if policy.MaxConcurrent > 0 && refusal.LiveCount+1 > policy.MaxConcurrent {
		refusal.Clause = "concurrency"
	}
	return refusal
}

// MarkExecBackendAttemptProvisioning claims a reserved row for provider setup.
func (s *Store) MarkExecBackendAttemptProvisioning(ctx context.Context, key ExecBackendAttemptKey) (bool, error) {
	return s.transitionExecBackendAttemptState(ctx, key, ExecBackendAttemptStateReserved, ExecBackendAttemptStateProvisioning)
}

// MarkExecBackendAttemptRunning records the provider handle only after creation
// succeeds. Until this guarded transition commits, sandbox_id stays NULL and is
// discoverable by ListExecBackendAttemptsWithoutSandboxID for reconciliation.
func (s *Store) MarkExecBackendAttemptRunning(ctx context.Context, key ExecBackendAttemptKey, sandboxID string) (bool, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return false, errors.New("execution backend sandbox id is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE execbackend_attempts
		SET state = ?, sandbox_id = ?, updated_at = CURRENT_TIMESTAMP
		WHERE job_id = ? AND attempt = ? AND lifecycle_generation = ? AND state = ?`,
		ExecBackendAttemptStateRunning, sandboxID, key.JobID, key.Attempt, key.LifecycleGeneration,
		ExecBackendAttemptStateProvisioning)
	if err != nil {
		return false, err
	}
	return exactlyOneExecBackendAttemptChanged(result)
}

// MarkExecBackendAttemptCollecting begins host-side result collection.
func (s *Store) MarkExecBackendAttemptCollecting(ctx context.Context, key ExecBackendAttemptKey) (bool, error) {
	return s.transitionExecBackendAttemptState(ctx, key, ExecBackendAttemptStateRunning, ExecBackendAttemptStateCollecting)
}

// MarkExecBackendAttemptDestroying records that provider teardown has begun.
func (s *Store) MarkExecBackendAttemptDestroying(ctx context.Context, key ExecBackendAttemptKey) (bool, error) {
	return s.transitionExecBackendAttemptState(ctx, key, ExecBackendAttemptStateCollecting, ExecBackendAttemptStateDestroying)
}

// MarkExecBackendAttemptDestroyed completes teardown and records compute cost
// separately from model cost. Cost enforcement remains outside the store.
func (s *Store) MarkExecBackendAttemptDestroyed(ctx context.Context, key ExecBackendAttemptKey, costActualUSD float64) (bool, error) {
	if costActualUSD < 0 {
		return false, errors.New("execution backend actual cost must be non-negative")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE execbackend_attempts
		SET state = ?, cost_actual_usd = ?, updated_at = CURRENT_TIMESTAMP
		WHERE job_id = ? AND attempt = ? AND lifecycle_generation = ? AND state = ?`,
		ExecBackendAttemptStateDestroyed, costActualUSD, key.JobID, key.Attempt, key.LifecycleGeneration,
		ExecBackendAttemptStateDestroying)
	if err != nil {
		return false, err
	}
	return exactlyOneExecBackendAttemptChanged(result)
}

// MarkExecBackendAttemptReconciledDestroyed records a destroy THE ENGINE DID NOT
// PERFORM: the provider's inventory reports the sandbox gone, from any live
// state, without the collecting -> destroying walk teardown makes.
//
// IT IS A SEPARATE FUNCTION RATHER THAN A WIDENED GUARD (#2147). Teardown's
// MarkExecBackendAttemptDestroyed admits exactly `destroying`, and that single
// source is load-bearing: teardown genuinely walks collecting -> destroying ->
// destroyed and must keep being refused from anywhere else. Relaxing its WHERE
// clause to serve the reconciler would silently widen teardown's guard too.
//
// AND IT IS ONE WRITE, NOT THREE. Walking the row through collecting and
// destroying would record two phases that never happened for a sandbox that was
// already gone, and would not be atomic.
//
// The defect this exists for: reconcileInventory could only reach `orphaned`
// from a live state, because that was the only terminal transition admitting
// one. So a provider-CONFIRMED destroy was recorded as an orphan - and orphaned
// is both a billing state for the compute cap and terminal-unreachable, so the
// reservation was held forever with no repair path.
func (s *Store) MarkExecBackendAttemptReconciledDestroyed(ctx context.Context, key ExecBackendAttemptKey, costActualUSD float64) (bool, error) {
	if costActualUSD < 0 {
		return false, errors.New("execution backend actual cost must be non-negative")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE execbackend_attempts
		SET state = ?, cost_actual_usd = ?, updated_at = CURRENT_TIMESTAMP
		WHERE job_id = ? AND attempt = ? AND lifecycle_generation = ?
			AND state IN (?, ?, ?, ?, ?)`,
		ExecBackendAttemptStateDestroyed, costActualUSD, key.JobID, key.Attempt, key.LifecycleGeneration,
		ExecBackendAttemptStateReserved, ExecBackendAttemptStateProvisioning,
		ExecBackendAttemptStateRunning, ExecBackendAttemptStateCollecting,
		ExecBackendAttemptStateDestroying)
	if err != nil {
		return false, err
	}
	return exactlyOneExecBackendAttemptChanged(result)
}

// MarkExecBackendAttemptOrphaned persists the reaper's conclusion explicitly.
// An orphan is terminal evidence, not a state callers should have to infer from
// a missing provider handle or an expired TTL.
func (s *Store) MarkExecBackendAttemptOrphaned(ctx context.Context, key ExecBackendAttemptKey) (bool, error) {
	return s.transitionExecBackendAttemptToTerminal(ctx, key, ExecBackendAttemptStateOrphaned)
}

// MarkExecBackendAttemptFailed terminates any active lifecycle arm as failed.
func (s *Store) MarkExecBackendAttemptFailed(ctx context.Context, key ExecBackendAttemptKey) (bool, error) {
	return s.transitionExecBackendAttemptToTerminal(ctx, key, ExecBackendAttemptStateFailed)
}

func (s *Store) transitionExecBackendAttemptState(ctx context.Context, key ExecBackendAttemptKey, from, to string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE execbackend_attempts
		SET state = ?, updated_at = CURRENT_TIMESTAMP
		WHERE job_id = ? AND attempt = ? AND lifecycle_generation = ? AND state = ?`,
		to, key.JobID, key.Attempt, key.LifecycleGeneration, from)
	if err != nil {
		return false, err
	}
	return exactlyOneExecBackendAttemptChanged(result)
}

func (s *Store) transitionExecBackendAttemptToTerminal(ctx context.Context, key ExecBackendAttemptKey, to string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE execbackend_attempts
		SET state = ?, updated_at = CURRENT_TIMESTAMP
		WHERE job_id = ? AND attempt = ? AND lifecycle_generation = ?
			AND state IN (?, ?, ?, ?, ?)`,
		to, key.JobID, key.Attempt, key.LifecycleGeneration,
		ExecBackendAttemptStateReserved, ExecBackendAttemptStateProvisioning,
		ExecBackendAttemptStateRunning, ExecBackendAttemptStateCollecting,
		ExecBackendAttemptStateDestroying)
	if err != nil {
		return false, err
	}
	return exactlyOneExecBackendAttemptChanged(result)
}

func exactlyOneExecBackendAttemptChanged(result sql.Result) (bool, error) {
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// GetExecBackendAttempt returns one exact lifecycle attempt.
func (s *Store) GetExecBackendAttempt(ctx context.Context, key ExecBackendAttemptKey) (ExecBackendAttempt, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+execBackendAttemptColumns+`
		FROM execbackend_attempts
		WHERE job_id = ? AND attempt = ? AND lifecycle_generation = ?`,
		key.JobID, key.Attempt, key.LifecycleGeneration)
	return scanExecBackendAttempt(row)
}

// ListExecBackendAttemptsWithoutSandboxID exposes the deliberate NULL-handle
// crash window to the bidirectional reaper. Rows are returned across all states:
// the stored state is evidence the reaper needs when deciding what to do.
func (s *Store) ListExecBackendAttemptsWithoutSandboxID(ctx context.Context) ([]ExecBackendAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+execBackendAttemptColumns+`
		FROM execbackend_attempts WHERE sandbox_id IS NULL
		ORDER BY job_id, lifecycle_generation, attempt`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attempts []ExecBackendAttempt
	for rows.Next() {
		attempt, err := scanExecBackendAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

// ListRecoverableExecBackendAttempts returns every active row for one provider.
// Terminal rows make no claim that a sandbox remains live; if a provider later
// reports one with only a terminal-row identity, recovery surfaces it as a
// provider-only allocation rather than silently reviving terminal evidence.
func (s *Store) ListRecoverableExecBackendAttempts(ctx context.Context, provider string) ([]ExecBackendAttempt, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, errors.New("execution backend provider is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+execBackendAttemptColumns+`
		FROM execbackend_attempts
		WHERE provider = ? AND state NOT IN (?, ?, ?)
		ORDER BY job_id, lifecycle_generation, attempt`,
		provider, ExecBackendAttemptStateDestroyed, ExecBackendAttemptStateOrphaned,
		ExecBackendAttemptStateFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attempts []ExecBackendAttempt
	for rows.Next() {
		attempt, err := scanExecBackendAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func scanExecBackendAttempt(row interface{ Scan(...any) error }) (ExecBackendAttempt, error) {
	var (
		attempt      ExecBackendAttempt
		sandboxID    sql.NullString
		costReserved sql.NullFloat64
		costActual   sql.NullFloat64
	)
	if err := row.Scan(&attempt.JobID, &attempt.Attempt, &attempt.LifecycleGeneration,
		&attempt.Provider, &sandboxID, &attempt.DaemonFencingToken, &attempt.BootID,
		&attempt.TTLExpiresAt, &attempt.State, &costReserved, &costActual,
		&attempt.CreatedAt, &attempt.UpdatedAt); err != nil {
		return ExecBackendAttempt{}, err
	}
	if sandboxID.Valid {
		attempt.SandboxID = &sandboxID.String
	}
	if costReserved.Valid {
		attempt.CostReservedUSD = &costReserved.Float64
	}
	if costActual.Valid {
		attempt.CostActualUSD = &costActual.Float64
	}
	return attempt, nil
}
