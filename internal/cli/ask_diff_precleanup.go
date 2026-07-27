package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
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

	out := &boundedCountingBuffer{limit: readOnlyWorktreeDiffMaxBytes - readOnlyWorktreeDiffReserve}
	status, err := runReadOnlyWorktreeGit(ctx, worktree, "git status --short", "status", "--short", "--untracked-files=all")
	if err != nil {
		return "", false, err
	}
	diff, err := runReadOnlyWorktreeGit(ctx, worktree, "git diff HEAD", "diff", "--no-ext-diff", "--binary", "HEAD", "--")
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

func runReadOnlyWorktreeGit(ctx context.Context, worktree, label string, args ...string) (*boundedCountingBuffer, error) {
	out := &boundedCountingBuffer{limit: readOnlyWorktreeDiffMaxBytes}
	var stderr boundedCountingBuffer
	stderr.limit = 4096
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = worktree
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%s in %s: %w: %s", label, worktree, err, detail)
		}
		return nil, fmt.Errorf("%s in %s: %w", label, worktree, err)
	}
	return out, nil
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
