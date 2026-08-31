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

	"github.com/gitmoot/gitmoot/internal/db"
)

const (
	checkoutMutationLockTTL     = 30 * time.Minute
	checkoutMutationWaitTimeout = 2 * time.Minute
	checkoutMutationWaitBackoff = 100 * time.Millisecond
	checkoutMutationBusyMessage = "This checkout is already being mutated by another Gitmoot task. Run tasks sequentially or enable per-task worktrees for parallel implementation."
)

// AcquireCheckoutMutationLock is the EXPORTED seam for out-of-package callers (the
// CLI hard-verifier sandbox provisioner, #474/#617) to serialize a shared-checkout
// operation on the SAME resource key every in-package worktree/checkout mutation uses
// (AllocateTaskWorktree, AllocateReadOnlyDelegationWorktree, the merge gate). It waits
// up to the standard budget for a concurrent holder, then returns a release the caller
// MUST call to drop the lock. An empty checkoutPath is a no-op that returns a no-op
// release (nothing to serialize). Callers hold it only around the short shared-repo
// op (e.g. a local clone that reads the base .git) and release it before any long,
// self-contained work, so the leg never stalls a real job for more than that op.
func AcquireCheckoutMutationLock(ctx context.Context, store *db.Store, checkoutPath string, ownerID string, now time.Time) (func(context.Context) error, error) {
	release, _, err := acquireCheckoutMutationLockWithWait(ctx, store, checkoutPath, ownerID, now)
	return release, err
}

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

func acquireCheckoutMutationLockWithWait(ctx context.Context, store *db.Store, checkoutPath string, ownerID string, now time.Time) (func(context.Context) error, string, error) {
	return acquireCheckoutMutationLockWithWaitBudget(ctx, store, checkoutPath, ownerID, now, checkoutMutationWaitTimeout, checkoutMutationWaitBackoff)
}

func acquireCheckoutMutationLockWithWaitBudget(ctx context.Context, store *db.Store, checkoutPath string, ownerID string, now time.Time, waitBudget time.Duration, backoff time.Duration) (func(context.Context) error, string, error) {
	release, key, err := acquireCheckoutMutationLock(ctx, store, checkoutPath, ownerID, now)
	if err == nil {
		return release, key, nil
	}
	var blocked BlockedError
	if !errors.As(err, &blocked) || waitBudget <= 0 {
		return nil, key, err
	}
	if backoff <= 0 {
		backoff = checkoutMutationWaitBackoff
	}
	deadline := now.UTC().Add(waitBudget)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, key, checkoutMutationWaitExpired(waitBudget)
		}
		sleepFor := backoff
		if remaining < sleepFor {
			sleepFor = remaining
		}
		timer := time.NewTimer(sleepFor)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, key, ctx.Err()
		case <-timer.C:
		}
		release, key, err = acquireCheckoutMutationLock(ctx, store, checkoutPath, ownerID, time.Now().UTC())
		if err == nil {
			return release, key, nil
		}
		if !errors.As(err, &blocked) {
			return nil, key, err
		}
	}
}

func checkoutMutationWaitExpired(waitBudget time.Duration) BlockedError {
	return BlockedError{Reason: fmt.Sprintf("%s Waited up to %s for the checkout mutation lock.", checkoutMutationBusyMessage, waitBudget.Round(time.Second))}
}

// CheckoutMutationLockContention reports whether err is the self-healing
// checkout-mutation-lock block: another worker holds the shared .git for a short
// op (a detached `git worktree add`, the merge gate) and releases it, so a waiter
// that spent its budget succeeds on a later attempt. It is deliberately narrow —
// the BlockedError TYPE and the mutation-busy reason together — so a caller that
// HOLDS a job on it cannot also hold one on an unrelated block. The daemon's
// pre-flight uses it to defer a read-only worktree allocation for re-dispatch
// rather than terminally failing the job; every other allocation failure (a
// missing commit object, an unwritable path) is not this error and stays terminal.
func CheckoutMutationLockContention(err error) bool {
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		return false
	}
	return strings.Contains(blocked.Reason, checkoutMutationBusyMessage)
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
