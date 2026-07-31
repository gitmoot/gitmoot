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

// Guard the default path, and make the guard LOAD-BEARING. An earlier version of
// this test asserted only that the stored boolean was false, which an
// EXTRA_DEFAULT_UPDATE mutant — one that always calls the setter, passing the
// payload's own false — survived, because false-written and never-written look
// identical from the boolean alone.
//
// Seeding the lock with skip=TRUE and advancing a job that carries NO intent
// makes the two distinguishable without depending on timestamp granularity: if
// the default path calls the setter at all it writes false and flips the lock,
// so the mutant is killed by the assertion rather than by a clock.
func TestEngineAdvanceImplementWithoutIntentDoesNotTouchTheLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-8", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	// Pre-arm the lock. A correct default path must leave this alone.
	if err := store.SetBranchLockReviewFanout(ctx, "gitmoot/gitmoot", "task-8", true); err != nil {
		t.Fatalf("SetBranchLockReviewFanout returned error: %v", err)
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
	if !lock.SkipNativeReviewFanout {
		t.Fatalf("the default path WROTE to the lock and cleared a pre-armed flag: %+v", lock)
	}
}

// #1277, the escape g7-review reproduced against the first version of this fix.
// The branch lock cannot rescue a multi-generation tree, because at the moment
// the intent is dropped NO intent-bearing implement job has advanced yet, so
// nothing has published the intent onto the lock.
//
// The path: an intent-bearing coordinator delegates a child; the child's
// continuation is built by a constructor that does NOT set the flag (there are
// nine such constructors and none set it); that continuation then delegates the
// implement leg, which opens the PR and re-arms the fanout.
//
// Enqueue-time inheritance closes it for every constructor at once, which is why
// the continuation request below deliberately leaves SkipNativeReviewFanout
// unset — exactly as engine_continuation_synthesis.go builds it.
func TestIntentSurvivesContinuationThenImplementChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask", "review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	mailbox := Mailbox{Store: store}

	root, err := mailbox.Enqueue(ctx, JobRequest{
		ID:                     "root-coordinator",
		Agent:                  "coordinator",
		Action:                 "ask",
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-9",
		TaskID:                 "task-9",
		Instructions:           "coordinate",
		SkipNativeReviewFanout: true,
	})
	if err != nil {
		t.Fatalf("Enqueue root returned error: %v", err)
	}

	// Generation 2: a continuation constructor that drops the flag.
	continuation, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "continuation-1",
		Agent:        "coordinator",
		Action:       "ask",
		Repo:         "gitmoot/gitmoot",
		Branch:       "task-9",
		TaskID:       "task-9",
		Instructions: "synthesize",
		ParentJobID:  root.ID,
	})
	if err != nil {
		t.Fatalf("Enqueue continuation returned error: %v", err)
	}
	continuationPayload, err := ParseJobPayload(continuation.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(continuation) returned error: %v", err)
	}
	if !continuationPayload.SkipNativeReviewFanout {
		t.Fatalf("continuation dropped the intent: %+v", continuationPayload)
	}

	// Generation 3: the implement child that would open the PR.
	child, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "implement-child",
		Agent:        "impl",
		Action:       "implement",
		Repo:         "gitmoot/gitmoot",
		Branch:       "task-9",
		TaskID:       "task-9",
		Instructions: "do the work",
		ParentJobID:  continuation.ID,
	})
	if err != nil {
		t.Fatalf("Enqueue implement child returned error: %v", err)
	}
	childPayload, err := ParseJobPayload(child.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(child) returned error: %v", err)
	}
	if !childPayload.SkipNativeReviewFanout {
		t.Fatalf("implement child after continuation re-armed native fanout: %+v", childPayload)
	}
}

// A root dispatch with no intent and no parent must stay exactly as built — the
// inheritance lookup must not invent an intent.
func TestEnqueueWithoutParentDoesNotInventIntent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	mailbox := Mailbox{Store: store}

	job, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "plain-root",
		Agent:        "impl",
		Action:       "implement",
		Repo:         "gitmoot/gitmoot",
		Branch:       "task-10",
		TaskID:       "task-10",
		Instructions: "do the work",
	})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.SkipNativeReviewFanout {
		t.Fatalf("a parentless root invented the intent: %+v", payload)
	}
}

// #1277, the third escape g7-review reproduced. dispatchFix builds a PARENTLESS
// implement request carrying neither ParentJobID nor SkipNativeReviewFanout, so
// it slips past the enqueue-chokepoint inheritance (which requires a parent) and
// its fix round re-armed the fanout — on a branch whose lock already read true.
//
// The root cause is an asymmetry between the two PR-open triggers: the daemon
// PR-watcher read the branch lock, the in-process trigger read only the payload.
// This pins the fix from the OUTSIDE — a payload with no intent, on a branch
// whose lock carries it, must not fan out.
func TestInProcessPROpenHonoursTheBranchLockIntent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"reviewer"}
	gate := &fakeMergeGate{decision: MergeDecision{Reason: "ci is pending"}}
	engine.MergeGate = gate

	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-11", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	// The branch was already told, e.g. by the original intent-bearing implement.
	if err := store.SetBranchLockReviewFanout(ctx, "gitmoot/gitmoot", "task-11", true); err != nil {
		t.Fatalf("SetBranchLockReviewFanout returned error: %v", err)
	}
	// The fix-round job, as dispatchFix builds it: no intent, no parent.
	insertCompletedJob(t, store, db.Job{
		ID:    "fix-round-implement",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-11",
		PullRequest:            11,
		HeadSHA:                "head456",
		TaskID:                 "task-11",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: false,
		Result:                 &AgentResult{Decision: "implemented", Summary: "addressed requested changes"},
	})

	if err := engine.AdvanceJob(ctx, "fix-round-implement"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("parentless fix round re-armed native fanout despite the branch lock: %+v", job)
		}
	}
}

// #1277, the guard gap g7-review found by mutation. The branch-lock fallback
// must hold across the engine's OWN identity rewrite, and the previous test only
// covered the homogeneous case where the implement job's agent is also the lock
// owner. A mutant narrowing the fallback to lock.Owner == job.Agent survived all
// twelve fanout/PR-open/dispatchFix tests while being wrong for a shape the
// engine actively supports.
//
// That shape is runtime_session_busy: when a runtime session is occupied the work
// is delegated to a TEMPORARY WORKER, so job.Agent is the worker while the branch
// lock is still owned by OriginalAgent — and it is OriginalAgent that
// leadAgent-resolution restores. The branch carries the intent; the worker's
// payload does not. Ownership must be irrelevant to reading it.
func TestInProcessPROpenHonoursBranchLockAcrossSessionBusyDelegation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "orig", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "temp-worker", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"reviewer"}
	gate := &fakeMergeGate{decision: MergeDecision{Reason: "ci is pending"}}
	engine.MergeGate = gate

	// The lock — and therefore the branch intent — belongs to the ORIGINAL agent.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-14", Owner: "orig"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.SetBranchLockReviewFanout(ctx, "gitmoot/gitmoot", "task-14", true); err != nil {
		t.Fatalf("SetBranchLockReviewFanout returned error: %v", err)
	}
	// The advancing job is the DELEGATED temporary worker, carrying no intent of
	// its own. LeadAgent is empty so the runtime_session_busy restore runs.
	insertCompletedJob(t, store, db.Job{
		ID:    "session-busy-implement",
		Agent: "temp-worker",
		Type:  "implement",
	}, JobPayload{
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-14",
		PullRequest:            14,
		HeadSHA:                "head789",
		TaskID:                 "task-14",
		LeadAgent:              "",
		DelegationReason:       "runtime_session_busy",
		DelegatedAgent:         "temp-worker",
		OriginalAgent:          "orig",
		SkipNativeReviewFanout: false,
		Result:                 &AgentResult{Decision: "implemented", Summary: "delegated worker pushed the branch"},
	})

	if err := engine.AdvanceJob(ctx, "session-busy-implement"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("temporary worker re-armed fanout despite the original owner's branch intent: %+v", job)
		}
	}
}

// #1277 x #1236, the cross-product g7-review found by mutation — and the
// highest-stakes cell in this whole change, because the failure mode is a MERGE
// GATE RUN rather than a spurious review.
//
// The two fixes install two different fail-closed mechanisms in the same
// function, and this is where they meet. When NO reviewer roster is configured,
// HandlePullRequestOpened treats the PR as "no native review discipline" and runs
// the MERGE GATE. The branch intent is what must return through the baseline
// BEFORE that arm is reached. So if the lock fallback is ever narrowed to only
// apply when a roster exists, an intent-bearing zero-roster PR stops being
// suppressed and starts being MERGE-GATED — the intent inverts from "do not
// review this" into "consider this for merge unreviewed".
//
// A ROSTER_ONLY_LOCK_FALLBACK mutant survived all nineteen fanout/PR-open/
// implement/dispatchFix/preflight/zero-roster tests, so nothing pinned this.
func TestBranchLockIntentSuppressesTheMergeGateWithNoConfiguredRoster(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	// Deliberately NO configured reviewers — this is the zero-roster arm.
	engine.RequiredReviewers = nil
	gate := &fakeMergeGate{decision: MergeDecision{Merged: true}}
	engine.MergeGate = gate

	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-15", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	// The branch carries the intent; the payload does not.
	if err := store.SetBranchLockReviewFanout(ctx, "gitmoot/gitmoot", "task-15", true); err != nil {
		t.Fatalf("SetBranchLockReviewFanout returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "zero-roster-implement",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-15",
		PullRequest:            15,
		HeadSHA:                "head015",
		TaskID:                 "task-15",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: false,
		Result:                 &AgentResult{Decision: "implemented", Summary: "opened PR on an intent-bearing branch"},
	})

	// Assert on the gate BEFORE the advance error: when the intent is dropped the
	// gate runs and its decision can itself error, which would otherwise mask the
	// real defect behind a generic "merge gate rejected action". This regression
	// has to name what went wrong, not merely go red.
	advanceErr := engine.AdvanceJob(ctx, "zero-roster-implement")
	if len(gate.requests) != 0 {
		t.Fatalf("branch intent was dropped and let a zero-roster PR reach the merge gate: %+v", gate.requests)
	}
	if advanceErr != nil {
		t.Fatalf("AdvanceJob returned error: %v", advanceErr)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("expected no review jobs, found %+v", job)
		}
	}
}
