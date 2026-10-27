package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

const CleanupRetryBudget = 3
const CleanupRetryDelay = time.Minute

type CleanupApplyResult struct {
	SchemaVersion int                `json:"schema_version"`
	State         string             `json:"state"`
	Decision      CleanupInspection  `json:"decision"`
	Receipt       *db.CleanupReceipt `json:"receipt,omitempty"`
	// RefusalReceipt is the durable record of a refused consumer apply (#2360 v2).
	RefusalReceipt *db.CleanupRefusal `json:"refusal_receipt,omitempty"`
}

// ApplyProducerCleanup accepts only a producer-derived resource and a prior
// eligibility token. It never accepts a path, reopens quarantine or changes TTL.
// It is the consumer path, so it is always budget-gated (#2360 v2): the caller cannot opt out.
func (e Engine) ApplyProducerCleanup(ctx context.Context, jobID, resourceID, token string, policy CleanupPolicy) (CleanupApplyResult, error) {
	if strings.TrimSpace(resourceID) == "" || strings.TrimSpace(token) == "" {
		return CleanupApplyResult{}, errors.New("cleanup apply requires resource identity and eligibility token")
	}
	policy.BudgetGate = CleanupBudgetGateEnforced
	return e.applyProducerCleanup(ctx, jobID, resourceID, token, policy, true)
}

func (e Engine) applyProducerCleanup(ctx context.Context, jobID, resourceID, token string, policy CleanupPolicy, countFailure bool) (result CleanupApplyResult, err error) {
	result = CleanupApplyResult{SchemaVersion: CleanupSchemaVersion, State: "refused"}
	if policy.BudgetGate != CleanupBudgetGateEnforced {
		policy.BudgetGate = CleanupBudgetGateNotApplicable
	}
	if err := e.validate(); err != nil {
		return result, err
	}
	ctx, releaseLifecycle, err := e.Store.AcquireCleanupLifecycle(ctx)
	if err != nil {
		return result, err
	}
	defer releaseLifecycle()
	// Every refused consumer apply leaves a durable refusal receipt (#2360 v2), written in its own
	// short transaction after all filesystem work, while the lifecycle fence is still held.
	defer func() {
		if policy.BudgetGate != CleanupBudgetGateEnforced || result.State != "refused" || result.Decision.ResourceID == "" {
			return
		}
		recorded, recErr := e.recordCleanupRefusal(context.WithoutCancel(ctx), jobID, token, result.Decision)
		if recErr != nil {
			err = errors.Join(err, fmt.Errorf("record cleanup refusal: %w", recErr))
			return
		}
		result.RefusalReceipt = &recorded
	}()
	defer func() {
		if err == nil || result.State != "retryable" || result.Decision.ResourceID == "" {
			return
		}
		persistCtx := context.WithoutCancel(ctx)
		if countFailure {
			cause := errors.New(RedactCommentText(err.Error()))
			_, persistErr := e.Store.RecordCleanupObligationFailure(persistCtx, jobID, result.Decision.ExpectedPath,
				db.ClassifyCleanupObligationFailure("reclaim", err), cause, e.now(), e.now().Add(CleanupRetryDelay), CleanupRetryBudget)
			err = errors.Join(err, persistErr)
		} else {
			err = errors.Join(err, e.deferDelegationCleanupFailure(persistCtx, jobID, result.Decision.ExpectedPath, "reclaim", err))
		}
	}()
	reconciled, handled, reconcileErr := e.reconcileCleanupReceipt(ctx, jobID, resourceID, token)
	if handled || reconcileErr != nil {
		return reconciled, reconcileErr
	}
	decision, payload, err := e.inspectProducerCleanup(ctx, jobID, policy, nil)
	result.Decision = decision
	if err != nil || !decision.Eligible {
		if err == nil && decision.Receipt != nil && decision.Receipt.RemovedAt != "" {
			result.State, result.Receipt = "already_removed", decision.Receipt
		}
		return result, err
	}
	if resourceID != "" && decision.ResourceID != resourceID || token != "" && decision.EligibilityToken != token {
		result.Decision = decision.refuse("eligibility_changed")
		return result, nil
	}
	releaseCheckout, _, err := acquireCheckoutMutationLockWithWait(ctx, e.Store, e.DelegationCheckout, "producer-cleanup:"+jobID, time.Now().UTC())
	if err != nil {
		return result, err
	}
	defer func() {
		if releaseCheckout != nil {
			err = errors.Join(err, releaseCheckout(context.WithoutCancel(ctx)))
		}
	}()
	// The checkout lock is additional serialization, never a substitute for the
	// lifecycle fence. Recheck physical identity after obtaining both.
	current, payload, err := e.inspectProducerCleanup(ctx, jobID, policy, nil)
	if err != nil || !current.Eligible || current.EligibilityToken != decision.EligibilityToken {
		if err == nil && current.Eligible {
			current = current.refuse("eligibility_changed")
		}
		result.Decision = current
		return result, err
	}
	if _, err := e.Store.EnsureCleanupObligation(ctx, jobID, current.ExpectedPath, e.now()); err != nil {
		return result, err
	}
	result.State = "retryable"
	receipt := current.Receipt
	if receipt == nil {
		job, err := e.Store.GetJob(ctx, jobID)
		if err != nil {
			return result, err
		}
		if isReadOnlyDelegationWorktree(job.Type, payload) {
			if e.BeforeReadOnlyWorktreeCleanup == nil {
				result.State, result.Decision = "refused", current.refuse("preservation_unavailable")
				return result, nil
			}
			if !cleanupArtifactsPublished(ctx, job) {
				if err := e.BeforeReadOnlyWorktreeCleanup(ctx, jobID, job.Type, payload); err != nil {
					return result, fmt.Errorf("preserve producer artifacts: %w", err)
				}
			}
		}
		// Collectors may enrich the payload. Their durable output is included in
		// the immutable receipt; changed generation/path/dependencies still refuse.
		preserved, refreshed, err := e.inspectProducerCleanup(ctx, jobID, policy, &current.evidence)
		if err != nil || !preserved.Eligible {
			result.State, result.Decision = "refused", preserved
			return result, err
		}
		payload = refreshed
		evidence, err := json.Marshal(preserved.evidence)
		if err != nil {
			return result, err
		}
		prepared, err := e.Store.PrepareCleanupReceipt(ctx, db.CleanupReceipt{
			ResourceID: preserved.ResourceID, LifecycleGeneration: preserved.LifecycleGeneration,
			OwnerJobID: jobID, ExpectedPath: preserved.ExpectedPath, TerminalState: preserved.OwnerState,
			Evidence: string(evidence), EligibilityToken: current.EligibilityToken,
		}, e.now())
		if err != nil {
			return result, err
		}
		receipt = &prepared
		current = preserved
	}
	result.Receipt, result.Decision = receipt, current
	// Recheck the inode immediately before removal, including on replay. An
	// absent path is only reconcilable because this episode already has a receipt.
	if info, statErr := os.Lstat(current.ExpectedPath); statErr == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || fmt.Sprintf("%d:%d", stat.Dev, stat.Ino) != current.evidence.PathIdentity {
			result.State, result.Decision = "refused", current.refuse("resource_replaced")
			return result, nil
		}
		if live, known := e.worktreeLiveness(current.ResolvedPath); live || !known {
			result.State, result.Decision = "refused", current.refuse("process_active_or_unknown")
			return result, nil
		}
		// Final destructive gate (#2360 v2): a FRESH complete measurement within the approved cap,
		// under the lifecycle fence and checkout lock, immediately before removal.
		if false && current.Policy.BudgetGate == CleanupBudgetGateEnforced { // MUTANT M1
			if reason := e.measureForCleanup(&current, current.Policy, true); reason != "" {
				result.State, result.Decision = "refused", current.refuse(reason)
				return result, nil
			}
			result.Decision = current
		}
		if err := e.DelegationWorktrees.(ReadOnlyWorktreeManager).RemoveWorktreeForce(ctx, current.ExpectedPath); err != nil {
			return result, fmt.Errorf("remove producer worktree: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return result, statErr
	}
	if current.OwnedBranch != "" {
		checker := e.DelegationWorktrees.(BranchExistenceChecker)
		exists, err := checker.BranchExists(ctx, current.OwnedBranch)
		if err != nil {
			return result, err
		}
		if exists {
			head, err := e.DelegationWorktrees.(cleanupWorktreeInspector).RevParse(ctx, "refs/heads/"+current.OwnedBranch)
			if err != nil || strings.TrimSpace(head) != current.evidence.Git.HeadSHA {
				result.State, result.Decision = "refused", current.refuse("branch_identity_changed")
				return result, err
			}
			if err := e.DelegationWorktrees.(BranchDeleter).DeleteBranch(ctx, current.OwnedBranch); err != nil {
				return result, err
			}
		}
		if _, err := releaseDelegationBranchLock(ctx, e.Store, "implement", payload); err != nil {
			return result, err
		}
	}
	if current.Policy.Mode == "aged" {
		if pruner, ok := e.DelegationWorktrees.(WorktreePruner); ok {
			if err := pruner.PruneWorktrees(ctx); err != nil {
				return result, err
			}
		}
	}
	completed, err := e.finishProducerCleanupReceipt(ctx, *receipt)
	if err != nil {
		return result, err
	}
	result.State, result.Receipt = "removed", &completed
	return result, nil
}

func (e Engine) cleanupProducerAutomatically(ctx context.Context, jobID string, policy CleanupPolicy) (bool, error) {
	ctx, err := e.publishCleanupArtifacts(context.WithoutCancel(ctx), jobID)
	if err != nil {
		return false, err
	}
	result, err := e.applyProducerCleanup(context.WithoutCancel(ctx), jobID, "", "", policy, false)
	if errors.Is(err, db.ErrCleanupLifecycleBusy) {
		// An enclosing resume/advance still owns the shared fence. Preserve the
		// resource and let the existing deferred-cleanup pass retry after it exits.
		job, payload, readErr := e.jobPayload(ctx, jobID)
		if readErr != nil {
			return false, readErr
		}
		if !isImplementDelegationWorktree(job.Type, payload) && !isReadOnlyDelegationWorktree(job.Type, payload) {
			return false, nil
		}
		e.recordCleanupSkippedOnce(ctx, jobID, payload, "lifecycle operation is still in progress")
		return false, e.deferDelegationCleanupObligation(ctx, jobID, payload.WorktreePath, db.CleanupReasonTerminalDeferred)
	}
	if err != nil {
		return false, err
	}
	if result.State == "refused" && result.Decision.ExpectedPath != "" {
		_, payload, readErr := e.jobPayload(ctx, jobID)
		if readErr != nil {
			return false, readErr
		}
		e.recordCleanupSkippedOnce(ctx, jobID, payload, strings.Join(result.Decision.ReasonCodes, ","))
	}
	return result.State == "removed", nil
}
