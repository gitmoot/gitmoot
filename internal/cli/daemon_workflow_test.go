package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRefreshDaemonJobPayloadPreservesTaskWorktreeHeadForFinalizer(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	checkout := filepath.Join(home, "repo")
	worktree := filepath.Join(home, "task-1")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, checkout)
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "push", "-u", "origin", "main")
	runGit(t, checkout, "worktree", "add", "-b", "task-1", worktree, "main")
	oldHead, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}

	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "unused", []string{"review"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: worktree}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: "owner/repo", Number: 12, URL: "https://github.com/owner/repo/pull/12", HeadBranch: "task-1", BaseBranch: "main", HeadSHA: oldHead, State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("self committed\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	runGit(t, worktree, "add", "feature.txt")
	runGit(t, worktree, "commit", "-m", "agent self commit")
	newHead, err := (gitutil.Client{Dir: worktree}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if newHead == oldHead {
		t.Fatal("worker did not create a new commit")
	}
	payload := workflow.JobPayload{Repo: "owner/repo", Branch: "task-1", PullRequest: 12, HeadSHA: oldHead, GoalID: "goal-1", TaskID: "task-1", TaskTitle: "Task 1", Result: &workflow.AgentResult{Decision: "implemented", Summary: "done"}}
	refreshed, err := refreshDaemonJobPayload(ctx, store, worktree, db.Job{ID: "job-implement-worktree", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("refreshDaemonJobPayload returned error: %v", err)
	}
	if refreshed.HeadSHA != oldHead {
		t.Fatalf("refreshed head = %q, want original %q", refreshed.HeadSHA, oldHead)
	}
	finalized, err := (daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}).FinalizeImplementation(ctx, db.Job{ID: "job-implement-worktree", Agent: "lead", Type: "implement"}, refreshed)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	if finalized.HeadSHA != newHead {
		t.Fatalf("finalized head = %q, want %q", finalized.HeadSHA, newHead)
	}
	remoteHead := strings.TrimSpace(runGitOutput(t, checkout, "ls-remote", "origin", "refs/heads/task-1"))
	if !strings.Contains(remoteHead, newHead) {
		t.Fatalf("remote task branch %q does not contain local head %q", remoteHead, newHead)
	}
}

func TestDaemonImplementationFinalizerCommitsBeforeReusingExistingPullRequest(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: "owner/repo", Number: 12, URL: "https://github.com/owner/repo/pull/12", HeadBranch: "task-1", BaseBranch: "main", HeadSHA: "old", State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}
	payload := workflow.JobPayload{
		Repo:      "owner/repo",
		Branch:    "task-1",
		GoalID:    "goal-1",
		TaskID:    "task-1",
		TaskTitle: "Task 1",
		LeadAgent: "lead",
		Result:    &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	finalized, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}

	if finalized.PullRequest != 12 {
		t.Fatalf("pull request = %d, want 12", finalized.PullRequest)
	}
	clean, err := (gitutil.Client{Dir: repoDir}).WorktreeClean(ctx)
	if err != nil {
		t.Fatalf("WorktreeClean returned error: %v", err)
	}
	if !clean {
		t.Fatal("worktree still has uncommitted changes")
	}
	localHead, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if finalized.HeadSHA != localHead {
		t.Fatalf("finalized head = %q, want %q", finalized.HeadSHA, localHead)
	}
	remoteHead := strings.TrimSpace(runGitOutput(t, repoDir, "ls-remote", "origin", "refs/heads/task-1"))
	if !strings.Contains(remoteHead, localHead) {
		t.Fatalf("remote task branch %q does not contain local head %q", remoteHead, localHead)
	}
}

// With no local PR record, the finalizer must adopt an out-of-band/concurrent
// open PR via EnsurePullRequest instead of erroring with a BlockedError — the
// #387 idempotency fix.
func TestDaemonImplementationFinalizerAdoptsExistingPullRequestViaEnsure(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	// No local PR record: the local fast-path misses, so the github-side ensure runs.
	gh := &stubEnsureGitHub{existing: github.PullRequest{
		Number:  99,
		URL:     "https://github.com/owner/repo/pull/99",
		State:   "open",
		HeadRef: "task-1",
		BaseRef: "main",
	}}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: gh}
	payload := workflow.JobPayload{
		Repo:      "owner/repo",
		Branch:    "task-1",
		GoalID:    "goal-1",
		TaskID:    "task-1",
		TaskTitle: "Task 1",
		LeadAgent: "lead",
		Result:    &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	finalized, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	var blocked workflow.BlockedError
	if errors.As(err, &blocked) {
		t.Fatalf("FinalizeImplementation returned BlockedError on an existing PR: %v", err)
	}
	if finalized.PullRequest != 99 {
		t.Fatalf("finalized pull request = %d, want 99 (adopted)", finalized.PullRequest)
	}
	if gh.ensureCalls != 1 {
		t.Fatalf("EnsurePullRequest calls = %d, want 1", gh.ensureCalls)
	}
	if gh.createCalls != 0 {
		t.Fatalf("CreatePullRequest calls = %d, want 0 (adopt, not create)", gh.createCalls)
	}
	// The adopted PR is persisted locally.
	stored, err := store.GetPullRequest(ctx, "owner/repo", 99)
	if err != nil {
		t.Fatalf("GetPullRequest(99) returned error: %v", err)
	}
	if stored.HeadBranch != "task-1" || !strings.EqualFold(stored.State, "open") {
		t.Fatalf("stored PR = %+v", stored)
	}
}

// #390: with --skip-native-review-fanout, the finalizer must persist the skip
// flag onto the branch lock BEFORE it opens the PR, so there is no TOCTOU window
// in which the daemon's PR-watcher sees the fresh PR with the flag unset.
func TestDaemonImplementationFinalizerPersistsSkipFanoutBeforeOpeningPR(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	// The branch lock exists from job start; the finalizer flips its flag.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	gh := &stubSkipFlagAtOpenGitHub{store: store, repo: "owner/repo", branch: "task-1", pr: github.PullRequest{
		Number:  7,
		URL:     "https://github.com/owner/repo/pull/7",
		State:   "open",
		HeadRef: "task-1",
		BaseRef: "main",
	}}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: gh}
	payload := workflow.JobPayload{
		Repo:                   "owner/repo",
		Branch:                 "task-1",
		GoalID:                 "goal-1",
		TaskID:                 "task-1",
		TaskTitle:              "Task 1",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: true,
		Result:                 &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	if _, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload); err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	if !gh.opened {
		t.Fatal("EnsurePullRequest was never called; cannot assert ordering")
	}
	if !gh.skipAtOpen {
		t.Fatal("#390 regression: branch-lock skip flag was NOT persisted before the PR was opened")
	}
	// And the flag is durably set after finalize.
	lock, err := store.GetBranchLock(ctx, "owner/repo", "task-1")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if !lock.SkipNativeReviewFanout {
		t.Fatalf("expected lock.SkipNativeReviewFanout = true after finalize, got %+v", lock)
	}
}

// #390 edge case: a re-finalize with no new changes and a PR already attached
// takes the early "no changes" return BEFORE any PR-open path. The skip flag must
// still be persisted on that path (it is written-ahead as soon as the branch is
// confirmed), or a retry could leave the flag unset and reintroduce the TOCTOU.
func TestDaemonImplementationFinalizerPersistsSkipFanoutOnNoChangeReFinalize(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	runGit(t, repoDir, "add", "feature.txt")
	runGit(t, repoDir, "commit", "-m", "work")
	head, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-1", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	// Clean worktree + PR already attached + head unchanged ⇒ the no-changes early
	// return. NoopClient: no PR-open call is expected on this path.
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}
	payload := workflow.JobPayload{
		Repo:                   "owner/repo",
		Branch:                 "task-1",
		PullRequest:            7,
		HeadSHA:                head,
		GoalID:                 "goal-1",
		TaskID:                 "task-1",
		TaskTitle:              "Task 1",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: true,
		Result:                 &workflow.AgentResult{Decision: "implemented", Summary: "no new changes"},
	}

	finalized, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	if finalized.PullRequest != 7 {
		t.Fatalf("finalized.PullRequest = %d, want 7 (unchanged on no-change path)", finalized.PullRequest)
	}
	lock, err := store.GetBranchLock(ctx, "owner/repo", "task-1")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if !lock.SkipNativeReviewFanout {
		t.Fatal("#390 regression: skip flag NOT persisted on the no-changes early-return path")
	}
}

func TestDaemonImplementationFinalizerPushesAlreadyCommittedWork(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	oldHead, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("already committed\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	runGit(t, repoDir, "add", "feature.txt")
	runGit(t, repoDir, "commit", "-m", "agent commit")

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: "owner/repo", Number: 12, URL: "https://github.com/owner/repo/pull/12", HeadBranch: "task-1", BaseBranch: "main", HeadSHA: oldHead, State: "open"}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}
	payload := workflow.JobPayload{
		Repo:      "owner/repo",
		Branch:    "task-1",
		HeadSHA:   oldHead,
		GoalID:    "goal-1",
		TaskID:    "task-1",
		TaskTitle: "Task 1",
		LeadAgent: "lead",
		Result:    &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	finalized, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	localHead, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}
	if finalized.HeadSHA != localHead || finalized.HeadSHA == oldHead {
		t.Fatalf("finalized head = %q, local=%q old=%q", finalized.HeadSHA, localHead, oldHead)
	}
	remoteHead := strings.TrimSpace(runGitOutput(t, repoDir, "ls-remote", "origin", "refs/heads/task-1"))
	if !strings.Contains(remoteHead, localHead) {
		t.Fatalf("remote task branch %q does not contain local head %q", remoteHead, localHead)
	}
}

func TestDaemonImplementationFinalizerBlocksWrongBranch(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "wrong-branch")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("wrong branch work\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}
	payload := workflow.JobPayload{
		Repo:      "owner/repo",
		Branch:    "task-1",
		GoalID:    "goal-1",
		TaskID:    "task-1",
		TaskTitle: "Task 1",
		LeadAgent: "lead",
		Result:    &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	_, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	var blocked workflow.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("FinalizeImplementation error = %v, want BlockedError", err)
	}
	if !strings.Contains(blocked.Reason, "not task-1") {
		t.Fatalf("blocked reason = %q", blocked.Reason)
	}
}

func TestDaemonImplementationFinalizerAllowsAlreadyFinalizedPullRequest(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	remoteDir := filepath.Join(home, "remote.git")
	repoDir := filepath.Join(home, "repo")
	runGit(t, home, "init", "--bare", remoteDir)
	runGit(t, home, "clone", remoteDir, repoDir)
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "push", "-u", "origin", "main")
	runGit(t, repoDir, "switch", "-c", "task-1")
	head, err := (gitutil.Client{Dir: repoDir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA returned error: %v", err)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: repoDir, DefaultBranch: "main", PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1", Title: "Task 1", State: string(workflow.TaskImplementing), Branch: "task-1", WorktreePath: repoDir}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	finalizer := daemonImplementationFinalizer{Store: store, GitHub: github.NoopClient{}}
	payload := workflow.JobPayload{
		Repo:        "owner/repo",
		Branch:      "task-1",
		PullRequest: 12,
		HeadSHA:     head,
		GoalID:      "goal-1",
		TaskID:      "task-1",
		TaskTitle:   "Task 1",
		LeadAgent:   "lead",
		Result:      &workflow.AgentResult{Decision: "implemented", Summary: "done"},
	}

	finalized, err := finalizer.FinalizeImplementation(ctx, db.Job{ID: "job-1", Agent: "lead", Type: "implement"}, payload)
	if err != nil {
		t.Fatalf("FinalizeImplementation returned error: %v", err)
	}
	if finalized.HeadSHA != head || finalized.PullRequest != 12 || finalized.Branch != "task-1" {
		t.Fatalf("finalized payload = %+v", finalized)
	}
}

func TestNewDaemonPolicyMergeGateIncludesWorktreeCleaner(t *testing.T) {
	gate := newDaemonPolicyMergeGate(nil, github.NoopClient{}, "/tmp/gitmoot-checkout")

	if gate.Worktrees == nil {
		t.Fatal("daemon merge gate missing worktree cleaner")
	}
	if gate.CheckoutPath != "/tmp/gitmoot-checkout" {
		t.Fatalf("checkout path = %q", gate.CheckoutPath)
	}
	if !gate.DeleteBranch {
		t.Fatal("daemon merge gate should delete merged branches")
	}
}

func TestDaemonMergeGateCanBeDisabledByEnvironment(t *testing.T) {
	t.Setenv("GITMOOT_DISABLE_NATIVE_MERGE_GATE", "1")
	gate := daemonMergeGate{}

	decision, err := gate.Evaluate(context.Background(), workflow.MergeRequest{
		Repo:        "owner/repo",
		PullRequest: 1,
	})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready {
		t.Fatalf("decision.Ready = true, want disabled gate to block")
	}
	if !strings.Contains(decision.Reason, "GITMOOT_DISABLE_NATIVE_MERGE_GATE") {
		t.Fatalf("decision reason = %q", decision.Reason)
	}
}

func TestDaemonMergeGatePreservesInjectedGitHubClient(t *testing.T) {
	fake := github.NoopClient{}
	gate := daemonMergeGate{GitHub: fake}

	if got := gate.githubClient("/tmp/checkout"); got != fake {
		t.Fatalf("github client = %#v, want injected fake %#v", got, fake)
	}
}

func TestDaemonReviewersIgnoresTempWorkerInstances(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"review"}, "owner/repo")
	now := time.Now().UTC()
	if err := store.UpsertAgentInstance(ctx, db.AgentInstance{
		Name:           "audit-temp-job-a",
		Type:           tempWorkerAgentType("audit"),
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     "550e8400-e29b-41d4-a716-446655440555",
		RepoFullName:   "owner/repo",
		Role:           "reviewer",
		Capabilities:   []string{"review"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		State:          "idle",
		CreatedAt:      now.Format(time.RFC3339),
		LastUsedAt:     now.Format(time.RFC3339),
		ExpiresAt:      now.Add(time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpsertAgentInstance returned error: %v", err)
	}

	reviewers, err := daemonReviewers(ctx, store, "owner/repo")
	if err != nil {
		t.Fatalf("daemonReviewers returned error: %v", err)
	}

	if !reflect.DeepEqual(reviewers, []string{"audit"}) {
		t.Fatalf("reviewers = %+v, want only regular configured reviewer", reviewers)
	}
}

// TestBuildEscalationCommentIsNotParsedAsCommand pins the #340 regression where
// the escalation notification, posted by the daemon on its own PR, led a line
// with "@<handle> Gitmoot paused…" and so was parsed by ParseCommand as a
// "@<agent> <action=Gitmoot>" command — making the daemon reply with a spurious
// "unsupported command action 'Gitmoot'" ack. The body must @-mention the human
// (so GitHub still notifies) without any line that ParseCommand treats as a
// command.
func TestBuildEscalationCommentIsNotParsedAsCommand(t *testing.T) {
	req := workflow.EscalationRequest{
		CoordinatorJobID: "coord-123",
		DelegationID:     "failing-leg",
		Reason:           "child returned failed",
		Question:         "how should we proceed?",
	}
	body := buildEscalationComment("jerryfane", req)

	// The human is still mentioned (GitHub notification preserved)...
	if !strings.Contains(body, "@jerryfane") {
		t.Fatalf("escalation comment dropped the @-mention; body:\n%s", body)
	}
	// ...but never as the first token of a line (which ParseCommand would treat
	// as a `@<agent> <action>` command), and no line starts with a bare /gitmoot.
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "@") {
			t.Fatalf("escalation comment line starts with an @-mention (parses as a command): %q", line)
		}
		if fields[0] == "/gitmoot" {
			t.Fatalf("escalation comment line starts with a bare /gitmoot (parses as a command): %q", line)
		}
	}

	// And end-to-end: the daemon's own parser yields no command at all for the
	// notification body, so the daemon never acks it as an (un)routable command.
	if cmds := daemon.ParseCommands(body); len(cmds) != 0 {
		t.Fatalf("escalation comment parsed into %d command(s); want 0: %+v\nbody:\n%s", len(cmds), cmds, body)
	}
}

// TestBuildAskGateComment pins the #445 ask-gate notification: when the request
// is flagged Ask, the comment quotes each question (id + prompt + choices) and
// offers the `answer` resume verb, NOT the failure retry/continue/abort verbs.
// It must also be unparseable as a command (same regression guard as #340).
func TestBuildAskGateComment(t *testing.T) {
	req := workflow.EscalationRequest{
		CoordinatorJobID: "coord-123",
		Ask:              true,
		Questions: []workflow.HumanQuestion{
			{ID: "q1", Prompt: "Target v2 or v3 API?", Choices: []string{"v2", "v3"}},
		},
	}
	body := buildEscalationComment("jerryfane", req)

	if !strings.Contains(body, "@jerryfane") {
		t.Fatalf("ask-gate comment dropped the @-mention; body:\n%s", body)
	}
	if !strings.Contains(body, "q1") || !strings.Contains(body, "Target v2 or v3 API?") {
		t.Fatalf("ask-gate comment must quote the question; body:\n%s", body)
	}
	if !strings.Contains(body, "choices: v2, v3") {
		t.Fatalf("ask-gate comment must render choices; body:\n%s", body)
	}
	if !strings.Contains(body, "resume coord-123 answer") {
		t.Fatalf("ask-gate comment must offer the answer verb; body:\n%s", body)
	}
	if strings.Contains(body, "retry <instructions>") || strings.Contains(body, "abort` —") {
		t.Fatalf("ask-gate comment must NOT offer the failure verbs; body:\n%s", body)
	}
	// Same command-injection guard as the failure comment.
	if cmds := daemon.ParseCommands(body); len(cmds) != 0 {
		t.Fatalf("ask-gate comment parsed into %d command(s); want 0: %+v\nbody:\n%s", len(cmds), cmds, body)
	}
}
