package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
)

// repoRecordForCheckout must record the REMOTE's default branch, not the branch
// the worktree happens to be on (#2145).
//
// This tests the DEFECT SITE rather than the helper. The git-level test proves
// RemoteDefaultBranch reads origin/HEAD correctly; it says nothing about which
// source this function prefers, and a mutant that consults CurrentBranch first
// and origin/HEAD only as a fallback passes every other test in the package -
// which is precisely the shape of the bug that was in production.
func TestRepoRecordForCheckoutPrefersRemoteDefaultOverCheckedOutBranch(t *testing.T) {
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

	// Clone locally so origin/HEAD is created, then point origin at the forge URL
	// the record is validated against. set-url does not disturb refs, so
	// refs/remotes/origin/HEAD still resolves to origin/master.
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", origin, clone)
	runGit(t, clone, "remote", "set-url", "origin", "https://github.com/jerryfane/herdr.git")
	runGit(t, clone, "checkout", "-b", "fix/lan-address-portability")

	record, err := repoRecordForCheckout(context.Background(),
		github.Repository{Owner: "jerryfane", Name: "herdr"},
		gitutil.NewHostClient(clone))
	if err != nil {
		t.Fatalf("repoRecordForCheckout returned error: %v", err)
	}
	if record.DefaultBranch != "master" {
		t.Fatalf("DefaultBranch = %q, want master; the worktree is on fix/lan-address-portability and that must not be recorded as the default", record.DefaultBranch)
	}

	// Control: the worktree really is on the other branch, so the assertion is
	// distinguishing the two sources rather than passing because they agree.
	current, err := gitutil.NewHostClient(clone).CurrentBranch(context.Background())
	if err != nil || current != "fix/lan-address-portability" {
		t.Fatalf("CurrentBranch = %q err=%v, want the feature branch; fixture does not separate the sources", current, err)
	}
}

// With origin/HEAD unresolvable the checked-out branch is the only source, and
// dropping it would register a repository with no base branch at all - which
// repo doctor reports as a missing branch and dispatch reads as an empty
// default. That regression was caught by CI on the first version of this fix.
func TestRepoRecordForCheckoutFallsBackWhenOriginHeadIsUnset(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")
	// An origin exists but was never cloned from, so origin/HEAD is absent.
	runGit(t, dir, "remote", "add", "origin", "https://github.com/jerryfane/herdr.git")

	record, err := repoRecordForCheckout(context.Background(),
		github.Repository{Owner: "jerryfane", Name: "herdr"},
		gitutil.NewHostClient(dir))
	if err != nil {
		t.Fatalf("repoRecordForCheckout returned error: %v", err)
	}
	if record.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want main from the fallback; an empty value registers a repo with no base branch", record.DefaultBranch)
	}
}
