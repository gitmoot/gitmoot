package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// reconcileCleanupReceipt handles historical outcomes and accounting-only
// recovery before consulting current deletion policy. It never deletes a path
// or branch. A replacement lifecycle owns its remaining resources; an old token
// can close that old episode only as superseded, not as a successful removal.
func (e Engine) reconcileCleanupReceipt(ctx context.Context, jobID, resourceID, token, budgetGate string) (result CleanupApplyResult, handled bool, err error) {
	result = CleanupApplyResult{SchemaVersion: CleanupSchemaVersion, State: "refused"}
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
	// Accounting-only recovery never measures or deletes, so it carries the caller's gate and no
	// budget or measurement; the decision is still a complete schema-v2 shape.
	result.Decision = CleanupInspection{SchemaVersion: CleanupSchemaVersion, Producer: "gitmoot", DecisionOwner: "gitmoot",
		ObservedAt: e.now().UTC().Format(time.RFC3339Nano), BudgetGate: budgetGate,
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

// ReclaimAbsentWorktreeOutcome closes the cleanup of a delegation worktree
// that is already gone and whose repo is no longer registered (removed or
// renamed, #2531). The daemon calls it only then: with no registered checkout
// and no .git pointer left to find the owning one, the ordinary path refuses
// the resource (checkout_identity_unknown, resource_missing_without_receipt)
// on every pass, forever. It deletes nothing and runs no git. Under the
// cleanup lifecycle fence it rechecks that the owner is final and owns a
// delegation worktree, that the path is contained (an unproven worktree mount
// is never taken for absence), that this generation's obligation is open and
// due, and that the path is absent; then it records what an ordinary removal
// records: a receipt, its evidence marked absent, and the removal event. An
// unfinished receipt of an earlier episode is completed instead. reclaimed
// reports whether the resource is now accounted removed; the caller marks the
// obligation, as after any reclaim. An owned branch is left alone: no checkout
// is known to hold it.
func (e Engine) ReclaimAbsentWorktreeOutcome(ctx context.Context, jobID string, policy CleanupPolicy) (reclaimed bool, err error) {
	if err := e.validate(); err != nil {
		return false, err
	}
	ctx, release, err := e.Store.AcquireCleanupLifecycle(context.WithoutCancel(ctx))
	if err != nil {
		return false, err
	}
	defer release()
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return false, err
	}
	if !IsFinalJobState(job.State) || !isImplementDelegationWorktree(job.Type, payload) && !isReadOnlyDelegationWorktree(job.Type, payload) {
		return false, nil
	}
	validator := ValidateDelegationCleanupTarget
	if e.cleanupTargetValidator != nil {
		validator = e.cleanupTargetValidator
	}
	if err := validator(e.Home, jobID, job.Type, payload); err != nil {
		if errors.Is(err, ErrWorktreeRootUnavailable) {
			return false, nil
		}
		return false, err
	}
	path := filepath.Clean(strings.TrimSpace(payload.WorktreePath))
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		// There again, or unreadable: not proven absent.
		return false, err
	}
	obligation, err := e.Store.GetCleanupObligation(ctx, db.CleanupObligationResourceID(jobID, path))
	if err != nil {
		return false, err
	}
	if obligation.LifecycleGeneration != job.LifecycleGeneration ||
		obligation.State != db.CleanupObligationPending && obligation.State != db.CleanupObligationRetryable {
		return false, nil
	}
	if due, err := time.Parse(time.RFC3339Nano, obligation.NextAttemptAt); err != nil || due.After(e.now()) {
		return false, nil
	}
	receipt, err := e.Store.GetCleanupReceipt(ctx, obligation.ResourceID, job.LifecycleGeneration)
	switch {
	case err == nil && receipt.RemovedAt != "":
		return true, nil
	case err == nil && receipt.SupersededAt != "":
		return false, nil
	case errors.Is(err, sql.ErrNoRows):
		if receipt, err = e.prepareAbsentCleanupReceipt(ctx, job, obligation, policy); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	}
	if _, err := e.finishProducerCleanupReceipt(ctx, receipt); err != nil {
		return false, err
	}
	return true, nil
}

// prepareAbsentCleanupReceipt prepares the receipt of an absent resource. Its
// evidence carries what an ordinary one does except the path and git
// identity, which no longer exist.
func (e Engine) prepareAbsentCleanupReceipt(ctx context.Context, job db.Job, obligation db.CleanupObligation, policy CleanupPolicy) (db.CleanupReceipt, error) {
	policy.Mode = effectiveCleanupMode(policy.Mode, job)
	if policy.BudgetGate == "" {
		policy.BudgetGate = CleanupBudgetGateNotApplicable
	}
	policy.Budget, policy.SizeLimits = nil, nil
	encodedPolicy, err := json.Marshal(policy)
	if err != nil {
		return db.CleanupReceipt{}, err
	}
	evidence := cleanupEvidence{ResourceID: obligation.ResourceID, OwnerState: job.State, Generation: job.LifecycleGeneration,
		OwnerPayloadHash: cleanupHash(job.Payload), Policy: policy, PolicyHash: cleanupHash(string(encodedPolicy)), Absent: true}
	terminalAt, known := parseStoredJobTime(obligation.TerminalAt)
	if !known {
		terminalAt, known = parseStoredJobTime(job.UpdatedAt)
	}
	if known {
		evidence.TerminalAt = terminalAt.UTC().Format(time.RFC3339Nano)
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return db.CleanupReceipt{}, err
	}
	return e.Store.PrepareCleanupReceipt(ctx, db.CleanupReceipt{
		ResourceID: obligation.ResourceID, LifecycleGeneration: job.LifecycleGeneration,
		OwnerJobID: job.ID, ExpectedPath: obligation.ExpectedPath, TerminalState: job.State,
		Evidence: string(encoded), EligibilityToken: cleanupHash(string(encoded)),
	}, e.now())
}
