package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// stubWorktreeRootPreflight replaces the root preflight for one test: answer gets the 1-based
// call number and decides that proof.
func stubWorktreeRootPreflight(t *testing.T, answer func(call int, root config.WorktreeRoot) (uint64, error)) *int {
	t.Helper()
	calls := 0
	previous := worktreeRootPreflight
	worktreeRootPreflight = func(root config.WorktreeRoot) (uint64, error) {
		calls++
		return answer(calls, root)
	}
	t.Cleanup(func() { worktreeRootPreflight = previous })
	return &calls
}

func writeWorktreeRootConfig(t *testing.T, home, section string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, config.ConfigName), []byte(section), 0o600); err != nil {
		t.Fatal(err)
	}
}

// #2763 review P2-1: a missing path counts as removed only while its owning root is proven both
// before and after the absence check, on the same mount; the active and retired roots share that
// one proof, and the legacy root needs none.
func TestProveWorktreeAbsentRequiresOwningRootProofAroundCheck(t *testing.T) {
	unavailable := fmt.Errorf("%w: mount gone", ErrWorktreeRootUnavailable)
	for _, kind := range []string{"active", "retired"} {
		t.Run(kind, func(t *testing.T) {
			home, root := t.TempDir(), filepath.Join(t.TempDir(), "worktrees")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			section := fmt.Sprintf("[worktrees]\nroot = %q\n", root)
			if kind == "retired" {
				section = fmt.Sprintf("[[worktrees.retired]]\nroot = %q\n", root)
			}
			writeWorktreeRootConfig(t, home, section)
			path := filepath.Join(root, "owner--repo", "delegations", "job", "seat")
			for _, tc := range []struct {
				name   string
				answer func(int, config.WorktreeRoot) (uint64, error)
				ok     bool
			}{
				{"proven", func(int, config.WorktreeRoot) (uint64, error) { return 7, nil }, true},
				{"lost after check", func(call int, _ config.WorktreeRoot) (uint64, error) {
					if call == 1 {
						return 7, nil
					}
					return 0, unavailable
				}, false},
				{"mount replaced during check", func(call int, _ config.WorktreeRoot) (uint64, error) { return uint64(6 + call), nil }, false},
				{"lost before check", func(int, config.WorktreeRoot) (uint64, error) { return 0, unavailable }, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					calls := stubWorktreeRootPreflight(t, tc.answer)
					err := proveWorktreeAbsent(home, path)
					if tc.ok && err != nil || !tc.ok && !errors.Is(err, ErrWorktreeRootUnavailable) {
						t.Fatalf("proveWorktreeAbsent = %v, want ok=%t (ErrWorktreeRootUnavailable otherwise)", err, tc.ok)
					}
					if tc.ok && *calls != 2 {
						t.Fatalf("root proven %d times, want before and after the check", *calls)
					}
				})
			}
		})
	}
	t.Run("legacy", func(t *testing.T) {
		home := t.TempDir()
		calls := stubWorktreeRootPreflight(t, func(int, config.WorktreeRoot) (uint64, error) { return 0, unavailable })
		if err := proveWorktreeAbsent(home, filepath.Join(home, "worktrees", "owner--repo", "seat")); err != nil || *calls != 0 {
			t.Fatalf("legacy absence = %v after %d root proofs, want nil without any", err, *calls)
		}
	})
}

// #2763 review P2-1: the worktree root loses its mount after cleanup removed the worktree but
// before the receipt is completed. The final absence check must not take the now-empty mount
// point for a removal: nothing is recorded removed, the outcome is the retryable
// worktree_root_unavailable refusal without spending the failure budget, and once the root is
// proven again the same receipt completes. Same for the active and a retired root, for a worktree
// moved to trash (read-only) and one removed in place (own branch).
func TestCleanupApplyDefersWhenRootLosesMountBeforeCompletion(t *testing.T) {
	for _, kind := range []string{"active", "retired"} {
		for _, readOnly := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/readOnly=%t", kind, readOnly), func(t *testing.T) {
				ctx := context.Background()
				world := newProducerCleanupFixture(t, readOnly)
				// The fixture's worktree sits under <old home>/worktrees; a new home makes that
				// directory its configured or retired root, so the worktree's root needs a proof.
				root := filepath.Join(world.engine.Home, "worktrees")
				world.engine.Home = t.TempDir()
				section := fmt.Sprintf("[worktrees]\nroot = %q\n", root)
				if kind == "retired" {
					section = fmt.Sprintf("[[worktrees.retired]]\nroot = %q\n", root)
				}
				writeWorktreeRootConfig(t, world.engine.Home, section)
				// The mount is there while the worktree is; once the worktree is gone from its path
				// the mount is gone too: exactly the mount loss between the earlier checks and the
				// final absence check.
				mounted := true
				stubWorktreeRootPreflight(t, func(int, config.WorktreeRoot) (uint64, error) {
					if _, err := os.Lstat(world.path); err != nil {
						mounted = false
					}
					if !mounted {
						return 0, fmt.Errorf("%w: mount gone", ErrWorktreeRootUnavailable)
					}
					return 7, nil
				})
				decision, err := world.engine.InspectProducerCleanup(ctx, world.jobID, CleanupPolicy{Mode: "ordinary", BudgetGate: CleanupBudgetGateEnforced})
				if err != nil || !decision.Eligible {
					t.Fatalf("resource not eligible under a proven root: %+v %v", decision, err)
				}
				result, err := world.engine.ApplyProducerCleanup(ctx, world.jobID, decision.ResourceID, decision.EligibilityToken, CleanupPolicy{Mode: "ordinary"})
				if err != nil {
					t.Fatalf("mount loss before completion failed the apply: %v", err)
				}
				if result.State == "removed" || !slices.Contains(result.Decision.ReasonCodes, CleanupRefusalWorktreeRootUnavailable) {
					t.Fatalf("mount loss before completion = state %q reasons %v, want the %s refusal", result.State, result.Decision.ReasonCodes, CleanupRefusalWorktreeRootUnavailable)
				}
				if result.Receipt == nil {
					t.Fatal("apply prepared no receipt")
				}
				receipt, err := world.store.GetCleanupReceipt(ctx, decision.ResourceID, result.Receipt.LifecycleGeneration)
				if err != nil || receipt.RemovedAt != "" {
					t.Fatalf("receipt recorded removed while the root was unproven: %+v %v", receipt, err)
				}
				obligation, err := world.store.GetCleanupObligation(ctx, decision.ResourceID)
				if err != nil || obligation.State == db.CleanupObligationRemoved || obligation.State == db.CleanupObligationQuarantined || obligation.AttemptCount != 0 {
					t.Fatalf("obligation after mount loss = %+v %v, want open without a spent attempt", obligation, err)
				}
				// The root is proven again: the retry completes the same receipt.
				mounted = true
				stubWorktreeRootPreflight(t, func(int, config.WorktreeRoot) (uint64, error) { return 7, nil })
				retried, err := world.engine.ApplyProducerCleanup(ctx, world.jobID, decision.ResourceID, decision.EligibilityToken, CleanupPolicy{Mode: "ordinary"})
				if err != nil || retried.State != "removed" || retried.Receipt == nil || retried.Receipt.RemovedAt == "" || retried.Receipt.ReceiptID != receipt.ReceiptID {
					t.Fatalf("retry under the proven root = %+v %v, want the same receipt removed", retried, err)
				}
			})
		}
	}
}
