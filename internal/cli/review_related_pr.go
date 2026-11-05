package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Bounds for one related pull request tree (#2509). They keep a review's
// upload predictable; a larger tree refuses the request rather than silently
// delivering part of it.
const (
	reviewRelatedPRMaxFiles = 50000
	reviewRelatedPRMaxBytes = 256 << 20

	reviewRelatedPRInstalledEventKind = "review_related_pr_installed"
	reviewRelatedPRFetchTimeout       = 5 * time.Minute
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
// on this host and pins the exact head it resolves to. Each fetch lands in a
// private throwaway repository, so no shared checkout is touched and nothing
// persists after the request.
func resolveReviewRelatedPRs(ctx context.Context, fetchURL, repoName string, numbers []int) ([]workflow.ReviewRelatedPR, error) {
	resolved := make([]workflow.ReviewRelatedPR, 0, len(numbers))
	for _, number := range numbers {
		sha, err := resolveReviewRelatedPR(ctx, fetchURL, repoName, number)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, workflow.ReviewRelatedPR{PR: number, HeadSHA: sha})
	}
	return resolved, nil
}

func resolveReviewRelatedPR(ctx context.Context, fetchURL, repoName string, number int) (string, error) {
	gitDir, cleanup, err := newReviewRelatedGitDir()
	if err != nil {
		return "", err
	}
	defer cleanup()
	ref := fmt.Sprintf("refs/pull/%d/head", number)
	if _, err := reviewRelatedGit(ctx, gitDir, fetchURL, repoName, "fetch", "--quiet", "--depth=1", "--no-tags", "--no-recurse-submodules", fetchURL, ref); err != nil {
		return "", fmt.Errorf("--related-pr %d: cannot fetch %s of %s on this host: %w", number, ref, repoName, err)
	}
	out, err := reviewRelatedGit(ctx, gitDir, fetchURL, repoName, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("--related-pr %d: resolve fetched head: %w", number, err)
	}
	sha := strings.ToLower(strings.TrimSpace(out))
	if err := dispatchHeadSHAError(sha); err != nil {
		return "", fmt.Errorf("--related-pr %d: %w", number, err)
	}
	if _, err := listReviewRelatedTree(ctx, gitDir, fetchURL, repoName, sha); err != nil {
		return "", fmt.Errorf("--related-pr %d at %s: %w", number, sha, err)
	}
	return sha, nil
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

func reviewRelatedSafePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, '\\') {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
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
// at run time. Fetching by exact sha means a pull request that moved after
// dispatch still yields the tree that was pinned.
func fetchReviewRelatedTree(ctx context.Context, fetchURL, repoName string, related workflow.ReviewRelatedPR) (string, []reviewRelatedTreeEntry, func(), error) {
	if err := dispatchHeadSHAError(related.HeadSHA); err != nil {
		return "", nil, nil, fmt.Errorf("related pull request #%d: %w", related.PR, err)
	}
	gitDir, cleanup, err := newReviewRelatedGitDir()
	if err != nil {
		return "", nil, nil, err
	}
	if _, err := reviewRelatedGit(ctx, gitDir, fetchURL, repoName, "fetch", "--quiet", "--depth=1", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", fetchURL, related.HeadSHA); err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("related pull request #%d: cannot fetch pinned head %s of %s on this host: %w", related.PR, related.HeadSHA, repoName, err)
	}
	entries, err := listReviewRelatedTree(ctx, gitDir, fetchURL, repoName, related.HeadSHA)
	if err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("related pull request #%d at %s: %w", related.PR, related.HeadSHA, err)
	}
	return gitDir, entries, cleanup, nil
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
// review's own worktree at ReviewRelatedPR.Path(). The returned function
// removes them again so the worktree is pristine for cleanup.
func (w jobWorker) installLocalReviewRelatedPRs(ctx context.Context, jobID string, payload workflow.JobPayload, workspace string) (func(), error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("related pull request trees need the review's checkout")
	}
	root := filepath.Join(workspace, workflow.ReviewRelatedPRsDir)
	if err := removeReviewRelatedTrees(root); err != nil {
		return nil, err
	}
	remove := func() { _ = removeReviewRelatedTrees(root) }
	fetchURL, err := w.reviewRelatedPRSource(ctx, payload.Repo)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
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
		w.recordReviewRelatedPRInstalled(ctx, jobID, related, count, "local")
	}
	if err := os.Chmod(root, 0o555); err != nil {
		remove()
		return nil, err
	}
	return remove, nil
}

func materializeLocalReviewRelatedPR(ctx context.Context, fetchURL, repoName string, related workflow.ReviewRelatedPR, dest string) (int, error) {
	gitDir, entries, cleanup, err := fetchReviewRelatedTree(ctx, fetchURL, repoName, related)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	if err := os.Mkdir(dest, 0o755); err != nil {
		return 0, err
	}
	var dirs []string
	count := 0
	err = walkReviewRelatedTree(ctx, gitDir, entries, func(entry reviewRelatedTreeEntry, body []byte) error {
		target := filepath.Join(dest, filepath.FromSlash(entry.path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		switch {
		case entry.kind == "commit":
			dirs = append(dirs, target)
			return os.Mkdir(target, 0o755)
		case entry.mode == "120000":
			count++
			return os.Symlink(string(body), target)
		default:
			count++
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
	return count, nil
}

// removeReviewRelatedTrees restores write permission on every directory (never
// following a symlink) and removes root.
func removeReviewRelatedTrees(root string) error {
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	_ = filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(name, 0o755)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// remoteReviewRelatedPRScript unpacks one archive into the sandbox workspace,
// deletes the archive, makes the tree read-only and reports what it measured:
// the number of files and symlinks, and the number still writable.
const remoteReviewRelatedPRScript = `set -eu
dest=$1; archive=$2
mkdir -p .gitmoot-related
if [ ! -e .gitmoot-related/.gitignore ]; then printf '*\n' > .gitmoot-related/.gitignore; fi
if [ -e "$dest" ]; then chmod -R u+w "$dest"; rm -rf "$dest"; fi
mkdir "$dest"
tar -x -f "$archive" -C "$dest" --no-same-owner
rm -f "$archive"
chmod -R a-w "$dest" .gitmoot-related/.gitignore
printf '%s %s\n' "$(find "$dest" \( -type f -o -type l \) | wc -l | tr -d ' ')" "$(find "$dest" ! -type l -perm -u=w | wc -l | tr -d ' ')"
`

// installRemoteReviewRelatedPRs streams each pinned tree into an E2B review's
// workspace as a tar archive built on the host from git objects; the sandbox
// receives file bytes only, never a repository, URL or credential.
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
	for _, related := range payload.ReviewRelatedPRs {
		gitDir, entries, cleanup, err := fetchReviewRelatedTree(ctx, fetchURL, payload.Repo, related)
		if err != nil {
			return err
		}
		archive, expected, err := reviewRelatedPRArchive(ctx, gitDir, entries)
		cleanup()
		if err != nil {
			return fmt.Errorf("related pull request #%d: build archive: %w", related.PR, err)
		}
		archivePath := fmt.Sprintf("%s/pr-%d.tar", remoteReviewRelatedPRArchiveDir, related.PR)
		if _, err := installer.InstallInstanceFile(ctx, instance, archivePath, bytes.NewReader(archive), 0o600); err != nil {
			return fmt.Errorf("install related pull request #%d: %w", related.PR, err)
		}
		result, err := runner.Run(ctx, instance.Workspace, "sh", "-c", remoteReviewRelatedPRScript, "related", related.Path(), archivePath)
		if err != nil {
			return fmt.Errorf("unpack related pull request #%d in the sandbox: %w", related.PR, err)
		}
		fields := strings.Fields(result.Stdout)
		if len(fields) != 2 || fields[0] != strconv.Itoa(expected) || fields[1] != "0" {
			return fmt.Errorf("related pull request #%d reads back wrong in the sandbox: want %d read-only files, measured %q", related.PR, expected, strings.TrimSpace(result.Stdout))
		}
		w.recordReviewRelatedPRInstalled(ctx, job.ID, related, expected, "remote")
	}
	return nil
}

func reviewRelatedPRArchive(ctx context.Context, gitDir string, entries []reviewRelatedTreeEntry) ([]byte, int, error) {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	count := 0
	err := walkReviewRelatedTree(ctx, gitDir, entries, func(entry reviewRelatedTreeEntry, body []byte) error {
		header := &tar.Header{Name: entry.path, Mode: 0o644, Format: tar.FormatPAX}
		switch {
		case entry.kind == "commit":
			header.Typeflag, header.Name, header.Mode = tar.TypeDir, entry.path+"/", 0o755
		case entry.mode == "120000":
			count++
			header.Typeflag, header.Linkname, header.Mode = tar.TypeSymlink, string(body), 0o777
		default:
			count++
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
		return nil, 0, err
	}
	if err := writer.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), count, nil
}

func (w jobWorker) recordReviewRelatedPRInstalled(ctx context.Context, jobID string, related workflow.ReviewRelatedPR, files int, backend string) {
	if w.Store == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"pr": related.PR, "head_sha": related.HeadSHA, "path": related.Path(), "files": files, "backend": backend})
	if err := w.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{JobID: jobID, Kind: reviewRelatedPRInstalledEventKind, Message: string(body)}); err != nil {
		writeLine(w.Stdout, "job %s %s event failed: %v", jobID, reviewRelatedPRInstalledEventKind, err)
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
	b.WriteString("Each directory is a plain file tree of that commit: no git history and no credential. It is read-only, outside this pull request's diff, and removed when the review ends. If a directory is missing, say so in your verdict instead of fetching the pull request: this environment has no GitHub credential.")
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
