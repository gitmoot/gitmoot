package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func mergeGateCheckout(ctx context.Context, store *db.Store, repo string, fallback string) (string, error) {
	if store == nil {
		return strings.TrimSpace(fallback), nil
	}
	record, err := store.GetRepo(ctx, repo)
	if err != nil {
		return "", err
	}
	checkout := strings.TrimSpace(record.CheckoutPath)
	if checkout == "" {
		return "", fmt.Errorf("repo %s has no checkout path", repo)
	}
	return checkout, nil
}

func (w jobWorker) defaultCheckout(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent) (string, error) {
	checkout, err := w.resolveJobCheckout(ctx, job, payload)
	if err != nil {
		return "", err
	}
	switch job.Type {
	case "implement":
		// A worktree-less delegation child (delegation leg, empty WorktreePath)
		// can only resolve the registered shared checkout, which sits on `main`,
		// never its inherited coordinator branch — so validating that checkout
		// against payload.Branch would reject it with "checkout branch is main, not
		// job branch <X>". Skip the branch-identity guard for that child only (the
		// engine's delegation_worktree_skipped fallback runs it against the shared
		// checkout and still holds its branch lock); mirror the #389 ask-arm escape.
		// validateImplementationLock stays UNCONDITIONAL — the branch lock, not this
		// identity guard, is the designed mutation-safety mechanism (#413).
		if !isWorktreeLessDelegationChild(payload) {
			if err := w.validateTargetCheckout(ctx, payload, checkout); err != nil {
				return "", err
			}
		}
		if err := w.validateImplementationLock(ctx, payload, implementationLockOwner(agent, payload)); err != nil {
			return "", err
		}
	case "review":
		switch {
		case payload.PullRequest > 0 && strings.TrimSpace(payload.TaskID) != "":
			if err := w.validateReviewCheckout(ctx, payload, checkout); err != nil {
				// #684: the PR branch commonly advances between enqueue and execution
				// in an active dev loop, leaving the checkout on a NEWER head than the
				// one the review was pinned to. Re-target the review to the checkout's
				// current head (reviewing the newest commit is what a human reviewer
				// does) when the PR is still open, instead of failing on the mismatch.
				// A closed/merged PR, a dirty tree, or any other checkout error keeps
				// the existing terminal / deferral path.
				if resynced, resyncErr := w.resyncReviewHead(ctx, job, payload, checkout, err); resyncErr != nil {
					return "", resyncErr
				} else if resynced {
					return checkout, nil
				}
				return "", err
			}
		case payload.PullRequest <= 0 && strings.TrimSpace(payload.Branch) == "":
			// A PR-less, branchless review heartbeat (#564: Action="review",
			// PullRequest=0, Branch="") carries no branch identity to validate. Like a
			// PR-less ask it runs read-only against the registered checkout as-is, and
			// the engine's PR-less-review guard treats the delivered review as terminal.
			// Validating it against the empty payload.Branch would reject the registered
			// default-branch checkout ("checkout branch is main, not job branch "),
			// wedging the heartbeat at the worker before the engine ever sees it.
		case !isWorktreeLessDelegationChild(payload):
			// Same worktree-less delegation child escape as the implement arm; a
			// review is read-only ⇒ running it against the shared checkout is
			// trivially safe (#413).
			if err := w.validateTargetCheckout(ctx, payload, checkout); err != nil {
				return "", err
			}
		}
	case "ask":
		// A PR ask carries BOTH the PR head branch and PullRequest>0, so the
		// registered checkout must be on that branch/head before the agent reads
		// the tree. An issue ask (#389) reuses PullRequest for the *issue number*
		// (PullRequest>0) but carries no branch, so the prior `PullRequest > 0`
		// gate wrongly validated it against the job branch and failed it with
		// "checkout branch is main, not job branch ". Require both a positive
		// PullRequest AND a branch so only a real PR ask is validated; a branchless
		// issue ask — and a branch-only PR-less CLI ask — run against the
		// registered checkout as-is.
		if payload.PullRequest > 0 && strings.TrimSpace(payload.Branch) != "" {
			if err := w.validateTargetCheckout(ctx, payload, checkout); err != nil {
				return "", err
			}
		}
	}
	return checkout, nil
}

func (w jobWorker) resolveJobCheckout(ctx context.Context, job db.Job, payload workflow.JobPayload) (string, error) {
	repoRecord, err := w.Store.GetRepo(ctx, payload.Repo)
	if err != nil {
		return "", err
	}
	repo, err := daemon.ParseRepository(payload.Repo)
	if err != nil {
		return "", err
	}
	checkout, err := w.healRegisteredRepoCheckout(ctx, job, repo, repoRecord)
	if err != nil {
		return "", err
	}
	if err := preflightDaemonRepoCheckout(ctx, repo, checkout); err != nil {
		return "", err
	}
	taskCheckout, ok, err := w.taskWorktreeCheckout(ctx, payload)
	if err != nil {
		return "", err
	}
	if ok {
		checkout = taskCheckout
		if err := preflightDaemonRepoCheckout(ctx, repo, checkout); err != nil {
			return "", err
		}
	}
	return checkout, nil
}

func (w jobWorker) healRegisteredRepoCheckout(ctx context.Context, job db.Job, repo github.Repository, record db.Repo) (string, error) {
	checkout := strings.TrimSpace(record.CheckoutPath)
	resolved, healed, err := resolveRegisteredRepoRecord(ctx, w.Store, repo, record)
	if err != nil {
		return "", err
	}
	healedPath := strings.TrimSpace(resolved.CheckoutPath)
	if !healed {
		return healedPath, nil
	}
	message := repoCheckoutHealMessage(repo.FullName(), checkout, healedPath)
	if err := w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "repo_checkout_self_healed", Message: message}); err != nil {
		return "", err
	}
	if w.Stdout != nil {
		writeLine(w.Stdout, "WARN: %s", message)
	}
	return healedPath, nil
}

func sameCheckoutPath(a, b string) bool {
	return filepath.Clean(strings.TrimSpace(a)) == filepath.Clean(strings.TrimSpace(b))
}

func (w jobWorker) taskWorktreeCheckout(ctx context.Context, payload workflow.JobPayload) (string, bool, error) {
	// Delegated jobs carry their own per-delegation worktree path in the payload
	// (an implement child's branch worktree, or a read-only fan-out child's
	// detached worktree); prefer it over the task-table worktree so the child runs
	// in its isolated checkout.
	if delegationPath := strings.TrimSpace(payload.WorktreePath); delegationPath != "" {
		checkout, err := normalizeTaskWorktreePath(delegationPath)
		if err != nil {
			return "", false, err
		}
		return checkout, checkout != "", nil
	}
	if strings.TrimSpace(payload.TaskID) == "" {
		return "", false, nil
	}
	task, err := w.Store.GetTask(ctx, payload.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(task.RepoFullName) != "" && task.RepoFullName != payload.Repo {
		return "", false, fmt.Errorf("task %s belongs to repo %s, not %s", payload.TaskID, task.RepoFullName, payload.Repo)
	}
	if strings.TrimSpace(task.Branch) != "" && task.Branch != payload.Branch {
		return "", false, fmt.Errorf("task %s branch is %s, not job branch %s", payload.TaskID, task.Branch, payload.Branch)
	}
	checkout := strings.TrimSpace(task.WorktreePath)
	if checkout == "" {
		return "", false, nil
	}
	checkout, err = normalizeTaskWorktreePath(checkout)
	if err != nil {
		return "", false, err
	}
	return checkout, true, nil
}

func normalizeTaskWorktreePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("normalize task worktree path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func (w jobWorker) validateTargetCheckout(ctx context.Context, payload workflow.JobPayload, checkout string) error {
	git := gitutil.Client{Dir: checkout}
	// A delegation worktree child runs in a gitmoot-managed worktree. An implement
	// child is on its delegation branch (created off the parent base, whose tip may
	// have advanced past the inherited HeadSHA — so its HeadSHA check is skipped),
	// while a read-only child uses a *detached* worktree with no branch at all (so
	// CurrentBranch errors). Validate the branch when the worktree has one (the
	// implement guard, preserved) and skip it for a detached read-only worktree;
	// both still require the freshly allocated worktree to be clean.
	if isDelegationWorktreeChild(payload) {
		if branch, err := git.CurrentBranch(ctx); err == nil && branch != payload.Branch {
			return fmt.Errorf("checkout branch is %s, not job branch %s", branch, payload.Branch)
		}
		clean, err := git.WorktreeClean(ctx)
		if err != nil {
			return err
		}
		if !clean {
			return fmt.Errorf("checkout %s has uncommitted changes", checkout)
		}
		return nil
	}
	branch, err := git.CurrentBranch(ctx)
	if err != nil {
		return err
	}
	if branch != payload.Branch {
		return fmt.Errorf("checkout branch is %s, not job branch %s", branch, payload.Branch)
	}
	clean, err := git.WorktreeClean(ctx)
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("checkout %s has uncommitted changes", checkout)
	}
	expectedHead := strings.TrimSpace(payload.HeadSHA)
	if expectedHead == "" {
		// A delegation child can inherit an empty HeadSHA from a coordinator that
		// has no PR context (a local `gitmoot orchestrate`). It is gitmoot-dispatched
		// against the registered checkout, so run it against the current HEAD rather
		// than failing — e.g. a decompose-and-verify verify step, or any read-only
		// follow-up delegation. Implement children always run in a per-delegation
		// worktree (handled above), so only non-mutating shared-checkout delegation
		// children reach here. Non-delegation jobs (PR comments) still require a
		// HeadSHA.
		if strings.TrimSpace(payload.DelegationID) != "" {
			return nil
		}
		return fmt.Errorf("job for %s has no head SHA", payload.Branch)
	}
	head, err := git.HeadSHA(ctx)
	if err != nil {
		return err
	}
	if head != expectedHead {
		return fmt.Errorf("checkout head is %s, not job head %s", head, expectedHead)
	}
	return nil
}

// isDelegationWorktreeChild reports whether the job is a delegated child running
// in its own per-delegation worktree (it carries both a delegation id and an
// allocated worktree path). Such children are validated against their isolated
// worktree HEAD rather than the inherited parent HeadSHA.
func isDelegationWorktreeChild(payload workflow.JobPayload) bool {
	return strings.TrimSpace(payload.DelegationID) != "" && strings.TrimSpace(payload.WorktreePath) != ""
}

// isWorktreeLessDelegationChild is the exact complement of
// isDelegationWorktreeChild: a delegation child (it carries a delegation id — a
// delegation *leg*, NOT just a ParentJobID, which continuations also carry with
// an empty DelegationID and which route through the `ask` arm) that has no
// allocated worktree path. With no worktree it can only resolve the repo's
// registered shared checkout, which sits on `main` — never the inherited
// coordinator branch (delegationRequest sets Branch: payload.Branch) — so
// validating that checkout against payload.Branch would reject it with
// "checkout branch is main, not job branch <X>". A wrong-branch *task* worktree
// is already rejected upstream at taskWorktreeCheckout, so WorktreePath == ""
// cleanly means "the shared registered checkout is the only resolution."
// defaultCheckout's implement/review arms skip the validateTargetCheckout branch
// guard for such a child (the branch lock, not this identity guard, is the
// designed mutation-safety mechanism); see #389 (the ask-arm precedent) and #413.
func isWorktreeLessDelegationChild(payload workflow.JobPayload) bool {
	return strings.TrimSpace(payload.DelegationID) != "" && strings.TrimSpace(payload.WorktreePath) == ""
}

func (w jobWorker) validateReviewCheckout(ctx context.Context, payload workflow.JobPayload, checkout string) error {
	git := gitutil.Client{Dir: checkout}
	clean, err := git.WorktreeClean(ctx)
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("checkout %s has uncommitted changes", checkout)
	}
	expectedHead := strings.TrimSpace(payload.HeadSHA)
	if expectedHead == "" {
		return fmt.Errorf("review job for PR #%d has no head SHA", payload.PullRequest)
	}
	head, err := git.HeadSHA(ctx)
	if err != nil {
		return err
	}
	if head != expectedHead {
		return fmt.Errorf("checkout head is %s, not review job head %s", head, expectedHead)
	}
	return nil
}

// isReviewHeadMismatch reports whether a checkout pre-flight error is specifically
// the review head-SHA drift emitted by validateReviewCheckout ("checkout head is
// X, not review job head Y") — NOT a dirty tree, a missing head, or a branch
// mismatch. Only that one condition is eligible for the #684 re-sync; every other
// checkout error keeps its existing terminal / deferral path.
func isReviewHeadMismatch(cause error) bool {
	if cause == nil {
		return false
	}
	return strings.Contains(cause.Error(), "not review job head")
}

// reviewPullRequestOpen reports whether the review's PR is KNOWN to be open, using
// the locally-tracked pull_requests record (the daemon's PR-watcher upserts an
// open record for every PR it watches before it fans out review jobs, so a genuine
// #684 review of an active PR has one). Re-sync is gated on a definitively-open PR:
//
//   - record found + state open (or any non-closed/-merged state) ⇒ open (re-sync).
//   - record found + state closed/merged ⇒ NOT open (a stale review of a dead PR
//     must not silently pass; keep the existing terminal path).
//   - NO record (sql.ErrNoRows) ⇒ NOT open. The store has no evidence the PR is
//     live, so it falls through to the existing #532 checkout-contention deferral
//     rather than re-targeting to a possibly-unrelated checkout head.
//   - a real DB error ⇒ surfaced; the caller declines to re-sync.
func (w jobWorker) reviewPullRequestOpen(ctx context.Context, repo string, number int) (bool, error) {
	pr, err := w.Store.GetPullRequest(ctx, repo, int64(number))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state := strings.ToLower(strings.TrimSpace(pr.State))
	return state != "closed" && state != "merged", nil
}

// resyncReviewHead handles #684 head-SHA drift for a PR review job. A review is
// pinned to the PR head SHA at enqueue; in an active dev loop the branch often
// advances (a newer commit is pushed) before the queued review runs, so the
// registered checkout sits on a NEWER head than the one the review was pinned to.
// validateReviewCheckout then rejects it with "checkout head is <new>, not review
// job head <old>", and the job ultimately fails — even though reviewing the
// checkout's current head is strictly more useful (it is exactly what a human
// reviewer does). resyncReviewHead re-targets the review to the checkout's current
// head instead of failing, but ONLY when:
//
//   - the validation failure was specifically the review head-SHA mismatch (a
//     dirty tree, a missing head, or a branch mismatch is left untouched), and
//   - the PR is still OPEN (a closed/merged PR keeps the existing terminal path so
//     a stale review of a dead PR does not silently pass).
//
// On a re-sync it persists the current head onto the job payload (RunJob re-reads
// the payload from the store, so the delivered review prompt and the posted PR
// comment carry the new head) and records a review_head_resynced event, then
// returns true so defaultCheckout proceeds with the review. Every declined case
// returns false so the caller's existing error path runs byte-identically.
func (w jobWorker) resyncReviewHead(ctx context.Context, job db.Job, payload workflow.JobPayload, checkout string, cause error) (bool, error) {
	if !isReviewHeadMismatch(cause) {
		return false, nil
	}
	if payload.PullRequest <= 0 {
		return false, nil
	}
	open, err := w.reviewPullRequestOpen(ctx, payload.Repo, payload.PullRequest)
	if err != nil {
		// Undeterminable PR state (a DB read error) ⇒ do not re-sync; fall through to
		// the existing deferral/terminal path rather than reviewing a possibly-dead PR.
		return false, nil
	}
	if !open {
		// A closed/merged PR, or one the store has no record of, keeps the existing
		// #532 deferral / terminal path — only a definitively-open PR is re-synced.
		return false, nil
	}
	// Confirm the resolved checkout is actually on the PR's head branch before
	// re-targeting. A review that falls back to the registered shared checkout (which
	// sits on `main`, not the PR branch) must NOT be re-synced to main's head — that
	// would review the wrong tree and could post an approval against a SHA that is not
	// the PR head. We only decline when we can POSITIVELY confirm the branch differs:
	// a detached-HEAD worktree (CurrentBranch errors) is a legitimate #684 target and
	// is left to proceed. We deliberately do NOT gate on head == pr.HeadSHA because the
	// PR-watcher can lag the push, which is exactly the drift #684 exists to tolerate.
	if b, err := (gitutil.Client{Dir: checkout}).CurrentBranch(ctx); err == nil &&
		strings.TrimSpace(b) != strings.TrimSpace(payload.Branch) {
		return false, nil
	}
	head, err := (gitutil.Client{Dir: checkout}).HeadSHA(ctx)
	if err != nil {
		return false, err
	}
	head = strings.TrimSpace(head)
	previous := strings.TrimSpace(payload.HeadSHA)
	if head == "" || head == previous {
		// Nothing to re-target to (empty or already-current head); let the caller's
		// existing path handle it.
		return false, nil
	}
	payload.HeadSHA = head
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	if err := w.Store.UpdateJobPayload(ctx, job.ID, string(encoded)); err != nil {
		return false, err
	}
	if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{
		JobID: job.ID,
		Kind:  "review_head_resynced",
		Message: fmt.Sprintf("PR #%d branch advanced from %s to %s before the review ran; re-targeting the review to the current head",
			payload.PullRequest, previous, head),
	}); eventErr != nil {
		return false, eventErr
	}
	writeLine(w.Stdout, "job %s review head re-synced %s -> %s (PR #%d advanced)", job.ID, previous, head, payload.PullRequest)
	return true, nil
}

func implementationLockOwner(agent runtime.Agent, payload workflow.JobPayload) string {
	if payload.DelegationReason == "runtime_session_busy" && payload.DelegatedAgent == agent.Name && strings.TrimSpace(payload.OriginalAgent) != "" {
		return payload.OriginalAgent
	}
	return agent.Name
}

func (w jobWorker) validateImplementationLock(ctx context.Context, payload workflow.JobPayload, owner string) error {
	lock, err := w.Store.GetBranchLock(ctx, payload.Repo, payload.Branch)
	if err != nil {
		return err
	}
	if lock.Owner != owner {
		return fmt.Errorf("branch %s is locked by %s, not %s", payload.Branch, lock.Owner, owner)
	}
	return nil
}

// resolveDaemonStartRepo resolves the repo record that `daemon start/run --repo
// owner/repo` should run against. When the repo is already registered with a
// checkout path, it validates that checkout and self-heals through its recorded
// primary when necessary, so the command works from any working directory
// (#202/#959). When the repo is not yet registered, it bootstraps from workDir;
// an implicit linked checkout is pinned to its primary.
func resolveDaemonStartRepo(ctx context.Context, store *db.Store, repo github.Repository, workDir string) (db.Repo, error) {
	return resolveRepoRecord(ctx, store, repo, workDir)
}

func repoRecordForCheckout(ctx context.Context, repo github.Repository, client gitutil.Client) (db.Repo, error) {
	root, err := client.Root(ctx)
	if err != nil {
		return db.Repo{}, fmt.Errorf("resolve repo checkout: %w", err)
	}
	remote, err := client.OriginRemote(ctx)
	if err != nil {
		return db.Repo{}, fmt.Errorf("resolve repo checkout remote: %w", err)
	}
	remoteRepo, err := gitutil.ParseGitHubRemote(remote)
	if err != nil {
		return db.Repo{}, err
	}
	if remoteRepo.String() != repo.FullName() {
		return db.Repo{}, fmt.Errorf("current checkout origin is %s, not %s", remoteRepo.String(), repo.FullName())
	}
	defaultBranch := ""
	if branch, err := client.CurrentBranch(ctx); err == nil {
		defaultBranch = branch
	}
	return db.Repo{
		Owner:         repo.Owner,
		Name:          repo.Name,
		DefaultBranch: defaultBranch,
		RemoteURL:     remote,
		CheckoutPath:  root,
	}, nil
}
