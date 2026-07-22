package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

var errSkillOptTrainGenerationBusy = errors.New("skillopt train generation is already running")

var errSkillOptTrainOptimizerBusy = errors.New("skillopt train optimizer is already running")

var errSkillOptTrainCandidateReviewBusy = errors.New("skillopt train candidate review is already publishing")

var errSkillOptTrainReviewBusy = errors.New("skillopt train review is already publishing")

var errSkillOptTrainStartNextBusy = errors.New("skillopt train next iteration is already starting")

const skillOptTrainGenerationLockTTL = 2 * time.Hour

const skillOptTrainGenerationLockBuffer = 10 * time.Minute

const skillOptTrainOptimizerLockTTL = 4 * time.Hour

const skillOptTrainOptimizerLockBuffer = 10 * time.Minute

const skillOptTrainOptimizerHeartbeatLeaseTTL = 2 * time.Minute

const skillOptTrainOptimizerExpiredHeartbeatGrace = 10 * time.Minute

const skillOptTrainCandidateReviewLockTTL = 30 * time.Minute

const skillOptTrainReviewLockTTL = 30 * time.Minute

const skillOptTrainStartNextLockTTL = 30 * time.Minute

func acquireSkillOptTrainCandidateReviewLock(ctx context.Context, store *db.Store, sessionID string, iterationID string) (func(context.Context) error, bool, error) {
	key := skillOptTrainCandidateReviewLockKey(sessionID, iterationID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-candidate-review", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(skillOptTrainCandidateReviewLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainCandidateReviewBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func skillOptTrainCandidateReviewLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-candidate-review:" + sessionID
	}
	return "skillopt-train-candidate-review:" + sessionID + ":" + iterationID
}

func acquireSkillOptTrainReviewLock(ctx context.Context, store *db.Store, sessionID string, iterationID string) (func(context.Context) error, bool, error) {
	key := skillOptTrainReviewLockKey(sessionID, iterationID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-review", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(skillOptTrainReviewLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainReviewBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func skillOptTrainReviewLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-review:" + sessionID
	}
	return "skillopt-train-review:" + sessionID + ":" + iterationID
}

func acquireSkillOptTrainStartNextLock(ctx context.Context, store *db.Store, sessionID string) (func(context.Context) error, bool, error) {
	key := skillOptTrainStartNextLockKey(sessionID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-start-next", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(skillOptTrainStartNextLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainStartNextBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func skillOptTrainStartNextLockKey(sessionID string) string {
	return "skillopt-train-start-next:" + strings.TrimSpace(sessionID)
}

func acquireSkillOptTrainOptimizerLock(ctx context.Context, store *db.Store, sessionID string, iterationID string, ttl time.Duration, request skillOptTrainOptimizerRequest) (func(context.Context) error, string, error) {
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, "", err
	}
	legacyToken, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, "", err
	}
	if ttl <= 0 {
		ttl = skillOptTrainOptimizerLockTTL
	}
	leaseTTL := skillOptTrainOptimizerLeaseTTL(ttl)
	now := time.Now().UTC()
	lockKeys := skillOptTrainOptimizerLockKeys(sessionID, iterationID)
	lockState := "acquired"
	for _, existingKey := range lockKeys {
		if existing, err := store.GetResourceLock(ctx, existingKey); err == nil {
			if skillOptTrainOptimizerLockStatus(existing, now) == "stale" {
				released, releaseErr := store.ReleaseResourceLock(ctx, existingKey, existing.OwnerJobID, existing.OwnerToken)
				if releaseErr != nil {
					return noopAgentReservationRelease, "", releaseErr
				}
				if !released {
					return noopAgentReservationRelease, "", skillOptTrainOptimizerLockBusyError(existingKey, existing, now)
				}
				lockState = "recovered_stale"
				continue
			}
			return noopAgentReservationRelease, "", skillOptTrainOptimizerLockBusyError(existingKey, existing, now)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return noopAgentReservationRelease, "", err
		}
	}
	ownerJobID := localAgentJobID("skillopt-train-optimizer", strings.TrimSpace(sessionID))
	hostname, _ := os.Hostname()
	newKey := skillOptTrainOptimizerLockKey(sessionID, iterationID)
	legacyKey := skillOptTrainLegacyOptimizerLockKey(sessionID, iterationID)
	lockMetadata := db.ResourceLock{
		OwnerJobID:    ownerJobID,
		OwnerToken:    token,
		OwnerPID:      int64(os.Getpid()),
		OwnerHostname: hostname,
		// Boot id (#651) so a same-host owner from a prior boot is provably dead
		// without trusting a possibly-reused pid.
		OwnerBootID: db.BootID(),
		CommandHash: skillOptTrainOptimizerRequestHash(request),
		ExpiresAt:   now.Add(leaseTTL).Format(time.RFC3339Nano),
	}
	lockMetadata.ResourceKey = newKey
	acquired, err := store.AcquireResourceLock(ctx, lockMetadata, now)
	if err != nil {
		return noopAgentReservationRelease, "", err
	}
	if !acquired {
		if existing, lockErr := store.GetResourceLock(ctx, newKey); lockErr == nil {
			return noopAgentReservationRelease, "", skillOptTrainOptimizerLockBusyError(newKey, existing, time.Now().UTC())
		}
		return noopAgentReservationRelease, "", fmt.Errorf("%w: %s", errSkillOptTrainOptimizerBusy, newKey)
	}
	lockMetadata.ResourceKey = legacyKey
	lockMetadata.OwnerToken = legacyToken
	legacyAcquired, err := store.AcquireResourceLock(ctx, lockMetadata, now)
	if err != nil {
		_, _ = store.ReleaseResourceLock(context.Background(), newKey, ownerJobID, token)
		return noopAgentReservationRelease, "", err
	}
	if !legacyAcquired {
		_, _ = store.ReleaseResourceLock(context.Background(), newKey, ownerJobID, token)
		if existing, lockErr := store.GetResourceLock(ctx, legacyKey); lockErr == nil {
			return noopAgentReservationRelease, "", skillOptTrainOptimizerLockBusyError(legacyKey, existing, time.Now().UTC())
		}
		return noopAgentReservationRelease, "", fmt.Errorf("%w: %s", errSkillOptTrainOptimizerBusy, legacyKey)
	}
	stopHeartbeat := startSkillOptTrainOptimizerLockHeartbeat(context.Background(), store, newKey, ownerJobID, token, leaseTTL)
	stopLegacyHeartbeat := startSkillOptTrainOptimizerLockHeartbeat(context.Background(), store, legacyKey, ownerJobID, legacyToken, leaseTTL)
	return func(releaseCtx context.Context) error {
		stopHeartbeat()
		stopLegacyHeartbeat()
		_, err := store.ReleaseResourceLock(releaseCtx, newKey, ownerJobID, token)
		_, legacyErr := store.ReleaseResourceLock(releaseCtx, legacyKey, ownerJobID, legacyToken)
		if err != nil {
			return err
		}
		return legacyErr
	}, lockState, nil
}

func skillOptTrainOptimizerLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train:" + sessionID
	}
	return "skillopt-train:" + sessionID + ":" + iterationID
}

func skillOptTrainLegacyOptimizerLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-optimizer:" + sessionID
	}
	return "skillopt-train-optimizer:" + sessionID + ":" + iterationID
}

func skillOptTrainOptimizerLockKeys(sessionID string, iterationID string) []string {
	return []string{
		skillOptTrainOptimizerLockKey(sessionID, iterationID),
		skillOptTrainLegacyOptimizerLockKey(sessionID, iterationID),
	}
}

func skillOptTrainOptimizerLeaseTTL(maxRuntimeTTL time.Duration) time.Duration {
	if maxRuntimeTTL > 0 && maxRuntimeTTL < skillOptTrainOptimizerHeartbeatLeaseTTL {
		return maxRuntimeTTL
	}
	return skillOptTrainOptimizerHeartbeatLeaseTTL
}

func startSkillOptTrainOptimizerLockHeartbeat(ctx context.Context, store *db.Store, key string, ownerJobID string, ownerToken string, leaseTTL time.Duration) func() {
	heartbeatEvery := skillOptTrainOptimizerHeartbeatInterval(leaseTTL)
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case now := <-ticker.C:
				_, _ = store.HeartbeatResourceLock(context.Background(), key, ownerToken, now.UTC().Add(leaseTTL))
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func skillOptTrainOptimizerHeartbeatInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 30 * time.Second
	}
	interval := ttl / 4
	if interval <= 0 {
		return time.Second
	}
	if interval > 30*time.Second {
		return 30 * time.Second
	}
	return interval
}

func skillOptTrainOptimizerRequestHash(request skillOptTrainOptimizerRequest) string {
	content, err := json.Marshal(map[string]any{
		"backend":           strings.TrimSpace(request.Backend),
		"model":             strings.TrimSpace(request.Model),
		"optimizer_model":   strings.TrimSpace(request.OptimizerModel),
		"target_model":      strings.TrimSpace(request.TargetModel),
		"optimizer_backend": strings.TrimSpace(request.OptimizerBackend),
		"target_backend":    strings.TrimSpace(request.TargetBackend),
		"evaluator_id":      strings.TrimSpace(request.EvaluatorID),
		"evaluator_model":   strings.TrimSpace(request.EvaluatorModel),
		"evaluator_backend": strings.TrimSpace(request.EvaluatorBackend),
		"skill_update_mode": strings.TrimSpace(request.SkillUpdateMode),
		"num_epochs":        request.NumEpochs,
		"batch_size":        request.BatchSize,
		"optimizer_views": map[string]any{
			"set":   request.OptimizerViewsSet,
			"value": request.OptimizerViews,
		},
		"retry_optimizer_views": map[string]any{
			"set":   request.RetryOptimizerViewsSet,
			"value": strings.TrimSpace(request.RetryOptimizerViews),
		},
		"noop_retry_budget": map[string]any{
			"set":   request.NoopRetryBudgetSet,
			"value": request.NoopRetryBudget,
		},
		"gate_reject_retry_budget": map[string]any{
			"set":   request.GateRejectRetryBudgetSet,
			"value": request.GateRejectRetryBudget,
		},
		"wrong_artifact_retry_budget": map[string]any{
			"set":   request.WrongArtifactRetryBudgetSet,
			"value": request.WrongArtifactRetryBudget,
		},
		"target_artifact_retry_budget": map[string]any{
			"set":   request.TargetArtifactRetryBudgetSet,
			"value": request.TargetArtifactRetryBudget,
		},
		"hard_failure_retry_budget": map[string]any{
			"set":   request.HardFailureRetryBudgetSet,
			"value": request.HardFailureRetryBudget,
		},
		"feedback_direct_mode": strings.TrimSpace(request.FeedbackDirectMode),
		"final_eval":           request.FinalEval,
		"final_eval_set":       request.FinalEvalSet,
		"gate":                 strings.TrimSpace(request.Gate),
		"timeout":              strings.TrimSpace(request.Timeout),
		"dry_run":              request.DryRun,
		"rerun_optimizer":      request.RerunOptimizer,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func skillOptTrainOptimizerLockBusyError(key string, lock db.ResourceLock, now time.Time) error {
	status := skillOptTrainOptimizerLockStatus(lock, now)
	message := fmt.Sprintf("%s (%s owner=%s pid=%s host=%s heartbeat=%s expires=%s elapsed=%s hash=%s)",
		key,
		status,
		emptyText(lock.OwnerJobID),
		skillOptLockPIDText(lock.OwnerPID),
		emptyText(lock.OwnerHostname),
		emptyText(lock.UpdatedAt),
		emptyText(lock.ExpiresAt),
		skillOptLockElapsedText(lock.AcquiredAt, now),
		emptyText(lock.CommandHash),
	)
	return fmt.Errorf("%w: %s", errSkillOptTrainOptimizerBusy, message)
}

func skillOptTrainOptimizerLockStatus(lock db.ResourceLock, now time.Time) string {
	expired := false
	var expiresAt time.Time
	if parsed, ok := parseSkillOptStatusTime(lock.ExpiresAt); ok {
		expiresAt = parsed
		expired = !expiresAt.After(now)
	}
	if expired {
		if !skillOptOwnerPIDLive(lock.OwnerPID) || now.Sub(expiresAt) >= skillOptTrainOptimizerExpiredHeartbeatGrace {
			return "stale"
		}
		return "active_expired_heartbeat"
	}
	return "active"
}

func skillOptOwnerPIDLive(pid int64) bool {
	if pid <= 0 {
		return false
	}
	running, err := processRunning(int(pid))
	return err == nil && running
}

func skillOptLockPIDText(pid int64) string {
	if pid <= 0 {
		return "-"
	}
	return strconv.FormatInt(pid, 10)
}

func skillOptLockElapsedText(acquiredAt string, now time.Time) string {
	acquired, ok := parseSkillOptStatusTime(acquiredAt)
	if !ok {
		return "unknown"
	}
	elapsed := now.Sub(acquired)
	if elapsed < 0 {
		return "unknown"
	}
	return elapsed.Round(time.Second).String()
}

func skillOptTrainOptimizerLockTTLForRequest(request skillOptTrainOptimizerRequest) (time.Duration, error) {
	timeout := strings.TrimSpace(request.Timeout)
	if timeout == "" {
		return skillOptTrainOptimizerLockTTL, nil
	}
	duration, err := time.ParseDuration(timeout)
	if err != nil {
		return 0, fmt.Errorf("parse optimizer timeout: %w", err)
	}
	if duration <= 0 {
		return 0, errors.New("optimizer timeout must be greater than zero")
	}
	ttl := duration + skillOptTrainOptimizerLockBuffer
	if ttl < skillOptTrainOptimizerLockTTL {
		return skillOptTrainOptimizerLockTTL, nil
	}
	return ttl, nil
}

// skillOptTrainNoopExtend is a no-op lock-extend function for failure/automated paths.
func skillOptTrainNoopExtend() error { return nil }

func acquireSkillOptTrainGenerationLock(ctx context.Context, store *db.Store, sessionID string, iterationID string, ttl time.Duration) (release func(context.Context) error, extend func() error, acquired bool, err error) {
	key := skillOptTrainGenerationLockKey(sessionID, iterationID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, skillOptTrainNoopExtend, false, err
	}
	if ttl <= 0 {
		ttl = skillOptTrainGenerationLockTTL
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-generation", strings.TrimSpace(sessionID))
	hostname, _ := os.Hostname()
	ok, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey:   key,
		OwnerJobID:    ownerJobID,
		OwnerToken:    token,
		OwnerPID:      int64(os.Getpid()),
		OwnerHostname: hostname,
		// Boot id (#651): recover --generation treats a same-host owner from a
		// different boot as definitively dead (PID-reuse-immune, no kill(2)).
		OwnerBootID: db.BootID(),
		ExpiresAt:   now.Add(ttl).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, skillOptTrainNoopExtend, false, err
	}
	if !ok {
		return noopAgentReservationRelease, skillOptTrainNoopExtend, false, fmt.Errorf("%w: %s", errSkillOptTrainGenerationBusy, key)
	}
	release = func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}
	// extend pushes the lock TTL forward so a long generation run does not
	// outlive the upfront estimate (called from the per-option progress hook).
	extend = func() error {
		extendNow := time.Now().UTC()
		_, err := store.HeartbeatResourceLock(context.Background(), key, token, extendNow.Add(ttl))
		return err
	}
	return release, extend, true, nil
}

func skillOptTrainGenerationLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-generation:" + sessionID
	}
	return "skillopt-train-generation:" + sessionID + ":" + iterationID
}
