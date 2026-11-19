package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// Host side of a workspace (#2678 slice 2): the host mirror of the work tree
// (`workspace mirror`) and commits out (`workspace push`).

var workspaceCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func runWorkspaceMirror(args []string, stdout, stderr io.Writer) int {
	positionals, home, jsonOutput, code := workspaceFileArgs("workspace mirror", args, stdout, stderr, nil)
	if code >= 0 {
		return code
	}
	if len(positionals) != 2 || positionals[1] == "" {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "usage: gitmoot workspace mirror <id> <dir>"))
	}
	dir, err := filepath.Abs(positionals[1])
	if err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "mirror directory: %v", err))
	}
	var token string
	err = withReadyWorkspace(home, positionals[0], func(ctx context.Context, store *db.Store, _ config.Paths, workspace db.Workspace, sandbox workspaceSandbox) *workspaceError {
		var failure *workspaceError
		token, failure = mirrorWorkspace(ctx, store, workspace, sandbox, dir)
		return failure
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, err)
	}
	if jsonOutput {
		return writeWorkspaceJSON(stdout, stderr, map[string]string{"token": token, "dir": dir})
	}
	fmt.Fprintln(stdout, token)
	return 0
}

// mirrorWorkspace fills dir (absent or empty) with the work tree: the
// workspace's base commit from the host clone, then whatever `changes
// --since start` and git report as different, pulled from the VM. Only the
// differences cross the network. It returns the changes token the mirror
// is current at.
func mirrorWorkspace(ctx context.Context, store *db.Store, workspace db.Workspace, sandbox workspaceSandbox, dir string) (string, *workspaceError) {
	record, err := store.GetRepo(ctx, workspace.Repo)
	if err != nil {
		return "", wsErrorf(wsErrNotFound, "repository %s is not registered with gitmoot: %v", workspace.Repo, err)
	}
	checkout := strings.TrimSpace(record.CheckoutPath)
	if checkout == "" {
		return "", wsErrorf(wsErrNotFound, "repository %s has no registered checkout", workspace.Repo)
	}
	if failure := prepareWorkspaceMirrorDir(dir); failure != nil {
		return "", failure
	}
	if err := workspaceMirrorBase(ctx, checkout, workspace.Base, dir); err != nil {
		return "", asWorkspaceError(err, wsErrInternal)
	}

	var listing bytes.Buffer
	if failure := workspaceChangesOp(ctx, sandbox, workspace, "start", &listing); failure != nil {
		return "", failure
	}
	var changes workspaceChanges
	if err := json.Unmarshal(listing.Bytes(), &changes); err != nil {
		return "", wsErrorf(wsErrInternal, "workspace changes printed invalid JSON: %v", err)
	}
	// Run after the listing: anything edited since is reported by git too,
	// and pulled; never the other way round.
	var diff bytes.Buffer
	if failure := runWorkspaceFileOp(ctx, sandbox, workspace, map[string]any{"op": "basediff", "base": workspace.Base}, nil, &diff); failure != nil {
		return "", failure
	}
	differs := map[string]bool{}
	for _, rel := range strings.Split(diff.String(), "\x00") {
		if rel != "" {
			differs[rel] = true
		}
	}

	inVM := make(map[string]workspaceChangeEntry, len(changes.Changed))
	for _, entry := range changes.Changed {
		inVM[entry.Path] = entry
	}
	if err := pruneWorkspaceMirror(dir, inVM); err != nil {
		return "", asWorkspaceError(err, wsErrInternal)
	}
	var pull []string
	var dirModes []workspaceChangeEntry
	for _, entry := range changes.Changed {
		host := filepath.Join(dir, filepath.FromSlash(entry.Path))
		info, statErr := os.Lstat(host)
		same := statErr == nil && !differs[entry.Path]
		switch entry.Type {
		case "dir":
			dirModes = append(dirModes, entry)
			if statErr != nil || !info.IsDir() {
				pull = append(pull, entry.Path)
			}
		case "file":
			if same && info.Mode().IsRegular() && info.Size() == entry.Size {
				if err := os.Chmod(host, fs.FileMode(entry.Mode&0o7777)); err != nil {
					return "", asWorkspaceError(err, wsErrInternal)
				}
				continue
			}
			pull = append(pull, entry.Path)
		case "symlink":
			if !same || info.Mode()&fs.ModeSymlink == 0 {
				pull = append(pull, entry.Path)
			}
		}
	}
	for start := 0; start < len(pull); start += workspaceMirrorBatch {
		batch := pull[start:min(start+workspaceMirrorBatch, len(pull))]
		if failure := pullIntoWorkspaceMirror(ctx, sandbox, workspace, dir, batch); failure != nil {
			return "", failure
		}
	}
	// Directory modes last, deepest first, so a read-only directory is
	// filled before it is closed.
	for i := len(dirModes) - 1; i >= 0; i-- {
		host := filepath.Join(dir, filepath.FromSlash(dirModes[i].Path))
		if err := os.Chmod(host, fs.FileMode(dirModes[i].Mode&0o7777)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", asWorkspaceError(err, wsErrInternal)
		}
	}
	return changes.Token, nil
}

func prepareWorkspaceMirrorDir(dir string) *workspaceError {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return wsErrorf(wsErrInvalidArgs, "create the mirror directory: %v", err)
		}
		return nil
	}
	if err != nil {
		return wsErrorf(wsErrInvalidArgs, "mirror directory: %v", err)
	}
	if !info.IsDir() {
		return wsErrorf(wsErrInvalidArgs, "mirror directory %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return wsErrorf(wsErrInvalidArgs, "mirror directory: %v", err)
	}
	if len(entries) != 0 {
		return wsErrorf(wsErrInvalidArgs, "mirror directory %s is not empty", dir)
	}
	return nil
}

// pruneWorkspaceMirror removes every mirror entry the VM does not have.
func pruneWorkspaceMirror(dir string, inVM map[string]workspaceChangeEntry) error {
	return filepath.WalkDir(dir, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if file == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, file)
		if err != nil {
			return err
		}
		if _, ok := inVM[filepath.ToSlash(rel)]; ok {
			return nil
		}
		if err := os.RemoveAll(file); err != nil {
			return err
		}
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

func pullIntoWorkspaceMirror(ctx context.Context, sandbox workspaceSandbox, workspace db.Workspace, dir string, paths []string) *workspaceError {
	reader, writer := io.Pipe()
	done := make(chan *workspaceError, 1)
	go func() {
		failure := runWorkspaceFileOp(ctx, sandbox, workspace, map[string]any{"op": "pull", "paths": paths}, nil, writer)
		if failure != nil {
			_ = writer.CloseWithError(failure)
		} else {
			_ = writer.Close()
		}
		done <- failure
	}()
	extractFailure := extractWorkspaceTar(dir, reader)
	// Drain what is left, so the operation never blocks on a reader that
	// stopped early.
	_, _ = io.Copy(io.Discard, reader)
	if failure := <-done; failure != nil {
		return failure
	}
	return extractFailure
}

// extractWorkspaceTar applies a tar stream (a workspace pull or a host
// `git archive`) to dir: each entry replaced atomically, with its permission
// bits. A name that leaves dir, or a path through a symlink, is refused.
func extractWorkspaceTar(dir string, input io.Reader) *workspaceError {
	reader := tar.NewReader(input)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return wsErrorf(wsErrInternal, "read the tar stream: %v", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		parts, failure := workspaceTarParts(header.Name)
		if failure != nil {
			return failure
		}
		parent, err := workspaceMirrorParent(dir, parts[:len(parts)-1])
		if err != nil {
			return asWorkspaceError(err, wsErrInternal)
		}
		target := filepath.Join(parent, parts[len(parts)-1])
		mode := fs.FileMode(header.Mode & 0o777)
		var placeErr error
		switch header.Typeflag {
		case tar.TypeDir:
			placeErr = placeWorkspaceMirrorDir(target, mode)
		case tar.TypeReg:
			placeErr = placeWorkspaceMirrorFile(parent, target, reader, header.Mode&0o7777)
		case tar.TypeSymlink:
			placeErr = placeWorkspaceMirrorSymlink(parent, target, header.Linkname)
		default:
			continue
		}
		if placeErr != nil {
			return wsErrorf(wsErrInternal, "mirror %s: %v", header.Name, placeErr)
		}
	}
}

// workspaceMirrorParent makes the directories parts names below dir,
// refusing a symlink or non-directory on the way.
func workspaceMirrorParent(dir string, parts []string) (string, error) {
	current := dir
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", current)
		}
	}
	return current, nil
}

func placeWorkspaceMirrorDir(target string, mode fs.FileMode) error {
	info, err := os.Lstat(target)
	if err == nil && info.IsDir() {
		return nil
	}
	if err == nil {
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	return os.Mkdir(target, mode|0o700)
}

// clearForMirrorEntry removes a directory where a file or symlink goes.
func clearForMirrorEntry(target string) error {
	if info, err := os.Lstat(target); err == nil && info.IsDir() {
		return os.RemoveAll(target)
	}
	return nil
}

func mirrorTempName(parent string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return filepath.Join(parent, ".gitmoot-mirror-"+hex.EncodeToString(raw[:])), nil
}

func placeWorkspaceMirrorFile(parent, target string, content io.Reader, mode int64) error {
	if err := clearForMirrorEntry(target); err != nil {
		return err
	}
	temp, err := mirrorTempName(parent)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, content)
	chmodErr := file.Chmod(workspaceGoMode(mode))
	if err := errors.Join(copyErr, chmodErr, file.Close()); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// workspaceGoMode converts Unix permission bits (with setuid, setgid and
// sticky) to an fs.FileMode.
func workspaceGoMode(mode int64) fs.FileMode {
	out := fs.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		out |= fs.ModeSetuid
	}
	if mode&0o2000 != 0 {
		out |= fs.ModeSetgid
	}
	if mode&0o1000 != 0 {
		out |= fs.ModeSticky
	}
	return out
}

func placeWorkspaceMirrorSymlink(parent, target, linkname string) error {
	if err := clearForMirrorEntry(target); err != nil {
		return err
	}
	temp, err := mirrorTempName(parent)
	if err != nil {
		return err
	}
	if err := os.Symlink(linkname, temp); err != nil {
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// workspaceMirrorBase writes the tree of commit base from the registered
// host clone into dir: every blob exactly as stored (no export, eol or
// filter attribute applied, so nothing differs from the object the VM
// checked out but its checkout conversions, which the size check and git's
// diff catch), with its mode, and symlinks as symlinks. Gitlinks are left
// to the VM listing.
func workspaceMirrorBase(ctx context.Context, checkout, base, dir string) error {
	if !workspaceCommitSHA.MatchString(base) {
		return fmt.Errorf("workspace base %q is not a commit id", base)
	}
	listing := exec.CommandContext(ctx, "git", "-C", checkout, "ls-tree", "-r", "-z", "--full-tree", base)
	var listErr bytes.Buffer
	listing.Stderr = &listErr
	tree, err := listing.Output()
	if err != nil {
		return fmt.Errorf("git ls-tree %s in %s: %w: %s", base, checkout, err, strings.TrimSpace(listErr.String()))
	}
	type blob struct{ mode, sha, path string }
	var blobs []blob
	for _, record := range strings.Split(string(tree), "\x00") {
		if record == "" {
			continue
		}
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return fmt.Errorf("git ls-tree printed %q", record)
		}
		if fields[1] == "blob" {
			blobs = append(blobs, blob{mode: fields[0], sha: fields[2], path: name})
		}
	}
	if len(blobs) == 0 {
		return nil
	}
	reader := exec.CommandContext(ctx, "git", "-C", checkout, "cat-file", "--batch")
	var readErr bytes.Buffer
	reader.Stderr = &readErr
	requests, err := reader.StdinPipe()
	if err != nil {
		return err
	}
	answers, err := reader.StdoutPipe()
	if err != nil {
		return err
	}
	if err := reader.Start(); err != nil {
		return err
	}
	go func() {
		for _, entry := range blobs {
			if _, err := io.WriteString(requests, entry.sha+"\n"); err != nil {
				break
			}
		}
		_ = requests.Close()
	}()
	extractErr := func() error {
		buffered := bufio.NewReaderSize(answers, 64<<10)
		for _, entry := range blobs {
			header, err := buffered.ReadString('\n')
			if err != nil {
				return fmt.Errorf("git cat-file: %w", err)
			}
			fields := strings.Fields(header)
			if len(fields) != 3 || fields[0] != entry.sha || fields[1] != "blob" {
				return fmt.Errorf("git cat-file answered %q for %s", strings.TrimSpace(header), entry.sha)
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return fmt.Errorf("git cat-file answered %q for %s", strings.TrimSpace(header), entry.sha)
			}
			parts, failure := workspaceTarParts(entry.path)
			if failure != nil {
				return failure
			}
			parent, err := workspaceMirrorParent(dir, parts[:len(parts)-1])
			if err != nil {
				return err
			}
			target := filepath.Join(parent, parts[len(parts)-1])
			content := io.LimitReader(buffered, size)
			switch entry.mode {
			case "120000":
				link, err := io.ReadAll(content)
				if err == nil {
					err = placeWorkspaceMirrorSymlink(parent, target, string(link))
				}
				if err != nil {
					return err
				}
			default:
				mode := int64(0o644)
				if entry.mode == "100755" {
					mode = 0o755
				}
				if err := placeWorkspaceMirrorFile(parent, target, content, mode); err != nil {
					return err
				}
			}
			if newline, err := buffered.ReadByte(); err != nil || newline != '\n' {
				return fmt.Errorf("git cat-file: blob %s is not followed by a newline", entry.sha)
			}
		}
		return nil
	}()
	_, _ = io.Copy(io.Discard, answers)
	if err := reader.Wait(); err != nil {
		return errors.Join(fmt.Errorf("git cat-file --batch in %s: %w: %s", checkout, err, strings.TrimSpace(readErr.String())), extractErr)
	}
	return extractErr
}

// workspacePushTarget is the only branch a workspace pushes: a PR
// workspace's head branch, or ws/<seat>/<issue>.
func workspacePushTarget(workspace db.Workspace) (string, *workspaceError) {
	branch := workspace.Branch
	if workspace.PR <= 0 {
		expected := fmt.Sprintf("ws/%s/%d", workspace.Seat, workspace.Issue)
		if branch != expected {
			return "", wsErrorf(wsErrPushRefused, "issue workspace %s has branch %q, not %q", workspace.ID, branch, expected)
		}
	}
	if branch == "" || strings.HasPrefix(branch, "-") {
		return "", wsErrorf(wsErrPushRefused, "workspace %s has no pushable branch", workspace.ID)
	}
	return branch, nil
}

// pushWorkspaceCommits copies the workspace branch's new commits out of the
// VM as a git bundle (no credential needed or present in the VM), checks they
// descend from the recorded base, and pushes them from the host clone with
// the host's own identity and credentials: a fast-forward of the workspace
// branch only, never a force.
func pushWorkspaceCommits(ctx context.Context, store *db.Store, paths config.Paths, workspace db.Workspace, sandbox workspaceSandbox) (string, *workspaceError) {
	target, failure := workspacePushTarget(workspace)
	if failure != nil {
		return "", failure
	}
	if !workspaceCommitSHA.MatchString(workspace.Base) {
		return "", wsErrorf(wsErrInternal, "workspace %s has no recorded base commit", workspace.ID)
	}
	record, err := store.GetRepo(ctx, workspace.Repo)
	if err != nil {
		return "", wsErrorf(wsErrNotFound, "repository %s is not registered with gitmoot: %v", workspace.Repo, err)
	}
	checkout := strings.TrimSpace(record.CheckoutPath)
	if checkout == "" {
		return "", wsErrorf(wsErrNotFound, "repository %s has no registered checkout", workspace.Repo)
	}
	if _, err := workspacePushGit(ctx, checkout, "check-ref-format", "refs/heads/"+target); err != nil {
		return "", wsErrorf(wsErrPushRefused, "%q is not a valid branch name", target)
	}
	hostDir, err := workspaceHostDir(paths, workspace.ID)
	if err != nil {
		return "", asWorkspaceError(err, wsErrInternal)
	}
	bundle, err := os.CreateTemp(hostDir, "push-*.bundle")
	if err != nil {
		return "", asWorkspaceError(err, wsErrInternal)
	}
	bundlePath := bundle.Name()
	defer os.Remove(bundlePath)
	failure = runWorkspaceFileOp(ctx, sandbox, workspace, map[string]any{"op": "bundle", "branch": workspace.Branch, "base": workspace.Base}, nil, bundle)
	if closeErr := bundle.Close(); failure == nil && closeErr != nil {
		failure = asWorkspaceError(closeErr, wsErrInternal)
	}
	if failure != nil {
		return "", failure
	}
	heads, err := workspacePushGit(ctx, checkout, "bundle", "list-heads", bundlePath)
	if err != nil {
		return "", wsErrorf(wsErrPushRefused, "the workspace bundle is not valid: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(heads), "\n")
	fields := strings.Fields(lines[0])
	if len(lines) != 1 || len(fields) != 2 || fields[1] != "refs/heads/"+workspace.Branch || !workspaceCommitSHA.MatchString(fields[0]) {
		return "", wsErrorf(wsErrPushRefused, "the workspace bundle must carry exactly refs/heads/%s, got %q", workspace.Branch, strings.TrimSpace(heads))
	}
	tip := fields[0]
	if _, err := workspacePushGit(ctx, checkout, "-c", "gc.auto=0", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", bundlePath, "refs/heads/"+workspace.Branch); err != nil {
		return "", wsErrorf(wsErrPushRefused, "fetch the workspace commits into %s: %v", checkout, err)
	}
	if _, err := workspacePushGit(ctx, checkout, "merge-base", "--is-ancestor", workspace.Base, tip); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", wsErrorf(wsErrPushRefused, "%s does not descend from the workspace base %s; rewritten history is never pushed", tip, workspace.Base)
		}
		return "", wsErrorf(wsErrInternal, "check %s descends from %s: %v", tip, workspace.Base, err)
	}
	// No '+' and no --force: the remote takes a fast-forward of exactly this
	// branch or nothing.
	if _, err := workspacePushGit(ctx, checkout, "push", "--quiet", "--no-follow-tags", "--recurse-submodules=no", "origin", tip+":refs/heads/"+target); err != nil {
		return "", wsErrorf(wsErrPushRefused, "push %s to %s: %v", tip, target, err)
	}
	return tip, nil
}

// workspacePushGit runs git in the registered host clone for commits out,
// with the host's environment (identity, credential helpers) and no
// terminal prompt. Its error wraps *exec.ExitError.
func workspacePushGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
