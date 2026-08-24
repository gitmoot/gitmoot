package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

func quarantineCleanupObligation(t *testing.T, store *db.Store, jobID, path string) {
	t.Helper()
	now := time.Now().UTC()
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := store.RecordCleanupObligationFailure(context.Background(), jobID, path, db.CleanupReasonUnknown, errors.New("persistent failure"), now, now.Add(time.Minute), 3); err != nil {
			t.Fatalf("quarantine cleanup obligation: %v", err)
		}
	}
}

// TestQuarantinedCleanupObligationBlocksEveryDirectActuator kills mutants that
// discard prepareDelegationCleanupObligation's actuation decision in any direct
// fix, read-only, or implement cleanup path.
func TestQuarantinedCleanupObligationBlocksEveryDirectActuator(t *testing.T) {
	t.Run("fix", func(t *testing.T) {
		ctx := context.Background()
		store := openEngineStore(t)
		engine := testEngine(store)
		engine.cleanupTargetValidator = nil
		engine.Home = t.TempDir()
		jobID := "fix-job"
		path, err := FixWorktreePath(engine.Home, "owner/repo", jobID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		quarantineCleanupObligation(t, store, jobID, path)
		payload := JobPayload{Repo: "owner/repo", WorktreePath: path, FixWorktree: true}
		for range 3 {
			if err := engine.cleanupFixWorktree(ctx, jobID, "implement", payload); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("quarantined fix worktree was actuated: %v", err)
		}
	})

	for _, tc := range []struct {
		name    string
		jobID   string
		jobType string
		payload func(home string) JobPayload
		cleanup func(Engine, context.Context, string, string, JobPayload) error
	}{
		{
			name: "read-only", jobID: "ask-job", jobType: "ask",
			payload: func(home string) JobPayload {
				path, _ := DelegationWorktreePath(home, "owner/repo", "ask-job", "readonly-seat", 0)
				return JobPayload{Repo: "owner/repo", WorktreePath: path, ReadOnlyWorktree: true}
			},
			cleanup: func(engine Engine, ctx context.Context, jobID, jobType string, payload JobPayload) error {
				return engine.cleanupReadOnlyDelegationWorktree(ctx, jobID, jobType, payload)
			},
		},
		{
			name: "implement", jobID: "parent/delegation/build", jobType: "implement",
			payload: func(home string) JobPayload {
				path, _ := DelegationWorktreePath(home, "owner/repo", "parent", "build", 0)
				return JobPayload{Repo: "owner/repo", ParentJobID: "parent", DelegationID: "build", WorktreePath: path, Branch: "gitmoot-delegation-build"}
			},
			cleanup: func(engine Engine, ctx context.Context, jobID, jobType string, payload JobPayload) error {
				return engine.cleanupImplementDelegationWorktree(ctx, jobID, jobType, payload)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			engine := testEngine(store)
			engine.cleanupTargetValidator = nil
			engine.Home = t.TempDir()
			engine.DelegationCheckout = t.TempDir()
			manager := &fakeWorktreeManager{existingBranches: map[string]bool{"gitmoot-delegation-build": true}}
			engine.DelegationWorktrees = manager
			payload := tc.payload(engine.Home)
			if err := os.MkdirAll(payload.WorktreePath, 0o755); err != nil {
				t.Fatal(err)
			}
			quarantineCleanupObligation(t, store, tc.jobID, payload.WorktreePath)
			for range 3 {
				if err := tc.cleanup(engine, ctx, tc.jobID, tc.jobType, payload); err != nil {
					t.Fatal(err)
				}
			}
			if len(manager.removedForce) != 0 || len(manager.deletedBranches) != 0 {
				t.Fatalf("quarantined cleanup actuated: removed=%v branches=%v", manager.removedForce, manager.deletedBranches)
			}
		})
	}
}

// TestAdvanceRetryDoesNotActuateQuarantinedCleanup kills the mutant that checks
// quarantine only in daemon selectors while leaving AdvanceJob's deferred direct
// cleanup active on every advancement retry.
func TestAdvanceRetryDoesNotActuateQuarantinedCleanup(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.cleanupTargetValidator = nil
	engine.Home = t.TempDir()
	engine.DelegationCheckout = t.TempDir()
	manager := &fakeWorktreeManager{}
	engine.DelegationWorktrees = manager
	jobID := "advance-retry-job"
	path, err := DelegationWorktreePath(engine.Home, "owner/repo", jobID, "readonly-seat", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := JobPayload{
		Repo: "owner/repo", WorktreePath: path, ReadOnlyWorktree: true,
		Result: &AgentResult{Decision: "approved", Summary: "done"},
	}
	insertCompletedJob(t, store, db.Job{ID: jobID, Agent: "audit", Type: "ask"}, payload)
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "advance_retry", Message: "persistent advance failure"}); err != nil {
		t.Fatal(err)
	}
	quarantineCleanupObligation(t, store, jobID, path)
	for range 3 {
		_ = engine.AdvanceJob(ctx, jobID)
	}
	if len(manager.removedForce) != 0 {
		t.Fatalf("advance retry actuated quarantined cleanup %d times", len(manager.removedForce))
	}
}

// TestCleanupTargetRejectsSymlinkEscapeAndIdentityMismatch kills two independent
// mutants: replacing resolved containment with filepath.Rel, and accepting any
// in-root path instead of a path derived from the owner identity.
func TestCleanupTargetRejectsSymlinkEscapeAndIdentityMismatch(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "worktrees", "owner--repo")
	outside := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "delegations")); err != nil {
		t.Fatal(err)
	}
	path, err := DelegationWorktreePath(home, "owner/repo", "parent", "build", 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := JobPayload{Repo: "owner/repo", ParentJobID: "parent", DelegationID: "build", WorktreePath: path, Branch: "branch"}
	if err := ValidateDelegationCleanupTarget(home, "parent/delegation/build", "implement", payload); err == nil {
		t.Fatal("symlinked ancestor escaping the managed root was accepted")
	}

	home = t.TempDir()
	mismatched, err := DelegationWorktreePath(home, "other/repo", "parent", "build", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mismatched, 0o755); err != nil {
		t.Fatal(err)
	}
	payload.WorktreePath = mismatched
	if err := ValidateDelegationCleanupTarget(home, "parent/delegation/build", "implement", payload); err == nil {
		t.Fatal("path inconsistent with the payload repo identity was accepted")
	}
}
