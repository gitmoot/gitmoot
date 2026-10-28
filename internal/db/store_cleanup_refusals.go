package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CleanupRefusal is one durable, append-only record of a refused producer cleanup decision
// (#2360 v2). ApprovedSizeBudget and SizeMeasurement hold the exact JSON the decision carried
// ("null" when absent), so the evidence stays readable after the resource is gone.
type CleanupRefusal struct {
	RefusalID           string   `json:"refusal_id"`
	SchemaVersion       int      `json:"schema_version"`
	ResourceID          string   `json:"resource_id"`
	OwnerJobID          string   `json:"owner_job_id"`
	LifecycleGeneration int64    `json:"lifecycle_generation"`
	SubmittedToken      string   `json:"submitted_token,omitempty"`
	PolicyHash          string   `json:"policy_hash,omitempty"`
	BudgetGate          string   `json:"budget_gate"`
	ApprovedSizeBudget  string   `json:"approved_size_budget"`
	SizeMeasurement     string   `json:"size_measurement"`
	ReasonCodes         []string `json:"reason_codes"`
	ObservedAt          string   `json:"observed_at"`
	CreatedAt           string   `json:"created_at"`
}

// RecordCleanupRefusal appends a refusal under the exclusive cleanup lifecycle fence, in one short
// write transaction taken after all filesystem work. It never touches cleanup_receipts.
func (s *Store) RecordCleanupRefusal(ctx context.Context, refusal CleanupRefusal, now time.Time) (CleanupRefusal, error) {
	if err := s.requireCleanupLifecycle(ctx); err != nil {
		return CleanupRefusal{}, err
	}
	if strings.TrimSpace(refusal.ResourceID) == "" || strings.TrimSpace(refusal.OwnerJobID) == "" {
		return CleanupRefusal{}, errors.New("cleanup refusal requires resource and owner job")
	}
	if len(refusal.ReasonCodes) == 0 || strings.TrimSpace(refusal.BudgetGate) == "" {
		return CleanupRefusal{}, errors.New("cleanup refusal requires reason codes and the budget gate")
	}
	for _, raw := range []*string{&refusal.ApprovedSizeBudget, &refusal.SizeMeasurement} {
		if strings.TrimSpace(*raw) == "" {
			*raw = "null"
		}
		if !json.Valid([]byte(*raw)) {
			return CleanupRefusal{}, fmt.Errorf("cleanup refusal evidence is not valid JSON: %q", *raw)
		}
	}
	reasons, err := json.Marshal(refusal.ReasonCodes)
	if err != nil {
		return CleanupRefusal{}, err
	}
	refusal.CreatedAt = now.UTC().Format(time.RFC3339Nano)
	// Random, not derived: two identical refusals in one clock tick are two events, and each must
	// get its own append-only row rather than a UNIQUE failure.
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return CleanupRefusal{}, err
	}
	refusal.RefusalID = "cleanup-refusal-" + hex.EncodeToString(id[:])
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO cleanup_refusals(refusal_id, schema_version, resource_id, owner_job_id, lifecycle_generation,
	submitted_token, policy_hash, budget_gate, approved_size_budget, size_measurement, reason_codes, observed_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		refusal.RefusalID, refusal.SchemaVersion, refusal.ResourceID, refusal.OwnerJobID, refusal.LifecycleGeneration,
		refusal.SubmittedToken, refusal.PolicyHash, refusal.BudgetGate, refusal.ApprovedSizeBudget, refusal.SizeMeasurement,
		string(reasons), refusal.ObservedAt, refusal.CreatedAt); err != nil {
		return CleanupRefusal{}, err
	}
	return refusal, nil
}

// ListCleanupRefusals returns a resource's refusals, oldest first.
func (s *Store) ListCleanupRefusals(ctx context.Context, resourceID string) ([]CleanupRefusal, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT refusal_id, schema_version, resource_id, owner_job_id, lifecycle_generation, submitted_token, policy_hash,
	budget_gate, approved_size_budget, size_measurement, reason_codes, observed_at, created_at
FROM cleanup_refusals WHERE resource_id = ? ORDER BY id`, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CleanupRefusal
	for rows.Next() {
		var r CleanupRefusal
		var reasons string
		if err := rows.Scan(&r.RefusalID, &r.SchemaVersion, &r.ResourceID, &r.OwnerJobID, &r.LifecycleGeneration,
			&r.SubmittedToken, &r.PolicyHash, &r.BudgetGate, &r.ApprovedSizeBudget, &r.SizeMeasurement,
			&reasons, &r.ObservedAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(reasons), &r.ReasonCodes); err != nil {
			return nil, fmt.Errorf("decode cleanup refusal %s reasons: %w", r.RefusalID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
