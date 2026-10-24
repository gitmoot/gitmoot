package workflow

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestIsImplementDelegationWorktree(t *testing.T) {
	base := JobPayload{DelegationID: "d1", WorktreePath: "/wt/d1", Branch: "gitmoot-delegation-x-d1"}
	if !isImplementDelegationWorktree("implement", base) {
		t.Fatal("implement delegation worktree child not detected")
	}
	if isImplementDelegationWorktree("ask", base) {
		t.Fatal("ask child must not be treated as an implement worktree")
	}
	if isImplementDelegationWorktree("review", base) {
		t.Fatal("review child must not be treated as an implement worktree")
	}
	if isImplementDelegationWorktree("implement", JobPayload{WorktreePath: "/wt/d1", Branch: "b"}) {
		t.Fatal("non-delegation job (no delegation id) must not match")
	}
	if isImplementDelegationWorktree("implement", JobPayload{DelegationID: "d1", Branch: "b"}) {
		t.Fatal("implement child without a worktree path must not match")
	}
	if isImplementDelegationWorktree("implement", JobPayload{DelegationID: "d1", WorktreePath: "/wt/d1"}) {
		t.Fatal("implement child without a branch must not match")
	}
}

func TestCleanupImplementDelegationWorktreeRemovesWorktreeAndBranch(t *testing.T) {
	world := newProducerCleanupFixture(t, false)
	for range 2 {
		if err := world.engine.cleanupImplementDelegationWorktree(context.Background(), world.jobID, "implement", world.payload); err != nil {
			t.Fatal(err)
		}
		world.assertRemoved(t)
	}
	if got := countJobEvents(t, world.store, world.jobID, "delegation_worktree_removed"); got != 1 {
		t.Fatalf("replayed removal produced %d completion events", got)
	}
}

func TestCleanupImplementDelegationWorktreePreservesWhileConsumerLive(t *testing.T) {
	world := newProducerCleanupFixture(t, false)
	ctx := context.Background()
	parent := JobPayload{Repo: "owner/repo", Result: &AgentResult{Decision: "approved", Delegations: []Delegation{
		{ID: "produce", Action: "implement", Prompt: "produce"},
		{ID: "integrate", Action: "review", Prompt: "verify", Deps: []string{"produce"}},
	}}}
	encoded, err := marshalPayload(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpdateJobPayload(ctx, world.payload.ParentJobID, encoded); err != nil {
		t.Fatal(err)
	}
	// A declaration without an enqueued consumer is still a consumer obligation.
	if err := world.engine.cleanupImplementDelegationWorktree(ctx, world.jobID, "implement", world.payload); err != nil {
		t.Fatal(err)
	}
	world.assertPreserved(t)
	consumer := world.payload.ParentJobID + "/delegation/integrate"
	insertCompletedJob(t, world.store, db.Job{ID: consumer, Agent: "consumer", Type: "review", ParentJobID: world.payload.ParentJobID, DelegationID: "integrate"}, JobPayload{
		Repo: "owner/repo", ParentJobID: world.payload.ParentJobID, DelegationID: "integrate", Deps: []string{"produce"}, Result: &AgentResult{Decision: "approved"},
	})
	for _, state := range []JobState{JobQueued, JobRunning, JobBlocked, JobFailed, JobCancelled} {
		if err := world.store.UpdateJobState(ctx, consumer, string(state)); err != nil {
			t.Fatal(err)
		}
		if err := world.engine.cleanupImplementDelegationWorktree(ctx, world.jobID, "implement", world.payload); err != nil {
			t.Fatal(err)
		}
		world.assertPreserved(t)
	}
	if err := world.store.UpdateJobState(ctx, consumer, string(JobSucceeded)); err != nil {
		t.Fatal(err)
	}
	if err := world.engine.cleanupImplementDelegationWorktree(ctx, world.jobID, "implement", world.payload); err != nil {
		t.Fatal(err)
	}
	world.assertRemoved(t)
}

func TestCleanupConsumedImplementLegWorktreesAfterIntegrationTerminal(t *testing.T) {
	world := newProducerCleanupFixture(t, false)
	ctx := context.Background()
	consumer := world.payload.ParentJobID + "/delegation/integrate"
	payload := JobPayload{Repo: "owner/repo", ParentJobID: world.payload.ParentJobID, DelegationID: "integrate", Deps: []string{"produce"}, Result: &AgentResult{Decision: "approved"}}
	insertCompletedJob(t, world.store, db.Job{ID: consumer, Agent: "consumer", Type: "review", ParentJobID: payload.ParentJobID, DelegationID: payload.DelegationID}, payload)
	if err := world.store.UpdateJobState(ctx, consumer, string(JobBlocked)); err != nil {
		t.Fatal(err)
	}
	if err := world.engine.cleanupConsumedImplementLegWorktrees(ctx, payload); err != nil {
		t.Fatal(err)
	}
	world.assertPreserved(t)
	if err := world.store.UpdateJobState(ctx, consumer, string(JobSucceeded)); err != nil {
		t.Fatal(err)
	}
	if err := world.engine.cleanupConsumedImplementLegWorktrees(ctx, payload); err != nil {
		t.Fatal(err)
	}
	world.assertRemoved(t)
}

func TestAdvanceJobTerminalImplementLegFiresCleanup(t *testing.T) {
	for _, state := range []JobState{JobSucceeded, JobFailed} {
		t.Run(string(state), func(t *testing.T) {
			world := newProducerCleanupFixture(t, false)
			ctx := context.Background()
			if state == JobFailed {
				world.payload.Result.Decision = "failed"
				encoded, err := marshalPayload(world.payload)
				if err != nil {
					t.Fatal(err)
				}
				if err := world.store.UpdateJobPayload(ctx, world.jobID, encoded); err != nil {
					t.Fatal(err)
				}
				if err := world.store.UpdateJobState(ctx, world.jobID, string(state)); err != nil {
					t.Fatal(err)
				}
			}
			err := world.engine.AdvanceJob(ctx, world.jobID)
			var blocked BlockedError
			if err != nil && !errors.As(err, &blocked) {
				t.Fatal(err)
			}
			if state == JobSucceeded {
				world.assertRemoved(t)
			} else {
				world.assertPreserved(t)
			}
		})
	}
}

func acquireCleanupRuntimeLock(t *testing.T, world producerCleanupFixture, token string, pid int64, expiresIn time.Duration) {
	t.Helper()
	now := time.Now().UTC()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := world.store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: "runtime:codex:cleanup-session", OwnerJobID: world.jobID,
		OwnerToken: token, OwnerPID: pid, OwnerHostname: host,
		ExpiresAt: now.Add(expiresIn).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		t.Fatalf("runtime lock acquired=%t err=%v", acquired, err)
	}
}

func TestAdvanceJobTerminalImplementLegCleansWhileSelfHoldsLock(t *testing.T) {
	for _, self := range []bool{true, false} {
		name := "foreign owner"
		if self {
			name = "self owner"
		}
		t.Run(name, func(t *testing.T) {
			world := newProducerCleanupFixture(t, false)
			const token = "cleanup-owner-token"
			acquireCleanupRuntimeLock(t, world, token, int64(os.Getpid()), time.Hour)
			ctx := context.Background()
			if self {
				ctx = WithRuntimeSelfOwnerToken(ctx, token)
			}
			if err := world.engine.AdvanceJob(ctx, world.jobID); err != nil {
				t.Fatal(err)
			}
			if self {
				world.assertRemoved(t)
			} else {
				world.assertPreserved(t)
			}
		})
	}
}

func TestCleanupImplementDelegationWorktreeRefusesWhileRuntimeOwnerActive(t *testing.T) {
	for _, tc := range []struct {
		name          string
		lock, pidLive bool
		expiresIn     time.Duration
		removed       bool
	}{
		{"live owner", true, true, time.Hour, false},
		{"unexpired dead owner", true, false, time.Hour, false},
		{"expired dead owner", true, false, -time.Minute, true},
		{"no owner", false, false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := newProducerCleanupFixture(t, false)
			world.engine.OwnerPIDLive = func(int64) bool { return tc.pidLive }
			if tc.lock {
				acquireCleanupRuntimeLock(t, world, "runtime-owner", 4242, tc.expiresIn)
			}
			if err := world.engine.cleanupImplementDelegationWorktree(context.Background(), world.jobID, "implement", world.payload); err != nil {
				t.Fatal(err)
			}
			if tc.removed {
				world.assertRemoved(t)
			} else {
				world.assertPreserved(t)
			}
		})
	}
}

func TestCleanupImplementDelegationWorktreeLeaseExpiryBoundary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		live, known bool
	}{
		{"live process", true, true}, {"unknown process state", false, false}, {"known idle", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := newProducerCleanupFixture(t, false)
			world.engine.WorktreeLiveness = func(string) (bool, bool) { return tc.live, tc.known }
			if err := world.engine.cleanupImplementDelegationWorktree(context.Background(), world.jobID, "implement", world.payload); err != nil {
				t.Fatal(err)
			}
			if tc.known && !tc.live {
				world.assertRemoved(t)
			} else {
				world.assertPreserved(t)
			}
		})
	}
}

func TestCleanupImplementDelegationWorktreeSkipIsIdempotent(t *testing.T) {
	world := newProducerCleanupFixture(t, false)
	live := true
	world.engine.WorktreeLiveness = func(string) (bool, bool) { return live, true }
	for range 3 {
		if err := world.engine.cleanupImplementDelegationWorktree(context.Background(), world.jobID, "implement", world.payload); err != nil {
			t.Fatal(err)
		}
		world.assertPreserved(t)
	}
	if got := countJobEvents(t, world.store, world.jobID, "delegation_worktree_cleanup_skipped"); got != 1 {
		t.Fatalf("repeated live-owner deferrals produced %d diagnostic events", got)
	}
	live = false
	if err := world.engine.cleanupImplementDelegationWorktree(context.Background(), world.jobID, "implement", world.payload); err != nil {
		t.Fatal(err)
	}
	world.assertRemoved(t)
}
