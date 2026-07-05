package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// worktreeHasLiveProcess reports whether a live process on this host still has its
// working directory inside the worktree at path. It is the lock-independent,
// PID-reuse- and hostname-rename-immune never-clobber gate the destructive cleanup
// consults so a worktree a daemon-crash-reparented worker is STILL writing is not
// force-removed even after its runtime-session lease has expired and its lock been
// reaped (#536 finding 1).
func (e Engine) worktreeHasLiveProcess(path string) bool {
	if e.WorktreeHasLiveProcess != nil {
		return e.WorktreeHasLiveProcess(path)
	}
	return defaultWorktreeHasLiveProcess(path)
}

// WorktreeHasLiveProcess exposes the same conservative /proc cwd liveness probe
// used by destructive worktree cleanup to CLI recovery/dispatch paths. It is
// intentionally best-effort: false means "no same-host cwd owner observed", not a
// proof that no external process can write.
func WorktreeHasLiveProcess(path string) bool {
	return defaultWorktreeHasLiveProcess(path)
}

// defaultWorktreeHasLiveProcess is a best-effort Linux /proc scan: it returns true
// when any process's cwd symlink resolves to path or a descendant of it. The codex
// resume worker in #536 ran with cwd == the delegation worktree, so its presence is
// the decisive "still writing" signal independent of any lock or PID. On a platform
// without a readable /proc (e.g. darwin) it returns false — it cannot detect a live
// worker there, so it favors not stranding the worktree (the lease/lock gate still
// applies while the lease is unexpired). A worktree mid-removal can report a cwd of
// "<path> (deleted)"; that suffix is stripped before comparison.
func defaultWorktreeHasLiveProcess(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	self := os.Getpid()
	for _, entry := range entries {
		name := entry.Name()
		if len(name) == 0 || name[0] < '0' || name[0] > '9' {
			continue
		}
		// Skip our own process: its cwd is never the worktree (the daemon runs from
		// the home/checkout), but guard regardless so a test driving cleanup from a
		// worktree cwd cannot self-trip.
		if pid, perr := strconv.Atoi(name); perr == nil && pid == self {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", name, "cwd"))
		if err != nil {
			continue
		}
		cwd = strings.TrimSuffix(strings.TrimSpace(cwd), " (deleted)")
		if cwd == abs || strings.HasPrefix(cwd, abs+string(os.PathSeparator)) {
			return true
		}
	}
	return false
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
