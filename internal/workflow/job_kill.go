package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

// KillDelegationTree is the operator kill switch (#341). It marks the delegation
// tree as killed so the engine and daemon stop expanding it, then returns the
// killed root job.
//
// jobID may be ANY job in the tree — the root coordinator or any child/
// continuation. A child carries payload.RootJobID; the kill is always applied to
// that real root, since the engine and daemon only ever consult the root's flag.
// Passing a child id therefore kills the whole tree rather than silently marking a
// row nothing reads.
//
// The kill is graceful, not a hard stop: it does NOT cancel in-flight jobs. The
// flag takes effect at the two expansion points:
//
//   - the engine's dispatchDelegations sees IsRootJobKilled as its first backstop
//     and, on the coordinator's next continuation, routes through the #305 graceful
//     finalize continuation (synthesize what completed → stop) instead of
//     dispatching the next generation; and
//   - the daemon skips queued child legs of a killed root (continuations still run).
//
// In-flight children run to completion normally. The job must exist; an unknown id
// is an error so an operator typo does not silently no-op.
func KillDelegationTree(ctx context.Context, store *db.Store, jobID string) (db.Job, error) {
	if store == nil {
		return db.Job{}, fmt.Errorf("store is required")
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return db.Job{}, fmt.Errorf("job id is required")
	}
	target, err := store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, fmt.Errorf("kill delegation tree %s: %w", jobID, err)
	}
	// Resolve to the real root: a child/continuation carries RootJobID, the root
	// coordinator does not. The engine/daemon only consult the root's flag, so we
	// must mark the root even when the operator passes a descendant id.
	root := target
	if payload, perr := unmarshalPayload(target.Payload); perr == nil {
		if r := strings.TrimSpace(payload.RootJobID); r != "" && r != target.ID {
			root, err = store.GetJob(ctx, r)
			if err != nil {
				return db.Job{}, fmt.Errorf("kill delegation tree: resolve root %s of %s: %w", r, jobID, err)
			}
		}
	}
	if err := store.SetRootJobKilled(ctx, root.ID); err != nil {
		return db.Job{}, err
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID:   root.ID,
		Kind:    "delegation_killed",
		Message: fmt.Sprintf("operator killed delegation tree rooted at %s; new delegations and queued child legs stop, in-flight jobs finish", root.ID),
	}); err != nil {
		return db.Job{}, err
	}
	root.RootKilled = true
	return root, nil
}
