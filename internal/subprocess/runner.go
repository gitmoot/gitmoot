package subprocess

import (
	"bytes"
	"context"
	"os/exec"
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

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, command string, args ...string) (Result, error) {
	return Run(ctx, dir, command, args...)
}

func (ExecRunner) LookPath(file string) (string, error) {
	return exec.LookPath(file)
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
