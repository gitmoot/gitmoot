package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RuntimeSessionLockKeyPrefix is the resource_key prefix of the per-job
// runtime-session lock (runtime:<runtime>:<ref>). A running job that drives a
// resumable runtime (codex/claude/kimi) holds exactly one such lock for the
// duration of its run; it records the owning gitmoot process PID, its host, and a
// lease whose expiry reflects the effective job timeout. The lock is RELEASED on a
// normal terminal transition, so its continued presence after a job has been
// "running" past a coarse threshold is the liveness signal that stale-recovery and
// destructive worktree-cleanup consult before acting (#536).
const RuntimeSessionLockKeyPrefix = "runtime:"

// ResourceLockLiveness is the liveness classification of a held resource lock,
// derived from its lease expiry, owner host, and owner PID. It deliberately
// separates the three orthogonal signals so different callers can compose their
// own policy: stale-recovery requires a STRICT live owner (an unexpired lease AND
// a provably-alive owner) before it declines to requeue, whereas destructive
// worktree-cleanup must be conservative and treats ANY of the signals as "still
// owned" before it declines to force-remove.
type ResourceLockLiveness struct {
	// LeaseUnexpired is true when the lock's expiry is still in the future — the
	// runtime contract (job timeout) the lock encodes has not elapsed.
	LeaseUnexpired bool
	// OwnerPIDLive is true when the owner is on this host and its recorded PID is a
	// provably-live process.
	OwnerPIDLive bool
	// CrossHost is true when the owner is on a different, named host whose process
	// liveness cannot be verified locally. Treated conservatively as possibly-live.
	CrossHost bool
	// Reason is a short human-readable description of the strongest live signal.
	Reason string
}

// Active reports whether the owner should be treated as STILL owning its
// resources for the purpose of DESTRUCTIVE actions (force worktree removal,
// branch deletion). It is conservative: an unexpired lease, a live same-host PID,
// or an unverifiable cross-host owner each independently means "do not destroy".
// On a healthy terminal transition the lock is already released (no row), so this
// reports false and cleanup proceeds unchanged.
func (l ResourceLockLiveness) Active() bool {
	return l.LeaseUnexpired || l.OwnerPIDLive || l.CrossHost
}

// LiveAndUnexpired reports a STRICT live owner: an unexpired lease whose owner is
// provably alive (a live same-host PID) or on an unverifiable remote host. It
// gates stale-running-job recovery so a long-running job whose runtime lease has
// not elapsed and whose owner process is still alive is not wrongly requeued as
// stale. A dead same-host owner (an unexpired lease left by a crashed worker) is
// NOT strict-live, so legitimate restart recovery still proceeds.
func (l ResourceLockLiveness) LiveAndUnexpired() bool {
	return l.LeaseUnexpired && (l.OwnerPIDLive || l.CrossHost)
}

// classifyResourceLockLiveness applies the host/PID/lease classification used by
// runtimeSessionHeldByLiveOwner (internal/cli/runtime_lock.go), generalized to
// operate on any held lock and with an injectable PID-liveness probe so it is
// pure and table-testable. An empty owner host is treated as this/local host
// (legacy/local-first), mirroring the #303 recovery.
func classifyResourceLockLiveness(lock ResourceLock, now time.Time, thisHost string, pidAlive func(int64) bool) ResourceLockLiveness {
	res := ResourceLockLiveness{}
	if expiresAt, ok := parseResourceLockTime(lock.ExpiresAt); ok && expiresAt.After(now) {
		res.LeaseUnexpired = true
	}
	host := strings.TrimSpace(lock.OwnerHostname)
	thisHost = strings.TrimSpace(thisHost)
	sameHost := host == "" || strings.EqualFold(host, thisHost)
	hostText := host
	if hostText == "" {
		hostText = "this host"
	}
	switch {
	case sameHost && lock.OwnerPID > 0 && pidAlive != nil && pidAlive(lock.OwnerPID):
		res.OwnerPIDLive = true
		res.Reason = fmt.Sprintf("owner pid %d live on %s", lock.OwnerPID, hostText)
	case !sameHost:
		res.CrossHost = true
		res.Reason = fmt.Sprintf("owner pid %d on %s (cross-host; liveness unverifiable)", lock.OwnerPID, hostText)
	}
	if res.Reason == "" && res.LeaseUnexpired {
		res.Reason = fmt.Sprintf("lease for %s held by job %s not yet expired", strings.TrimSpace(lock.ResourceKey), strings.TrimSpace(lock.OwnerJobID))
	}
	return res
}

func resourceLockLivenessScore(l ResourceLockLiveness) int {
	score := 0
	if l.OwnerPIDLive {
		score += 4
	}
	if l.CrossHost {
		score += 2
	}
	if l.LeaseUnexpired {
		score++
	}
	return score
}

// JobRuntimeLockLiveness returns the liveness of the runtime-session lock(s) held
// by ownerJobID, or nil when the job holds no such lock (the healthy case once a
// terminal transition has released it). When a job holds more than one runtime
// lock the strongest live signal is returned. pidAlive is the same-host process
// liveness probe; thisHost is the local hostname used for the host comparison.
func (s *Store) JobRuntimeLockLiveness(ctx context.Context, ownerJobID string, now time.Time, thisHost string, pidAlive func(int64) bool) (*ResourceLockLiveness, error) {
	ownerJobID = strings.TrimSpace(ownerJobID)
	if ownerJobID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT resource_key, owner_job_id, owner_token, owner_pid, owner_hostname, command_hash, acquired_at, updated_at, expires_at
		FROM resource_locks
		WHERE owner_job_id = ? AND resource_key LIKE ?
		ORDER BY resource_key`, ownerJobID, RuntimeSessionLockKeyPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var best *ResourceLockLiveness
	bestScore := -1
	for rows.Next() {
		var lock ResourceLock
		if err := rows.Scan(&lock.ResourceKey, &lock.OwnerJobID, &lock.OwnerToken, &lock.OwnerPID, &lock.OwnerHostname, &lock.CommandHash, &lock.AcquiredAt, &lock.UpdatedAt, &lock.ExpiresAt); err != nil {
			return nil, err
		}
		liveness := classifyResourceLockLiveness(lock, now, thisHost, pidAlive)
		if score := resourceLockLivenessScore(liveness); score > bestScore {
			bestScore = score
			copied := liveness
			best = &copied
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return best, nil
}

func parseResourceLockTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), true
	}
	return time.Time{}, false
}
