package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/gitmoot/gitmoot/internal/db"
)

type cleanupPreservationKey struct{}
type cleanupPreservationProof struct {
	JobID       string
	Generation  int64
	PayloadHash string
}

// Publication cannot wait for dependent consumers: service stages need their
// producer's deliverables before they can finish. This shared-fenced operation
// preserves results without granting permission to remove the workspace.
func (e Engine) publishCleanupArtifacts(ctx context.Context, jobID string) (context.Context, error) {
	if e.BeforeReadOnlyWorktreeCleanup == nil {
		return ctx, nil
	}
	ctx, release, err := e.Store.AcquireJobLifecycle(ctx)
	if err != nil {
		return ctx, err
	}
	defer release()
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return ctx, err
	}
	if !IsFinalJobState(job.State) || !isReadOnlyDelegationWorktree(job.Type, payload) {
		return ctx, nil
	}
	resource := db.CleanupObligationResourceID(jobID, payload.WorktreePath)
	if _, err := e.Store.GetCleanupReceipt(ctx, resource, job.LifecycleGeneration); err == nil {
		// A prepared receipt already certifies preservation; replay must not
		// overwrite its payload or artifacts, even after partial removal.
		return ctx, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ctx, err
	}
	validator := ValidateDelegationCleanupTarget
	if e.cleanupTargetValidator != nil {
		validator = e.cleanupTargetValidator
	}
	if err := validator(e.Home, jobID, job.Type, payload); err != nil {
		return ctx, err
	}
	if _, err := os.Lstat(payload.WorktreePath); err != nil {
		if os.IsNotExist(err) {
			return ctx, nil
		}
		return ctx, err
	}
	if _, err := e.Store.EnsureCleanupObligation(ctx, jobID, payload.WorktreePath, e.now()); err != nil {
		return ctx, err
	}
	if reason := e.cleanupRuntimeRefusal(ctx, jobID, payload.WorktreePath); reason != "" {
		return ctx, fmt.Errorf("producer artifact preservation refused: %s", reason)
	}
	if err := e.BeforeReadOnlyWorktreeCleanup(ctx, jobID, job.Type, payload); err != nil {
		if errors.Is(err, db.ErrCleanupEvidenceChanged) {
			return ctx, err
		}
		return ctx, errors.Join(err, e.deferDelegationCleanupFailure(ctx, jobID, payload.WorktreePath, "preserve", err))
	}
	current, updated, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return ctx, err
	}
	if current.LifecycleGeneration != job.LifecycleGeneration || current.State != job.State ||
		updated.WorktreePath != payload.WorktreePath || updated.Branch != payload.Branch {
		return ctx, errors.New("producer lifecycle changed during artifact preservation")
	}
	return context.WithValue(ctx, cleanupPreservationKey{}, cleanupPreservationProof{
		JobID: jobID, Generation: current.LifecycleGeneration, PayloadHash: cleanupHash(current.Payload),
	}), nil
}

func cleanupArtifactsPublished(ctx context.Context, job db.Job) bool {
	proof, ok := ctx.Value(cleanupPreservationKey{}).(cleanupPreservationProof)
	return ok && proof.JobID == job.ID && proof.Generation == job.LifecycleGeneration &&
		proof.PayloadHash == cleanupHash(job.Payload)
}

func (e Engine) cleanupRuntimeRefusal(ctx context.Context, jobID, path string) string {
	host, err := os.Hostname()
	if err != nil {
		return "runtime_owner_unknown"
	}
	liveness, err := e.Store.JobRuntimeLockLiveness(ctx, jobID, e.now().UTC(), host, e.ownerPIDLive(), runtimeSelfOwnerToken(ctx))
	if err != nil {
		return "runtime_owner_unknown"
	}
	if liveness != nil && liveness.Active() {
		return "runtime_owner_active"
	}
	if live, known := e.worktreeLiveness(path); live || !known {
		return "process_active_or_unknown"
	}
	return ""
}
