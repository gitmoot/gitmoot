package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

func acquireRuntimeSessionLock(ctx context.Context, store *db.Store, jobID string, agent runtime.Agent, now time.Time, ttl time.Duration) (func(context.Context) error, bool, string, error) {
	key, ok := runtimeSessionResourceKey(agent)
	if !ok {
		return func(context.Context) error { return nil }, true, "", nil
	}
	if ttl <= 0 {
		return nil, false, key, fmt.Errorf("runtime lock ttl must be positive")
	}
	ownerToken, err := newRuntimeLockOwnerToken()
	if err != nil {
		return nil, false, key, err
	}
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  jobID,
		OwnerToken:  ownerToken,
		ExpiresAt:   now.UTC().Add(ttl).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		return func(context.Context) error { return nil }, acquired, key, err
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, jobID, ownerToken)
		return err
	}, true, key, nil
}

func runtimeSessionResourceKey(agent runtime.Agent) (string, bool) {
	runtimeName := strings.TrimSpace(agent.Runtime)
	runtimeRef := strings.TrimSpace(agent.RuntimeRef)
	switch runtimeName {
	case runtime.CodexRuntime, runtime.ClaudeRuntime, runtime.KimiRuntime:
	default:
		return "", false
	}
	if runtimeRef == "" {
		return "", false
	}
	return "runtime:" + runtimeName + ":" + runtimeRef, true
}

func newRuntimeLockOwnerToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate runtime lock owner token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}
