package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// pollRepo must correct a cached default branch that names a feature branch
// (#2145 review, F3).
//
// This drives pollRepo itself rather than the helpers underneath it. The review
// found that all three original tests targeted lower-level functions and NOTHING
// exercised the reconcile block's wiring - its placement, its store write, or
// its in-memory update of the record the rest of the poll then uses.
//
// The fixture reproduces the live shape exactly: the stored default branch is
// `fix/lan-address-portability` while the checkout's origin/HEAD resolves to
// `master`, which is what `jerryfane/herdr` looked like on this host.
func TestPollRepoReconcilesCachedDefaultBranch(t *testing.T) {
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
	checkout := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", origin, checkout)
	runGit(t, checkout, "checkout", "-b", "fix/lan-address-portability")

	home, paths, store := heartbeatLoopE2EHome(t)
	ctx := context.Background()
	repoRecord := db.Repo{
		Owner: "owner", Name: "repo", CheckoutPath: checkout,
		DefaultBranch: "fix/lan-address-portability", PollInterval: "30s",
	}
	if err := store.UpsertRepo(ctx, repoRecord); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	poller := defaultRegisteredRepoPoller(store, 1, false, &out, home, paths.Home)
	poller.GitHubClient = func(string) github.Client { return &cliPollFakeGitHub{} }
	if _, err := poller.pollRepo(ctx, repoRecord, time.Now().UTC()); err != nil {
		t.Fatalf("pollRepo returned error: %v", err)
	}

	stored, err := store.GetRepo(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if stored.DefaultBranch != "master" {
		t.Fatalf("stored default_branch = %q, want master after reconcile", stored.DefaultBranch)
	}
	// The correction must be visible to an operator, and must name what it
	// replaced: a silent store write leaves nobody able to tell a reconcile from
	// a value that was always right.
	if !strings.Contains(out.String(), "default branch reconciled to master (was fix/lan-address-portability)") {
		t.Fatalf("poll output = %q, want a reconcile line naming the old value", out.String())
	}
}

// A second pass must not rewrite or re-log. The poll runs every tick, so a
// reconcile that reports a change every time is indistinguishable from a genuine
// correction and would bury the real one.
func TestPollRepoDefaultBranchReconcileIsQuietWhenCorrect(t *testing.T) {
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
	checkout := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", origin, checkout)

	home, paths, store := heartbeatLoopE2EHome(t)
	ctx := context.Background()
	repoRecord := db.Repo{Owner: "owner", Name: "repo", CheckoutPath: checkout, DefaultBranch: "master", PollInterval: "30s"}
	if err := store.UpsertRepo(ctx, repoRecord); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	poller := defaultRegisteredRepoPoller(store, 1, false, &out, home, paths.Home)
	poller.GitHubClient = func(string) github.Client { return &cliPollFakeGitHub{} }
	if _, err := poller.pollRepo(ctx, repoRecord, time.Now().UTC()); err != nil {
		t.Fatalf("pollRepo returned error: %v", err)
	}
	if strings.Contains(out.String(), "default branch reconciled") {
		t.Fatalf("poll output = %q, want no reconcile line when the value is already correct", out.String())
	}
}

// preserveRegisteredRepoFields must let a REMOTE-derived default overwrite a
// stored one, and must not let a FALLBACK value do so (#2145 review, F1).
//
// This was the P1: the function kept the stored value whenever it was non-empty,
// so re-resolving a repository could never correct a wrong base branch. The
// preserve rule was protective only while the resolved value came from the local
// HEAD, which is the defect it was compensating for.
func TestPreserveRegisteredRepoFieldsPrefersRemoteDefault(t *testing.T) {
	existing := db.Repo{DefaultBranch: "fix/lan-address-portability", PollInterval: "30s", Enabled: true}
	for _, test := range []struct {
		name          string
		resolved      db.Repo
		remoteDefault string
		want          string
	}{
		{
			name:          "a remote-derived default overwrites the stored value",
			resolved:      db.Repo{DefaultBranch: "master"},
			remoteDefault: "master",
			want:          "master",
		},
		{
			// origin/HEAD unresolvable: resolved carries the worktree's branch, and
			// overwriting a stored value with it is the original defect.
			name:     "a fallback value must not overwrite the stored value",
			resolved: db.Repo{DefaultBranch: "fix/preview-fork-website-skip"},
			want:     "fix/lan-address-portability",
		},
		{
			name:     "with nothing stored the fallback is kept rather than dropped",
			resolved: db.Repo{DefaultBranch: "main"},
			want:     "main",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			from := existing
			if test.want == "main" {
				from = db.Repo{PollInterval: "30s", Enabled: true}
			}
			got := preserveRegisteredRepoFields(test.resolved, from, test.remoteDefault)
			if got.DefaultBranch != test.want {
				t.Fatalf("DefaultBranch = %q, want %q", got.DefaultBranch, test.want)
			}
			if got.PollInterval != from.PollInterval || got.Enabled != from.Enabled {
				t.Fatalf("operator-owned fields lost: %+v", got)
			}
		})
	}
}
