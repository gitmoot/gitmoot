package cli

import (
	"context"
	"io"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// runWorkflowAutoSettleOnce performs one home-scoped lifecycle sweep. Candidate
// failures are isolated so one malformed workflow cannot abort daemon
// maintenance or prevent other eligible workflows from settling.
func runWorkflowAutoSettleOnce(ctx context.Context, paths config.Paths, store *db.Store, now time.Time, stdout io.Writer) error {
	policy, err := config.LoadWorkflowLifecycle(paths)
	if err != nil {
		return err
	}
	if policy.AutoSettleAfter <= 0 {
		return nil
	}
	candidates, err := store.ListWorkflowAutoSettleCandidates(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		// Cheap pre-filter using the anchor the candidate query already computed:
		// skip workflows that are not yet quiet so the common (recently active)
		// case never opens a transaction. The authoritative in-tx recheck still
		// recomputes the anchor, so a stale skip only defers a settle by one tick.
		if candidate.QuietAnchor.IsZero() || now.Before(candidate.QuietAnchor) ||
			now.Sub(candidate.QuietAnchor) < policy.AutoSettleAfter {
			continue
		}
		settled, err := store.SettleWorkflowIfEligible(ctx, candidate.WorkflowID, now, policy.AutoSettleAfter)
		if err != nil {
			writeLine(stdout, "workflow auto-settle %s error: %s", candidate.WorkflowID, err)
			continue
		}
		if settled {
			writeLine(stdout, "auto-settled workflow %s", candidate.WorkflowID)
		}
	}
	return nil
}

// runGhostSessionJobReaperOnce performs one home-scoped orphan sweep. Unlike
// workflow auto-settle, a disabled age threshold still leaves the independent
// terminal-workflow signal enabled.
func runGhostSessionJobReaperOnce(ctx context.Context, paths config.Paths, store *db.Store, now time.Time, stdout io.Writer) error {
	policy, err := config.LoadWorkflowLifecycle(paths)
	after := time.Duration(0)
	if err != nil {
		writeLine(stdout, "ghost session-job reaper config error: %s; age-based reaping disabled for this sweep", err)
	} else {
		after = policy.AutoSettleAfter
	}
	reaped, err := store.ReapGhostSessionJobs(ctx, now, after)
	if err != nil {
		return err
	}
	for _, jobID := range reaped {
		writeLine(stdout, "reaped ghost session job %s", jobID)
	}
	return nil
}
