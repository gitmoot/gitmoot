package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Round-2 review of PR #2694. Written against helpers that existed at
// 7a662cb1, so each test also runs (and fails) on that commit.

// The workspace root itself is opened from its parent without following a
// symlink: a root another command replaced with an outside symlink is
// refused, never resolved and entered.
func TestWorkspaceSupervisorRefusesReplacedRoot(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	home := t.TempDir()
	outside := t.TempDir()
	root := filepath.Join(home, "work")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-c", workspaceSupervisorScript, workspaceSupervisorConfig(t, root, ".", []string{"pwd", "-P"}))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if code != workspaceCLIFailure || stdout.Len() != 0 || !strings.Contains(stderr.String(), `"code": "cwd_outside_root"`) {
		t.Fatalf("replaced root: exit %d, stdout %q, stderr %q (outside is %s)", code, stdout.String(), stderr.String(), outside)
	}
}

// local_root through a symlink: /proc reports physical working directories,
// so teardown must still find and stop the running command before close
// reports the workspace closed.
func TestWorkspaceLocalCloseStopsCommandUnderSymlinkedRoot(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{local: true})
	physical := t.TempDir()
	link := filepath.Join(t.TempDir(), "local-root")
	if err := os.Symlink(physical, link); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(env.paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), "\n[remote_exec]\n", fmt.Sprintf("\n[remote_exec]\nlocal_root = %q\n", link), 1)
	if updated == string(data) {
		t.Fatal("no [remote_exec] section to extend")
	}
	if err := os.WriteFile(env.paths.ConfigFile, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := env.open("--issue", "1")
	if !strings.HasPrefix(opened.Root, link+string(filepath.Separator)) {
		t.Fatalf("root %s is not under the symlinked local_root %s", opened.Root, link)
	}
	done := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		done <- runWorkspaceExec([]string{"--home", env.home, opened.ID, "--", "sh", "-c", "echo $$ > cmd.pid; exec sleep 120"}, nil, &stdout, &stderr)
	}()
	var pid int
	waitFor(t, "the command to start", 20*time.Second, func() bool { pid = readPIDFile(t, filepath.Join(opened.Root, "cmd.pid")); return pid > 0 })
	result := env.run("workspace", "close", opened.ID, "--json")
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the command outlived close %+v", result)
	}
	if result.code != 0 || !strings.Contains(result.stdout, `"state":"closed"`) {
		t.Fatalf("close = %+v", result)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the host exec did not end after close")
	}
}
