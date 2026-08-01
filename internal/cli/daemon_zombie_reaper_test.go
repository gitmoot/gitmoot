package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDaemonZombieReaperReapsDetachedChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc liveness assertion is Linux-specific")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDaemonZombieReaper(ctx, io.Discard)

	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			state, ppid, session, _ := procProcessIdentity(pid)
			t.Fatalf("detached child pid %d still present: state=%q ppid=%d session=%d", pid, state, ppid, session)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDaemonRunWiresZombieReaper(t *testing.T) {
	source, err := os.ReadFile("daemon_lifecycle.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(source), "startDaemonZombieReaper(ctx, stdout)"); got != 1 {
		t.Fatalf("daemon run zombie-reaper wiring count = %d, want 1", got)
	}
}
