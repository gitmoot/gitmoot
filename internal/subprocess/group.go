package subprocess

import (
	"bytes"
	"context"
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

func (GroupRunner) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

// RunGroup is Run with process-group semantics: the child gets its own pgid
// (Setpgid) so the daemon never signals itself, cancellation SIGTERMs the
// group, Go's WaitDelay reaps a stuck main child after the grace period, and a
// final best-effort SIGKILL sweeps any group members that ignored SIGTERM.
func RunGroup(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// On ctx cancel, signal the whole group. The pgid is resolved HERE, while
	// the process is alive, and remembered for the final sweep — re-resolving
	// after the child is reaped could chase a reused pid into an unrelated
	// process group. syscall.Kill takes the pgid negated (golang/go#53199).
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

	err := cmd.Run()
	if ctx.Err() != nil && pgid > 0 {
		// The run was cancelled: sweep group members that survived SIGTERM and
		// the main child's kill (orphaned grandchildren). A fully-dead group
		// makes this a harmless ESRCH.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	return Result{
		Command: command,
		Args:    args,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}, err
}
