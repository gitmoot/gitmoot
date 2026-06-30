package workflow

import (
	"context"
	"os"
	"syscall"
)

// runtimeOwnerActive reports whether jobID's runtime-session lock is still held by
// an active owner — an unexpired lease, a live same-host owner PID, or an
// unverifiable cross-host owner. It is the conservative gate the DESTRUCTIVE
// implement-worktree cleanup consults so a worktree owned by a still-running
// runtime worker is never force-removed (#536). A job that holds no runtime lock
// (the healthy case once a terminal transition has released it) is never active,
// so cleanup behavior is unchanged. The returned reason is used for the skip event.
func (e Engine) runtimeOwnerActive(ctx context.Context, jobID string) (bool, string) {
	if e.Store == nil {
		return false, ""
	}
	thisHost, _ := os.Hostname()
	liveness, err := e.Store.JobRuntimeLockLiveness(ctx, jobID, e.now().UTC(), thisHost, e.ownerPIDLive())
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
