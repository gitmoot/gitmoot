package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// RemoteDefaultBranch must read the REMOTE's default, not the local HEAD.
//
// This is the #2145 defect as a test: the caller previously used CurrentBranch,
// so a clone sitting on a feature branch recorded that branch as the
// repository's default - and the value is consumed as the base branch. The
// fixture therefore checks out a feature branch and asserts the remote's default
// is returned anyway. Under the old CurrentBranch read this returns
// "fix/lan-address-portability", which is exactly what was found in the store.
func TestRemoteDefaultBranchIgnoresTheCheckedOutBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origin := t.TempDir()
	runGit(t, origin, "init", "-b", "master")
	runGit(t, origin, "config", "user.email", "gitmoot@example.com")
	runGit(t, origin, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("# origin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, origin, "add", "README.md")
	runGit(t, origin, "commit", "-m", "init")

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", origin, clone)
	// The worktree now sits on a branch that is NOT the remote default.
	runGit(t, clone, "checkout", "-b", "fix/lan-address-portability")

	client := NewHostClient(clone)
	branch, err := client.RemoteDefaultBranch(context.Background())
	if err != nil {
		t.Fatalf("RemoteDefaultBranch returned error: %v", err)
	}
	if branch != "master" {
		t.Fatalf("RemoteDefaultBranch = %q, want master (the remote default, not the checked-out branch)", branch)
	}

	// Control: the local HEAD really is the other branch, so the assertion above
	// is distinguishing the two rather than passing because they coincide.
	current, err := client.CurrentBranch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current != "fix/lan-address-portability" {
		t.Fatalf("CurrentBranch = %q, want the feature branch; fixture does not separate the two values", current)
	}
}

// An unset origin/HEAD must ERROR rather than guess. The caller stores the
// result in the base-branch field and preserves the existing value on an empty
// read, so a fabricated "main" would silently overwrite a correct record on any
// repository with a different default.
func TestRemoteDefaultBranchErrorsWhenOriginHeadIsUnset(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# no origin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")

	// Second arm, and the REALISTIC one: a clone that HAS an origin but whose
	// origin/HEAD ref has been deleted. `git remote set-head --delete` is the
	// supported way to reach that state, and it is what a partial or mirrored
	// clone can look like in production. The no-origin repo above cannot
	// distinguish "unset" from "no remote".
	origin := t.TempDir()
	runGit(t, origin, "init", "-b", "master")
	runGit(t, origin, "config", "user.email", "gitmoot@example.com")
	runGit(t, origin, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("# origin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, origin, "add", "README.md")
	runGit(t, origin, "commit", "-m", "init")
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", origin, clone)
	runGit(t, clone, "remote", "set-head", "origin", "--delete")
	if deleted, err := NewHostClient(clone).RemoteDefaultBranch(context.Background()); err == nil {
		t.Fatalf("RemoteDefaultBranch = %q with no error after origin/HEAD was deleted, want an error", deleted)
	}

	branch, err := NewHostClient(dir).RemoteDefaultBranch(context.Background())
	if err == nil {
		t.Fatalf("RemoteDefaultBranch = %q with no error, want an error when origin/HEAD is unset", branch)
	}
	if branch != "" {
		t.Fatalf("RemoteDefaultBranch = %q on error, want empty", branch)
	}
}
