package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReleaseRuntimeSessionLocksFromForeignBoot deletes every runtime-session lock
// (resource_key LIKE 'runtime:%') whose owner_boot_id proves it was acquired on a
// PREVIOUS boot of this host (#651). After a reboot such a lock's owning process
// is dead, but its lease survives in the DB and would otherwise leave a stale
// runtime-session lock attached to the failed owner — so it is reclaimed
// regardless of lease. The daemon first records the evidence-bearing terminal
// transition selected by ListRunningJobIDsFromForeignBoot; this method only
// reclaims the lock row. A session-recorded owner is excluded only while its job
// is running outside the daemon; once the session row is terminal (or gone), its
// stale lock is reclaimed too. It returns the number of locks released.
//
// It is a STRICT no-op when currentBootID is "" and, via the `!= ”` guard, never
// reclaims an identity-less lock (a non-pid-stamping acquire or a legacy row),
// which stays governed by its lease/TTL. Because a foreign boot id can only have
// been written by a process on a prior boot, it can never match an in-flight owner
// in THIS process, so no live session is ever reclaimed out from under it.
func (s *Store) ReleaseRuntimeSessionLocksFromForeignBoot(ctx context.Context, currentBootID string) (int64, error) {
	currentBootID = strings.TrimSpace(currentBootID)
	if currentBootID == "" {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE resource_key LIKE ? AND owner_boot_id != '' AND owner_boot_id != ?
		AND NOT EXISTS (
			SELECT 1 FROM jobs
			WHERE jobs.id = resource_locks.owner_job_id
				AND jobs.state = 'running'
				AND (jobs.externally_driven = 1 OR jobs.id LIKE 'session-%')
		)`,
		RuntimeSessionLockKeyPrefix+"%", currentBootID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ResourceLockOwnerBootID returns the recorded owner_boot_id for a held lock, or
// "" when the lock is absent or its boot id was never stamped (#651). It is a
// targeted single-column read kept deliberately OUT of the shared 9-column lock
// SELECTs so their scan arity is unchanged; the generation-lock recovery
// path uses it to prove a same-host owner from a different boot is dead without a
// kill(2) syscall (and PID-reuse-immune).
func (s *Store) ResourceLockOwnerBootID(ctx context.Context, resourceKey string) (string, error) {
	var bootID string
	err := s.db.QueryRowContext(ctx, `SELECT owner_boot_id FROM resource_locks WHERE resource_key = ?`, strings.TrimSpace(resourceKey)).Scan(&bootID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(bootID), nil
}

func (s *Store) AcquireResourceLock(ctx context.Context, lock ResourceLock, now time.Time) (bool, error) {
	resourceKey := strings.TrimSpace(lock.ResourceKey)
	ownerJobID := strings.TrimSpace(lock.OwnerJobID)
	ownerToken := strings.TrimSpace(lock.OwnerToken)
	if resourceKey == "" {
		return false, errors.New("resource lock key is required")
	}
	if ownerJobID == "" {
		return false, errors.New("resource lock owner job id is required")
	}
	if ownerToken == "" {
		return false, errors.New("resource lock owner token is required")
	}
	expiresAt := strings.TrimSpace(lock.ExpiresAt)
	if expiresAt == "" {
		return false, errors.New("resource lock expiry is required")
	}
	parsedExpiresAt, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return false, fmt.Errorf("resource lock expiry must be RFC3339: %w", err)
	}
	expiresAt = formatResourceLockTime(parsedExpiresAt)
	nowText := formatResourceLockTime(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE resource_key = ?
			AND expires_at <= ?
			AND NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.id = resource_locks.owner_job_id
					AND jobs.state = 'running'
			)`, resourceKey, nowText); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO resource_locks(resource_key, owner_job_id, owner_token, owner_pid, owner_hostname, owner_boot_id, command_hash, acquired_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, resourceKey, ownerJobID, ownerToken, lock.OwnerPID, strings.TrimSpace(lock.OwnerHostname), strings.TrimSpace(lock.OwnerBootID), strings.TrimSpace(lock.CommandHash), nowText, nowText, expiresAt)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 1 {
		return true, tx.Commit()
	}
	return false, tx.Commit()
}

func (s *Store) GetResourceLock(ctx context.Context, resourceKey string) (ResourceLock, error) {
	row := s.db.QueryRowContext(ctx, `SELECT resource_key, owner_job_id, owner_token, owner_pid, owner_hostname, command_hash, acquired_at, updated_at, expires_at FROM resource_locks WHERE resource_key = ?`, resourceKey)
	var lock ResourceLock
	if err := row.Scan(&lock.ResourceKey, &lock.OwnerJobID, &lock.OwnerToken, &lock.OwnerPID, &lock.OwnerHostname, &lock.CommandHash, &lock.AcquiredAt, &lock.UpdatedAt, &lock.ExpiresAt); err != nil {
		return ResourceLock{}, err
	}
	return lock, nil
}

// ListResourceLocks returns all held resource locks, ordered by resource key.
func (s *Store) ListResourceLocks(ctx context.Context) ([]ResourceLock, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT resource_key, owner_job_id, owner_token, owner_pid, owner_hostname, command_hash, acquired_at, updated_at, expires_at FROM resource_locks ORDER BY resource_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var locks []ResourceLock
	for rows.Next() {
		var lock ResourceLock
		if err := rows.Scan(&lock.ResourceKey, &lock.OwnerJobID, &lock.OwnerToken, &lock.OwnerPID, &lock.OwnerHostname, &lock.CommandHash, &lock.AcquiredAt, &lock.UpdatedAt, &lock.ExpiresAt); err != nil {
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, rows.Err()
}

// SupersedeAdvanceLockKeyPrefix namespaces the resource lock a supersession
// recovery holds while it drives a child's parent advance (#1673).
//
// The ownership primitive is deliberately the EXISTING resource_locks table
// rather than a bespoke marker: it already carries an owner token, a renewable
// lease (HeartbeatResourceLock), an explicit release, and PID/boot identity, so a
// crashed owner is recovered by the same machinery that recovers a dead runtime
// session instead of by a fixed timeout nobody can renew.
const SupersedeAdvanceLockKeyPrefix = "supersede-advance:"

// noLiveSupersedeAdvanceLockSQL excludes a job whose parent advance is OWNED right
// now. It is a predicate, not a pre-check, so it lands in the same statement as the
// transition it guards; a pre-check loses to a recovery pass that acquires
// ownership between the caller's read and its write.
//
// Ownership is honoured only while the lease is unexpired, which is what makes an
// abandoned owner recoverable — and the owner renews that lease for as long as its
// advance is genuinely running, so a slow-but-live advance never lapses.
const noLiveSupersedeAdvanceLockSQL = ` AND NOT EXISTS (
			SELECT 1 FROM resource_locks advance
			WHERE advance.resource_key = '` + SupersedeAdvanceLockKeyPrefix + `' || jobs.id
			  AND advance.expires_at > ?
		)`

// AdvanceOwnershipHeld reports whether resourceKey is held by ownerToken with an
// unexpired lease. Effect commits use it INSIDE their own transaction, so an
// irreversible parent effect can only land while its advance still owns the job.
func advanceOwnershipHeldTx(ctx context.Context, tx *sql.Tx, resourceKey string, ownerToken string, now time.Time) (bool, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	ownerToken = strings.TrimSpace(ownerToken)
	if resourceKey == "" || ownerToken == "" {
		return false, nil
	}
	var held int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM resource_locks
		WHERE resource_key = ? AND owner_token = ? AND expires_at > ?`,
		resourceKey, ownerToken, formatResourceLockTime(now)).Scan(&held)
	if err != nil {
		return false, err
	}
	return held > 0, nil
}

// AdvanceOwnershipHeld is the non-transactional read used by renewal checks and by
// tests. The authoritative check for a WRITE is advanceOwnershipHeldTx inside that
// write's own transaction.
func (s *Store) AdvanceOwnershipHeld(ctx context.Context, resourceKey string, ownerToken string, now time.Time) (bool, error) {
	var held int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM resource_locks
		WHERE resource_key = ? AND owner_token = ? AND expires_at > ?`,
		strings.TrimSpace(resourceKey), strings.TrimSpace(ownerToken), formatResourceLockTime(now)).Scan(&held)
	if err != nil {
		return false, err
	}
	return held > 0, nil
}

// advanceOwnershipHeldAnyTokenTx answers "is this job's advance owned by ANYONE
// right now", which is how a losing retry distinguishes "ownership refused me"
// from "my state CAS simply did not match". It runs in the caller's transaction so
// the answer belongs to the same snapshot as the failed write.
func advanceOwnershipHeldAnyTokenTx(ctx context.Context, tx *sql.Tx, resourceKey string, now time.Time) (bool, error) {
	var held int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM resource_locks
		WHERE resource_key = ? AND expires_at > ?`,
		strings.TrimSpace(resourceKey), formatResourceLockTime(now)).Scan(&held)
	if err != nil {
		return false, err
	}
	return held > 0, nil
}

func (s *Store) ListExpiredRuntimeSessionLocks(ctx context.Context, now time.Time) ([]ResourceLock, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT resource_key, owner_job_id, owner_token, owner_pid, owner_hostname, command_hash, acquired_at, updated_at, expires_at
		FROM resource_locks
		WHERE resource_key LIKE ? AND expires_at <= ?
		ORDER BY resource_key`, RuntimeSessionLockKeyPrefix+"%", formatResourceLockTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var locks []ResourceLock
	for rows.Next() {
		var lock ResourceLock
		if err := rows.Scan(&lock.ResourceKey, &lock.OwnerJobID, &lock.OwnerToken, &lock.OwnerPID, &lock.OwnerHostname, &lock.CommandHash, &lock.AcquiredAt, &lock.UpdatedAt, &lock.ExpiresAt); err != nil {
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, rows.Err()
}

func (s *Store) HeartbeatResourceLock(ctx context.Context, resourceKey string, ownerToken string, expiresAt time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE resource_locks
		SET expires_at = ?, updated_at = ?
		WHERE resource_key = ? AND owner_token = ?`,
		formatResourceLockTime(expiresAt), formatResourceLockTime(time.Now().UTC()), strings.TrimSpace(resourceKey), strings.TrimSpace(ownerToken))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) ReleaseResourceLock(ctx context.Context, resourceKey string, ownerJobID string, ownerToken string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks WHERE resource_key = ? AND owner_job_id = ? AND owner_token = ?`, resourceKey, ownerJobID, ownerToken)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// AdvanceOwnership names the exact lease an advance holds: key, token, the job it
// advances, and the lifecycle generation it was granted for. Every renewal and
// every irreversible effect is bound to ALL FOUR, in one predicate.
type AdvanceOwnership struct {
	LockKey      string
	OwnerToken   string
	OwnerJobID   string
	AtGeneration int64
}

// advanceOwnershipLiveSQL is the predicate: this exact token still holds this exact
// key, its lease has NOT expired, and the job is still on the generation the lease
// was granted for.
//
// Expiry is part of it because a lapsed lease is how an abandoned pass is
// recovered: once RetryJob commits generation N+1 against an expired lock, the old
// token must be permanently dead. A renewal that only matched key+token would
// RESURRECT it and let a dead pass emit effects against a lifecycle that has moved.
// The generation clause closes the same door from the other side.
const advanceOwnershipLiveSQL = `SELECT 1 FROM resource_locks
		WHERE resource_key = ? AND owner_token = ? AND expires_at > ?
		  AND EXISTS (SELECT 1 FROM jobs WHERE jobs.id = ? AND jobs.lifecycle_generation = ?)`

// RenewAdvanceOwnershipLease extends a lease that is STILL live and still on its
// granted lifecycle. Zero rows means ownership is gone for good, never "try again".
//
// It is deliberately separate from HeartbeatResourceLock: a runtime-session
// heartbeat has different recovery semantics, and widening that method would change
// behaviour for every session on the box.
func (s *Store) RenewAdvanceOwnershipLease(ctx context.Context, own AdvanceOwnership, expiresAt time.Time, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE resource_locks
		SET expires_at = ?, updated_at = ?
		WHERE resource_key = ? AND owner_token = ? AND expires_at > ?
		  AND EXISTS (SELECT 1 FROM jobs WHERE jobs.id = ? AND jobs.lifecycle_generation = ?)`,
		formatResourceLockTime(expiresAt), formatResourceLockTime(now),
		strings.TrimSpace(own.LockKey), strings.TrimSpace(own.OwnerToken), formatResourceLockTime(now),
		strings.TrimSpace(own.OwnerJobID), own.AtGeneration)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// advanceOwnershipLiveTx answers the same predicate INSIDE a caller's transaction,
// so an irreversible write can be bound to live ownership at COMMIT rather than by
// a preceding read. A pre-write heartbeat is not sufficient: the gap between it and
// the write is exactly where a lease loss lands.
func advanceOwnershipLiveTx(ctx context.Context, tx *sql.Tx, own AdvanceOwnership, now time.Time) (bool, error) {
	if strings.TrimSpace(own.LockKey) == "" || strings.TrimSpace(own.OwnerToken) == "" {
		return false, nil
	}
	var live int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+advanceOwnershipLiveSQL+`)`,
		strings.TrimSpace(own.LockKey), strings.TrimSpace(own.OwnerToken), formatResourceLockTime(now),
		strings.TrimSpace(own.OwnerJobID), own.AtGeneration).Scan(&live)
	if err != nil {
		return false, err
	}
	return live > 0, nil
}

// excludeLiveAdvanceLockSQL keeps a supersession parent-advance lease OUT of every
// broad, owner-scoped resource-lock delete (#1673).
//
// The lease is not a resource the OWNER JOB holds; it is a coordination lock held
// by whichever recovery PASS is advancing that job right now, and its owner_job_id
// is only how it is addressed. A cleanup keyed on owner_job_id therefore deletes a
// lock belonging to a different, live pass — after which a retry's exclusion
// predicate sees nothing and rolls the lifecycle over mid-advance. Ownership of
// this class is managed only by its owner's explicit release and by lease expiry.
//
// Liveness is part of the exclusion so an ABANDONED lease is still swept by the
// same cleanup: only an unexpired one is protected.
const excludeLiveAdvanceLockSQL = ` AND NOT (resource_key LIKE '` + SupersedeAdvanceLockKeyPrefix + `%' AND expires_at > ?)`

// DeleteResourceLocksByOwner releases every resource lock held by ownerJobID,
// regardless of token/expiry — used when a job is cancelled and can no longer
// renew its locks. A LIVE supersede-advance lease is excluded: it belongs to a
// recovery pass, not to this job. Returns the number released.
func (s *Store) DeleteResourceLocksByOwner(ctx context.Context, ownerJobID string, now time.Time) (int64, error) {
	ownerJobID = strings.TrimSpace(ownerJobID)
	if ownerJobID == "" {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks WHERE owner_job_id = ?`+excludeLiveAdvanceLockSQL,
		ownerJobID, formatResourceLockTime(now))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DeleteResourceLocksByOwnerIfNotRunning releases an owner's resource locks
// only while that owner job is not currently running, evaluated atomically in
// the DELETE itself. This mirrors DeleteExpiredResourceLocks's
// `NOT EXISTS (... jobs.state='running')` guard and closes the TOCTOU race in
// the delegation-kill cleanup path (#479): a child that raced queued->running
// after a stale snapshot was read keeps its live runtime-session / checkout
// lock instead of having it deleted out from under its in-flight process.
//
// A live supersede-advance lease is excluded for the same reason as above: the
// owner job is terminal by construction here, so this guard alone would not save
// another pass's lease (#1673).
func (s *Store) DeleteResourceLocksByOwnerIfNotRunning(ctx context.Context, ownerJobID string, now time.Time) (int64, error) {
	ownerJobID = strings.TrimSpace(ownerJobID)
	if ownerJobID == "" {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE owner_job_id = ?
			AND NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.id = resource_locks.owner_job_id
					AND jobs.state = 'running'
			)`+excludeLiveAdvanceLockSQL, ownerJobID, formatResourceLockTime(now))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ReleaseSupersededJobResourceLocksAtGeneration releases an owner's resource
// locks only while that owner job is STILL the settled lifecycle the caller
// claimed: same lifecycle_generation, and still in one of the two terminal states
// a supersession writes.
//
// The guard and the delete are ONE WRITE STATEMENT, and it is the FIRST statement
// of the transaction. That ordering is the fix, not decoration: with a deferred
// BeginTx a SELECT-then-DELETE takes a READ snapshot first, and under WAL another
// PROCESS committing `gitmoot job retry` in between makes the upgrade fail
// SQLITE_BUSY_SNAPSHOT — safe in isolation, but the caller then has to distinguish
// "guard lost" from "write refused", and swallowing it closed the debt. Writing
// first takes the write lock immediately, so a concurrent re-queue either lands
// BEFORE this statement (the EXISTS predicate fails and nothing is deleted) or
// serialises AFTER the commit (the locks it goes on to acquire were never in
// scope). There is no third interleaving.
//
// guarded is read AFTER the write, inside the same transaction, so it reports the
// state that decided the delete rather than a pre-read another process could have
// invalidated. It is distinct from released, which is 0 both for a lost guard and
// for a job that simply held no locks.
//
// Every error is returned. A caller that cannot prove the cleanup ran must leave
// its debt outstanding rather than record it paid.
// A LIVE supersede-advance lease is excluded (#1673). This cleanup is keyed on
// owner_job_id, and a competing recovery pass's advance lease carries the same
// owner_job_id, so without the exclusion a second finalizer deletes the FIRST
// pass's unexpired lock — and a retry then rolls the lifecycle over mid-advance,
// which is the precise failure the lease exists to prevent. An expired lease is
// still swept here, so a crashed pass is recovered as before.
func (s *Store) ReleaseSupersededJobResourceLocksAtGeneration(ctx context.Context, ownerJobID string, atGeneration int64, now time.Time) (released int64, guarded bool, err error) {
	ownerJobID = strings.TrimSpace(ownerJobID)
	if ownerJobID == "" {
		return 0, false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE owner_job_id = ?
		  AND EXISTS (
			SELECT 1 FROM jobs
			WHERE jobs.id = ?
			  AND jobs.lifecycle_generation = ?
			  AND jobs.state IN ('cancelled', 'failed')
		  )`+excludeLiveAdvanceLockSQL, ownerJobID, ownerJobID, atGeneration, formatResourceLockTime(now))
	if err != nil {
		return 0, false, err
	}
	released, err = result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	var state string
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT state, lifecycle_generation FROM jobs WHERE id = ?`, ownerJobID).Scan(&state, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, tx.Commit()
	}
	if err != nil {
		return 0, false, err
	}
	if generation != atGeneration || (state != "cancelled" && state != "failed") {
		return 0, false, tx.Commit()
	}
	return released, true, tx.Commit()
}

// ForceReleaseDelegationBranchLockAtJobGeneration force-releases a branch lock only
// while the owning job is still the settled lifecycle the caller claimed.
//
// The unguarded ForceReleaseLockWithEvent deletes by (repo, branch) alone, so a
// retry that re-queued and re-acquired the same delegation branch lock lost it to
// the previous run's cleanup. The generation lives in the DELETE's own predicate,
// and the audit event is written in the same transaction, so a lost guard leaves
// both the lock and the history untouched.
func (s *Store) ForceReleaseDelegationBranchLockAtJobGeneration(ctx context.Context, repoFullName string, branch string, ownerJobID string, atGeneration int64, event BranchLockEvent) (bool, error) {
	repoFullName = strings.TrimSpace(repoFullName)
	branch = strings.TrimSpace(branch)
	ownerJobID = strings.TrimSpace(ownerJobID)
	if repoFullName == "" || branch == "" || ownerJobID == "" {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// The guarded DELETE is the FIRST statement, for the same reason as the resource
	// release above: a read taken first would hold a WAL snapshot another process's
	// re-queue could invalidate, turning this into a stale upgrade. RETURNING hands
	// back the owner the audit event needs, so nothing has to be read beforehand.
	var owner string
	err = tx.QueryRowContext(ctx, `DELETE FROM branch_locks
		WHERE repo_full_name = ? AND branch = ?
		  AND EXISTS (
			SELECT 1 FROM jobs
			WHERE jobs.id = ?
			  AND jobs.lifecycle_generation = ?
			  AND jobs.state IN ('cancelled', 'failed')
		  )
		RETURNING owner`, repoFullName, branch, ownerJobID, atGeneration).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		// No lock row, or the guard refused: either way nothing was released and no
		// history is written.
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	event.RepoFullName = repoFullName
	event.Branch = branch
	event.Owner = owner
	if strings.TrimSpace(event.Kind) == "" {
		event.Kind = "released"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lock_events(repo_full_name, branch, owner, kind, message) VALUES (?, ?, ?, ?, ?)`,
		event.RepoFullName, event.Branch, event.Owner, event.Kind, event.Message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// DeleteExpiredResourceLocks reaps lock rows whose lease has elapsed.
//
// The owner_pid<=0 clause keeps the historical conservatism for NON-runtime locks
// a lock that records a live-process owner PID is
// reclaimed by a PID-liveness check elsewhere, not by blind expiry. Runtime-session
// locks (resource_key LIKE 'runtime:%') are the explicit exception: their recorded
// PID is the gitmoot DAEMON's, not the spawned worker's, so it is meaningless after
// a daemon restart and must NOT keep an expired lease alive forever. Without this
// exception an expired runtime-session lock (which always sets owner_pid>0) would
// NEVER be reaped here — stranding the job's recovery and worktree cleanup (#536
// finding 2). Once a runtime lease expires, the daemon may requeue the running
// owner job from the same recovery tick.
func (s *Store) DeleteExpiredResourceLocks(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE expires_at <= ?
			AND (owner_pid <= 0 OR resource_key LIKE 'runtime:%')
			AND (resource_key LIKE 'runtime:%' OR NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.id = resource_locks.owner_job_id
					AND jobs.state = 'running'
			))`, formatResourceLockTime(now))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DeleteExpiredResourceLocksExcludingOwners is DeleteExpiredResourceLocks minus
// locks held by the given owner job IDs (#562): the daemon's recovery tick skips
// reaping a lock whose owner job is in flight in this very process (its worker
// goroutine is alive — e.g. a ctx-deaf runtime overrunning its lease), because
// deleting it would let a second run of the same session start beside the live
// one. An empty exclusion list is byte-identical to DeleteExpiredResourceLocks.
func (s *Store) DeleteExpiredResourceLocksExcludingOwners(ctx context.Context, now time.Time, excludeOwners []string) (int64, error) {
	if len(excludeOwners) == 0 {
		return s.DeleteExpiredResourceLocks(ctx, now)
	}
	placeholders := strings.Repeat("?,", len(excludeOwners))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(excludeOwners)+1)
	args = append(args, formatResourceLockTime(now))
	for _, owner := range excludeOwners {
		args = append(args, owner)
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks
		WHERE expires_at <= ?
			AND (owner_pid <= 0 OR resource_key LIKE 'runtime:%')
			AND (resource_key LIKE 'runtime:%' OR NOT EXISTS (
				SELECT 1 FROM jobs
				WHERE jobs.id = resource_locks.owner_job_id
					AND jobs.state = 'running'
			))
			AND owner_job_id NOT IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) AcquireLock(ctx context.Context, lock BranchLock) (bool, error) {
	created, err := s.CreateLock(ctx, lock)
	if err != nil {
		return false, err
	}
	if created {
		return true, nil
	}

	var owner string
	err = s.db.QueryRowContext(ctx, `SELECT owner FROM branch_locks WHERE repo_full_name = ? AND branch = ?`, lock.RepoFullName, lock.Branch).Scan(&owner)
	if err != nil {
		return false, err
	}
	return owner == lock.Owner, nil
}

func (s *Store) CreateLock(ctx context.Context, lock BranchLock) (bool, error) {
	// #1250: the acting org role is written HERE and only here — one writer, two
	// readers. Attribution belongs to the branch's ownership event, so it is
	// captured when the branch is taken and never rewritten afterwards.
	// The ON CONFLICT arm is deliberately NOT a second writer: it is the SAME
	// writer completing a row it already created. Worktree allocation
	// (AllocateTaskWorktree / AllocateDelegationWorktree) creates the lock BEFORE
	// the role-aware ensureBranchLock runs on the same dispatch, so a plain INSERT
	// OR IGNORE froze the blank attribution forever and the "single writer" claim
	// was false. Filling is restricted to the SAME OWNER and to a currently BLANK
	// role, so a role is never overwritten and one owner can never relabel
	// another's branch. Reacquisition by a different owner still goes through
	// release + fresh insert, which replaces owner and attribution together.
	result, err := s.db.ExecContext(ctx, `INSERT INTO branch_locks(repo_full_name, branch, owner, acting_org_role, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo_full_name, branch) DO UPDATE SET
			acting_org_role = excluded.acting_org_role,
			updated_at = CURRENT_TIMESTAMP
		WHERE branch_locks.owner = excluded.owner
			AND TRIM(branch_locks.acting_org_role) = ''
			AND TRIM(excluded.acting_org_role) <> ''`, lock.RepoFullName, lock.Branch, lock.Owner, strings.TrimSpace(lock.ActingOrgRole))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) GetBranchLock(ctx context.Context, repoFullName string, branch string) (BranchLock, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, branch, owner, skip_native_review_fanout, acting_org_role FROM branch_locks WHERE repo_full_name = ? AND branch = ?`, repoFullName, branch)
	var lock BranchLock
	if err := row.Scan(&lock.RepoFullName, &lock.Branch, &lock.Owner, &lock.SkipNativeReviewFanout, &lock.ActingOrgRole); err != nil {
		return BranchLock{}, err
	}
	return lock, nil
}

func (s *Store) ListBranchLocks(ctx context.Context, repoFullName string) ([]BranchLock, error) {
	query := `SELECT repo_full_name, branch, owner, skip_native_review_fanout, acting_org_role FROM branch_locks`
	args := []any{}
	if strings.TrimSpace(repoFullName) != "" {
		query += ` WHERE repo_full_name = ?`
		args = append(args, repoFullName)
	}
	query += ` ORDER BY repo_full_name, branch`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locks []BranchLock
	for rows.Next() {
		var lock BranchLock
		if err := rows.Scan(&lock.RepoFullName, &lock.Branch, &lock.Owner, &lock.SkipNativeReviewFanout, &lock.ActingOrgRole); err != nil {
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, rows.Err()
}

// parseStoredTimestamp parses a stored SQLite timestamp. Columns defaulted to
// CURRENT_TIMESTAMP are UTC in "2006-01-02 15:04:05" form; RFC3339[Nano] is also
// accepted defensively for columns written explicitly. An unrecognized value yields
// the zero time (callers treat that as "age unknown") rather than an error.
func parseStoredTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ListBranchLocksWithAge returns the branch locks for repoFullName (all repos when
// empty) alongside their created_at/updated_at timestamps, ordered like
// ListBranchLocks. It exists so callers that need to reason about lock AGE (stale
// stranded-lock detection, #617) do not have to widen the lean BranchLock struct
// every read path already scans.
func (s *Store) ListBranchLocksWithAge(ctx context.Context, repoFullName string) ([]BranchLockInfo, error) {
	query := `SELECT repo_full_name, branch, owner, skip_native_review_fanout, acting_org_role, created_at, updated_at FROM branch_locks`
	args := []any{}
	if strings.TrimSpace(repoFullName) != "" {
		query += ` WHERE repo_full_name = ?`
		args = append(args, repoFullName)
	}
	query += ` ORDER BY repo_full_name, branch`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var infos []BranchLockInfo
	for rows.Next() {
		var info BranchLockInfo
		var createdAt, updatedAt string
		if err := rows.Scan(&info.RepoFullName, &info.Branch, &info.Owner, &info.SkipNativeReviewFanout, &info.ActingOrgRole, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		info.CreatedAt = parseStoredTimestamp(createdAt)
		info.UpdatedAt = parseStoredTimestamp(updatedAt)
		infos = append(infos, info)
	}
	return infos, rows.Err()
}

// SetBranchLockReviewFanout persists the skip_native_review_fanout flag onto the
// branch lock for (repoFullName, branch). It is a no-op when no lock exists for
// the pair. The flag is never written at lock creation (CreateLock defaults it to
// 0); only the implement-job advancement path sets it so the daemon's PR-watcher
// can read whether the native review fanout should be skipped.
func (s *Store) SetBranchLockReviewFanout(ctx context.Context, repoFullName string, branch string, skip bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE branch_locks SET skip_native_review_fanout = ?, updated_at = CURRENT_TIMESTAMP WHERE repo_full_name = ? AND branch = ?`, skip, repoFullName, branch)
	return err
}

func (s *Store) ReleaseLock(ctx context.Context, lock BranchLock) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM branch_locks WHERE repo_full_name = ? AND branch = ? AND owner = ?`, lock.RepoFullName, lock.Branch, lock.Owner)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) ReleaseLockWithEvent(ctx context.Context, lock BranchLock, event BranchLockEvent) (bool, error) {
	return s.releaseLockWithEvent(ctx, lock, false, event)
}

// ReleaseBranchLockIfInactiveWithEvent releases lock only when no non-terminal
// task or job still references its repo+branch. ignoredImplementingTaskID lets
// event-driven cancellation exclude only its exact implementing task while the
// stale-task reconciler retains ownership of task dismissal and cleanup. Empty
// disables the exclusion, as the daemon sweeper requires. A zero updatedBefore
// disables the age predicate; the sweeper supplies a cutoff so a newly acquired
// lane can never be reclaimed.
//
// The terminal sets are deliberately allowlists. A new or unknown lifecycle
// state therefore keeps the lock until its release policy is decided explicitly.
// In particular, blocked, awaiting_human_merge, and awaiting_human tasks retain
// their lanes for operator resumption, and blocked jobs remain non-terminal.
func (s *Store) ReleaseBranchLockIfInactiveWithEvent(ctx context.Context, lock BranchLock, ignoredImplementingTaskID string, updatedBefore time.Time, event BranchLockEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	query := `DELETE FROM branch_locks
		WHERE repo_full_name = ? AND branch = ? AND owner = ?`
	args := []any{strings.TrimSpace(lock.RepoFullName), strings.TrimSpace(lock.Branch), strings.TrimSpace(lock.Owner)}
	if !updatedBefore.IsZero() {
		query += ` AND updated_at <= ?`
		args = append(args, updatedBefore.UTC().Format("2006-01-02 15:04:05"))
	}
	args = append(args, strings.TrimSpace(ignoredImplementingTaskID))
	query += `
		AND NOT EXISTS (
			SELECT 1 FROM tasks
			WHERE tasks.repo_full_name = branch_locks.repo_full_name
				AND TRIM(tasks.branch) = TRIM(branch_locks.branch)
				AND NOT (tasks.id = ? AND tasks.state = 'implementing')
				AND (
					tasks.state IN (
						'planned', 'implementing', 'pr_open', 'reviewing', 'changes_requested', 'ready_to_merge',
						'blocked', 'awaiting_human_merge', 'awaiting_human'
					)
					OR tasks.state NOT IN (
						'planned', 'implementing', 'pr_open', 'reviewing', 'changes_requested', 'ready_to_merge',
						'merged', 'superseded', 'stranded', 'blocked', 'awaiting_human_merge', 'dismissed', 'awaiting_human'
					)
				)
		)
		AND NOT EXISTS (
			SELECT 1 FROM jobs
			WHERE jobs.state NOT IN ('succeeded', 'failed', 'cancelled')
				AND json_valid(jobs.payload)
				AND TRIM(COALESCE(json_extract(jobs.payload, '$.branch'), '')) = TRIM(branch_locks.branch)
				AND (
					TRIM(jobs.repo) = TRIM(branch_locks.repo_full_name)
					OR TRIM(COALESCE(json_extract(jobs.payload, '$.repo'), '')) = TRIM(branch_locks.repo_full_name)
				)
		)`
	result, err := tx.ExecContext(ctx, query, args...)
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

	event.RepoFullName = strings.TrimSpace(lock.RepoFullName)
	event.Branch = strings.TrimSpace(lock.Branch)
	event.Owner = strings.TrimSpace(lock.Owner)
	if strings.TrimSpace(event.Kind) == "" {
		event.Kind = "released"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lock_events(repo_full_name, branch, owner, kind, message)
		VALUES (?, ?, ?, ?, ?)`, event.RepoFullName, event.Branch, event.Owner, event.Kind, event.Message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ReleaseTaskLaneBranchLockAtJobGeneration is ReleaseBranchLockIfInactiveWithEvent
// with the owning job's LIFECYCLE pinned.
//
// The inactivity vetoes alone are not enough for a caller that decided a poll ago.
// The tasks veto deliberately EXCLUDES the exact implementing task it was asked
// about, so a retry of that same task cannot re-assert the lane through it; and
// the jobs veto only fires while the retry is non-terminal, so a retry that has
// already re-queued, run and settled terminal passes both. The generation lives in
// the DELETE's own predicate here, which is the only formulation that cannot be
// outrun.
func (s *Store) ReleaseTaskLaneBranchLockAtJobGeneration(ctx context.Context, lock BranchLock, ignoredImplementingTaskID string, ownerJobID string, atGeneration int64, event BranchLockEvent) (bool, error) {
	ownerJobID = strings.TrimSpace(ownerJobID)
	if ownerJobID == "" {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Write first: no read precedes it, so there is no snapshot to stale-upgrade
	// when another process commits a re-queue (the WAL hazard #1673 hit on the
	// resource-lock release).
	result, err := tx.ExecContext(ctx, `DELETE FROM branch_locks
		WHERE repo_full_name = ? AND branch = ? AND owner = ?
		AND EXISTS (
			SELECT 1 FROM jobs
			WHERE jobs.id = ?
			  AND jobs.lifecycle_generation = ?
			  AND jobs.state IN ('cancelled', 'failed')
		)
		AND NOT EXISTS (
			SELECT 1 FROM tasks
			WHERE tasks.repo_full_name = branch_locks.repo_full_name
				AND TRIM(tasks.branch) = TRIM(branch_locks.branch)
				AND NOT (tasks.id = ? AND tasks.state = 'implementing')
				AND (
					tasks.state IN (
						'planned', 'implementing', 'pr_open', 'reviewing', 'changes_requested', 'ready_to_merge',
						'blocked', 'awaiting_human_merge', 'awaiting_human'
					)
					OR tasks.state NOT IN (
						'planned', 'implementing', 'pr_open', 'reviewing', 'changes_requested', 'ready_to_merge',
						'merged', 'superseded', 'stranded', 'blocked', 'awaiting_human_merge', 'dismissed', 'awaiting_human'
					)
				)
		)
		AND NOT EXISTS (
			SELECT 1 FROM jobs
			WHERE jobs.state NOT IN ('succeeded', 'failed', 'cancelled')
				AND json_valid(jobs.payload)
				AND TRIM(COALESCE(json_extract(jobs.payload, '$.branch'), '')) = TRIM(branch_locks.branch)
				AND (
					TRIM(jobs.repo) = TRIM(branch_locks.repo_full_name)
					OR TRIM(COALESCE(json_extract(jobs.payload, '$.repo'), '')) = TRIM(branch_locks.repo_full_name)
				)
		)`,
		strings.TrimSpace(lock.RepoFullName), strings.TrimSpace(lock.Branch), strings.TrimSpace(lock.Owner),
		ownerJobID, atGeneration, strings.TrimSpace(ignoredImplementingTaskID))
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
	event.RepoFullName = strings.TrimSpace(lock.RepoFullName)
	event.Branch = strings.TrimSpace(lock.Branch)
	event.Owner = strings.TrimSpace(lock.Owner)
	if strings.TrimSpace(event.Kind) == "" {
		event.Kind = "released"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lock_events(repo_full_name, branch, owner, kind, message)
		VALUES (?, ?, ?, ?, ?)`, event.RepoFullName, event.Branch, event.Owner, event.Kind, event.Message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) ForceReleaseLockWithEvent(ctx context.Context, repoFullName string, branch string, event BranchLockEvent) (BranchLock, bool, error) {
	lock, err := s.GetBranchLock(ctx, repoFullName, branch)
	if errors.Is(err, sql.ErrNoRows) {
		return BranchLock{}, false, nil
	}
	if err != nil {
		return BranchLock{}, false, err
	}
	released, err := s.releaseLockWithEvent(ctx, lock, true, event)
	if err != nil {
		return BranchLock{}, false, err
	}
	if !released {
		return BranchLock{}, false, nil
	}
	return lock, true, nil
}

func (s *Store) releaseLockWithEvent(ctx context.Context, lock BranchLock, force bool, event BranchLockEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	current := lock
	if force || strings.TrimSpace(current.Owner) == "" {
		row := tx.QueryRowContext(ctx, `SELECT repo_full_name, branch, owner FROM branch_locks WHERE repo_full_name = ? AND branch = ?`, lock.RepoFullName, lock.Branch)
		if err := row.Scan(&current.RepoFullName, &current.Branch, &current.Owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, tx.Commit()
			}
			return false, err
		}
	}

	query := `DELETE FROM branch_locks WHERE repo_full_name = ? AND branch = ? AND owner = ?`
	args := []any{current.RepoFullName, current.Branch, current.Owner}
	if force {
		query = `DELETE FROM branch_locks WHERE repo_full_name = ? AND branch = ?`
		args = []any{current.RepoFullName, current.Branch}
	}
	result, err := tx.ExecContext(ctx, query, args...)
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

	event.RepoFullName = current.RepoFullName
	event.Branch = current.Branch
	event.Owner = current.Owner
	if strings.TrimSpace(event.Kind) == "" {
		event.Kind = "released"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lock_events(repo_full_name, branch, owner, kind, message)
		VALUES (?, ?, ?, ?, ?)`, event.RepoFullName, event.Branch, event.Owner, event.Kind, event.Message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) ListBranchLockEvents(ctx context.Context, repoFullName string, branch string) ([]BranchLockEvent, error) {
	query := `SELECT repo_full_name, branch, owner, kind, message FROM lock_events`
	args := []any{}
	conditions := []string{}
	if strings.TrimSpace(repoFullName) != "" {
		conditions = append(conditions, "repo_full_name = ?")
		args = append(args, repoFullName)
	}
	if strings.TrimSpace(branch) != "" {
		conditions = append(conditions, "branch = ?")
		args = append(args, branch)
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY rowid`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []BranchLockEvent
	for rows.Next() {
		var event BranchLockEvent
		if err := rows.Scan(&event.RepoFullName, &event.Branch, &event.Owner, &event.Kind, &event.Message); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) UpsertMergeGate(ctx context.Context, gate MergeGate) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO merge_gates(repo_full_name, pull_request, state, reason, block_class, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo_full_name, pull_request) DO UPDATE SET
			state = excluded.state,
			reason = excluded.reason,
			block_class = excluded.block_class,
			updated_at = CURRENT_TIMESTAMP`,
		gate.RepoFullName, gate.PullRequest, gate.State, gate.Reason, gate.BlockClass)
	return err
}

func (s *Store) GetMergeGate(ctx context.Context, repoFullName string, pullRequest int64) (MergeGate, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, pull_request, state, reason, block_class
		FROM merge_gates WHERE repo_full_name = ? AND pull_request = ?`,
		repoFullName, pullRequest)
	var gate MergeGate
	if err := row.Scan(&gate.RepoFullName, &gate.PullRequest, &gate.State, &gate.Reason, &gate.BlockClass); err != nil {
		return MergeGate{}, err
	}
	return gate, nil
}

func (s *Store) UpsertMergeGateStatusObservation(ctx context.Context, obs MergeGateStatusObservation) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO merge_gate_status_observations(repo_full_name, pull_request, head_sha, kind, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo_full_name, pull_request) DO UPDATE SET
			head_sha = excluded.head_sha,
			kind = excluded.kind,
			updated_at = CURRENT_TIMESTAMP`,
		obs.RepoFullName, obs.PullRequest, obs.HeadSHA, obs.Kind)
	return err
}

func (s *Store) GetMergeGateStatusObservation(ctx context.Context, repoFullName string, pullRequest int64) (MergeGateStatusObservation, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, pull_request, head_sha, kind
		FROM merge_gate_status_observations WHERE repo_full_name = ? AND pull_request = ?`,
		repoFullName, pullRequest)
	var obs MergeGateStatusObservation
	if err := row.Scan(&obs.RepoFullName, &obs.PullRequest, &obs.HeadSHA, &obs.Kind); err != nil {
		return MergeGateStatusObservation{}, err
	}
	return obs, nil
}

// UpsertNoCIObservation records (or refreshes) the first zero-external CI
// observation for a PR (#596). Recording at a new head SHA overwrites the prior
// observation, which is exactly the reset-on-new-head semantics the merge gate
// relies on.
func (s *Store) UpsertNoCIObservation(ctx context.Context, obs NoCIObservation) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO merge_gate_ci_observations(repo_full_name, pull_request, head_sha, first_zero_at, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo_full_name, pull_request) DO UPDATE SET
			head_sha = excluded.head_sha,
			first_zero_at = excluded.first_zero_at,
			updated_at = CURRENT_TIMESTAMP`,
		obs.RepoFullName, obs.PullRequest, obs.HeadSHA, obs.FirstZeroAt)
	return err
}

// GetNoCIObservation returns the recorded first zero-external CI observation for
// a PR, or sql.ErrNoRows if none has been recorded yet (#596).
func (s *Store) GetNoCIObservation(ctx context.Context, repoFullName string, pullRequest int64) (NoCIObservation, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, pull_request, head_sha, first_zero_at
		FROM merge_gate_ci_observations WHERE repo_full_name = ? AND pull_request = ?`,
		repoFullName, pullRequest)
	var obs NoCIObservation
	if err := row.Scan(&obs.RepoFullName, &obs.PullRequest, &obs.HeadSHA, &obs.FirstZeroAt); err != nil {
		return NoCIObservation{}, err
	}
	return obs, nil
}
