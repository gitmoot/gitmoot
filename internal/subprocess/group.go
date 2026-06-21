package subprocess

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// groupKillGrace is how long a cancelled process group gets to exit after
// SIGTERM before the remaining processes are SIGKILLed.
const groupKillGrace = 10 * time.Second

// GroupRunner runs commands in their own process group and, on context
// cancellation, signals the WHOLE group (SIGTERM, then SIGKILL after a grace
// period). Plain exec.CommandContext only kills the immediate child, which
// orphans grandchildren — runtime CLIs like codex/claude spawn helpers that
// must die with the job. Used by the runtime adapters; short-lived tool calls
// (gh, git) keep the plain ExecRunner.
type GroupRunner struct{}

func (GroupRunner) Run(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	return RunGroup(ctx, dir, command, args...)
}

// RunStream gives GroupRunner the StreamRunner contract: process-group kill
// semantics PLUS a live line-tee of the child's stdout/stderr to out. It is what
// TeeRunner{} defaults to, so teeing a runtime adapter's output into a per-job
// log keeps the same whole-group cancellation the adapters rely on. A nil out
// degrades to RunGroup.
func (GroupRunner) RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error) {
	return RunGroupStream(ctx, dir, out, command, args...)
}

func (GroupRunner) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

// RunGroup is Run with process-group semantics: the child gets its own pgid
// (Setpgid) so the daemon never signals itself, cancellation SIGTERMs the
// group, Go's WaitDelay reaps a stuck main child after the grace period, and a
// final best-effort SIGKILL sweeps any group members that ignored SIGTERM.
func RunGroup(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	cmd, sweep := newGroupCmd(ctx, dir, command, args)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	sweep()
	return Result{
		Command: command,
		Args:    args,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}, err
}

// RunGroupStream is RunGroup that additionally streams the child's stdout and
// stderr to out, line by line, as they are produced — the buffered Result is
// byte-identical to RunGroup's, so the tee is purely additive. A nil out
// degrades to RunGroup. The whole-group cancellation/sweep is identical.
func RunGroupStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error) {
	if out == nil {
		return RunGroup(ctx, dir, command, args...)
	}
	cmd, sweep := newGroupCmd(ctx, dir, command, args)
	result, err := runStreamingCmd(cmd, out, command, args)
	sweep()
	return result, err
}

// newGroupCmd builds an *exec.Cmd wired for process-group semantics and returns
// it alongside a sweep closure to call after cmd.Run() returns. The child gets
// its own pgid (Setpgid) so the daemon never signals itself; on ctx cancel the
// whole group is SIGTERMed (pgid resolved while alive, then remembered for the
// sweep — re-resolving after the child is reaped could chase a reused pid into
// an unrelated group; syscall.Kill takes the pgid negated, golang/go#53199);
// WaitDelay reaps a stuck main child after the grace; sweep SIGKILLs any group
// members that survived (orphaned grandchildren) when the run was cancelled.
func newGroupCmd(ctx context.Context, dir string, command string, args []string) (*exec.Cmd, func()) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var pgid int
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if id, err := syscall.Getpgid(cmd.Process.Pid); err == nil && id > 0 {
			pgid = id
			return syscall.Kill(-pgid, syscall.SIGTERM)
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	// If the main child ignores SIGTERM, Wait force-kills it after the grace.
	cmd.WaitDelay = groupKillGrace

	sweep := func() {
		if ctx.Err() != nil && pgid > 0 {
			// The run was cancelled: sweep group members that survived SIGTERM and
			// the main child's kill (orphaned grandchildren). A fully-dead group
			// makes this a harmless ESRCH.
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	return cmd, sweep
}
