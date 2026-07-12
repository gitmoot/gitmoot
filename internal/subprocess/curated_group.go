package subprocess

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// CuratedGroupRunner is GroupRunner with an explicit base environment. Runtime
// wrappers pass their entries as extraEnv, which is appended after BaseEnv so
// wrapper-owned values keep the existing last-wins behavior.
type CuratedGroupRunner struct {
	BaseEnv        []string
	MaxOutputBytes int
	// ScratchDirs are recreated empty with mode 0700 for each subprocess and
	// removed after it exits, including error and cancellation paths.
	ScratchDirs []string
}

func (r CuratedGroupRunner) Run(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	return r.run(ctx, dir, nil, nil, command, args...)
}

func (r CuratedGroupRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (Result, error) {
	return r.run(ctx, dir, env, nil, command, args...)
}

func (r CuratedGroupRunner) RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error) {
	return r.run(ctx, dir, nil, out, command, args...)
}

func (r CuratedGroupRunner) RunEnvStream(ctx context.Context, dir string, env []string, out io.Writer, command string, args ...string) (Result, error) {
	return r.run(ctx, dir, env, out, command, args...)
}

func (r CuratedGroupRunner) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

func (r CuratedGroupRunner) run(ctx context.Context, dir string, extraEnv []string, out io.Writer, command string, args ...string) (Result, error) {
	if err := r.prepareScratch(); err != nil {
		return Result{Command: command, Args: args}, err
	}
	defer r.cleanupScratch()
	env := make([]string, 0, len(r.BaseEnv)+len(extraEnv))
	env = append(env, r.BaseEnv...)
	env = append(env, extraEnv...)
	if out != nil {
		return runCuratedGroupStream(ctx, dir, env, out, command, args...)
	}
	if r.MaxOutputBytes > 0 {
		return runCuratedGroupBounded(ctx, dir, env, r.MaxOutputBytes, command, args...)
	}
	return runCuratedGroup(ctx, dir, env, command, args...)
}

func (r CuratedGroupRunner) prepareScratch() error {
	for _, path := range r.ScratchDirs {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("reset curated runtime scratch: %w", err)
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create curated runtime scratch: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("chmod curated runtime scratch: %w", err)
		}
	}
	return nil
}

func (r CuratedGroupRunner) cleanupScratch() {
	for _, path := range r.ScratchDirs {
		_ = os.RemoveAll(path)
	}
}

func runCuratedGroup(ctx context.Context, dir string, env []string, command string, args ...string) (Result, error) {
	cmd, sweep := newGroupCmd(ctx, dir, command, args)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	sweep()
	return Result{Command: command, Args: args, Stdout: stdout.String(), Stderr: stderr.String()}, err
}

func runCuratedGroupBounded(ctx context.Context, dir string, env []string, maxOutputBytes int, command string, args ...string) (Result, error) {
	cmd, sweep := newGroupCmd(ctx, dir, command, args)
	cmd.Env = env
	stdout := tailBuffer{max: maxOutputBytes}
	stderr := tailBuffer{max: maxOutputBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	sweep()
	return Result{Command: command, Args: args, Stdout: stdout.String(), Stderr: stderr.String()}, err
}

func runCuratedGroupStream(ctx context.Context, dir string, env []string, out io.Writer, command string, args ...string) (Result, error) {
	if out == nil {
		return runCuratedGroup(ctx, dir, env, command, args...)
	}
	cmd, sweep := newGroupCmd(ctx, dir, command, args)
	cmd.Env = env
	result, err := runStreamingCmd(cmd, out, command, args)
	sweep()
	return result, err
}
