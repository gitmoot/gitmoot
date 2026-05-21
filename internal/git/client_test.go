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

func TestClientRejectsUnsafeBranchNames(t *testing.T) {
	for _, branch := range []string{"", " task", "task ", "-bad", "bad branch", "bad..branch", "bad.lock", "HEAD:main", "bad~branch", "bad^branch", "bad?branch", "bad[branch", "bad\\branch", "bad@{branch", "/bad", "bad/", "bad//branch"} {
		t.Run(branch, func(t *testing.T) {
			if err := (Client{}).CreateBranch(context.Background(), branch, "main"); err == nil {
				t.Fatal("CreateBranch accepted unsafe branch")
			}
		})
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
