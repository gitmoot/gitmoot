package subprocess

import (
	"bytes"
	"context"
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

// StreamRunner additionally tees the child's stdout and stderr to a writer as
// they are produced, while still returning the buffered Result — for
// long-lived subprocesses whose progress should appear live (e.g. in a log a
// TUI tails) instead of only after exit.
type StreamRunner interface {
	Runner
	RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	return Run(ctx, dir, command, args...)
}

func (ExecRunner) RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error) {
	return RunStream(ctx, dir, out, command, args...)
}

func (ExecRunner) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

func (ExecRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (Result, error) {
	return RunEnv(ctx, dir, env, command, args...)
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

func (t TeeRunner) LookPath(file string) (string, error) {
	return t.inner().LookPath(file)
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

// runStreamingCmd wires line-teeing tee writers (sharing one SyncWriter so the
// two pipes interleave safely) plus the buffered captures onto cmd, runs it, and
// returns the same buffered Result Run/RunGroup produce. The cmd's run strategy
// (plain context-cancel vs process-group) is the caller's choice: RunStream
// passes a plain cmd; RunGroupStream wires the group cancel/sweep first. The
// returned Result is byte-identical to the non-streaming runners' Result, so the
// tee never changes result capture.
func runStreamingCmd(cmd *exec.Cmd, out io.Writer, command string, args []string) (Result, error) {
	tee := SyncWriter(out)
	outLines := &lineWriter{out: tee}
	errLines := &lineWriter{out: tee}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, outLines)
	cmd.Stderr = io.MultiWriter(&stderr, errLines)

	err := cmd.Run()
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
