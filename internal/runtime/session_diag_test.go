package runtime

import (
	"context"
	"strings"
	"testing"
)

// shellDiagAgent builds a shell-runtime agent whose session command is the
// given script, run by a REAL process (nil Runner -> GroupRunner) so the exit
// code / signal decoding is exercised at the true runtime boundary.
func shellDiagAgent(script string) Agent {
	return Agent{Name: "diag", Role: "reviewer", Runtime: ShellRuntime, RuntimeRef: script, RepoScope: "jerryfane/gitmoot"}
}

func TestShellDeliverSessionDiagNonZeroExit(t *testing.T) {
	adapter := ShellAdapter{}
	agent := shellDiagAgent(`echo "boom on stderr" >&2; exit 5`)

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "hello"})

	if err == nil {
		t.Fatal("Deliver succeeded despite exit 5")
	}
	diag := result.SessionDiag
	if diag == nil {
		t.Fatal("SessionDiag = nil, want process evidence")
	}
	if diag.ExitCode == nil || *diag.ExitCode != 5 {
		t.Fatalf("ExitCode = %v, want 5", diag.ExitCode)
	}
	if diag.Signal != "" {
		t.Fatalf("Signal = %q, want empty for a plain exit", diag.Signal)
	}
	if diag.StdoutSeen {
		t.Fatal("StdoutSeen = true, want false (the script wrote stderr only)")
	}
	if !strings.Contains(diag.Stderr, "boom on stderr") {
		t.Fatalf("Stderr = %q, want the captured stderr", diag.Stderr)
	}
	if diag.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty for the shell runtime", diag.SessionID)
	}
}

func TestShellDeliverSessionDiagSignalTermination(t *testing.T) {
	adapter := ShellAdapter{}
	agent := shellDiagAgent(`kill -TERM $$`)

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "hello"})

	if err == nil {
		t.Fatal("Deliver succeeded despite the process killing itself")
	}
	diag := result.SessionDiag
	if diag == nil {
		t.Fatal("SessionDiag = nil, want process evidence")
	}
	if diag.ExitCode != nil {
		t.Fatalf("ExitCode = %d, want nil for a signal-terminated process", *diag.ExitCode)
	}
	if diag.Signal != "terminated" {
		t.Fatalf("Signal = %q, want %q", diag.Signal, "terminated")
	}
}

func TestShellDeliverSessionDiagSuccess(t *testing.T) {
	adapter := ShellAdapter{}
	agent := shellDiagAgent(`echo "all good"`)

	result, err := adapter.Deliver(context.Background(), agent, Job{Prompt: "hello"})

	if err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}
	diag := result.SessionDiag
	if diag == nil {
		t.Fatal("SessionDiag = nil, want exit-zero evidence on success too")
	}
	if diag.ExitCode == nil || *diag.ExitCode != 0 {
		t.Fatalf("ExitCode = %v, want 0", diag.ExitCode)
	}
	if !diag.StdoutSeen {
		t.Fatal("StdoutSeen = false, want true")
	}
}

func TestDecodeProcessExitNonExitError(t *testing.T) {
	code, signal := decodeProcessExit(context.Canceled)
	if code != nil || signal != "" {
		t.Fatalf("decodeProcessExit(context.Canceled) = (%v, %q), want (nil, \"\")", code, signal)
	}
}
