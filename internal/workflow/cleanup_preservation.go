package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

type cleanupPreservationKey struct{}
type cleanupPreservationProof struct {
	JobID       string
	Generation  int64
	PayloadHash string
}

// ErrPreservationDeferred marks a pre-cleanup collector failure that was
// already recorded durably on the job (#2491), such as a read-only diff
// capture that timed out on a slow filesystem. The workspace is then KEPT, with
// a deferred cleanup obligation, and advancement continues: a missing snapshot
// must never hold back the job's own verdict. A collector returns it only for
// evidence nothing downstream consumes; publication that a dependent stage
// needs keeps failing hard.
var ErrPreservationDeferred = errors.New("cleanup preservation deferred")

// onlyPreservationDeferred reports whether every failure in err (walking
// errors.Join trees) is a deferred preservation, so one real publication
// failure joined beside it still blocks.
func onlyPreservationDeferred(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if child != nil && !onlyPreservationDeferred(child) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, ErrPreservationDeferred)
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
	obligation, err := e.Store.EnsureCleanupObligation(ctx, jobID, payload.WorktreePath, e.now())
	if err != nil {
		return ctx, err
	}
	if reason := e.cleanupRuntimeRefusal(ctx, jobID, payload.WorktreePath); reason != "" {
		return ctx, fmt.Errorf("producer artifact preservation refused: %s", reason)
	}
	if err := e.BeforeReadOnlyWorktreeCleanup(ctx, jobID, job.Type, payload); err != nil {
		if errors.Is(err, db.ErrCleanupEvidenceChanged) {
			return ctx, err
		}
		if onlyPreservationDeferred(err) {
			// The collector already recorded its failure on the job. A capture
			// that failed once tends to fail again, so back off with how long
			// the cleanup has been pending (#2517); transient gates elsewhere
			// keep the one-minute retry.
			now := e.now().UTC()
			_, deferErr := e.Store.DeferCleanupObligation(context.WithoutCancel(ctx), jobID, payload.WorktreePath,
				db.ClassifyCleanupObligationFailure("preserve", err), now, now.Add(PreservationDeferredRetryDelay(obligation.CreatedAt, now)))
			return ctx, errors.Join(err, deferErr)
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

// PreservationDeferredRetryDelay is how long a cleanup deferred because its
// pre-cleanup evidence could not be captured waits before the next attempt
// (#2517): a quarter of how long the cleanup has been pending, between one
// minute and six hours. Before, it retried after one minute forever, so a
// worktree whose capture always timed out was retried on every sweep. An
// unreadable creation time gets the one-minute floor.
func PreservationDeferredRetryDelay(obligationCreatedAt string, now time.Time) time.Duration {
	const floor, ceiling = time.Minute, 6 * time.Hour
	created, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(obligationCreatedAt))
	if err != nil {
		return floor
	}
	return min(max(now.Sub(created)/4, floor), ceiling)
}
