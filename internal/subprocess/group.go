package subprocess

import (
	"bytes"
	"context"
	"io"
	"os"
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
// (gh, git) keep the plain ExecRunner. A child that deliberately calls setsid
// escapes both this group and its cleanup; that residual is out of scope.
type GroupRunner struct {
	// MaxOutputBytes, when positive, retains only the tail of stdout and stderr
	// independently. Zero preserves the historical unbounded capture behavior.
	MaxOutputBytes int
	// Credential, when non-nil, is applied by the kernel before the command
	// starts. Start errors are returned unchanged; callers must never retry the
	// command without the credential.
	Credential *syscall.Credential
}

func (r GroupRunner) Run(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	return r.RunEnv(ctx, dir, nil, command, args...)
}

func (r GroupRunner) RunWithPID(ctx context.Context, dir string, onPID PIDCallback, command string, args ...string) (Result, error) {
	return r.RunEnvWithPID(ctx, dir, nil, onPID, command, args...)
}

// RunEnv gives GroupRunner the EnvRunner contract: process-group kill semantics
// PLUS extra KEY=VALUE env vars appended to the inherited environment, without
// losing whole-tree cancellation. A nil/empty env is byte-identical to Run.

func (r GroupRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (Result, error) {
	if r.MaxOutputBytes > 0 {
		return runGroupEnvBounded(ctx, dir, env, r.MaxOutputBytes, r.Credential, nil, command, args...)
	}
	return runGroupEnv(ctx, dir, env, r.Credential, nil, command, args...)
}

func (r GroupRunner) RunEnvWithPID(ctx context.Context, dir string, env []string, onPID PIDCallback, command string, args ...string) (Result, error) {
	if r.MaxOutputBytes > 0 {
		return runGroupEnvBounded(ctx, dir, env, r.MaxOutputBytes, r.Credential, onPID, command, args...)
	}
	return runGroupEnv(ctx, dir, env, r.Credential, onPID, command, args...)
}

// RunStream gives GroupRunner the StreamRunner contract: process-group kill
// semantics PLUS a live line-tee of the child's stdout/stderr to out. It is what
// TeeRunner{} defaults to, so teeing a runtime adapter's output into a per-job
// log keeps the same whole-group cancellation the adapters rely on. A nil out
// degrades to Run.
func (r GroupRunner) RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error) {
	return r.RunEnvStream(ctx, dir, nil, out, command, args...)
}

func (r GroupRunner) RunStreamWithPID(ctx context.Context, dir string, out io.Writer, onPID PIDCallback, command string, args ...string) (Result, error) {
	return r.RunEnvStreamWithPID(ctx, dir, nil, out, onPID, command, args...)
}

func (r GroupRunner) RunEnvStream(ctx context.Context, dir string, env []string, out io.Writer, command string, args ...string) (Result, error) {
	return runGroupEnvStream(ctx, dir, env, out, r.Credential, nil, command, args...)
}

func (r GroupRunner) RunEnvStreamWithPID(ctx context.Context, dir string, env []string, out io.Writer, onPID PIDCallback, command string, args ...string) (Result, error) {
	return runGroupEnvStream(ctx, dir, env, out, r.Credential, onPID, command, args...)
}

func (GroupRunner) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

func runGroupEnv(ctx context.Context, dir string, extraEnv []string, credential *syscall.Credential, onPID PIDCallback, command string, args ...string) (Result, error) {
	cmd, sweep := newGroupCmd(ctx, dir, command, args, credential)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	var err error
	if onPID != nil {
		err = startAndWait(cmd, onPID)
	} else {
		err = cmd.Run()
	}
	sweep()
	return Result{
		Command: command,
		Args:    args,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}, err
}

// runGroupEnvBounded is runGroupEnv with bounded tail capture for stdout and
// stderr. It preserves the same process-group cancellation, WaitDelay, and final
// SIGKILL sweep while preventing a noisy child from growing memory without bound.
func runGroupEnvBounded(ctx context.Context, dir string, extraEnv []string, maxOutputBytes int, credential *syscall.Credential, onPID PIDCallback, command string, args ...string) (Result, error) {
	cmd, sweep := newGroupCmd(ctx, dir, command, args, credential)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	stdout := tailBuffer{max: maxOutputBytes}
	stderr := tailBuffer{max: maxOutputBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	var err error
	if onPID != nil {
		err = startAndWait(cmd, onPID)
	} else {
		err = cmd.Run()
	}
	sweep()
	return Result{Command: command, Args: args, Stdout: stdout.String(), Stderr: stderr.String()}, err
}

type tailBuffer struct {
	max  int
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.max <= 0 {
		return n, nil
	}
	b.data = append(b.data, p...)
	if len(b.data) > b.max {
		b.data = append(b.data[:0], b.data[len(b.data)-b.max:]...)
	}
	return n, nil
}

func (b *tailBuffer) String() string { return string(b.data) }

func runGroupEnvStream(ctx context.Context, dir string, extraEnv []string, out io.Writer, credential *syscall.Credential, onPID PIDCallback, command string, args ...string) (Result, error) {
	if out == nil {
		return runGroupEnv(ctx, dir, extraEnv, credential, onPID, command, args...)
	}
	cmd, sweep := newGroupCmd(ctx, dir, command, args, credential)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var result Result
	var err error
	if onPID != nil {
		result, err = runStreamingCmdWithPID(cmd, out, onPID, command, args)
	} else {
		result, err = runStreamingCmd(cmd, out, command, args)
	}
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
func newGroupCmd(ctx context.Context, dir string, command string, args []string, credential *syscall.Credential) (*exec.Cmd, func()) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: credential}

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
