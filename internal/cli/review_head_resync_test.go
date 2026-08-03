package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// daemonWorkerHeadSHA reads the current HEAD sha of a git checkout for the review
// head-resync tests.
func daemonWorkerHeadSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func hasResyncEvent(events []db.JobEvent, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// #684 failure mode A (the reliability fix): a PR review job is pinned to the head
// SHA the branch had at ENQUEUE time; in an active dev loop the branch advances
// before the queued review runs, so the registered checkout sits on a NEWER head.
// The review must re-target the checkout's current head (what a human reviewer
// does) instead of failing on the mismatch — as long as the PR is still OPEN.
func TestDefaultCheckoutResyncsReviewHeadWhenPRIsOpen(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "feat/x")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	// The head the review was pinned to at enqueue (the branch's original tip).
	staleHead := daemonWorkerHeadSHA(t, checkout)

	// The branch advances: a newer commit is pushed before the review runs.
	if err := os.WriteFile(checkout+"/feature.txt", []byte("more work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "feature.txt")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "advance the branch")
	newHead := daemonWorkerHeadSHA(t, checkout)
	if newHead == staleHead {
		t.Fatal("test setup: branch head did not advance")
	}

	// The PR is still OPEN.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo",
		Number:       23,
		HeadBranch:   "feat/x",
		HeadSHA:      staleHead,
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}

	// A queued review job pinned to the now-stale head.
	if _, err := (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{
		ID:          "workflow-review-1",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "feat/x",
		PullRequest: 23,
		HeadSHA:     staleHead,
		TaskID:      "review-task-1", // no task row → resolves the shared checkout
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "workflow-review-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}

	got, err := worker.defaultCheckout(ctx, job, payload, runtime.Agent{Name: "reviewer"})
	if err != nil {
		t.Fatalf("defaultCheckout failed the stale-head review instead of re-syncing: %v", err)
	}
	if got != checkout {
		t.Fatalf("defaultCheckout = %q, want shared checkout %q", got, checkout)
	}

	// The job payload is re-targeted to the checkout's CURRENT head so RunJob (which
	// re-reads the payload) delivers a review of the newest commit.
	reloaded, err := store.GetJob(ctx, "workflow-review-1")
	if err != nil {
		t.Fatalf("GetJob (reload) returned error: %v", err)
	}
	reloadedPayload, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload (reload) returned error: %v", err)
	}
	if reloadedPayload.HeadSHA != newHead {
		t.Fatalf("re-synced HeadSHA = %q, want current head %q (was %q)", reloadedPayload.HeadSHA, newHead, staleHead)
	}
	events, err := store.ListJobEvents(ctx, "workflow-review-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasResyncEvent(events, "review_head_resynced") {
		t.Fatalf("events = %+v, want a review_head_resynced event", events)
	}
}

// #684 mode A boundary: a head-SHA mismatch on a CLOSED (or merged) PR must NOT
// re-sync — a stale review of a dead PR is not useful, so the job keeps the
// existing terminal path and fails cleanly on the mismatch.
func TestDefaultCheckoutFailsReviewHeadMismatchWhenPRClosed(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "feat/x")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	staleHead := daemonWorkerHeadSHA(t, checkout)
	if err := os.WriteFile(checkout+"/feature.txt", []byte("more work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "feature.txt")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "advance the branch")

	// The PR is CLOSED.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo",
		Number:       23,
		HeadBranch:   "feat/x",
		HeadSHA:      staleHead,
		State:        "closed",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}

	if _, err := (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{
		ID:          "workflow-review-2",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "feat/x",
		PullRequest: 23,
		HeadSHA:     staleHead,
		TaskID:      "review-task-2",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "workflow-review-2")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}

	_, err = worker.defaultCheckout(ctx, job, payload, runtime.Agent{Name: "reviewer"})
	if err == nil {
		t.Fatal("defaultCheckout re-synced a review whose PR is closed; want a clean head-mismatch failure")
	}
	if !strings.Contains(err.Error(), "not review job head") {
		t.Fatalf("expected the review head-mismatch error, got: %v", err)
	}

	// Payload head is unchanged, and no re-sync event was recorded.
	reloaded, err := store.GetJob(ctx, "workflow-review-2")
	if err != nil {
		t.Fatalf("GetJob (reload) returned error: %v", err)
	}
	reloadedPayload, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload (reload) returned error: %v", err)
	}
	if reloadedPayload.HeadSHA != staleHead {
		t.Fatalf("closed-PR HeadSHA = %q, want it left unchanged at %q", reloadedPayload.HeadSHA, staleHead)
	}
	events, err := store.ListJobEvents(ctx, "workflow-review-2")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if hasResyncEvent(events, "review_head_resynced") {
		t.Fatalf("events = %+v, want NO review_head_resynced event for a closed PR", events)
	}
}

// #684 mode A safety guard: when a review falls back to the registered SHARED
// checkout (which sits on `main`, not the PR branch — e.g. the task worktree was
// cleaned up while a stale review was still queued), it must NOT be re-synced to
// main's head. Re-targeting there would review main's tree (none of the PR's
// changes) and could post an approval against a SHA that is not the PR head. The
// branch mismatch (checkout on `main`, review pinned to `feat/x`) makes resync
// decline, so the job keeps the existing head-mismatch failure.
func TestDefaultCheckoutDeclinesResyncWhenCheckoutOnWrongBranch(t *testing.T) {
	ctx := context.Background()
	// The shared checkout sits on `main`, NOT the PR's head branch.
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	// The head the review was pinned to (the PR branch's tip) — differs from main's
	// head, so validateReviewCheckout raises the head-SHA mismatch.
	staleHead := daemonWorkerHeadSHA(t, checkout)
	if err := os.WriteFile(checkout+"/main.txt", []byte("main advances\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "main.txt")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "advance main")
	mainHead := daemonWorkerHeadSHA(t, checkout)
	if mainHead == staleHead {
		t.Fatal("test setup: main head did not advance")
	}

	// The PR is OPEN on branch feat/x — the store would otherwise green-light a resync.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo",
		Number:       23,
		HeadBranch:   "feat/x",
		HeadSHA:      staleHead,
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}

	// The queued review is pinned to the feat/x branch + its stale head, but its task
	// worktree is gone so it resolves the shared checkout (which is on main).
	if _, err := (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{
		ID:          "workflow-review-3",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "feat/x",
		PullRequest: 23,
		HeadSHA:     staleHead,
		TaskID:      "review-task-3", // no task row → resolves the shared checkout
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "workflow-review-3")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}

	_, err = worker.defaultCheckout(ctx, job, payload, runtime.Agent{Name: "reviewer"})
	if err == nil {
		t.Fatal("defaultCheckout re-synced a review to a checkout on the wrong branch; want a clean head-mismatch failure")
	}
	if !strings.Contains(err.Error(), "not review job head") {
		t.Fatalf("expected the review head-mismatch error, got: %v", err)
	}

	// The payload head is left unchanged (NOT re-targeted to main's head) and no
	// re-sync event was recorded.
	reloaded, err := store.GetJob(ctx, "workflow-review-3")
	if err != nil {
		t.Fatalf("GetJob (reload) returned error: %v", err)
	}
	reloadedPayload, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload (reload) returned error: %v", err)
	}
	if reloadedPayload.HeadSHA != staleHead {
		t.Fatalf("wrong-branch HeadSHA = %q, want it left unchanged at %q (not main's head %q)", reloadedPayload.HeadSHA, staleHead, mainHead)
	}
	events, err := store.ListJobEvents(ctx, "workflow-review-3")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if hasResyncEvent(events, "review_head_resynced") {
		t.Fatalf("events = %+v, want NO review_head_resynced event for a wrong-branch checkout", events)
	}
}

// #684 failure mode B: a foreground review whose serialized runtime session is
// busy must be LEFT QUEUED for the daemon to run (a review is naturally
// asynchronous), not cancelled and dropped. Ask/implement keep their existing
// synchronous cancel behavior (covered by TestRunAgentAskCancelsQueuedJobWhenRuntimeSessionBusy).
func TestRunAgentReviewRequeuesQueuedJobWhenRuntimeSessionBusy(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	if err := os.WriteFile(repoDir+"/README.md", []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	t.Chdir(repoDir)
	head := daemonWorkerHeadSHA(t, repoDir)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "subscribe", "reviewer",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440042",
		"--role", "reviewer",
		"--repo", "owner/repo",
		"--capability", "review",
		"--capability", "implement",
		"--policy", "workspace-write",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe reviewer exit code = %d, stderr=%s", code, stderr.String())
	}

	store := openCLIJobStore(t, home)
	// A pre-seeded review task whose worktree is at the requested head lets the
	// review dispatch resolve locally without any GitHub call.
	if err := store.UpsertTask(context.Background(), db.Task{
		ID:           "review-task",
		RepoFullName: "owner/repo",
		GoalID:       "local-review",
		Title:        "Review PR #1",
		State:        string(workflow.TaskReviewing),
		Branch:       "feat/x",
		WorktreePath: repoDir,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	// The runtime session is busy (held by another owner).
	if acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: "runtime:codex:550e8400-e29b-41d4-a716-446655440042",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	store.Close()

	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{
		"agent", "review", "reviewer", "Please review",
		"--home", home,
		"--repo", "owner/repo",
		"--pr", "1",
		"--head-sha", head,
		"--branch", "feat/x",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("busy review exit code = %d, want 0 (requeued, not dropped); stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime calls = %+v, want none (job left queued, not run)", runner.calls)
	}
	// No daemon is running in this test, so the foreground caller MUST be told the
	// review is only queued and will not run until a daemon picks it up — otherwise a
	// bare "state: queued" reads as "the review ran" and the review silently never
	// executes.
	if out := stdout.String(); !strings.Contains(out, "daemon is not running") || !strings.Contains(out, "job run") {
		t.Fatalf("foreground busy-review stdout missing daemon guidance; got:\n%s", out)
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want one queued review job", jobs)
	}
	if jobs[0].State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued (left for the daemon)", jobs[0].State)
	}
	events, err := store.ListJobEvents(context.Background(), jobs[0].ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasResyncEvent(events, "requeued_runtime_busy") || !hasResyncEvent(events, "runtime_lock_wait") {
		t.Fatalf("events = %+v, want requeued_runtime_busy and runtime_lock_wait", events)
	}
	if hasResyncEvent(events, string(workflow.JobCancelled)) {
		t.Fatalf("events = %+v, review job must not be cancelled/dropped", events)
	}
}
