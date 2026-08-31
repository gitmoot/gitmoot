package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func TestClientUsesSharedSubprocessRunner(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{}, {Stdout: "task-1\n"}, {}, {Stdout: "/repo\n"}, {Stdout: "https://github.com/gitmoot/gitmoot.git\n"}, {}, {}, {}}}
	client := NewClient("/repo", runner)

	if err := client.CreateBranch(context.Background(), "task-1", "main"); err != nil {
		t.Fatalf("CreateBranch returned error: %v", err)
	}
	branch, err := client.CurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("CurrentBranch returned error: %v", err)
	}
	if branch != "task-1" {
		t.Fatalf("branch = %q, want task-1", branch)
	}
	if err := client.PushBranch(context.Background(), "origin", "task-1"); err != nil {
		t.Fatalf("PushBranch returned error: %v", err)
	}
	root, err := client.Root(context.Background())
	if err != nil {
		t.Fatalf("Root returned error: %v", err)
	}
	if root != "/repo" {
		t.Fatalf("root = %q, want /repo", root)
	}
	remote, err := client.OriginRemote(context.Background())
	if err != nil {
		t.Fatalf("OriginRemote returned error: %v", err)
	}
	if remote != "https://github.com/gitmoot/gitmoot.git" {
		t.Fatalf("remote = %q", remote)
	}
	clean, err := client.WorktreeClean(context.Background())
	if err != nil {
		t.Fatalf("WorktreeClean returned error: %v", err)
	}
	if !clean {
		t.Fatal("WorktreeClean reported dirty worktree")
	}
	if err := client.UpdateBase(context.Background(), "origin", "main"); err != nil {
		t.Fatalf("UpdateBase returned error: %v", err)
	}

	runner.wantArgs(t, 0, "git", "switch", "-c", "task-1", "main")
	runner.wantArgs(t, 1, "git", "branch", "--show-current")
	runner.wantArgs(t, 2, "git", "push", "-u", "origin", "task-1")
	runner.wantArgs(t, 3, "git", "rev-parse", "--show-toplevel")
	runner.wantArgs(t, 4, "git", "remote", "get-url", "origin")
	runner.wantArgs(t, 5, "git", "status", "--porcelain")
	runner.wantArgs(t, 6, "git", "fetch", "origin", "main")
	runner.wantArgs(t, 7, "git", "switch", "main")
	runner.wantArgs(t, 8, "git", "pull", "--ff-only", "origin", "main")
}

func TestClientRequiresSubprocessRunner(t *testing.T) {
	client := NewClient(t.TempDir(), nil)
	if _, err := client.HeadSHA(context.Background()); err == nil || !strings.Contains(err.Error(), "git subprocess runner is required") {
		t.Fatalf("HeadSHA error = %v, want required-runner refusal", err)
	}
}

func TestClientStatusPorcelainDisablesOptionalLocks(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: " M file.go\n"}}}
	status, err := (NewClient("/repo", runner)).StatusPorcelain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != "M file.go" {
		t.Fatalf("status = %q, want trimmed porcelain output", status)
	}
	runner.wantArgs(t, 0, "git", "--no-optional-locks", "status", "--porcelain")
}

func TestClientIsLinkedWorktree(t *testing.T) {
	tests := []struct {
		name       string
		results    []subprocess.Result
		errs       []error
		wantLinked bool
		wantCalls  [][]string
	}{
		{
			name:       "primary absolute paths match",
			results:    []subprocess.Result{{Stdout: "/repo/.git\n/repo/.git\n"}},
			wantLinked: false,
			wantCalls:  [][]string{{"git", "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir"}},
		},
		{
			name:       "linked absolute paths differ",
			results:    []subprocess.Result{{Stdout: "/repo/.git/worktrees/task\n/repo/.git\n"}},
			wantLinked: true,
			wantCalls:  [][]string{{"git", "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir"}},
		},
		{
			name:       "old git fallback resolves relative paths",
			results:    []subprocess.Result{{}, {Stdout: ".git\n.git\n"}},
			errs:       []error{errors.New("unknown option"), nil},
			wantLinked: false,
			wantCalls: [][]string{
				{"git", "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir"},
				{"git", "rev-parse", "--git-dir", "--git-common-dir"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{results: tc.results, errs: tc.errs}
			linked, err := (NewClient("/repo", runner)).IsLinkedWorktree(context.Background())
			if err != nil {
				t.Fatalf("IsLinkedWorktree returned error: %v", err)
			}
			if linked != tc.wantLinked {
				t.Fatalf("linked = %t, want %t", linked, tc.wantLinked)
			}
			for i, call := range tc.wantCalls {
				runner.wantArgs(t, i, call...)
			}
		})
	}
}

func TestClientPrimaryWorktree(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "worktree /repo\nHEAD abc\nbranch refs/heads/main\n\nworktree /repo-linked\nHEAD def\nbranch refs/heads/task\n"}}}
	primary, err := (NewClient("/repo-linked", runner)).PrimaryWorktree(context.Background())
	if err != nil {
		t.Fatalf("PrimaryWorktree returned error: %v", err)
	}
	if primary != "/repo" {
		t.Fatalf("primary = %q, want /repo", primary)
	}
	runner.wantArgs(t, 0, "git", "worktree", "list", "--porcelain")
}

func TestClientPrimaryWorktreeSkipsBareAndFallsBackToSelf(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{
		{Stdout: "worktree /repo.git\nbare\n"},
		{Stdout: "/repo-linked\n"},
	}}
	primary, err := (NewClient("/repo-linked", runner)).PrimaryWorktree(context.Background())
	if err != nil {
		t.Fatalf("PrimaryWorktree returned error: %v", err)
	}
	if primary != "/repo-linked" {
		t.Fatalf("primary = %q, want /repo-linked", primary)
	}
	runner.wantArgs(t, 0, "git", "worktree", "list", "--porcelain")
	runner.wantArgs(t, 1, "git", "rev-parse", "--show-toplevel")
}

func TestClientRejectsUnsafeBranchNames(t *testing.T) {
	for _, branch := range []string{"", " task", "task ", "-bad", "bad branch", "bad..branch", "bad.lock", "HEAD:main", "bad~branch", "bad^branch", "bad?branch", "bad[branch", "bad\\branch", "bad@{branch", "/bad", "bad/", "bad//branch"} {
		t.Run(branch, func(t *testing.T) {
			if err := (NewClient("", nil)).CreateBranch(context.Background(), branch, "main"); err == nil {
				t.Fatal("CreateBranch accepted unsafe branch")
			}
		})
	}
}

func TestClientWorktreeCommandConstruction(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{}, {}, {}, {}}}
	client := NewClient("/repo", runner)

	if err := client.AddWorktree(context.Background(), "task-1", "/worktrees/task-1", "main"); err != nil {
		t.Fatalf("AddWorktree returned error: %v", err)
	}
	exists, err := client.BranchExists(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("BranchExists returned error: %v", err)
	}
	if !exists {
		t.Fatal("BranchExists returned false after successful fake show-ref")
	}
	if err := client.AddExistingBranchWorktree(context.Background(), "task-1", "/worktrees/task-1-existing"); err != nil {
		t.Fatalf("AddExistingBranchWorktree returned error: %v", err)
	}
	if err := client.RemoveWorktree(context.Background(), "/worktrees/task-1"); err != nil {
		t.Fatalf("RemoveWorktree returned error: %v", err)
	}

	runner.wantArgs(t, 0, "git", "worktree", "add", "-b", "task-1", "/worktrees/task-1", "main")
	runner.wantArgs(t, 1, "git", "show-ref", "--verify", "--quiet", "refs/heads/task-1")
	runner.wantArgs(t, 2, "git", "worktree", "add", "/worktrees/task-1-existing", "task-1")
	runner.wantArgs(t, 3, "git", "worktree", "remove", "/worktrees/task-1")
}

func TestClientAddWorktreeRejectsInvalidInput(t *testing.T) {
	if err := (NewClient("", nil)).AddWorktree(context.Background(), "bad branch", "/tmp/wt", "main"); err == nil {
		t.Fatal("AddWorktree accepted unsafe branch")
	}
	if err := (NewClient("", nil)).AddExistingBranchWorktree(context.Background(), "bad branch", "/tmp/wt"); err == nil {
		t.Fatal("AddExistingBranchWorktree accepted unsafe branch")
	}
	if _, err := (NewClient("", nil)).BranchExists(context.Background(), "bad branch"); err == nil {
		t.Fatal("BranchExists accepted unsafe branch")
	}
	if err := (NewClient("", nil)).AddWorktree(context.Background(), "task-1", "", "main"); err == nil {
		t.Fatal("AddWorktree accepted empty path")
	}
	if err := (NewClient("", nil)).AddExistingBranchWorktree(context.Background(), "task-1", " "); err == nil {
		t.Fatal("AddExistingBranchWorktree accepted empty path")
	}
	if err := (NewClient("", nil)).RemoveWorktree(context.Background(), " "); err == nil {
		t.Fatal("RemoveWorktree accepted empty path")
	}
	if err := (NewClient("", nil)).RemoveWorktreeForce(context.Background(), " "); err == nil {
		t.Fatal("RemoveWorktreeForce accepted empty path")
	}
	if err := (NewClient("", nil)).AddDetachedWorktree(context.Background(), "", "main"); err == nil {
		t.Fatal("AddDetachedWorktree accepted empty path")
	}
	if err := (NewClient("", nil)).AddDetachedWorktree(context.Background(), "/tmp/wt", " "); err == nil {
		t.Fatal("AddDetachedWorktree accepted empty ref")
	}
	if err := (NewClient("", nil)).AddDetachedWorktree(context.Background(), "/tmp/wt", "-bad"); err == nil {
		t.Fatal("AddDetachedWorktree accepted ref starting with '-'")
	}
}

func TestClientDetachedAndForceRemoveCommandConstruction(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{}, {}, {}}}
	client := NewClient("/repo", runner)
	if err := client.AddDetachedWorktree(context.Background(), "/worktrees/d1", "main"); err != nil {
		t.Fatalf("AddDetachedWorktree returned error: %v", err)
	}
	if err := client.RemoveWorktreeForce(context.Background(), "/worktrees/d1"); err != nil {
		t.Fatalf("RemoveWorktreeForce returned error: %v", err)
	}
	if err := client.PruneWorktrees(context.Background()); err != nil {
		t.Fatalf("PruneWorktrees returned error: %v", err)
	}
	runner.wantArgs(t, 0, "git", "worktree", "add", "--detach", "/worktrees/d1", "main")
	runner.wantArgs(t, 1, "git", "worktree", "remove", "--force", "/worktrees/d1")
	runner.wantArgs(t, 2, "git", "worktree", "prune")
}

func TestClientRemoveWorktreeForceSmoke(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# smoke\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")

	client := NewHostClient(dir)
	wt := filepath.Join(t.TempDir(), "detached")
	if err := client.AddDetachedWorktree(context.Background(), wt, "HEAD"); err != nil {
		t.Fatalf("AddDetachedWorktree returned error: %v", err)
	}
	// A read-only runtime may leave untracked scratch files behind; plain remove
	// refuses, force remove disposes the throwaway worktree anyway.
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatalf("WriteFile scratch returned error: %v", err)
	}
	if err := client.RemoveWorktree(context.Background(), wt); err == nil {
		t.Fatal("RemoveWorktree unexpectedly removed a worktree with untracked files")
	}
	if err := client.RemoveWorktreeForce(context.Background(), wt); err != nil {
		t.Fatalf("RemoveWorktreeForce returned error: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still present after force remove: stat err = %v", err)
	}
}

func TestClientMergeBranchesCommandConstruction(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{}, {}}}
	client := NewClient("/repo", runner)
	if err := client.MergeBranches(context.Background(), "/wt/integration", []string{"legA", "legB"}, "integrate"); err != nil {
		t.Fatalf("MergeBranches returned error: %v", err)
	}
	runner.wantArgs(t, 0, "git", "merge", "--no-edit", "-m", "integrate", "legA")
	runner.wantArgs(t, 1, "git", "merge", "--no-edit", "-m", "integrate", "legB")
}

func TestClientMergeBranchesAbortsAndNamesConflictingBranch(t *testing.T) {
	runner := &fakeRunner{errs: []error{nil, errors.New("CONFLICT")}}
	client := NewClient("/repo", runner)
	err := client.MergeBranches(context.Background(), "/wt/integration", []string{"legA", "legB"}, "integrate")
	if err == nil {
		t.Fatal("expected error when a leg merge conflicts")
	}
	if !strings.Contains(err.Error(), "legB") {
		t.Fatalf("error must name the conflicting branch: %v", err)
	}
	last := runner.calls[len(runner.calls)-1]
	if len(last) < 3 || last[1] != "merge" || last[2] != "--abort" {
		t.Fatalf("expected a 'merge --abort' after conflict, got %v", last)
	}
	if err := (NewClient("", nil)).MergeBranches(context.Background(), " ", []string{"legA"}, "m"); err == nil {
		t.Fatal("MergeBranches accepted an empty dir")
	}
	if err := (NewClient("", &fakeRunner{})).MergeBranches(context.Background(), "/wt", []string{"bad branch"}, "m"); err == nil {
		t.Fatal("MergeBranches accepted an unsafe branch name")
	}
}

func TestClientMergeBranchesSmoke(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "base")
	// Two file-disjoint legs off main.
	runGit(t, dir, "checkout", "-b", "legA")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatalf("WriteFile a.txt returned error: %v", err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-m", "legA")
	runGit(t, dir, "checkout", "main")
	runGit(t, dir, "checkout", "-b", "legB")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatalf("WriteFile b.txt returned error: %v", err)
	}
	runGit(t, dir, "add", "b.txt")
	runGit(t, dir, "commit", "-m", "legB")
	runGit(t, dir, "checkout", "main")

	client := NewHostClient(dir)
	wt := filepath.Join(t.TempDir(), "integration")
	if err := client.AddDetachedWorktree(context.Background(), wt, "main"); err != nil {
		t.Fatalf("AddDetachedWorktree returned error: %v", err)
	}
	if err := client.MergeBranches(context.Background(), wt, []string{"legA", "legB"}, "integrate"); err != nil {
		t.Fatalf("MergeBranches returned error: %v", err)
	}
	// The integration worktree must now contain BOTH legs' files.
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(wt, f)); err != nil {
			t.Fatalf("%s missing from integration worktree after merge: %v", f, err)
		}
	}
}

func TestClientCommitWorktreeSmoke(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "base")

	client := NewHostClient(dir)
	// Clean worktree -> no commit.
	committed, err := client.CommitWorktree(context.Background(), dir, "noop")
	if err != nil {
		t.Fatalf("CommitWorktree(clean) returned error: %v", err)
	}
	if committed {
		t.Fatal("CommitWorktree reported a commit for a clean worktree")
	}
	// Edit -> commit.
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile feature.txt returned error: %v", err)
	}
	committed, err = client.CommitWorktree(context.Background(), dir, "add feature")
	if err != nil {
		t.Fatalf("CommitWorktree(edit) returned error: %v", err)
	}
	if !committed {
		t.Fatal("CommitWorktree did not commit a dirty worktree")
	}
	clean, err := client.WorktreeClean(context.Background())
	if err != nil {
		t.Fatalf("WorktreeClean returned error: %v", err)
	}
	if !clean {
		t.Fatal("worktree should be clean after CommitWorktree")
	}
	if _, err := (NewClient("", nil)).CommitWorktree(context.Background(), " ", "m"); err == nil {
		t.Fatal("CommitWorktree accepted an empty dir")
	}
}

func TestClientBranchExistsReturnsFalseForMissingBranch(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("exit status 1")}}
	exists, err := (NewClient("/repo", runner)).BranchExists(context.Background(), "missing")
	if err != nil {
		t.Fatalf("BranchExists returned error: %v", err)
	}
	if exists {
		t.Fatal("BranchExists returned true for missing branch")
	}
	runner.wantArgs(t, 0, "git", "show-ref", "--verify", "--quiet", "refs/heads/missing")
}

func TestClientRemoteBranchesBatchesExactRefs(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "abc\trefs/heads/feature/one\ndef\trefs/heads/unrequested\n"}}}
	branches, err := (NewClient("/repo", runner)).RemoteBranches(context.Background(), []string{"feature/one", "feature/two"})
	if err != nil {
		t.Fatalf("RemoteBranches: %v", err)
	}
	if _, ok := branches["feature/one"]; !ok || len(branches) != 1 {
		t.Fatalf("branches = %v", branches)
	}
	runner.wantArgs(t, 0, "git", "ls-remote", "--heads", "origin", "refs/heads/feature/one", "refs/heads/feature/two")
	if _, err := (NewClient("", nil)).RemoteBranches(context.Background(), []string{"bad branch"}); err == nil {
		t.Fatal("RemoteBranches accepted unsafe branch")
	}
}

func TestClientHeadSHA(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "abc123\n"}}}
	sha, err := (NewClient("/repo", runner)).HeadSHA(context.Background())
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if sha != "abc123" {
		t.Fatalf("sha = %q, want abc123", sha)
	}
	runner.wantArgs(t, 0, "git", "rev-parse", "HEAD")
}

func TestClientCloneOnlyCommitSeesEveryLocalRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	clone := filepath.Join(root, "fix")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "clone", remote, seed)
	runGit(t, seed, "config", "user.email", "gitmoot@example.com")
	runGit(t, seed, "config", "user.name", "Gitmoot")
	runGit(t, seed, "switch", "-c", "feature/fix")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	runGit(t, seed, "add", "base.txt")
	runGit(t, seed, "commit", "-m", "base")
	runGit(t, seed, "push", "-u", "origin", "feature/fix")
	runGit(t, root, "clone", remote, clone)
	runGit(t, clone, "switch", "feature/fix")
	runGit(t, clone, "config", "user.email", "gitmoot@example.com")
	runGit(t, clone, "config", "user.name", "Gitmoot")

	host := NewHostClient(seed)
	cloneClient := NewHostClient(clone)
	if err := host.RefreshCloneProofRefs(ctx, clone, remote); err != nil {
		t.Fatalf("RefreshCloneProofRefs: %v", err)
	}
	unpublished, err := host.CloneOnlyCommit(ctx, clone)
	if err != nil {
		t.Fatalf("CloneOnlyCommit on a fully pushed clone: %v", err)
	}
	if unpublished != "" {
		t.Fatalf("fully pushed clone reported clone-only commit %s", unpublished)
	}

	// A side branch is exactly what a HEAD-ancestry proof misses: status stays
	// empty, HEAD stays published, and RemoveAll would take the objects with it.
	runGit(t, clone, "switch", "-c", "scratch")
	runGit(t, clone, "commit", "--allow-empty", "-m", "clone only")
	scratch, err := cloneClient.HeadSHA(ctx)
	if err != nil {
		t.Fatalf("scratch HeadSHA: %v", err)
	}
	runGit(t, clone, "switch", "feature/fix")
	status, err := cloneClient.StatusPorcelain(ctx)
	if err != nil {
		t.Fatalf("StatusPorcelain: %v", err)
	}
	if status != "" {
		t.Fatalf("clone with a side branch is not status-clean: %q", status)
	}
	if err := host.RefreshCloneProofRefs(ctx, clone, remote); err != nil {
		t.Fatalf("RefreshCloneProofRefs after side branch: %v", err)
	}
	unpublished, err = host.CloneOnlyCommit(ctx, clone)
	if err != nil {
		t.Fatalf("CloneOnlyCommit with a side branch: %v", err)
	}
	if unpublished != scratch {
		t.Fatalf("clone-only commit = %q, want side-branch commit %s", unpublished, scratch)
	}

	// Publishing the side branch discharges it without any other change.
	runGit(t, clone, "push", "origin", "scratch")
	if err := host.RefreshCloneProofRefs(ctx, clone, remote); err != nil {
		t.Fatalf("RefreshCloneProofRefs after push: %v", err)
	}
	unpublished, err = host.CloneOnlyCommit(ctx, clone)
	if err != nil {
		t.Fatalf("CloneOnlyCommit after push: %v", err)
	}
	if unpublished != "" {
		t.Fatalf("pushed side branch still reported clone-only commit %s", unpublished)
	}

	// A stash holds commits that no ref reaches and that `git status` cannot see.
	if err := os.WriteFile(filepath.Join(clone, "base.txt"), []byte("stashed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile stashed: %v", err)
	}
	runGit(t, clone, "stash", "push", "-m", "clone only stash")
	status, err = cloneClient.StatusPorcelain(ctx)
	if err != nil {
		t.Fatalf("StatusPorcelain after stash: %v", err)
	}
	if status != "" {
		t.Fatalf("stashed clone is not status-clean: %q", status)
	}
	unpublished, err = host.CloneOnlyCommit(ctx, clone)
	if err != nil {
		t.Fatalf("CloneOnlyCommit with a stash: %v", err)
	}
	if unpublished == "" {
		t.Fatal("stashed clone-only commit reported as fully published")
	}
}

// The refresh fetches and prunes only heads/* and tags/* inside the proof
// namespace, so the proof must exclude exactly those. A wider glob would let any
// other ref parked in the namespace act as an unprunable exclusion tip that hides
// every unpublished commit behind it.
func TestClientCloneOnlyCommitIgnoresUnprunableNamespaceRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	clone := filepath.Join(root, "fix")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "clone", remote, seed)
	runGit(t, seed, "config", "user.email", "gitmoot@example.com")
	runGit(t, seed, "config", "user.name", "Gitmoot")
	runGit(t, seed, "switch", "-c", "feature/fix")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	runGit(t, seed, "add", "base.txt")
	runGit(t, seed, "commit", "-m", "base")
	runGit(t, seed, "push", "-u", "origin", "feature/fix")
	runGit(t, root, "clone", remote, clone)
	runGit(t, clone, "switch", "feature/fix")
	runGit(t, clone, "config", "user.email", "gitmoot@example.com")
	runGit(t, clone, "config", "user.name", "Gitmoot")
	runGit(t, clone, "commit", "--allow-empty", "-m", "clone only")
	cloneHead, err := NewHostClient(clone).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("clone HeadSHA: %v", err)
	}
	// Park the unpublished commit under the proof namespace, outside heads/tags.
	runGit(t, clone, "update-ref", "refs/remotes/gitmoot-reclaim-proof/evil", cloneHead)

	host := NewHostClient(seed)
	if err := host.RefreshCloneProofRefs(ctx, clone, remote); err != nil {
		t.Fatalf("RefreshCloneProofRefs: %v", err)
	}
	unpublished, err := host.CloneOnlyCommit(ctx, clone)
	if err != nil {
		t.Fatalf("CloneOnlyCommit: %v", err)
	}
	if unpublished != cloneHead {
		t.Fatalf("clone-only commit = %q, want %s despite the parked namespace ref", unpublished, cloneHead)
	}
}

// Grafts and replace refs are clone-local ancestry rewrites. If the proof honours
// them, a clone can attach an upstream tip to its own unpublished root and report
// that nothing is unpublished, which authorises deleting the only copy.
func TestClientCloneOnlyCommitIgnoresCloneLocalAncestryRewrites(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	clone := filepath.Join(root, "fix")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "clone", remote, seed)
	runGit(t, seed, "config", "user.email", "gitmoot@example.com")
	runGit(t, seed, "config", "user.name", "Gitmoot")
	runGit(t, seed, "switch", "-c", "feature/fix")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	runGit(t, seed, "add", "base.txt")
	runGit(t, seed, "commit", "-m", "base")
	runGit(t, seed, "push", "-u", "origin", "feature/fix")
	upstreamHead, err := NewHostClient(seed).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("upstream HeadSHA: %v", err)
	}
	runGit(t, root, "clone", remote, clone)
	runGit(t, clone, "switch", "feature/fix")
	runGit(t, clone, "config", "user.email", "gitmoot@example.com")
	runGit(t, clone, "config", "user.name", "Gitmoot")
	runGit(t, clone, "switch", "--orphan", "clone-only")
	runGit(t, clone, "commit", "--allow-empty", "-m", "clone only root")
	cloneOnlyRoot, err := NewHostClient(clone).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("clone-only HeadSHA: %v", err)
	}
	runGit(t, clone, "switch", "feature/fix")

	host := NewHostClient(seed)
	if err := host.RefreshCloneProofRefs(ctx, clone, remote); err != nil {
		t.Fatalf("RefreshCloneProofRefs: %v", err)
	}
	unpublished, err := host.CloneOnlyCommit(ctx, clone)
	if err != nil {
		t.Fatalf("CloneOnlyCommit: %v", err)
	}
	if unpublished != cloneOnlyRoot {
		t.Fatalf("clone-only commit = %q, want %s", unpublished, cloneOnlyRoot)
	}

	// A grafts file cannot be suppressed from the command line, so the proof must
	// refuse outright; a replace ref is disabled, and the replacement commit object
	// it creates is itself clone-only, so the proof must still name a commit.
	infoDir := filepath.Join(clone, ".git", "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll info: %v", err)
	}
	graftFile := filepath.Join(infoDir, "grafts")
	if err := os.WriteFile(graftFile, []byte(upstreamHead+" "+cloneOnlyRoot+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile grafts: %v", err)
	}
	if _, err := host.CloneOnlyCommit(ctx, clone); err == nil || !strings.Contains(err.Error(), "grafts file") {
		t.Fatalf("CloneOnlyCommit error = %v, want a refusal naming the grafts file", err)
	}
	if err := os.Remove(graftFile); err != nil {
		t.Fatalf("Remove grafts: %v", err)
	}

	runGit(t, clone, "replace", "--graft", upstreamHead, cloneOnlyRoot)
	unpublished, err = host.CloneOnlyCommit(ctx, clone)
	if err != nil {
		t.Fatalf("CloneOnlyCommit after replace: %v", err)
	}
	if unpublished == "" {
		t.Fatal("replace-graft ancestry hid every unpublished commit")
	}
}

// Git reads grafts from the COMMON directory, so a linked worktree's own admin
// directory is the wrong place to look: probing it would miss an active graft and
// let rewritten ancestry hide an unpublished commit.
func TestClientCloneOnlyCommitDetectsCommonDirGrafts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	main := filepath.Join(root, "main")
	linked := filepath.Join(root, "linked")
	runGit(t, root, "init", "-q", main)
	runGit(t, main, "config", "user.email", "gitmoot@example.com")
	runGit(t, main, "config", "user.name", "Gitmoot")
	runGit(t, main, "commit", "--allow-empty", "-m", "base")
	runGit(t, main, "worktree", "add", linked, "-b", "linked")
	infoDir := filepath.Join(main, ".git", "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll info: %v", err)
	}
	head, err := NewHostClient(main).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "grafts"), []byte(head+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile grafts: %v", err)
	}
	if _, err := NewHostClient(main).CloneOnlyCommit(ctx, linked); err == nil || !strings.Contains(err.Error(), "grafts file") {
		t.Fatalf("CloneOnlyCommit on a linked worktree = %v, want a refusal naming the common-directory grafts file", err)
	}
}

func TestClientCloneOnlyCommitDistrustsCloneOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	clone := filepath.Join(root, "fix")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "clone", remote, seed)
	runGit(t, seed, "config", "user.email", "gitmoot@example.com")
	runGit(t, seed, "config", "user.name", "Gitmoot")
	runGit(t, seed, "switch", "-c", "feature/fix")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	runGit(t, seed, "add", "base.txt")
	runGit(t, seed, "commit", "-m", "base")
	runGit(t, seed, "push", "-u", "origin", "feature/fix")
	runGit(t, root, "clone", remote, clone)
	runGit(t, clone, "switch", "feature/fix")
	runGit(t, clone, "config", "user.email", "gitmoot@example.com")
	runGit(t, clone, "config", "user.name", "Gitmoot")
	runGit(t, clone, "commit", "--allow-empty", "-m", "clone only")
	cloneHead, err := NewHostClient(clone).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("clone HeadSHA: %v", err)
	}
	// Whatever ran in the clone can repoint its own origin at itself, which would
	// make every local commit look published to a proof that trusts clone config.
	runGit(t, clone, "remote", "set-url", "origin", clone)

	host := NewHostClient(seed)
	trusted, err := host.RemoteURL(ctx, "origin")
	if err != nil {
		t.Fatalf("RemoteURL: %v", err)
	}
	if trusted != remote {
		t.Fatalf("trusted remote url = %q, want %q", trusted, remote)
	}
	if err := host.RefreshCloneProofRefs(ctx, clone, trusted); err != nil {
		t.Fatalf("RefreshCloneProofRefs: %v", err)
	}
	unpublished, err := host.CloneOnlyCommit(ctx, clone)
	if err != nil {
		t.Fatalf("CloneOnlyCommit: %v", err)
	}
	if unpublished != cloneHead {
		t.Fatalf("clone-only commit = %q, want %s despite the rewritten origin", unpublished, cloneHead)
	}
}

func TestClientCloneOnlyCommitDischargesMergedDeletedBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	source := filepath.Join(root, "source")
	fixClone := filepath.Join(root, "fix")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "clone", remote, source)
	runGit(t, source, "config", "user.email", "gitmoot@example.com")
	runGit(t, source, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(source, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	runGit(t, source, "add", "base.txt")
	runGit(t, source, "commit", "-m", "base")
	runGit(t, source, "branch", "-M", "main")
	runGit(t, source, "push", "-u", "origin", "main")
	runGit(t, source, "switch", "-c", "feature/fix")
	if err := os.WriteFile(filepath.Join(source, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatalf("WriteFile fix: %v", err)
	}
	runGit(t, source, "add", "fix.txt")
	runGit(t, source, "commit", "-m", "fix")
	runGit(t, source, "push", "-u", "origin", "feature/fix")
	featureHead, err := NewHostClient(source).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("feature HeadSHA: %v", err)
	}

	sourceClient := NewHostClient(source)
	if err := sourceClient.CloneLocalNoCheckout(ctx, fixClone); err != nil {
		t.Fatalf("CloneLocalNoCheckout: %v", err)
	}
	fixClient := NewHostClient(fixClone)
	if err := fixClient.SetRemoteURL(ctx, "origin", remote); err != nil {
		t.Fatalf("SetRemoteURL: %v", err)
	}
	if err := fixClient.CheckoutBranchAt(ctx, "feature/fix", featureHead); err != nil {
		t.Fatalf("CheckoutBranchAt: %v", err)
	}

	// Merge the branch and delete it on origin, the state a reclaim candidate is
	// normally in: the commits survive on the default branch, so nothing is lost.
	runGit(t, source, "switch", "main")
	runGit(t, source, "merge", "--ff-only", "feature/fix")
	runGit(t, source, "push", "origin", "main")
	runGit(t, source, "push", "origin", "--delete", "feature/fix")
	runGit(t, source, "branch", "-D", "feature/fix")

	if err := sourceClient.RefreshCloneProofRefs(ctx, fixClone, remote); err != nil {
		t.Fatalf("RefreshCloneProofRefs: %v", err)
	}
	unpublished, err := sourceClient.CloneOnlyCommit(ctx, fixClone)
	if err != nil {
		t.Fatalf("CloneOnlyCommit: %v", err)
	}
	if unpublished != "" {
		t.Fatalf("merged-and-deleted branch reported clone-only commit %s", unpublished)
	}

	// A squash merge republishes the CONTENT under a new commit, so the branch
	// commits themselves remain clone-only and the clone is retained. This is the
	// documented conservative outcome, asserted so it cannot regress silently.
	runGit(t, source, "switch", "-c", "feature/squash")
	if err := os.WriteFile(filepath.Join(source, "squash.txt"), []byte("squash\n"), 0o644); err != nil {
		t.Fatalf("WriteFile squash: %v", err)
	}
	runGit(t, source, "add", "squash.txt")
	runGit(t, source, "commit", "-m", "squash candidate")
	squashHead, err := NewHostClient(source).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("squash HeadSHA: %v", err)
	}
	squashClone := filepath.Join(root, "squash-fix")
	if err := sourceClient.CloneLocalNoCheckout(ctx, squashClone); err != nil {
		t.Fatalf("CloneLocalNoCheckout squash: %v", err)
	}
	squashClient := NewHostClient(squashClone)
	if err := squashClient.SetRemoteURL(ctx, "origin", remote); err != nil {
		t.Fatalf("SetRemoteURL squash: %v", err)
	}
	if err := squashClient.CheckoutBranchAt(ctx, "feature/squash", squashHead); err != nil {
		t.Fatalf("CheckoutBranchAt squash: %v", err)
	}
	runGit(t, source, "switch", "main")
	runGit(t, source, "merge", "--squash", "feature/squash")
	runGit(t, source, "commit", "-m", "squashed fix")
	runGit(t, source, "push", "origin", "main")
	runGit(t, source, "branch", "-D", "feature/squash")
	if err := sourceClient.RefreshCloneProofRefs(ctx, squashClone, remote); err != nil {
		t.Fatalf("RefreshCloneProofRefs squash: %v", err)
	}
	unpublished, err = sourceClient.CloneOnlyCommit(ctx, squashClone)
	if err != nil {
		t.Fatalf("CloneOnlyCommit squash: %v", err)
	}
	if unpublished != squashHead {
		t.Fatalf("squash-merged clone-only commit = %q, want %s", unpublished, squashHead)
	}
}

func TestClientRevParse(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "def456\n"}}}
	sha, err := (NewClient("/repo", runner)).RevParse(context.Background(), "origin/main")
	if err != nil {
		t.Fatalf("RevParse returned error: %v", err)
	}
	if sha != "def456" {
		t.Fatalf("sha = %q, want def456", sha)
	}
	runner.wantArgs(t, 0, "git", "rev-parse", "origin/main")
}

// TestClientRevParseRejectsDashRev guards against argument injection: a rev
// starting with '-' would be parsed by git as a flag, so RevParse must reject it
// before ever invoking git (no runner call). Mirrors validateBranch's dash guard.
func TestClientRevParseRejectsDashRev(t *testing.T) {
	runner := &fakeRunner{}
	if _, err := (NewClient("/repo", runner)).RevParse(context.Background(), "--upload-pack=evil"); err == nil {
		t.Fatal("RevParse accepted a rev starting with '-', want an error")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("RevParse invoked git for a rejected rev; calls=%v", runner.calls)
	}
}

func TestClientFetchRemote(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{}}}
	if err := (NewClient("/repo", runner)).FetchRemote(context.Background(), "origin"); err != nil {
		t.Fatalf("FetchRemote returned error: %v", err)
	}
	runner.wantArgs(t, 0, "git", "fetch", "origin")
	if err := (NewClient("", nil)).FetchRemote(context.Background(), "-unsafe"); err == nil {
		t.Fatal("FetchRemote accepted an unsafe remote")
	}
}

func TestClientBehindCount(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "12\n"}}}
	count, err := (NewClient("/repo", runner)).BehindCount(context.Background(), "origin/main")
	if err != nil {
		t.Fatalf("BehindCount returned error: %v", err)
	}
	if count != 12 {
		t.Fatalf("behind count = %d, want 12", count)
	}
	runner.wantArgs(t, 0, "git", "rev-list", "--count", "HEAD..origin/main")
}

func TestClientBehindCountRejectsInvalidOutput(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "many\n"}}}
	if _, err := (NewClient("/repo", runner)).BehindCount(context.Background(), "origin/main"); err == nil {
		t.Fatal("BehindCount accepted non-numeric output")
	}
	if _, err := (NewClient("", nil)).BehindCount(context.Background(), "-unsafe"); err == nil {
		t.Fatal("BehindCount accepted an unsafe ref")
	}
}

func TestClientIsAncestor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "history.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	runGit(t, dir, "add", "history.txt")
	runGit(t, dir, "commit", "-m", "base")
	client := NewHostClient(dir)
	base, err := client.HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "history.txt"), []byte("base\nchild\n"), 0o644); err != nil {
		t.Fatalf("WriteFile child: %v", err)
	}
	runGit(t, dir, "commit", "-am", "child")
	child, err := client.HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA child: %v", err)
	}
	isAncestor, err := client.IsAncestor(ctx, base, child)
	if err != nil || !isAncestor {
		t.Fatalf("IsAncestor true case = %t, %v", isAncestor, err)
	}

	runGit(t, dir, "switch", "--detach", base)
	if err := os.WriteFile(filepath.Join(dir, "sibling.txt"), []byte("sibling\n"), 0o644); err != nil {
		t.Fatalf("WriteFile sibling: %v", err)
	}
	runGit(t, dir, "add", "sibling.txt")
	runGit(t, dir, "commit", "-m", "sibling")
	sibling, err := client.HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA sibling: %v", err)
	}
	isAncestor, err = client.IsAncestor(ctx, child, sibling)
	if err != nil || isAncestor {
		t.Fatalf("IsAncestor false case = %t, %v", isAncestor, err)
	}
	if _, err := client.IsAncestor(ctx, "missing-ref", sibling); err == nil {
		t.Fatal("IsAncestor accepted an invalid ref")
	}
}

func TestClientCommitExistsDistinguishesMissingObject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial")
	client := NewHostClient(dir)
	head, err := client.HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if present, err := client.CommitExists(ctx, head); err != nil || !present {
		t.Fatalf("CommitExists(present) = %t, %v", present, err)
	}
	if present, err := client.CommitExists(ctx, strings.Repeat("0", 40)); err != nil || present {
		t.Fatalf("CommitExists(missing) = %t, %v", present, err)
	}
}

func TestClientCommitExistsPropagatesRepositoryFailure(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("object database unavailable")}}
	present, err := (NewClient("/repo", runner)).CommitExists(context.Background(), "deadbeef")
	if err == nil || present {
		t.Fatalf("CommitExists(repository failure) = %t, %v", present, err)
	}
	runner.wantArgs(t, 0, "git", "rev-parse", "--verify", "--quiet", "deadbeef^{commit}")
}

func TestClientCreateBranchSmoke(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# smoke\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")

	client := NewHostClient(dir)
	if err := client.CreateBranch(context.Background(), "task-branch", "main"); err != nil {
		t.Fatalf("CreateBranch returned error: %v", err)
	}
	branch, err := client.CurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("CurrentBranch returned error: %v", err)
	}
	if branch != "task-branch" {
		t.Fatalf("branch = %q, want task-branch", branch)
	}
}

func TestClientWorktreePristineAtClassifiesMissingGitPointer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	path := t.TempDir()
	_, err := NewHostClient(path).WorktreePristineAt(context.Background(), path)
	var terminal terminalWorktreeRemovalError
	if !errors.As(err, &terminal) {
		t.Fatalf("WorktreePristineAt error = %v, want terminal worktree removal error", err)
	}
}

func TestClientWorktreePristineAtPreservesStandaloneCloneStatusError(t *testing.T) {
	path := t.TempDir()
	if err := os.Mkdir(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("Mkdir .git: %v", err)
	}
	cause := errors.New("transient git status failure")
	runner := &fakeRunner{errs: []error{cause}}
	_, err := NewClient(path, runner).WorktreePristineAt(context.Background(), path)
	if !errors.Is(err, cause) {
		t.Fatalf("WorktreePristineAt error = %v, want original status error", err)
	}
	var terminal terminalWorktreeRemovalError
	if errors.As(err, &terminal) {
		t.Fatalf("standalone clone status failure was classified terminal: %v", err)
	}
	runner.wantArgs(t, 0, "git", "status", "--porcelain", "--ignored")
}

func TestClientRemoveWorktreeKeepsVanishedPathRetryable(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gone")
	cause := errors.New("fatal: '" + path + "' is not a working tree")
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "fatal: is not a working tree"}},
		errs:    []error{cause},
	}
	err := NewClient(root, runner).RemoveWorktree(context.Background(), path)
	if !errors.Is(err, cause) {
		t.Fatalf("RemoveWorktree error = %v, want original removal error", err)
	}
	var terminal terminalWorktreeRemovalError
	if errors.As(err, &terminal) {
		t.Fatalf("vanished worktree path was classified terminal: %v", err)
	}
	runner.wantArgs(t, 0, "git", "worktree", "remove", path)
}

func TestClientWorktreeCleanSmoke(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# smoke\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.log\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .gitignore returned error: %v", err)
	}
	runGit(t, dir, "add", "README.md", ".gitignore")
	runGit(t, dir, "commit", "-m", "init")

	client := NewHostClient(dir)
	clean, err := client.WorktreeClean(context.Background())
	if err != nil {
		t.Fatalf("WorktreeClean returned error: %v", err)
	}
	if !clean {
		t.Fatal("new repository should be clean")
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.log"), []byte("local-only\n"), 0o644); err != nil {
		t.Fatalf("WriteFile ignored returned error: %v", err)
	}
	clean, err = client.WorktreeCleanAt(context.Background(), dir)
	if err != nil {
		t.Fatalf("ignored WorktreeCleanAt returned error: %v", err)
	}
	if !clean {
		t.Fatal("WorktreeCleanAt treated ignored content as ordinary dirtiness")
	}
	pristine, err := client.WorktreePristineAt(context.Background(), dir)
	if err != nil {
		t.Fatalf("WorktreePristineAt returned error: %v", err)
	}
	if pristine {
		t.Fatal("WorktreePristineAt did not report ignored content")
	}
	if err := os.Remove(filepath.Join(dir, "ignored.log")); err != nil {
		t.Fatalf("Remove ignored returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirty returned error: %v", err)
	}
	clean, err = client.WorktreeClean(context.Background())
	if err != nil {
		t.Fatalf("dirty WorktreeClean returned error: %v", err)
	}
	if clean {
		t.Fatal("WorktreeClean did not report untracked file")
	}
}

func TestClientAddExistingBranchWorktreeRefusesCheckedOutBranchSmoke(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# smoke\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")
	runGit(t, dir, "switch", "-c", "task-branch")

	client := NewHostClient(dir)
	err := client.AddExistingBranchWorktree(context.Background(), "task-branch", filepath.Join(dir, "task-worktree"))
	if err == nil {
		t.Fatal("AddExistingBranchWorktree allowed a branch already checked out in the main worktree")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}

type fakeRunner struct {
	results []subprocess.Result
	errs    []error
	calls   [][]string
}

func (f *fakeRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	call := append([]string{command}, args...)
	f.calls = append(f.calls, call)
	index := len(f.calls) - 1
	result := subprocess.Result{Command: command, Args: args}
	if index < len(f.results) {
		result = f.results[index]
		result.Command = command
		result.Args = args
	}
	var err error
	if index < len(f.errs) {
		err = f.errs[index]
	}
	return result, err
}

func (f *fakeRunner) LookPath(string) (string, error) {
	return "", errors.New("not implemented")
}

func (f *fakeRunner) wantArgs(t *testing.T, index int, want ...string) {
	t.Helper()
	if index >= len(f.calls) {
		t.Fatalf("missing call %d; calls=%v", index, f.calls)
	}
	if !reflect.DeepEqual(f.calls[index], want) {
		t.Fatalf("call %d = %v, want %v", index, f.calls[index], want)
	}
}
