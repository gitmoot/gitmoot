package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Round-3 review of PR #2694. Only helpers that existed at a8860395, so the
// test also runs (and fails) on that commit.

// The reviewer's probe on the local backend: a workspace command owns every
// ancestor of the root, so it renames the instance home and puts a symlink
// to an outside copy (with its own workspace/) in its place. The next exec
// must be refused, not run in the outside copy.
func TestWorkspaceSupervisorRefusesReplacedRootParent(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{local: true})
	opened := env.open("--issue", "1")
	home := filepath.Dir(opened.Root)
	outside := filepath.Join(t.TempDir(), "outside")
	swap := `set -e
cp -a "$HOME" "$OUTSIDE"
echo outside > "$OUTSIDE/workspace/marker"
mv "$HOME" "$HOME.moved"
ln -s "$OUTSIDE" "$HOME"`
	result := env.run("workspace", "exec", opened.ID, "--env", "OUTSIDE="+outside, "--", "sh", "-c", swap)
	if result.code != 0 {
		t.Fatalf("swap = %+v", result)
	}
	if target, err := os.Readlink(home); err != nil || target != outside {
		t.Fatalf("home is not the outside symlink: %q %v", target, err)
	}
	next := env.run("workspace", "exec", opened.ID, "--", "sh", "-c", "pwd -P; cat marker")
	if next.code != workspaceCLIFailure || strings.Contains(next.stdout, "outside") || workspaceErrorCode(t, next.stderr) != wsErrCwdOutsideRoot {
		t.Fatalf("exec after the parent swap = %+v (outside is %s)", next, outside)
	}
}

// open refuses a root that is not a directory: the identity it records must
// name the directory the supervisor will open.
func TestWorkspaceRootIdentityRefusesNonDirectory(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{local: true})
	opened := env.open("--issue", "1")
	sandbox, err := workspaceTestLocalSandbox(env, opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := workspaceRootIdentity(context.Background(), sandbox); err != nil {
		t.Fatalf("identity of the real root: %v", err)
	}
	if err := os.RemoveAll(opened.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opened.Root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workspaceRootIdentity(context.Background(), sandbox); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("identity of a regular-file root = %v", err)
	}
	if err := os.Remove(opened.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), opened.Root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workspaceRootIdentity(context.Background(), sandbox); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("identity of a symlink root = %v", err)
	}
}

func workspaceTestLocalSandbox(env *workspaceTestEnv, id string) (workspaceSandbox, error) {
	record := env.record(id)
	provider, err := newWorkspaceProvider(env.paths, record.Provider, "")
	if err != nil {
		return nil, err
	}
	sandbox, _, err := provider.backend.attach(context.Background(), record.ProviderInstanceID)
	return sandbox, err
}
