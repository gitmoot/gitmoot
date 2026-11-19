package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// Tests for the PR #2694 review: every ambiguous outcome keeps its
// reservation, and the VM-side watchdog and cwd confinement hold.

func workspaceCommittedUSD(t *testing.T, env *workspaceTestEnv) float64 {
	t.Helper()
	var committed float64
	if err := withStore(env.home, func(store *db.Store) error {
		var err error
		committed, err = store.WorkspaceCommittedUSD(context.Background())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return committed
}

// The reviewer's probe: the provider allocates, then answers 503, and its
// list does not show the sandbox. The reservation stays held, so a second
// open under max_total_usd = 1.1 at $1/h is refused.
func TestWorkspaceLostCreateAnswerKeepsReservation(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{rate: 1, maxTotal: 1.1, maxWorkspace: 1.1})
	env.cloud.mu.Lock()
	env.cloud.createLost, env.cloud.listLost = true, true
	env.cloud.mu.Unlock()
	first := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "1", "--json")
	if first.code != workspaceCLIFailure || workspaceErrorCode(t, first.stdout) != wsErrProviderUnavailable {
		t.Fatalf("first open = %+v", first)
	}
	var lost db.Workspace
	_ = withStore(env.home, func(store *db.Store) error {
		all, err := store.ListWorkspaces(context.Background(), true)
		if err == nil && len(all) == 1 {
			lost = all[0]
		}
		return err
	})
	if lost.State != db.WorkspaceStateClosing || lost.CostReservedUSD != 1 || lost.ProviderInstanceID != "" {
		t.Fatalf("after a lost create answer: %+v", lost)
	}
	created := env.cloud.createdCount()
	second := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "2", "--json")
	if second.code != workspaceCLIFailure || workspaceErrorCode(t, second.stdout) != wsErrCostCap {
		t.Fatalf("second open under the held reservation = %+v", second)
	}
	if env.cloud.createdCount() != created {
		t.Fatal("a refused open called create")
	}
	// Still held just before the provider's deadline guarantee...
	env.advance(time.Hour + workspaceExpiryGrace - time.Minute)
	if _, err := closeWorkspaceForTest(env, lost.ID); err == nil {
		t.Fatal("an unconfirmed close settled before the provider deadline")
	}
	if got := env.record(lost.ID); got.State != db.WorkspaceStateClosing {
		t.Fatalf("settled early: %+v", got)
	}
	// ...and settled, charging the whole reservation, once it has passed.
	env.advance(2 * time.Minute)
	env.sweep()
	got := env.record(lost.ID)
	if got.State != db.WorkspaceStateFailed || got.CloseReason != workspaceReasonExpired || got.CostAccruedUSD < got.CostReservedUSD-1e-9 {
		t.Fatalf("after the provider deadline: %+v", got)
	}
	if committed := workspaceCommittedUSD(t, env); committed < 1-1e-9 {
		t.Fatalf("committed spend %v under-counts the lost sandbox", committed)
	}
}

func closeWorkspaceForTest(env *workspaceTestEnv, id string) (string, error) {
	var out bytes.Buffer
	err := withStoreAndPaths(env.home, func(paths config.Paths, store *db.Store) error {
		return closeWorkspace(context.Background(), store, paths, id, "manual", &out)
	})
	return out.String(), err
}

// A lost create answer whose sandbox the provider's list does show is found
// by its workspace metadata and torn down, which releases the reservation.
func TestWorkspaceLostCreateFoundByMetadataIsTornDown(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{rate: 1, maxTotal: 1.1, maxWorkspace: 1.1})
	env.cloud.mu.Lock()
	env.cloud.createLost = true
	env.cloud.mu.Unlock()
	if first := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "1", "--json"); first.code != workspaceCLIFailure {
		t.Fatalf("first open = %+v", first)
	}
	var lost db.Workspace
	_ = withStore(env.home, func(store *db.Store) error {
		all, _ := store.ListWorkspaces(context.Background(), true)
		lost = all[0]
		return nil
	})
	if lost.State != db.WorkspaceStateFailed || lost.ProviderInstanceID == "" || !env.cloud.sandbox(lost.ProviderInstanceID).deleted {
		t.Fatalf("lost sandbox was not found and destroyed: %+v", lost)
	}
	env.cloud.mu.Lock()
	env.cloud.createLost = false
	env.cloud.mu.Unlock()
	env.open("--issue", "2")
}

// E2B answers 404 for an expired sandbox, which the shared client keeps
// inconclusive. The workspace stays closing and reserved until its funded
// deadline plus the provider grace has passed, then settles as expired.
func TestWorkspaceExpiredSandboxSettlesAfterProviderDeadline(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")
	sandboxID := env.record(opened.ID).ProviderInstanceID
	env.cloud.expire(sandboxID)
	result := env.run("workspace", "close", opened.ID, "--json")
	if result.code != workspaceCLIFailure || workspaceErrorCode(t, result.stdout) != wsErrProviderUnavailable {
		t.Fatalf("close of an unconfirmable sandbox = %+v", result)
	}
	if got := env.record(opened.ID); got.State != db.WorkspaceStateClosing || got.CostReservedUSD != 1 {
		t.Fatalf("before the deadline: %+v", got)
	}
	env.advance(time.Hour + workspaceExpiryGrace)
	env.sweep()
	got := env.record(opened.ID)
	if got.State != db.WorkspaceStateClosed || got.CloseReason != workspaceReasonExpired || got.CostAccruedUSD < got.CostReservedUSD-1e-9 {
		t.Fatalf("after the deadline: %+v", got)
	}
	if again := env.open("--issue", "1"); again.ID == opened.ID {
		t.Fatal("the subject was not freed")
	}
}

// Every provider deadline request lands inside the funded time: the create
// and each renewal ask for paid_until minus the margin, from the request.
func TestWorkspaceProviderDeadlineStaysInsideFundedTime(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")
	record := env.record(opened.ID)
	check := func(ttl int) {
		t.Helper()
		record = env.record(opened.ID)
		deadline := env.clock().Add(time.Duration(ttl) * time.Second)
		if deadline.After(record.PaidUntil.Add(-workspaceDeadlineMargin)) {
			t.Fatalf("provider deadline %s passes the funded %s minus the margin", deadline, record.PaidUntil)
		}
	}
	timeouts := env.cloud.sandbox(record.ProviderInstanceID).timeouts
	check(timeouts[0])
	env.advance(52 * time.Minute)
	env.sweep()
	timeouts = env.cloud.sandbox(record.ProviderInstanceID).timeouts
	if len(timeouts) != 2 {
		t.Fatalf("timeouts = %v", timeouts)
	}
	check(timeouts[1])
}

func workspaceSupervisorConfig(t *testing.T, root, cwd string, argv []string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"root": root, "cwd": cwd, "env": map[string]string{"PATH": "/usr/bin:/bin"}, "argv": argv,
		"timeout": 0, "grace": 60, "kill_grace": 0.2, "stdin": false, "log": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// A signal to the supervisor itself (on the old code, the host's fallback
// SIGTERM when the frame queue was full) must kill the command's group
// rather than leave it running without its watchdog.
func TestWorkspaceSupervisorSignalKillsCommandGroup(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	root := t.TempDir()
	cmd := exec.Command("python3", "-c", workspaceSupervisorScript,
		workspaceSupervisorConfig(t, root, ".", []string{"sh", "-c", "echo $$ > cmd.pid; exec sleep 300"}))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	waitFor(t, "the command to start", 10*time.Second, func() bool { pid = readPIDFile(t, filepath.Join(root, "cmd.pid")); return pid > 0 })
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the supervisor did not exit")
	}
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("the command outlived its signalled supervisor")
	}
	if !strings.Contains(stderr.String(), `"code": "supervisor_signal"`) {
		t.Fatalf("supervisor trailer = %q", stderr.String())
	}
}

// The supervisor launches from the directory it validated: replacing that
// directory with an outside symlink between the check and the launch does
// not move the command outside the root.
func TestWorkspaceSupervisorPinsValidatedCwd(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Swap the directory for a symlink right before the supervisor launches.
	wrapper := fmt.Sprintf(`import os, subprocess, sys
_real = subprocess.Popen
class Swapped(_real):
    def __init__(self, *args, **kwargs):
        os.rename(%[1]q + "/d", %[1]q + "/moved")
        os.symlink(%[2]q, %[1]q + "/d")
        super().__init__(*args, **kwargs)
subprocess.Popen = Swapped
sys.argv = ["-c", sys.argv[1]]
exec(compile(%[3]q, "supervisor", "exec"), {"__name__": "__main__"})
`, root, outside, workspaceSupervisorScript)
	cmd := exec.Command("python3", "-c", wrapper, workspaceSupervisorConfig(t, root, "d", []string{"pwd", "-P"}))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	_ = cmd.Run()
	if got := strings.TrimSpace(stdout.String()); got != filepath.Join(root, "moved") {
		t.Fatalf("command ran in %q, want the pinned %q (outside is %q)", got, filepath.Join(root, "moved"), outside)
	}
}

// Closing a local workspace stops its running commands before it reports
// the instance destroyed.
func TestWorkspaceLocalCloseStopsRunningCommand(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{local: true})
	opened := env.open("--issue", "1")
	if opened.Provider != "local" {
		t.Fatalf("provider = %q", opened.Provider)
	}
	done := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		done <- runWorkspaceExec([]string{"--home", env.home, opened.ID, "--", "sh", "-c", "echo $$ > cmd.pid; exec sleep 120"}, nil, &stdout, &stderr)
	}()
	var pid int
	waitFor(t, "the command to start", 20*time.Second, func() bool { pid = readPIDFile(t, filepath.Join(opened.Root, "cmd.pid")); return pid > 0 })
	if result := env.run("workspace", "close", opened.ID, "--json"); result.code != 0 || !strings.Contains(result.stdout, `"state":"closed"`) {
		t.Fatalf("close = %+v", result)
	}
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("the command outlived the closed workspace")
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the host exec did not end after close")
	}
}
