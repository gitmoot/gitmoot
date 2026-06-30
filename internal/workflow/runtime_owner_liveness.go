package workflow

import (
	"context"
	"os"
	"syscall"
)

// runtimeSelfOwnerTokenKey carries the runtime-session lock owner token of the
// run that is CURRENTLY executing this job in-process (set by the daemon worker
// after it acquires the lock). It lets the destructive worktree-cleanup gate tell
// its OWN about-to-be-released lock from a foreign live owner — see
// runtimeOwnerActive.
type runtimeSelfOwnerTokenKey struct{}

// WithRuntimeSelfOwnerToken tags ctx with the owner token of the runtime-session
// lock the current in-flight run holds. The daemon sets this immediately after
// acquiring the lock so that the terminal worktree cleanup — which runs inside
// engine.RunJob -> AdvanceJob while the daemon STILL holds the lock (it releases
// only after RunJob returns) — does not mistake the run's own lock for a foreign
// live owner and refuse the healthy-path cleanup (#536 / #478 regression).
func WithRuntimeSelfOwnerToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, runtimeSelfOwnerTokenKey{}, token)
}

func runtimeSelfOwnerToken(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	token, _ := ctx.Value(runtimeSelfOwnerTokenKey{}).(string)
	return token
}

// runtimeOwnerActive reports whether jobID's runtime-session lock is still held by
// an active FOREIGN owner — an unexpired lease, a live same-host owner PID, or an
// unverifiable cross-host owner. It is the conservative gate the DESTRUCTIVE
// implement-worktree cleanup consults so a worktree owned by a still-running
// runtime worker is never force-removed (#536).
//
// The run that is finishing normally still holds its OWN runtime-session lock at
// cleanup time, because the daemon releases that lock only AFTER engine.RunJob
// (hence AdvanceJob's deferred cleanup) returns. Counting that own lock as
// "active" would refuse cleanup on EVERY healthy completion and leak the worktree
// + gitmoot-delegation-* branch (the #478 regression). So the run's own lock —
// identified by the owner token threaded through ctx — is excluded; only a foreign
// owner gates the destructive removal. A job that holds no (other) runtime lock is
// never active, so cleanup behavior is unchanged.
func (e Engine) runtimeOwnerActive(ctx context.Context, jobID string) (bool, string) {
	if e.Store == nil {
		return false, ""
	}
	thisHost, _ := os.Hostname()
	liveness, err := e.Store.JobRuntimeLockLiveness(ctx, jobID, e.now().UTC(), thisHost, e.ownerPIDLive(), runtimeSelfOwnerToken(ctx))
	if err != nil || liveness == nil {
		return false, ""
	}
	return liveness.Active(), liveness.Reason
}

func (e Engine) ownerPIDLive() func(int64) bool {
	if e.OwnerPIDLive != nil {
		return e.OwnerPIDLive
	}
	return defaultOwnerPIDLive
}

// defaultOwnerPIDLive probes same-host process liveness via signal 0, mirroring
// the cli daemon's processRunning. EPERM (process exists but is not ours) counts
// as live; ESRCH means gone.
func defaultOwnerPIDLive(pid int64) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(int(pid), 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}
