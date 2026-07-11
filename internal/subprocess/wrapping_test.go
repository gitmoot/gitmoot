package subprocess

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type wrappingCaptureRunner struct {
	dir     string
	command string
	args    []string
	env     []string
}

func (r *wrappingCaptureRunner) Run(_ context.Context, dir, command string, args ...string) (Result, error) {
	r.dir, r.command, r.args = dir, command, append([]string(nil), args...)
	return Result{Command: command, Args: args}, nil
}

func (r *wrappingCaptureRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (Result, error) {
	r.env = append([]string(nil), env...)
	return r.Run(ctx, dir, command, args...)
}

func (r *wrappingCaptureRunner) LookPath(file string) (string, error) { return file, nil }

func TestWrappingRunnerRewritesArgvExactly(t *testing.T) {
	capture := &wrappingCaptureRunner{}
	runner := WrappingRunner{Inner: capture, Executable: "/opt/gitmoot", WritablePaths: []string{"/data/a", "/data/b"}}
	if _, err := runner.Run(context.Background(), "/work", "claude", "-p", "task"); err != nil {
		t.Fatal(err)
	}
	want := []string{"sandbox-exec", "--write", "/data/a", "--write", "/data/b", "--", "claude", "-p", "task"}
	if capture.dir != "/work" || capture.command != "/opt/gitmoot" || !reflect.DeepEqual(capture.args, want) {
		t.Fatalf("wrapped call = dir %q command %q args %v, want /work /opt/gitmoot %v", capture.dir, capture.command, capture.args, want)
	}
	if got, err := runner.LookPath("claude"); err != nil || got != "claude" {
		t.Fatalf("LookPath = %q, %v", got, err)
	}
}

func TestWrappingRunnerInjectsEnvWhileStreaming(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "shim.sh")
	if err := os.WriteFile(shim, []byte(`#!/bin/sh
test "$1" = sandbox-exec || exit 91
shift
while test "$1" != --; do shift 2; done
shift
exec "$@"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	var streamed bytes.Buffer
	runner := WrappingRunner{
		Inner:         GroupRunner{},
		Executable:    shim,
		WritablePaths: []string{dir},
		Env:           []string{"GITMOOT_WRAPPER_STATE=wrapped"},
	}
	result, err := runner.RunStream(context.Background(), dir, &streamed, "sh", "-c", `printf '%s' "$GITMOOT_WRAPPER_STATE"`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "wrapped" || streamed.String() != "wrapped\n" {
		t.Fatalf("stdout = %q, streamed = %q, want wrapped", result.Stdout, streamed.String())
	}
}

func TestWrappingRunnerStateEnvWinsCallerOverride(t *testing.T) {
	capture := &wrappingCaptureRunner{}
	runner := WrappingRunner{
		Inner:      capture,
		Executable: "/opt/gitmoot",
		Env:        []string{"CLAUDE_CONFIG_DIR=/home/.claude"},
	}
	_, err := runner.RunEnv(context.Background(), "/work", []string{
		"CLAUDE_CONFIG_DIR=/outside", "GITMOOT_TEST=1",
	}, "claude", "-p", "task")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CLAUDE_CONFIG_DIR=/outside", "GITMOOT_TEST=1", "CLAUDE_CONFIG_DIR=/home/.claude"}
	if !reflect.DeepEqual(capture.env, want) {
		t.Fatalf("wrapped env = %v, want %v", capture.env, want)
	}
}

func TestWrappingRunnerComposesWithGroupRunnerAndKillsChildTree(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "shim.sh")
	if err := os.WriteFile(shim, []byte(`#!/bin/sh
test "$1" = sandbox-exec || exit 91
shift
while test "$1" != --; do shift 2; done
shift
exec "$@"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (WrappingRunner{Inner: GroupRunner{}, Executable: shim, WritablePaths: []string{dir}}).Run(
			ctx, dir, "sh", "-c", "sleep 300 & echo $! > \"$1\"; wait", "gitmoot-test", pidFile)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	var childPID int
	for {
		if data, err := os.ReadFile(pidFile); err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("child pid was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled wrapped group returned nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wrapped GroupRunner did not return promptly after cancellation")
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("wrapped child %d survived group cancellation", childPID)
}
