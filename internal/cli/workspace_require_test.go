package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
