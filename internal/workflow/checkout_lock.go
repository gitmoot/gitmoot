package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
)

const (
	checkoutMutationLockTTL     = 30 * time.Minute
	checkoutMutationBusyMessage = "This checkout is already being mutated by another Gitmoot task. Run tasks sequentially or enable per-task worktrees for parallel implementation."
)

func acquireCheckoutMutationLock(ctx context.Context, store *db.Store, checkoutPath string, ownerID string, now time.Time) (func(context.Context) error, string, error) {
	key, err := checkoutMutationLockKey(checkoutPath)
	if err != nil {
		return nil, "", err
	}
	if key == "" {
		return func(context.Context) error { return nil }, "", nil
	}
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return nil, key, errors.New("checkout mutation lock owner is required")
	}
	token, err := newCheckoutMutationOwnerToken()
	if err != nil {
		return nil, key, err
	}
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerID,
		OwnerToken:  token,
		ExpiresAt:   now.UTC().Add(checkoutMutationLockTTL).Format(time.RFC3339Nano),
	}, now.UTC())
	if err != nil {
		return nil, key, err
	}
	if !acquired {
		return nil, key, BlockedError{Reason: checkoutMutationBusyMessage}
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerID, token)
		return err
	}, key, nil
}

func checkoutMutationLockKey(checkoutPath string) (string, error) {
	checkoutPath = strings.TrimSpace(checkoutPath)
	if checkoutPath == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(checkoutPath)
	if err != nil {
		return "", fmt.Errorf("normalize checkout path: %w", err)
	}
	return "checkout-mutation:" + filepath.Clean(absolute), nil
}

func newCheckoutMutationOwnerToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate checkout mutation lock owner token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}
