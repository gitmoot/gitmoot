package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

func TestWorkspaceOpenShowListCloseRoundTrip(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "12")
	if opened.Seat != "intern" || opened.Repo != "acme/widget" || opened.Issue == nil || *opened.Issue != 12 || opened.PR != nil ||
		opened.Branch != "ws/intern/12" || opened.Base != env.baseSHA || opened.State != "ready" || opened.Provider != "e2b" ||
		opened.Root != filepath.Join(env.cloud.guestHome, "work") || opened.CreatedAt == "" || opened.LastActiveAt == "" {
		t.Fatalf("open = %+v", opened)
	}
	// The pinned keys, exactly: issue and pr are present even when null.
	raw := env.run("workspace", "show", opened.ID, "--json")
	if raw.code != 0 {
		t.Fatalf("show exited %d: %s", raw.code, raw.stderr)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw.stdout), &keys); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "seat", "repo", "issue", "pr", "branch", "base", "root", "provider", "state", "created_at", "last_active_at", "env"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("show --json lacks %q: %s", key, raw.stdout)
		}
	}
	if string(keys["pr"]) != "null" {
		t.Errorf("pr = %s, want null", keys["pr"])
	}
	var shown workspaceJSON
	_ = json.Unmarshal([]byte(raw.stdout), &shown)
	wantEnv := map[string]string{"PATH": workspaceBasePATH, "HOME": env.cloud.guestHome, "LANG": "C.UTF-8", "GITMOOT_WORKSPACE_ROOT": opened.Root}
	if len(shown.Env) != len(wantEnv) {
		t.Errorf("show env = %v, want %v", shown.Env, wantEnv)
	}
	for name, value := range wantEnv {
		if shown.Env[name] != value {
			t.Errorf("show env %s = %q, want %q", name, shown.Env[name], value)
		}
	}
	// The VM holds the branch at the base, tracking gitmoot-base, with no remote.
	git := exec.Command("git", "-C", opened.Root, "rev-parse", "--symbolic-full-name", "HEAD", "@{upstream}")
	output, err := git.CombinedOutput()
	head, _ := exec.Command("git", "-C", opened.Root, "rev-parse", "HEAD").Output()
	if err != nil || strings.Join(strings.Fields(string(output)), " ") != "refs/heads/ws/intern/12 refs/heads/gitmoot-base" || strings.TrimSpace(string(head)) != env.baseSHA {
		t.Fatalf("guest checkout = %q %q, %v", output, head, err)
	}
	if remotes, _ := exec.Command("git", "-C", opened.Root, "remote").Output(); len(bytes.TrimSpace(remotes)) != 0 {
		t.Fatalf("guest checkout has remotes %q", remotes)
	}
	sandboxID := env.record(opened.ID).ProviderInstanceID
	if got := env.cloud.sandbox(sandboxID).metadata["job_id"]; got != "workspace:"+opened.ID {
		t.Fatalf("sandbox metadata job_id = %q", got)
	}

	list := env.run("workspace", "ls", "--json")
	var listed []workspaceListJSON
	if err := json.Unmarshal([]byte(list.stdout), &listed); err != nil || len(listed) != 1 || listed[0].ID != opened.ID || listed[0].CostReservedUSD != 1 {
		t.Fatalf("ls = %s (%v)", list.stdout, err)
	}
	if text := env.run("workspace", "ls"); !strings.Contains(text.stdout, opened.ID) || !strings.Contains(text.stdout, "issue #12") {
		t.Fatalf("ls text = %q", text.stdout)
	}

	closed := env.run("workspace", "close", opened.ID, "--reason", "done for now")
	if closed.code != 0 {
		t.Fatalf("close exited %d: %s", closed.code, closed.stderr)
	}
	record := env.record(opened.ID)
	if record.State != db.WorkspaceStateClosed || record.CloseReason != "done for now" || record.ClosedAt.IsZero() || !env.cloud.sandbox(sandboxID).deleted {
		t.Fatalf("closed record = %+v, sandbox = %+v", record, env.cloud.sandbox(sandboxID))
	}
	if list := env.run("workspace", "ls", "--json"); strings.TrimSpace(list.stdout) != "[]" {
		t.Fatalf("ls after close = %s", list.stdout)
	}
	if all := env.run("workspace", "ls", "--all", "--json"); !strings.Contains(all.stdout, `"close_reason":"done for now"`) {
		t.Fatalf("ls --all = %s", all.stdout)
	}
	if missing := env.run("workspace", "show", "ws-nope", "--json"); missing.code != workspaceCLIFailure || workspaceErrorCode(t, missing.stdout) != wsErrNotFound || workspaceErrorCode(t, missing.stderr) != wsErrNotFound {
		t.Fatalf("show missing = %+v", missing)
	}
	t.Setenv("GITMOOT_WORKSPACE", "")
	if notReady := env.run("ws", "sh", "true"); notReady.code != workspaceCLIFailure {
		t.Fatalf("ws sh without GITMOOT_WORKSPACE = %+v", notReady)
	}
	t.Setenv("GITMOOT_WORKSPACE", opened.ID)
	if notReady := env.run("ws", "sh", "true"); notReady.code != workspaceCLIFailure || workspaceErrorCode(t, notReady.stderr) != wsErrNotReady {
		t.Fatalf("ws sh on a closed workspace = %+v", notReady)
	}
}

func TestWorkspaceOpenPullRequestChecksOutHeadBranch(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	head := env.addPullRequest(7, "feature/thing")
	opened := env.open("--pr", "7")
	if opened.Branch != "feature/thing" || opened.Base != head || opened.PR == nil || *opened.PR != 7 || opened.Issue != nil {
		t.Fatalf("open --pr = %+v", opened)
	}
	output, err := exec.Command("git", "-C", opened.Root, "log", "--format=%s", "@{upstream}..HEAD").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "feature" {
		t.Fatalf("commits over gitmoot-base = %q, %v", output, err)
	}
	env.setPull(github.PullRequest{Number: 8, State: "closed", HeadRef: "old", HeadSHA: head, BaseRef: "main"})
	if refused := env.run("workspace", "open", "--repo", "acme/widget", "--pr", "8", "--json"); refused.code != workspaceCLIFailure || workspaceErrorCode(t, refused.stdout) != wsErrInvalidArgs {
		t.Fatalf("open closed PR = %+v", refused)
	}
	if both := env.run("workspace", "open", "--repo", "acme/widget", "--pr", "7", "--issue", "1", "--json"); both.code != workspaceCLIFailure || workspaceErrorCode(t, both.stdout) != wsErrInvalidArgs {
		t.Fatalf("open with both = %+v", both)
	}
}

func TestWorkspaceOneOpenWorkspacePerSeatAndSubject(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	first := env.open("--issue", "3")
	again := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "3", "--json")
	if again.code != workspaceCLIFailure || workspaceErrorCode(t, again.stdout) != wsErrAlreadyOpen || !strings.Contains(again.stdout, first.ID) {
		t.Fatalf("second open = %+v", again)
	}
	if env.cloud.createdCount() != 1 {
		t.Fatalf("a refused open provisioned: %d sandboxes", env.cloud.createdCount())
	}
	// The store's unique index holds even when the pre-check is bypassed.
	err := withStore(env.home, func(store *db.Store) error {
		now := time.Now()
		return store.CreateWorkspace(context.Background(), db.Workspace{ID: "ws-dup", Seat: "intern", Repo: "acme/widget", Issue: 3, Provider: "e2b", Root: "/r", CreatedAt: now, LastActiveAt: now, PaidUntil: now}, 50)
	})
	var already *db.WorkspaceAlreadyOpenError
	if !errors.As(err, &already) || already.ID != first.ID {
		t.Fatalf("direct duplicate insert = %v", err)
	}
	t.Setenv("GITMOOT_ORG_ROLE", "other-seat")
	other := env.open("--issue", "3")
	t.Setenv("GITMOOT_ORG_ROLE", "intern")
	if other.ID == first.ID || other.Seat != "other-seat" || other.Branch != "ws/other-seat/3" {
		t.Fatalf("other seat's open = %+v", other)
	}
	if closed := env.run("workspace", "close", first.ID); closed.code != 0 {
		t.Fatalf("close = %+v", closed)
	}
	if reopened := env.open("--issue", "3"); reopened.ID == first.ID {
		t.Fatalf("reopen reused %s", first.ID)
	}
}

func TestWorkspaceExecStreamsOutputAndReturnsExitStatus(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")

	result := env.run("workspace", "exec", opened.ID, "--", "sh", "-c", "echo out; echo err >&2; exit 3")
	if result.code != 3 || result.stdout != "out\n" || result.stderr != "err\n" {
		t.Fatalf("exec = %+v", result)
	}
	signaled := env.run("workspace", "exec", opened.ID, "--", "sh", "-c", "kill -TERM $$")
	if signaled.code != 128+15 || signaled.stderr != "" {
		t.Fatalf("signaled exec = %+v", signaled)
	}
	if missing := env.run("workspace", "exec", opened.ID, "--", "no-such-command-gitmoot"); missing.code != 127 {
		t.Fatalf("missing command = %+v", missing)
	}

	// Live: the command blocks until the host has seen its first line.
	release := filepath.Join(opened.Root, "release")
	var stdout, stderr bytes.Buffer
	live := &workspaceTriggerWriter{out: &stdout, marker: "first", trigger: func() { _ = os.WriteFile(release, nil, 0o600) }}
	code := runWorkspaceExec([]string{"--home", env.home, opened.ID, "--timeout", "20", "--", "sh", "-c",
		`echo first; while [ ! -f release ]; do sleep 0.01; done; echo second`}, nil, live, &stderr)
	if code != 0 || stdout.String() != "first\nsecond\n" || !live.fired {
		t.Fatalf("live exec = %d %q %q", code, stdout.String(), stderr.String())
	}

	// No stdin unless --stdin.
	if quiet := env.run("workspace", "exec", opened.ID, "--", "cat"); quiet.code != 0 || quiet.stdout != "" {
		t.Fatalf("cat without --stdin = %+v", quiet)
	}
	fed := env.runWithStdin(strings.NewReader("hello\nworld\n"), "workspace", "exec", opened.ID, "--stdin", "--", "cat")
	if fed.code != 0 || fed.stdout != "hello\nworld\n" {
		t.Fatalf("cat with --stdin = %+v", fed)
	}
	// last_active_at moved with the exec.
	env.advance(time.Minute)
	before := env.record(opened.ID).LastActiveAt
	if again := env.run("workspace", "exec", opened.ID, "--", "true"); again.code != 0 {
		t.Fatalf("exec true = %+v", again)
	}
	if after := env.record(opened.ID).LastActiveAt; !after.After(before) {
		t.Fatalf("last_active_at %s did not move past %s", after, before)
	}
}

type workspaceTriggerWriter struct {
	mu      sync.Mutex
	out     *bytes.Buffer
	marker  string
	trigger func()
	fired   bool
}

func (w *workspaceTriggerWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.out.Write(data)
	if !w.fired && strings.Contains(w.out.String(), w.marker) {
		w.fired = true
		w.trigger()
	}
	return len(data), nil
}

func TestWorkspaceExecStatusFile(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")
	statusFile := filepath.Join(t.TempDir(), "status.json")
	read := func() map[string]any {
		t.Helper()
		data, err := os.ReadFile(statusFile)
		if err != nil {
			t.Fatal(err)
		}
		var status map[string]any
		if err := json.Unmarshal(data, &status); err != nil {
			t.Fatalf("status file %q: %v", data, err)
		}
		if len(status) != 4 {
			t.Fatalf("status file keys = %v", status)
		}
		return status
	}
	if result := env.run("workspace", "exec", opened.ID, "--status-file", statusFile, "--", "sh", "-c", "exit 125"); result.code != 125 {
		t.Fatalf("exit 125 = %+v", result)
	}
	if status := read(); status["exit_code"] != 125.0 || status["signal"] != nil || status["timed_out"] != false || status["error"] != nil {
		t.Fatalf("command exit 125 status = %v", status)
	}
	if result := env.run("workspace", "exec", opened.ID, "--status-file", statusFile, "--", "sh", "-c", "kill -KILL $$"); result.code != 137 {
		t.Fatalf("SIGKILL = %+v", result)
	}
	if status := read(); status["exit_code"] != 137.0 || status["signal"] != 9.0 || status["timed_out"] != false || status["error"] != nil {
		t.Fatalf("signal status = %v", status)
	}
	if result := env.run("workspace", "exec", opened.ID, "--timeout", "0.3", "--status-file", statusFile, "--", "sleep", "30"); result.code != 124 {
		t.Fatalf("timeout = %+v", result)
	}
	if status := read(); status["exit_code"] != 124.0 || status["signal"] != 15.0 || status["timed_out"] != true || status["error"] != nil {
		t.Fatalf("timeout status = %v", status)
	}
	if result := env.run("workspace", "exec", "ws-missing", "--status-file", statusFile, "--", "true"); result.code != workspaceCLIFailure || workspaceErrorCode(t, result.stderr) != wsErrNotFound {
		t.Fatalf("missing workspace = %+v", result)
	}
	status := read()
	gitmootErr, _ := status["error"].(map[string]any)
	if status["exit_code"] != 125.0 || gitmootErr["code"] != wsErrNotFound {
		t.Fatalf("gitmoot failure status = %v", status)
	}
}

func TestWorkspaceExecRefusesCwdOutsideRoot(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")
	if err := os.MkdirAll(filepath.Join(opened.Root, "sub", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if inside := env.run("workspace", "exec", opened.ID, "--cwd", "sub/dir/../dir", "--", "pwd"); inside.code != 0 || strings.TrimSpace(inside.stdout) != filepath.Join(opened.Root, "sub", "dir") {
		t.Fatalf("cwd inside = %+v", inside)
	}
	for _, cwd := range []string{"..", "../guest", "sub/../../x", "/etc"} {
		result := env.run("workspace", "exec", opened.ID, "--cwd", cwd, "--", "pwd")
		if result.code != workspaceCLIFailure || result.stdout != "" || workspaceErrorCode(t, result.stderr) != wsErrCwdOutsideRoot {
			t.Fatalf("cwd %q = %+v", cwd, result)
		}
	}
	// A symlink inside the root cannot lead out: the VM side resolves it.
	if err := os.Symlink("/", filepath.Join(opened.Root, "escape")); err != nil {
		t.Fatal(err)
	}
	result := env.run("workspace", "exec", opened.ID, "--cwd", "escape", "--", "pwd")
	if result.code != workspaceCLIFailure || result.stdout != "" || workspaceErrorCode(t, result.stderr) != wsErrCwdOutsideRoot {
		t.Fatalf("symlink cwd = %+v", result)
	}
}

func TestWorkspaceExecNeverPassesHostEnvironment(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")
	t.Setenv("GITMOOT_TEST_HOST_SECRET", "host-secret-value")
	t.Setenv("GH_TOKEN", "ghp_host_token")
	result := env.run("workspace", "exec", opened.ID, "--env", "EXTRA=1", "--env", "LANG=en_US.UTF-8", "--", "env")
	if result.code != 0 {
		t.Fatalf("env = %+v", result)
	}
	got := strings.Split(strings.TrimSpace(result.stdout), "\n")
	slices.Sort(got)
	want := []string{"EXTRA=1", "GITMOOT_WORKSPACE_ROOT=" + opened.Root, "HOME=" + env.cloud.guestHome, "LANG=en_US.UTF-8", "PATH=" + workspaceBasePATH}
	if !slices.Equal(got, want) {
		t.Fatalf("guest environment = %q, want %q", got, want)
	}
	for _, proc := range env.cloud.supervisors() {
		if len(proc.env) != 0 {
			t.Fatalf("envd Start carried environment %v", proc.env)
		}
		// One owner for the kill grace: the supervisor gets the host's value.
		if len(proc.args) < 3 || !strings.Contains(proc.args[2], fmt.Sprintf(`"kill_grace":%v`, workspaceKillGrace.Seconds())) {
			t.Fatalf("supervisor config lacks the host kill grace: %v", proc.args)
		}
		for _, arg := range proc.args {
			if strings.Contains(arg, "host-secret-value") || strings.Contains(arg, "ghp_host_token") {
				t.Fatalf("a host secret reached the VM in argv")
			}
		}
	}
	if bad := env.run("workspace", "exec", opened.ID, "--env", "NOEQUALS", "--", "true"); bad.code != workspaceCLIFailure || workspaceErrorCode(t, bad.stderr) != wsErrInvalidArgs {
		t.Fatalf("bad --env = %+v", bad)
	}
}

func TestWorkspaceWSShUsesWorkspaceFromEnvironment(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")
	if err := os.MkdirAll(filepath.Join(opened.Root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITMOOT_WORKSPACE", opened.ID)
	result := env.run("ws", "sh", "--cwd", "pkg", `pwd; shopt -q login_shell && echo login; echo "$GITMOOT_WORKSPACE_ROOT"; exit 4`)
	want := filepath.Join(opened.Root, "pkg") + "\nlogin\n" + opened.Root + "\n"
	if result.code != 4 || result.stdout != want {
		t.Fatalf("ws sh = %+v, want stdout %q", result, want)
	}
	if two := env.run("ws", "sh", "echo", "a"); two.code != workspaceCLIFailure || workspaceErrorCode(t, two.stderr) != wsErrInvalidArgs {
		t.Fatalf("ws sh with two arguments = %+v", two)
	}
	if escape := env.run("ws", "sh", "--cwd", "../..", "pwd"); escape.code != workspaceCLIFailure || workspaceErrorCode(t, escape.stderr) != wsErrCwdOutsideRoot {
		t.Fatalf("ws sh cwd escape = %+v", escape)
	}
	t.Setenv("GITMOOT_WORKSPACE", "ws-other")
	if other := env.run("ws", "sh", "true"); other.code != workspaceCLIFailure || workspaceErrorCode(t, other.stderr) != wsErrNotFound {
		t.Fatalf("ws sh for an unknown workspace = %+v", other)
	}
}

// startWorkspaceCLIChild runs `gitmoot ws sh` as a real process wired to the
// fake, so the test can signal or kill it.
func startWorkspaceCLIChild(t *testing.T, env *workspaceTestEnv, id, command string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "ws", "sh", "--home", env.home, command)
	cmd.Env = append(os.Environ(),
		workspaceCLIChildEnv+"=1",
		workspaceChildGuestHomeEnv+"="+env.cloud.guestHome,
		workspaceChildEnvdURLEnv+"="+env.cloud.envd.URL,
		"GITMOOT_WORKSPACE="+id,
	)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, &output
}

func TestWorkspaceExecHostKillEndsRemoteProcess(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")
	pidFile := filepath.Join(opened.Root, "cmd.pid")
	cmd, _ := startWorkspaceCLIChild(t, env, opened.ID, `echo $$ > cmd.pid; exec sleep 300`)
	var pid int
	waitFor(t, "the remote command to start", 20*time.Second, func() bool { pid = readPIDFile(t, pidFile); return pid > 0 })
	if processGone(pid) {
		t.Fatal("remote command is not running")
	}
	// SIGKILL: the host process cannot clean up. The VM-side watchdog notices
	// the heartbeats stop and kills the command's process group.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitFor(t, "the watchdog to kill the remote command", 20*time.Second, func() bool { return processGone(pid) })
	var supervisor *fakeWorkspaceProc
	for _, proc := range env.cloud.supervisors() {
		supervisor = proc
	}
	select {
	case <-supervisor.done:
	case <-time.After(20 * time.Second):
		t.Fatal("the supervisor did not exit")
	}
	env.cloud.mu.Lock()
	trailer := supervisor.stderr.String()
	env.cloud.mu.Unlock()
	if !strings.Contains(trailer, `"code": "watchdog"`) {
		t.Fatalf("supervisor trailer = %q", trailer)
	}
}

func TestWorkspaceExecHostSignalEndsRemoteProcessPromptly(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")
	pidFile := filepath.Join(opened.Root, "cmd.pid")
	cmd, output := startWorkspaceCLIChild(t, env, opened.ID, `trap "" INT; echo $$ > cmd.pid; exec sleep 300`)
	var pid int
	waitFor(t, "the remote command to start", 20*time.Second, func() bool { pid = readPIDFile(t, pidFile); return pid > 0 })
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128+15 {
		t.Fatalf("ws sh after SIGTERM: %v\n%s", err, output.String())
	}
	if !processGone(pid) {
		t.Fatal("the remote command outlived the signalled host process")
	}
}

func TestWorkspaceCloseCopiesLogsBeforeTeardown(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")
	sandboxID := env.record(opened.ID).ProviderInstanceID
	if result := env.run("workspace", "exec", opened.ID, "--", "sh", "-c", "echo build-output-marker; echo change > new.txt"); result.code != 0 {
		t.Fatalf("exec = %+v", result)
	}
	if result := env.run("workspace", "close", opened.ID, "--json"); result.code != 0 || !strings.Contains(result.stdout, `"state":"closed"`) {
		t.Fatalf("close = %+v", result)
	}
	hostDir := filepath.Join(env.paths.Workspaces, opened.ID)
	logs, _ := filepath.Glob(filepath.Join(hostDir, "logs", "*.log"))
	if len(logs) != 1 {
		t.Fatalf("copied logs = %v", logs)
	}
	if data, _ := os.ReadFile(logs[0]); !strings.Contains(string(data), "build-output-marker") || !strings.Contains(string(data), `"exit_code": 0`) {
		t.Fatalf("copied log = %q", data)
	}
	if summary, _ := os.ReadFile(filepath.Join(hostDir, "close-summary.txt")); !strings.Contains(string(summary), "new.txt") {
		t.Fatalf("close summary = %q", summary)
	}
	var record workspaceCloseRecord
	data, _ := os.ReadFile(filepath.Join(hostDir, "close.json"))
	if err := json.Unmarshal(data, &record); err != nil || !record.LogsCopied || record.Reason != "manual" || record.State != db.WorkspaceStateClosed {
		t.Fatalf("close.json = %s (%v)", data, err)
	}
	events := env.cloud.snapshotEvents()
	copied := slices.Index(events, "start:sh gitmoot-ws-logs")
	deleted := slices.Index(events, "delete:"+sandboxID)
	if copied < 0 || deleted < 0 || copied > deleted {
		t.Fatalf("events = %v: logs must be copied before the sandbox is deleted", events)
	}
}

func TestWorkspaceCostCapRefusesOpenAndClosesRunning(t *testing.T) {
	// Three reservations of $10 do not fit under $29.50; two do.
	env := newWorkspaceTestEnv(t, workspaceTestConfig{rate: 10, maxTotal: 29.5, maxWorkspace: 29.5})
	first := env.open("--issue", "1")
	env.advance(time.Minute)
	second := env.open("--issue", "2")
	refused := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "3", "--json")
	if refused.code != workspaceCLIFailure || workspaceErrorCode(t, refused.stdout) != wsErrCostCap {
		t.Fatalf("third open = %+v", refused)
	}
	if env.cloud.createdCount() != 2 {
		t.Fatalf("a refused open provisioned: %d sandboxes", env.cloud.createdCount())
	}
	committed := func() float64 {
		var total float64
		_ = withStore(env.home, func(store *db.Store) error {
			var err error
			total, err = store.WorkspaceCommittedUSD(context.Background())
			return err
		})
		return total
	}
	if got := committed(); got != 20 {
		t.Fatalf("committed after two opens = %v", got)
	}
	// At 55 minutes both are due: the older for 55 more minutes ($9.17),
	// which fits under $29.50; the newer for 54 ($9.00), which then does not,
	// so it is closed with reason cost_cap.
	env.advance(54 * time.Minute)
	env.sweep()
	a, b := env.record(first.ID), env.record(second.ID)
	if a.State != db.WorkspaceStateReady || b.State != db.WorkspaceStateClosed || b.CloseReason != workspaceReasonCostCap {
		t.Fatalf("after first settlement: %s %s(%s)", a.State, b.State, b.CloseReason)
	}
	if !closeTo(a.CostAccruedUSD, 55.0/6) || !closeTo(a.CostReservedUSD, 10+55.0/6) || !a.PaidUntil.Equal(env.clock().Add(time.Hour)) {
		t.Fatalf("renewed ledger = %+v", a)
	}
	if timeouts := env.cloud.sandbox(a.ProviderInstanceID).timeouts; len(timeouts) != 2 || timeouts[1] != 3540 {
		t.Fatalf("provider deadline moves = %v", timeouts)
	}
	if got := committed(); got > 29.5+1e-9 {
		t.Fatalf("committed %v passed the cap", got)
	}
	// At the next settlement the cap stops the survivor too.
	env.advance(55 * time.Minute)
	env.sweep()
	a = env.record(first.ID)
	if a.State != db.WorkspaceStateClosed || a.CloseReason != workspaceReasonCostCap || !env.cloud.sandbox(a.ProviderInstanceID).deleted {
		t.Fatalf("survivor = %+v", a)
	}
	if got := committed(); got > 29.5+1e-9 || !closeTo(got, 110.0/6+54.0/6) {
		t.Fatalf("total spend = %v", got)
	}
	if again := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "4", "--json"); workspaceErrorCode(t, again.stdout) != wsErrCostCap {
		t.Fatalf("open after the cap = %+v", again)
	}
}

func TestWorkspaceLoweredGlobalCapClosesRunning(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{rate: 2})
	opened := env.open("--issue", "1")
	data, _ := os.ReadFile(env.paths.ConfigFile)
	lowered := strings.Replace(string(data), "\nmax_total_usd = 50", "\nmax_total_usd = 1", 1)
	lowered = strings.Replace(lowered, "\nmax_workspace_usd = 10", "\nmax_workspace_usd = 1", 1)
	if err := os.WriteFile(env.paths.ConfigFile, []byte(lowered), 0o600); err != nil {
		t.Fatal(err)
	}
	env.sweep()
	if record := env.record(opened.ID); record.State != db.WorkspaceStateClosed || record.CloseReason != workspaceReasonCostCap {
		t.Fatalf("after lowering the cap: %+v", record)
	}
}

func closeTo(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestWorkspaceIdleTimeoutCloses(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{idle: "30m"})
	opened := env.open("--issue", "1")
	env.advance(29 * time.Minute)
	env.sweep()
	if record := env.record(opened.ID); record.State != db.WorkspaceStateReady {
		t.Fatalf("closed before the idle timeout: %+v", record)
	}
	// exec moves last_active_at, so the timeout restarts from it.
	if result := env.run("workspace", "exec", opened.ID, "--", "true"); result.code != 0 {
		t.Fatalf("exec = %+v", result)
	}
	env.advance(29 * time.Minute)
	env.sweep()
	if record := env.record(opened.ID); record.State != db.WorkspaceStateReady {
		t.Fatalf("closed although exec kept it active: %+v", record)
	}
	env.advance(2 * time.Minute)
	out := env.sweep()
	record := env.record(opened.ID)
	if record.State != db.WorkspaceStateClosed || record.CloseReason != workspaceReasonIdle || !env.cloud.sandbox(record.ProviderInstanceID).deleted {
		t.Fatalf("after the idle timeout: %+v\n%s", record, out)
	}
	if _, err := os.Stat(filepath.Join(env.paths.Workspaces, opened.ID, "close.json")); err != nil {
		t.Fatalf("idle close wrote no close record: %v", err)
	}
}

func TestWorkspacePullRequestMergeOrCloseClosesWorkspace(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	env.addPullRequest(7, "feature/thing")
	prWorkspace := env.open("--pr", "7")
	issueWorkspace := env.open("--issue", "12")
	repo := github.Repository{Owner: "acme", Name: "widget"}
	reconcile := func(open ...github.PullRequest) {
		t.Helper()
		var out bytes.Buffer
		if err := withStore(env.home, func(store *db.Store) error {
			return workspacePullRequestReconciler(store, env.home, workspaceTestGitHub{env: env}, repo, &out)(context.Background(), open)
		}); err != nil {
			t.Fatalf("reconcile: %v\n%s", err, out.String())
		}
	}
	env.pullsMu.Lock()
	pr7 := env.pulls[7]
	env.pullsMu.Unlock()
	issuePR := github.PullRequest{Number: 9, State: "open", HeadRef: "ws/intern/12", HeadRepoFullName: "acme/widget", BaseRef: "main"}
	env.setPull(issuePR)
	reconcile(pr7, issuePR)
	if record := env.record(issueWorkspace.ID); record.LinkedPR != 9 || record.State != db.WorkspaceStateReady {
		t.Fatalf("issue workspace after its PR opened: %+v", record)
	}
	merged := pr7
	merged.State, merged.Merged, merged.MergedAt = "closed", true, "2026-11-19T13:00:00Z"
	env.setPull(merged)
	reconcile(issuePR)
	if record := env.record(prWorkspace.ID); record.State != db.WorkspaceStateClosed || record.CloseReason != workspaceReasonPRMerged || !env.cloud.sandbox(record.ProviderInstanceID).deleted {
		t.Fatalf("PR workspace after merge: %+v", record)
	}
	issuePR.State = "closed"
	env.setPull(issuePR)
	reconcile()
	if record := env.record(issueWorkspace.ID); record.State != db.WorkspaceStateClosed || record.CloseReason != workspaceReasonPRClosed {
		t.Fatalf("issue workspace after its PR closed: %+v", record)
	}
}

func TestWorkspaceDaemonRestartReattachesFromRecord(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opened := env.open("--issue", "1")
	sandboxID := env.record(opened.ID).ProviderInstanceID
	// A new daemon: nothing in memory, only the workspaces table.
	workspaceSweepState = &workspaceSweepGate{}
	env.advance(52 * time.Minute)
	var out bytes.Buffer
	if err := withStoreAndPaths(env.home, func(paths config.Paths, store *db.Store) error {
		worker := defaultJobWorker(store, &out, env.home)
		sweepWorkspacesTick(context.Background(), worker, &out, time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	record := env.record(opened.ID)
	if record.State != db.WorkspaceStateReady || !record.PaidUntil.Equal(env.clock().Add(time.Hour)) {
		t.Fatalf("after the restarted sweep: %+v\n%s", record, out.String())
	}
	if timeouts := env.cloud.sandbox(sandboxID).timeouts; len(timeouts) != 2 || timeouts[1] != 3540 {
		t.Fatalf("restarted daemon did not renew the recorded sandbox: %v\n%s", timeouts, out.String())
	}
	// And exec, a fresh process each time, attaches from the same record.
	if result := env.run("workspace", "exec", opened.ID, "--", "cat", "README.md"); result.code != 0 || result.stdout != "widget\n" {
		t.Fatalf("exec after restart = %+v", result)
	}
}

func TestWorkspaceOpenRequiresEnabledAndSeat(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	data, _ := os.ReadFile(env.paths.ConfigFile)
	if err := os.WriteFile(env.paths.ConfigFile, []byte(strings.Replace(string(data), "[workspaces]\nenabled = true", "[workspaces]\nenabled = false", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if off := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "1", "--json"); workspaceErrorCode(t, off.stdout) != wsErrDisabled {
		t.Fatalf("open while disabled = %+v", off)
	}
	t.Setenv("GITMOOT_ORG_ROLE", "")
	if seatless := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "1", "--json"); workspaceErrorCode(t, seatless.stdout) != wsErrInvalidArgs {
		t.Fatalf("open without a seat = %+v", seatless)
	}
	if env.cloud.createdCount() != 0 {
		t.Fatal("a refused open provisioned")
	}
}

func TestWorkspaceStatusTrailerHoldsOnlyTheTrailer(t *testing.T) {
	var out bytes.Buffer
	trailer := &workspaceStatusTrailer{out: &out}
	full := "partial \x1e line\nmore" + string(workspaceStatusMark) + `{"exit_code":3,"signal":null,"timed_out":false,"error":null}` + "\n"
	for i := 0; i < len(full); i += 3 {
		end := min(i+3, len(full))
		_, _ = trailer.Write([]byte(full[i:end]))
	}
	status, ok := trailer.finish()
	if !ok || status.ExitCode != 3 || out.String() != "partial \x1e line\nmore" {
		t.Fatalf("trailer = %+v %v, output %q", status, ok, out.String())
	}
	out.Reset()
	lookalike := &workspaceStatusTrailer{out: &out}
	_, _ = lookalike.Write([]byte("x" + string(workspaceStatusMark) + "not json"))
	if _, ok := lookalike.finish(); ok || out.String() != "x"+string(workspaceStatusMark)+"not json" {
		t.Fatalf("lookalike output = %q", out.String())
	}
}
