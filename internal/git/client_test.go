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

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

func TestClientUsesSharedSubprocessRunner(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{}, {Stdout: "task-1\n"}, {}, {Stdout: "/repo\n"}, {Stdout: "https://github.com/jerryfane/gitmoot.git\n"}, {}, {}, {}}}
	client := Client{Runner: runner, Dir: "/repo"}

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
	if remote != "https://github.com/jerryfane/gitmoot.git" {
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
			linked, err := (Client{Runner: runner, Dir: "/repo"}).IsLinkedWorktree(context.Background())
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
	primary, err := (Client{Runner: runner, Dir: "/repo-linked"}).PrimaryWorktree(context.Background())
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
	primary, err := (Client{Runner: runner, Dir: "/repo-linked"}).PrimaryWorktree(context.Background())
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
			if err := (Client{}).CreateBranch(context.Background(), branch, "main"); err == nil {
				t.Fatal("CreateBranch accepted unsafe branch")
			}
		})
	}
}

func TestClientWorktreeCommandConstruction(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{}, {}, {}, {}}}
	client := Client{Runner: runner, Dir: "/repo"}

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
	if err := (Client{}).AddWorktree(context.Background(), "bad branch", "/tmp/wt", "main"); err == nil {
		t.Fatal("AddWorktree accepted unsafe branch")
	}
	if err := (Client{}).AddExistingBranchWorktree(context.Background(), "bad branch", "/tmp/wt"); err == nil {
		t.Fatal("AddExistingBranchWorktree accepted unsafe branch")
	}
	if _, err := (Client{}).BranchExists(context.Background(), "bad branch"); err == nil {
		t.Fatal("BranchExists accepted unsafe branch")
	}
	if err := (Client{}).AddWorktree(context.Background(), "task-1", "", "main"); err == nil {
		t.Fatal("AddWorktree accepted empty path")
	}
	if err := (Client{}).AddExistingBranchWorktree(context.Background(), "task-1", " "); err == nil {
		t.Fatal("AddExistingBranchWorktree accepted empty path")
	}
	if err := (Client{}).RemoveWorktree(context.Background(), " "); err == nil {
		t.Fatal("RemoveWorktree accepted empty path")
	}
	if err := (Client{}).RemoveWorktreeForce(context.Background(), " "); err == nil {
		t.Fatal("RemoveWorktreeForce accepted empty path")
	}
	if err := (Client{}).AddDetachedWorktree(context.Background(), "", "main"); err == nil {
		t.Fatal("AddDetachedWorktree accepted empty path")
	}
	if err := (Client{}).AddDetachedWorktree(context.Background(), "/tmp/wt", " "); err == nil {
		t.Fatal("AddDetachedWorktree accepted empty ref")
	}
	if err := (Client{}).AddDetachedWorktree(context.Background(), "/tmp/wt", "-bad"); err == nil {
		t.Fatal("AddDetachedWorktree accepted ref starting with '-'")
	}
}

func TestClientDetachedAndForceRemoveCommandConstruction(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{}, {}}}
	client := Client{Runner: runner, Dir: "/repo"}
	if err := client.AddDetachedWorktree(context.Background(), "/worktrees/d1", "main"); err != nil {
		t.Fatalf("AddDetachedWorktree returned error: %v", err)
	}
	if err := client.RemoveWorktreeForce(context.Background(), "/worktrees/d1"); err != nil {
		t.Fatalf("RemoveWorktreeForce returned error: %v", err)
	}
	runner.wantArgs(t, 0, "git", "worktree", "add", "--detach", "/worktrees/d1", "main")
	runner.wantArgs(t, 1, "git", "worktree", "remove", "--force", "/worktrees/d1")
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

	client := Client{Dir: dir}
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
	client := Client{Runner: runner, Dir: "/repo"}
	if err := client.MergeBranches(context.Background(), "/wt/integration", []string{"legA", "legB"}, "integrate"); err != nil {
		t.Fatalf("MergeBranches returned error: %v", err)
	}
	runner.wantArgs(t, 0, "git", "merge", "--no-edit", "-m", "integrate", "legA")
	runner.wantArgs(t, 1, "git", "merge", "--no-edit", "-m", "integrate", "legB")
}

func TestClientMergeBranchesAbortsAndNamesConflictingBranch(t *testing.T) {
	runner := &fakeRunner{errs: []error{nil, errors.New("CONFLICT")}}
	client := Client{Runner: runner, Dir: "/repo"}
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
	if err := (Client{}).MergeBranches(context.Background(), " ", []string{"legA"}, "m"); err == nil {
		t.Fatal("MergeBranches accepted an empty dir")
	}
	if err := (Client{Runner: &fakeRunner{}}).MergeBranches(context.Background(), "/wt", []string{"bad branch"}, "m"); err == nil {
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

	client := Client{Dir: dir}
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

	client := Client{Dir: dir}
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
	if _, err := (Client{}).CommitWorktree(context.Background(), " ", "m"); err == nil {
		t.Fatal("CommitWorktree accepted an empty dir")
	}
}

func TestClientBranchExistsReturnsFalseForMissingBranch(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("exit status 1")}}
	exists, err := (Client{Runner: runner, Dir: "/repo"}).BranchExists(context.Background(), "missing")
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
	branches, err := (Client{Runner: runner, Dir: "/repo"}).RemoteBranches(context.Background(), []string{"feature/one", "feature/two"})
	if err != nil {
		t.Fatalf("RemoteBranches: %v", err)
	}
	if _, ok := branches["feature/one"]; !ok || len(branches) != 1 {
		t.Fatalf("branches = %v", branches)
	}
	runner.wantArgs(t, 0, "git", "ls-remote", "--heads", "origin", "refs/heads/feature/one", "refs/heads/feature/two")
	if _, err := (Client{}).RemoteBranches(context.Background(), []string{"bad branch"}); err == nil {
		t.Fatal("RemoteBranches accepted unsafe branch")
	}
}

func TestClientHeadSHA(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "abc123\n"}}}
	sha, err := (Client{Runner: runner, Dir: "/repo"}).HeadSHA(context.Background())
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if sha != "abc123" {
		t.Fatalf("sha = %q, want abc123", sha)
	}
	runner.wantArgs(t, 0, "git", "rev-parse", "HEAD")
}

func TestClientRevParse(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "def456\n"}}}
	sha, err := (Client{Runner: runner, Dir: "/repo"}).RevParse(context.Background(), "origin/main")
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
	if _, err := (Client{Runner: runner, Dir: "/repo"}).RevParse(context.Background(), "--upload-pack=evil"); err == nil {
		t.Fatal("RevParse accepted a rev starting with '-', want an error")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("RevParse invoked git for a rejected rev; calls=%v", runner.calls)
	}
}

func TestClientFetchRemote(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{}}}
	if err := (Client{Runner: runner, Dir: "/repo"}).FetchRemote(context.Background(), "origin"); err != nil {
		t.Fatalf("FetchRemote returned error: %v", err)
	}
	runner.wantArgs(t, 0, "git", "fetch", "origin")
	if err := (Client{}).FetchRemote(context.Background(), "-unsafe"); err == nil {
		t.Fatal("FetchRemote accepted an unsafe remote")
	}
}

func TestClientBehindCount(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: "12\n"}}}
	count, err := (Client{Runner: runner, Dir: "/repo"}).BehindCount(context.Background(), "origin/main")
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
	if _, err := (Client{Runner: runner, Dir: "/repo"}).BehindCount(context.Background(), "origin/main"); err == nil {
		t.Fatal("BehindCount accepted non-numeric output")
	}
	if _, err := (Client{}).BehindCount(context.Background(), "-unsafe"); err == nil {
		t.Fatal("BehindCount accepted an unsafe ref")
	}
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

	client := Client{Dir: dir}
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
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")

	client := Client{Dir: dir}
	clean, err := client.WorktreeClean(context.Background())
	if err != nil {
		t.Fatalf("WorktreeClean returned error: %v", err)
	}
	if !clean {
		t.Fatal("new repository should be clean")
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

	client := Client{Dir: dir}
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
