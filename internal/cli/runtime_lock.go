package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// acquireRuntimeSessionLock acquires the per-job runtime-session lock and returns
// a release closure, whether it was acquired, the resource key, the owner token
// recorded on the lock, and any error. The owner token is surfaced so the caller
// can thread it into the run context (workflow.WithRuntimeSelfOwnerToken): the
// terminal worktree cleanup runs while this lock is still held (it is released
// only after the run returns), so cleanup must be able to recognize the run's OWN
// lock and not treat it as a foreign live owner (#536). A non-resumable runtime
// (no resource key) returns an empty token and a no-op release.
func acquireRuntimeSessionLock(ctx context.Context, store *db.Store, jobID string, agent runtime.Agent, now time.Time, ttl time.Duration) (func(context.Context) error, bool, string, string, error) {
	key, ok := runtimeSessionResourceKey(agent)
	if !ok {
		return func(context.Context) error { return nil }, true, "", "", nil
	}
	if ttl <= 0 {
		return nil, false, key, "", fmt.Errorf("runtime lock ttl must be positive")
	}
	ownerToken, err := newRuntimeLockOwnerToken()
	if err != nil {
		return nil, false, key, "", err
	}
	// Record the acquiring process's identity (additive metadata) so a later
	// liveness check — e.g. `agent restart`'s session-lock guard (#425) — can tell
	// a LIVE same-host foreground session from a STRANDED (dead-owner) one. This is
	// purely additive: AcquireResourceLock does not gate on OwnerPID, so the locking
	// semantics are unchanged. hostname is best-effort: an empty host is treated as
	// this/local host by the consumer (local-first, mirroring the #303 recovery).
	hostname, _ := os.Hostname()
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey:   key,
		OwnerJobID:    jobID,
		OwnerToken:    ownerToken,
		OwnerPID:      int64(os.Getpid()),
		OwnerHostname: strings.TrimSpace(hostname),
		ExpiresAt:     now.UTC().Add(ttl).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		return func(context.Context) error { return nil }, acquired, key, "", err
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, jobID, ownerToken)
		return err
	}, true, key, ownerToken, nil
}

func runtimeSessionResourceKey(agent runtime.Agent) (string, bool) {
	runtimeName := strings.TrimSpace(agent.Runtime)
	runtimeRef := strings.TrimSpace(agent.RuntimeRef)
	switch runtimeName {
	case runtime.CodexRuntime, runtime.ClaudeRuntime, runtime.KimiRuntime:
	default:
		return "", false
	}
	if runtimeRef == "" {
		return "", false
	}
	return "runtime:" + runtimeName + ":" + runtimeRef, true
}

// runtimeSessionHeldByLiveOwner reports whether the agent's runtime:<rt>:<ref>
// session lock is currently held by a provably-LIVE, same-host owner. It is the
// guard `agent restart` uses to refuse clobbering a live foreground `agent ask`
// while still PROCEEDING on a stranded/expired/absent lock (the recovery story).
//
// Classification (mirrors the #303 generation-lock recovery, inverted — there we
// reclaim only a provably-dead lock, here we BLOCK only a provably-live one):
//   - no resource key (non-resumable runtime / empty ref) → not held.
//   - no lock (ErrNoRows) → not held (proceed).
//   - lease expired (ExpiresAt < now) → not held (stale/self-clearing → proceed).
//   - active lease, same host (empty host = legacy/local, treated as this host),
//     OwnerPID > 0, and the PID is live → HELD (reject).
//   - active lease, same host, PID dead or ≤0 (legacy) → not held (stranded → proceed).
//   - active lease, genuinely cross-host (non-empty, different host — liveness not
//     locally verifiable) → HELD (conservative: a cross-host runtime-session lock
//     is not a local-first scenario, so refuse rather than risk abandoning a
//     possibly-live session).
//
// A GetResourceLock error other than not-found is returned (never silently proceed).
func runtimeSessionHeldByLiveOwner(ctx context.Context, store *db.Store, agent runtime.Agent) (bool, string, error) {
	key, ok := runtimeSessionResourceKey(agent)
	if !ok {
		return false, "", nil
	}
	lock, err := store.GetResourceLock(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, "", nil
		}
		return false, "", err
	}
	now := time.Now().UTC()
	if expiresAt, parsed := parseSkillOptStatusTime(lock.ExpiresAt); parsed && !expiresAt.After(now) {
		// Lease expired: stale and self-clearing on the next acquire — proceed.
		return false, "", nil
	}
	host := strings.TrimSpace(lock.OwnerHostname)
	thisHost, _ := os.Hostname()
	sameHost := host == "" || strings.EqualFold(host, strings.TrimSpace(thisHost))
	hostText := host
	if hostText == "" {
		hostText = "this host"
	}
	switch {
	case sameHost && lock.OwnerPID > 0 && skillOptOwnerPIDLive(lock.OwnerPID):
		// Live same-host owner: refuse — restarting would clobber a live session.
		return true, fmt.Sprintf("held by pid %d on %s", lock.OwnerPID, hostText), nil
	case sameHost:
		// Same-host dead/legacy owner: stranded session — restart recovers it.
		return false, "", nil
	default:
		// Cross-host owner we cannot liveness-check locally: refuse conservatively.
		return true, fmt.Sprintf("held by pid %d on %s (cross-host; liveness unverifiable)", lock.OwnerPID, hostText), nil
	}
}

// runtimeOwnerLeaseHeld reports whether the running job jobID still holds a
// runtime-session lock whose LEASE has not elapsed (or whose owner is on an
// unverifiable cross-host). It is the gate stale-running-job recovery consults so
// a long-running job is not requeued while its real job timeout — encoded in the
// lease — has not elapsed (#536).
//
// It deliberately keys on the lease, NOT on owner-PID liveness: the lock records
// the gitmoot DAEMON's PID, not the spawned runtime worker's, so on a daemon
// restart the recorded PID is the dead prior daemon even while the reparented
// worker is still progressing. Honoring the lease is therefore correct across a
// restart and immune to PID reuse; see db.ResourceLockLiveness.LeaseHeld. A job
// with no runtime lock (released on a normal terminal, or a non-resumable runtime)
// is never lease-held, so it is recovered as before; once a lease expires
// (recoverExpiredRuntimeSessionLocks reclaims it) the job is recovered too.
func runtimeOwnerLeaseHeld(ctx context.Context, store *db.Store, jobID string, now time.Time) (bool, error) {
	thisHost, _ := os.Hostname()
	// excludeOwnerToken is "" — recovery runs from a daemon tick/startup that holds
	// no runtime-session lock of its own, so there is nothing to exclude.
	liveness, err := store.JobRuntimeLockLiveness(ctx, jobID, now, thisHost, skillOptOwnerPIDLive, "")
	if err != nil {
		return false, err
	}
	if liveness == nil {
		return false, nil
	}
	return liveness.LeaseHeld(), nil
}

func newRuntimeLockOwnerToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate runtime lock owner token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}
