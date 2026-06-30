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
// branch deletion). On a healthy terminal transition the lock is already released
// (no row), so this reports false and cleanup proceeds unchanged.
//
// It is keyed on the LEASE alone — the live-PID and cross-host signals are
// deliberately NOT consulted once the lease has expired (#536 findings 2/5):
//   - A cross-host owner past its lease is almost always our OWN pre-restart self
//     under a new hostname (k8s/docker pod recreate). Treating it as active forever
//     would strand the job's worktree+branch permanently — the inverse #536 failure.
//   - A live same-host PID past its lease is far more likely PID reuse than the
//     real worker: the lock records the gitmoot DAEMON's PID, not the spawned
//     runtime worker's, so a recycled PID must NOT block reclamation.
//
// The never-clobber protection for a worker that is genuinely STILL RUNNING past
// lease expiry is provided separately and correctly by the physical
// worktree-process probe the cleanup gate consults
// (workflow.Engine.worktreeHasLiveProcess), which is PID-reuse- and
// hostname-rename-immune. So past lease expiry this lock-based gate steps aside and
// the physical probe decides.
func (l ResourceLockLiveness) Active() bool {
	return l.LeaseUnexpired
}

// LiveAndUnexpired reports a STRICT live owner: an unexpired lease whose owner is
// provably alive (a live same-host PID) or on an unverifiable remote host. A dead
// same-host owner (an unexpired lease left by a crashed worker) is NOT strict-live.
//
// NOTE: this is NO LONGER the gate stale-running-job recovery consults — see
// LeaseHeld for why the PID signal is unsafe on the daemon-restart path. It is
// retained for callers that genuinely need a provably-live owner (and for the
// liveness classification table test).
func (l ResourceLockLiveness) LiveAndUnexpired() bool {
	return l.LeaseUnexpired && (l.OwnerPIDLive || l.CrossHost)
}

// LeaseHeld reports whether the lock's timeout contract is still in force: its
// lease has not elapsed. This is the gate stale-running-job recovery consults
// (#536).
//
// It consults ONLY the lease — neither OwnerPIDLive nor CrossHost.
//
// It does NOT consult OwnerPIDLive because the lock records the PID of the gitmoot
// DAEMON process that acquired it, NOT the spawned codex/claude/kimi runtime
// worker. On a daemon restart (crash/redeploy/OOM) the runtime worker is reparented
// to init and keeps running, but the lock still carries the OLD, now dead daemon
// PID. A PID-based "is it alive?" check therefore reports the wrong answer exactly
// on the restart path this recovery is named for, and would requeue a job whose
// worker is still progressing — the original #536 failure.
//
// It does NOT consult CrossHost (the previous behavior, fixed in #536 finding 2)
// because a cross-host lock whose lease has EXPIRED is almost always our own
// pre-restart self under a new hostname (k8s/docker pod recreate); keeping it
// "held" forever would mean the job is never requeued and its worktree never
// cleaned — a permanent strand (the inverse failure). A cross-host lock whose lease
// is UNEXPIRED is still protected, because LeaseUnexpired is true in that case.
//
// The lease encodes the effective job timeout and survives a restart in the DB, so
// it is the correct (PID-reuse- and hostname-rename-immune) contract: do not
// requeue while the lease is unexpired; once it expires
// (recoverExpiredRuntimeSessionLocks reaps the expired runtime-session lock) the
// job is requeued. The trade-off is that a genuinely-crashed worker whose daemon
// also died is not requeued until its lease expires rather than at the coarse 30m
// threshold — promptness traded for never failing live work, which is the
// unattended-reliability goal.
func (l ResourceLockLiveness) LeaseHeld() bool {
	return l.LeaseUnexpired
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
//
// excludeOwnerToken, when non-empty, drops any lock the CALLER itself currently
// holds (matched by owner token) from the classification. This lets an in-flight
// run that is finishing normally — which still holds its OWN runtime-session lock
// because the daemon releases it only AFTER engine.RunJob (and thus AdvanceJob's
// terminal worktree cleanup) returns — distinguish its own about-to-be-released
// lock from a FOREIGN live owner. Recovery/retry paths that hold no lock pass "".
func (s *Store) JobRuntimeLockLiveness(ctx context.Context, ownerJobID string, now time.Time, thisHost string, pidAlive func(int64) bool, excludeOwnerToken string) (*ResourceLockLiveness, error) {
	ownerJobID = strings.TrimSpace(ownerJobID)
	if ownerJobID == "" {
		return nil, nil
	}
	excludeOwnerToken = strings.TrimSpace(excludeOwnerToken)
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
		if excludeOwnerToken != "" && strings.TrimSpace(lock.OwnerToken) == excludeOwnerToken {
			continue
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
