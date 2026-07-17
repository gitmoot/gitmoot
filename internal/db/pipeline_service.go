package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const pipelineExposureTokenBytes = 32

var ErrExposureDisabled = errors.New("pipeline exposure is disabled")

var (
	ErrExposureRateLimited      = errors.New("pipeline exposure rate limit exceeded")
	ErrServiceRunGlobalLimit    = errors.New("global active service run limit exceeded")
	ErrServiceRunPipelineActive = errors.New("pipeline already has an active run")
)

// ServiceRunAdmission is the complete write set for accepting an authenticated
// service request. AdmitServiceRun persists it atomically with both concurrency
// checks and the exposure token-bucket debit.
type ServiceRunAdmission struct {
	Run             PipelineRun
	Stages          []PipelineRunStage
	ServiceRun      PipelineServiceRun
	Now             time.Time
	BucketCapacity  float64
	RefillPerSecond float64
	BucketCost      float64
	MaxActive       int
}

// ServiceRunAdmissionCheck is the read-only, non-consuming half of service-run
// admission. It lets the HTTP boundary reject an obviously full bucket or
// concurrency slot before freezing a bundle. AdmitServiceRun remains the
// authoritative transactional check and debit after the freeze.
type ServiceRunAdmissionCheck struct {
	PipelineName    string
	Now             time.Time
	BucketCapacity  float64
	RefillPerSecond float64
	BucketCost      float64
	MaxActive       int
}

// ExposureRateLimitError carries the stable delay the HTTP layer publishes in
// Retry-After. It contains no submitted input or credential material.
type ExposureRateLimitError struct {
	RetryAfter time.Duration
}

func (e *ExposureRateLimitError) Error() string { return ErrExposureRateLimited.Error() }
func (e *ExposureRateLimitError) Unwrap() error { return ErrExposureRateLimited }

// PipelineExposure is the non-secret service declaration persisted for a named
// pipeline. TokenHash is returned only to store-layer callers and is excluded
// from JSON; CLI/API response structs must never embed this type directly.
type PipelineExposure struct {
	PipelineName    string
	SchemaVersion   int
	SchemaJSON      string
	SchemaHash      string
	TokenHash       []byte `json:"-"`
	Enabled         bool
	BucketTokens    float64
	BucketUpdatedAt time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// PipelineServiceRun is the durable artifact/proof receipt metadata for one
// accepted service-triggered pipeline run.
type PipelineServiceRun struct {
	RunID           string
	PipelineName    string
	ArtifactRelpath string
	ArtifactSHA256  string
	ProofID         string
	ProofVerifiedAt time.Time
	CreatedAt       time.Time
}

// GetJobForProof loads the structured columns needed by the offline proof
// verifier, including the denormalized result hash omitted by general readers.
func (s *Store) GetJobForProof(ctx context.Context, id string) (Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, agent, type, state, payload, model, parent_job_id, delegation_id,
		delegation_depth, delegated_by, root_id, workflow_id, repo, pull_request, root_killed, input_tokens,
		output_tokens, updated_at, created_at, result_hash FROM jobs WHERE id = ?`, strings.TrimSpace(id))
	var job Job
	if err := row.Scan(&job.ID, &job.Agent, &job.Type, &job.State, &job.Payload, &job.Model, &job.ParentJobID,
		&job.DelegationID, &job.DelegationDepth, &job.DelegatedBy, &job.RootID, &job.WorkflowID, &job.Repo,
		&job.PullRequest, &job.RootKilled, &job.InputTokens, &job.OutputTokens, &job.UpdatedAt, &job.CreatedAt,
		&job.ResultHash); err != nil {
		return Job{}, err
	}
	return job, nil
}

// CreateExposure creates or refreshes a pipeline's schema declaration. It mints
// and returns a plaintext bearer token only for the first insert; refreshing an
// existing declaration preserves the token hash. The bool reports first insert.
func (s *Store) CreateExposure(ctx context.Context, exposure PipelineExposure) (token string, created bool, err error) {
	exposure.PipelineName = strings.TrimSpace(exposure.PipelineName)
	exposure.SchemaJSON = strings.TrimSpace(exposure.SchemaJSON)
	exposure.SchemaHash = strings.TrimSpace(exposure.SchemaHash)
	if exposure.PipelineName == "" {
		return "", false, errors.New("pipeline exposure name is required")
	}
	if exposure.SchemaVersion <= 0 || exposure.SchemaJSON == "" || exposure.SchemaHash == "" {
		return "", false, errors.New("pipeline exposure schema version, JSON, and hash are required")
	}
	if math.IsNaN(exposure.BucketTokens) || math.IsInf(exposure.BucketTokens, 0) || exposure.BucketTokens < 0 {
		return "", false, errors.New("pipeline exposure bucket tokens must be finite and >= 0")
	}
	if exposure.BucketUpdatedAt.IsZero() {
		exposure.BucketUpdatedAt = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var pipelineExists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM pipelines WHERE name = ?`, exposure.PipelineName).Scan(&pipelineExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, fmt.Errorf("pipeline %s not found", exposure.PipelineName)
		}
		return "", false, err
	}
	var existingHash []byte
	err = tx.QueryRowContext(ctx, `SELECT token_hash FROM pipeline_exposures WHERE pipeline_name = ?`, exposure.PipelineName).Scan(&existingHash)
	switch {
	case err == nil:
		enabled := 0
		if exposure.Enabled {
			enabled = 1
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pipeline_exposures
			SET schema_version = ?, schema_json = ?, schema_hash = ?, enabled = ?, updated_at = CURRENT_TIMESTAMP
			WHERE pipeline_name = ?`, exposure.SchemaVersion, exposure.SchemaJSON, exposure.SchemaHash, enabled, exposure.PipelineName); err != nil {
			return "", false, err
		}
		if err := tx.Commit(); err != nil {
			return "", false, err
		}
		return "", false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, err
	}

	token, tokenHash, err := newPipelineExposureToken()
	if err != nil {
		return "", false, err
	}
	enabled := 0
	if exposure.Enabled {
		enabled = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_exposures
		(pipeline_name, schema_version, schema_json, schema_hash, token_hash, enabled, bucket_tokens, bucket_updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, exposure.PipelineName, exposure.SchemaVersion, exposure.SchemaJSON,
		exposure.SchemaHash, tokenHash[:], enabled, exposure.BucketTokens, formatHeartbeatTime(exposure.BucketUpdatedAt)); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return token, true, nil
}

func (s *Store) GetExposure(ctx context.Context, pipelineName string) (PipelineExposure, bool, error) {
	pipelineName = strings.TrimSpace(pipelineName)
	if pipelineName == "" {
		return PipelineExposure{}, false, errors.New("pipeline exposure name is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT pipeline_name, schema_version, schema_json, schema_hash, token_hash,
		enabled, bucket_tokens, bucket_updated_at, created_at, updated_at
		FROM pipeline_exposures WHERE pipeline_name = ?`, pipelineName)
	exposure, err := scanPipelineExposure(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelineExposure{}, false, nil
	}
	if err != nil {
		return PipelineExposure{}, false, err
	}
	return exposure, true, nil
}

// RotateExposureToken atomically replaces the stored digest and returns the new
// base64url plaintext exactly once.
func (s *Store) RotateExposureToken(ctx context.Context, pipelineName string) (string, error) {
	pipelineName = strings.TrimSpace(pipelineName)
	if pipelineName == "" {
		return "", errors.New("pipeline exposure name is required")
	}
	token, tokenHash, err := newPipelineExposureToken()
	if err != nil {
		return "", err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE pipeline_exposures SET token_hash = ?, updated_at = CURRENT_TIMESTAMP WHERE pipeline_name = ?`, tokenHash[:], pipelineName)
	if err != nil {
		return "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if affected == 0 {
		return "", fmt.Errorf("pipeline exposure %s not found", pipelineName)
	}
	return token, nil
}

func (s *Store) SetExposureEnabled(ctx context.Context, pipelineName string, enabled bool) error {
	pipelineName = strings.TrimSpace(pipelineName)
	if pipelineName == "" {
		return errors.New("pipeline exposure name is required")
	}
	flag := 0
	if enabled {
		flag = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE pipeline_exposures SET enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE pipeline_name = ?`, flag, pipelineName)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("pipeline exposure %s not found", pipelineName)
	}
	return nil
}

// AuthenticateExposureToken hashes the presented plaintext and compares the
// fixed-size digest in constant time. Enabled is deliberately not folded into
// authentication so the HTTP layer can distinguish authorization policy from
// credential validity without ever receiving the digest.
func (s *Store) AuthenticateExposureToken(ctx context.Context, pipelineName, token string) (PipelineExposure, bool, error) {
	exposure, ok, err := s.GetExposure(ctx, pipelineName)
	if err != nil || !ok {
		return PipelineExposure{}, false, err
	}
	presented := sha256.Sum256([]byte(token))
	valid := len(exposure.TokenHash) == sha256.Size && subtle.ConstantTimeCompare(exposure.TokenHash, presented[:]) == 1
	return exposure, valid, nil
}

// DebitExposureBucket performs read/refill/debit/write under one transaction.
// now is caller-supplied so tests and retries can deterministically reproduce a
// rate-limit decision. A denied debit still persists the refill and timestamp.
func (s *Store) DebitExposureBucket(ctx context.Context, pipelineName string, now time.Time, capacity, refillPerSecond, cost float64) (remaining float64, allowed bool, err error) {
	pipelineName = strings.TrimSpace(pipelineName)
	if pipelineName == "" {
		return 0, false, errors.New("pipeline exposure name is required")
	}
	if now.IsZero() {
		return 0, false, errors.New("pipeline exposure bucket time is required")
	}
	if !finitePositive(capacity) {
		return 0, false, errors.New("pipeline exposure bucket capacity is invalid")
	}
	if math.IsNaN(refillPerSecond) || math.IsInf(refillPerSecond, 0) || refillPerSecond < 0 {
		return 0, false, errors.New("pipeline exposure bucket refill rate is invalid")
	}
	if !finitePositive(cost) {
		return 0, false, errors.New("pipeline exposure bucket cost is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var tokens float64
	var updatedRaw string
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT bucket_tokens, bucket_updated_at, enabled FROM pipeline_exposures WHERE pipeline_name = ?`, pipelineName).Scan(&tokens, &updatedRaw, &enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("pipeline exposure %s not found", pipelineName)
		}
		return 0, false, err
	}
	if enabled == 0 {
		return tokens, false, ErrExposureDisabled
	}
	updatedAt := parseHeartbeatTime(updatedRaw)
	effectiveNow := now.UTC()
	if !updatedAt.IsZero() && effectiveNow.Before(updatedAt) {
		effectiveNow = updatedAt // wall-clock rollback must not manufacture refill
	}
	elapsed := effectiveNow.Sub(updatedAt)
	if updatedAt.IsZero() {
		elapsed = 0
	}
	tokens = math.Min(capacity, math.Max(0, tokens)+elapsed.Seconds()*refillPerSecond)
	allowed = tokens >= cost
	if allowed {
		tokens -= cost
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pipeline_exposures SET bucket_tokens = ?, bucket_updated_at = ?, updated_at = CURRENT_TIMESTAMP WHERE pipeline_name = ?`, tokens, formatHeartbeatTime(effectiveNow), pipelineName); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return tokens, allowed, nil
}

func finitePositive(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}

func (s *Store) CreateServiceRun(ctx context.Context, serviceRun PipelineServiceRun) error {
	serviceRun.RunID = strings.TrimSpace(serviceRun.RunID)
	serviceRun.PipelineName = strings.TrimSpace(serviceRun.PipelineName)
	if serviceRun.RunID == "" || serviceRun.PipelineName == "" {
		return errors.New("pipeline service run id and pipeline name are required")
	}
	if serviceRun.CreatedAt.IsZero() {
		serviceRun.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pipelineName string
	if err := tx.QueryRowContext(ctx, `SELECT pipeline FROM pipeline_runs WHERE id = ?`, serviceRun.RunID).Scan(&pipelineName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("pipeline run %s not found", serviceRun.RunID)
		}
		return err
	}
	if pipelineName != serviceRun.PipelineName {
		return fmt.Errorf("pipeline run %s belongs to %s, not %s", serviceRun.RunID, pipelineName, serviceRun.PipelineName)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_service_runs
		(run_id, pipeline_name, artifact_relpath, artifact_sha256, proof_id, proof_verified_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, serviceRun.RunID, serviceRun.PipelineName,
		strings.TrimSpace(serviceRun.ArtifactRelpath), strings.TrimSpace(serviceRun.ArtifactSHA256),
		strings.TrimSpace(serviceRun.ProofID), formatHeartbeatTime(serviceRun.ProofVerifiedAt), formatHeartbeatTime(serviceRun.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetServiceRun(ctx context.Context, runID string) (PipelineServiceRun, bool, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return PipelineServiceRun{}, false, errors.New("pipeline service run id is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT run_id, pipeline_name, artifact_relpath, artifact_sha256, proof_id, proof_verified_at, created_at
		FROM pipeline_service_runs WHERE run_id = ?`, runID)
	var serviceRun PipelineServiceRun
	var proofVerifiedAt, createdAt string
	if err := row.Scan(&serviceRun.RunID, &serviceRun.PipelineName, &serviceRun.ArtifactRelpath,
		&serviceRun.ArtifactSHA256, &serviceRun.ProofID, &proofVerifiedAt, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PipelineServiceRun{}, false, nil
		}
		return PipelineServiceRun{}, false, err
	}
	serviceRun.ProofVerifiedAt = parseHeartbeatTime(proofVerifiedAt)
	serviceRun.CreatedAt = parseStoredTimestamp(createdAt)
	return serviceRun, true, nil
}

// ActiveServicePipelineRun returns an accepted service run that is still
// active, regardless of any newer manual/scheduled run row.
func (s *Store) ActiveServicePipelineRun(ctx context.Context, pipelineName string) (PipelineRun, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, pipeline, trigger, payload_json, spec_hash, state, halt_stage, halt_reason, needs_json, started_at, finished_at
		FROM pipeline_runs WHERE pipeline = ? AND trigger = 'service' AND state = 'running' ORDER BY started_at DESC, id DESC LIMIT 1`, strings.TrimSpace(pipelineName))
	run, err := scanPipelineRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelineRun{}, false, nil
	}
	if err != nil {
		return PipelineRun{}, false, err
	}
	return run, true, nil
}

// CheckServiceRunAdmission performs a cheap read-only snapshot of the same
// bucket and concurrency limits enforced transactionally by AdmitServiceRun. It
// never debits/refills the persisted bucket and never creates a run; callers
// must still call AdmitServiceRun because concurrent admissions may invalidate
// this advisory result immediately after it returns.
func (s *Store) CheckServiceRunAdmission(ctx context.Context, check ServiceRunAdmissionCheck) error {
	check.PipelineName = strings.TrimSpace(check.PipelineName)
	if check.PipelineName == "" || check.Now.IsZero() || !finitePositive(check.BucketCapacity) ||
		math.IsNaN(check.RefillPerSecond) || math.IsInf(check.RefillPerSecond, 0) || check.RefillPerSecond < 0 ||
		!finitePositive(check.BucketCost) || check.MaxActive < 1 {
		return errors.New("invalid pipeline service admission check")
	}

	var tokens float64
	var updatedRaw string
	var enabled int
	if err := s.db.QueryRowContext(ctx, `SELECT bucket_tokens, bucket_updated_at, enabled
		FROM pipeline_exposures WHERE pipeline_name = ?`, check.PipelineName).Scan(&tokens, &updatedRaw, &enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("pipeline exposure %s not found", check.PipelineName)
		}
		return err
	}
	if enabled == 0 {
		return ErrExposureDisabled
	}
	effectiveNow := check.Now.UTC()
	updatedAt := parseHeartbeatTime(updatedRaw)
	if !updatedAt.IsZero() && effectiveNow.Before(updatedAt) {
		effectiveNow = updatedAt
	}
	elapsed := time.Duration(0)
	if !updatedAt.IsZero() {
		elapsed = effectiveNow.Sub(updatedAt)
	}
	tokens = math.Min(check.BucketCapacity, math.Max(0, tokens)+elapsed.Seconds()*check.RefillPerSecond)
	if tokens < check.BucketCost {
		retryAfter := time.Second
		if check.RefillPerSecond > 0 {
			retryAfter = time.Duration(math.Ceil((check.BucketCost-tokens)/check.RefillPerSecond)) * time.Second
		}
		return &ExposureRateLimitError{RetryAfter: retryAfter}
	}

	var activeForPipeline int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_runs WHERE pipeline = ? AND state = 'running'`, check.PipelineName).Scan(&activeForPipeline); err != nil {
		return err
	}
	if activeForPipeline != 0 {
		return ErrServiceRunPipelineActive
	}
	var activeServices int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_runs WHERE trigger = 'service' AND state = 'running'`).Scan(&activeServices); err != nil {
		return err
	}
	if activeServices >= check.MaxActive {
		return ErrServiceRunGlobalLimit
	}
	return nil
}

// AdmitServiceRun creates the run, its stage rows, and its durable service
// receipt in one SQLite transaction. The exposure row is touched first to take
// the database write lock before concurrency checks, preventing two concurrent
// admissions from both observing spare capacity. Rate-limit denials persist only
// elapsed refill; all other denials roll the whole transaction back.
func (s *Store) AdmitServiceRun(ctx context.Context, admission ServiceRunAdmission) error {
	admission.Run.ID = strings.TrimSpace(admission.Run.ID)
	admission.Run.Pipeline = strings.TrimSpace(admission.Run.Pipeline)
	admission.ServiceRun.RunID = strings.TrimSpace(admission.ServiceRun.RunID)
	admission.ServiceRun.PipelineName = strings.TrimSpace(admission.ServiceRun.PipelineName)
	if admission.Run.ID == "" || admission.Run.Pipeline == "" {
		return errors.New("pipeline service run id and pipeline name are required")
	}
	if admission.Run.ID != admission.ServiceRun.RunID || admission.Run.Pipeline != admission.ServiceRun.PipelineName {
		return errors.New("pipeline service admission run identity mismatch")
	}
	if admission.Run.Trigger != "service" {
		return errors.New("pipeline service admission trigger must be service")
	}
	if admission.Now.IsZero() || !finitePositive(admission.BucketCapacity) ||
		math.IsNaN(admission.RefillPerSecond) || math.IsInf(admission.RefillPerSecond, 0) || admission.RefillPerSecond < 0 ||
		!finitePositive(admission.BucketCost) || admission.MaxActive < 1 {
		return errors.New("invalid pipeline service admission limits")
	}
	if admission.ServiceRun.CreatedAt.IsZero() {
		admission.ServiceRun.CreatedAt = admission.Now.UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Acquire the writer lock before reading bucket/concurrency state.
	result, err := tx.ExecContext(ctx, `UPDATE pipeline_exposures SET bucket_tokens = bucket_tokens WHERE pipeline_name = ?`, admission.Run.Pipeline)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return fmt.Errorf("pipeline exposure %s not found", admission.Run.Pipeline)
	}

	var tokens float64
	var updatedRaw string
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT bucket_tokens, bucket_updated_at, enabled FROM pipeline_exposures WHERE pipeline_name = ?`, admission.Run.Pipeline).
		Scan(&tokens, &updatedRaw, &enabled); err != nil {
		return err
	}
	if enabled == 0 {
		return ErrExposureDisabled
	}
	effectiveNow := admission.Now.UTC()
	updatedAt := parseHeartbeatTime(updatedRaw)
	if !updatedAt.IsZero() && effectiveNow.Before(updatedAt) {
		effectiveNow = updatedAt
	}
	elapsed := time.Duration(0)
	if !updatedAt.IsZero() {
		elapsed = effectiveNow.Sub(updatedAt)
	}
	tokens = math.Min(admission.BucketCapacity, math.Max(0, tokens)+elapsed.Seconds()*admission.RefillPerSecond)
	if tokens < admission.BucketCost {
		if _, err := tx.ExecContext(ctx, `UPDATE pipeline_exposures SET bucket_tokens = ?, bucket_updated_at = ?, updated_at = CURRENT_TIMESTAMP WHERE pipeline_name = ?`,
			tokens, formatHeartbeatTime(effectiveNow), admission.Run.Pipeline); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		retryAfter := time.Second
		if admission.RefillPerSecond > 0 {
			retryAfter = time.Duration(math.Ceil((admission.BucketCost-tokens)/admission.RefillPerSecond)) * time.Second
		}
		return &ExposureRateLimitError{RetryAfter: retryAfter}
	}

	var activeForPipeline int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_runs WHERE pipeline = ? AND state = 'running'`, admission.Run.Pipeline).Scan(&activeForPipeline); err != nil {
		return err
	}
	if activeForPipeline != 0 {
		return ErrServiceRunPipelineActive
	}
	var activeServices int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_runs WHERE trigger = 'service' AND state = 'running'`).Scan(&activeServices); err != nil {
		return err
	}
	if activeServices >= admission.MaxActive {
		return ErrServiceRunGlobalLimit
	}

	if err := insertPipelineRun(ctx, tx, admission.Run); err != nil {
		return err
	}
	for _, stage := range admission.Stages {
		if stage.RunID != admission.Run.ID {
			return errors.New("pipeline service admission stage run id mismatch")
		}
		if err := insertPipelineRunStage(ctx, tx, stage); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_service_runs
		(run_id, pipeline_name, artifact_relpath, artifact_sha256, proof_id, proof_verified_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, admission.ServiceRun.RunID, admission.ServiceRun.PipelineName,
		strings.TrimSpace(admission.ServiceRun.ArtifactRelpath), strings.TrimSpace(admission.ServiceRun.ArtifactSHA256),
		strings.TrimSpace(admission.ServiceRun.ProofID), formatHeartbeatTime(admission.ServiceRun.ProofVerifiedAt),
		formatHeartbeatTime(admission.ServiceRun.CreatedAt)); err != nil {
		return err
	}
	if err := updatePipelineLastRun(ctx, tx, admission.Run.Pipeline, admission.Run.ID, admission.Run.State, admission.Now.UTC()); err != nil {
		return err
	}
	tokens -= admission.BucketCost
	if _, err := tx.ExecContext(ctx, `UPDATE pipeline_exposures SET bucket_tokens = ?, bucket_updated_at = ?, updated_at = CURRENT_TIMESTAMP WHERE pipeline_name = ?`,
		tokens, formatHeartbeatTime(effectiveNow), admission.Run.Pipeline); err != nil {
		return err
	}
	return tx.Commit()
}

// FinalizeServiceRun records a completed, verified archive exactly once. A
// retry with identical metadata is idempotent; conflicting metadata fails.
func (s *Store) FinalizeServiceRun(ctx context.Context, finalized PipelineServiceRun) error {
	if strings.TrimSpace(finalized.RunID) == "" || strings.TrimSpace(finalized.ArtifactRelpath) == "" ||
		strings.TrimSpace(finalized.ArtifactSHA256) == "" || strings.TrimSpace(finalized.ProofID) == "" || finalized.ProofVerifiedAt.IsZero() {
		return errors.New("complete pipeline service artifact metadata is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE pipeline_service_runs
		SET artifact_relpath = ?, artifact_sha256 = ?, proof_id = ?, proof_verified_at = ?
		WHERE run_id = ? AND artifact_relpath = '' AND artifact_sha256 = '' AND proof_id = '' AND proof_verified_at = ''`,
		strings.TrimSpace(finalized.ArtifactRelpath), strings.TrimSpace(finalized.ArtifactSHA256), strings.TrimSpace(finalized.ProofID),
		formatHeartbeatTime(finalized.ProofVerifiedAt), strings.TrimSpace(finalized.RunID))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 0 {
		return nil
	}
	existing, ok, err := s.GetServiceRun(ctx, finalized.RunID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("pipeline service run %s not found", finalized.RunID)
	}
	if existing.ArtifactRelpath == strings.TrimSpace(finalized.ArtifactRelpath) &&
		existing.ArtifactSHA256 == strings.TrimSpace(finalized.ArtifactSHA256) &&
		existing.ProofID == strings.TrimSpace(finalized.ProofID) &&
		existing.ProofVerifiedAt.Equal(finalized.ProofVerifiedAt.UTC()) {
		return nil
	}
	return fmt.Errorf("pipeline service run %s is already finalized with different metadata", finalized.RunID)
}

func newPipelineExposureToken() (string, [sha256.Size]byte, error) {
	var raw [pipelineExposureTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("generate pipeline exposure token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	return token, sha256.Sum256([]byte(token)), nil
}

func scanPipelineExposure(row interface{ Scan(...any) error }) (PipelineExposure, error) {
	var exposure PipelineExposure
	var enabled int
	var bucketUpdatedAt, createdAt, updatedAt string
	if err := row.Scan(&exposure.PipelineName, &exposure.SchemaVersion, &exposure.SchemaJSON, &exposure.SchemaHash,
		&exposure.TokenHash, &enabled, &exposure.BucketTokens, &bucketUpdatedAt, &createdAt, &updatedAt); err != nil {
		return PipelineExposure{}, err
	}
	exposure.TokenHash = append([]byte(nil), exposure.TokenHash...)
	exposure.Enabled = enabled != 0
	exposure.BucketUpdatedAt = parseHeartbeatTime(bucketUpdatedAt)
	exposure.CreatedAt = parseStoredTimestamp(createdAt)
	exposure.UpdatedAt = parseStoredTimestamp(updatedAt)
	return exposure, nil
}
