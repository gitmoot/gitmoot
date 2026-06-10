package subprocess

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunGroupKillsGrandchildrenOnCancel proves that cancelling the context
// terminates not just the shell but the background grandchild it spawned —
// the failure mode plain exec.CommandContext leaves behind.
func TestRunGroupKillsGrandchildrenOnCancel(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// The shell spawns a long-lived grandchild, records its pid, then waits.
		_, err := RunGroup(ctx, "", "sh", "-c", "sleep 300 & echo $! > "+pidFile+"; wait")
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
		t.Fatal("RunGroup did not return after cancellation")
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

// TestRunGroupNormalCompletionUnaffected: group semantics must not change
// successful runs.
func TestRunGroupNormalCompletionUnaffected(t *testing.T) {
	result, err := RunGroup(context.Background(), "", "sh", "-c", "echo hello")
	if err != nil {
		t.Fatalf("RunGroup: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}
