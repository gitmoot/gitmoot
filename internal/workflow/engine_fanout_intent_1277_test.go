package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1277. The skip-native-review-fanout intent must survive an engine-initiated
// hop. A coordinator dispatched with --skip-native-review-fanout that delegates
// its implement leg produced a child whose payload had the flag CLEARED, so the
// child's PR-open re-armed the native fanout and enlisted the implementer as its
// own reviewer (#1236). delegationRequest inherits ~25 parent fields — including
// RiskTier, ActingOrgRole and the cockpit trio — but dropped this one.
func TestDelegationChildInheritsSkipNativeReviewFanout(t *testing.T) {
	store := openEngineStore(t)
	engine := testEngine(store)

	parent := db.Job{ID: "coordinator-job", Agent: "lead", Type: "ask"}
	payload := JobPayload{
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-7",
		TaskID:                 "task-7",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: true,
	}

	child := engine.delegationRequest(parent, payload, Delegation{
		ID:     "leg-1",
		Agent:  "impl",
		Action: "implement",
		Prompt: "do the work",
	})

	if !child.SkipNativeReviewFanout {
		t.Fatalf("delegation child dropped SkipNativeReviewFanout: %+v", child)
	}
}

// #1277. An implement job that carries the intent but does NOT open a PR must
// still publish it onto the branch lock. This is the arm the issue measured:
// PR #1275 was opened BY HAND on a branch whose implement dispatch had passed
// the flag, and because the persist lived behind the PullRequest > 0 branch the
// lock still read skip=false, so the daemon's PR-watcher armed six review jobs.
// Persisting on the no-PR advance makes the intent a property of the BRANCH, so
// it no longer matters who opens the PR.
func TestEngineAdvanceImplementPersistsSkipReviewFanoutWithoutPR(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job-no-pr",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-7",
		PullRequest:            0,
		TaskID:                 "task-7",
		TaskTitle:              "Workflow Engine",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: true,
		Result:                 &AgentResult{Decision: "implemented", Summary: "pushed the branch, PR opened by hand"},
	})

	if err := engine.AdvanceJob(ctx, "implement-job-no-pr"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	lock, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-7")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if !lock.SkipNativeReviewFanout {
		t.Fatalf("no-PR implement advance did not publish the intent onto the lock: %+v", lock)
	}
}

// Guard the default: a job WITHOUT the intent must never write the flag onto a
// branch lock, so the common path stays byte-identical.
func TestEngineAdvanceImplementWithoutIntentLeavesLockUntouched(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-8", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job-plain",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:      "gitmoot/gitmoot",
		Branch:    "task-8",
		TaskID:    "task-8",
		LeadAgent: "lead",
		Result:    &AgentResult{Decision: "implemented", Summary: "no intent set"},
	})

	if err := engine.AdvanceJob(ctx, "implement-job-plain"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	lock, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-8")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.SkipNativeReviewFanout {
		t.Fatalf("a job with no intent armed the skip flag: %+v", lock)
	}
}
