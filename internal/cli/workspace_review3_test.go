package cli

import (
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
