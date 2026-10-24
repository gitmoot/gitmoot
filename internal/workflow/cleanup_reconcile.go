package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// reconcileCleanupReceipt handles historical outcomes and accounting-only
// recovery before consulting current deletion policy. It never deletes a path
// or branch. A replacement lifecycle owns its remaining resources; an old token
// can close that old episode only as superseded, not as a successful removal.
func (e Engine) reconcileCleanupReceipt(ctx context.Context, jobID, resourceID, token string) (result CleanupApplyResult, handled bool, err error) {
	result = CleanupApplyResult{SchemaVersion: 1, State: "refused"}
	var receipt db.CleanupReceipt
	var owner db.Job
	if token != "" {
		receipt, err = e.Store.CleanupReceiptForToken(ctx, token)
	} else {
		var payload JobPayload
		owner, payload, err = e.jobPayload(ctx, jobID)
		if err != nil {
			return result, true, err
		}
		resourceID = db.CleanupObligationResourceID(jobID, payload.WorktreePath)
		receipt, err = e.Store.GetCleanupReceipt(ctx, resourceID, owner.LifecycleGeneration)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, true, err
	}
	if receipt.OwnerJobID != jobID || receipt.ResourceID != resourceID {
		return result, true, errors.New("cleanup token belongs to another resource")
	}
	result.Receipt = &receipt
	if receipt.RemovedAt != "" {
		result.State = "already_removed"
		return result, true, nil
	}
	if receipt.SupersededAt != "" {
		result.State = "superseded"
		return result, true, nil
	}
	if owner.ID == "" {
		owner, err = e.Store.GetJob(ctx, jobID)
		if err != nil {
			return result, true, err
		}
	}
	if owner.LifecycleGeneration > receipt.LifecycleGeneration {
		settled, err := e.Store.SupersedeCleanupReceipt(ctx, receipt, owner.LifecycleGeneration, e.now())
		if err != nil {
			return result, true, err
		}
		result.State, result.Receipt = "superseded", &settled
		return result, true, nil
	}
	if owner.LifecycleGeneration != receipt.LifecycleGeneration || owner.State != receipt.TerminalState || owner.Payload != receipt.OwnerPayload {
		return result, false, nil
	}
	payload, err := unmarshalPayload(receipt.OwnerPayload)
	if err != nil {
		return result, true, err
	}
	validator := ValidateDelegationCleanupTarget
	if e.cleanupTargetValidator != nil {
		validator = e.cleanupTargetValidator
	}
	if err := validator(e.Home, jobID, owner.Type, payload); err != nil {
		return result, true, err
	}
	if payload.WorktreePath != receipt.ExpectedPath || db.CleanupObligationResourceID(jobID, payload.WorktreePath) != receipt.ResourceID {
		return result, true, errors.New("cleanup receipt resource identity changed")
	}
	if _, err := os.Lstat(receipt.ExpectedPath); !os.IsNotExist(err) {
		if err != nil {
			return result, true, err
		}
		return result, false, nil
	}
	obligation, err := e.Store.GetCleanupObligation(ctx, receipt.ResourceID)
	if err != nil {
		return result, true, err
	}
	result.Decision = CleanupInspection{SchemaVersion: 1, Producer: "gitmoot", DecisionOwner: "gitmoot",
		ResourceID: receipt.ResourceID, ResourceKind: "delegation_worktree", OwnerJobID: jobID,
		OwnerState: owner.State, LifecycleGeneration: receipt.LifecycleGeneration,
		ExpectedPath: receipt.ExpectedPath, Receipt: &receipt, Obligation: &obligation}
	if obligation.LifecycleGeneration != receipt.LifecycleGeneration {
		return result, false, nil
	}
	if obligation.State == db.CleanupObligationQuarantined {
		result.Decision = result.Decision.refuse("quarantined")
		return result, true, nil
	}
	if due, err := time.Parse(time.RFC3339Nano, obligation.NextAttemptAt); err != nil || due.After(e.now()) {
		result.Decision = result.Decision.refuse("retry_not_due")
		return result, true, nil
	}
	if payload.CleanupArtifacts != nil {
		if err := ValidateCleanupArtifactSnapshot(e.Home, receipt.ResourceID, receipt.LifecycleGeneration, *payload.CleanupArtifacts); err != nil {
			result.Decision = result.Decision.refuse("artifact_evidence_unavailable")
			return result, true, nil
		}
	}
	release, _, err := acquireCheckoutMutationLockWithWait(ctx, e.Store, e.DelegationCheckout, "cleanup-reconcile:"+jobID, time.Now().UTC())
	if err != nil {
		return result, true, err
	}
	defer func() {
		err = errors.Join(err, release(context.WithoutCancel(ctx)))
	}()
	inspector, ok := e.DelegationWorktrees.(cleanupWorktreeInspector)
	if !ok {
		return result, false, nil
	}
	identity, err := inspector.InspectWorktree(ctx, receipt.ExpectedPath)
	if err != nil {
		return result, true, err
	}
	// A remaining registration or branch requires current deletion admission,
	// not a historical authorization. The normal path will revalidate it.
	if identity.Registered {
		return result, false, nil
	}
	if isImplementDelegationWorktree(owner.Type, payload) {
		checker, ok := e.DelegationWorktrees.(BranchExistenceChecker)
		if !ok {
			return result, false, nil
		}
		exists, err := checker.BranchExists(ctx, payload.Branch)
		if err != nil {
			return result, true, err
		}
		if exists {
			return result, false, nil
		}
		lock, err := e.Store.GetBranchLock(ctx, payload.Repo, payload.Branch)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, true, err
		}
		if err == nil && lock.Owner != cleanupBranchOwner(owner, payload) {
			result.Decision = result.Decision.refuse("branch_owner_changed")
			return result, true, nil
		}
		if _, err := releaseDelegationBranchLock(ctx, e.Store, owner.Type, payload); err != nil {
			return result, true, err
		}
	}
	result.State = "retryable"
	completed, err := e.finishProducerCleanupReceipt(ctx, receipt)
	if err != nil {
		return result, true, err
	}
	result.State, result.Receipt = "removed", &completed
	return result, true, nil
}

func (e Engine) finishProducerCleanupReceipt(ctx context.Context, receipt db.CleanupReceipt) (db.CleanupReceipt, error) {
	var evidence cleanupEvidence
	if err := json.Unmarshal([]byte(receipt.Evidence), &evidence); err != nil {
		return db.CleanupReceipt{}, err
	}
	kind := "delegation_worktree_removed"
	if evidence.Policy.Mode == "aged" {
		kind = "delegation_worktree_reclaimed_ttl"
	}
	if err := e.Store.AddJobEventIfAbsent(ctx, db.JobEvent{JobID: receipt.OwnerJobID, Kind: kind,
		Message: fmt.Sprintf("producer cleanup %s completed for %s", receipt.ReceiptID, receipt.ExpectedPath)}); err != nil {
		return db.CleanupReceipt{}, err
	}
	return e.Store.CompleteCleanupReceipt(ctx, receipt.ResourceID, receipt.LifecycleGeneration, receipt.ReceiptID, e.now())
}
