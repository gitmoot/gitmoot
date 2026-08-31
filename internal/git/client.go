package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

type Client struct {
	runner subprocess.Runner
	dir    string
}

// NewClient binds every git command to an explicit subprocess runner. Job paths
// pass a backend-resolved runner; operator tooling that intentionally executes
// on the host uses NewHostClient.
func NewClient(dir string, runner subprocess.Runner) Client {
	return Client{runner: runner, dir: dir}
}

// NewHostClient explicitly selects host execution for non-job CLI and daemon
// administration paths.
func NewHostClient(dir string) Client {
	return NewClient(dir, subprocess.ExecRunner{})
}

// Dir returns the checkout directory bound to this client.
func (c Client) Dir() string { return c.dir }

const maxGitErrorStderrRunes = 4096

type terminalWorktreeRemovalError struct {
	err error
}

func (e terminalWorktreeRemovalError) Error() string {
	return e.err.Error()
}

func (e terminalWorktreeRemovalError) Unwrap() error {
	return e.err
}

// TerminalWorktreeRemoval marks a stale worktree-admin/registered-root mismatch
// that cannot become healthy by retrying the same registered-root command.
func (e terminalWorktreeRemovalError) TerminalWorktreeRemoval() bool {
	return true
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

// CheckoutBranchAt attaches HEAD to branch at ref, replacing a copied local
// branch when necessary. It is intended for a fresh independent clone whose refs
// were copied from the registered checkout but whose writable fix branch must be
// reset to the just-fetched forge head.
func (c Client) CheckoutBranchAt(ctx context.Context, branch string, ref string) error {
	if err := validateBranch(branch); err != nil {
		return err
	}
	if err := validateRef(ref); err != nil {
		return err
	}
	_, err := c.run(ctx, "checkout", "-B", branch, ref)
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

// CloneLocalNoCheckout makes an INDEPENDENT local clone of this repo (c.dir) into
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
	src := strings.TrimSpace(c.dir)
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

func (c Client) SetRemoteURL(ctx context.Context, name string, url string) error {
	name = strings.TrimSpace(name)
	url = strings.TrimSpace(url)
	if name == "" {
		return errors.New("remote name is required")
	}
	if strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("remote name %q is invalid", name)
	}
	if url == "" {
		return errors.New("remote URL is required")
	}
	_, err := c.run(ctx, "remote", "set-url", name, url)
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

// RemoteBranches returns the requested branches that exist on origin using one
// exact-ref ls-remote call. Callers batch a bounded candidate set so stale-task
// reconciliation never performs one subprocess/network round trip per task.
func (c Client) RemoteBranches(ctx context.Context, branches []string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if len(branches) == 0 {
		return out, nil
	}
	args := []string{"ls-remote", "--heads", "origin"}
	requested := make(map[string]string, len(branches))
	for _, branch := range branches {
		branch = strings.TrimSpace(branch)
		if err := validateBranch(branch); err != nil {
			return nil, err
		}
		ref := "refs/heads/" + branch
		args = append(args, ref)
		requested[ref] = branch
	}
	result, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if branch, ok := requested[fields[1]]; ok {
			out[branch] = struct{}{}
		}
	}
	return out, nil
}

func (c Client) RemoveWorktree(ctx context.Context, path string) error {
	return c.removeWorktree(ctx, path, false)
}

// RemoveWorktreeForce removes a worktree even when it has uncommitted or
// untracked changes. It is intended for throwaway worktrees (e.g. detached
// read-only delegation fan-out worktrees) whose contents are never integrated,
// so a runtime that left scratch files behind must not block disposal.
func (c Client) RemoveWorktreeForce(ctx context.Context, path string) error {
	return c.removeWorktree(ctx, path, true)
}

func (c Client) removeWorktree(ctx context.Context, path string, force bool) error {
	path, err := validateWorktreePath(path)
	if err != nil {
		return err
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	result, err := c.run(ctx, args...)
	if err == nil || !strings.Contains(strings.ToLower(result.Stderr), "is not a working tree") {
		return err
	}
	// A path that vanished between the caller's stat and this removal is a race,
	// not a registration failure: the next pass sees an absent path and reconciles
	// it. Classifying it terminal is sticky and permanently retires the candidate.
	if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
		return err
	}
	owner, ownerErr := worktreeOwnerCheckout(path)
	if ownerErr != nil {
		if errors.Is(ownerErr, os.ErrNotExist) {
			return err
		}
		return terminalWorktreeRemovalError{err: fmt.Errorf("%w: resolve owning checkout: %v", err, ownerErr)}
	}
	if filepath.Clean(owner) == filepath.Clean(c.dir) {
		return terminalWorktreeRemovalError{err: err}
	}
	fallbackResult, fallbackErr := NewClient(owner, c.runner).run(ctx, args...)
	if fallbackErr == nil {
		return nil
	}
	if terminalWorktreeRegistrationFailure(fallbackResult.Stderr) {
		return terminalWorktreeRemovalError{err: fmt.Errorf("%w: owning checkout %s also rejected removal: %v", err, owner, fallbackErr)}
	}
	return fallbackErr
}

func terminalWorktreeRegistrationFailure(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "is not a working tree") ||
		strings.Contains(lower, "not a git repository") ||
		strings.Contains(lower, "cannot change to")
}

// PruneWorktrees removes stale administrative entries left by interrupted or
// forcibly removed worktrees. It runs against the primary checkout's shared git
// directory and is idempotent.
func (c Client) PruneWorktrees(ctx context.Context) error {
	_, err := c.run(ctx, "worktree", "prune")
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
	git := NewClient(dir, c.runner)
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
	git := NewClient(dir, c.runner)
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

// IsLinkedWorktree reports whether c.dir is a linked worktree rather than the
// primary checkout. Git 2.31 added --path-format=absolute; older versions fall
// back to resolving git-dir/common-dir relative to the client directory.
func (c Client) IsLinkedWorktree(ctx context.Context) (bool, error) {
	result, err := c.run(ctx, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		result, err = c.run(ctx, "rev-parse", "--git-dir", "--git-common-dir")
		if err != nil {
			return false, err
		}
	}
	paths := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(paths) != 2 {
		return false, fmt.Errorf("git rev-parse returned %d paths, want 2", len(paths))
	}
	gitDir, err := c.absoluteGitPath(paths[0])
	if err != nil {
		return false, err
	}
	commonDir, err := c.absoluteGitPath(paths[1])
	if err != nil {
		return false, err
	}
	return gitDir != commonDir, nil
}

// PrimaryWorktree returns the first non-bare record from git's porcelain
// worktree list. Git writes the primary checkout first. A worktree-only repo
// with no non-bare record falls back to the current checkout.
func (c Client) PrimaryWorktree(ctx context.Context) (string, error) {
	result, err := c.run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	for _, record := range strings.Split(strings.TrimSpace(result.Stdout), "\n\n") {
		var worktree string
		bare := false
		for _, line := range strings.Split(record, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				worktree = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			case strings.TrimSpace(line) == "bare":
				bare = true
			}
		}
		if worktree != "" && !bare {
			absolute, err := c.absoluteGitPath(worktree)
			if err != nil {
				return "", err
			}
			return absolute, nil
		}
	}
	return c.Root(ctx)
}

func (c Client) absoluteGitPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("git path is empty")
	}
	if !filepath.IsAbs(path) {
		base := strings.TrimSpace(c.dir)
		if base == "" {
			base = "."
		}
		path = filepath.Join(base, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
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

// OriginRemoteConfigured returns the literal configured origin URL without Git's
// url.*.insteadOf rewrite. A disposable clone must preserve the forge-facing URL
// in its own config even when the source checkout rewrites transport locally.
func (c Client) OriginRemoteConfigured(ctx context.Context) (string, error) {
	result, err := c.run(ctx, "config", "--get", "remote.origin.url")
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

func (c Client) WorktreeCleanAt(ctx context.Context, path string) (bool, error) {
	return c.worktreeStatusEmptyAt(ctx, path, false)
}

// WorktreePristineAt reports whether a destructive cleanup would preserve all
// tracked, untracked, and ignored content.
func (c Client) WorktreePristineAt(ctx context.Context, path string) (bool, error) {
	return c.worktreeStatusEmptyAt(ctx, path, true)
}

func (c Client) worktreeStatusEmptyAt(ctx context.Context, path string, includeIgnored bool) (bool, error) {
	path, err := validateWorktreePath(path)
	if err != nil {
		return false, err
	}
	args := []string{"status", "--porcelain"}
	if includeIgnored {
		args = append(args, "--ignored")
	}
	result, err := NewClient(path, c.runner).run(ctx, args...)
	if err == nil {
		return strings.TrimSpace(result.Stdout) == "", nil
	}
	gitDir, gitDirErr := worktreeGitDir(path)
	if gitDirErr == nil {
		if _, statErr := os.Stat(gitDir); os.IsNotExist(statErr) {
			return false, terminalWorktreeRemovalError{err: fmt.Errorf("%w: worktree admin directory %s is missing", err, gitDir)}
		}
	} else {
		gitMarker := filepath.Join(path, ".git")
		info, statErr := os.Stat(gitMarker)
		if os.IsNotExist(statErr) || (statErr == nil && !info.IsDir()) {
			return false, terminalWorktreeRemovalError{err: fmt.Errorf("%w: worktree has no valid .git pointer: %v", err, gitDirErr)}
		}
	}
	owner, ownerErr := worktreeOwnerCheckout(path)
	if ownerErr == nil && filepath.Clean(owner) != filepath.Clean(c.dir) {
		return false, terminalWorktreeRemovalError{err: fmt.Errorf("%w: worktree is registered to %s, not %s", err, owner, c.dir)}
	}
	return false, err
}

// cloneReclaimProofNamespace holds the refs fetched from the TRUSTED remote URL
// during a disposable-clone removal proof. It is deliberately separate from
// refs/remotes/origin/*: a disposable clone's own remote configuration is
// writable by whatever ran in it, so origin is not evidence of anything.
const cloneReclaimProofNamespace = "refs/remotes/gitmoot-reclaim-proof/"

// RemoteURL returns the URL of a remote in this client's checkout. The registered
// repository checkout is the trust anchor for disposable-clone removal proofs.
func (c Client) RemoteURL(ctx context.Context, remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = "origin"
	}
	if strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, " \t\r\n") {
		return "", fmt.Errorf("remote %q is invalid", remote)
	}
	result, err := c.run(ctx, "remote", "get-url", remote)
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(result.Stdout)
	if url == "" {
		return "", fmt.Errorf("remote %q has no url", remote)
	}
	return url, nil
}

// RefreshCloneProofRefs mirrors every branch and tag of the trusted remote URL
// into the clone's proof namespace, pruning refs the remote no longer has. It
// takes a URL rather than a remote name so the proof never consults the
// disposable clone's own (mutable) remote configuration.
func (c Client) RefreshCloneProofRefs(ctx context.Context, path string, remoteURL string) error {
	path, err := validateWorktreePath(path)
	if err != nil {
		return err
	}
	remoteURL = strings.TrimSpace(remoteURL)
	switch {
	case remoteURL == "":
		return errors.New("trusted remote url is required")
	case strings.HasPrefix(remoteURL, "-"):
		return fmt.Errorf("trusted remote url %q must not start with '-'", remoteURL)
	case strings.ContainsAny(remoteURL, " \t\r\n"):
		return fmt.Errorf("trusted remote url %q must not contain whitespace", remoteURL)
	}
	_, err = NewClient(path, c.runner).run(ctx, "fetch", "--prune", "--no-write-fetch-head", remoteURL,
		"+refs/heads/*:"+cloneReclaimProofNamespace+"heads/*",
		"+refs/tags/*:"+cloneReclaimProofNamespace+"tags/*",
	)
	return err
}

// CloneOnlyCommit returns the first commit that some local ref or reflog in the
// clone reaches and no ref of the trusted remote contains, or "" when every
// local commit is published. Removing a clone deletes its whole object database,
// so HEAD ancestry alone is not a proof: side branches, tags, stashes and
// reflog-only commits are exactly what a HEAD check misses.
//
// It is local-only, so callers can repeat it immediately before removal.
// RefreshCloneProofRefs must have populated the proof namespace first; an empty
// namespace makes every local commit clone-only, which retains the clone.
func (c Client) CloneOnlyCommit(ctx context.Context, path string) (string, error) {
	path, err := validateWorktreePath(path)
	if err != nil {
		return "", err
	}
	result, err := NewClient(path, c.runner).run(ctx, "rev-list", "--max-count=1", "--all", "--reflog",
		"--not", "--glob="+cloneReclaimProofNamespace+"*")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if sha := strings.TrimSpace(line); sha != "" {
			return sha, nil
		}
	}
	return "", nil
}

func (c Client) StatusPorcelain(ctx context.Context) (string, error) {
	result, err := c.run(ctx, "--no-optional-locks", "status", "--porcelain")
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

func (c Client) HeadSHAAt(ctx context.Context, path string) (string, error) {
	path, err := validateWorktreePath(path)
	if err != nil {
		return "", err
	}
	return NewClient(path, c.runner).HeadSHA(ctx)
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

// CommitExists reports whether rev resolves to a commit in the local object
// database. A missing object is an ordinary false result; repository failures
// remain errors so callers do not misdiagnose corruption as an unfetched ref.
func (c Client) CommitExists(ctx context.Context, rev string) (bool, error) {
	rev = strings.TrimSpace(rev)
	if err := validateRef(rev); err != nil {
		return false, err
	}
	_, err := c.run(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// IsAncestor reports whether ancestor is reachable from descendant. Git uses
// exit status 1 for the ordinary "not an ancestor" result; every other failure
// remains an error so invalid refs and repository failures fail closed.
func (c Client) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	ancestor = strings.TrimSpace(ancestor)
	descendant = strings.TrimSpace(descendant)
	if err := validateRef(ancestor); err != nil {
		return false, err
	}
	if err := validateRef(descendant); err != nil {
		return false, err
	}
	_, err := c.run(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
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
	if c.runner == nil {
		return subprocess.Result{}, errors.New("git subprocess runner is required")
	}
	result, err := c.runner.Run(ctx, c.dir, "git", args...)
	if err != nil {
		if stderr := boundedGitErrorStderr(result.Stderr); stderr != "" {
			return result, fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, stderr)
		}
		return result, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return result, nil
}

func boundedGitErrorStderr(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	runes := []rune(stderr)
	if len(runes) <= maxGitErrorStderrRunes {
		return stderr
	}
	return string(runes[:maxGitErrorStderrRunes]) + "..."
}

// worktreeGitDir resolves the administrative directory named by a linked
// worktree's .git pointer.
func worktreeGitDir(path string) (string, error) {
	data, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", fmt.Errorf("worktree %s has no gitdir pointer", path)
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	return filepath.Clean(gitDir), nil
}

// worktreeOwnerCheckout resolves the repository that owns a linked worktree
// from the worktree's gitdir pointer, even when Client.dir is a different clone.
func worktreeOwnerCheckout(path string) (string, error) {
	gitDir, err := worktreeGitDir(path)
	if err != nil {
		return "", err
	}
	worktreesDir := filepath.Dir(gitDir)
	if filepath.Base(worktreesDir) != "worktrees" {
		return "", fmt.Errorf("worktree %s has unexpected gitdir %s", path, gitDir)
	}
	commonDir := filepath.Dir(worktreesDir)
	if filepath.Base(commonDir) == ".git" {
		return filepath.Dir(commonDir), nil
	}
	return commonDir, nil
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
