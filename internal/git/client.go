package git

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

type Client struct {
	Runner subprocess.Runner
	Dir    string
}

func (c Client) CreateBranch(ctx context.Context, branch string, base string) error {
	if err := validateBranch(branch); err != nil {
		return err
	}
	args := []string{"switch", "-c", branch}
	if strings.TrimSpace(base) != "" {
		args = append(args, base)
	}
	_, err := c.run(ctx, args...)
	return err
}

func (c Client) AddWorktree(ctx context.Context, branch string, path string, base string) error {
	if err := validateBranch(branch); err != nil {
		return err
	}
	path, err := validateWorktreePath(path)
	if err != nil {
		return err
	}
	args := []string{"worktree", "add", "-b", branch, path}
	if strings.TrimSpace(base) != "" {
		args = append(args, base)
	}
	_, err = c.run(ctx, args...)
	return err
}

func (c Client) AddExistingBranchWorktree(ctx context.Context, branch string, path string) error {
	if err := validateBranch(branch); err != nil {
		return err
	}
	path, err := validateWorktreePath(path)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, "worktree", "add", path, branch)
	return err
}

func (c Client) AddDetachedWorktree(ctx context.Context, path string, ref string) error {
	path, err := validateWorktreePath(path)
	if err != nil {
		return err
	}
	if err := validateRef(ref); err != nil {
		return err
	}
	_, err = c.run(ctx, "worktree", "add", "--detach", path, ref)
	return err
}

// CloneLocalNoCheckout makes an INDEPENDENT local clone of this repo (c.Dir) into
// dest via `git clone --local --no-checkout`. Because the source is local, git
// HARDLINKS everything under objects/ (fast, space-cheap) and copies refs, but the
// clone gets its OWN git directory: its own object DB directory, refs, config, HEAD,
// and worktree registry. A command later run INSIDE the clone (`git config`,
// `git update-ref`, `git gc`, `git worktree prune`) therefore mutates only the
// clone's git state, never the source repo's — the containment property a detached
// `git worktree` off the source CANNOT provide (a worktree shares the source's
// object DB, refs, and config). --no-checkout leaves the working tree empty for a
// subsequent CheckoutDetach at a specific ref. Because objects are copied wholesale
// (not just reachable ones), any SHA present in the source's object DB stays
// checkoutable in the clone, so it preserves the availability of a raw
// `git worktree add --detach <sha>`.
func (c Client) CloneLocalNoCheckout(ctx context.Context, dest string) error {
	dest, err := validateWorktreePath(dest)
	if err != nil {
		return err
	}
	src := strings.TrimSpace(c.Dir)
	if src == "" {
		return errors.New("clone source (client dir) is required")
	}
	_, err = c.run(ctx, "clone", "--local", "--no-checkout", src, dest)
	return err
}

// CheckoutDetach checks out ref as a detached HEAD (`git checkout --detach <ref>`).
// It accepts a raw SHA even when unreachable from any ref, so it pairs with
// CloneLocalNoCheckout to materialize an exact merged head in a fresh clone.
func (c Client) CheckoutDetach(ctx context.Context, ref string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	_, err := c.run(ctx, "checkout", "--detach", ref)
	return err
}

// RemoveRemote drops a configured remote (`git remote remove <name>`). It is used to
// sever a throwaway sandbox clone from its origin (the daemon checkout) so a verifier
// command can never `git fetch`/`git push` back against the live repo.
func (c Client) RemoveRemote(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("remote name is required")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("remote name %q must not start with '-'", name)
	}
	_, err := c.run(ctx, "remote", "remove", name)
	return err
}

func (c Client) BranchExists(ctx context.Context, branch string) (bool, error) {
	if err := validateBranch(branch); err != nil {
		return false, err
	}
	_, err := c.run(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (c Client) RemoveWorktree(ctx context.Context, path string) error {
	path, err := validateWorktreePath(path)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, "worktree", "remove", path)
	return err
}

// RemoveWorktreeForce removes a worktree even when it has uncommitted or
// untracked changes. It is intended for throwaway worktrees (e.g. detached
// read-only delegation fan-out worktrees) whose contents are never integrated,
// so a runtime that left scratch files behind must not block disposal.
func (c Client) RemoveWorktreeForce(ctx context.Context, path string) error {
	path, err := validateWorktreePath(path)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, "worktree", "remove", "--force", path)
	return err
}

// DeleteBranch force-deletes a local branch (git branch -D). It is used to tear
// down a terminal implement delegation's gitmoot-delegation-* branch so it does
// not linger in the shared checkout and contaminate a later coordinator's
// planning. Force (-D) because the branch may be unmerged in the shared checkout.
func (c Client) DeleteBranch(ctx context.Context, branch string) error {
	if err := validateBranch(branch); err != nil {
		return err
	}
	_, err := c.run(ctx, "branch", "-D", branch)
	return err
}

// MergeBranches sequentially merges each branch into the worktree at dir (its
// current HEAD). It is used to integrate the per-delegation branches of parallel
// implement legs into one tree before a dependent verify/review step runs
// (issue #332). Sequential (not octopus) so a conflict pinpoints the offending
// branch; on conflict the in-progress merge is aborted and an error naming the
// branch is returned, so the caller can block rather than auto-resolve.
func (c Client) MergeBranches(ctx context.Context, dir string, branches []string, message string) error {
	dir, err := validateWorktreePath(dir)
	if err != nil {
		return err
	}
	if strings.TrimSpace(message) == "" {
		message = "Gitmoot integration merge"
	}
	git := Client{Dir: dir, Runner: c.Runner}
	for _, branch := range branches {
		if err := validateBranch(branch); err != nil {
			return err
		}
		if _, err := git.run(ctx, "merge", "--no-edit", "-m", message, branch); err != nil {
			// Leave the worktree clean for disposal even on failure.
			_, _ = git.run(ctx, "merge", "--abort")
			return fmt.Errorf("merge branch %q: %w", branch, err)
		}
	}
	return nil
}

// CommitWorktree stages everything in the worktree at dir and commits it to that
// worktree's current branch, returning whether a commit was made. It lets an
// implement delegation leg persist its work to its own branch on success — even
// in a PR-less local orchestrate where the task/PR finalizer never runs — so a
// dependent integration step (#332) has committed branches to merge. A clean
// worktree (nothing to commit) returns (false, nil). Unlike CommitAll it targets
// an explicit dir and is a no-op (not an error) when there is nothing to commit.
func (c Client) CommitWorktree(ctx context.Context, dir string, message string) (bool, error) {
	dir, err := validateWorktreePath(dir)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(message) == "" {
		message = "Gitmoot delegation commit"
	}
	git := Client{Dir: dir, Runner: c.Runner}
	if _, err := git.run(ctx, "add", "-A"); err != nil {
		return false, err
	}
	// `git diff --cached --quiet` exits 0 when nothing is staged.
	if _, err := git.run(ctx, "diff", "--cached", "--quiet"); err == nil {
		return false, nil
	}
	if _, err := git.run(ctx, "commit", "-m", message); err != nil {
		return false, err
	}
	return true, nil
}

func (c Client) CurrentBranch(ctx context.Context) (string, error) {
	result, err := c.run(ctx, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(result.Stdout)
	if branch == "" {
		return "", errors.New("current git branch is empty")
	}
	return branch, nil
}

func (c Client) PushBranch(ctx context.Context, remote string, branch string) error {
	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}
	if err := validateBranch(branch); err != nil {
		return err
	}
	_, err := c.run(ctx, "push", "-u", remote, branch)
	return err
}

func (c Client) FetchPullRequest(ctx context.Context, remote string, number int) error {
	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}
	if number <= 0 {
		return errors.New("pull request number must be positive")
	}
	_, err := c.run(ctx, "fetch", remote, fmt.Sprintf("pull/%d/head", number))
	return err
}

// FetchRemote refreshes every advertised ref from a named remote. Implement
// base resolution uses it before resolving origin/* so a queued job cannot be
// based on a stale remote-tracking ref.
func (c Client) FetchRemote(ctx context.Context, remote string) error {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = "origin"
	}
	if strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, " \t\r\n") {
		return fmt.Errorf("remote %q is invalid", remote)
	}
	_, err := c.run(ctx, "fetch", remote)
	return err
}

func (c Client) Root(ctx context.Context) (string, error) {
	result, err := c.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(result.Stdout)
	if root == "" {
		return "", errors.New("git root is empty")
	}
	return root, nil
}

func (c Client) OriginRemote(ctx context.Context) (string, error) {
	result, err := c.run(ctx, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	remote := strings.TrimSpace(result.Stdout)
	if remote == "" {
		return "", errors.New("origin remote is empty")
	}
	return remote, nil
}

func (c Client) WorktreeClean(ctx context.Context) (bool, error) {
	result, err := c.run(ctx, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(result.Stdout) == "", nil
}

func (c Client) StatusPorcelain(ctx context.Context) (string, error) {
	result, err := c.run(ctx, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (c Client) CommitAll(ctx context.Context, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return errors.New("commit message is required")
	}
	if _, err := c.run(ctx, "add", "-A"); err != nil {
		return err
	}
	_, err := c.run(ctx, "commit", "-m", message)
	return err
}

func (c Client) HeadSHA(ctx context.Context) (string, error) {
	result, err := c.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(result.Stdout)
	if sha == "" {
		return "", errors.New("git HEAD SHA is empty")
	}
	return sha, nil
}

func (c Client) RevParse(ctx context.Context, rev string) (string, error) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return "", errors.New("git revision is required")
	}
	// Defense-in-depth against argument injection: a rev starting with '-' would be
	// parsed by git as a flag, not a revision. No legitimate revision (SHA, HEAD,
	// HEAD~1, refs/…, owner/branch) starts with '-'. Mirrors validateBranch.
	if strings.HasPrefix(rev, "-") {
		return "", fmt.Errorf("git revision %q must not start with '-'", rev)
	}
	result, err := c.run(ctx, "rev-parse", rev)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(result.Stdout)
	if sha == "" {
		return "", errors.New("git revision SHA is empty")
	}
	return sha, nil
}

// BehindCount reports how many commits upstream has that HEAD does not. It is
// the checkout-side equivalent of `git rev-list --count HEAD..<upstream>`.
func (c Client) BehindCount(ctx context.Context, upstream string) (int, error) {
	upstream = strings.TrimSpace(upstream)
	if err := validateRef(upstream); err != nil {
		return 0, err
	}
	result, err := c.run(ctx, "rev-list", "--count", "HEAD.."+upstream)
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if err != nil || count < 0 {
		return 0, fmt.Errorf("invalid git behind count %q", strings.TrimSpace(result.Stdout))
	}
	return count, nil
}

func (c Client) UpdateBase(ctx context.Context, remote string, branch string) error {
	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}
	if err := validateBranch(branch); err != nil {
		return err
	}
	if _, err := c.run(ctx, "fetch", remote, branch); err != nil {
		return err
	}
	if _, err := c.run(ctx, "switch", branch); err != nil {
		return err
	}
	_, err := c.run(ctx, "pull", "--ff-only", remote, branch)
	return err
}

func (c Client) run(ctx context.Context, args ...string) (subprocess.Result, error) {
	runner := c.Runner
	if runner == nil {
		runner = subprocess.ExecRunner{}
	}
	result, err := runner.Run(ctx, c.Dir, "git", args...)
	if err != nil {
		return result, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return result, nil
}

func validateBranch(branch string) error {
	trimmed := strings.TrimSpace(branch)
	switch {
	case trimmed == "":
		return errors.New("branch is required")
	case trimmed != branch:
		return fmt.Errorf("branch %q must not contain leading or trailing whitespace", branch)
	case strings.HasPrefix(branch, "-"):
		return fmt.Errorf("branch %q must not start with '-'", branch)
	case strings.ContainsAny(branch, " \t\r\n"):
		return fmt.Errorf("branch %q must not contain whitespace", branch)
	case strings.ContainsAny(branch, ":~^?*[\\"):
		return fmt.Errorf("branch %q contains invalid git ref characters", branch)
	case strings.Contains(branch, ".."):
		return fmt.Errorf("branch %q must not contain '..'", branch)
	case strings.Contains(branch, "@{"):
		return fmt.Errorf("branch %q must not contain '@{'", branch)
	case strings.Contains(branch, "//"):
		return fmt.Errorf("branch %q must not contain '//'", branch)
	case strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/"):
		return fmt.Errorf("branch %q must not start or end with '/'", branch)
	case strings.HasSuffix(branch, ".lock"):
		return fmt.Errorf("branch %q must not end with .lock", branch)
	}
	return nil
}

func validateRef(ref string) error {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return errors.New("git ref is required")
	case strings.HasPrefix(ref, "-"):
		return fmt.Errorf("git ref %q must not start with '-'", ref)
	case strings.ContainsAny(ref, " \t\r\n"):
		return fmt.Errorf("git ref %q must not contain whitespace", ref)
	}
	return nil
}

func validateWorktreePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("worktree path is required")
	}
	return path, nil
}
