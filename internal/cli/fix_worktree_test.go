package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type fixWorktreeFixture struct {
	store      *db.Store
	rawHome    string
	home       string
	registered string
	remote     string
	branch     string
	head       string
}

func newFixWorktreeFixture(t *testing.T) fixWorktreeFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runFixGit(t, root, "init", "--bare", remote)
	registered := filepath.Join(root, "registered")
	if err := os.MkdirAll(registered, 0o755); err != nil {
		t.Fatal(err)
	}
	runFixGit(t, registered, "init", "-b", "main")
	runFixGit(t, registered, "config", "user.email", "gitmoot@example.com")
	runFixGit(t, registered, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(registered, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runFixGit(t, registered, "add", "README.md")
	runFixGit(t, registered, "commit", "-m", "base")
	branch := "feature/fix-round"
	runFixGit(t, registered, "switch", "-c", branch)
	forgeURL := "https://github.com/owner/repo.git"
	runFixGit(t, registered, "remote", "add", "origin", forgeURL)
	runFixGit(t, registered, "config", "url."+remote+".insteadOf", forgeURL)
	runFixGit(t, registered, "push", "-u", "origin", branch)
	head := strings.TrimSpace(runFixGit(t, registered, "rev-parse", "HEAD"))
	rawHome := filepath.Join(root, "home")
	paths := config.PathsForHome(rawHome)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedDaemonWorkerRepo(t, store, "owner/repo", registered)
	if err := store.UpsertTask(ctx, db.Task{ID: "review-pr-7", RepoFullName: "owner/repo", Branch: branch, State: string(workflow.TaskChangesRequested)}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	return fixWorktreeFixture{store: store, rawHome: rawHome, home: paths.Home, registered: registered, remote: remote, branch: branch, head: head}
}

func (f fixWorktreeFixture) allocate(t *testing.T, jobID string) workflow.FixWorktreeAllocation {
	t.Helper()
	allocation, err := allocateFixWorktree(context.Background(), f.store, f.home, f.registered, workflow.FixWorktreeRequest{
		JobID: jobID, Repo: "owner/repo", Branch: f.branch,
	})
	if err != nil {
		t.Fatalf("allocateFixWorktree: %v", err)
	}
	return allocation
}

// #1523 (review follow-up): the finalizer's branch resolution in
// implementationFinalizationTargetFor unconditionally overrides the delivery
// branch with payload.Branch for a FixWorktree job. That override is only safe
// because this producer-side guard hard-errors on a blank branch BEFORE
// dispatchFix ever sets FixWorktree=true — otherwise an empty payload.Branch
// would clobber a valid task.Branch and newly refuse work that previously
// succeeded. This test asserts the rejection by its observable behaviour so
// removing the guard breaks here instead of silently widening the predicate's
// exposure.
func TestAllocateFixWorktreeRejectsBlankBranch(t *testing.T) {
	store := daemonWorkerStore(t)
	for _, branch := range []string{"", "   "} {
		_, err := allocateFixWorktree(context.Background(), store, t.TempDir(), t.TempDir(), workflow.FixWorktreeRequest{
			JobID: "fix-blank-branch", Repo: "owner/repo", Branch: branch,
		})
		if err == nil || !strings.Contains(err.Error(), "fix worktree branch is required") {
			t.Fatalf("allocateFixWorktree with branch %q: err = %v, want \"fix worktree branch is required\"", branch, err)
		}
	}
}

func TestReviewFixRunsInPerJobBranchWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixWorktreeFixture(t)
	allocation := fixture.allocate(t, "fix-job")
	// Allocation needs a local transport in this hermetic fixture. Remove that
	// rewrite before worker delivery so both the owned checkout and the registered-
	// checkout mutant present the same literal GitHub origin to daemon preflight.
	runFixGit(t, fixture.registered, "config", "--unset-all", "url."+fixture.remote+".insteadOf")
	seedDaemonWorkerAgent(t, fixture.store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	if acquired, err := fixture.store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: fixture.branch, Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock = %v, %v", acquired, err)
	}
	enqueueDaemonWorkerJob(t, fixture.store, workflow.JobRequest{
		ID: "fix-job", Agent: "lead", Action: "implement", Repo: "owner/repo", Branch: fixture.branch,
		PullRequest: 7, HeadSHA: fixture.head, TaskID: "review-pr-7", LeadAgent: "lead",
		WorktreePath: allocation.Path, FixWorktree: true,
	})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"failed","summary":"stop after checkout observation","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	executionDir := ""
	executionBranch := ""
	var workerOutput bytes.Buffer
	worker := defaultJobWorker(fixture.store, &workerOutput, fixture.rawHome)
	worker.AdapterFactory = func(_ runtime.Agent, checkout string) (workflow.DeliveryAdapter, error) {
		executionDir = checkout
		executionBranch, _ = (gitutil.Client{Dir: checkout}).CurrentBranch(ctx)
		return adapter, nil
	}
	worker.CommenterFactory = func(string) github.Client { return github.NoopClient{} }
	job, err := fixture.store.GetJob(ctx, "fix-job")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	if sameCheckoutPath(executionDir, fixture.registered) {
		t.Fatalf("fix execution dir = registered checkout %s", executionDir)
	}
	if !sameCheckoutPath(executionDir, allocation.Path) {
		latest, _ := fixture.store.GetJob(ctx, "fix-job")
		events, _ := fixture.store.ListJobEvents(ctx, "fix-job")
		t.Fatalf("fix execution dir = %s, want per-job worktree %s; state=%s output=%q events=%+v", executionDir, allocation.Path, latest.State, workerOutput.String(), events)
	}
	if executionBranch != fixture.branch {
		t.Fatalf("fix execution branch = %q, want %q", executionBranch, fixture.branch)
	}
}

func TestReviewFixWorktreeCanCommitAndPushBranchHead(t *testing.T) {
	ctx := context.Background()
	fixture := newFixWorktreeFixture(t)
	allocation := fixture.allocate(t, "push-job")
	runFixGit(t, allocation.Path, "config", "user.email", "gitmoot@example.com")
	runFixGit(t, allocation.Path, "config", "user.name", "Gitmoot")
	runFixGit(t, allocation.Path, "config", "url."+fixture.remote+".insteadOf", "https://github.com/owner/repo.git")
	if err := os.WriteFile(filepath.Join(allocation.Path, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := gitutil.Client{Dir: allocation.Path}
	if err := git.CommitAll(ctx, "fix round"); err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	committedHead, err := git.HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if err := git.PushBranch(ctx, "origin", fixture.branch); err != nil {
		t.Fatalf("PushBranch: %v", err)
	}
	remoteHead := strings.TrimSpace(runFixGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.branch))
	if remoteHead != committedHead {
		t.Fatalf("remote head after push = %s, want committed fix head %s", remoteHead, committedHead)
	}
}

func runFixGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, output)
	}
	return fmt.Sprint(string(output))
}
