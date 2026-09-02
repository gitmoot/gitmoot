package subprocess

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestGroupRunnerKillsGrandchildrenOnCancel proves that cancelling the context
// terminates not just the shell but the background grandchild it spawned —
// the failure mode plain exec.CommandContext leaves behind.
func TestGroupRunnerKillsGrandchildrenOnCancel(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// The shell spawns a long-lived grandchild, records its pid, then waits.
		_, err := GroupRunner{}.Run(ctx, "", "sh", "-c", "sleep 300 & echo $! > "+pidFile+"; wait")
		done <- err
	}()

	// Wait for the grandchild pid to appear.
	var grandchild int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				grandchild = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatal("grandchild pid never appeared")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("GroupRunner.Run did not return after cancellation")
	}

	// The grandchild must be gone (signal 0 probes existence). Allow a short
	// settling window for the SIGKILL sweep.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(grandchild, 0); err != nil {
			return // dead — success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d survived group cancellation", grandchild)
}

// TestGroupRunnerNormalCompletionUnaffected: group semantics must not change
// successful runs.
func TestGroupRunnerNormalCompletionUnaffected(t *testing.T) {
	result, err := GroupRunner{}.Run(context.Background(), "", "sh", "-c", "echo hello")
	if err != nil {
		t.Fatalf("GroupRunner.Run: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}

// TestGroupRunnerRunStreamTeesAndBuffers: the streaming group runner tees live
// to out while returning the same buffered Result as GroupRunner.Run — the tee
// is additive.
func TestGroupRunnerRunStreamTeesAndBuffers(t *testing.T) {
	var tee bytes.Buffer
	result, err := GroupRunner{}.RunStream(context.Background(), "", &tee, "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("GroupRunner.RunStream: %v", err)
	}
	if result.Stdout != "out\n" || result.Stderr != "err\n" {
		t.Fatalf("buffered result = %+v", result)
	}
	if got := tee.String(); !strings.Contains(got, "out\n") || !strings.Contains(got, "err\n") {
		t.Fatalf("tee missing streams: %q", got)
	}
}

// TestGroupRunnerRunStreamKillsGrandchildrenOnCancel: streaming must NOT weaken
// the whole-group kill — cancelling still terminates the background grandchild,
// the failure mode plain exec.CommandContext (and the non-group RunStream)
// leaves behind. This is the load-bearing guarantee for teeing runtime adapter
// output.
func TestGroupRunnerRunStreamKillsGrandchildrenOnCancel(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var tee bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := GroupRunner{}.RunStream(ctx, "", &tee, "sh", "-c", "sleep 300 & echo $! > "+pidFile+"; wait")
		done <- err
	}()

	var grandchild int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				grandchild = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatal("grandchild pid never appeared")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("GroupRunner.RunStream did not return after cancellation")
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(grandchild, 0); err != nil {
			return // dead — success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d survived group cancellation", grandchild)
}
