package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2704: a repository's [repos."o/r".workspace] template and require checks.

func TestWorkspaceTemplatePrecedence(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	opens := []struct {
		issue, want string
		config      string
	}{
		// [remote_exec] e2b_template when nothing else is set.
		{issue: "1", want: "tmpl"},
		// [workspaces] template over [remote_exec] e2b_template.
		{issue: "2", want: "global-v1", config: "template = \"global-v1\"\n"},
		// The repository's template over both; its key matches case-insensitively.
		{issue: "3", want: "team/widget:v2", config: "\n[repos.\"ACME/Widget\".workspace]\ntemplate = \"team/widget:v2\"\n"},
	}
	for _, step := range opens {
		appendWorkspaceConfig(t, env.paths, step.config)
		opened := env.openDetail("--issue", step.issue)
		if got := env.cloud.sandbox(env.record(opened.ID).ProviderInstanceID).template; got != step.want {
			t.Fatalf("issue %s: sandbox template = %q, want %q", step.issue, got, step.want)
		}
		if opened.Template == nil || *opened.Template != step.want {
			t.Fatalf("issue %s: open reported template %v, want %q", step.issue, opened.Template, step.want)
		}
		shown := env.showDetail(opened.ID)
		if shown.Template == nil || *shown.Template != step.want || shown.Checks == nil || len(shown.Checks) != 0 {
			t.Fatalf("issue %s: show = template %v checks %#v, want %q and []", step.issue, shown.Template, shown.Checks, step.want)
		}
		if text := env.mustRun(nil, "workspace", "show", opened.ID); !strings.Contains(text, "template     "+step.want+"\n") {
			t.Fatalf("issue %s: show text lacks the template:\n%s", step.issue, text)
		}
	}
}

func TestWorkspaceRequireChecksPass(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "e2b-fake", true: "local"}[local], func(t *testing.T) {
			env := newWorkspaceTestEnv(t, workspaceTestConfig{local: local})
			checks := []string{"test -f README.md", "pwd > checked.txt && echo $HOME >> checked.txt"}
			appendWorkspaceConfig(t, env.paths, "\n[repos.\"acme/widget\".workspace]\nrequire = [\"test -f README.md\", \"pwd > checked.txt && echo $HOME >> checked.txt\"]\n")
			opened := env.openDetail("--issue", "12")
			if opened.State != db.WorkspaceStateReady || !reflect.DeepEqual(opened.Checks, checks) {
				t.Fatalf("open = state %s checks %#v, want ready and %#v", opened.State, opened.Checks, checks)
			}
			// The checks ran in order in the workspace root, through the exec
			// path's login shell and base environment.
			data, err := os.ReadFile(filepath.Join(opened.Root, "checked.txt"))
			if err != nil {
				t.Fatalf("the checks did not run in the workspace root: %v", err)
			}
			if lines := strings.Fields(string(data)); len(lines) != 2 || lines[0] != opened.Root || lines[1] != filepath.Dir(opened.Root) {
				t.Fatalf("checked.txt = %q, want the root %s and the guest home", data, opened.Root)
			}
			if local {
				if opened.Template != nil {
					t.Fatalf("the local provider reported template %q", *opened.Template)
				}
			}
			if shown := env.showDetail(opened.ID); !reflect.DeepEqual(shown.Checks, checks) {
				t.Fatalf("show checks = %#v, want %#v", shown.Checks, checks)
			}
			if text := env.mustRun(nil, "workspace", "show", opened.ID); !strings.Contains(text, "check        test -f README.md\n") {
				t.Fatalf("show text lacks the checks:\n%s", text)
			}
			text := env.mustRun(nil, "workspace", "open", "--repo", "acme/widget", "--issue", "13")
			if !strings.Contains(text, " ready: ") || !strings.Contains(text, "check passed: test -f README.md\ncheck passed: pwd > checked.txt") {
				t.Fatalf("open text = %q, want the passed checks", text)
			}
		})
	}
}

func TestWorkspaceRequireCheckFailureClosesWorkspace(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "e2b-fake", true: "local"}[local], func(t *testing.T) {
			env := newWorkspaceTestEnv(t, workspaceTestConfig{local: local})
			failing := `head -c 5000 /dev/zero | tr '\\0' x; echo; echo api_key=sekrit-value-123; sleep 0.3; echo to-stderr >&2; exit 3`
			appendWorkspaceConfig(t, env.paths, "\n[repos.\"acme/widget\".workspace]\nrequire = [\"true\", \""+failing+"\", \"touch never.txt\"]\n")
			failingCommand := strings.ReplaceAll(failing, `\\`, `\`)
			result := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "12", "--json")
			unmet := workspaceErrorShape(t, result)
			if unmet.Code != wsErrRequirementsUnmet || unmet.FailedCheck != failingCommand || unmet.ExitCode == nil || *unmet.ExitCode != 3 || unmet.OutputTail == nil {
				t.Fatalf("open = exit %d %+v, want requirements_unmet for %q exiting 3", result.code, unmet, failingCommand)
			}
			tail := *unmet.OutputTail
			if len(tail) > workspaceRequireTailBytes || !strings.HasSuffix(strings.TrimSpace(tail), "to-stderr") || !strings.Contains(tail, "api_key=[REDACTED]") || strings.Contains(tail, "sekrit") || !strings.HasPrefix(tail, "xxxx") {
				t.Fatalf("output_tail (%d bytes) = %q, want the redacted last %d bytes of stdout and stderr", len(tail), tail, workspaceRequireTailBytes)
			}
			workspace := env.onlyWorkspace()
			if workspace.State != db.WorkspaceStateClosed || workspace.CloseReason != "requirements_unmet: "+failingCommand || workspace.ClosedAt.IsZero() {
				t.Fatalf("workspace after a failed check = state %s reason %q closed_at %v", workspace.State, workspace.CloseReason, workspace.ClosedAt)
			}
			if !local && !env.cloud.sandbox(workspace.ProviderInstanceID).deleted {
				t.Fatal("the failed open left its sandbox running")
			}
			if _, err := os.Stat(filepath.Join(workspace.Root, "never.txt")); err == nil {
				t.Fatal("a check after the failed one ran")
			}
			// The close settled the ledger through closeWorkspace's close.json.
			var closed workspaceCloseRecord
			data, err := os.ReadFile(filepath.Join(env.paths.Workspaces, workspace.ID, "close.json"))
			if err != nil || json.Unmarshal(data, &closed) != nil || closed.State != db.WorkspaceStateClosed || closed.Reason != workspace.CloseReason {
				t.Fatalf("close.json = %s, %v", data, err)
			}
			// show still reports what open was asked to check.
			if shown := env.showDetail(workspace.ID); len(shown.Checks) != 3 || shown.Checks[1] != failingCommand {
				t.Fatalf("show checks after a failed open = %#v", shown.Checks)
			}
		})
	}
}

func TestWorkspaceRequireCheckTimeout(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{local: true})
	previous := workspaceRequireTimeout
	workspaceRequireTimeout = time.Second
	t.Cleanup(func() { workspaceRequireTimeout = previous })
	appendWorkspaceConfig(t, env.paths, "\n[repos.\"acme/widget\".workspace]\nrequire = [\"echo started; sleep 30\"]\n")
	result := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "12")
	unmet := workspaceErrorShape(t, result)
	if unmet.Code != wsErrRequirementsUnmet || unmet.FailedCheck != "echo started; sleep 30" || unmet.ExitCode == nil || *unmet.ExitCode != 124 ||
		unmet.OutputTail == nil || strings.TrimSpace(*unmet.OutputTail) != "started" {
		t.Fatalf("open = exit %d %+v, want requirements_unmet exit_code 124", result.code, unmet)
	}
	if workspace := env.onlyWorkspace(); workspace.State != db.WorkspaceStateClosed || workspace.CloseReason != "requirements_unmet: echo started; sleep 30" {
		t.Fatalf("workspace after a timed-out check = state %s reason %q", workspace.State, workspace.CloseReason)
	}
}

// workspaceErrorShape requires a gitmoot failure (exit 125) and returns its
// error object from stderr.
func workspaceErrorShape(t *testing.T, result workspaceCLIResult) workspaceError {
	t.Helper()
	if result.code != workspaceCLIFailure {
		t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", result.code, workspaceCLIFailure, result.stdout, result.stderr)
	}
	var shape struct {
		Error *workspaceError `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.stderr)), &shape); err != nil || shape.Error == nil {
		t.Fatalf("stderr %q is not one error line: %v", result.stderr, err)
	}
	return *shape.Error
}

func (e *workspaceTestEnv) openDetail(args ...string) workspaceDetailJSON {
	e.t.Helper()
	out := e.mustRun(nil, append([]string{"workspace", "open", "--repo", "acme/widget", "--json"}, args...)...)
	var opened workspaceDetailJSON
	if err := json.Unmarshal([]byte(out), &opened); err != nil {
		e.t.Fatalf("workspace open printed %q: %v", out, err)
	}
	return opened
}

func (e *workspaceTestEnv) showDetail(id string) workspaceDetailJSON {
	e.t.Helper()
	out := e.mustRun(nil, "workspace", "show", id, "--json")
	var shown workspaceDetailJSON
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		e.t.Fatalf("workspace show printed %q: %v", out, err)
	}
	return shown
}

func (e *workspaceTestEnv) onlyWorkspace() db.Workspace {
	e.t.Helper()
	var listed []workspaceListJSON
	out := e.mustRun(nil, "workspace", "ls", "--all", "--json")
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed) != 1 {
		e.t.Fatalf("ls --all = %s (%v), want one workspace", out, err)
	}
	return e.record(listed[0].ID)
}

// #2707 review P1: the checks run while the workspace is still opening, so an
// opener killed mid-check leaves a workspace the abandoned-open sweep reaps,
// never a ready one the daemon renews until it idles.
func TestWorkspaceOpenKilledMidCheckIsReaped(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	flags := t.TempDir()
	block, started := filepath.Join(flags, "block"), filepath.Join(flags, "started")
	if err := os.WriteFile(block, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check := fmt.Sprintf("if [ -f %s ]; then touch %s; exec sleep 300; fi", block, started)
	appendWorkspaceConfig(t, env.paths, "\n[repos.\"acme/widget\".workspace]\nrequire = ["+strconv.Quote(check)+"]\n")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opener := exec.Command(executable, "workspace", "open", "--home", env.home, "--repo", "acme/widget", "--issue", "12", "--json")
	opener.Env = append(os.Environ(),
		workspaceCLIChildEnv+"=1",
		workspaceChildGuestHomeEnv+"="+env.cloud.guestHome,
		workspaceChildEnvdURLEnv+"="+env.cloud.envd.URL,
	)
	if err := opener.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opener.Process.Kill(); _ = opener.Wait() })
	waitFor(t, "the require check to start", 30*time.Second, func() bool { _, err := os.Stat(started); return err == nil })
	if err := opener.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = opener.Wait()
	dead := env.onlyWorkspace()
	if dead.State != db.WorkspaceStateOpening {
		t.Fatalf("an opener killed mid-check left the workspace %s, want opening", dead.State)
	}
	// The child ran on the real clock; the sweep runs on the test clock.
	env.nowMu.Lock()
	env.now = dead.LastActiveAt.Add(workspaceOpenAbandonedAfter - time.Minute)
	env.nowMu.Unlock()
	env.sweep()
	if got := env.record(dead.ID); got.State != db.WorkspaceStateOpening {
		t.Fatalf("reaped %s after %s, before the abandoned-open bound", got.State, workspaceOpenAbandonedAfter-time.Minute)
	}
	env.advance(time.Minute)
	env.sweep()
	reaped := env.record(dead.ID)
	if reaped.State != db.WorkspaceStateFailed || reaped.CloseReason != workspaceReasonOpenAbandoned || reaped.ClosedAt.IsZero() || !env.cloud.sandbox(reaped.ProviderInstanceID).deleted {
		t.Fatalf("after the bound = state %s reason %q closed_at %v sandbox %+v", reaped.State, reaped.CloseReason, reaped.ClosedAt, env.cloud.sandbox(reaped.ProviderInstanceID))
	}
	var closed workspaceCloseRecord
	data, err := os.ReadFile(filepath.Join(env.paths.Workspaces, reaped.ID, "close.json"))
	if err != nil || json.Unmarshal(data, &closed) != nil || closed.State != db.WorkspaceStateFailed || closed.CostAccruedUSD <= 0 {
		t.Fatalf("close.json = %s, %v; want the ledger settled", data, err)
	}
	// A retry is not refused as already_open.
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	if retried := env.open("--issue", "12"); retried.ID == reaped.ID || retried.State != db.WorkspaceStateReady {
		t.Fatalf("retry = %+v", retried)
	}
}

// A live opener whose checks run past created_at + the abandoned-open bound
// is kept: each check moves last_active_at, which the sweep measures from.
func TestWorkspaceOpenWithLongChecksIsNotReaped(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	flags := t.TempDir()
	flag := func(name string) string { return filepath.Join(flags, name) }
	release := func(name string) {
		if err := os.WriteFile(flag(name), nil, 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { release("go1"); release("go2") })
	var checks []string
	for _, n := range []string{"1", "2"} {
		checks = append(checks, strconv.Quote(fmt.Sprintf("touch %s; while [ ! -f %s ]; do sleep 0.05; done", flag("started"+n), flag("go"+n))))
	}
	appendWorkspaceConfig(t, env.paths, "\n[repos.\"acme/widget\".workspace]\nrequire = ["+strings.Join(checks, ", ")+"]\n")
	done := make(chan workspaceCLIResult, 1)
	go func() { done <- env.run("workspace", "open", "--repo", "acme/widget", "--issue", "12", "--json") }()
	exists := func(name string) func() bool {
		return func() bool { _, err := os.Stat(flag(name)); return err == nil }
	}
	waitFor(t, "the first check to start", 30*time.Second, exists("started1"))
	opening := env.onlyWorkspace()
	if opening.State != db.WorkspaceStateOpening {
		t.Fatalf("the workspace is %s while its checks run, want opening", opening.State)
	}
	env.advance(workspaceOpenAbandonedAfter - time.Minute)
	release("go1")
	waitFor(t, "the second check to start", 30*time.Second, exists("started2"))
	// Past created_at + the bound, but not past the second check's start.
	env.advance(5 * time.Minute)
	env.sweep()
	if got := env.record(opening.ID); got.State != db.WorkspaceStateOpening || env.cloud.sandbox(got.ProviderInstanceID).deleted {
		t.Fatalf("a live opener's workspace was reaped: state %s reason %q", got.State, got.CloseReason)
	}
	release("go2")
	select {
	case result := <-done:
		if result.code != 0 {
			t.Fatalf("open exited %d: %s", result.code, result.stderr)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("open did not finish")
	}
	if got := env.record(opening.ID); got.State != db.WorkspaceStateReady {
		t.Fatalf("after the checks passed the workspace is %s", got.State)
	}
}

// #2707 review P2: a workspace header that will not unquote refuses open
// instead of opening the repository without its checks.
func TestWorkspaceOpenRefusesUnreadableWorkspaceHeader(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{local: true})
	appendWorkspaceConfig(t, env.paths, "\n[repos.\"acme/widget.workspace]\nrequire = [\"go version\"]\n")
	result := env.run("workspace", "open", "--repo", "acme/widget", "--issue", "12")
	if refused := workspaceErrorShape(t, result); refused.Code != wsErrInvalidArgs || !strings.Contains(refused.Message, `repos."acme/widget.workspace`) {
		t.Fatalf("open = %+v, want invalid_args naming the header", refused)
	}
	if list := env.mustRun(nil, "workspace", "ls", "--all", "--json"); strings.TrimSpace(list) != "[]" {
		t.Fatalf("a refused open created %s", list)
	}
}
