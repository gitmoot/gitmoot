package subprocess

import (
	"bytes"
	"context"
	"io"
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
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir

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
