package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	readOnlyWorktreeDiffMaxBytes = 4 << 20
	readOnlyWorktreeDiffTimeout  = 10 * time.Second
	readOnlyWorktreeDiffReserve  = 128
)

type beforeReadOnlyWorktreeCleanupHook func(context.Context, string, string, workflow.JobPayload) error

// composeBeforeReadOnlyWorktreeCleanupHooks preserves every pre-cleanup
// collector: one collector failing must not prevent the others from durably
// saving their artifacts before the workflow engine removes the worktree.
func composeBeforeReadOnlyWorktreeCleanupHooks(hooks ...beforeReadOnlyWorktreeCleanupHook) beforeReadOnlyWorktreeCleanupHook {
	return func(ctx context.Context, jobID, jobType string, payload workflow.JobPayload) error {
		var errs []error
		for _, hook := range hooks {
			if hook == nil {
				continue
			}
			if err := hook(ctx, jobID, jobType, payload); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// askReviewDiffPrecleanupHook captures edits from a terminal isolated ask/review
// while its detached worktree still exists. The snapshot rides the existing
// payload JSON so it remains available after synchronous worktree disposal
// without creating a second file-retention lifecycle.
func askReviewDiffPrecleanupHook(store *db.Store) beforeReadOnlyWorktreeCleanupHook {
	return func(ctx context.Context, jobID, jobType string, payload workflow.JobPayload) error {
		if store == nil ||
			(strings.TrimSpace(jobType) != "ask" && strings.TrimSpace(jobType) != "review") ||
			strings.TrimSpace(payload.WorktreePath) == "" {
			return nil
		}

		snapshot, truncated, captureErr := captureReadOnlyWorktreeDiff(ctx, payload.WorktreePath)
		job, loadErr := store.GetJob(ctx, jobID)
		if loadErr != nil {
			return errors.Join(captureErr, fmt.Errorf("load job before persisting read-only worktree diff: %w", loadErr))
		}
		latest, loadErr := daemonJobPayload(job)
		if loadErr != nil {
			return errors.Join(captureErr, loadErr)
		}
		latest.ReadOnlyWorktreeDiff = snapshot
		latest.ReadOnlyWorktreeDiffTruncated = truncated
		latest.ReadOnlyWorktreeDiffError = ""
		if captureErr != nil {
			latest.ReadOnlyWorktreeDiff = ""
			latest.ReadOnlyWorktreeDiffTruncated = false
			latest.ReadOnlyWorktreeDiffError = workflow.RedactCommentText(captureErr.Error())
		}

		encoded, marshalErr := json.Marshal(latest)
		if marshalErr != nil {
			return errors.Join(captureErr, fmt.Errorf("marshal read-only worktree diff metadata: %w", marshalErr))
		}
		persistErr := store.UpdateJobPayload(ctx, jobID, string(encoded))
		if persistErr != nil {
			persistErr = fmt.Errorf("persist read-only worktree diff metadata: %w", persistErr)
		}
		return errors.Join(captureErr, persistErr)
	}
}

func captureReadOnlyWorktreeDiff(ctx context.Context, worktree string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, readOnlyWorktreeDiffTimeout)
	defer cancel()

	sandbox, err := newReadOnlyWorktreeGitSandbox(ctx, worktree)
	if err != nil {
		return "", false, err
	}
	defer sandbox.close()

	out := &boundedCountingBuffer{limit: readOnlyWorktreeDiffMaxBytes - readOnlyWorktreeDiffReserve}
	status, err := sandbox.run(ctx, "git status --short", "status", "--short", "--untracked-files=all", "--ignore-submodules=dirty")
	if err != nil {
		return "", false, err
	}
	diff, err := sandbox.run(ctx, "git diff HEAD", "diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=dirty", "--binary", "HEAD", "--")
	if err != nil {
		return "", false, err
	}
	writeReadOnlyWorktreeDiffSection(out, "git status --short", status)
	writeReadOnlyWorktreeDiffSection(out, "git diff HEAD", diff)

	snapshot := out.String()
	if strings.TrimSpace(snapshot) == "" {
		return "", false, nil
	}
	if out.dropped == 0 {
		return strings.TrimRight(snapshot, "\n") + "\n", false, nil
	}
	marker := fmt.Sprintf("\n[gitmoot: read-only worktree diff truncated; omitted %d bytes]\n", out.dropped)
	return strings.TrimRight(snapshot, "\n") + marker, true, nil
}

// readOnlyWorktreeGitSandbox is a minimal temporary Git directory that reuses
// only the target worktree's index and object database. In particular it does
// not reuse the repository's local config, so attribute-selected clean,
// textconv, and external-diff helpers plus core.fsmonitor cannot execute under
// the daemon's authority during capture.
type readOnlyWorktreeGitSandbox struct {
	dir      string
	worktree string
	env      []string
}

func newReadOnlyWorktreeGitSandbox(ctx context.Context, worktree string) (*readOnlyWorktreeGitSandbox, error) {
	worktree = filepath.Clean(strings.TrimSpace(worktree))
	if !filepath.IsAbs(worktree) {
		absolute, err := filepath.Abs(worktree)
		if err != nil {
			return nil, fmt.Errorf("resolve worktree path: %w", err)
		}
		worktree = absolute
	}
	temp, err := os.MkdirTemp("", "gitmoot-readonly-diff-")
	if err != nil {
		return nil, fmt.Errorf("create isolated git metadata: %w", err)
	}
	sandbox := &readOnlyWorktreeGitSandbox{dir: temp, worktree: worktree}
	ok := false
	defer func() {
		if !ok {
			sandbox.close()
		}
	}()

	baseEnv := sanitizedReadOnlyWorktreeGitEnv(temp)
	head, err := runReadOnlyWorktreeMetadataGit(ctx, baseEnv, worktree, "resolve HEAD", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, err
	}
	indexPath, err := runReadOnlyWorktreeMetadataGit(ctx, baseEnv, worktree, "resolve index", "rev-parse", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	sharedIndexPath, err := runOptionalReadOnlyWorktreeMetadataGit(ctx, baseEnv, worktree, "resolve shared index", "rev-parse", "--shared-index-path")
	if err != nil {
		return nil, err
	}
	objectsPath, err := runReadOnlyWorktreeMetadataGit(ctx, baseEnv, worktree, "resolve object database", "rev-parse", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	indexPath = absoluteGitPath(worktree, indexPath)
	if strings.TrimSpace(sharedIndexPath) != "" {
		sharedIndexPath = absoluteGitPath(worktree, sharedIndexPath)
	}
	objectsPath = absoluteGitPath(worktree, objectsPath)

	if err := os.MkdirAll(filepath.Join(temp, "objects"), 0o700); err != nil {
		return nil, fmt.Errorf("create isolated git object directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(temp, "refs"), 0o700); err != nil {
		return nil, fmt.Errorf("create isolated git refs directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(temp, "HEAD"), []byte(strings.TrimSpace(head)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write isolated git HEAD: %w", err)
	}
	if err := copyReadOnlyWorktreeGitIndex(ctx, indexPath, filepath.Join(temp, "index")); err != nil {
		return nil, err
	}
	if sharedIndexPath != "" {
		name := filepath.Base(sharedIndexPath)
		if !strings.HasPrefix(name, "sharedindex.") || name == "sharedindex." {
			return nil, fmt.Errorf("resolve shared index in %s returned invalid path %q", worktree, sharedIndexPath)
		}
		if err := copyReadOnlyWorktreeGitIndex(ctx, sharedIndexPath, filepath.Join(temp, name)); err != nil {
			return nil, err
		}
	}

	sandbox.env = append(baseEnv,
		"GIT_DIR="+temp,
		"GIT_WORK_TREE="+worktree,
		"GIT_INDEX_FILE="+filepath.Join(temp, "index"),
		"GIT_OBJECT_DIRECTORY="+objectsPath,
	)
	ok = true
	return sandbox, nil
}

func (s *readOnlyWorktreeGitSandbox) close() {
	if s != nil && strings.TrimSpace(s.dir) != "" {
		_ = os.RemoveAll(s.dir)
	}
}

func (s *readOnlyWorktreeGitSandbox) run(ctx context.Context, label string, args ...string) (*boundedCountingBuffer, error) {
	out := &boundedCountingBuffer{limit: readOnlyWorktreeDiffMaxBytes}
	var stderr boundedCountingBuffer
	stderr.limit = 4096
	cmdArgs := append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = s.worktree
	cmd.Env = s.env
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%s in %s: %w: %s", label, s.worktree, err, detail)
		}
		return nil, fmt.Errorf("%s in %s: %w", label, s.worktree, err)
	}
	return out, nil
}

func runReadOnlyWorktreeMetadataGit(ctx context.Context, env []string, worktree, label string, args ...string) (string, error) {
	return runReadOnlyWorktreeMetadataGitOutput(ctx, env, worktree, label, false, args...)
}

func runOptionalReadOnlyWorktreeMetadataGit(ctx context.Context, env []string, worktree, label string, args ...string) (string, error) {
	return runReadOnlyWorktreeMetadataGitOutput(ctx, env, worktree, label, true, args...)
}

func runReadOnlyWorktreeMetadataGitOutput(ctx context.Context, env []string, worktree, label string, allowEmpty bool, args ...string) (string, error) {
	cmdArgs := append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = worktree
	cmd.Env = env
	var stdout, stderr boundedCountingBuffer
	stdout.limit = 64 << 10
	stderr.limit = 4096
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return "", fmt.Errorf("%s in %s: %w: %s", label, worktree, err, detail)
		}
		return "", fmt.Errorf("%s in %s: %w", label, worktree, err)
	}
	value := strings.TrimSpace(stdout.String())
	if (!allowEmpty && value == "") || stdout.dropped != 0 {
		return "", fmt.Errorf("%s in %s returned invalid metadata", label, worktree)
	}
	return value, nil
}

func sanitizedReadOnlyWorktreeGitEnv(tempHome string) []string {
	env := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || key == "HOME" || key == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HOME="+tempHome,
		"XDG_CONFIG_HOME="+tempHome,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
}

func absoluteGitPath(worktree, path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(worktree, path)
}

func copyReadOnlyWorktreeGitIndex(ctx context.Context, source, destination string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("copy worktree index: %w", err)
	}
	return waitForReadOnlyWorktreeGitIndexCopy(ctx, func() error {
		return copyReadOnlyWorktreeGitIndexFile(source, destination)
	})
}

func waitForReadOnlyWorktreeGitIndexCopy(ctx context.Context, copyFile func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- copyFile()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// A filesystem syscall already in progress cannot be force-cancelled
		// portably. Stop waiting so terminal cleanup can proceed; the buffered
		// result lets the copier exit without depending on this caller.
		return fmt.Errorf("copy worktree index: %w", ctx.Err())
	}
}

func copyReadOnlyWorktreeGitIndexFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open worktree index: %w", err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create isolated git index: %w", err)
	}
	keep := false
	defer func() {
		_ = output.Close()
		if !keep {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return fmt.Errorf("copy worktree index: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close isolated git index: %w", err)
	}
	keep = true
	return nil
}

func writeReadOnlyWorktreeDiffSection(out *boundedCountingBuffer, heading string, section *boundedCountingBuffer) {
	if section == nil || strings.TrimSpace(section.String()) == "" {
		return
	}
	_, _ = out.Write([]byte("## " + heading + "\n"))
	_, _ = out.Write([]byte(section.String()))
	if !strings.HasSuffix(section.String(), "\n") {
		_, _ = out.Write([]byte("\n"))
	}
	out.dropped += section.dropped
}

type boundedCountingBuffer struct {
	buf     strings.Builder
	limit   int
	dropped int
}

func (b *boundedCountingBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		keep := len(p)
		if keep > remaining {
			keep = remaining
		}
		_, _ = b.buf.Write(p[:keep])
	}
	if overflow := len(p) - max(remaining, 0); overflow > 0 {
		b.dropped += overflow
	}
	return len(p), nil
}

func (b *boundedCountingBuffer) String() string {
	return b.buf.String()
}
