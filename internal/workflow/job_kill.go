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
	// Best-effort tree cleanup (#479 + #480). The kill's contract — the root flag
	// plus the delegation_killed event — is already committed above, so neither
	// step here may turn a successful kill into an error. This mirrors CancelJob's
	// documented philosophy that incidental lock cleanup must never fail the
	// operation. If the tree walk itself fails, skip cleanup and return the
	// already-killed root.
	if tree, terr := store.ListJobsByRoot(ctx, root.ID); terr == nil {
		for _, j := range tree {
			// (#480) Eagerly terminalize QUEUED, not-yet-started delegation child
			// legs. Guard exactly like daemon.go queuedChildOfKilledRoot: skip the
			// root (rootID == j.ID) and skip continuations (DelegationID == ""),
			// which must still run to drive the #305 graceful finalize. Only real
			// child legs are cancelled. The from=queued transition is idempotent: a
			// re-kill or a leg that raced to running simply does not match.
			if j.State == string(JobQueued) {
				if p, perr := unmarshalPayload(j.Payload); perr == nil {
					rootID := strings.TrimSpace(p.RootJobID)
					if rootID != "" && rootID != j.ID && strings.TrimSpace(p.DelegationID) != "" {
						cancelled, _ := store.TransitionJobStateWithEvent(ctx, j.ID, string(JobQueued), string(JobCancelled), db.JobEvent{
							JobID:   j.ID,
							Kind:    string(JobCancelled),
							Message: fmt.Sprintf("delegation tree rooted at %s killed; queued child leg cancelled before start", root.ID),
						})
						// (#617) A queued implement leg already allocated its
						// per-delegation worktree + branch lock at dispatch (allocation
						// precedes enqueue), and a cancel-before-start bypasses the engine's
						// terminal cleanupImplementDelegationWorktree, so release the branch
						// lock here — symmetric with AllocateDelegationWorktree — or the
						// killed burst strands gitmoot-delegation-* locks that block the next
						// same-repo orchestration. Only when the transition actually landed:
						// a leg that raced to running keeps its lock and releases it via its
						// own terminal cleanup when it finishes. Best-effort.
						if cancelled {
							if released, rerr := releaseDelegationBranchLock(ctx, store, j.Type, p); rerr == nil && released {
								_ = store.AddJobEvent(ctx, db.JobEvent{
									JobID:   j.ID,
									Kind:    "delegation_branch_lock_released",
									Message: fmt.Sprintf("released delegation branch lock %s on delegation kill of tree %s (#617)", strings.TrimSpace(p.Branch), root.ID),
								})
							}
						}
					}
				}
			}
			// (#479) Release any resource/branch locks this tree job owns, but ONLY
			// for jobs that are not still running. Resource locks are job-ID-owned (the
			// runtime-session lock in internal/cli/runtime_lock.go and the
			// checkout-mutation lock in checkout_lock.go), so dropping a RUNNING child's
			// lock while its process is mid-flight would let a competing same-key job (a
			// continuation, or a foreground `agent ask`/`agent run`) acquire it and run
			// concurrently against the same runtime session / working tree — risking
			// session or git-index corruption. The kill is graceful: running children
			// keep executing and release their own locks via their token-scoped deferred
			// ReleaseResourceLock when they finish.
			//
			// The running-or-not test MUST be evaluated atomically against the CURRENT
			// job state, not the j.State captured in the ListJobsByRoot snapshot above:
			// a child selected by an in-flight daemon tick (e.g. the barrier scheduler
			// built it into `pending` before root_killed committed) can transition
			// queued->running AFTER its snapshot row was read. Trusting the stale
			// snapshot state would delete that now-running child's live lock. So we use
			// DeleteResourceLocksByOwnerIfNotRunning, whose `NOT EXISTS (... state=
			// 'running')` clause mirrors DeleteExpiredResourceLocks and skips the delete
			// in the same statement that reads the live state. A just-cancelled queued
			// child (transitioned to cancelled just above) is terminal, so its lock is
			// correctly released; the root and any already-terminal descendants release
			// too — only in-flight (running) legs are kept.
			// The call returns 0 on a re-kill, so the lock_released event is not
			// duplicated. This mirrors CancelJob, which only frees locks for the job it
			// is STOPPING.
			if released, derr := store.DeleteResourceLocksByOwnerIfNotRunning(ctx, j.ID); derr == nil && released > 0 {
				_ = store.AddJobEvent(ctx, db.JobEvent{
					JobID:   j.ID,
					Kind:    "lock_released",
					Message: fmt.Sprintf("released %d resource lock(s) on delegation kill of tree %s", released, root.ID),
				})
			}
		}
	}
	root.RootKilled = true
	return root, nil
}
