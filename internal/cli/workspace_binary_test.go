package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// TestWorkspaceLocalProviderDrivesBuiltBinary is the no-spend gate both repos
// share: the BUILT gitmoot binary, a throwaway home with
// [workspaces] provider = "local", and the contract end to end.
func TestWorkspaceLocalProviderDrivesBuiltBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the gitmoot binary")
	}
	for _, tool := range []string{"go", "git", "python3", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required", tool)
		}
	}
	binary := filepath.Join(t.TempDir(), "gitmoot")
	build := exec.Command("go", "build", "-o", binary, "github.com/gitmoot/gitmoot/cmd/gitmoot")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}

	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	appendWorkspaceConfig(t, paths, "\n[workspaces]\nenabled = true\nprovider = \"local\"\n")
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	checkout := filepath.Join(root, "checkout")
	runWorkspaceTestGit(t, root, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	runWorkspaceTestGit(t, root, "init", "--quiet", "--initial-branch=main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("widget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceTestGit(t, seed, "add", "README.md")
	runWorkspaceTestGit(t, seed, "commit", "--quiet", "-m", "initial")
	runWorkspaceTestGit(t, seed, "push", "--quiet", origin, "main")
	runWorkspaceTestGit(t, root, "clone", "--quiet", origin, checkout)
	if err := withStore(home, func(store *db.Store) error {
		return store.UpsertRepo(context.Background(), db.Repo{Owner: "acme", Name: "widget", DefaultBranch: "main", RemoteURL: origin, CheckoutPath: checkout})
	}); err != nil {
		t.Fatal(err)
	}

	gitmoot := func(extraEnv []string, args ...string) (int, string, string) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GITMOOT_ORG_ROLE=intern", "HOST_SECRET=do-not-pass"}, extraEnv...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("gitmoot %v: %v", args, err)
		}
		return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()
	}

	code, stdout, stderr := gitmoot(nil, "workspace", "open", "--home", home, "--repo", "acme/widget", "--issue", "5", "--json")
	if code != 0 {
		t.Fatalf("open exited %d: %s %s", code, stdout, stderr)
	}
	var opened workspaceJSON
	if err := json.Unmarshal([]byte(stdout), &opened); err != nil {
		t.Fatalf("open printed %q: %v", stdout, err)
	}
	instances := filepath.Join(paths.Home, "execbackends", "local")
	if opened.Provider != "local" || opened.State != "ready" || opened.Branch != "ws/intern/5" || !strings.HasPrefix(opened.Root, instances+string(filepath.Separator)) {
		t.Fatalf("open = %+v", opened)
	}
	code, stdout, _ = gitmoot(nil, "workspace", "show", opened.ID, "--home", home, "--json")
	if code != 0 || !strings.Contains(stdout, `"provider":"local"`) {
		t.Fatalf("show = %d %s", code, stdout)
	}

	statusFile := filepath.Join(t.TempDir(), "status.json")
	workspaceEnv := []string{"GITMOOT_WORKSPACE=" + opened.ID}
	code, stdout, stderr = gitmoot(workspaceEnv, "ws", "sh", "--home", home, "--status-file", statusFile, `echo hi; cat README.md; echo "secret=${HOST_SECRET-}" >&2; exit 3`)
	if code != 3 || stdout != "hi\nwidget\n" || stderr != "secret=\n" {
		t.Fatalf("ws sh = %d %q %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != `{"exit_code":3,"signal":null,"timed_out":false,"error":null}` {
		t.Fatalf("status file = %s", data)
	}
	code, _, stderr = gitmoot(workspaceEnv, "ws", "sh", "--home", home, "--cwd", "..", "pwd")
	if code != workspaceCLIFailure || workspaceErrorCode(t, stderr) != wsErrCwdOutsideRoot {
		t.Fatalf("cwd escape = %d %s", code, stderr)
	}

	code, stdout, stderr = gitmoot(nil, "workspace", "close", opened.ID, "--home", home, "--json")
	if code != 0 || !strings.Contains(stdout, `"state":"closed"`) {
		t.Fatalf("close = %d %s %s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Dir(opened.Root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local instance survived close: %v", err)
	}
	logs, _ := filepath.Glob(filepath.Join(paths.Workspaces, opened.ID, "logs", "*.log"))
	if len(logs) != 1 {
		t.Fatalf("copied logs = %v", logs)
	}
	code, _, stderr = gitmoot(workspaceEnv, "ws", "sh", "--home", home, "true")
	if code != workspaceCLIFailure || workspaceErrorCode(t, stderr) != wsErrNotReady {
		t.Fatalf("ws sh after close = %d %s", code, stderr)
	}
}
