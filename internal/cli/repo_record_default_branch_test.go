package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	record, fromRemote, err := repoRecordForCheckout(context.Background(),
		github.Repository{Owner: "jerryfane", Name: "herdr"},
		gitutil.NewHostClient(clone))
	if err != nil {
		t.Fatalf("repoRecordForCheckout returned error: %v", err)
	}
	// The provenance flag is what lets the caller overwrite a stored value, so a
	// correct branch reported as a fallback is as broken as a wrong branch.
	if !fromRemote {
		t.Fatal("fromRemote = false with origin/HEAD resolvable; the caller would then refuse to correct a stored value")
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

// A checkout whose configured origin is the forge URL but whose fetches are
// redirected by url.*.insteadOf (a local mirror) is that forge repository: the
// identity check must read the configured URL, not the rewritten transport. This
// is what lets a built binary register a test repo with no network.
func TestRepoRecordForCheckoutUsesConfiguredOriginNotInsteadOfRewrite(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	mirror := t.TempDir()
	runGit(t, mirror, "init", "-b", "main")
	runGit(t, mirror, "config", "user.email", "gitmoot@example.com")
	runGit(t, mirror, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(mirror, "README.md"), []byte("# mirror\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, mirror, "add", "README.md")
	runGit(t, mirror, "commit", "-m", "init")

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", mirror, clone)
	runGit(t, clone, "remote", "set-url", "origin", "https://github.com/acme/widget.git")
	runGit(t, clone, "config", "url."+mirror+".insteadOf", "https://github.com/acme/widget.git")

	// Control: the rewrite is active, so get-url really returns the local path.
	rewritten, err := gitutil.NewHostClient(clone).OriginRemote(context.Background())
	if err != nil || rewritten != mirror {
		t.Fatalf("OriginRemote = %q err=%v, want the insteadOf target %q; fixture does not exercise the rewrite", rewritten, err, mirror)
	}

	record, _, err := repoRecordForCheckout(context.Background(),
		github.Repository{Owner: "acme", Name: "widget"},
		gitutil.NewHostClient(clone))
	if err != nil {
		t.Fatalf("repoRecordForCheckout returned error %v; a checkout whose configured origin is acme/widget must register as acme/widget", err)
	}
	if record.FullName() != "acme/widget" {
		t.Fatalf("FullName() = %q, want acme/widget", record.FullName())
	}
}

// Origin can carry several url entries, directly or through an include. Git
// fetches from the first, so that is the repository the checkout IS: it must
// register as the first and refuse the last (which `git config --get` returns).
func TestRepoRecordForCheckoutUsesFirstOfSeveralOriginURLs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, viaInclude := range []bool{false, true} {
		name := "direct"
		if viaInclude {
			name = "include"
		}
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			runGit(t, repo, "init", "-b", "main")
			runGit(t, repo, "remote", "add", "origin", "https://github.com/acme/actual.git")
			if viaInclude {
				extra := filepath.Join(t.TempDir(), "extra.gitconfig")
				if err := os.WriteFile(extra, []byte("[remote \"origin\"]\n\turl = https://github.com/acme/other.git\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGit(t, repo, "config", "--add", "include.path", extra)
			} else {
				runGit(t, repo, "config", "--add", "remote.origin.url", "https://github.com/acme/other.git")
			}
			// Control: git really sees two URLs and fetches from the first.
			client := gitutil.NewHostClient(repo)
			if fetched, err := client.OriginRemote(context.Background()); err != nil || fetched != "https://github.com/acme/actual.git" {
				t.Fatalf("git remote get-url = %q err=%v; fixture should fetch acme/actual", fetched, err)
			}
			out, err := exec.Command("git", "-C", repo, "config", "--get", "remote.origin.url").Output()
			if err != nil || strings.TrimSpace(string(out)) != "https://github.com/acme/other.git" {
				t.Fatalf("git config --get = %q err=%v; fixture should have acme/other last", out, err)
			}

			if _, _, err := repoRecordForCheckout(context.Background(), github.Repository{Owner: "acme", Name: "actual"}, client); err != nil {
				t.Fatalf("registering as the fetched repository acme/actual failed: %v", err)
			}
			if _, _, err := repoRecordForCheckout(context.Background(), github.Repository{Owner: "acme", Name: "other"}, client); err == nil {
				t.Fatal("registering as acme/other succeeded, but origin fetches acme/actual")
			}
		})
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

	record, fromRemote, err := repoRecordForCheckout(context.Background(),
		github.Repository{Owner: "jerryfane", Name: "herdr"},
		gitutil.NewHostClient(dir))
	if err != nil {
		t.Fatalf("repoRecordForCheckout returned error: %v", err)
	}
	if fromRemote {
		t.Fatal("fromRemote = true with origin/HEAD unset; a fallback value must never be reported as remote-derived")
	}
	if record.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want main from the fallback; an empty value registers a repo with no base branch", record.DefaultBranch)
	}
}
