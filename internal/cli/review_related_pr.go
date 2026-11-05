package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Bounds for one related pull request tree (#2509). They keep a review's
// upload predictable; a larger tree refuses the request rather than silently
// delivering part of it.
const (
	reviewRelatedPRMaxFiles     = 50000
	reviewRelatedPRMaxBytes     = 256 << 20
	reviewRelatedPRMaxLinkHops  = 40
	reviewRelatedPRFetchTimeout = 5 * time.Minute

	reviewRelatedPRInstalledEventKind = "review_related_pr_installed"
	reviewRelatedPRVerifiedEventKind  = "review_related_pr_verified"
	reviewRelatedPRAlteredEventKind   = "review_related_pr_altered"

	// reviewRelatedPROwnerFile marks a .gitmoot-related directory as created by
	// one job, so removal never deletes a directory the checkout brought.
	reviewRelatedPROwnerFile = ".gitmoot-owner"
)

// remoteReviewRelatedPRArchiveDir is where the host streams each related tree
// archive before it is unpacked into the sandbox workspace and deleted.
const remoteReviewRelatedPRArchiveDir = execbackend.RuntimeMaterialDir + "/related"

type reviewRelatedTreeEntry struct {
	mode   string
	kind   string
	object string
	size   int64
	path   string
}

// reviewRelatedTree is one fetched, validated tree in a private host
// repository, with the digest of exactly what will be materialized.
type reviewRelatedTree struct {
	gitDir  string
	entries []reviewRelatedTreeEntry
	digest  string
	files   int
}

// reviewRelatedPRFetchURL is the host-side fetch location of the reviewed
// repository. It is used only by host git, which authenticates with the
// daemon's own credential helper; it never enters a payload or a sandbox.
func reviewRelatedPRFetchURL(record db.Repo, repo github.Repository) string {
	if remote := strings.TrimSpace(record.RemoteURL); remote != "" {
		return remote
	}
	return "https://github.com/" + repo.FullName() + ".git"
}

// resolveReviewRelatedPRs fetches refs/pull/N/head of each related pull request
// on this host and pins the exact head it resolves to and its tree digest. Each
// fetch lands in a private throwaway repository, so no shared checkout is
// touched and nothing persists after the request.
func resolveReviewRelatedPRs(ctx context.Context, fetchURL, repoName string, numbers []int) ([]workflow.ReviewRelatedPR, error) {
	resolved := make([]workflow.ReviewRelatedPR, 0, len(numbers))
	for _, number := range numbers {
		related, err := resolveReviewRelatedPR(ctx, fetchURL, repoName, number)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, related)
	}
	return resolved, nil
}

func resolveReviewRelatedPR(ctx context.Context, fetchURL, repoName string, number int) (workflow.ReviewRelatedPR, error) {
	gitDir, cleanup, err := newReviewRelatedGitDir()
	if err != nil {
		return workflow.ReviewRelatedPR{}, err
	}
	defer cleanup()
	ref := fmt.Sprintf("refs/pull/%d/head", number)
	if _, err := reviewRelatedGit(ctx, gitDir, fetchURL, repoName, "fetch", "--quiet", "--depth=1", "--no-tags", "--no-recurse-submodules", fetchURL, ref); err != nil {
		return workflow.ReviewRelatedPR{}, fmt.Errorf("--related-pr %d: cannot fetch %s of %s on this host: %w", number, ref, repoName, err)
	}
	out, err := reviewRelatedGit(ctx, gitDir, fetchURL, repoName, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return workflow.ReviewRelatedPR{}, fmt.Errorf("--related-pr %d: resolve fetched head: %w", number, err)
	}
	sha := strings.ToLower(strings.TrimSpace(out))
	if err := dispatchHeadSHAError(sha); err != nil {
		return workflow.ReviewRelatedPR{}, fmt.Errorf("--related-pr %d: %w", number, err)
	}
	tree, err := inspectReviewRelatedTree(ctx, gitDir, fetchURL, repoName, sha)
	if err != nil {
		return workflow.ReviewRelatedPR{}, fmt.Errorf("--related-pr %d at %s: %w", number, sha, err)
	}
	return workflow.ReviewRelatedPR{PR: number, HeadSHA: sha, TreeSHA256: tree.digest}, nil
}

func newReviewRelatedGitDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "gitmoot-related-pr-")
	if err != nil {
		return "", nil, fmt.Errorf("create related pull request fetch dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if _, err := reviewRelatedGit(context.Background(), dir, "", "", "init", "--quiet", "--bare", "--template="); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// reviewRelatedGit runs host git in a private repository. Prompts are disabled
// so a missing credential fails instead of hanging, and the fetch URL is
// replaced by the repository name in any error text so a URL carrying a token
// is never echoed into a job event.
func reviewRelatedGit(ctx context.Context, gitDir, fetchURL, repoName string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, reviewRelatedPRFetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", gitDir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if fetchURL != "" {
			detail = strings.ReplaceAll(detail, fetchURL, repoName)
		}
		if detail == "" {
			detail = err.Error()
		}
		return "", errors.New(detail)
	}
	return stdout.String(), nil
}

// inspectReviewRelatedTree lists sha's tree, enforces bounds and path safety,
// reads every blob once to compute the tree digest, and refuses any symlink
// whose target resolves outside the tree.
func inspectReviewRelatedTree(ctx context.Context, gitDir, fetchURL, repoName, sha string) (reviewRelatedTree, error) {
	entries, err := listReviewRelatedTree(ctx, gitDir, fetchURL, repoName, sha)
	if err != nil {
		return reviewRelatedTree{}, err
	}
	digest := newReviewRelatedDigest()
	links := map[string]string{}
	files := 0
	err = walkReviewRelatedTree(ctx, gitDir, entries, func(entry reviewRelatedTreeEntry, body []byte) error {
		switch {
		case entry.kind == "commit":
			digest.add("d", nil, entry.path)
		case entry.mode == "120000":
			files++
			links[entry.path] = string(body)
			digest.add("l", body, entry.path)
		default:
			files++
			digest.add("f", body, entry.path)
		}
		return nil
	})
	if err != nil {
		return reviewRelatedTree{}, err
	}
	if err := validateReviewRelatedSymlinks(links); err != nil {
		return reviewRelatedTree{}, err
	}
	return reviewRelatedTree{gitDir: gitDir, entries: entries, digest: digest.sum(), files: files}, nil
}

// listReviewRelatedTree enumerates every entry of sha's tree and enforces the
// bounds and path safety. Submodules become empty directories.
func listReviewRelatedTree(ctx context.Context, gitDir, fetchURL, repoName, sha string) ([]reviewRelatedTreeEntry, error) {
	out, err := reviewRelatedGit(ctx, gitDir, fetchURL, repoName, "ls-tree", "-r", "-l", "-z", "--full-tree", sha)
	if err != nil {
		return nil, fmt.Errorf("list tree: %w", err)
	}
	var entries []reviewRelatedTreeEntry
	var total int64
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return nil, fmt.Errorf("unexpected ls-tree entry %q", record)
		}
		if !reviewRelatedSafePath(name) {
			return nil, fmt.Errorf("tree path %q is not a safe relative path", name)
		}
		entry := reviewRelatedTreeEntry{mode: fields[0], kind: fields[1], object: fields[2], path: name}
		if entry.kind == "blob" {
			entry.size, err = strconv.ParseInt(fields[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("tree entry %q size: %w", name, err)
			}
			total += entry.size
		}
		entries = append(entries, entry)
		if len(entries) > reviewRelatedPRMaxFiles {
			return nil, fmt.Errorf("tree has more than %d entries", reviewRelatedPRMaxFiles)
		}
		if total > reviewRelatedPRMaxBytes {
			return nil, fmt.Errorf("tree is larger than %d MiB", reviewRelatedPRMaxBytes>>20)
		}
	}
	return entries, nil
}

// reviewRelatedSafePath accepts a clean relative path with no .git component
// and no control characters, so it can be written, archived and listed one
// per line by the sandbox measurement without ambiguity.
func reviewRelatedSafePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, '\\') || reviewRelatedHasControl(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

func reviewRelatedHasControl(value string) bool {
	for i := range len(value) {
		if value[i] < 0x20 || value[i] == 0x7f {
			return true
		}
	}
	return false
}

// validateReviewRelatedSymlinks refuses the tree when any symlink's target,
// following chains through other symlinks of the same tree, resolves outside
// the tree root. Absolute targets are refused outright.
func validateReviewRelatedSymlinks(links map[string]string) error {
	names := make([]string, 0, len(links))
	for name := range links {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := resolveReviewRelatedSymlink(links, name); err != nil {
			return fmt.Errorf("symlink %q -> %q: %w", name, links[name], err)
		}
	}
	return nil
}

func resolveReviewRelatedSymlink(links map[string]string, name string) error {
	target := links[name]
	if err := reviewRelatedLinkTargetError(target); err != nil {
		return err
	}
	var stack []string
	if dir := path.Dir(name); dir != "." {
		stack = strings.Split(dir, "/")
	}
	pending := strings.Split(target, "/")
	for hops := 0; len(pending) > 0; {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return errors.New("resolves outside the pull request tree")
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, component)
		next, isLink := links[strings.Join(stack, "/")]
		if !isLink {
			continue
		}
		if hops++; hops > reviewRelatedPRMaxLinkHops {
			return errors.New("symlink chain is too long or loops")
		}
		if err := reviewRelatedLinkTargetError(next); err != nil {
			return err
		}
		stack = stack[:len(stack)-1]
		pending = append(strings.Split(next, "/"), pending...)
	}
	return nil
}

func reviewRelatedLinkTargetError(target string) error {
	switch {
	case target == "":
		return errors.New("empty symlink target")
	case strings.HasPrefix(target, "/"):
		return errors.New("absolute symlink target")
	case strings.ContainsRune(target, '\\') || reviewRelatedHasControl(target):
		return errors.New("symlink target has unsafe characters")
	}
	return nil
}

// reviewRelatedDigest is the tree digest the host pins and the sandbox
// re-measures: one line "<kind> <sha256|-> <path>\n" per directory, file and
// symlink (a symlink hashes its target), sorted by path, then sha256 of all
// lines. remoteReviewRelatedPRDigestShell computes the same value in a sandbox.
type reviewRelatedDigest struct {
	lines map[string]string
}

func newReviewRelatedDigest() *reviewRelatedDigest {
	return &reviewRelatedDigest{lines: map[string]string{}}
}

func (d *reviewRelatedDigest) add(kind string, body []byte, name string) {
	sum := "-"
	if kind != "d" {
		raw := sha256.Sum256(body)
		sum = hex.EncodeToString(raw[:])
	}
	d.lines[name] = kind + " " + sum + " " + name + "\n"
	for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
		if _, ok := d.lines[dir]; !ok {
			d.lines[dir] = "d - " + dir + "\n"
		}
	}
}

func (d *reviewRelatedDigest) sum() string {
	names := make([]string, 0, len(d.lines))
	for name := range d.lines {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		_, _ = io.WriteString(hash, d.lines[name])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// walkReviewRelatedTree streams every entry's exact bytes from git's object
// store (no checkout, no attributes, no filters) to visit. Symlink content is
// the link target; a submodule has no content.
func walkReviewRelatedTree(ctx context.Context, gitDir string, entries []reviewRelatedTreeEntry, visit func(reviewRelatedTreeEntry, []byte) error) error {
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "cat-file", "--batch")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("read related tree objects: %w", err)
	}
	reader := bufio.NewReader(stdoutPipe)
	walkErr := func() error {
		for _, entry := range entries {
			if entry.kind != "blob" {
				if err := visit(entry, nil); err != nil {
					return err
				}
				continue
			}
			if _, err := io.WriteString(stdin, entry.object+"\n"); err != nil {
				return err
			}
			header, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("read object %s: %w", entry.object, err)
			}
			fields := strings.Fields(header)
			if len(fields) != 3 || fields[0] != entry.object || fields[1] != "blob" {
				return fmt.Errorf("object %s is missing from the fetched tree", entry.object)
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil || size != entry.size {
				return fmt.Errorf("object %s size mismatch", entry.object)
			}
			body := make([]byte, size+1)
			if _, err := io.ReadFull(reader, body); err != nil {
				return fmt.Errorf("read object %s: %w", entry.object, err)
			}
			if err := visit(entry, body[:size]); err != nil {
				return err
			}
		}
		return nil
	}()
	_ = stdin.Close()
	waitErr := cmd.Wait()
	if walkErr != nil {
		return walkErr
	}
	return waitErr
}

// fetchReviewRelatedTree re-fetches the pinned head into a private repository
// at run time and checks it against the digest pinned at dispatch. Fetching by
// exact sha means a pull request that moved after dispatch still yields the
// tree that was pinned.
func fetchReviewRelatedTree(ctx context.Context, fetchURL, repoName string, related workflow.ReviewRelatedPR) (reviewRelatedTree, func(), error) {
	if err := dispatchHeadSHAError(related.HeadSHA); err != nil {
		return reviewRelatedTree{}, nil, fmt.Errorf("related pull request #%d: %w", related.PR, err)
	}
	if strings.TrimSpace(related.TreeSHA256) == "" {
		return reviewRelatedTree{}, nil, fmt.Errorf("related pull request #%d has no pinned tree digest; request the review again", related.PR)
	}
	gitDir, cleanup, err := newReviewRelatedGitDir()
	if err != nil {
		return reviewRelatedTree{}, nil, err
	}
	if _, err := reviewRelatedGit(ctx, gitDir, fetchURL, repoName, "fetch", "--quiet", "--depth=1", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", fetchURL, related.HeadSHA); err != nil {
		cleanup()
		return reviewRelatedTree{}, nil, fmt.Errorf("related pull request #%d: cannot fetch pinned head %s of %s on this host: %w", related.PR, related.HeadSHA, repoName, err)
	}
	tree, err := inspectReviewRelatedTree(ctx, gitDir, fetchURL, repoName, related.HeadSHA)
	if err != nil {
		cleanup()
		return reviewRelatedTree{}, nil, fmt.Errorf("related pull request #%d at %s: %w", related.PR, related.HeadSHA, err)
	}
	if tree.digest != related.TreeSHA256 {
		cleanup()
		return reviewRelatedTree{}, nil, fmt.Errorf("related pull request #%d at %s: fetched tree digest %s differs from the pinned %s", related.PR, related.HeadSHA, tree.digest, related.TreeSHA256)
	}
	return tree, cleanup, nil
}

func (w jobWorker) reviewRelatedPRSource(ctx context.Context, repoName string) (string, error) {
	repo, err := github.ParseRepository(repoName)
	if err != nil {
		return "", err
	}
	record := db.Repo{}
	if w.Store != nil {
		if found, err := w.Store.GetRepo(ctx, repo.FullName()); err == nil {
			record = found
		}
	}
	return reviewRelatedPRFetchURL(record, repo), nil
}

// installLocalReviewRelatedPRs writes each pinned tree read-only into the
// review's own worktree at ReviewRelatedPR.Path(). It refuses, touching
// nothing, when the checkout already has a .gitmoot-related entry it did not
// create for this job. The returned function removes the trees again.
func (w jobWorker) installLocalReviewRelatedPRs(ctx context.Context, jobID string, payload workflow.JobPayload, workspace string) (func(), error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("related pull request trees need the review's checkout")
	}
	root := filepath.Join(workspace, workflow.ReviewRelatedPRsDir)
	if _, err := os.Lstat(root); err == nil {
		// A previous attempt of this same job may have left its own trees.
		removed, removeErr := removeOwnedReviewRelatedTrees(workspace, jobID)
		if removeErr != nil {
			return nil, removeErr
		}
		if !removed {
			return nil, fmt.Errorf("the reviewed checkout already contains %s, which gitmoot did not create; refusing to install related pull request trees there", workflow.ReviewRelatedPRsDir)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	fetchURL, err := w.reviewRelatedPRSource(ctx, payload.Repo)
	if err != nil {
		return nil, err
	}
	// Mkdir never follows a final-component symlink: it fails if anything is
	// already there. Lstat then proves it is the real directory just created.
	if err := os.Mkdir(root, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", workflow.ReviewRelatedPRsDir, err)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is not the directory gitmoot created", workflow.ReviewRelatedPRsDir)
	}
	if err := os.WriteFile(filepath.Join(root, reviewRelatedPROwnerFile), []byte(jobID+"\n"), 0o444); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	remove := func() { _, _ = removeOwnedReviewRelatedTrees(workspace, jobID) }
	// Untracked trees stay out of the review worktree's git status.
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*\n"), 0o444); err != nil {
		remove()
		return nil, err
	}
	for _, related := range payload.ReviewRelatedPRs {
		count, err := materializeLocalReviewRelatedPR(ctx, fetchURL, payload.Repo, related, filepath.Join(workspace, filepath.FromSlash(related.Path())))
		if err != nil {
			remove()
			return nil, err
		}
		w.recordReviewRelatedPREvent(ctx, jobID, reviewRelatedPRInstalledEventKind, related, count, "local", related.TreeSHA256)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		remove()
		return nil, err
	}
	return remove, nil
}

func materializeLocalReviewRelatedPR(ctx context.Context, fetchURL, repoName string, related workflow.ReviewRelatedPR, dest string) (int, error) {
	tree, cleanup, err := fetchReviewRelatedTree(ctx, fetchURL, repoName, related)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	if err := os.Mkdir(dest, 0o755); err != nil {
		return 0, err
	}
	var dirs []string
	err = walkReviewRelatedTree(ctx, tree.gitDir, tree.entries, func(entry reviewRelatedTreeEntry, body []byte) error {
		target := filepath.Join(dest, filepath.FromSlash(entry.path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		switch {
		case entry.kind == "commit":
			dirs = append(dirs, target)
			return os.Mkdir(target, 0o755)
		case entry.mode == "120000":
			return os.Symlink(string(body), target)
		default:
			mode := os.FileMode(0o444)
			if entry.mode == "100755" {
				mode = 0o555
			}
			return os.WriteFile(target, body, mode)
		}
	})
	if err != nil {
		return 0, fmt.Errorf("related pull request #%d: write tree: %w", related.PR, err)
	}
	// Directories become read-only last, deepest first.
	err = filepath.WalkDir(dest, func(name string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, name)
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			return 0, err
		}
	}
	return tree.files, nil
}

// removeOwnedReviewRelatedTrees removes workspace/.gitmoot-related only when it
// is a real directory whose owner marker names jobID; anything else is left
// untouched and reported as not removed. Write bits are restored first (never
// following a symlink) so an unprivileged daemon can delete the 0555 tree.
func removeOwnedReviewRelatedTrees(workspace, jobID string) (bool, error) {
	root := filepath.Join(workspace, workflow.ReviewRelatedPRsDir)
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return false, nil
	}
	marker := filepath.Join(root, reviewRelatedPROwnerFile)
	markerInfo, err := os.Lstat(marker)
	if err != nil || !markerInfo.Mode().IsRegular() {
		return false, nil
	}
	owner, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(owner)) != strings.TrimSpace(jobID) || strings.TrimSpace(jobID) == "" {
		return false, nil
	}
	_ = filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(name, 0o755)
		}
		return nil
	})
	if err := os.RemoveAll(root); err != nil {
		return false, fmt.Errorf("remove related pull request trees: %w", err)
	}
	return true, nil
}

// reviewRelatedPRPrecleanupHook removes a terminal review's related trees
// BEFORE its worktree is disposed of: the 0555 directories would otherwise
// block an unprivileged `git worktree remove --force` and strand the trash.
func reviewRelatedPRPrecleanupHook() beforeReadOnlyWorktreeCleanupHook {
	return func(_ context.Context, jobID, jobType string, payload workflow.JobPayload) error {
		if strings.TrimSpace(jobType) != "review" || len(payload.ReviewRelatedPRs) == 0 || strings.TrimSpace(payload.WorktreePath) == "" {
			return nil
		}
		_, err := removeOwnedReviewRelatedTrees(payload.WorktreePath, jobID)
		return err
	}
}

// remoteReviewRelatedPRDigestShell defines digest(), the sandbox computation
// of reviewRelatedDigest for one installed tree.
const remoteReviewRelatedPRDigestShell = `digest() {
  (cd "$1" && LC_ALL=C find . -mindepth 1 -print | LC_ALL=C sort | while IFS= read -r p; do
    r=${p#./}
    if [ -L "$p" ]; then printf 'l %s %s\n' "$(printf '%s' "$(readlink -- "$p")" | sha256sum | cut -d' ' -f1)" "$r"
    elif [ -d "$p" ]; then printf 'd - %s\n' "$r"
    elif [ -f "$p" ]; then printf 'f %s %s\n' "$(sha256sum < "$p" | cut -d' ' -f1)" "$r"
    else printf 'o - %s\n' "$r"; fi
  done) | sha256sum | cut -d' ' -f1
}
`

// remoteReviewRelatedPRPrepareScript creates the related root in the sandbox
// workspace. Anything already there came from the reviewed checkout, so it is
// refused before anything is created, extracted, changed or removed.
const remoteReviewRelatedPRPrepareScript = `set -eu
if [ -e .gitmoot-related ] || [ -L .gitmoot-related ]; then
  echo "the reviewed checkout already contains .gitmoot-related; refusing to install related pull request trees there" >&2
  exit 3
fi
mkdir .gitmoot-related
[ -d .gitmoot-related ] && [ ! -L .gitmoot-related ]
printf '*\n' > .gitmoot-related/.gitignore
chmod a-w .gitmoot-related/.gitignore
`

// remoteReviewRelatedPRUnpackScript unpacks one archive into a new directory
// under the verified root, deletes the archive, makes the tree read-only and
// reports the file and symlink count, the writable count and the digest.
const remoteReviewRelatedPRUnpackScript = `set -eu
` + remoteReviewRelatedPRDigestShell + `dest=$1; archive=$2
[ -d .gitmoot-related ] && [ ! -L .gitmoot-related ]
mkdir "$dest"
tar -x -f "$archive" -C "$dest" --no-same-owner
rm -f "$archive"
chmod -R a-w "$dest"
printf '%s %s %s\n' "$(find "$dest" \( -type f -o -type l \) | wc -l | tr -d ' ')" "$(find "$dest" ! -type l -perm -u=w | wc -l | tr -d ' ')" "$(digest "$dest")"
`

// remoteReviewRelatedPRMeasureScript prints "<digest> <path>" for each tree.
const remoteReviewRelatedPRMeasureScript = `set -eu
` + remoteReviewRelatedPRDigestShell + `for d in "$@"; do printf '%s %s\n' "$(digest "$d")" "$d"; done
`

// installRemoteReviewRelatedPRs streams each pinned tree into an E2B review's
// workspace as a tar archive built on the host from git objects; the sandbox
// receives file bytes only, never a repository, URL or credential. The sandbox
// user owns the files and could restore write bits, so the digest measured
// here is checked against the pin, and guardRemoteReviewRelatedPRs measures it
// again after the review ran.
func (w jobWorker) installRemoteReviewRelatedPRs(ctx context.Context, lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance, job db.Job) error {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return err
	}
	if len(payload.ReviewRelatedPRs) == 0 {
		return nil
	}
	if !reviewEvidenceProviderAllowed(payload.ExecProvider) {
		return fmt.Errorf("related pull request trees are delivered only to local and E2B reviews; this job targets provider %q", payload.ExecProvider)
	}
	installer, ok := lifecycle.(execbackend.InstanceFileInstaller)
	if !ok {
		return fmt.Errorf("execution backend %q cannot install related pull request trees", lifecycle.Name())
	}
	fetchURL, err := w.reviewRelatedPRSource(ctx, payload.Repo)
	if err != nil {
		return err
	}
	runner := execbackend.InstanceRunner{Backend: lifecycle, Instance: instance}
	if _, err := runner.Run(ctx, instance.Workspace, "sh", "-c", remoteReviewRelatedPRPrepareScript, "related"); err != nil {
		return fmt.Errorf("prepare related pull request trees in the sandbox: %w", err)
	}
	for _, related := range payload.ReviewRelatedPRs {
		tree, cleanup, err := fetchReviewRelatedTree(ctx, fetchURL, payload.Repo, related)
		if err != nil {
			return err
		}
		archive, err := reviewRelatedPRArchive(ctx, tree)
		cleanup()
		if err != nil {
			return fmt.Errorf("related pull request #%d: build archive: %w", related.PR, err)
		}
		archivePath := fmt.Sprintf("%s/pr-%d.tar", remoteReviewRelatedPRArchiveDir, related.PR)
		if _, err := installer.InstallInstanceFile(ctx, instance, archivePath, bytes.NewReader(archive), 0o600); err != nil {
			return fmt.Errorf("install related pull request #%d: %w", related.PR, err)
		}
		result, err := runner.Run(ctx, instance.Workspace, "sh", "-c", remoteReviewRelatedPRUnpackScript, "related", related.Path(), archivePath)
		if err != nil {
			return fmt.Errorf("unpack related pull request #%d in the sandbox: %w", related.PR, err)
		}
		fields := strings.Fields(result.Stdout)
		if len(fields) != 3 || fields[0] != strconv.Itoa(tree.files) || fields[1] != "0" || fields[2] != related.TreeSHA256 {
			return fmt.Errorf("related pull request #%d reads back wrong in the sandbox: want %d read-only files with digest %s, measured %q", related.PR, tree.files, related.TreeSHA256, strings.TrimSpace(result.Stdout))
		}
		w.recordReviewRelatedPREvent(ctx, job.ID, reviewRelatedPRInstalledEventKind, related, tree.files, "remote", fields[2])
	}
	return nil
}

func reviewRelatedPRArchive(ctx context.Context, tree reviewRelatedTree) ([]byte, error) {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	err := walkReviewRelatedTree(ctx, tree.gitDir, tree.entries, func(entry reviewRelatedTreeEntry, body []byte) error {
		header := &tar.Header{Name: entry.path, Mode: 0o644, Format: tar.FormatPAX}
		switch {
		case entry.kind == "commit":
			header.Typeflag, header.Name, header.Mode = tar.TypeDir, entry.path+"/", 0o755
		case entry.mode == "120000":
			header.Typeflag, header.Linkname, header.Mode = tar.TypeSymlink, string(body), 0o777
		default:
			header.Typeflag, header.Size = tar.TypeReg, int64(len(body))
			if entry.mode == "100755" {
				header.Mode = 0o755
			}
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeReg {
			_, err := writer.Write(body)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// verifyRemoteReviewRelatedPRs re-measures every installed tree in the sandbox
// and fails when any digest differs from its pin. It always records an event.
func (w jobWorker) verifyRemoteReviewRelatedPRs(ctx context.Context, runner execbackend.InstanceRunner, instance *execbackend.Instance, jobID string, related []workflow.ReviewRelatedPR) error {
	paths := make([]string, 0, len(related))
	for _, entry := range related {
		paths = append(paths, entry.Path())
	}
	result, measureErr := runner.Run(ctx, instance.Workspace, "sh", append([]string{"-c", remoteReviewRelatedPRMeasureScript, "measure"}, paths...)...)
	measured := map[string]string{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if digest, name, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			measured[name] = digest
		}
	}
	var altered []string
	for _, entry := range related {
		kind := reviewRelatedPRVerifiedEventKind
		if measured[entry.Path()] != entry.TreeSHA256 {
			kind = reviewRelatedPRAlteredEventKind
			altered = append(altered, fmt.Sprintf("#%d (%s)", entry.PR, entry.Path()))
		}
		w.recordReviewRelatedPREvent(ctx, jobID, kind, entry, 0, "remote", measured[entry.Path()])
	}
	if measureErr != nil {
		return fmt.Errorf("related pull request trees could not be measured in the sandbox after the review: %w", measureErr)
	}
	if len(altered) > 0 {
		return fmt.Errorf("related pull request trees changed in the sandbox during the review, so its verdict is rejected: %s", strings.Join(altered, ", "))
	}
	return nil
}

// guardRemoteReviewRelatedPRs re-verifies the related trees after every
// delivery turn, the same change-detection-with-rejection rule private
// evidence uses (#2476).
func (w jobWorker) guardRemoteReviewRelatedPRs(adapter workflow.DeliveryAdapter, lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance, jobID string, payload workflow.JobPayload) workflow.DeliveryAdapter {
	if len(payload.ReviewRelatedPRs) == 0 || lifecycle == nil || instance == nil {
		return adapter
	}
	runner := execbackend.InstanceRunner{Backend: lifecycle, Instance: instance}
	related := payload.ReviewRelatedPRs
	return reviewEvidenceGuardAdapter{inner: adapter, verify: func(ctx context.Context) error {
		return w.verifyRemoteReviewRelatedPRs(ctx, runner, instance, jobID, related)
	}, inspect: func(context.Context, runtime.Agent) {}}
}

func (w jobWorker) recordReviewRelatedPREvent(ctx context.Context, jobID, kind string, related workflow.ReviewRelatedPR, files int, backend, measured string) {
	if w.Store == nil {
		return
	}
	fields := map[string]any{"pr": related.PR, "head_sha": related.HeadSHA, "path": related.Path(), "backend": backend, "tree_sha256": related.TreeSHA256, "measured_sha256": measured}
	if files > 0 {
		fields["files"] = files
	}
	body, _ := json.Marshal(fields)
	if err := w.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{JobID: jobID, Kind: kind, Message: string(body)}); err != nil {
		writeLine(w.Stdout, "job %s %s event failed: %v", jobID, kind, err)
	}
}

// reviewRelatedPRInstructions names each related pull request, its pinned head
// and where its tree is, and says what the reviewer may do with it.
func reviewRelatedPRInstructions(related []workflow.ReviewRelatedPR) string {
	if len(related) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("RELATED PULL REQUESTS (read-only, #2509): the requester attached the exact head tree of these same-repository pull requests so you can check this change against them. They are NOT part of the change under review: do not review or edit them, and do not cite their files as this pull request's code.\n")
	for _, entry := range related {
		fmt.Fprintf(&b, "- #%d at %s: %s/ (relative to the root of your checkout)\n", entry.PR, entry.HeadSHA, entry.Path())
	}
	b.WriteString("Each directory is a plain file tree of that commit: no git history and no credential. It is read-only and its contents are re-verified after you finish: changing it rejects the review. If a directory is missing, say so in your verdict instead of fetching the pull request: this environment has no GitHub credential.")
	return b.String()
}

func sameReviewRelatedPRs(a, b []workflow.ReviewRelatedPR) bool {
	if len(a) != len(b) {
		return false
	}
	want := make(map[int]string, len(a))
	for _, entry := range a {
		want[entry.PR] = strings.ToLower(entry.HeadSHA)
	}
	for _, entry := range b {
		if sha, ok := want[entry.PR]; !ok || sha != strings.ToLower(entry.HeadSHA) {
			return false
		}
	}
	return true
}

func describeReviewRelatedPRs(related []workflow.ReviewRelatedPR) string {
	if len(related) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(related))
	for _, entry := range related {
		parts = append(parts, fmt.Sprintf("#%d@%s", entry.PR, entry.HeadSHA[:min(12, len(entry.HeadSHA))]))
	}
	return strings.Join(parts, ", ")
}
