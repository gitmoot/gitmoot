package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type diffBaseForgeStub struct{ pull github.PullRequest }

func (s diffBaseForgeStub) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	return s.pull, nil
}

func (s diffBaseForgeStub) ListPullRequestChecks(context.Context, github.Repository, int64) ([]github.PullRequestCheck, error) {
	return nil, nil
}

// A queued review resolves its sandbox scope from origin/<base>. When the
// checkout's remote-tracking ref lags the real base branch, the merge base is
// an older commit and the sandbox diff silently grows to include changes that
// are already merged — the failure observed on 2026-09-18, where a review of
// PR #2231 was handed twenty files from unrelated prior work.
func TestRemoteReviewDiffBaseHEADRefreshesStaleBaseRef(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	origin := createDaemonWorkerGitCheckout(t, "main")
	runDaemonWorkerGit(t, origin, "remote", "remove", "origin")

	checkout := t.TempDir()
	runDaemonWorkerGit(t, checkout, "clone", origin, checkout)
	runDaemonWorkerGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runDaemonWorkerGit(t, checkout, "config", "user.name", "Gitmoot")

	// The base branch advances after the clone: this commit is already merged
	// and must NOT appear in the review scope.
	writeReviewBaseFile(t, origin, "already-merged.txt", "merged before the review\n")
	runDaemonWorkerGit(t, origin, "add", "-A")
	runDaemonWorkerGit(t, origin, "commit", "-m", "merged base commit")
	freshBase := gitOutputForTest(t, origin, "rev-parse", "HEAD")

	// The PR head branches off the fresh base.
	runDaemonWorkerGit(t, origin, "switch", "-c", "review-head")
	writeReviewBaseFile(t, origin, "under-review.txt", "the actual review subject\n")
	runDaemonWorkerGit(t, origin, "add", "-A")
	runDaemonWorkerGit(t, origin, "commit", "-m", "review head commit")
	head := gitOutputForTest(t, origin, "rev-parse", "HEAD")
	runDaemonWorkerGit(t, origin, "switch", "main")

	// The daemon has the head but a stale origin/main, exactly as a checkout
	// that fetched the PR ref alone would.
	runDaemonWorkerGit(t, checkout, "fetch", "origin", "review-head")
	staleBase := gitOutputForTest(t, checkout, "rev-parse", "origin/main")
	if staleBase == freshBase {
		t.Fatal("test setup failed to produce a stale origin/main")
	}

	const repo = "owner/repo"
	seedDaemonWorkerRepo(t, store, repo, checkout)
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo, Number: 2231, URL: "https://github.com/owner/repo/pull/2231",
		HeadBranch: "review-head", BaseBranch: "main", HeadSHA: head, State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}

	payload, err := json.Marshal(workflow.JobPayload{
		Repo: repo, Branch: "review-head", PullRequest: 2231, HeadSHA: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard)
	job := db.Job{ID: "review-stale-base", Agent: "reviewer", Type: "review", Payload: string(payload)}

	base, err := worker.remoteReviewDiffBaseHEAD(ctx, job, checkout)
	if err != nil {
		t.Fatalf("remoteReviewDiffBaseHEAD: %v", err)
	}
	if base != freshBase {
		t.Fatalf("review diff base = %s, want the refreshed base %s (stale ref was %s)", base, freshBase, staleBase)
	}

	// The scope the sandbox will show must be the PR's own change alone.
	changed := strings.Fields(gitOutputForTest(t, checkout, "diff", "--name-only", base, head))
	if len(changed) != 1 || changed[0] != "under-review.txt" {
		t.Fatalf("review scope = %v, want only under-review.txt", changed)
	}
}

// The forge is authoritative for a PR that the local watcher has never cached.
// A missing row must not prevent a valid exact-head review from reaching the
// remote sandbox, nor substitute an unverified local branch name for its base.
func TestRemoteReviewDiffBaseHEADUsesForgeOnCacheMiss(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	origin := createDaemonWorkerGitCheckout(t, "main")
	runDaemonWorkerGit(t, origin, "remote", "remove", "origin")
	base := gitOutputForTest(t, origin, "rev-parse", "HEAD")
	runDaemonWorkerGit(t, origin, "switch", "-c", "review-head")
	writeReviewBaseFile(t, origin, "under-review.txt", "review subject\n")
	runDaemonWorkerGit(t, origin, "add", "-A")
	runDaemonWorkerGit(t, origin, "commit", "-m", "review head")
	head := gitOutputForTest(t, origin, "rev-parse", "HEAD")
	runDaemonWorkerGit(t, origin, "switch", "main")

	checkout := t.TempDir()
	runDaemonWorkerGit(t, checkout, "clone", origin, checkout)
	runDaemonWorkerGit(t, checkout, "fetch", "origin", "review-head")
	const repo = "owner/repo"
	seedDaemonWorkerRepo(t, store, repo, checkout)
	payload, err := json.Marshal(workflow.JobPayload{Repo: repo, Branch: "review-head", PullRequest: 2211, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.ReviewAdmissionGitHubFactory = func(string) remoteReviewAdmissionGitHub {
		return diffBaseForgeStub{pull: github.PullRequest{BaseRef: "main", HeadSHA: head}}
	}
	job := db.Job{ID: "review-uncached-pr", Agent: "reviewer", Type: "review", Payload: string(payload)}
	resolved, err := worker.remoteReviewDiffBaseHEAD(ctx, job, checkout)
	if err != nil {
		t.Fatalf("uncached review diff base: %v", err)
	}
	if resolved != base {
		t.Fatalf("uncached review diff base = %s, want %s", resolved, base)
	}
	changed := strings.Fields(gitOutputForTest(t, checkout, "diff", "--name-only", resolved, head))
	if len(changed) != 1 || changed[0] != "under-review.txt" {
		t.Fatalf("uncached review scope = %v, want only under-review.txt", changed)
	}
	worker.ReviewAdmissionGitHubFactory = func(string) remoteReviewAdmissionGitHub {
		return diffBaseForgeStub{pull: github.PullRequest{BaseRef: "main", HeadSHA: base}}
	}
	if _, err := worker.remoteReviewDiffBaseHEAD(ctx, job, checkout); err == nil || !strings.Contains(err.Error(), "head moved") {
		t.Fatalf("moved uncached PR head should be refused, got %v", err)
	}
}

// A re-review bounded to a prior head keeps that head, and never widens to the
// base branch.
func TestRemoteReviewDiffBaseHEADHonorsPriorReviewHead(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	priorHead := gitOutputForTest(t, checkout, "rev-parse", "HEAD")
	writeReviewBaseFile(t, checkout, "round-two.txt", "second round\n")
	runDaemonWorkerGit(t, checkout, "add", "-A")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "second round")
	head := gitOutputForTest(t, checkout, "rev-parse", "HEAD")

	const repo = "owner/repo"
	seedDaemonWorkerRepo(t, store, repo, checkout)
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: repo, Branch: "main", PullRequest: 7, HeadSHA: head,
		ReviewScope: &workflow.ReviewScope{PreviousHeadSHA: priorHead},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard)
	job := db.Job{ID: "review-round-two", Agent: "reviewer", Type: "review", Payload: string(payload)}

	base, err := worker.remoteReviewDiffBaseHEAD(ctx, job, checkout)
	if err != nil {
		t.Fatalf("remoteReviewDiffBaseHEAD: %v", err)
	}
	if base != priorHead {
		t.Fatalf("review diff base = %s, want prior head %s", base, priorHead)
	}
}

// A base that is not an ancestor of the head cannot describe the review, and
// is refused rather than handed to the sandbox as a nonsense diff.
func TestRemoteReviewDiffBaseHEADRefusesUnrelatedBase(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	head := gitOutputForTest(t, checkout, "rev-parse", "HEAD")
	runDaemonWorkerGit(t, checkout, "switch", "--orphan", "unrelated")
	writeReviewBaseFile(t, checkout, "unrelated.txt", "unrelated history\n")
	runDaemonWorkerGit(t, checkout, "add", "-A")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "unrelated root")
	unrelated := gitOutputForTest(t, checkout, "rev-parse", "HEAD")

	const repo = "owner/repo"
	seedDaemonWorkerRepo(t, store, repo, checkout)
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: repo, Branch: "main", PullRequest: 9, HeadSHA: head,
		ReviewScope: &workflow.ReviewScope{PreviousHeadSHA: unrelated},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard)
	job := db.Job{ID: "review-unrelated", Agent: "reviewer", Type: "review", Payload: string(payload)}

	if _, err := worker.remoteReviewDiffBaseHEAD(ctx, job, checkout); err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("remoteReviewDiffBaseHEAD error = %v, want an ancestry refusal", err)
	}
}

func gitOutputForTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeReviewBaseFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}
