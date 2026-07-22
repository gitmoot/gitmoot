package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestValidateTargetCheckoutSkipsHeadShaForDelegationWorktreeChild(t *testing.T) {
	// A delegated implement child runs in its own freshly-allocated worktree whose
	// HEAD is the base-branch tip at allocation time; the dispatcher clears the
	// inherited parent HeadSHA. validateTargetCheckout must not reject such a child
	// even when its payload HeadSHA is empty or stale, as long as the worktree is
	// on the job branch and clean. A non-delegation job with a mismatched HeadSHA
	// must still be rejected.
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "task-005")
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)

	// Delegation child: empty HeadSHA + worktree path -> accepted.
	delegationPayload := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "task-005",
		DelegationID: "d1",
		ParentJobID:  "parent-job",
		WorktreePath: checkout,
	}
	if err := worker.validateTargetCheckout(ctx, delegationPayload, checkout); err != nil {
		t.Fatalf("validateTargetCheckout rejected delegation worktree child: %v", err)
	}

	// Delegation child with a stale (non-matching) HeadSHA -> still accepted,
	// because the equality check is skipped for delegation worktree children.
	stalePayload := delegationPayload
	stalePayload.HeadSHA = "0000000000000000000000000000000000000000"
	if err := worker.validateTargetCheckout(ctx, stalePayload, checkout); err != nil {
		t.Fatalf("validateTargetCheckout rejected delegation child with stale HeadSHA: %v", err)
	}

	// A non-delegation job with a mismatched HeadSHA must still be rejected so the
	// HeadSHA guard is not weakened for ordinary jobs.
	ordinaryPayload := workflow.JobPayload{
		Repo:    "owner/repo",
		Branch:  "task-005",
		HeadSHA: "0000000000000000000000000000000000000000",
	}
	if err := worker.validateTargetCheckout(ctx, ordinaryPayload, checkout); err == nil {
		t.Fatal("validateTargetCheckout accepted ordinary job with mismatched HeadSHA")
	}
}

func TestValidateTargetCheckoutAcceptsDetachedReadOnlyWorktreeChild(t *testing.T) {
	// A read-only delegation child runs in a *detached* worktree (no branch).
	// CurrentBranch errors on a detached HEAD, so validateTargetCheckout must
	// recognize the delegation worktree child and accept it on the clean-worktree
	// check alone rather than rejecting it on the branch check.
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "task-005")
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)

	detached := filepath.Join(t.TempDir(), "ro-worktree")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "--detach", detached, "HEAD")

	payload := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "task-005",
		DelegationID: "d1",
		ParentJobID:  "parent-job",
		WorktreePath: detached,
	}
	if err := worker.validateTargetCheckout(ctx, payload, detached); err != nil {
		t.Fatalf("validateTargetCheckout rejected detached read-only worktree child: %v", err)
	}
}

func TestValidateTargetCheckoutAllowsSharedCheckoutDelegationChildWithoutHeadSHA(t *testing.T) {
	// A read-only delegation child from a local orchestrate (no PR) inherits an
	// empty HeadSHA and runs in the shared checkout (no per-delegation worktree).
	// It must be accepted and run against the current HEAD, not rejected for a
	// missing HeadSHA. A non-delegation job with no HeadSHA is still rejected.
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "task-005")
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)

	delegationChild := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "task-005",
		DelegationID: "verify-1",
		ParentJobID:  "parent-job",
		// no WorktreePath (shared checkout), no HeadSHA (PR-less local orchestrate)
	}
	if err := worker.validateTargetCheckout(ctx, delegationChild, checkout); err != nil {
		t.Fatalf("validateTargetCheckout rejected shared-checkout delegation child without HeadSHA: %v", err)
	}

	nonDelegation := workflow.JobPayload{Repo: "owner/repo", Branch: "task-005"}
	if err := worker.validateTargetCheckout(ctx, nonDelegation, checkout); err == nil {
		t.Fatal("validateTargetCheckout accepted a non-delegation job with no HeadSHA")
	}
}

// TestDefaultCheckoutAllowsBranchlessIssueAsk is the regression guard for the
// #389 live bug: an issue `@<agent> ask` job carries the *issue number* in
// PullRequest (>0) but no Branch (the question stands alone). The `ask` case in
// defaultCheckout previously gated its branch validation on PullRequest>0, so an
// issue ask was rejected with "checkout branch is main, not job branch " — the
// job failed instead of answering, and no real reply was ever posted. This test
// drives the real defaultCheckout against a real git checkout that is on `main`
// (not the empty job branch) and asserts the branchless issue ask is accepted.
// It also asserts that a branch-carrying ask (the PR ask) is still validated, so
// the PR ask's checkout guard is not weakened.
func TestDefaultCheckoutAllowsBranchlessIssueAsk(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	issueAsk := workflow.JobPayload{
		Repo:        "owner/repo",
		PullRequest: 7, // the issue number; >0 but carries no branch
		// Branch and HeadSHA intentionally empty (issue ask, no PR context).
	}
	job := db.Job{ID: "issue-comment-regression", Type: "ask"}
	got, err := worker.defaultCheckout(ctx, job, issueAsk, runtime.Agent{})
	if err != nil {
		t.Fatalf("defaultCheckout rejected branchless issue ask: %v", err)
	}
	if got != checkout {
		t.Fatalf("defaultCheckout = %q, want %q", got, checkout)
	}

	// A PR ask carries the PR head branch; when that branch does not match the
	// checkout (here the checkout is on `main`), validation must still fail so the
	// PR ask guard is preserved.
	prAsk := workflow.JobPayload{
		Repo:        "owner/repo",
		PullRequest: 12,
		Branch:      "feature-branch",
	}
	if _, err := worker.defaultCheckout(ctx, job, prAsk, runtime.Agent{}); err == nil {
		t.Fatal("defaultCheckout accepted a PR ask whose branch does not match the checkout")
	}
}

func TestMergeGateCheckoutUsesRegisteredCheckoutOverTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	worktree := filepath.Join(t.TempDir(), "task-1")
	runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", "task-1", worktree, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	got, err := mergeGateCheckout(ctx, store, "owner/repo", worktree)
	if err != nil {
		t.Fatalf("mergeGateCheckout returned error: %v", err)
	}
	if got != checkout {
		t.Fatalf("merge gate checkout = %q, want registered checkout %q", got, checkout)
	}
}

func TestTaskWorktreeCheckoutPrefersDelegationPayloadWorktree(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := defaultJobWorker(store, io.Discard)

	// The task table records a DIFFERENT worktree; a delegated implement child must
	// run in its own payload.WorktreePath, never the shared task checkout.
	taskCheckout := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "owner/repo", Branch: "task-1", WorktreePath: taskCheckout}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	delegationCheckout := t.TempDir()
	payload := workflow.JobPayload{
		Repo:         "owner/repo",
		Branch:       "gitmoot-delegation-api",
		TaskID:       "task-1",
		DelegationID: "api",
		ParentJobID:  "parent-job",
		WorktreePath: delegationCheckout,
	}

	checkout, ok, err := worker.taskWorktreeCheckout(ctx, payload)
	if err != nil {
		t.Fatalf("taskWorktreeCheckout returned error: %v", err)
	}
	wantCheckout, err := normalizeTaskWorktreePath(delegationCheckout)
	if err != nil {
		t.Fatalf("normalizeTaskWorktreePath returned error: %v", err)
	}
	if !ok || checkout != wantCheckout {
		t.Fatalf("taskWorktreeCheckout = (%q,%v), want (%q,true) from payload worktree, not task table", checkout, ok, wantCheckout)
	}
	if checkout == taskCheckout {
		t.Fatal("checkout resolved to the shared task worktree instead of the delegation worktree")
	}

	// queuedJobTaskWorktreePath keys the scheduler off the same delegation path so
	// siblings sharing a task id get distinct checkout keys and run in parallel.
	path, keyed := queuedJobTaskWorktreePath(ctx, store, payload)
	if !keyed || path != wantCheckout {
		t.Fatalf("queuedJobTaskWorktreePath = (%q,%v), want (%q,true) from payload worktree", path, keyed, wantCheckout)
	}
}

func TestResolveJobCheckoutSelfHealsDanglingLinkedWorktree(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	primary, linked := setupLinkedWorktreeRepo(t)
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepoForce(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: linked, PrimaryCheckoutPath: primary}); err != nil {
		t.Fatalf("UpsertRepoForce returned error: %v", err)
	}
	job := db.Job{ID: "job-checkout-heal", Agent: "audit", Type: "ask", State: string(workflow.JobQueued), Payload: `{"repo":"owner/repo"}`}
	if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	runGit(t, primary, "worktree", "remove", "--force", linked)
	var output bytes.Buffer
	worker := jobWorker{Store: store, Stdout: &output}
	checkout, err := worker.resolveJobCheckout(ctx, job, workflow.JobPayload{Repo: "owner/repo"})
	if err != nil {
		t.Fatalf("resolveJobCheckout returned error: %v", err)
	}
	if checkout != primary {
		t.Fatalf("checkout = %q, want healed primary %q", checkout, primary)
	}
	record, err := store.GetRepo(ctx, "owner/repo")
	if err != nil {
		t.Fatalf("GetRepo returned error: %v", err)
	}
	if record.CheckoutPath != primary || record.PrimaryCheckoutPath != primary {
		t.Fatalf("healed record = %+v", record)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !daemonWorkerHasEvent(events, "repo_checkout_self_healed") {
		t.Fatalf("events = %+v, want repo_checkout_self_healed", events)
	}
	if !strings.Contains(output.String(), "WARN:") || !strings.Contains(output.String(), linked) || !strings.Contains(output.String(), primary) {
		t.Fatalf("warning output = %q", output.String())
	}
}

// TestResolveDaemonStartRepoUsesRegisteredCheckout is the #202 regression: when
// the target repo is already registered with a checkout, `daemon start --repo
// owner/repo` resolves against the REGISTERED checkout regardless of the current
// working directory, instead of failing because cwd's origin does not match.
func TestResolveDaemonStartRepoUsesRegisteredCheckout(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	// The registered checkout for owner/repo.
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	// A non-matching working directory: a different git repo whose origin points
	// at some other repo. Resolving from here previously errored with
	// "current checkout origin is owner/other, not owner/repo".
	otherDir := t.TempDir()
	runGit(t, otherDir, "init")
	runGit(t, otherDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, otherDir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(otherDir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, otherDir, "add", "f.txt")
	runGit(t, otherDir, "commit", "-m", "init")
	runGit(t, otherDir, "remote", "add", "origin", "https://github.com/owner/other.git")

	repo, err := daemon.ParseRepository("owner/repo")
	if err != nil {
		t.Fatalf("ParseRepository returned error: %v", err)
	}
	record, err := resolveDaemonStartRepo(ctx, store, repo, otherDir)
	if err != nil {
		t.Fatalf("resolveDaemonStartRepo from non-matching cwd returned error: %v", err)
	}
	wantRoot := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "--show-toplevel"))
	if record.CheckoutPath != wantRoot {
		t.Fatalf("resolved checkout = %q, want registered checkout %q", record.CheckoutPath, wantRoot)
	}
	if record.Owner != "owner" || record.Name != "repo" {
		t.Fatalf("resolved repo = %s/%s, want owner/repo", record.Owner, record.Name)
	}
}

// TestResolveDaemonStartRepoBootstrapsUnregistered pins that an unregistered repo
// still resolves from the current checkout (first-time setup path), and that an
// origin mismatch there still fails (origin protection intact).
func TestResolveDaemonStartRepoBootstrapsUnregistered(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")

	repo, err := daemon.ParseRepository("owner/repo")
	if err != nil {
		t.Fatalf("ParseRepository returned error: %v", err)
	}
	// Bootstraps from the matching cwd.
	record, err := resolveDaemonStartRepo(ctx, store, repo, checkout)
	if err != nil {
		t.Fatalf("resolveDaemonStartRepo bootstrap returned error: %v", err)
	}
	if record.Owner != "owner" || record.Name != "repo" {
		t.Fatalf("bootstrapped repo = %s/%s, want owner/repo", record.Owner, record.Name)
	}
	// Origin protection: a non-matching cwd for an unregistered repo still fails.
	other, err := daemon.ParseRepository("owner/elsewhere")
	if err != nil {
		t.Fatalf("ParseRepository returned error: %v", err)
	}
	if _, err := resolveDaemonStartRepo(ctx, store, other, checkout); err == nil {
		t.Fatal("expected origin mismatch error for unregistered repo from non-matching cwd")
	}
}
