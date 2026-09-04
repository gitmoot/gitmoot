package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
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

func (w jobWorker) defaultCheckoutForRunner(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent, runner subprocess.Runner) (string, error) {
	checkout, err := w.resolveJobCheckoutForRunner(ctx, job, payload, runner)
	if err != nil {
		return "", err
	}
	switch job.Type {
	case "implement":
		// A delegation child with an empty WorktreePath either resolves a compatible
		// task-table worktree (validated by taskWorktreeCheckout) or falls back to the
		// registered shared checkout, which may sit on `main` rather than the inherited
		// coordinator branch. Preserve the branch-identity escape for this payload
		// shape; delivery observation separately distinguishes the effective source.
		// validateImplementationLock stays UNCONDITIONAL — the branch lock, not this
		// identity guard, is the designed mutation-safety mechanism (#413).
		if !isWorktreeLessDelegationChild(payload) {
			if err := w.validateTargetCheckoutForRunner(ctx, payload, checkout, runner); err != nil {
				return "", err
			}
		}
		if err := w.validateImplementationLock(ctx, payload, implementationLockOwner(agent, payload)); err != nil {
			return "", err
		}
	case "review":
		switch {
		case payload.PullRequest > 0 && strings.TrimSpace(payload.TaskID) != "":
			if err := w.validateReviewCheckoutForRunner(ctx, payload, checkout, runner); err != nil {
				// #684: the PR branch commonly advances between enqueue and execution
				// in an active dev loop, leaving the checkout on a NEWER head than the
				// one the review was pinned to. Re-target the review to the checkout's
				// current head (reviewing the newest commit is what a human reviewer
				// does) when the PR is still open, instead of failing on the mismatch.
				// A closed/merged PR, a dirty tree, or any other checkout error keeps
				// the existing terminal / deferral path.
				if resynced, resyncErr := w.resyncReviewHeadForRunner(ctx, job, payload, checkout, runner, err); resyncErr != nil {
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
			// Same empty-payload-worktree escape as the implement arm; a review is
			// read-only, while taskWorktreeCheckout has already checked any task-table
			// source against the payload's repo and branch (#413).
			if err := w.validateTargetCheckoutForRunner(ctx, payload, checkout, runner); err != nil {
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
			if err := w.validateTargetCheckoutForRunner(ctx, payload, checkout, runner); err != nil {
				return "", err
			}
		}
	}
	return checkout, nil
}

// prepareNativeReviewWorktreeForRunner gives a native PR review leg that reached
// the worker WITHOUT a dispatch-time worktree the same exact-head isolation as a
// deliberate `agent review` dispatch. Since the engine hoisted the ROUTINE leg's
// allocation to before enqueue (prepareNativeReviewWorktree), the remaining
// callers are the configurations where no engine allocation happens at all: an
// engine with no read-only worktree manager or no Home/DelegationCheckout (the
// routine leg and the high-risk lens child both arrive path-less there). A leg
// born with a WorktreePath fails the last gate conjunct below and returns
// untouched, so no configuration has two live allocation paths. It runs after
// scheduler admission, including the disk guard, and before checkout validation.
// The persisted marker lets the existing terminal cleanup reclaim the detached
// worktree.
func (w jobWorker) prepareNativeReviewWorktreeForRunner(ctx context.Context, job db.Job, payload workflow.JobPayload, runner subprocess.Runner) (workflow.JobPayload, error) {
	if job.Type != "review" ||
		payload.PullRequest <= 0 ||
		strings.TrimSpace(payload.HeadSHA) == "" ||
		strings.TrimSpace(payload.ReviewRound) == "" ||
		len(payload.Reviewers) == 0 ||
		strings.TrimSpace(payload.WorktreePath) != "" {
		return payload, nil
	}
	repoRecord, err := w.Store.GetRepo(ctx, payload.Repo)
	if err != nil {
		return payload, err
	}
	checkout := strings.TrimSpace(repoRecord.CheckoutPath)
	if checkout == "" {
		return payload, fmt.Errorf("native review repo %s has no registered checkout", payload.Repo)
	}
	client := jobGitClient(checkout, runner)
	allocate := func() (string, error) {
		return workflow.AllocateReadOnlyWorktree(
			ctx,
			w.Store,
			w.workflowHome(),
			payload.Repo,
			checkout,
			job.ID,
			"readonly-seat",
			0,
			strings.TrimSpace(payload.HeadSHA),
			// Short budget, like every other read-only allocation site: passing 0
			// expanded to the ~2-minute checkoutMutationWaitTimeout, stalling this
			// worker slot on a lock another worker holds for a short shared-.git op.
			// The caller now DEFERS a spent budget for re-dispatch instead of waiting.
			workflow.ReadOnlyWorktreeDispatchLockWaitBudget,
			client,
		)
	}
	path, err := allocate()
	if err != nil {
		// A spent checkout-mutation-lock budget is contention, not a cold checkout: a
		// fetch cannot help it, and the caller defers the leg for re-dispatch. Return
		// it unwrapped-in-kind so that classification still sees the BlockedError.
		if workflow.CheckoutMutationLockContention(err) {
			return payload, fmt.Errorf("allocate native review worktree: %w", err)
		}
		fetchErr := client.FetchPullRequest(ctx, "origin", payload.PullRequest)
		if fetchErr != nil {
			return payload, fmt.Errorf("allocate native review worktree: %w; fetch PR ref: %v", err, fetchErr)
		}
		path, err = allocate()
		if err != nil {
			return payload, fmt.Errorf("allocate native review worktree after fetch: %w", err)
		}
	}
	cleanup := func() {
		_ = client.RemoveWorktreeForce(context.WithoutCancel(ctx), path)
	}
	payload.WorktreePath = path
	payload.ReadOnlyWorktree = true
	payload.ReadOnlySeat = true
	if note := workflow.ReadOnlyWorktreeContextNote(checkout); note != "" {
		payload.Instructions += note
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		cleanup()
		return payload, err
	}
	written, err := w.Store.UpdateJobPayloadAtGeneration(ctx, job.ID, string(encoded), job.LifecycleGeneration)
	if err != nil {
		cleanup()
		return payload, err
	}
	if !written {
		cleanup()
		return payload, fmt.Errorf("native review job %s advanced while allocating its exact-head worktree", job.ID)
	}
	_ = w.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "review_worktree_allocated_exact_head",
		Message: fmt.Sprintf("allocated owned read-only worktree at review head %s", strings.TrimSpace(payload.HeadSHA)),
	})
	return payload, nil
}

func (w jobWorker) resolveJobCheckoutForRunner(ctx context.Context, job db.Job, payload workflow.JobPayload, runner subprocess.Runner) (string, error) {
	if payload.FixWorktree {
		checkout, err := normalizeTaskWorktreePath(payload.WorktreePath)
		if err != nil {
			return "", err
		}
		if checkout == "" {
			return "", errors.New("review fix job has no allocated worktree path")
		}
		repo, err := github.ParseRepository(payload.Repo)
		if err != nil {
			return "", err
		}
		if err := preflightDaemonRepoCheckoutWithRunner(ctx, repo, checkout, runner); err != nil {
			return "", err
		}
		return checkout, nil
	}
	repoRecord, err := w.Store.GetRepo(ctx, payload.Repo)
	if err != nil {
		return "", err
	}
	repo, err := github.ParseRepository(payload.Repo)
	if err != nil {
		return "", err
	}
	checkout, err := w.healRegisteredRepoCheckoutForRunner(ctx, job, repo, repoRecord, runner)
	if err != nil {
		return "", err
	}
	if err := preflightDaemonRepoCheckoutWithRunner(ctx, repo, checkout, runner); err != nil {
		return "", err
	}
	taskCheckout, ok, err := w.taskWorktreeCheckout(ctx, payload)
	if err != nil {
		return "", err
	}
	if ok {
		checkout = taskCheckout
		if err := preflightDaemonRepoCheckoutWithRunner(ctx, repo, checkout, runner); err != nil {
			return "", err
		}
	}
	return checkout, nil
}

func (w jobWorker) healRegisteredRepoCheckoutForRunner(ctx context.Context, job db.Job, repo github.Repository, record db.Repo, runner subprocess.Runner) (string, error) {
	checkout := strings.TrimSpace(record.CheckoutPath)
	resolved, healed, err := resolveRegisteredRepoRecordWithRunner(ctx, w.Store, repo, record, runner)
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

func (w jobWorker) validateTargetCheckoutForRunner(ctx context.Context, payload workflow.JobPayload, checkout string, runner subprocess.Runner) error {
	git := jobGitClient(checkout, runner)
	// A fix-round checkout is an independent writable clone attached to the real
	// task branch. Its allocator bound HEAD to the fetched branch tip at dispatch;
	// validate branch identity and cleanliness here without comparing the inherited
	// review HeadSHA, which may legitimately predate that fetched tip.
	if payload.FixWorktree {
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
		return nil
	}
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

// isWorktreeLessDelegationChild reports the payload shape of a delegation child
// with no per-delegation worktree. A TaskID may still resolve an effective
// task-table worktree, so callers deciding whether the job truly used the shared
// checkout must also consult taskWorktreeCheckout.
func isWorktreeLessDelegationChild(payload workflow.JobPayload) bool {
	return strings.TrimSpace(payload.DelegationID) != "" && strings.TrimSpace(payload.WorktreePath) == ""
}

// deliveryWorktreeResolver injects the checkout already resolved by the worker
// into workflow's result-delivery seam. A payload without a per-delegation path
// may still have an effective task-table worktree, so exclusion is based on that
// resolved source rather than payload.WorktreePath alone.
func deliveryWorktreeResolver(store *db.Store, checkout string) workflow.DeliveryWorktreeResolver {
	checkout = strings.TrimSpace(checkout)
	return func(ctx context.Context, _ db.Job, payload workflow.JobPayload) (workflow.DeliveryWorktreeResolution, error) {
		if isWorktreeLessDelegationChild(payload) {
			if strings.TrimSpace(payload.TaskID) != "" {
				if store == nil {
					return workflow.DeliveryWorktreeResolution{}, errors.New("resolve delivery task worktree: store is nil")
				}
				taskCheckout, ok, err := (jobWorker{Store: store}).taskWorktreeCheckout(ctx, payload)
				if err != nil {
					return workflow.DeliveryWorktreeResolution{}, fmt.Errorf("resolve delivery task worktree: %w", err)
				}
				if ok {
					if !sameCheckoutPath(taskCheckout, checkout) {
						return workflow.DeliveryWorktreeResolution{}, fmt.Errorf("resolved delivery checkout %q differs from task worktree %q", checkout, taskCheckout)
					}
					return workflow.DeliveryWorktreeResolution{Path: checkout}, nil
				}
			}
			return workflow.DeliveryWorktreeResolution{ExcludedSource: workflow.ResultObservationSourceWorktreeLessDelegationChild}, nil
		}
		if checkout == "" {
			return workflow.DeliveryWorktreeResolution{}, errors.New("resolved job checkout is empty")
		}
		return workflow.DeliveryWorktreeResolution{Path: checkout}, nil
	}
}

func (w jobWorker) validateReviewCheckoutForRunner(ctx context.Context, payload workflow.JobPayload, checkout string, runner subprocess.Runner) error {
	git := jobGitClient(checkout, runner)
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

// Job-event kinds recorded by resyncReviewHead. Each names the relationship the
// code actually MEASURED between the dispatched head and the checkout head, so a
// coordinator reading a job's record can tell a fast-forward re-target from a
// same-commit expansion from a refusal (#1561) — the previous single kind claimed
// "branch advanced" for all three, a direction the code never measured.
const (
	reviewHeadResyncedEvent      = "review_head_resynced"
	reviewHeadNormalizedEvent    = "review_head_normalized"
	reviewHeadResyncRefusedEvent = "review_head_resync_refused"
)

// resolvedCommitID returns the 40-character object id git resolves rev to, or ""
// when this repository cannot resolve it to a commit. The rev is handed to git
// VERBATIM: this code must never interpret the spelling of a revision, so it is
// not lowercased, length-tested, or hex-tested. Lowercasing here is what
// previously broke "HEAD" (git rejects "head"), and every shape test that stood in
// for identity produced a false movement claim for some other spelling of the same
// commit (#1561).
//
// The "^{commit}" peel is what makes the resolution VERIFIED rather than merely
// syntactic: plain `rev-parse <40 hex>` echoes a well-formed SHA back even when
// the object is absent, so an unfetched or garbage-collected head would look
// resolved and reach the ancestry comparison, which is the one place that must
// only ever see object ids present here.
func resolvedCommitID(ctx context.Context, git gitutil.Client, rev string) string {
	resolved, err := git.RevParse(ctx, rev+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(resolved)
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
//     a stale review of a dead PR does not silently pass), and
//   - the checkout head is a strict FAST-FORWARD of the dispatched head, i.e. the
//     dispatched commit is an ancestor of it (#1561).
//
// That last gate is what makes the re-target directional. Measured on this host,
// a queued review could be re-pointed at a commit that was NOT the branch tip in
// either direction: backwards onto a superseded commit, and — after a force-push —
// onto a commit reachable from zero remote branches, so a verdict described an
// orphan (#1561 comments 5326259747 and 5330559022). A non-descendant checkout
// head is now refused rather than reviewed, and the refusal is recorded on the job.
//
// A same-commit expansion is not a re-target at all: a 12-char dispatched head and
// the checkout's 40-char head are one commit, and counting those as re-syncs is the
// measurement error that produced #1561's retracted headline. Those persist the
// normalized head (so the string head-check at validateReviewCheckout passes and
// the review still runs, exactly as before) but record the distinct normalization
// event instead of a re-sync.
//
// On a re-sync it persists the current head onto the job payload (RunJob re-reads
// the payload from the store, so the delivered review prompt and the posted PR
// comment carry the new head) and records the measured event, then returns true so
// defaultCheckoutForRunner proceeds with the review. Every declined case returns
// false so the caller's existing error path runs byte-identically.
func (w jobWorker) resyncReviewHeadForRunner(ctx context.Context, job db.Job, payload workflow.JobPayload, checkout string, runner subprocess.Runner, cause error) (bool, error) {
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
	git := jobGitClient(checkout, runner)
	// Confirm the resolved checkout is actually on the PR's head branch before
	// re-targeting. A review that falls back to the registered shared checkout (which
	// sits on `main`, not the PR branch) must NOT be re-synced to main's head — that
	// would review the wrong tree and could post an approval against a SHA that is not
	// the PR head. We only decline when we can POSITIVELY confirm the branch differs:
	// a detached-HEAD worktree (CurrentBranch errors) is a legitimate #684 target and
	// is left to proceed. We deliberately do NOT gate on head == pr.HeadSHA because the
	// PR-watcher can lag the push, which is exactly the drift #684 exists to tolerate;
	// the ancestry gate below compares the checkout head against the head this job was
	// DISPATCHED with, never against the watcher's possibly-lagging PR record.
	if b, err := git.CurrentBranch(ctx); err == nil &&
		strings.TrimSpace(b) != strings.TrimSpace(payload.Branch) {
		return false, nil
	}
	head, err := git.HeadSHA(ctx)
	if err != nil {
		return false, err
	}
	head = strings.TrimSpace(head)
	dispatchedRev := strings.TrimSpace(payload.HeadSHA)
	if head == "" || dispatchedRev == "" {
		// Nothing to re-target to, or nothing to re-target FROM; let the caller's
		// existing path handle it.
		return false, nil
	}
	// IDENTITY IS DECIDED BY RESOLUTION, NEVER BY SHAPE. The dispatched rev and the
	// checkout head are compared as the 40-character object ids git resolves them
	// to, so no spelling of one commit can be mistaken for a different commit:
	// "8aaa1b", "<sha>^0", "feat/x", "HEAD", a 12-char abbreviation and a
	// differently-cased 40-char SHA are all the same comparison. Three rounds of
	// this fix enumerated spellings instead (a prefix test, then a case test) and
	// each time another spelling reached the ancestry gate, where a commit is its
	// own ancestor and a head that NEVER MOVED was recorded as a fast-forward
	// re-sync. A rev that git cannot resolve is not identity; it goes to the
	// ancestry gate below.
	dispatched := head
	if !strings.EqualFold(dispatchedRev, head) {
		// Cheap path only: two 40-char spellings of one id need no subprocess. Every
		// other rev is resolved by git, which this path can afford — it is reached
		// only after a head mismatch already failed the pre-flight.
		dispatched = resolvedCommitID(ctx, git, dispatchedRev)
	}
	if dispatched == head {
		if err := w.persistReviewHead(ctx, job, payload, head); err != nil {
			return false, err
		}
		if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{
			JobID: job.ID,
			Kind:  reviewHeadNormalizedEvent,
			Message: fmt.Sprintf("PR #%d dispatched head %s resolves to checkout head %s — the SAME commit; recording the canonical SHA, not re-targeting the review",
				payload.PullRequest, dispatchedRev, head),
		}); eventErr != nil {
			return false, eventErr
		}
		writeLine(w.Stdout, "job %s review head %s recorded canonically as %s (PR #%d same commit)", job.ID, dispatchedRev, head, payload.PullRequest)
		return true, nil
	}
	if dispatched == "" {
		// git cannot resolve the dispatched rev here (an unfetched object, or an
		// orphaned force-push target that was garbage-collected), so the relationship
		// is UNDECIDABLE. Record the diagnosis on the job and return false, letting
		// the caller's ORIGINAL "checkout head is X, not review job head Y" error
		// propagate untouched: that string is what classifyCheckoutContention
		// (job_blocker_checkout.go:101) matches to defer and auto-retry the job, and
		// an unfetched object is exactly the self-healing case #532 exists for. A
		// distinctly-worded error here classified as nothing and terminally failed a
		// job that used to recover.
		_ = w.Store.AddJobEvent(ctx, db.JobEvent{
			JobID: job.ID,
			Kind:  reviewHeadResyncRefusedEvent,
			Message: fmt.Sprintf("PR #%d dispatched head %s does not resolve in this checkout, so its relationship to checkout head %s is undecidable; refusing to re-target the review",
				payload.PullRequest, dispatchedRev, head),
		})
		writeLine(w.Stdout, "job %s review head re-sync refused: dispatched head %s does not resolve in the checkout (PR #%d)", job.ID, dispatchedRev, payload.PullRequest)
		return false, nil
	}
	fastForward, err := git.IsAncestor(ctx, dispatched, head)
	if err != nil {
		// merge-base could not answer. A non-1 exit is NOT confined to a corrupt
		// object: a cancelled context at daemon shutdown, a killed subprocess, or a
		// broken git binary all land here, and shutdown cancellation is the ordinary
		// case. Returning a distinctly-worded error was a defect — the caller
		// propagates it verbatim (:68), classifyCheckoutContention scores it
		// checkoutContentionNone (job_blocker_checkout.go:104) and the job fails
		// terminally, where the SAME job on the original wrong-head error scores
		// Dirty and defers. It also contradicted this package's own test, which
		// constructs that wording as the string a refusal must NOT introduce. So
		// this branch does what the other two refusals do: record the diagnosis on
		// the job and return false, leaving the caller's original error untouched.
		_ = w.Store.AddJobEvent(ctx, db.JobEvent{
			JobID: job.ID,
			Kind:  reviewHeadResyncRefusedEvent,
			Message: fmt.Sprintf("PR #%d could not compare checkout head %s with dispatched head %s (%v); refusing to re-target the review",
				payload.PullRequest, head, dispatched, err),
		})
		writeLine(w.Stdout, "job %s review head re-sync refused: comparing %s with dispatched head %s failed: %v (PR #%d)", job.ID, head, dispatched, err, payload.PullRequest)
		return false, nil
	}
	if !fastForward {
		// Record the refusal on the JOB, not just the daemon journal (#1561 ask 2):
		// this is precisely the case a coordinator cannot otherwise see. Best-effort
		// on purpose — the caller must keep propagating the original head-mismatch
		// error, so an event-write failure must not replace it with a DB error.
		_ = w.Store.AddJobEvent(ctx, db.JobEvent{
			JobID: job.ID,
			Kind:  reviewHeadResyncRefusedEvent,
			Message: fmt.Sprintf("PR #%d checkout head %s does not have dispatched head %s as an ancestor (superseded or divergent commit); refusing to re-target the review",
				payload.PullRequest, head, dispatched),
		})
		writeLine(w.Stdout, "job %s review head re-sync refused: %s is not a descendant of dispatched head %s (PR #%d)", job.ID, head, dispatched, payload.PullRequest)
		return false, nil
	}
	if err := w.persistReviewHead(ctx, job, payload, head); err != nil {
		return false, err
	}
	if eventErr := w.Store.AddJobEvent(ctx, db.JobEvent{
		JobID: job.ID,
		Kind:  reviewHeadResyncedEvent,
		Message: fmt.Sprintf("PR #%d checkout head %s has dispatched head %s as an ancestor (fast-forward); re-targeting the review to the checkout head",
			payload.PullRequest, head, dispatched),
	}); eventErr != nil {
		return false, eventErr
	}
	writeLine(w.Stdout, "job %s review head re-synced %s -> %s (PR #%d fast-forward)", job.ID, dispatched, head, payload.PullRequest)
	return true, nil
}

// persistReviewHead writes the head the review will actually be delivered against
// onto the job payload; RunJob re-reads the payload from the store, so the review
// prompt and the posted PR comment carry it. The dispatched head survives only in
// the event message above: that audit trail exists, but a machine-readable
// dispatched-vs-read contract does not, because both SHAs are interpolated into
// prose. Giving it a structured form needs a JobPayload field plus the consumer
// that reads it, outside this seam; #1561 ask 3 stays open for that.
func (w jobWorker) persistReviewHead(ctx context.Context, job db.Job, payload workflow.JobPayload, head string) error {
	payload.HeadSHA = head
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return w.Store.UpdateJobPayload(ctx, job.ID, string(encoded))
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
