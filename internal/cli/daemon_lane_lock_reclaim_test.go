package cli

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func acquireTaskLaneLock(t *testing.T, store *db.Store, branch string) {
	t.Helper()
	acquired, err := store.AcquireLock(context.Background(), db.BranchLock{
		RepoFullName: "owner/repo", Branch: branch, Owner: "lead",
	})
	if err != nil {
		t.Fatalf("AcquireLock(%s): %v", branch, err)
	}
	if !acquired {
		t.Fatalf("AcquireLock(%s) did not acquire", branch)
	}
}

func assertTaskLaneLock(t *testing.T, store *db.Store, branch string, want bool) {
	t.Helper()
	_, err := store.GetBranchLock(context.Background(), "owner/repo", branch)
	if want && err != nil {
		t.Fatalf("GetBranchLock(%s): %v", branch, err)
	}
	if !want && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetBranchLock(%s) error = %v, want sql.ErrNoRows", branch, err)
	}
}

// TestStaleTaskLaneLockSweepReleasesTerminalLane is the positive firing guard:
// an aged lane referenced only by terminal task/job rows must be released.
func TestStaleTaskLaneLockSweepReleasesTerminalLane(t *testing.T) {
	ctx := context.Background()
	store := openCLIJobStore(t, t.TempDir())
	const branch = "terminal-lane"
	acquireTaskLaneLock(t, store, branch)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-terminal", RepoFullName: "owner/repo", State: string(workflow.TaskDismissed), Branch: branch,
	}); err != nil {
		t.Fatal(err)
	}
	seedCLIJob(t, store, db.Job{
		ID: "job-terminal", Agent: "lead", Type: "implement", State: string(workflow.JobFailed), Repo: "owner/repo",
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: branch, TaskID: "task-terminal"}),
	}, "terminal")

	if err := reclaimStaleTaskLaneLocks(ctx, store, "owner/repo", io.Discard, time.Now().UTC().Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertTaskLaneLock(t, store, branch, false)
}

// TestStaleTaskLaneLockSweepEnforcesAgeFloor kills the mutant that passes a
// zero cutoff to ReleaseBranchLockIfInactiveWithEvent: an otherwise-idle lane
// acquired less than 24 hours ago must survive.
func TestStaleTaskLaneLockSweepEnforcesAgeFloor(t *testing.T) {
	store := openCLIJobStore(t, t.TempDir())
	const branch = "fresh-lane"
	acquireTaskLaneLock(t, store, branch)

	if err := reclaimStaleTaskLaneLocks(context.Background(), store, "owner/repo", io.Discard, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	assertTaskLaneLock(t, store, branch, true)
}

// TestStaleTaskLaneLockSweepKeepsResumableTaskStates kills mutants that remove
// blocked or awaiting states from the explicit retention allowlist. All three
// states can resume through an operator or human decision and must keep the lane.
func TestStaleTaskLaneLockSweepKeepsResumableTaskStates(t *testing.T) {
	for _, state := range []workflow.TaskState{
		workflow.TaskBlocked,
		workflow.TaskAwaitingHumanMerge,
		workflow.TaskAwaitingHuman,
	} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			store := openCLIJobStore(t, t.TempDir())
			branch := "kept-" + string(state)
			acquireTaskLaneLock(t, store, branch)
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-" + string(state), RepoFullName: "owner/repo", State: string(state), Branch: branch,
			}); err != nil {
				t.Fatal(err)
			}

			if err := reclaimStaleTaskLaneLocks(ctx, store, "owner/repo", io.Discard, time.Now().UTC().Add(25*time.Hour)); err != nil {
				t.Fatal(err)
			}
			assertTaskLaneLock(t, store, branch, true)
		})
	}
}

// TestStaleTaskLaneLockSweepRequiresNoNonTerminalReferences guards the dangerous
// direction: a reviewing task retains its lane after its implement job succeeds,
// and any queued branch job vetoes reclaim even if the task row is terminal.
func TestStaleTaskLaneLockSweepRequiresNoNonTerminalReferences(t *testing.T) {
	t.Run("succeeded implement with reviewing task vetoes reclaim", func(t *testing.T) {
		ctx := context.Background()
		store := openCLIJobStore(t, t.TempDir())
		const branch = "review-lane"
		acquireTaskLaneLock(t, store, branch)
		if err := store.UpsertTask(ctx, db.Task{
			ID: "task-review", RepoFullName: "owner/repo", State: string(workflow.TaskReviewing), Branch: branch,
		}); err != nil {
			t.Fatal(err)
		}
		seedCLIJob(t, store, db.Job{
			ID: "job-implemented", Agent: "lead", Type: "implement", State: string(workflow.JobSucceeded), Repo: "owner/repo",
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: branch, TaskID: "task-review"}),
		}, "implemented")

		if err := reclaimStaleTaskLaneLocks(ctx, store, "owner/repo", io.Discard, time.Now().UTC().Add(25*time.Hour)); err != nil {
			t.Fatal(err)
		}
		assertTaskLaneLock(t, store, branch, true)
	})

	t.Run("queued branch job vetoes reclaim", func(t *testing.T) {
		ctx := context.Background()
		store := openCLIJobStore(t, t.TempDir())
		const branch = "queued-lane"
		acquireTaskLaneLock(t, store, branch)
		if err := store.UpsertTask(ctx, db.Task{
			ID: "task-done", RepoFullName: "owner/repo", State: string(workflow.TaskDismissed), Branch: branch,
		}); err != nil {
			t.Fatal(err)
		}
		seedCLIJob(t, store, db.Job{
			ID: "job-queued", Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Repo: "owner/repo",
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: branch, TaskID: "task-done"}),
		}, "queued")

		if err := reclaimStaleTaskLaneLocks(ctx, store, "owner/repo", io.Discard, time.Now().UTC().Add(25*time.Hour)); err != nil {
			t.Fatal(err)
		}
		assertTaskLaneLock(t, store, branch, true)
	})

	t.Run("same branch in another repo does not veto", func(t *testing.T) {
		ctx := context.Background()
		store := openCLIJobStore(t, t.TempDir())
		const branch = "shared-name"
		acquireTaskLaneLock(t, store, branch)
		seedCLIJob(t, store, db.Job{
			ID: "job-other-repo", Agent: "lead", Type: "implement", State: string(workflow.JobQueued), Repo: "other/repo",
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: "other/repo", Branch: branch, TaskID: "task-other"}),
		}, "queued elsewhere")

		if err := reclaimStaleTaskLaneLocks(ctx, store, "owner/repo", io.Discard, time.Now().UTC().Add(25*time.Hour)); err != nil {
			t.Fatal(err)
		}
		assertTaskLaneLock(t, store, branch, false)
	})
}
