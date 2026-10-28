package workflow

import (
	"context"
	"encoding/json"

	"github.com/gitmoot/gitmoot/internal/db"
)

// Producer cleanup byte budgets, schema v2 (#2360; wire agreement 356319 with workspace-janitor#69).
// Concrete caps and limits in tests are fixtures, never live defaults: a resource class with no
// configured positive cap has a nil ApprovedCleanupSizeBudget, which means UNKNOWN.

const (
	// CleanupSchemaVersion is the producer cleanup wire version. v2 adds the approved byte budget
	// object, the size measurement and the budget gate.
	CleanupSchemaVersion = 2

	CleanupSizeUnitAllocatedBytes = "allocated_bytes"
	CleanupSizeScopeWorktreeTree  = "worktree_tree"

	// CleanupBudgetGateEnforced marks a decision on the consumer path (`job cleanup apply`), where
	// a positive approved budget and a complete measurement within it are required.
	CleanupBudgetGateEnforced = "enforced"
	// CleanupBudgetGateNotApplicable marks automatic producer reclaim (ordinary/aged), which keeps
	// its existing protections and is NOT budget-gated (workspace-janitor 366497, option A). Its
	// receipts never present a null budget as approved.
	CleanupBudgetGateNotApplicable = "not_applicable"

	CleanupRefusalSizeBudgetUnknown         = "size_budget_unknown"
	CleanupRefusalSizeMeasurementUnknown    = "size_measurement_unknown"
	CleanupRefusalSizeMeasurementIncomplete = "size_measurement_incomplete"
	CleanupRefusalSizeOverBudget            = "size_over_budget"
	CleanupRefusalBudgetPolicyChanged       = "budget_policy_changed"
)

// ApprovedCleanupSizeBudget is a producer-owned, explicitly approved positive cap for one
// recognized resource class. Owner and Approval are trusted-policy provenance from the producer's
// own configuration, never caller-asserted.
type ApprovedCleanupSizeBudget struct {
	Unit       string `json:"unit"`
	LimitBytes int64  `json:"limit_bytes"`
	Owner      string `json:"owner"`
	Approval   string `json:"approval"`
}

// CleanupSizeLimits bounds one measurement. All three must be positive.
type CleanupSizeLimits struct {
	MaxEntries int64 `json:"max_entries"`
	MaxDepth   int   `json:"max_depth"`
	TimeoutMS  int64 `json:"timeout_ms"`
}

// CleanupSizeMeasurement is one allocated-bytes measurement of a resource tree. Bytes is nil
// whenever Complete is false; a complete zero is a real measurement, distinct from unknown.
type CleanupSizeMeasurement struct {
	Unit             string            `json:"unit"`
	Scope            string            `json:"scope"`
	Bytes            *int64            `json:"bytes"`
	Complete         bool              `json:"complete"`
	ObservedAt       string            `json:"observed_at"`
	Limits           CleanupSizeLimits `json:"limits"`
	IncompleteReason string            `json:"incomplete_reason,omitempty"`
}

// cleanupBudgetRefusal is the budget gate: "" admits, otherwise the refusal code. measured may be
// nil (no measurement was possible).
func cleanupBudgetRefusal(budget *ApprovedCleanupSizeBudget, measured *CleanupSizeMeasurement) string {
	if budget == nil || budget.LimitBytes <= 0 || budget.Unit != CleanupSizeUnitAllocatedBytes ||
		budget.Owner == "" || budget.Approval == "" {
		return CleanupRefusalSizeBudgetUnknown
	}
	if measured == nil || measured.Unit != CleanupSizeUnitAllocatedBytes {
		return CleanupRefusalSizeMeasurementUnknown
	}
	if !measured.Complete || measured.Bytes == nil {
		return CleanupRefusalSizeMeasurementIncomplete
	}
	if *measured.Bytes > budget.LimitBytes {
		return CleanupRefusalSizeOverBudget
	}
	return ""
}

// measureForCleanup runs the budget gate on the enforced path: it measures the resource (when it
// exists) with the policy's sizing bounds and records the measurement on the decision. It returns
// "" to admit or a refusal code. A missing resource (an episode being reconciled) is not measured:
// nothing remains to destroy, and its policy was already bound by the receipt check.
func (e Engine) measureForCleanup(d *CleanupInspection, policy CleanupPolicy, exists bool) string {
	if policy.Budget == nil || policy.SizeLimits == nil {
		return CleanupRefusalSizeBudgetUnknown
	}
	if !exists {
		return ""
	}
	m := measureCleanupTree(d.ExpectedPath, *policy.SizeLimits, e.now)
	d.SizeMeasurement = &m
	return cleanupBudgetRefusal(policy.Budget, &m)
}

// recordCleanupRefusal persists a refused consumer decision with its budget and measurement
// evidence (#2360 v2). The submitted token is kept even though a refused decision carries none.
func (e Engine) recordCleanupRefusal(ctx context.Context, jobID, submittedToken string, d CleanupInspection) (db.CleanupRefusal, error) {
	budget, err := json.Marshal(d.ApprovedSizeBudget)
	if err != nil {
		return db.CleanupRefusal{}, err
	}
	measurement, err := json.Marshal(d.SizeMeasurement)
	if err != nil {
		return db.CleanupRefusal{}, err
	}
	gate := d.BudgetGate
	if gate == "" {
		gate = CleanupBudgetGateEnforced
	}
	reasons := d.ReasonCodes
	if len(reasons) == 0 {
		reasons = []string{"refused"}
	}
	return e.Store.RecordCleanupRefusal(ctx, db.CleanupRefusal{
		SchemaVersion: CleanupSchemaVersion, ResourceID: d.ResourceID, OwnerJobID: jobID,
		LifecycleGeneration: d.LifecycleGeneration, SubmittedToken: submittedToken, PolicyHash: d.PolicyHash,
		BudgetGate: gate, ApprovedSizeBudget: string(budget), SizeMeasurement: string(measurement),
		ReasonCodes: reasons, ObservedAt: d.ObservedAt,
	}, e.now())
}
