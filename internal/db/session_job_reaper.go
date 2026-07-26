package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
)

const (
	// A terminal workflow is a strong orphan signal, but status writes and a live
	// session's CloseExternalJob are separate transactions. Keep one short sweep
	// grace so a normal close racing an operator's terminal status can win first.
	ghostSessionTerminalWorkflowGrace = 5 * time.Minute

	// A store constructed directly in low-level migration tests has no path from
	// which to resolve config. Use the same historical default in that case.
	ghostSessionBackfillAfter = 24 * time.Hour
)

type ghostSessionJobCandidate struct {
	id         string
	workflowID string
	updatedAt  time.Time
}

// ReapGhostSessionJobs cancels orphaned externally-driven jobs using the same
// running-state CAS and durable event for migration cleanup and daemon sweeps.
// A terminal workflow is the fast path; every other workflow state falls back
// to the job's configured inactivity threshold. A non-positive threshold
// disables only that age-based path.
func (s *Store) ReapGhostSessionJobs(ctx context.Context, now time.Time, autoSettleAfter time.Duration) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, workflow_id, updated_at
FROM jobs
WHERE state = 'running' AND externally_driven = 1
ORDER BY updated_at, id`)
	if err != nil {
		return nil, err
	}
	var candidates []ghostSessionJobCandidate
	for rows.Next() {
		var candidate ghostSessionJobCandidate
		var updatedAt string
		if err := rows.Scan(&candidate.id, &candidate.workflowID, &updatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidate.workflowID = strings.TrimSpace(candidate.workflowID)
		candidate.updatedAt = parseStoredTimestamp(updatedAt)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	now = now.UTC()
	reaped := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		reason := ""
		useAgeFallback := candidate.workflowID == ""
		workflowStatus := "none"
		if candidate.workflowID != "" {
			meta, err := s.GetWorkflowMeta(ctx, candidate.workflowID)
			switch {
			case err == nil && IsTerminalWorkflowStatus(meta.Status):
				terminalAt := parseStoredTimestamp(meta.UpdatedAt)
				if terminalAt.IsZero() {
					terminalAt = candidate.updatedAt
				}
				if terminalAt.IsZero() || now.Before(terminalAt) || now.Sub(terminalAt) < ghostSessionTerminalWorkflowGrace {
					continue
				}
				reason = fmt.Sprintf("reaped: parent workflow %s settled", candidate.workflowID)
			case err == nil:
				// A non-terminal workflow does not prove this specific job is
				// alive: a long-lived workflow can move on to newer session jobs
				// without closing an older one. Apply the job-age safety net.
				useAgeFallback = true
				workflowStatus = strings.TrimSpace(meta.Status)
				if workflowStatus == "" {
					workflowStatus = "unset"
				}
			case errors.Is(err, sql.ErrNoRows):
				useAgeFallback = true
			default:
				return reaped, err
			}
		}
		if reason == "" && useAgeFallback {
			if autoSettleAfter <= 0 || candidate.updatedAt.IsZero() || now.Before(candidate.updatedAt) ||
				now.Sub(candidate.updatedAt) < autoSettleAfter {
				continue
			}
			reason = fmt.Sprintf("reaped: no activity for >%s (workflow status: %s)", autoSettleAfter, workflowStatus)
		}
		if reason == "" {
			continue
		}
		transitioned, err := s.TransitionJobStateWithEvent(ctx, candidate.id, "running", "cancelled", JobEvent{
			Kind:    "cancelled",
			Message: reason,
		})
		if err != nil {
			return reaped, err
		}
		if transitioned {
			reaped = append(reaped, candidate.id)
		}
	}
	return reaped, nil
}

// backfillGhostSessionJobs converges the pre-#1125 backlog through the same CAS
// and event-emitting reaper as steady-state maintenance. Re-running it is safe:
// already-cancelled rows no longer match the running candidate query.
func (s *Store) backfillGhostSessionJobs(ctx context.Context) error {
	after := ghostSessionBackfillAfter
	if strings.TrimSpace(s.path) != "" {
		policy, err := config.LoadWorkflowLifecycle(config.Paths{
			ConfigFile: filepath.Join(filepath.Dir(s.path), config.ConfigName),
		})
		if err != nil {
			// A malformed lifecycle policy must never become permission to reap.
			// Preserve the always-safe terminal-workflow signal while disabling
			// age-only cleanup; the daemon's normal sweep reports the config error.
			after = 0
		} else {
			after = policy.AutoSettleAfter
		}
	}
	_, err := s.ReapGhostSessionJobs(ctx, time.Now().UTC(), after)
	return err
}
