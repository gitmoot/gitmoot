package subprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// WrappingRunner launches commands through Gitmoot's hidden sandbox-exec shim.
// It is intended to wrap the innermost GroupRunner, beneath TeeRunner, so live
// output and process-group cancellation retain their existing behavior.
type WrappingRunner struct {
	Inner         Runner
	Executable    string
	ReadablePaths []string
	ReadableFiles []string
	WritablePaths []string
	// Env is appended to the sandbox-exec process environment and inherited by
	// the exec'd runtime. It is empty for every existing wrapper except Claude
	// produce, which relocates its mutable config into its granted state dir.
	Env []string
}

func (r WrappingRunner) inner() Runner {
	if r.Inner != nil {
		return r.Inner
	}
	return GroupRunner{}
}

func (r WrappingRunner) command(command string, args []string) (string, []string, error) {
	executable := r.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return "", nil, fmt.Errorf("resolve Gitmoot executable: %w", err)
		}
	}
	wrapped := []string{"sandbox-exec"}
	for _, path := range r.ReadablePaths {
		wrapped = append(wrapped, "--read", path)
	}
	for _, path := range r.ReadableFiles {
		wrapped = append(wrapped, "--read-file", path)
	}
	for _, path := range r.WritablePaths {
		wrapped = append(wrapped, "--write", path)
	}
	wrapped = append(wrapped, "--", command)
	wrapped = append(wrapped, args...)
	return executable, wrapped, nil
}

func (r WrappingRunner) Run(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	executable, wrapped, err := r.command(command, args)
	if err != nil {
		return Result{}, err
	}
	if len(r.Env) > 0 {
		if inner, ok := r.inner().(EnvRunner); ok {
			return inner.RunEnv(ctx, dir, r.Env, executable, wrapped...)
		}
		return Result{}, errors.New("wrapped runner does not support environment injection")
	}
	return r.inner().Run(ctx, dir, executable, wrapped...)
}

func (r WrappingRunner) RunWithPID(ctx context.Context, dir string, onPID PIDCallback, command string, args ...string) (Result, error) {
	executable, wrapped, err := r.command(command, args)
	if err != nil {
		return Result{}, err
	}
	if len(r.Env) > 0 {
		if inner, ok := r.inner().(EnvPIDRunner); ok {
			return inner.RunEnvWithPID(ctx, dir, r.Env, onPID, executable, wrapped...)
		}
		return r.Run(ctx, dir, command, args...)
	}
	if inner, ok := r.inner().(PIDRunner); ok {
		return inner.RunWithPID(ctx, dir, onPID, executable, wrapped...)
	}
	return r.inner().Run(ctx, dir, executable, wrapped...)
}

func (r WrappingRunner) RunStream(ctx context.Context, dir string, out io.Writer, command string, args ...string) (Result, error) {
	executable, wrapped, err := r.command(command, args)
	if err != nil {
		return Result{}, err
	}
	if len(r.Env) > 0 {
		if inner, ok := r.inner().(EnvStreamRunner); ok {
			return inner.RunEnvStream(ctx, dir, r.Env, out, executable, wrapped...)
		}
		return Result{}, errors.New("wrapped streaming runner does not support environment injection")
	}
	if inner, ok := r.inner().(StreamRunner); ok {
		return inner.RunStream(ctx, dir, out, executable, wrapped...)
	}
	return r.inner().Run(ctx, dir, executable, wrapped...)
}

func (r WrappingRunner) RunStreamWithPID(ctx context.Context, dir string, out io.Writer, onPID PIDCallback, command string, args ...string) (Result, error) {
	executable, wrapped, err := r.command(command, args)
	if err != nil {
		return Result{}, err
	}
	if len(r.Env) > 0 {
		if inner, ok := r.inner().(EnvPIDStreamRunner); ok {
			return inner.RunEnvStreamWithPID(ctx, dir, r.Env, out, onPID, executable, wrapped...)
		}
		return r.RunStream(ctx, dir, out, command, args...)
	}
	if inner, ok := r.inner().(PIDStreamRunner); ok {
		return inner.RunStreamWithPID(ctx, dir, out, onPID, executable, wrapped...)
	}
	if out == nil {
		if inner, ok := r.inner().(PIDRunner); ok {
			return inner.RunWithPID(ctx, dir, onPID, executable, wrapped...)
		}
	}
	return r.RunStream(ctx, dir, out, command, args...)
}

func (r WrappingRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (Result, error) {
	executable, wrapped, err := r.command(command, args)
	if err != nil {
		return Result{}, err
	}
	// Wrapper-owned values come last so a caller cannot move mutable runtime
	// state back outside the paths granted to sandbox-exec.
	merged := append(append([]string{}, env...), r.Env...)
	if inner, ok := r.inner().(EnvRunner); ok {
		return inner.RunEnv(ctx, dir, merged, executable, wrapped...)
	}
	if len(merged) != 0 {
		return Result{}, errors.New("wrapped runner does not support environment injection")
	}
	return r.inner().Run(ctx, dir, executable, wrapped...)
}

func (r WrappingRunner) RunEnvWithPID(ctx context.Context, dir string, env []string, onPID PIDCallback, command string, args ...string) (Result, error) {
	executable, wrapped, err := r.command(command, args)
	if err != nil {
		return Result{}, err
	}
	merged := append(append([]string{}, env...), r.Env...)
	if inner, ok := r.inner().(EnvPIDRunner); ok {
		return inner.RunEnvWithPID(ctx, dir, merged, onPID, executable, wrapped...)
	}
	if len(merged) == 0 {
		if inner, ok := r.inner().(PIDRunner); ok {
			return inner.RunWithPID(ctx, dir, onPID, executable, wrapped...)
		}
	}
	return r.RunEnv(ctx, dir, env, command, args...)
}

func (r WrappingRunner) LookPath(file string) (string, error) {
	return r.inner().LookPath(file)
}
