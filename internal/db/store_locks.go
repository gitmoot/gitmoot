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
// is dead, but its lease survives in the DB and would otherwise block the requeued
// owner job from re-acquiring its session lock on re-dispatch — so it is reclaimed
// regardless of lease. The owner job itself is requeued by
// RequeueRunningJobsFromForeignBoot (its runner_boot_id matches this lock's
// foreign boot), so this method only reclaims the lock row. It returns the number
// of locks released.
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
		WHERE resource_key LIKE ? AND owner_boot_id != '' AND owner_boot_id != ?`,
		RuntimeSessionLockKeyPrefix+"%", currentBootID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ResourceLockOwnerBootID returns the recorded owner_boot_id for a held lock, or
// "" when the lock is absent or its boot id was never stamped (#651). It is a
// targeted single-column read kept deliberately OUT of the shared 9-column lock
// SELECTs so their scan arity is unchanged; the skillopt generation-lock recovery
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

// DeleteResourceLocksByOwner releases every resource lock held by ownerJobID,
// regardless of token/expiry — used when a job is cancelled and can no longer
// renew its locks. Returns the number released.
func (s *Store) DeleteResourceLocksByOwner(ctx context.Context, ownerJobID string) (int64, error) {
	ownerJobID = strings.TrimSpace(ownerJobID)
	if ownerJobID == "" {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks WHERE owner_job_id = ?`, ownerJobID)
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
func (s *Store) DeleteResourceLocksByOwnerIfNotRunning(ctx context.Context, ownerJobID string) (int64, error) {
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
			)`, ownerJobID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DeleteExpiredResourceLocks reaps lock rows whose lease has elapsed.
//
// The owner_pid<=0 clause keeps the historical conservatism for NON-runtime locks
// (e.g. skillopt-train-generation): a lock that records a live-process owner PID is
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
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO branch_locks(repo_full_name, branch, owner, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)`, lock.RepoFullName, lock.Branch, lock.Owner)
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
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, branch, owner, skip_native_review_fanout FROM branch_locks WHERE repo_full_name = ? AND branch = ?`, repoFullName, branch)
	var lock BranchLock
	if err := row.Scan(&lock.RepoFullName, &lock.Branch, &lock.Owner, &lock.SkipNativeReviewFanout); err != nil {
		return BranchLock{}, err
	}
	return lock, nil
}

func (s *Store) ListBranchLocks(ctx context.Context, repoFullName string) ([]BranchLock, error) {
	query := `SELECT repo_full_name, branch, owner, skip_native_review_fanout FROM branch_locks`
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
		if err := rows.Scan(&lock.RepoFullName, &lock.Branch, &lock.Owner, &lock.SkipNativeReviewFanout); err != nil {
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
	query := `SELECT repo_full_name, branch, owner, skip_native_review_fanout, created_at, updated_at FROM branch_locks`
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
		if err := rows.Scan(&info.RepoFullName, &info.Branch, &info.Owner, &info.SkipNativeReviewFanout, &createdAt, &updatedAt); err != nil {
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO merge_gates(repo_full_name, pull_request, state, reason, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(repo_full_name, pull_request) DO UPDATE SET
			state = excluded.state,
			reason = excluded.reason,
			updated_at = CURRENT_TIMESTAMP`,
		gate.RepoFullName, gate.PullRequest, gate.State, gate.Reason)
	return err
}

func (s *Store) GetMergeGate(ctx context.Context, repoFullName string, pullRequest int64) (MergeGate, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_full_name, pull_request, state, reason
		FROM merge_gates WHERE repo_full_name = ? AND pull_request = ?`,
		repoFullName, pullRequest)
	var gate MergeGate
	if err := row.Scan(&gate.RepoFullName, &gate.PullRequest, &gate.State, &gate.Reason); err != nil {
		return MergeGate{}, err
	}
	return gate, nil
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
