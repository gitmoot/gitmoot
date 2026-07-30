package subprocess

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
)

type Result struct {
	Command string
	Args    []string
	Stdout  string
	Stderr  string
}

type Runner interface {
	Run(ctx context.Context, dir string, command string, args ...string) (Result, error)
	LookPath(file string) (string, error)
}

// EnvRunner is an optional Runner capability: it runs a command with extra
// environment variables (KEY=VALUE entries) appended to the inherited
// environment. It is opt-in so callers that only need the env override (e.g. the
// doctor probing a specific Claude credential) can type-assert for it and fall
// back to a plain Run when the runner does not implement it — fakes that key on
// command+args therefore need no change.
type EnvRunner interface {
	Runner
	RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (Result, error)
}

// PIDCallback is invoked after a subprocess has started successfully and before
// its runner blocks waiting for completion.
type PIDCallback func(pid int)

// PIDRunner is an optional Runner capability for callers that need the exact
// process started by the runner. Callers must fall back to Runner.Run when the
// capability is absent so existing fakes and custom runners remain unchanged.
type PIDRunner interface {
	Runner
	RunWithPID(ctx context.Context, dir string, onPID PIDCallback, command string, args ...string) (Result, error)
}

// EnvPIDRunner combines optional environment injection and PID capture.
type EnvPIDRunner interface {
	EnvRunner
	RunEnvWithPID(ctx context.Context, dir string, env []string, onPID PIDCallback, command string, args ...string) (Result, error)
}

// StreamRunner additionally tees the child's stdout and stderr to a writer as
// they are produced, while still returning the buffered Result — for
// long-lived subprocesses whose progress should appear live (e.g. in a log a
// TUI tails) instead of only after exit.
type StreamRunner interface {
	Runner
	RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error)
}

// EnvStreamRunner combines the optional environment and streaming capabilities.
// Wrappers that must inject environment without dropping live output use this
// seam when their inner runner provides it.
type EnvStreamRunner interface {
	StreamRunner
	RunEnvStream(ctx context.Context, dir string, env []string, out io.Writer, command string, args ...string) (Result, error)
}

// PIDStreamRunner combines optional streaming and PID capture.
type PIDStreamRunner interface {
	StreamRunner
	RunStreamWithPID(ctx context.Context, dir string, out io.Writer, onPID PIDCallback, command string, args ...string) (Result, error)
}

// EnvPIDStreamRunner combines environment injection, streaming, and PID capture.
type EnvPIDStreamRunner interface {
	EnvStreamRunner
	RunEnvStreamWithPID(ctx context.Context, dir string, env []string, out io.Writer, onPID PIDCallback, command string, args ...string) (Result, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	return Run(ctx, dir, command, args...)
}

func (ExecRunner) RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error) {
	return RunStream(ctx, dir, out, command, args...)
}

func (ExecRunner) RunWithPID(ctx context.Context, dir string, onPID PIDCallback, command string, args ...string) (Result, error) {
	return RunEnvWithPID(ctx, dir, nil, onPID, command, args...)
}

func (ExecRunner) RunStreamWithPID(ctx context.Context, dir string, out io.Writer, onPID PIDCallback, command string, args ...string) (Result, error) {
	return RunStreamWithPID(ctx, dir, out, onPID, command, args...)
}

func (ExecRunner) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

func (ExecRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (Result, error) {
	return RunEnv(ctx, dir, env, command, args...)
}

func (ExecRunner) RunEnvWithPID(ctx context.Context, dir string, env []string, onPID PIDCallback, command string, args ...string) (Result, error) {
	return RunEnvWithPID(ctx, dir, env, onPID, command, args...)
}

// TeeRunner adapts a stream-capable runner into a plain Runner that always tees
// the child's stdout/stderr live to Out. Adapters call only .Run(); wrapping
// the inner runner in a TeeRunner makes those same .Run() calls stream their
// output into Out (a per-job log a pane tails) with ZERO adapter change. Inner
// defaults to GroupRunner{}, so the process-group kill semantics the runtime
// adapters rely on are preserved. A nil Out degrades to the inner's plain Run,
// and the buffered Result returned is exactly the one RunStream produces — so
// result capture, locks, and signals are unchanged. The tee is purely additive.
type TeeRunner struct {
	Inner StreamRunner
	Out   io.Writer
}

func (t TeeRunner) inner() StreamRunner {
	if t.Inner != nil {
		return t.Inner
	}
	return GroupRunner{}
}

func (t TeeRunner) Run(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	return t.inner().RunStream(ctx, dir, t.Out, command, args...)
}

func (t TeeRunner) RunWithPID(ctx context.Context, dir string, onPID PIDCallback, command string, args ...string) (Result, error) {
	inner := t.inner()
	if pidStream, ok := inner.(PIDStreamRunner); ok {
		return pidStream.RunStreamWithPID(ctx, dir, t.Out, onPID, command, args...)
	}
	if t.Out == nil {
		if pidRunner, ok := inner.(PIDRunner); ok {
			return pidRunner.RunWithPID(ctx, dir, onPID, command, args...)
		}
	}
	return Result{}, errors.New("tee runner inner does not support PID streaming")
}

// RunEnv preserves TeeRunner's live-output semantics while forwarding exact
// extra environment entries to an env+stream-capable inner runner. The default
// GroupRunner implements that combined seam.
func (t TeeRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (Result, error) {
	inner := t.inner()
	if envStream, ok := inner.(EnvStreamRunner); ok {
		return envStream.RunEnvStream(ctx, dir, env, t.Out, command, args...)
	}
	if t.Out == nil {
		if envRunner, ok := inner.(EnvRunner); ok {
			return envRunner.RunEnv(ctx, dir, env, command, args...)
		}
	}
	return Result{}, errors.New("tee runner inner does not support environment streaming")
}

func (t TeeRunner) RunEnvWithPID(ctx context.Context, dir string, env []string, onPID PIDCallback, command string, args ...string) (Result, error) {
	inner := t.inner()
	if envPIDStream, ok := inner.(EnvPIDStreamRunner); ok {
		return envPIDStream.RunEnvStreamWithPID(ctx, dir, env, t.Out, onPID, command, args...)
	}
	if t.Out == nil {
		if envPIDRunner, ok := inner.(EnvPIDRunner); ok {
			return envPIDRunner.RunEnvWithPID(ctx, dir, env, onPID, command, args...)
		}
	}
	return Result{}, errors.New("tee runner inner does not support environment PID streaming")
}

func (t TeeRunner) LookPath(file string) (string, error) {
	return t.inner().LookPath(file)
}

// EnvInjectingRunner wraps a runner to always append Env (KEY=VALUE entries)
// to the runtime subprocess environment while preserving process-group kill. The
// #732 daemon dispatches a moot seat's runtime adapter with one of these so the
// seat's GITMOOT_CHAT_RELAY[_AUTH] reaches its `gitmoot chat send/wait` subprocess
// - ONLY for moot seats. Inner defaults to GroupRunner, preserving the historical
// behavior when credential curation is off. Env is applied on every Run/RunEnv call.
type EnvInjectingRunner struct {
	Inner Runner
	Env   []string
}

func (r EnvInjectingRunner) inner() Runner {
	if r.Inner != nil {
		return r.Inner
	}
	return GroupRunner{}
}

func (r EnvInjectingRunner) Run(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	if inner, ok := r.inner().(EnvRunner); ok {
		return inner.RunEnv(ctx, dir, r.Env, command, args...)
	}
	if len(r.Env) == 0 {
		return r.inner().Run(ctx, dir, command, args...)
	}
	return Result{}, errors.New("environment-injecting runner inner does not support environment injection")
}

func (r EnvInjectingRunner) RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error) {
	if inner, ok := r.inner().(EnvStreamRunner); ok {
		return inner.RunEnvStream(ctx, dir, r.Env, out, command, args...)
	}
	if len(r.Env) == 0 {
		if inner, ok := r.inner().(StreamRunner); ok {
			return inner.RunStream(ctx, dir, out, command, args...)
		}
	}
	return Result{}, errors.New("environment-injecting runner inner does not support environment streaming")
}

func (r EnvInjectingRunner) RunWithPID(ctx context.Context, dir string, onPID PIDCallback, command string, args ...string) (Result, error) {
	if inner, ok := r.inner().(EnvPIDRunner); ok {
		return inner.RunEnvWithPID(ctx, dir, r.Env, onPID, command, args...)
	}
	if len(r.Env) == 0 {
		if inner, ok := r.inner().(PIDRunner); ok {
			return inner.RunWithPID(ctx, dir, onPID, command, args...)
		}
	}
	return Result{}, errors.New("environment-injecting runner inner does not support PID capture")
}

func (r EnvInjectingRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (Result, error) {
	merged := append(append([]string{}, r.Env...), env...)
	if inner, ok := r.inner().(EnvRunner); ok {
		return inner.RunEnv(ctx, dir, merged, command, args...)
	}
	if len(merged) == 0 {
		return r.inner().Run(ctx, dir, command, args...)
	}
	return Result{}, errors.New("environment-injecting runner inner does not support environment injection")
}

func (r EnvInjectingRunner) RunEnvStream(ctx context.Context, dir string, env []string, out io.Writer, command string, args ...string) (Result, error) {
	merged := append(append([]string{}, r.Env...), env...)
	if inner, ok := r.inner().(EnvStreamRunner); ok {
		return inner.RunEnvStream(ctx, dir, merged, out, command, args...)
	}
	if len(merged) == 0 {
		if inner, ok := r.inner().(StreamRunner); ok {
			return inner.RunStream(ctx, dir, out, command, args...)
		}
	}
	return Result{}, errors.New("environment-injecting runner inner does not support environment streaming")
}

func (r EnvInjectingRunner) RunEnvWithPID(ctx context.Context, dir string, env []string, onPID PIDCallback, command string, args ...string) (Result, error) {
	merged := append(append([]string{}, r.Env...), env...)
	if inner, ok := r.inner().(EnvPIDRunner); ok {
		return inner.RunEnvWithPID(ctx, dir, merged, onPID, command, args...)
	}
	if len(merged) == 0 {
		if inner, ok := r.inner().(PIDRunner); ok {
			return inner.RunWithPID(ctx, dir, onPID, command, args...)
		}
	}
	return Result{}, errors.New("environment-injecting runner inner does not support environment PID capture")
}

func (r EnvInjectingRunner) RunEnvStreamWithPID(ctx context.Context, dir string, env []string, out io.Writer, onPID PIDCallback, command string, args ...string) (Result, error) {
	merged := append(append([]string{}, r.Env...), env...)
	if inner, ok := r.inner().(EnvPIDStreamRunner); ok {
		return inner.RunEnvStreamWithPID(ctx, dir, merged, out, onPID, command, args...)
	}
	if len(merged) == 0 {
		if inner, ok := r.inner().(PIDStreamRunner); ok {
			return inner.RunStreamWithPID(ctx, dir, out, onPID, command, args...)
		}
	}
	return Result{}, errors.New("environment-injecting runner inner does not support environment PID streaming")
}

func (r EnvInjectingRunner) LookPath(file string) (string, error) {
	return r.inner().LookPath(file)
}

// SyncWriter serializes writes to w. Stream tees and any sibling writers
// (e.g. a heartbeat ticker) sharing one destination should share one
// SyncWriter, since destinations like bytes.Buffer are not safe for
// concurrent writes.
func SyncWriter(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	if _, ok := w.(*syncWriter); ok {
		return w
	}
	return &syncWriter{w: w}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// lineWriter buffers partial writes and forwards only complete lines, so two
// pipes teeing into one destination interleave at line boundaries instead of
// arbitrary io.Copy chunk boundaries.
type lineWriter struct {
	out io.Writer
	buf bytes.Buffer
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf.Write(p)
	for {
		line, err := l.buf.ReadString('\n')
		if err != nil {
			// Incomplete line: keep it buffered for the next write.
			l.buf.WriteString(line)
			return len(p), nil
		}
		if _, err := io.WriteString(l.out, line); err != nil {
			return len(p), err
		}
	}
}

func (l *lineWriter) flush() {
	if l.buf.Len() > 0 {
		_, _ = io.WriteString(l.out, l.buf.String()+"\n")
		l.buf.Reset()
	}
}

// RunStream runs like Run but additionally streams the child's stdout and
// stderr to out, line by line, as they are produced. A nil out degrades to
// Run. out is wrapped in a SyncWriter; callers writing to the same
// destination concurrently should pass the same SyncWriter-wrapped value.
func RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error) {
	if out == nil {
		return Run(ctx, dir, command, args...)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	return runStreamingCmd(cmd, out, command, args)
}

func RunStreamWithPID(ctx context.Context, dir string, out io.Writer, onPID PIDCallback, command string, args ...string) (Result, error) {
	if out == nil {
		return RunEnvWithPID(ctx, dir, nil, onPID, command, args...)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	return runStreamingCmdWithPID(cmd, out, onPID, command, args)
}

// runStreamingCmd wires line-teeing tee writers (sharing one SyncWriter so the
// two pipes interleave safely) plus the buffered captures onto cmd, runs it, and
// returns the same buffered Result Run/RunGroup produce. The cmd's run strategy
// (plain context-cancel vs process-group) is the caller's choice: RunStream
// passes a plain cmd; RunGroupStream wires the group cancel/sweep first. The
// returned Result is byte-identical to the non-streaming runners' Result, so the
// tee never changes result capture.
func runStreamingCmd(cmd *exec.Cmd, out io.Writer, command string, args []string) (Result, error) {
	return runStreamingCmdWithPID(cmd, out, nil, command, args)
}

func runStreamingCmdWithPID(cmd *exec.Cmd, out io.Writer, onPID PIDCallback, command string, args []string) (Result, error) {
	tee := SyncWriter(out)
	outLines := &lineWriter{out: tee}
	errLines := &lineWriter{out: tee}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, outLines)
	cmd.Stderr = io.MultiWriter(&stderr, errLines)

	err := startAndWait(cmd, onPID)
	outLines.flush()
	errLines.flush()
	return Result{
		Command: command,
		Args:    args,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}, err
}

func Run(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	return RunEnv(ctx, dir, nil, command, args...)
}

// RunEnv runs like Run but appends extraEnv (KEY=VALUE entries) to the inherited
// process environment, letting later entries override earlier ones per the os/exec
// last-wins rule. A nil extraEnv leaves the environment untouched (cmd.Env nil →
// inherit os.Environ), so RunEnv(ctx, dir, nil, …) is byte-identical to the prior
// Run.
func RunEnv(ctx context.Context, dir string, extraEnv []string, command string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return Result{
		Command: command,
		Args:    args,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}, err
}

// RunEnvWithPID is RunEnv with an additive callback invoked after a successful
// start and before waiting for the subprocess to finish.
func RunEnvWithPID(ctx context.Context, dir string, extraEnv []string, onPID PIDCallback, command string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := startAndWait(cmd, onPID)
	return Result{
		Command: command,
		Args:    args,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}, err
}

func startAndWait(cmd *exec.Cmd, onPID PIDCallback) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	if onPID != nil {
		onPID(cmd.Process.Pid)
	}
	return cmd.Wait()
}
