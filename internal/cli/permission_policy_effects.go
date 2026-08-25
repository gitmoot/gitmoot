package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/permissionpolicy"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const permissionPolicyEffectCaptureTimeout = 10 * time.Second

type permissionPolicyEffectObserverAdapter struct {
	next    workflow.DeliveryAdapter
	observe func(context.Context) error
}

func (a permissionPolicyEffectObserverAdapter) Deliver(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	result, err := a.next.Deliver(ctx, agent, job)
	_ = a.observe(context.WithoutCancel(ctx))
	return result, err
}

func (w jobWorker) observePermissionPolicyEffects(adapter workflow.DeliveryAdapter, jobID, checkout string) workflow.DeliveryAdapter {
	return permissionPolicyEffectObserverAdapter{
		next: adapter,
		observe: func(ctx context.Context) error {
			return w.capturePermissionPolicyEffects(ctx, jobID, checkout)
		},
	}
}

// capturePermissionPolicyEffects is best-effort completion instrumentation. It
// updates only an R1 warning claim and never creates an observation of its own.
// AN OBSERVER HAS NO AUTHORITY OVER THE THING IT OBSERVES: callers log the error
// and the adapter wrapper discards it, so capture cannot fail, block, or retry a
// job.
func (w jobWorker) capturePermissionPolicyEffects(ctx context.Context, jobID, checkout string) error {
	job, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		logPermissionPolicyEffectCaptureFailure(w.Stdout, jobID, checkout, err)
		return err
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		logPermissionPolicyEffectCaptureFailure(w.Stdout, jobID, checkout, err)
		return err
	}
	if strings.TrimSpace(checkout) == "" {
		checkout = strings.TrimSpace(payload.WorktreePath)
	}

	var git permissionpolicy.EffectGit
	if checkout != "" {
		if w.PermissionPolicyEffectGit != nil {
			git = w.PermissionPolicyEffectGit(checkout)
		} else {
			runner, runnerErr := w.subprocessRunnerForJob(job)
			if runnerErr != nil {
				return fmt.Errorf("resolve permission-policy effect runner: %w", runnerErr)
			}
			git = jobGitClient(checkout, runner)
		}
	}
	captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), permissionPolicyEffectCaptureTimeout)
	defer cancel()
	_, err = permissionpolicy.RecordEffects(captureCtx, w.Store, jobID, checkout, payload.Branch, payload.PullRequest, git)
	if err != nil {
		err = fmt.Errorf("capture repository effects: %w", err)
		logPermissionPolicyEffectCaptureFailure(w.Stdout, jobID, checkout, err)
	}
	return err
}

func logPermissionPolicyEffectCaptureFailure(stdout io.Writer, jobID, checkout string, err error) {
	writeLine(stdout, "job %s permission-policy effect capture failed (%s): %v", jobID, permissionPolicyEffectCaptureLocation(checkout), err)
}

func permissionPolicyEffectCaptureLocation(checkout string) string {
	path := workflow.RedactCommentText(checkout)
	exists, workTree := "unknown", "unknown"
	if checkout != "" {
		info, err := os.Stat(checkout)
		switch {
		case err == nil:
			exists = "true"
			if !info.IsDir() {
				workTree = "false"
			} else if _, gitErr := os.Stat(filepath.Join(checkout, ".git")); gitErr == nil {
				workTree = "true"
			} else if os.IsNotExist(gitErr) {
				workTree = "false"
			}
		case os.IsNotExist(err):
			exists = "false"
		}
	}
	return fmt.Sprintf("path=%q exists=%s work_tree=%s", path, exists, workTree)
}
