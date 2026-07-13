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

func TestCuratedGroupRunnerBaseThenExtraLastWins(t *testing.T) {
	t.Setenv("AMBIENT_SECRET", "ambient")
	runner := CuratedGroupRunner{BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "ORDER=base"}}
	result, err := runner.RunEnv(context.Background(), "", []string{"ORDER=extra"}, "sh", "-c", `printf '%s|%s' "$ORDER" "${AMBIENT_SECRET-unset}"`)
	if err != nil {
		t.Fatalf("RunEnv: %v", err)
	}
	if result.Stdout != "extra|unset" {
		t.Fatalf("stdout = %q, want extra|unset", result.Stdout)
	}
}

func TestCuratedGroupRunnerRunVariantsAndLookPath(t *testing.T) {
	runner := CuratedGroupRunner{BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "BASE=yes"}}
	if _, err := runner.LookPath("sh"); err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	result, err := runner.Run(context.Background(), "", "sh", "-c", `printf '%s' "$BASE"`)
	if err != nil || result.Stdout != "yes" {
		t.Fatalf("Run result=%+v err=%v", result, err)
	}
	var streamed bytes.Buffer
	result, err = runner.RunStream(context.Background(), "", &streamed, "sh", "-c", `printf 'stream\n'`)
	if err != nil || result.Stdout != "stream\n" || streamed.String() != "stream\n" {
		t.Fatalf("RunStream result=%+v stream=%q err=%v", result, streamed.String(), err)
	}
	streamed.Reset()
	result, err = runner.RunEnvStream(context.Background(), "", []string{"EXTRA=ok"}, &streamed, "sh", "-c", `printf '%s\n' "$EXTRA"`)
	if err != nil || result.Stdout != "ok\n" || streamed.String() != "ok\n" {
		t.Fatalf("RunEnvStream result=%+v stream=%q err=%v", result, streamed.String(), err)
	}
	bounded := CuratedGroupRunner{BaseEnv: []string{"PATH=" + os.Getenv("PATH")}, MaxOutputBytes: 4}
	result, err = bounded.Run(context.Background(), "", "sh", "-c", `printf '123456'`)
	if err != nil || result.Stdout != "3456" {
		t.Fatalf("bounded Run result=%+v err=%v", result, err)
	}
}

func TestCuratedGroupRunnerScratchLifecycle(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, "gh")
	runner := CuratedGroupRunner{
		BaseEnv:     []string{"PATH=" + os.Getenv("PATH"), "GH_CONFIG_DIR=" + scratch},
		ScratchDirs: []string{scratch},
	}
	result, err := runner.Run(context.Background(), "", "sh", "-c", `test -d "$GH_CONFIG_DIR" && test -z "$(find "$GH_CONFIG_DIR" -mindepth 1 -print -quit)" && stat -c %a "$GH_CONFIG_DIR"`)
	if err != nil {
		t.Fatalf("Run: %v (%s)", err, result.Stderr)
	}
	if strings.TrimSpace(result.Stdout) != "700" {
		t.Fatalf("scratch mode = %q", result.Stdout)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch remains after success: %v", err)
	}
	_, _ = runner.Run(context.Background(), "", "sh", "-c", "exit 9")
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch remains after failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = runner.Run(ctx, "", "sh", "-c", "sleep 30")
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch remains after cancel: %v", err)
	}
}

func TestCuratedGroupRunnerKillsGrandchildrenOnCancel(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := CuratedGroupRunner{BaseEnv: []string{"PATH=" + os.Getenv("PATH")}}
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, "", "sh", "-c", "sleep 300 & echo $! > "+pidFile+"; wait")
		done <- err
	}()
	var grandchild int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			grandchild, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if grandchild > 0 {
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
		t.Fatal("curated runner did not return after cancellation")
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(grandchild, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d survived cancellation", grandchild)
}
