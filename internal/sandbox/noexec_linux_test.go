//go:build linux

package sandbox

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// noExecHarness runs `gitmoot sandbox-exec --no-execution` against a static Go
// probe (testdata/noexecprobe). The probe is the runtime: it is granted
// Execute and starts once; every later exec must be refused.
type noExecHarness struct {
	t       *testing.T
	gitmoot string
	probe   string
	readDir string
	write   string
}

func newNoExecHarness(t *testing.T) noExecHarness {
	t.Helper()
	requireLandlockABI(t)
	if err := NoExecutionSupported(); err != nil {
		t.Skipf("no-execution sandbox unsupported here: %v", err)
	}
	gitmoot := buildGitmootBinary(t)
	requireReadOnlySeatLaunchable(t, gitmoot)
	probeDir := t.TempDir()
	probe := filepath.Join(probeDir, "noexecprobe")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", probe, "./internal/sandbox/testdata/noexecprobe")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build no-exec probe: %v\n%s", err, output)
	}
	readDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(readDir, "input.txt"), []byte("readable-input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return noExecHarness{t: t, gitmoot: gitmoot, probe: probe, readDir: readDir, write: t.TempDir()}
}

func (h noExecHarness) command(runtimePath string, args ...string) *exec.Cmd {
	argv := []string{"sandbox-exec", "--no-execution", "--read", h.readDir, "--read", filepath.Dir(runtimePath), "--write", h.write}
	for _, system := range []string{"/usr", "/bin", "/lib", "/lib64", "/etc"} {
		if _, err := os.Stat(system); err == nil {
			argv = append(argv, "--read", system)
		}
	}
	argv = append(argv, "--", runtimePath)
	argv = append(argv, args...)
	cmd := exec.Command(h.gitmoot, argv...)
	cmd.Dir = h.readDir
	// Its own process group, as the daemon runs a seat, so the test can
	// kill the group the way the worker does.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A descendant left holding the output pipe must fail the test, not
	// hang it: Wait gives up on the pipe after this delay.
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func (h noExecHarness) run(args ...string) (string, int) {
	h.t.Helper()
	cmd := h.command(h.probe, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		h.t.Fatalf("run sandbox-exec: %v", err)
	}
	return strings.TrimSpace(out.String()), code
}

// TestNoExecutionSandboxDeniesEveryExecAfterBootstrapE2E is the #2364 denial
// contract on a real kernel: the runtime starts and can read its grants, and
// every later way of starting a program is refused, including re-executing
// the runtime binary itself (which keeps Execute) and execveat on a
// descriptor, which only the one-time supervisor can refuse.
func TestNoExecutionSandboxDeniesEveryExecAfterBootstrapE2E(t *testing.T) {
	h := newNoExecHarness(t)
	if out, code := h.run("read", filepath.Join(h.readDir, "input.txt")); code != 0 || out != "OK read readable-input" {
		t.Fatalf("bootstrap/read = %q (exit %d), want the runtime to start and read its grant", out, code)
	}
	denied := []struct {
		name string
		args []string
	}{
		{"shell", []string{"exec", "/bin/sh", "-c", "true"}},
		{"runtime re-exec", []string{"self"}},
		{"execveat on the runtime", []string{"execveat", h.probe}},
		{"execveat on a system binary", []string{"execveat", "/bin/true"}},
		{"written executable", []string{"written", h.write}},
	}
	for _, tc := range denied {
		out, code := h.run(tc.args...)
		if code != 3 || !strings.HasPrefix(out, "DENIED ") || !strings.Contains(out, "errno="+strconv.Itoa(int(syscall.EACCES))) {
			t.Errorf("%s: got %q (exit %d), want an EACCES denial", tc.name, out, code)
		}
	}
	for _, mode := range []string{"setsid", "setpgid"} {
		if out, code := h.run(mode); code != 3 || !strings.Contains(out, "errno="+strconv.Itoa(int(syscall.EPERM))) {
			t.Errorf("%s: got %q (exit %d), want EPERM so no descendant leaves the seat's process group", mode, out, code)
		}
	}
	if out, code := h.run("exitcode", "7"); code != 7 {
		t.Errorf("exit code = %d (%q), want the runtime's 7 passed through", code, out)
	}
}

// TestNoExecutionSandboxLoaderGrantIsNotAnExecPathE2E covers a DYNAMIC
// runtime, which needs Execute on its ELF loader to start at all. Running the
// loader explicitly with another program must still be refused.
func TestNoExecutionSandboxLoaderGrantIsNotAnExecPathE2E(t *testing.T) {
	h := newNoExecHarness(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 (a dynamic runtime) is not installed")
	}
	python, _ = filepath.EvalSymlinks(python)
	loader, err := elfInterpreter(python)
	if err != nil || loader == "" {
		t.Skipf("python3 is not a dynamic ELF here (%v)", err)
	}
	script := `import os,sys
try:
    os.execv(sys.argv[1], sys.argv[1:])
except OSError as e:
    print("DENIED errno=%d" % e.errno)
`
	cmd := h.command(python, "-c", script, loader, "/bin/true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dynamic runtime did not start: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "DENIED errno="+strconv.Itoa(int(syscall.EACCES)) {
		t.Fatalf("loader %s /bin/true = %q, want an EACCES denial", loader, got)
	}
}

// TestNoExecutionSandboxLeaksNoControlDescriptorE2E: the supervisor's
// notification socket must not be reachable from the runtime.
func TestNoExecutionSandboxLeaksNoControlDescriptorE2E(t *testing.T) {
	h := newNoExecHarness(t)
	out, code := h.run("fds")
	if code != 0 || !strings.HasPrefix(out, "OK fds") {
		t.Fatalf("fds = %q (exit %d)", out, code)
	}
	for _, entry := range strings.Fields(strings.TrimPrefix(out, "OK fds")) {
		if strings.Contains(entry, "socket:") || strings.Contains(entry, "anon_inode:seccomp") {
			t.Fatalf("runtime holds a supervisor control descriptor %q (all: %s)", entry, out)
		}
	}
}

// TestNoExecutionSandboxReapsDescendantsOnExitE2E: a descendant the runtime
// leaves behind does not outlive the seat.
func TestNoExecutionSandboxReapsDescendantsOnExitE2E(t *testing.T) {
	h := newNoExecHarness(t)
	pidFile := filepath.Join(h.write, "orphan.pid")
	if out, code := h.run("orphan", pidFile); code != 0 {
		t.Fatalf("orphan probe = %q (exit %d)", out, code)
	}
	pid := readPid(t, pidFile)
	if processAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("descendant %d outlived the runtime and supervisor", pid)
	}
}

// TestNoExecutionSandboxReapsMultiGenerationDescendantsE2E: the runtime
// leaves a live child A that has its own live child B. B reaches the
// supervisor only after A is killed, so cleanup must keep rescanning; a
// cleanup that blocks waiting after one generation hangs on B (Jarvis 362037).
func TestNoExecutionSandboxReapsMultiGenerationDescendantsE2E(t *testing.T) {
	h := newNoExecHarness(t)
	cmd := h.command(h.probe, "orphan-chain")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("orphan-chain run: %v (%s)", err, strings.TrimSpace(out.String()))
		}
	case <-time.After(3 * descendantReapBound):
		remaining := killGroup(pgid)
		<-done
		t.Fatalf("supervisor did not finish cleanup of a two-generation tree within %v (%d processes in the group)", 3*descendantReapBound, remaining)
	}
	if members := groupMembers(pgid); len(members) != 0 {
		killGroup(pgid)
		t.Fatalf("descendants %v outlived the runtime and supervisor", members)
	}
}

// TestNoExecutionSandboxCancellationFailsClosedE2E covers cancellation after
// bootstrap: SIGTERM is forwarded to the runtime, and a SIGKILLed supervisor
// takes the runtime with it, leaves no descendant outside the seat's process
// group, and a descendant that tries to exec after the supervisor is gone is
// still refused.
func TestNoExecutionSandboxCancellationFailsClosedE2E(t *testing.T) {
	h := newNoExecHarness(t)

	t.Run("sigterm is forwarded", func(t *testing.T) {
		pidFile, result := filepath.Join(h.write, "term.pid"), filepath.Join(h.write, "term.result")
		cmd := h.command(h.probe, "hold", pidFile, result)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		runtimePid := waitPid(t, pidFile)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		err := cmd.Wait()
		if processAlive(runtimePid) {
			t.Fatalf("runtime %d survived a forwarded SIGTERM", runtimePid)
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128+int(syscall.SIGTERM) {
			t.Fatalf("supervisor exit = %v, want %d (runtime killed by SIGTERM)", err, 128+int(syscall.SIGTERM))
		}
		killGroup(cmd.Process.Pid)
	})

	t.Run("supervisor sigkill", func(t *testing.T) {
		pidFile, result := filepath.Join(h.write, "kill.pid"), filepath.Join(h.write, "kill.result")
		cmd := h.command(h.probe, "hold", pidFile, result)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		runtimePid := waitPid(t, pidFile)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		deadline := time.Now().Add(5 * time.Second)
		for processAlive(runtimePid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if processAlive(runtimePid) {
			t.Fatalf("runtime %d survived its supervisor's SIGKILL", runtimePid)
		}
		// The descendant forked before the kill is still in the group and
		// tries to exec once the supervisor is gone: it must be refused.
		var got []byte
		for time.Now().Before(deadline.Add(5 * time.Second)) {
			if got, _ = os.ReadFile(result); len(got) > 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !strings.HasPrefix(string(got), "DENIED errno=") {
			t.Fatalf("exec after supervisor death = %q, want a denial", got)
		}
		if remaining := killGroup(cmd.Process.Pid); remaining != 0 {
			t.Fatalf("%d processes left in the seat's group after a group kill", remaining)
		}
	})
}

// TestNoExecutionSandboxCancellationBeforeBootstrapE2E kills the supervisor
// at a spread of instants around bootstrap. Whatever instant wins, nothing may
// survive the seat's group kill and no runtime may be left running.
func TestNoExecutionSandboxCancellationBeforeBootstrapE2E(t *testing.T) {
	h := newNoExecHarness(t)
	for i, delay := range []time.Duration{0, time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond} {
		pidFile, result := filepath.Join(h.write, "pre"+strconv.Itoa(i)+".pid"), filepath.Join(h.write, "pre"+strconv.Itoa(i)+".result")
		cmd := h.command(h.probe, "hold", pidFile, result)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(delay)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		time.Sleep(100 * time.Millisecond)
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && processAlive(pid) {
				t.Errorf("delay %v: runtime %d survived its supervisor", delay, pid)
			}
		}
		if remaining := killGroup(cmd.Process.Pid); remaining != 0 {
			t.Errorf("delay %v: %d processes left after the group kill", delay, remaining)
		}
		if got, _ := os.ReadFile(result); len(got) > 0 && !strings.HasPrefix(string(got), "DENIED") {
			t.Errorf("delay %v: a descendant exec was not refused: %q", delay, got)
		}
	}
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return readPid(t, path)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("runtime never wrote %s", path)
	return 0
}

// processAlive reports whether pid is a live (non-zombie) process.
func processAlive(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}

// killGroup SIGKILLs process group pgid, as the worker cancels a seat, and
// returns how many of its processes are still alive afterwards.
func killGroup(pgid int) int {
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	deadline := time.Now().Add(3 * time.Second)
	for {
		alive := len(groupMembers(pgid))
		if alive == 0 || time.Now().After(deadline) {
			return alive
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// groupMembers returns the live (non-zombie) processes in process group pgid.
func groupMembers(pgid int) []int {
	var alive []int
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if got, err := syscall.Getpgid(pid); err == nil && got == pgid && processAlive(pid) {
			alive = append(alive, pid)
		}
	}
	return alive
}
