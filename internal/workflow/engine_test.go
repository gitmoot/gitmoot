package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

func TestEngineAdvanceJobDispatchesDelegations(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "audit", Type: "ask"}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-005",
		PullRequest: 5,
		TaskID:      "task-5",
		TaskTitle:   "Parent",
		Sender:      "audit",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "del-1", Agent: "helper", Action: "review", Prompt: "review this"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	child := mustJob(t, store, "parent-job/delegation/del-1")
	if child.Agent != "helper" || child.Type != "review" || child.State != string(JobQueued) {
		t.Fatalf("child job = %+v", child)
	}
	if child.ParentJobID != "parent-job" || child.DelegationID != "del-1" || child.DelegationDepth != 1 || child.DelegatedBy != "audit" {
		t.Fatalf("child metadata = %+v", child)
	}

	payload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.ParentJobID != "parent-job" || payload.DelegationID != "del-1" || payload.DelegationDepth != 1 || payload.DelegatedBy != "audit" {
		t.Fatalf("child payload metadata = %+v", payload)
	}
	if payload.Sender != "audit" || payload.Instructions != "review this" {
		t.Fatalf("child payload context = %+v", payload)
	}

	// Idempotent: advancing the same parent again must not duplicate the child.
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("second AdvanceJob returned error: %v", err)
	}
}

func TestEngineEphemeralWorkerCannotDelegate(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// A job that is itself an ephemeral worker (payload.Ephemeral set) returns a
	// delegation. The engine must NOT dispatch it (an ephemeral worker is a leaf
	// that is auto-disposed; a continuation to its synthetic agent would strand).
	insertCompletedJob(t, store, db.Job{ID: "eph-job", Agent: "x-ephemeral-abc", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		Sender:    "x-ephemeral-abc",
		Ephemeral: &EphemeralSpec{Runtime: "codex"},
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "del-1", Agent: "helper", Action: "review", Prompt: "review this"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "eph-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if jobExists(t, store, "eph-job/delegation/del-1") {
		t.Fatalf("ephemeral worker's delegation must not be dispatched")
	}
}

func TestEngineAdvanceJobWritesDelegationArtifacts(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	root := t.TempDir()
	engine.ArtifactRoot = root

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "audit", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "audit",
		Result: &AgentResult{
			Decision:     "approved",
			Summary:      "done",
			ArtifactBody: "# Shared brief\n\nDo the work.\n",
			Delegations: []Delegation{
				{ID: "del-1", Agent: "helper", Action: "review", Prompt: "review this", Artifacts: []string{"brief.md"}},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	wantDir := filepath.Join(root, "delegations", "parent-job")
	briefBytes, err := os.ReadFile(filepath.Join(wantDir, "brief.md"))
	if err != nil {
		t.Fatalf("read brief.md: %v", err)
	}
	if string(briefBytes) != "# Shared brief\n\nDo the work.\n" {
		t.Fatalf("brief.md = %q", string(briefBytes))
	}
	if _, err := os.Stat(filepath.Join(wantDir, "context-manifest.json")); err != nil {
		t.Fatalf("context-manifest.json missing: %v", err)
	}

	child := mustJob(t, store, "parent-job/delegation/del-1")
	payload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.DelegationArtifactDir != wantDir {
		t.Fatalf("child DelegationArtifactDir = %q, want %q", payload.DelegationArtifactDir, wantDir)
	}
}

func TestEngineAdvanceJobSkipsArtifactsWithoutRoot(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "audit", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "audit",
		Result: &AgentResult{
			Decision:     "approved",
			Summary:      "done",
			ArtifactBody: "brief",
			Delegations: []Delegation{
				{ID: "del-1", Agent: "helper", Action: "review", Prompt: "review this", Artifacts: []string{"brief.md"}},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	child := mustJob(t, store, "parent-job/delegation/del-1")
	payload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.DelegationArtifactDir != "" {
		t.Fatalf("child DelegationArtifactDir = %q, want empty when no artifact root", payload.DelegationArtifactDir)
	}
}

func TestDispatchDelegationsTwoImplementSiblingsGetSeparateWorktrees(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "builder-a", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "builder-b", []string{"implement"}, "jerryfane/gitmoot")
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "audit", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "audit",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "d1", Agent: "builder-a", Action: "implement", Prompt: "build one"},
				{ID: "d2", Agent: "builder-b", Action: "implement", Prompt: "build two"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	childOne := mustJob(t, store, "parent-job/delegation/d1")
	childTwo := mustJob(t, store, "parent-job/delegation/d2")
	payloadOne, err := unmarshalPayload(childOne.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(d1) returned error: %v", err)
	}
	payloadTwo, err := unmarshalPayload(childTwo.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(d2) returned error: %v", err)
	}

	wantPathOne := filepath.Join(home, "worktrees", "jerryfane--gitmoot", "delegations", "parent-job", "d1")
	wantPathTwo := filepath.Join(home, "worktrees", "jerryfane--gitmoot", "delegations", "parent-job", "d2")
	if payloadOne.WorktreePath != wantPathOne {
		t.Fatalf("d1 worktree path = %q, want %q", payloadOne.WorktreePath, wantPathOne)
	}
	if payloadTwo.WorktreePath != wantPathTwo {
		t.Fatalf("d2 worktree path = %q, want %q", payloadTwo.WorktreePath, wantPathTwo)
	}
	if payloadOne.WorktreePath == payloadTwo.WorktreePath {
		t.Fatalf("siblings share worktree path %q", payloadOne.WorktreePath)
	}
	if payloadOne.Branch == payloadTwo.Branch {
		t.Fatalf("siblings share branch %q", payloadOne.Branch)
	}
	if payloadOne.Branch == "task-005" || payloadTwo.Branch == "task-005" {
		t.Fatalf("delegation branch not overridden: d1=%q d2=%q", payloadOne.Branch, payloadTwo.Branch)
	}
	if len(manager.calls) != 2 {
		t.Fatalf("AddWorktree calls = %+v, want two", manager.calls)
	}
	// Each child's HeadSHA is cleared so validateTargetCheckout validates the
	// fresh worktree HEAD instead of the stale parent SHA.
	if payloadOne.HeadSHA != "" || payloadTwo.HeadSHA != "" {
		t.Fatalf("delegated implement children must not inherit parent HeadSHA: d1=%q d2=%q", payloadOne.HeadSHA, payloadTwo.HeadSHA)
	}
}

func TestDispatchDelegationsSiblingsSharingWorktreeHintGetDistinctBranches(t *testing.T) {
	// Two sibling implement delegations that share an identical worktree hint must
	// still receive distinct, namespaced branches so the second AddWorktree does
	// not collide on a branch already checked out by the first.
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "builder-a", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "builder-b", []string{"implement"}, "jerryfane/gitmoot")
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "audit", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "audit",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "d1", Agent: "builder-a", Action: "implement", Prompt: "build one", Worktree: "shared"},
				{ID: "d2", Agent: "builder-b", Action: "implement", Prompt: "build two", Worktree: "shared"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	payloadOne, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/d1").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(d1) returned error: %v", err)
	}
	payloadTwo, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/d2").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(d2) returned error: %v", err)
	}
	if payloadOne.Branch == payloadTwo.Branch {
		t.Fatalf("siblings sharing worktree hint share branch %q", payloadOne.Branch)
	}
	if len(manager.calls) != 2 {
		t.Fatalf("AddWorktree calls = %+v, want two distinct allocations", manager.calls)
	}
	if manager.calls[0].branch == manager.calls[1].branch {
		t.Fatalf("AddWorktree branches collide: %+v", manager.calls)
	}
}

func TestDispatchDelegationsWithoutWorktreeManagerEmitsSkippedEvent(t *testing.T) {
	// When the engine has no per-delegation worktree manager, an implement
	// delegation falls back to a shared-checkout branch lock; the loss of
	// isolation must be observable via a delegation_worktree_skipped event.
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "builder", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	// No engine.Home / engine.DelegationWorktrees: isolation unavailable.

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "audit", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "audit",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "d1", Agent: "builder", Action: "implement", Prompt: "build one"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	child := mustJob(t, store, "parent-job/delegation/d1")
	payload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	// The fallback child runs in the shared checkout on the parent branch with no
	// per-delegation worktree path.
	if strings.TrimSpace(payload.WorktreePath) != "" {
		t.Fatalf("fallback child unexpectedly got worktree path %q", payload.WorktreePath)
	}
	if payload.Branch != "task-005" {
		t.Fatalf("fallback child branch = %q, want parent branch task-005", payload.Branch)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_worktree_skipped"); got != 1 {
		t.Fatalf("delegation_worktree_skipped event count = %d, want 1", got)
	}
}

func TestEngineDelegationRetryGetsIsolatedWorktreePathAndBranch(t *testing.T) {
	// A retry of a failed implement delegation must allocate a fresh, isolated
	// worktree path and branch (retry-suffixed) so it never collides with the
	// failed original attempt's leftover worktree directory and checked-out branch.
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "builder", []string{"implement"}, "jerryfane/gitmoot")
	home := t.TempDir()
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)
	engine.Home = home
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "build", Agent: "builder", Action: "implement", Prompt: "build it", Retry: 1},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	originalPayload, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/build").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(original) returned error: %v", err)
	}

	// Fail the original attempt and advance the parent so a retry is enqueued.
	completeDelegationChild(t, store, "parent-job/delegation/build", JobFailed, AgentResult{Decision: "failed", Summary: "broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/build"); err != nil {
		t.Fatalf("AdvanceJob(build) returned error: %v", err)
	}

	retry := mustJob(t, store, "parent-job/delegation/build/retry/1")
	retryPayload, err := unmarshalPayload(retry.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(retry) returned error: %v", err)
	}
	if retryPayload.WorktreePath == originalPayload.WorktreePath {
		t.Fatalf("retry reuses original worktree path %q", retryPayload.WorktreePath)
	}
	if retryPayload.Branch == originalPayload.Branch {
		t.Fatalf("retry reuses original branch %q", retryPayload.Branch)
	}
	wantRetryPath := filepath.Join(home, "worktrees", "jerryfane--gitmoot", "delegations", "parent-job", "build", "retry", "1")
	if retryPayload.WorktreePath != wantRetryPath {
		t.Fatalf("retry worktree path = %q, want %q", retryPayload.WorktreePath, wantRetryPath)
	}
	if !strings.HasSuffix(retryPayload.Branch, "-retry-1") {
		t.Fatalf("retry branch = %q, want -retry-1 suffix", retryPayload.Branch)
	}
	// Two distinct git worktrees were added: the original and the isolated retry.
	if len(manager.calls) != 2 {
		t.Fatalf("AddWorktree calls = %+v, want two (original + retry)", manager.calls)
	}
	if manager.calls[0].path == manager.calls[1].path || manager.calls[0].branch == manager.calls[1].branch {
		t.Fatalf("retry allocation collides with original: %+v", manager.calls)
	}
}

func TestEngineHandlePullRequestOpenedDispatchesReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		GoalID:            "goal-1",
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		Sender:            "lead",
		RequiredReviewers: []string{"audit"},
	})

	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	job := mustJob(t, store, "review-audit-task-7-review-1")
	if job.Agent != "audit" || job.Type != "review" || job.State != string(JobQueued) {
		t.Fatalf("review job = %+v", job)
	}
	if !strings.Contains(job.Payload, `"lead_agent":"lead"`) {
		t.Fatalf("payload missing lead agent: %s", job.Payload)
	}
	pr, err := store.GetPullRequest(ctx, "jerryfane/gitmoot", 7)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.HeadBranch != "task-7" || pr.HeadSHA != "" {
		t.Fatalf("pull request baseline = %+v", pr)
	}
}

func TestEngineHandlePullRequestOpenedBlocksOnBranchLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-7", Owner: "other"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	})

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "branch lock") {
		t.Fatalf("error = %v, want branch lock BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineHandlePullRequestOpenedBlocksWhenLeadLacksCapability(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	})

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "lacks") {
		t.Fatalf("error = %v, want capability BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineHandlePullRequestOpenedRunsMergeGateWithoutReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Reason: "ci is pending"}}
	engine.MergeGate = gate

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	})

	var blocked BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "ci is pending" {
		t.Fatalf("error = %v, want merge gate BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
	if len(gate.requests) != 1 || gate.requests[0].Reviewer != "" || gate.requests[0].PullRequest != 7 || !gate.requests[0].ReviewOptional {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestEngineHandlePullRequestOpenedDoesNotOverwriteNoReviewerMerge(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{
		decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"},
		onEvaluate: func(request MergeRequest) {
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName:   request.Repo,
				Number:         int64(request.PullRequest),
				URL:            "https://github.com/jerryfane/gitmoot/pull/7",
				HeadBranch:     request.Branch,
				BaseBranch:     "main",
				HeadSHA:        request.HeadSHA,
				MergeCommitSHA: "merge123",
				State:          "merged",
			}); err != nil {
				t.Fatalf("UpsertPullRequest returned error: %v", err)
			}
		},
	}
	engine.MergeGate = gate

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "head123",
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	})

	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskMerged)
	pr, err := store.GetPullRequest(ctx, "jerryfane/gitmoot", 7)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.State != "merged" || pr.MergeCommitSHA != "merge123" {
		t.Fatalf("stored pull request = %+v", pr)
	}
}

func TestEngineHandlePullRequestOpenedSkipsReviewFanout(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		HeadSHA:           "head123",
		GoalID:            "goal-1",
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		Sender:            "lead",
		RequiredReviewers: []string{"audit"},
		SkipReviewFanout:  true,
	})

	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskPullRequestOpen)
	// Zero review jobs enqueued despite a required reviewer being present.
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("expected no review jobs, found %+v", job)
		}
	}
	// The native merge gate does not run; an external council gate owns merge
	// authority for skip-native-review-fanout branches.
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
	// Baseline still recorded so the PR advances.
	pr, err := store.GetPullRequest(ctx, "jerryfane/gitmoot", 7)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.HeadBranch != "task-7" || pr.HeadSHA != "head123" {
		t.Fatalf("pull request baseline = %+v", pr)
	}
}

func TestEngineAdvanceImplementPersistsSkipReviewFanoutOntoLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Reason: "ci is pending"}}
	engine.MergeGate = gate
	// A branch lock owned by the lead must exist for the setter to flip.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:                   "jerryfane/gitmoot",
		Branch:                 "task-7",
		PullRequest:            7,
		HeadSHA:                "head123",
		TaskID:                 "task-7",
		TaskTitle:              "Workflow Engine",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: true,
		Result:                 &AgentResult{Decision: "implemented", Summary: "opened PR"},
	})

	err := engine.AdvanceJob(ctx, "implement-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
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
	lock, err := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-7")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if !lock.SkipNativeReviewFanout {
		t.Fatalf("expected lock.SkipNativeReviewFanout = true, got %+v", lock)
	}
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate requests = %+v, want none for skip-native-review-fanout", gate.requests)
	}
	assertTaskState(t, store, "task-7", TaskPullRequestOpen)
}

func TestEngineAdvanceImplementDispatchesReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"audit"}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		GoalID:      "goal-1",
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "implemented", Summary: "opened PR"},
	})

	err := engine.AdvanceJob(ctx, "implement-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	mustJob(t, store, "review-audit-task-7-review-1")
}

func TestEngineAdvanceImplementDefaultsLeadAgent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"audit"}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Result:      &AgentResult{Decision: "implemented", Summary: "opened PR"},
	})

	err := engine.AdvanceJob(ctx, "implement-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	mustJob(t, store, "review-audit-task-7-review-1")
}

func TestEngineAdvanceImplementSkipsPullRequestFlowWhenNoPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-7",
		GoalID:    "goal-1",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		LeadAgent: "lead",
		Result:    &AgentResult{Decision: "implemented", Summary: "done locally"},
	})

	err := engine.AdvanceJob(ctx, "implement-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "implement-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "advance_skipped_no_pr" {
			return
		}
	}
	t.Fatalf("events = %+v, want advance_skipped_no_pr", events)
}

func TestEngineAdvanceImplementDoesNotFinalizeWithoutTaskWorktree(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.ImplementationFinalizer = fakeImplementationFinalizer{err: errors.New("finalizer should not run")}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-7",
		GoalID:    "goal-1",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		LeadAgent: "lead",
		Result:    &AgentResult{Decision: "implemented", Summary: "done locally"},
	})

	err := engine.AdvanceJob(ctx, "implement-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "implement-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "advance_skipped_no_pr" {
			return
		}
	}
	t.Fatalf("events = %+v, want advance_skipped_no_pr", events)
}

func TestEngineAdvanceImplementUsesFinalizerBeforePullRequestFlow(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"audit"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-7",
		RepoFullName: "jerryfane/gitmoot",
		GoalID:       "goal-1",
		Title:        "Workflow Engine",
		State:        string(TaskImplementing),
		Branch:       "task-7",
		WorktreePath: "/tmp/gitmoot-task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	engine.ImplementationFinalizer = fakeImplementationFinalizer{
		payload: JobPayload{
			Repo:        "jerryfane/gitmoot",
			Branch:      "task-7",
			PullRequest: 8,
			HeadSHA:     "head-after-finalizer",
			GoalID:      "goal-1",
			TaskID:      "task-7",
			TaskTitle:   "Workflow Engine",
			LeadAgent:   "lead",
			Result:      &AgentResult{Decision: "implemented", Summary: "done locally"},
		},
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-7",
		GoalID:    "goal-1",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		LeadAgent: "lead",
		Result:    &AgentResult{Decision: "implemented", Summary: "done locally"},
	})

	if err := engine.AdvanceJob(ctx, "implement-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	latest := mustJob(t, store, "implement-job")
	payload, err := unmarshalPayload(latest.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.PullRequest != 8 || payload.HeadSHA != "head-after-finalizer" {
		t.Fatalf("payload PR/head = #%d %q", payload.PullRequest, payload.HeadSHA)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	mustJob(t, store, "review-audit-task-7-review-1")
}

func TestEngineAdvanceExistingPullRequestImplementStillUsesFinalizer(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-7",
		RepoFullName: "jerryfane/gitmoot",
		GoalID:       "goal-1",
		Title:        "Workflow Engine",
		State:        string(TaskImplementing),
		Branch:       "task-7",
		WorktreePath: "/tmp/gitmoot-task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	engine.ImplementationFinalizer = fakeImplementationFinalizer{
		payload: JobPayload{
			Repo:        "jerryfane/gitmoot",
			Branch:      "task-7",
			PullRequest: 7,
			HeadSHA:     "head-after-finalizer",
			GoalID:      "goal-1",
			TaskID:      "task-7",
			TaskTitle:   "Workflow Engine",
			LeadAgent:   "lead",
			Result:      &AgentResult{Decision: "implemented", Summary: "fixed requested changes"},
		},
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "old-head",
		GoalID:      "goal-1",
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "implemented", Summary: "fixed requested changes"},
	})

	if err := engine.AdvanceJob(ctx, "implement-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	latest := mustJob(t, store, "implement-job")
	payload, err := unmarshalPayload(latest.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.HeadSHA != "head-after-finalizer" {
		t.Fatalf("payload HeadSHA = %q, want finalizer head", payload.HeadSHA)
	}
}

func TestEngineRunJobAdvancesCompletedResult(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"audit"}
	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "lead"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"opened PR","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:          "implement-job",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	result, err := engine.RunJob(ctx, "implement-job", agent, adapter)

	if err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}
	if result.Decision != "implemented" {
		t.Fatalf("result = %+v", result)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	mustJob(t, store, "review-audit-task-7-review-1")
}

// A benign post-success advance condition (here: a merge-gate block on the
// freshly-"opened" PR) makes AdvanceJob return after the agent delivery + job
// already succeeded terminally. RunJob must wrap that error in AdvanceError
// (so callers can recover the persisted result) while still returning the
// result; the job stays JobSucceeded.
func TestEngineRunJobWrapsPostSuccessAdvanceError(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	// A pending (not-ready) merge gate blocks the no-reviewers tail -> AdvanceJob
	// returns a BlockedError AFTER the result is stored.
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Reason: "ci is pending"}}
	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "lead"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"opened PR","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:          "implement-job",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "head123",
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	result, err := engine.RunJob(ctx, "implement-job", agent, adapter)

	var advErr AdvanceError
	if !errors.As(err, &advErr) {
		t.Fatalf("RunJob error = %v, want AdvanceError", err)
	}
	// The wrapped cause is still reachable; the result is in hand.
	var blocked BlockedError
	if !errors.As(advErr, &blocked) || blocked.Reason != "ci is pending" {
		t.Fatalf("AdvanceError did not wrap the merge-gate BlockedError: %v", advErr)
	}
	if result.Decision != "implemented" || result.Summary != "opened PR" {
		t.Fatalf("result = %+v, want the delivered implemented result", result)
	}
	// The job is terminally succeeded despite the post-success advance error.
	job := mustJob(t, store, "implement-job")
	if job.State != string(JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
}

// A delivery/run failure (here: a non-existent runtime command) errors BEFORE
// AdvanceJob, so RunJob must return that error raw, NOT wrapped in AdvanceError,
// and the job must not be JobSucceeded.
func TestEngineRunJobDeliveryFailureStaysRaw(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "lead"}
	// A delivery error before the result is stored.
	adapter := &fakeDelivery{err: errors.New("runtime exploded")}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:        "implement-job",
		Agent:     "lead",
		Action:    "implement",
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-7",
		TaskID:    "task-7",
		LeadAgent: "lead",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	_, err := engine.RunJob(ctx, "implement-job", agent, adapter)

	if err == nil {
		t.Fatal("RunJob returned nil error, want delivery failure")
	}
	var advErr AdvanceError
	if errors.As(err, &advErr) {
		t.Fatalf("delivery failure wrapped as AdvanceError: %v", err)
	}
	job := mustJob(t, store, "implement-job")
	if job.State == string(JobSucceeded) {
		t.Fatalf("job state = %q, want non-succeeded after delivery failure", job.State)
	}
}

func TestEngineRunJobAllowsDelegatedImplementWithOriginalBranchLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	payload, err := marshalPayload(JobPayload{
		Repo:             "jerryfane/gitmoot",
		Branch:           "task-7",
		PullRequest:      7,
		HeadSHA:          "head123",
		TaskID:           "task-7",
		Reviewers:        []string{"audit"},
		OriginalAgent:    "lead",
		DelegatedAgent:   "lead-temp-job-1",
		DelegationReason: "runtime_session_busy",
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "lead-temp-job-1", Type: "implement", State: string(JobQueued), Payload: payload}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	engine := testEngine(store)
	agent := runtime.Agent{Name: "lead-temp-job-1", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "lead"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := engine.RunJob(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}

	job := mustJob(t, store, "job-1")
	if job.State != string(JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	lock, err := store.GetBranchLock(ctx, "jerryfane/gitmoot", "task-7")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.Owner != "lead" {
		t.Fatalf("branch lock owner = %q, want original lead", lock.Owner)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	mustJob(t, store, "review-audit-task-7-review-1")
}

func TestEngineRunJobAllowsMergeBackAskForImplementOnlyOriginal(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	payload, err := marshalPayload(JobPayload{
		Repo:             "jerryfane/gitmoot",
		Branch:           "task-7",
		TaskID:           "task-7",
		OriginalAgent:    "lead",
		DelegatedAgent:   "lead-temp-job-1",
		DelegationReason: "temp_worker_merge_back",
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "job-merge-back", Agent: "lead", Type: "ask", State: string(JobQueued), Payload: payload}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	engine := testEngine(store)
	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "lead"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"ack","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := engine.RunJob(ctx, "job-merge-back", agent, adapter); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}

	job := mustJob(t, store, "job-merge-back")
	if job.State != string(JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
}

func TestEngineRunJobPreflightsPolicyBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-7", Owner: "other"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	engine := testEngine(store)
	agent := runtime.Agent{Name: "lead", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "lead"}
	delivered := false
	adapter := &fakeDelivery{
		outputs: []string{
			`{"gitmoot_result":{"decision":"implemented","summary":"opened PR","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
		onDeliver: func() {
			delivered = true
		},
	}
	if _, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{
		ID:          "implement-job",
		Agent:       "lead",
		Action:      "implement",
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	_, err := engine.RunJob(ctx, "implement-job", agent, adapter)

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "branch lock") {
		t.Fatalf("error = %v, want branch lock BlockedError", err)
	}
	if delivered {
		t.Fatal("adapter delivered job before branch-lock preflight")
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineAdvanceReviewChangesRequestedDispatchesFix(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	job := mustJob(t, store, "implement-lead-task-7")
	if !strings.Contains(job.Payload, "fix edge case") {
		t.Fatalf("fix job payload = %s", job.Payload)
	}
}

func TestEngineAdvanceReviewSkipsPullRequestFlowWhenNoPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	// A non-nil merge gate that fails the test if it is ever evaluated: a PR-less
	// approved review must not route into the merge gate (the daemon wires a real
	// gate, so this mirrors that configuration).
	engine.MergeGate = &fakeMergeGate{onEvaluate: func(MergeRequest) {
		t.Fatalf("merge gate evaluated for a PR-less review; want skip")
	}}
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		// PullRequest=0 / Branch="" as a review heartbeat enqueues it.
		Result: &AgentResult{Decision: "changes_requested", Summary: "review stands as a comment"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	// No fix job is dispatched from a context-free review.
	if _, err := store.GetJob(ctx, "implement-lead-task-7"); err == nil {
		t.Fatalf("a PR-less review dispatched a fix job; want none")
	}
	events, err := store.ListJobEvents(ctx, "review-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "advance_skipped_no_pr" {
			return
		}
	}
	t.Fatalf("events = %+v, want advance_skipped_no_pr", events)
}

func TestEngineAdvanceReviewApprovedSkipsMergeGateWhenNoPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{onEvaluate: func(MergeRequest) {
		t.Fatalf("merge gate evaluated for a PR-less review; want skip")
	}}
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		Result:    &AgentResult{Decision: "approved", Summary: "looks good"},
	})

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "review-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "advance_skipped_no_pr" {
			return
		}
	}
	t.Fatalf("events = %+v, want advance_skipped_no_pr", events)
}

func TestEngineAdvanceReviewChangesRequestedReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("first AdvanceJob returned error: %v", err)
	}
	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("second AdvanceJob returned error: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	implementJobs := 0
	for _, job := range jobs {
		if job.ID == "implement-lead-task-7" {
			implementJobs++
		}
	}
	if implementJobs != 1 {
		t.Fatalf("implement job count = %d, want 1", implementJobs)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
}

func TestEngineAdvanceReviewChangesRequestedUsesBranchLockLead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "jerryfane/gitmoot", Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "manual-review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix manual review"},
	})

	err := engine.AdvanceJob(ctx, "manual-review-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	job := mustJob(t, store, "implement-lead-task-7")
	if !strings.Contains(job.Payload, "fix manual review") {
		t.Fatalf("fix job payload = %s", job.Payload)
	}
}

func TestEngineAdvanceReviewApprovalRunsMergeGate(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if len(gate.requests) != 1 || gate.requests[0].Reviewer != "audit" || gate.requests[0].PullRequest != 7 || gate.requests[0].ReviewOptional {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestEngineAdvanceDelegatedReviewApprovalUsesOriginalReviewer(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit-temp-job-a",
		Type:  "review",
	}, JobPayload{
		Repo:             "jerryfane/gitmoot",
		Branch:           "task-7",
		PullRequest:      7,
		TaskID:           "task-7",
		TaskTitle:        "Workflow Engine",
		Reviewers:        []string{"audit"},
		OriginalAgent:    "audit",
		DelegatedAgent:   "audit-temp-job-a",
		DelegationReason: "runtime_session_busy",
		Result:           &AgentResult{Decision: "approved", Summary: "ready"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if len(gate.requests) != 1 || gate.requests[0].Reviewer != "audit" || gate.requests[0].PullRequest != 7 {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestEngineAdvanceReviewApprovalCountsPriorDelegatedReviewer(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	reviewers := []string{"audit", "security"}
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review",
		Agent: "audit-temp-job-a",
		Type:  "review",
	}, JobPayload{
		Repo:             "jerryfane/gitmoot",
		Branch:           "task-7",
		PullRequest:      7,
		TaskID:           "task-7",
		TaskTitle:        "Workflow Engine",
		Reviewers:        reviewers,
		OriginalAgent:    "audit",
		DelegatedAgent:   "audit-temp-job-a",
		DelegationReason: "runtime_session_busy",
		Result:           &AgentResult{Decision: "approved", Summary: "audit ready"},
	})
	insertCompletedJob(t, store, db.Job{
		ID:    "security-review",
		Agent: "security",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		Result:      &AgentResult{Decision: "approved", Summary: "security ready"},
	})

	err := engine.AdvanceJob(ctx, "security-review")

	if err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if len(gate.requests) != 1 || gate.requests[0].Reviewer != "security" {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestEngineAdvanceReviewApprovalWaitsForAllRequiredReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	reviewers := []string{"audit", "security"}
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		Result:      &AgentResult{Decision: "approved", Summary: "audit ready"},
	})

	if err := engine.AdvanceJob(ctx, "audit-review"); err != nil {
		t.Fatalf("first AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran before all approvals: %+v", gate.requests)
	}

	insertCompletedJob(t, store, db.Job{
		ID:    "security-review",
		Agent: "security",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		Result:      &AgentResult{Decision: "approved", Summary: "security ready"},
	})

	if err := engine.AdvanceJob(ctx, "security-review"); err != nil {
		t.Fatalf("second AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if len(gate.requests) != 1 || gate.requests[0].Reviewer != "security" {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestEngineAdvanceReviewApprovalPreservesChangesRequested(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	reviewers := []string{"audit", "security"}
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Reviewers:   reviewers,
		Result:      &AgentResult{Decision: "changes_requested", Summary: "fix edge case"},
	})
	if err := engine.AdvanceJob(ctx, "audit-review"); err != nil {
		t.Fatalf("changes requested AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)

	insertCompletedJob(t, store, db.Job{
		ID:    "security-review",
		Agent: "security",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Reviewers:   reviewers,
		Result:      &AgentResult{Decision: "approved", Summary: "security ready"},
	})
	if err := engine.AdvanceJob(ctx, "security-review"); err != nil {
		t.Fatalf("approval AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskChangesRequested)
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran despite requested changes: %+v", gate.requests)
	}
}

func TestEngineHandlePullRequestOpenedCreatesNewReviewRoundForUpdates(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	event := PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		RequiredReviewers: []string{"audit"},
	}

	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("first HandlePullRequestOpened returned error: %v", err)
	}
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("second HandlePullRequestOpened returned error: %v", err)
	}

	mustJob(t, store, "review-audit-task-7-review-1")
	mustJob(t, store, "review-audit-task-7-review-2")
}

func TestEngineHandlePullRequestOpenedIsIdempotentAfterDelegatedReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	event := PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		HeadSHA:           "head123",
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		RequiredReviewers: []string{"audit"},
	}

	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("first HandlePullRequestOpened returned error: %v", err)
	}
	job := mustJob(t, store, "review-audit-task-7-review-1")
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	payload.OriginalAgent = "audit"
	payload.DelegatedAgent = "audit-temp-review-1"
	payload.DelegationReason = "runtime_session_busy"
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	delegated, err := store.DelegateQueuedJob(ctx, job.ID, "audit", "audit-temp-review-1", encoded, db.JobEvent{
		JobID:   job.ID,
		Kind:    "temp_worker_delegated",
		Message: "delegated for replay test",
	})
	if err != nil || !delegated {
		t.Fatalf("DelegateQueuedJob returned delegated=%v err=%v", delegated, err)
	}

	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("second HandlePullRequestOpened returned error: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	reviewJobs := 0
	for _, candidate := range jobs {
		if candidate.Type == "review" {
			reviewJobs++
		}
	}
	if reviewJobs != 1 {
		t.Fatalf("review job count = %d, want 1; jobs=%+v", reviewJobs, jobs)
	}
}

func TestEngineHandlePullRequestOpenedPreflightsReviewersBeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		TaskID:            "task-7",
		TaskTitle:         "Workflow Engine",
		LeadAgent:         "lead",
		RequiredReviewers: []string{"audit", "missing"},
	})

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "not subscribed") {
		t.Fatalf("error = %v, want missing reviewer BlockedError", err)
	}
	jobs, listErr := store.ListJobs(ctx)
	if listErr != nil {
		t.Fatalf("ListJobs returned error: %v", listErr)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("review job was enqueued before reviewer preflight completed: %+v", job)
		}
	}
}

func TestEngineAdvanceReviewApprovalIgnoresEarlierReviewRounds(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	reviewers := []string{"audit", "security"}
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-1",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "old approval"},
	})
	insertCompletedJob(t, store, db.Job{
		ID:    "security-review-round-2",
		Agent: "security",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "approved", Summary: "security ready"},
	})

	if err := engine.AdvanceJob(ctx, "security-review-round-2"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran with stale approval: %+v", gate.requests)
	}

	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-2",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "approved", Summary: "audit ready"},
	})
	if err := engine.AdvanceJob(ctx, "audit-review-round-2"); err != nil {
		t.Fatalf("second AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if len(gate.requests) != 1 || gate.requests[0].Reviewer != "audit" {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestEngineAdvanceReviewApprovalIgnoresStaleRoundWhenNewerRoundExists(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	if err := store.UpsertTask(ctx, db.Task{
		ID:     "task-7",
		GoalID: "goal-1",
		Title:  "Workflow Engine",
		State:  string(TaskReviewing),
		Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-1",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "old approval"},
	})
	encoded, err := marshalPayload(JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-2",
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      "audit-review-round-2",
		Agent:   "audit",
		Type:    "review",
		State:   string(JobQueued),
		Payload: encoded,
	}, db.JobEvent{Kind: string(JobQueued), Message: "newer round queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	if err := engine.AdvanceJob(ctx, "audit-review-round-1"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran with stale approval: %+v", gate.requests)
	}
}

func TestEngineAdvanceStaleReviewDoesNotRegressReadyState(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-1",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "old approval"},
	})
	insertCompletedJob(t, store, db.Job{
		ID:    "audit-review-round-2",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "approved", Summary: "new approval"},
	})

	if err := engine.AdvanceJob(ctx, "audit-review-round-2"); err != nil {
		t.Fatalf("newer AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
	if err := engine.AdvanceJob(ctx, "audit-review-round-1"); err != nil {
		t.Fatalf("stale AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)
}

func TestEngineAdvanceReviewApprovalIgnoresOtherRepoTaskID(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	gate := &fakeMergeGate{decision: MergeDecision{Ready: true}}
	engine.MergeGate = gate
	reviewers := []string{"audit", "security"}
	insertCompletedJob(t, store, db.Job{
		ID:    "other-repo-audit-review",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "other/repo",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "other repo ready"},
	})
	insertCompletedJob(t, store, db.Job{
		ID:    "security-review",
		Agent: "security",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Reviewers:   reviewers,
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "security ready"},
	})

	if err := engine.AdvanceJob(ctx, "security-review"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate ran with other repo approval: %+v", gate.requests)
	}
}

func TestEngineAdvanceReviewApprovalBlocksOnMergeGateRejection(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Reason: "checks are pending"}}
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	var blocked BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "checks are pending" {
		t.Fatalf("error = %v, want merge gate BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineAdvanceBlocksOnAgentBlockedResult(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-7",
		TaskID:    "task-7",
		TaskTitle: "Workflow Engine",
		Result:    &AgentResult{Decision: "blocked", Summary: "needs credentials"},
	})

	err := engine.AdvanceJob(ctx, "review-job")

	var blocked BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "needs credentials" {
		t.Fatalf("error = %v, want agent BlockedError", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
}

func TestEngineSetTaskStatePreservesExistingMetadata(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:     "task-7",
		GoalID: "goal-1",
		Title:  "Workflow Engine",
		State:  string(TaskPlanned),
		Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	engine := testEngine(store)

	if err := engine.setTaskState(ctx, taskRef{ID: "task-7"}, TaskReviewing); err != nil {
		t.Fatalf("setTaskState returned error: %v", err)
	}

	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.GoalID != "goal-1" || task.Title != "Workflow Engine" || task.Branch != "task-7" || task.State != string(TaskReviewing) {
		t.Fatalf("task metadata was not preserved: %+v", task)
	}
}

func openEngineStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

func testEngine(store *db.Store) Engine {
	return Engine{
		Store: store,
		JobID: func(request JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
		// Run the detached cross-family review leg SYNCHRONOUSLY in tests so its
		// dispatch + harvest are deterministic; production defaults to a goroutine.
		ReviewSpawner: func(fn func()) { fn() },
		// Likewise run the detached deterministic-checker leg (#485) synchronously so
		// its dispatch + harvest are deterministic in tests.
		CheckerSpawner: func(fn func()) { fn() },
		// Default the #536 physical worktree-process probe to "no live process" so
		// cleanup tests are not influenced by whatever real processes happen to live
		// on the host running the suite. Tests exercising the lease-expiry-boundary
		// gate (finding 1) override this explicitly.
		WorktreeHasLiveProcess: func(string) bool { return false },
	}
}

func seedAgent(t *testing.T, store *db.Store, name string, capabilities []string, repo string) {
	t.Helper()
	// Implement-capable agents need a write policy or the fail-closed dispatch
	// preflight (ensureAgentAllowedWithBranchOwner) BLOCKS their jobs (#452). Pick a
	// writable policy automatically so the ~dozens of implement seeds stay valid
	// without per-call-site churn; non-implement seeds keep the default "auto".
	policy := "auto"
	if runtime.HasImplementCapability(capabilities) {
		policy = runtime.AutonomyPolicyWorkspaceWrite
	}
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           name,
		Role:           "agent",
		Runtime:        runtime.ShellRuntime,
		RuntimeRef:     "printf ok",
		RepoScope:      repo,
		Capabilities:   capabilities,
		AutonomyPolicy: policy,
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
}

func insertCompletedJob(t *testing.T, store *db.Store, job db.Job, payload JobPayload) {
	t.Helper()
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	job.State = string(JobSucceeded)
	job.Payload = encoded
	if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{Kind: string(JobSucceeded), Message: "done"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
}

func assertTaskState(t *testing.T, store *db.Store, taskID string, want TaskState) {
	t.Helper()
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(want) {
		t.Fatalf("task state = %q, want %q", task.State, want)
	}
}

func mustJob(t *testing.T, store *db.Store, jobID string) db.Job {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", jobID, err)
	}
	return job
}

// completeDelegationChild transitions a queued delegation child to a terminal
// state and stamps a Result into its payload, mirroring what Mailbox.Run does
// when a child finishes, so the engine's advanceDelegations can observe it.
func completeDelegationChild(t *testing.T, store *db.Store, childID string, state JobState, result AgentResult) {
	t.Helper()
	ctx := context.Background()
	job, err := store.GetJob(ctx, childID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", childID, err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(%s) returned error: %v", childID, err)
	}
	payload.Result = &result
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload(%s) returned error: %v", childID, err)
	}
	if err := store.UpdateJobPayload(ctx, childID, encoded); err != nil {
		t.Fatalf("UpdateJobPayload(%s) returned error: %v", childID, err)
	}
	if err := store.UpdateJobState(ctx, childID, string(state)); err != nil {
		t.Fatalf("UpdateJobState(%s) returned error: %v", childID, err)
	}
}

func jobExists(t *testing.T, store *db.Store, jobID string) bool {
	t.Helper()
	_, err := store.GetJob(context.Background(), jobID)
	if err == nil {
		return true
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	t.Fatalf("GetJob(%s) returned error: %v", jobID, err)
	return false
}

func TestEngineAdvanceDelegationsGatesOnDeps(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "integ", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
				{ID: "integrate", Agent: "integ", Action: "review", Prompt: "integrate", Deps: []string{"api", "ui"}},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// Only the dependency-free delegations are enqueued initially.
	if !jobExists(t, store, "parent-job/delegation/api") {
		t.Fatalf("api child should be enqueued")
	}
	if !jobExists(t, store, "parent-job/delegation/ui") {
		t.Fatalf("ui child should be enqueued")
	}
	if jobExists(t, store, "parent-job/delegation/integrate") {
		t.Fatalf("integrate child must not be enqueued before deps succeed")
	}

	// First dep succeeds: integrate is still gated on ui.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	if jobExists(t, store, "parent-job/delegation/integrate") {
		t.Fatalf("integrate child must not be enqueued until all deps succeed")
	}

	// Second dep succeeds: integrate is now enqueued with correct metadata.
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobSucceeded, AgentResult{Decision: "approved", Summary: "ui ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/ui"); err != nil {
		t.Fatalf("AdvanceJob(ui) returned error: %v", err)
	}
	integrate := mustJob(t, store, "parent-job/delegation/integrate")
	if integrate.Agent != "integ" || integrate.Type != "review" || integrate.State != string(JobQueued) {
		t.Fatalf("integrate child = %+v", integrate)
	}
	if integrate.ParentJobID != "parent-job" || integrate.DelegationID != "integrate" || integrate.DelegationDepth != 1 {
		t.Fatalf("integrate child metadata = %+v", integrate)
	}
	payload, err := unmarshalPayload(integrate.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(integrate) returned error: %v", err)
	}
	if len(payload.Deps) != 2 || payload.Deps[0] != "api" || payload.Deps[1] != "ui" {
		t.Fatalf("integrate child deps = %v", payload.Deps)
	}
}

func TestEngineAdvanceDelegationsEnqueuesContinuationOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// First child finishes: not all siblings terminal, so no continuation yet.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatalf("continuation must not be enqueued before all siblings finish")
	}

	// Second child finishes: continuation enqueued exactly once.
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobSucceeded, AgentResult{Decision: "approved", Summary: "ui ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/ui"); err != nil {
		t.Fatalf("AdvanceJob(ui) returned error: %v", err)
	}
	continuation := mustJob(t, store, delegationContinuationID("parent-job"))
	if continuation.Agent != "coord" || continuation.Type != "ask" || continuation.ParentJobID != "parent-job" {
		t.Fatalf("continuation job = %+v", continuation)
	}
	if continuation.State != string(JobQueued) {
		t.Fatalf("continuation state = %q", continuation.State)
	}

	// Re-advancing another finished child must not create a duplicate.
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("second AdvanceJob(api) returned error: %v", err)
	}
	children, err := engine.childDelegationJobs(ctx, "parent-job")
	if err != nil {
		t.Fatalf("childDelegationJobs returned error: %v", err)
	}
	if _, ok := children[""]; ok {
		t.Fatalf("continuation should not appear as a delegation child")
	}
	continuationCount := 0
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.ID == delegationContinuationID("parent-job") {
			continuationCount++
		}
	}
	if continuationCount != 1 {
		t.Fatalf("continuation job count = %d, want 1", continuationCount)
	}
}

// TestEngineContinuationCarriesParentModel guards that a per-invocation model
// (e.g. from `orchestrate <agent> --model opus`) is carried into the coordinator's
// synthesis continuation, instead of silently falling back to the agent default.
func TestEngineContinuationCarriesParentModel(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Branch: "task-005",
		Sender: "coord",
		Model:  "opus",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent): %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api): %v", err)
	}
	continuation := mustJob(t, store, delegationContinuationID("parent-job"))
	cp, err := unmarshalPayload(continuation.Payload)
	if err != nil {
		t.Fatalf("unmarshal continuation payload: %v", err)
	}
	if cp.Model != "opus" {
		t.Fatalf("continuation payload Model = %q, want %q (per-invocation model must carry into the synthesis continuation)", cp.Model, "opus")
	}
}

// TestEngineContinuationCarriesRuntimeOverride guards the #531 session-safety
// invariant across delegation generations: a coordinator running under a
// per-job runtime override must enqueue its synthesis continuation WITH the
// override. Dropping it would run the continuation as the DEFAULT agent —
// resuming (and writing into) the agent's default-runtime session the override
// exists to stay off, and passing the override-runtime --model to the default
// runtime's CLI, which rejects it and strands the tree after every child
// already succeeded.
func TestEngineContinuationCarriesRuntimeOverride(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	freshRef, err := runtime.NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:               "jerryfane/gitmoot",
		Branch:             "task-005",
		Sender:             "coord",
		Model:              "claude-opus-4-5",
		RuntimeOverride:    runtime.ClaudeRuntime,
		RuntimeOverrideRef: freshRef,
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent): %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api): %v", err)
	}
	continuation := mustJob(t, store, delegationContinuationID("parent-job"))
	cp, err := unmarshalPayload(continuation.Payload)
	if err != nil {
		t.Fatalf("unmarshal continuation payload: %v", err)
	}
	if cp.RuntimeOverride != runtime.ClaudeRuntime || cp.RuntimeOverrideRef != freshRef {
		t.Fatalf("continuation payload override = %q/%q, want %q/%q (the override must carry into the synthesis continuation)",
			cp.RuntimeOverride, cp.RuntimeOverrideRef, runtime.ClaudeRuntime, freshRef)
	}
	if cp.Model != "claude-opus-4-5" {
		t.Fatalf("continuation payload Model = %q, want the override-runtime model to stay with its override", cp.Model)
	}
}

// TestEngineContinuationInheritsCockpit guards that a coordinator's cockpit
// settings carry into the post-synthesis continuation so the continuation
// renders its pane under the same workspace/session as the rest of the tree.
func TestEngineContinuationInheritsCockpit(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:           "jerryfane/gitmoot",
		Branch:         "task-005",
		Sender:         "coord",
		Cockpit:        true,
		CockpitSession: "room",
		CockpitPaneKey: "seat",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent): %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api): %v", err)
	}
	continuation := mustJob(t, store, delegationContinuationID("parent-job"))
	cp, err := unmarshalPayload(continuation.Payload)
	if err != nil {
		t.Fatalf("unmarshal continuation payload: %v", err)
	}
	if !cp.Cockpit || cp.CockpitSession != "room" || cp.CockpitPaneKey != "seat" {
		t.Fatalf("continuation cockpit fields = (%t, %q, %q), want (true, room, seat)", cp.Cockpit, cp.CockpitSession, cp.CockpitPaneKey)
	}
}

func TestEngineDelegationFailurePolicyBlockParent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "integ", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
				{ID: "integrate", Agent: "integ", Action: "review", Prompt: "integrate", Deps: []string{"api"}},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})
	err := engine.AdvanceJob(ctx, "parent-job/delegation/api")
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("AdvanceJob(api) error = %v, want BlockedError", err)
	}
	assertTaskState(t, store, "task-5", TaskBlocked)
	if jobExists(t, store, "parent-job/delegation/integrate") {
		t.Fatalf("dependent integrate child must not be enqueued after dep failed")
	}
}

func TestEngineDelegationFailurePolicyContinue(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "integ", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "continue"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
				{ID: "integrate", Agent: "integ", Action: "review", Prompt: "integrate", Deps: []string{"api"}},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// api fails under a continue policy: the parent is not blocked and the
	// independent ui sibling keeps running.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskBlocked) {
		t.Fatalf("continue policy must not block the parent task")
	}
	// integrate depends on the failed api and must never enqueue.
	if jobExists(t, store, "parent-job/delegation/integrate") {
		t.Fatalf("integrate depends on failed api and must not enqueue under continue")
	}

	// ui still completes; with api terminal-failed and ui succeeded and
	// integrate gated out, all top-level dels are terminal -> continuation.
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobSucceeded, AgentResult{Decision: "approved", Summary: "ui ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/ui"); err != nil {
		t.Fatalf("AdvanceJob(ui) returned error: %v", err)
	}
	if jobExists(t, store, "parent-job/delegation/integrate") {
		t.Fatalf("integrate must remain gated out after dep failure under continue")
	}
	// Once the continue-gated batch resolves (api failed under continue, ui
	// succeeded, integrate permanently gated), the coordinator continuation must
	// be enqueued so the batch does not stall.
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatalf("continuation must be enqueued once the continue-gated batch resolves")
	}
}

func TestEngineDelegationFailurePolicyEscalate(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "escalate"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// api fails under escalate: a continuation job is enqueued immediately,
	// without waiting for the ui sibling, and the parent is not blocked.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatalf("escalate must enqueue the continuation immediately")
	}
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskBlocked) {
		t.Fatalf("escalate policy must not block the parent task")
	}
}

func TestContinuationPromptInlinesChildResults(t *testing.T) {
	dels := []Delegation{
		{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
		{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
	}
	children := map[string]db.Job{
		"api": {ID: "parent-job/delegation/api", Agent: "api", State: string(JobSucceeded)},
		"ui":  {ID: "parent-job/delegation/ui", Agent: "ui", State: string(JobSucceeded)},
	}
	childPayloads := map[string]JobPayload{
		"api": {Repo: "jerryfane/gitmoot", PullRequest: 12, Result: &AgentResult{Decision: "implemented", Summary: "api built"}},
		"ui":  {Result: &AgentResult{Decision: "approved", Summary: "ui built"}},
	}
	prompt := Engine{}.buildContinuationPrompt("", "", &AgentResult{Delegations: dels}, children, childPayloads, "")
	for _, want := range []string{
		"parent-job/delegation/api",
		"parent-job/delegation/ui",
		"api built",
		"ui built",
		"implemented",
		"approved",
		"https://github.com/jerryfane/gitmoot/pull/12",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("continuation prompt missing %q\n%s", want, prompt)
		}
	}
}

func TestContinuationPromptIncludesPhase(t *testing.T) {
	dels := []Delegation{
		{ID: "api", Agent: "api", Action: "review", Prompt: "build api", Phase: "design"},
		{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
	}
	children := map[string]db.Job{
		"api": {ID: "parent-job/delegation/api", Agent: "api", State: string(JobSucceeded)},
		"ui":  {ID: "parent-job/delegation/ui", Agent: "ui", State: string(JobSucceeded)},
	}
	childPayloads := map[string]JobPayload{
		"api": {Result: &AgentResult{Decision: "implemented", Summary: "api built"}},
		"ui":  {Result: &AgentResult{Decision: "approved", Summary: "ui built"}},
	}
	withPhase := Engine{}.buildContinuationPrompt("", "", &AgentResult{Delegations: dels}, children, childPayloads, "")
	if !strings.Contains(withPhase, "[phase: design]") {
		t.Fatalf("continuation prompt missing phase label\n%s", withPhase)
	}

	// Omitting phase on every delegation leaves the prompt byte-identical to one
	// rendered without any phase plumbing, and emits no phase label.
	noPhaseDels := []Delegation{
		{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
		{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
	}
	noPhase := Engine{}.buildContinuationPrompt("", "", &AgentResult{Delegations: noPhaseDels}, children, childPayloads, "")
	if strings.Contains(noPhase, "phase") {
		t.Fatalf("continuation prompt must not mention phase when none set\n%s", noPhase)
	}

	// Only the api delegation carried a phase; clearing it must produce exactly
	// the no-phase rendering.
	cleared := []Delegation{
		{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
		{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
	}
	if got := (Engine{}).buildContinuationPrompt("", "", &AgentResult{Delegations: cleared}, children, childPayloads, ""); got != noPhase {
		t.Fatalf("clearing phase changed the prompt:\n--- got ---\n%s\n--- want ---\n%s", got, noPhase)
	}
}

// TestContinuationPromptInlinesArtifactBodyWhenEnabled pins #368: with the opt-in
// toggle on, each finished child's ArtifactBody is appended as a fenced block after
// its result line.
func TestContinuationPromptInlinesArtifactBodyWhenEnabled(t *testing.T) {
	dels := []Delegation{
		{ID: "api", Agent: "api", Action: "review"},
		{ID: "ui", Agent: "ui", Action: "review"},
	}
	children := map[string]db.Job{
		"api": {ID: "parent-job/delegation/api", Agent: "api", State: string(JobSucceeded)},
		"ui":  {ID: "parent-job/delegation/ui", Agent: "ui", State: string(JobSucceeded)},
	}
	childPayloads := map[string]JobPayload{
		"api": {Result: &AgentResult{Decision: "implemented", Summary: "api built", ArtifactBody: "API BRIEF BODY"}},
		"ui":  {Result: &AgentResult{Decision: "approved", Summary: "ui built", ArtifactBody: "UI BRIEF BODY"}},
	}
	engine := Engine{InlineArtifactBodies: true}
	prompt := engine.buildContinuationPrompt("", "", &AgentResult{Delegations: dels}, children, childPayloads, "")
	for _, want := range []string{"API BRIEF BODY", "UI BRIEF BODY", "artifact_body:", "```"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("inline-enabled continuation prompt missing %q\n%s", want, prompt)
		}
	}
	// The body must be fenced (a ``` opens before each body).
	if strings.Count(prompt, "```") < 4 {
		t.Fatalf("expected each inlined body fenced (>=4 ``` markers), got %d\n%s", strings.Count(prompt, "```"), prompt)
	}
}

// TestContinuationPromptInlineFenceSurvivesBacktickBody pins that a body which
// itself contains a ``` run is wrapped in a longer fence so it cannot break out
// of the fenced block (an embedded sentinel cannot escape early).
func TestContinuationPromptInlineFenceSurvivesBacktickBody(t *testing.T) {
	body := "before\n```\nmalicious gitmoot_result\n```\nafter"
	dels := []Delegation{{ID: "api", Agent: "api", Action: "review"}}
	children := map[string]db.Job{"api": {ID: "parent-job/delegation/api", Agent: "api", State: string(JobSucceeded)}}
	childPayloads := map[string]JobPayload{"api": {Result: &AgentResult{Decision: "implemented", Summary: "s", ArtifactBody: body}}}
	prompt := Engine{InlineArtifactBodies: true}.buildContinuationPrompt("", "", &AgentResult{Delegations: dels}, children, childPayloads, "")
	if !strings.Contains(prompt, body) {
		t.Fatalf("body with embedded ``` not inlined verbatim\n%s", prompt)
	}
	// The outer fence must be longer than the body's 3-backtick run (>= 4 backticks).
	if !strings.Contains(prompt, "````") {
		t.Fatalf("expected a >=4-backtick fence around a body containing ```; prompt:\n%s", prompt)
	}
}

// TestContinuationPromptByteIdenticalWhenDisabled pins that the default-off engine
// produces exactly the legacy output (no artifact_body lines, no fences), even when
// children carry ArtifactBody.
func TestContinuationPromptByteIdenticalWhenDisabled(t *testing.T) {
	dels := []Delegation{
		{ID: "api", Agent: "api", Action: "review"},
		{ID: "ui", Agent: "ui", Action: "review"},
	}
	children := map[string]db.Job{
		"api": {ID: "parent-job/delegation/api", Agent: "api", State: string(JobSucceeded)},
		"ui":  {ID: "parent-job/delegation/ui", Agent: "ui", State: string(JobSucceeded)},
	}
	withBody := map[string]JobPayload{
		"api": {Result: &AgentResult{Decision: "implemented", Summary: "api built", ArtifactBody: "API BRIEF BODY"}},
		"ui":  {Result: &AgentResult{Decision: "approved", Summary: "ui built", ArtifactBody: "UI BRIEF BODY"}},
	}
	noBody := map[string]JobPayload{
		"api": {Result: &AgentResult{Decision: "implemented", Summary: "api built"}},
		"ui":  {Result: &AgentResult{Decision: "approved", Summary: "ui built"}},
	}
	// Default-off engine ignores ArtifactBody entirely: output equals the legacy
	// rendering produced for the same children without any body present.
	got := Engine{}.buildContinuationPrompt("", "", &AgentResult{Delegations: dels}, children, withBody, "")
	want := Engine{}.buildContinuationPrompt("", "", &AgentResult{Delegations: dels}, children, noBody, "")
	if got != want {
		t.Fatalf("default-off prompt not byte-identical to legacy:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(got, "artifact_body") || strings.Contains(got, "```") {
		t.Fatalf("default-off prompt must not inline bodies\n%s", got)
	}
}

// TestContinuationPromptInlineOverCapTruncatedWithMarker pins per-body truncation:
// a body larger than the per-body cap is cut and a marker pointing at the on-disk
// brief is appended.
func TestContinuationPromptInlineOverCapTruncatedWithMarker(t *testing.T) {
	body := strings.Repeat("x", 100)
	dels := []Delegation{{ID: "api", Agent: "api", Action: "review"}}
	children := map[string]db.Job{
		"api": {ID: "parent-job/delegation/api", Agent: "api", State: string(JobSucceeded)},
	}
	childPayloads := map[string]JobPayload{
		"api": {Result: &AgentResult{Decision: "implemented", ArtifactBody: body}},
	}
	engine := Engine{InlineArtifactBodies: true, MaxInlineArtifactBytes: 10, ArtifactRoot: "/home/.gitmoot"}
	prompt := engine.buildContinuationPrompt("", "", &AgentResult{Delegations: dels}, children, childPayloads, "")
	if !strings.Contains(prompt, strings.Repeat("x", 10)) {
		t.Fatalf("expected the first 10 bytes inlined\n%s", prompt)
	}
	if strings.Contains(prompt, strings.Repeat("x", 11)) {
		t.Fatalf("body not truncated to the per-body cap\n%s", prompt)
	}
	wantMarker := "... (90 bytes truncated; full brief at /home/.gitmoot/delegations/parent-job/brief.md)"
	if !strings.Contains(prompt, wantMarker) {
		t.Fatalf("expected truncation marker %q\n%s", wantMarker, prompt)
	}
}

// TestContinuationPromptInlineRuneSafe pins that truncation at the byte cap does not
// split a multi-byte UTF-8 rune.
func TestContinuationPromptInlineRuneSafe(t *testing.T) {
	// Each "世" is 3 bytes. A cap of 10 lands mid-rune (3*3=9 < 10 < 12); the helper
	// must back up to 9 bytes (three full runes) rather than emit a broken rune.
	body := strings.Repeat("世", 10)
	dels := []Delegation{{ID: "api", Agent: "api", Action: "review"}}
	children := map[string]db.Job{
		"api": {ID: "parent-job/delegation/api", Agent: "api", State: string(JobSucceeded)},
	}
	childPayloads := map[string]JobPayload{
		"api": {Result: &AgentResult{Decision: "implemented", ArtifactBody: body}},
	}
	engine := Engine{InlineArtifactBodies: true, MaxInlineArtifactBytes: 10}
	prompt := engine.buildContinuationPrompt("", "", &AgentResult{Delegations: dels}, children, childPayloads, "")
	if !utf8.ValidString(prompt) {
		t.Fatalf("inlined prompt contains an invalid (split) UTF-8 rune\n%q", prompt)
	}
	if !strings.Contains(prompt, strings.Repeat("世", 3)) {
		t.Fatalf("expected three full runes inlined\n%s", prompt)
	}
	if strings.Contains(prompt, strings.Repeat("世", 4)) {
		t.Fatalf("expected at most three full runes under a 10-byte cap\n%s", prompt)
	}
}

// TestContinuationPromptInlineAggregateCapHonored pins the per-continuation
// aggregate budget across multiple children: once the total budget is spent, later
// children's bodies are not inlined.
func TestContinuationPromptInlineAggregateCapHonored(t *testing.T) {
	big := strings.Repeat("a", maxInlineArtifactTotalBytes)
	dels := []Delegation{
		{ID: "first", Agent: "a", Action: "review"},
		{ID: "second", Agent: "b", Action: "review"},
	}
	children := map[string]db.Job{
		"first":  {ID: "parent-job/delegation/first", Agent: "a", State: string(JobSucceeded)},
		"second": {ID: "parent-job/delegation/second", Agent: "b", State: string(JobSucceeded)},
	}
	childPayloads := map[string]JobPayload{
		"first":  {Result: &AgentResult{Decision: "implemented", ArtifactBody: big}},
		"second": {Result: &AgentResult{Decision: "implemented", ArtifactBody: "SECOND_BODY_MARKER"}},
	}
	// Per-body cap is large enough to admit the whole first body, exhausting the
	// aggregate budget so the second body is dropped entirely.
	engine := Engine{InlineArtifactBodies: true, MaxInlineArtifactBytes: maxInlineArtifactTotalBytes}
	prompt := engine.buildContinuationPrompt("", "", &AgentResult{Delegations: dels}, children, childPayloads, "")
	if strings.Contains(prompt, "SECOND_BODY_MARKER") {
		t.Fatalf("aggregate cap not honored: second body inlined despite exhausted budget")
	}
	if !strings.Contains(prompt, "a") {
		t.Fatalf("expected first body inlined\n")
	}
}

func countJobEvents(t *testing.T, store *db.Store, jobID, kind string) int {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s) returned error: %v", jobID, err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

// jobEventMessage returns the message of the first event of kind on jobID, or ""
// when none exists. Used to attribute a delegation_loop_* event to the structural
// vs result-aware path (both emit the same kind, distinguished by message).
func jobEventMessage(t *testing.T, store *db.Store, jobID, kind string) string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s) returned error: %v", jobID, err)
	}
	for _, event := range events {
		if event.Kind == kind {
			return event.Message
		}
	}
	return ""
}

func TestEngineDelegationTimeoutPlumbedToChildPayload(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "audit", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "audit",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "del-1", Agent: "helper", Action: "review", Prompt: "review this", Timeout: "30s"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	child := mustJob(t, store, "parent-job/delegation/del-1")
	payload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.JobTimeout != "30s" {
		t.Fatalf("child JobTimeout = %q, want %q", payload.JobTimeout, "30s")
	}
}

func TestEngineDelegationDefaultTimeoutsAppliedFromPolicy(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "conductor", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "planner", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "reviewer", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.DelegationTimeoutDefaults = DelegationTimeoutDefaults{
		Default: "45m",
		Plan:    "15m",
		Review:  "20m",
	}

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "conductor", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Branch: "task-005",
		Sender: "conductor",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "plan", Agent: "planner", Action: "ask", Phase: "plan", Prompt: "plan"},
				{ID: "review", Agent: "reviewer", Action: "review", Phase: "review", Prompt: "review"},
				{ID: "fallback", Agent: "planner", Action: "ask", Prompt: "fallback"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	planPayload, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/plan").Payload)
	if err != nil {
		t.Fatalf("unmarshal plan payload returned error: %v", err)
	}
	if planPayload.JobTimeout != "15m" {
		t.Fatalf("plan JobTimeout = %q, want 15m", planPayload.JobTimeout)
	}
	reviewPayload, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/review").Payload)
	if err != nil {
		t.Fatalf("unmarshal review payload returned error: %v", err)
	}
	if reviewPayload.JobTimeout != "20m" {
		t.Fatalf("review JobTimeout = %q, want 20m", reviewPayload.JobTimeout)
	}
	fallbackPayload, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/fallback").Payload)
	if err != nil {
		t.Fatalf("unmarshal fallback payload returned error: %v", err)
	}
	if fallbackPayload.JobTimeout != "45m" {
		t.Fatalf("fallback JobTimeout = %q, want 45m", fallbackPayload.JobTimeout)
	}
}

func TestEngineDelegationWithoutTimeoutDefaultStaysUnbounded(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "audit", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Branch: "task-005",
		Sender: "audit",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "del-1", Agent: "helper", Action: "review", Prompt: "review this"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	payload, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/del-1").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.JobTimeout != "" {
		t.Fatalf("non-council child JobTimeout = %q, want empty", payload.JobTimeout)
	}
}

func TestEngineDelegationModelPlumbedToChildPayload(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "audit", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "audit",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "del-1", Agent: "helper", Action: "review", Prompt: "review this", Model: "  opus  "},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	child := mustJob(t, store, "parent-job/delegation/del-1")
	payload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Model != "opus" {
		t.Fatalf("child payload Model = %q, want trimmed %q", payload.Model, "opus")
	}
}

func TestEngineDelegationRequestCopiesModel(t *testing.T) {
	engine := Engine{}
	request := engine.delegationRequest(
		db.Job{ID: "parent-job", Agent: "audit"},
		JobPayload{Repo: "jerryfane/gitmoot"},
		Delegation{ID: "del-1", Agent: "helper", Action: "review", Prompt: "go", Model: "opus"},
	)
	if request.Model != "opus" {
		t.Fatalf("request.Model = %q, want %q", request.Model, "opus")
	}
}

func TestEngineDelegationPhasePlumbedToChildPayload(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "audit", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "audit",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "del-1", Agent: "helper", Action: "review", Prompt: "review this", Phase: "  design  "},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	child := mustJob(t, store, "parent-job/delegation/del-1")
	payload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Phase != "design" {
		t.Fatalf("child payload Phase = %q, want trimmed %q", payload.Phase, "design")
	}
}

func TestEngineDelegationRequestCopiesPhase(t *testing.T) {
	engine := Engine{}
	request := engine.delegationRequest(
		db.Job{ID: "parent-job", Agent: "audit"},
		JobPayload{Repo: "jerryfane/gitmoot"},
		Delegation{ID: "del-1", Agent: "helper", Action: "review", Prompt: "go", Phase: "  design  "},
	)
	if request.Phase != "design" {
		t.Fatalf("request.Phase = %q, want trimmed %q", request.Phase, "design")
	}
}

func TestEngineDelegationRequestInheritsCockpit(t *testing.T) {
	engine := Engine{}
	request := engine.delegationRequest(
		db.Job{ID: "parent-job", Agent: "audit"},
		JobPayload{Repo: "jerryfane/gitmoot", Cockpit: true, CockpitSession: "room", CockpitPaneKey: "seat"},
		Delegation{ID: "del-1", Agent: "helper", Action: "review", Prompt: "go"},
	)
	if !request.Cockpit {
		t.Fatalf("request.Cockpit = false, want true (inherited from coordinator)")
	}
	if request.CockpitSession != "room" {
		t.Fatalf("request.CockpitSession = %q, want %q", request.CockpitSession, "room")
	}
	if request.CockpitPaneKey != "seat" {
		t.Fatalf("request.CockpitPaneKey = %q, want %q", request.CockpitPaneKey, "seat")
	}

	// A coordinator that did not opt in produces children with cockpit off.
	off := engine.delegationRequest(
		db.Job{ID: "parent-job", Agent: "audit"},
		JobPayload{Repo: "jerryfane/gitmoot"},
		Delegation{ID: "del-2", Agent: "helper", Action: "review", Prompt: "go"},
	)
	if off.Cockpit {
		t.Fatalf("request.Cockpit = true, want false when coordinator did not opt in")
	}
}

func TestEngineDelegationRequestThreadsEphemeralSpec(t *testing.T) {
	engine := Engine{}
	spec := &EphemeralSpec{Runtime: runtime.CodexRuntime, Model: "gpt-5.4"}
	request := engine.delegationRequest(
		db.Job{ID: "parent-job", Agent: "audit"},
		JobPayload{Repo: "jerryfane/gitmoot"},
		Delegation{ID: "worker", Ephemeral: spec, Action: "implement", Prompt: "hi"},
	)
	if request.Ephemeral != spec {
		t.Fatalf("request.Ephemeral = %+v, want the delegation spec", request.Ephemeral)
	}
	// The synthetic agent name replaces the (empty) delegation agent and carries
	// the TUI filter infix.
	if !strings.Contains(request.Agent, "-ephemeral-") {
		t.Fatalf("request.Agent = %q, want it to contain %q", request.Agent, "-ephemeral-")
	}
	if request.Agent != ephemeralAgentName("worker", "parent-job") {
		t.Fatalf("request.Agent = %q, want the deterministic ephemeral name", request.Agent)
	}

	// A non-ephemeral delegation keeps routing to its named agent unchanged.
	plain := engine.delegationRequest(
		db.Job{ID: "parent-job", Agent: "audit"},
		JobPayload{Repo: "jerryfane/gitmoot"},
		Delegation{ID: "del-1", Agent: "helper", Action: "review", Prompt: "go"},
	)
	if plain.Ephemeral != nil {
		t.Fatalf("non-ephemeral request carried a spec: %+v", plain.Ephemeral)
	}
	if plain.Agent != "helper" {
		t.Fatalf("non-ephemeral request.Agent = %q, want %q", plain.Agent, "helper")
	}
}

func TestEngineDispatchesEphemeralDelegationWithoutRegisteredAgent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	// The coordinator is registered; the ephemeral worker deliberately is NOT, so
	// this exercises the engine's bypass of the registered-agent existence,
	// repo-access, and capability checks for an ephemeral delegation.
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "worker", Ephemeral: &EphemeralSpec{Runtime: runtime.CodexRuntime, Model: "gpt-5.4"}, Action: "review", Prompt: "hi"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	child := mustJob(t, store, "parent-job/delegation/worker")
	if !strings.Contains(child.Agent, "-ephemeral-") {
		t.Fatalf("child agent = %q, want it to contain %q", child.Agent, "-ephemeral-")
	}
	payload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Ephemeral == nil {
		t.Fatalf("child payload missing ephemeral spec: %+v", payload)
	}
	if payload.Ephemeral.Runtime != runtime.CodexRuntime || payload.Ephemeral.Model != "gpt-5.4" {
		t.Fatalf("child payload ephemeral spec = %+v", payload.Ephemeral)
	}
	// The stored payload JSON must carry the ephemeral key so downstream consumers
	// (daemon worker materialization, dashboard) can read it back.
	if !strings.Contains(child.Payload, `"ephemeral"`) {
		t.Fatalf("stored payload missing ephemeral key: %s", child.Payload)
	}
}

func TestEngineDelegationInvalidLifecycleRejectedAtExtraction(t *testing.T) {
	// Each invalid lifecycle control must be rejected when the agent result is
	// extracted, so a malformed delegation never reaches the dispatcher.
	cases := map[string]string{
		"timeout":        `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"d","agent":"a","action":"review","prompt":"go","timeout":"banana"}]}}`,
		"negative retry": `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"d","agent":"a","action":"review","prompt":"go","retry":-1}]}}`,
		"failure_policy": `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"d","agent":"a","action":"review","prompt":"go","failure_policy":"explode"}]}}`,
		"synthesis_rule": `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"d","agent":"a","action":"review","prompt":"go","synthesis_rule":"coinflip"}]}}`,
		"quorum missing": `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"d","agent":"a","action":"review","prompt":"go","synthesis_rule":"quorum"}]}}`,
		"quorum zero":    `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"d","agent":"a","action":"review","prompt":"go","synthesis_rule":"quorum","quorum":0}]}}`,
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ExtractAgentResult(output); err == nil {
				t.Fatalf("ExtractAgentResult accepted invalid %s", name)
			}
		})
	}

	// Valid lifecycle controls pass validation.
	if err := validateDelegationLifecycle(Delegation{ID: "d", Timeout: "30s", Retry: 2, FailurePolicy: "continue", SynthesisRule: "vote"}); err != nil {
		t.Fatalf("validateDelegationLifecycle rejected valid controls: %v", err)
	}

	// synthesis_rule quorum is accepted when quorum > 0 and rejected otherwise.
	if err := validateDelegationLifecycle(Delegation{ID: "d", SynthesisRule: "quorum", Quorum: 2}); err != nil {
		t.Fatalf("validateDelegationLifecycle rejected valid quorum: %v", err)
	}
	if err := validateDelegationLifecycle(Delegation{ID: "d", SynthesisRule: "quorum", Quorum: 0}); err == nil {
		t.Fatal("validateDelegationLifecycle accepted quorum with quorum == 0")
	}
	if err := validateDelegationLifecycle(Delegation{ID: "d", SynthesisRule: "quorum"}); err == nil {
		t.Fatal("validateDelegationLifecycle accepted quorum with missing quorum")
	}
}

func TestEngineDelegationRetryReenqueuesUntilExhausted(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", Retry: 2},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// First failure: retry 1 is enqueued; the parent is not blocked.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	retry1 := mustJob(t, store, "parent-job/delegation/api/retry/1")
	if retry1.State != string(JobQueued) || retry1.DelegationID != "api" {
		t.Fatalf("retry/1 = %+v", retry1)
	}
	retry1Payload, err := unmarshalPayload(retry1.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(retry/1) returned error: %v", err)
	}
	if retry1Payload.RetryCount != 1 {
		t.Fatalf("retry/1 RetryCount = %d, want 1", retry1Payload.RetryCount)
	}
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskBlocked) {
		t.Fatalf("parent must not be blocked while retries remain")
	}

	// Second failure: retry 2 is enqueued.
	completeDelegationChild(t, store, "parent-job/delegation/api/retry/1", JobFailed, AgentResult{Decision: "failed", Summary: "still broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api/retry/1"); err != nil {
		t.Fatalf("AdvanceJob(retry/1) returned error: %v", err)
	}
	retry2 := mustJob(t, store, "parent-job/delegation/api/retry/2")
	retry2Payload, err := unmarshalPayload(retry2.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(retry/2) returned error: %v", err)
	}
	if retry2Payload.RetryCount != 2 {
		t.Fatalf("retry/2 RetryCount = %d, want 2", retry2Payload.RetryCount)
	}

	// Third failure: retry budget exhausted (RetryCount 2 == Retry 2). No retry/3
	// is enqueued and the default block_parent policy blocks the parent.
	completeDelegationChild(t, store, "parent-job/delegation/api/retry/2", JobFailed, AgentResult{Decision: "failed", Summary: "broke again"})
	err = engine.AdvanceJob(ctx, "parent-job/delegation/api/retry/2")
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("AdvanceJob(retry/2) error = %v, want BlockedError after retries exhausted", err)
	}
	if jobExists(t, store, "parent-job/delegation/api/retry/3") {
		t.Fatal("retry/3 must not be enqueued after the retry budget is exhausted")
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_retry"); got != 2 {
		t.Fatalf("delegation_retry event count = %d, want 2", got)
	}
	assertTaskState(t, store, "task-5", TaskBlocked)
}

func TestEngineDelegationFingerprintDedup(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", Fingerprint: "shared-fp"},
				{ID: "dup", Agent: "ui", Action: "review", Prompt: "build api again", Fingerprint: "shared-fp"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	if !jobExists(t, store, "parent-job/delegation/api") {
		t.Fatal("first delegation with the fingerprint must be enqueued")
	}
	if jobExists(t, store, "parent-job/delegation/dup") {
		t.Fatal("second delegation with the same fingerprint must be deduped")
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_deduped"); got != 1 {
		t.Fatalf("delegation_deduped event count = %d, want 1", got)
	}
}

func TestEngineDelegationFingerprintScopedPerParent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	for _, parent := range []string{"parent-a", "parent-b"} {
		insertCompletedJob(t, store, db.Job{ID: parent, Agent: "coord", Type: "ask"}, JobPayload{
			Repo:      "jerryfane/gitmoot",
			Branch:    "task-005",
			TaskID:    "task-5",
			TaskTitle: "Parent",
			Sender:    "coord",
			Result: &AgentResult{
				Decision: "approved",
				Summary:  "done",
				Delegations: []Delegation{
					{ID: "api", Agent: "api", Action: "review", Prompt: "build api", Fingerprint: "shared-fp"},
				},
			},
		})
		if err := engine.AdvanceJob(ctx, parent); err != nil {
			t.Fatalf("AdvanceJob(%s) returned error: %v", parent, err)
		}
	}

	// The same fingerprint under a different parent must NOT be deduped.
	if !jobExists(t, store, "parent-a/delegation/api") {
		t.Fatal("parent-a child must be enqueued")
	}
	if !jobExists(t, store, "parent-b/delegation/api") {
		t.Fatal("parent-b child must be enqueued; fingerprint dedup is scoped per parent")
	}
	if delegationFingerprintKey("parent-a", "shared-fp") == delegationFingerprintKey("parent-b", "shared-fp") {
		t.Fatal("fingerprint key must differ per parent")
	}
}

// TestEngineDelegationDedupedResolvesContinuationAndDependent pins the critical
// liveness fix: a fingerprint-deduped delegation has no child of its own, yet it
// must resolve against its winning sibling so the coordinator continuation is
// enqueued and a dependent of the deduped node still runs once the winner
// succeeds. Before the fix the deduped node was treated as forever-active and the
// continuation never fired.
func TestEngineDelegationDedupedResolvesContinuationAndDependent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "synth", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", Fingerprint: "shared-fp"},
				{ID: "dup", Agent: "ui", Action: "review", Prompt: "build api again", Fingerprint: "shared-fp"},
				{ID: "synth", Agent: "synth", Action: "review", Prompt: "synthesize", Deps: []string{"dup"}},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	// api wins the fingerprint; dup is deduped (no child) and synth is gated.
	if !jobExists(t, store, "parent-job/delegation/api") {
		t.Fatal("api child should be enqueued")
	}
	if jobExists(t, store, "parent-job/delegation/dup") {
		t.Fatal("dup must be deduped and never get a child")
	}
	if jobExists(t, store, "parent-job/delegation/synth") {
		t.Fatal("synth depends on the deduped dup and must wait for the winner")
	}

	// The winning sibling succeeds: the deduped dup resolves against it, so the
	// dependent of the deduped node enqueues and the continuation fires.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	if !jobExists(t, store, "parent-job/delegation/synth") {
		t.Fatal("dependent of the deduped node must enqueue once the winner succeeds")
	}
	// synth has not finished, so the continuation is still gated on it.
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("continuation must wait for the dependent of the deduped node")
	}

	completeDelegationChild(t, store, "parent-job/delegation/synth", JobSucceeded, AgentResult{Decision: "approved", Summary: "synth ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/synth"); err != nil {
		t.Fatalf("AdvanceJob(synth) returned error: %v", err)
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("continuation must be enqueued once the deduped node and its dependent resolve")
	}
}

// TestEngineDelegationDeferredDedupResolvesContinuation covers the narrower
// deferred-dedup ordering: two same-fingerprint delegations are BOTH deferred
// behind different deps, so the loser is deduped lazily inside the same
// advanceDelegations pass that clears its dep. Before re-reading events at the
// recompute points, the just-deduped delegation was invisible to
// allDelegationsResolved and the coordinator continuation stalled forever (no
// child ever re-triggers the parent). This asserts it now resolves in that pass.
func TestEngineDelegationDeferredDedupResolvesContinuation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "g1", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "g2", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "a", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "b", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "g1", Agent: "g1", Action: "review", Prompt: "gate 1"},
				{ID: "g2", Agent: "g2", Action: "review", Prompt: "gate 2"},
				{ID: "a", Agent: "a", Action: "review", Prompt: "shared work", Deps: []string{"g1"}, Fingerprint: "shared-fp"},
				{ID: "b", Agent: "b", Action: "review", Prompt: "shared work dup", Deps: []string{"g2"}, Fingerprint: "shared-fp"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent): %v", err)
	}
	if !jobExists(t, store, "parent-job/delegation/g1") || !jobExists(t, store, "parent-job/delegation/g2") {
		t.Fatal("both dep-free gates should be enqueued")
	}
	if jobExists(t, store, "parent-job/delegation/a") || jobExists(t, store, "parent-job/delegation/b") {
		t.Fatal("a and b are deferred behind their deps and must not enqueue yet")
	}

	// g1 succeeds first: a (the fingerprint winner) enqueues and succeeds.
	completeDelegationChild(t, store, "parent-job/delegation/g1", JobSucceeded, AgentResult{Decision: "approved", Summary: "g1 ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/g1"); err != nil {
		t.Fatalf("AdvanceJob(g1): %v", err)
	}
	if !jobExists(t, store, "parent-job/delegation/a") {
		t.Fatal("a should enqueue once g1 succeeds")
	}
	completeDelegationChild(t, store, "parent-job/delegation/a", JobSucceeded, AgentResult{Decision: "approved", Summary: "a ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/a"); err != nil {
		t.Fatalf("AdvanceJob(a): %v", err)
	}

	// g2 succeeds LAST: b's dep clears and b is deduped against a in this same
	// pass. The continuation must still fire.
	completeDelegationChild(t, store, "parent-job/delegation/g2", JobSucceeded, AgentResult{Decision: "approved", Summary: "g2 ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/g2"); err != nil {
		t.Fatalf("AdvanceJob(g2): %v", err)
	}
	if jobExists(t, store, "parent-job/delegation/b") {
		t.Fatal("b shares a's fingerprint and must be deduped (no child)")
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("continuation must enqueue in the same pass that deferred-dedups b against its winner")
	}
}

// TestEngineDelegationDepthCapStopsDispatch covers the runaway-recursion safety
// net surfaced by the live E2E smoke: a coordinator whose continuation
// re-delegates (e.g. a static shell agent) would otherwise spawn jobs forever
// because the continuation reused the parent's depth. At/over MaxDelegationDepth
// dispatch is refused with a delegation_depth_exceeded event; just under it,
// dispatch still proceeds.
func TestEngineDelegationDepthCapStopsDispatch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	result := func() *AgentResult {
		return &AgentResult{
			Decision:    "approved",
			Summary:     "delegating",
			Delegations: []Delegation{{ID: "w1", Agent: "w", Action: "ask", Prompt: "do work"}},
		}
	}

	// At the cap: dispatch is refused and a delegation_depth_exceeded event fires.
	insertCompletedJob(t, store, db.Job{ID: "deep-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-005", TaskID: "task-5", Sender: "coord",
		DelegationDepth: MaxDelegationDepth, Result: result(),
	})
	if err := engine.AdvanceJob(ctx, "deep-job"); err != nil {
		t.Fatalf("AdvanceJob(deep): %v", err)
	}
	if jobExists(t, store, "deep-job/delegation/w1") {
		t.Fatal("delegation must NOT be dispatched at the depth cap")
	}
	events, err := store.ListJobEvents(ctx, "deep-job")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	capped := false
	for _, ev := range events {
		if ev.Kind == "delegation_depth_exceeded" {
			capped = true
		}
	}
	if !capped {
		t.Fatal("expected a delegation_depth_exceeded event at the cap")
	}

	// Just under the cap: dispatch still proceeds.
	insertCompletedJob(t, store, db.Job{ID: "shallow-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-005", TaskID: "task-5", Sender: "coord",
		DelegationDepth: MaxDelegationDepth - 1, Result: result(),
	})
	if err := engine.AdvanceJob(ctx, "shallow-job"); err != nil {
		t.Fatalf("AdvanceJob(shallow): %v", err)
	}
	if !jobExists(t, store, "shallow-job/delegation/w1") {
		t.Fatal("delegation just under the depth cap must still dispatch")
	}
}

// TestEngineDelegationRootJobIDPropagates pins the lineage scaffolding the loop
// detector relies on: an originating coordinator has no RootJobID, so its own id
// is the root; both its delegation children and its continuation inherit that
// same root so the whole coordination tree shares one originating id.
func TestEngineDelegationRootJobIDPropagates(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "root-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "w1", Agent: "w", Action: "review", Prompt: "do work"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "root-job"); err != nil {
		t.Fatalf("AdvanceJob(root) returned error: %v", err)
	}

	// The child inherits the originating coordinator's id as its root.
	child := mustJob(t, store, "root-job/delegation/w1")
	childPayload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(child) returned error: %v", err)
	}
	if childPayload.RootJobID != "root-job" {
		t.Fatalf("child RootJobID = %q, want %q", childPayload.RootJobID, "root-job")
	}

	// Once the child finishes, the continuation must share the same root.
	completeDelegationChild(t, store, "root-job/delegation/w1", JobSucceeded, AgentResult{Decision: "approved", Summary: "w1 ok"})
	if err := engine.AdvanceJob(ctx, "root-job/delegation/w1"); err != nil {
		t.Fatalf("AdvanceJob(child) returned error: %v", err)
	}
	continuation := mustJob(t, store, delegationContinuationID("root-job"))
	continuationPayload, err := unmarshalPayload(continuation.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(continuation) returned error: %v", err)
	}
	if continuationPayload.RootJobID != "root-job" {
		t.Fatalf("continuation RootJobID = %q, want %q", continuationPayload.RootJobID, "root-job")
	}
}

// TestContinuationPromptIncludesCompletionContract pins the L1 completion
// contract: the continuation prompt must explicitly tell the coordinator to
// finish by returning an EMPTY delegations list when the goal is complete.
func TestContinuationPromptIncludesCompletionContract(t *testing.T) {
	prompt := Engine{}.buildContinuationPrompt(
		"",
		"",
		&AgentResult{Delegations: []Delegation{{ID: "w1", Agent: "w"}}},
		map[string]db.Job{},
		map[string]JobPayload{},
		"",
	)
	// Termination contract unchanged (#418): an empty delegations list finishes,
	// and new delegations are only for a genuine remaining gap.
	if !strings.Contains(prompt, "EMPTY delegations list") {
		t.Fatalf("continuation prompt missing completion contract: %q", prompt)
	}
	if !strings.Contains(prompt, "Only return new delegations if a genuine gap remains") {
		t.Fatalf("continuation prompt missing completion-contract guidance: %q", prompt)
	}
}

// TestEngineDelegationBudgetCapStopsDispatch pins the L3 per-root job budget: a
// coordinator tree that re-delegates wide is halted once it has produced
// MaxDelegationTotalJobs jobs. Dispatch is refused with a
// delegation_budget_exceeded event and no further children are created.
func TestEngineDelegationBudgetCapStopsDispatch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// Seed MaxDelegationTotalJobs jobs already belonging to the root's tree: the
	// originating coordinator itself plus enough children stamped with its root.
	insertCompletedJob(t, store, db.Job{ID: "root-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "w1", Agent: "w", Action: "review", Prompt: "do work"},
			},
		},
	})
	for i := 1; i < MaxDelegationTotalJobs; i++ {
		insertCompletedJob(t, store, db.Job{ID: fmt.Sprintf("root-job/filler/%d", i), Agent: "w", Type: "review"}, JobPayload{
			Repo:      "jerryfane/gitmoot",
			Branch:    "task-005",
			TaskID:    "task-5",
			Sender:    "coord",
			RootJobID: "root-job",
		})
	}

	count, err := engine.countRootDelegationJobs(ctx, "root-job")
	if err != nil {
		t.Fatalf("countRootDelegationJobs returned error: %v", err)
	}
	if count != MaxDelegationTotalJobs {
		t.Fatalf("countRootDelegationJobs = %d, want %d", count, MaxDelegationTotalJobs)
	}

	if err := engine.AdvanceJob(ctx, "root-job"); err != nil {
		t.Fatalf("AdvanceJob(root) returned error: %v", err)
	}

	// At the budget: dispatch is refused and no child is created.
	if jobExists(t, store, "root-job/delegation/w1") {
		t.Fatal("delegation must NOT be dispatched once the per-root budget is reached")
	}
	events, err := store.ListJobEvents(ctx, "root-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	budgeted := false
	for _, ev := range events {
		if ev.Kind == "delegation_budget_exceeded" {
			budgeted = true
		}
	}
	if !budgeted {
		t.Fatal("expected a delegation_budget_exceeded event at the per-root budget")
	}
}

// TestEngineDelegationWidthCapStopsDispatch covers the per-coordinator fan-out
// width cap: the total-jobs budget is checked before a batch is dispatched, so
// it cannot stop one enormous single fan-out; the width cap does.
func TestEngineDelegationWidthCapStopsDispatch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	dels := make([]Delegation, 0, MaxDelegationWidth+1)
	for i := 0; i <= MaxDelegationWidth; i++ {
		dels = append(dels, Delegation{ID: fmt.Sprintf("d%d", i), Agent: "w", Action: "ask", Prompt: "work"})
	}
	insertCompletedJob(t, store, db.Job{ID: "wide-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-005", TaskID: "task-5", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "too wide", Delegations: dels},
	})
	if err := engine.AdvanceJob(ctx, "wide-job"); err != nil {
		t.Fatalf("AdvanceJob(wide): %v", err)
	}
	if jobExists(t, store, "wide-job/delegation/d0") {
		t.Fatal("a delegation set wider than MaxDelegationWidth must not be dispatched")
	}
	events, err := store.ListJobEvents(ctx, "wide-job")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	widened := false
	for _, ev := range events {
		if ev.Kind == "delegation_width_exceeded" {
			widened = true
		}
	}
	if !widened {
		t.Fatal("expected a delegation_width_exceeded event")
	}
}

// TestEngineDelegationEscalateThenBlockParentFoldsIntoContinuation pins the
// contradictory-state fix: once an escalate failure has enqueued the
// continuation, a later block_parent sibling failure must NOT also block the
// shared parent task; the block folds into the already-enqueued continuation.
func TestEngineDelegationEscalateThenBlockParentFoldsIntoContinuation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "escalate"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", FailurePolicy: "block_parent"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// api fails under escalate: the continuation is enqueued immediately.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("escalate must enqueue the continuation immediately")
	}

	// ui then fails under block_parent: because a continuation is already in
	// flight, the parent task must NOT be blocked (that would contradict the
	// continuation). AdvanceJob must not return a BlockedError.
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobFailed, AgentResult{Decision: "failed", Summary: "ui broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/ui"); err != nil {
		var blocked BlockedError
		if errors.As(err, &blocked) {
			t.Fatalf("block_parent must not block the parent after a continuation was enqueued: %v", err)
		}
		t.Fatalf("AdvanceJob(ui) returned error: %v", err)
	}
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskBlocked) {
		t.Fatal("parent task must not be blocked once a continuation has been enqueued")
	}
}

// TestEngineDelegationTransitiveChainGatesAndContinues exercises a 3-level
// transitive dependency chain a -> b(deps a) -> c(deps b): b gates until a
// succeeds, c gates until b succeeds, and the continuation fires only after c
// terminates. This is the most fragile gating path (a deferred dependent that is
// itself the dep of another deferred dependent) and was previously untested.
func TestEngineDelegationTransitiveChainGatesAndContinues(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "wa", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "wb", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "wc", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "a", Agent: "wa", Action: "review", Prompt: "step a"},
				{ID: "b", Agent: "wb", Action: "review", Prompt: "step b", Deps: []string{"a"}},
				{ID: "c", Agent: "wc", Action: "review", Prompt: "step c", Deps: []string{"b"}},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	// Only a is enqueued; b and c are both gated.
	if !jobExists(t, store, "parent-job/delegation/a") {
		t.Fatal("a should be enqueued immediately")
	}
	if jobExists(t, store, "parent-job/delegation/b") {
		t.Fatal("b must gate until a succeeds")
	}
	if jobExists(t, store, "parent-job/delegation/c") {
		t.Fatal("c must gate until b succeeds")
	}

	// a succeeds: b enqueues, c is still gated on b.
	completeDelegationChild(t, store, "parent-job/delegation/a", JobSucceeded, AgentResult{Decision: "approved", Summary: "a ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/a"); err != nil {
		t.Fatalf("AdvanceJob(a) returned error: %v", err)
	}
	if !jobExists(t, store, "parent-job/delegation/b") {
		t.Fatal("b must enqueue once a succeeds")
	}
	if jobExists(t, store, "parent-job/delegation/c") {
		t.Fatal("c must still gate until b succeeds")
	}
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("continuation must not fire while the chain is in flight")
	}

	// b succeeds: c enqueues, continuation still gated on c.
	completeDelegationChild(t, store, "parent-job/delegation/b", JobSucceeded, AgentResult{Decision: "approved", Summary: "b ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/b"); err != nil {
		t.Fatalf("AdvanceJob(b) returned error: %v", err)
	}
	if !jobExists(t, store, "parent-job/delegation/c") {
		t.Fatal("c must enqueue once b succeeds")
	}
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("continuation must not fire until c terminates")
	}

	// c terminates: continuation fires.
	completeDelegationChild(t, store, "parent-job/delegation/c", JobSucceeded, AgentResult{Decision: "approved", Summary: "c ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/c"); err != nil {
		t.Fatalf("AdvanceJob(c) returned error: %v", err)
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("continuation must fire after the full chain terminates")
	}
}

// TestEngineAdvanceDelegationsConcurrentContinuationExactlyOnce drives the real
// concurrency the daemon exposes with --workers>1: both leaf children finish and
// AdvanceJob is called for each in parallel goroutines. Exactly one continuation
// job must exist and neither AdvanceJob may error. Run with -race.
func TestEngineAdvanceDelegationsConcurrentContinuationExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// Both leaf children finish before either is advanced, so both parallel
	// AdvanceJob passes observe an all-terminal batch and race to enqueue.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobSucceeded, AgentResult{Decision: "approved", Summary: "ui ok"})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, childID := range []string{"parent-job/delegation/api", "parent-job/delegation/ui"} {
		wg.Add(1)
		go func(idx int, id string) {
			defer wg.Done()
			errs[idx] = engine.AdvanceJob(ctx, id)
		}(i, childID)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent AdvanceJob[%d] returned error: %v", i, err)
		}
	}

	continuationCount := 0
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.ID == delegationContinuationID("parent-job") {
			continuationCount++
		}
	}
	if continuationCount != 1 {
		t.Fatalf("continuation job count = %d, want exactly 1", continuationCount)
	}
}

func TestEngineDelegationSynthesisRuleVoteBlocksOnFailure(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "continue", SynthesisRule: "vote"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", FailurePolicy: "continue", SynthesisRule: "vote"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// api succeeds, ui fails: the vote is not unanimous, so the continuation is
	// gated and the parent task is blocked instead of being enqueued.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobFailed, AgentResult{Decision: "failed", Summary: "ui broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/ui"); err != nil {
		var blocked BlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("AdvanceJob(ui) error = %v, want BlockedError from vote", err)
		}
	}
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("vote failure must not enqueue the continuation")
	}
	assertTaskState(t, store, "task-5", TaskBlocked)
}

func TestEngineDelegationSynthesisRuleVotePassesWhenAllApproved(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", SynthesisRule: "vote"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", SynthesisRule: "vote"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobSucceeded, AgentResult{Decision: "approved", Summary: "ui ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/ui"); err != nil {
		t.Fatalf("AdvanceJob(ui) returned error: %v", err)
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("a unanimous vote must enqueue the continuation")
	}
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskBlocked) {
		t.Fatal("a unanimous vote must not block the parent")
	}
}

func TestEngineDelegationSynthesisRuleQuorumBlocksWhenUnmet(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "continue", SynthesisRule: "quorum", Quorum: 2},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", FailurePolicy: "continue", SynthesisRule: "quorum", Quorum: 2},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// Only one of two children approves while quorum K=2: the continuation is
	// gated and the parent task is blocked instead of being enqueued.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobFailed, AgentResult{Decision: "failed", Summary: "ui broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/ui"); err != nil {
		var blocked BlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("AdvanceJob(ui) error = %v, want BlockedError from quorum", err)
		}
	}
	if jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("an unmet quorum must not enqueue the continuation")
	}
	assertTaskState(t, store, "task-5", TaskBlocked)
}

func TestEngineDelegationSynthesisRuleQuorumPassesWhenMet(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "continue", SynthesisRule: "quorum", Quorum: 1},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", FailurePolicy: "continue", SynthesisRule: "quorum", Quorum: 1},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// quorum K=1: one approving child meets the threshold even though the other
	// fails, so the continuation is enqueued and the parent is not blocked.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/ui", JobFailed, AgentResult{Decision: "failed", Summary: "ui broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/ui"); err != nil {
		t.Fatalf("AdvanceJob(ui) returned error: %v", err)
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("a met quorum must enqueue the continuation")
	}
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskBlocked) {
		t.Fatal("a met quorum must not block the parent")
	}
}

// TestQuorumThresholdExceedingDelegationCountRejected pins that a quorum K larger
// than the number of delegations (always unsatisfiable → would block forever) is
// rejected at extraction, while K == len (vote-equivalent) is accepted.
func TestQuorumThresholdExceedingDelegationCountRejected(t *testing.T) {
	result := AgentResult{
		Decision: "approved",
		Summary:  "s",
		Delegations: []Delegation{
			{ID: "a", Agent: "a", Action: "ask", Prompt: "p", SynthesisRule: "quorum", Quorum: 3},
			{ID: "b", Agent: "b", Action: "ask", Prompt: "p", SynthesisRule: "quorum", Quorum: 3},
		},
	}
	if err := validateAgentResult(result); err == nil {
		t.Fatal("quorum K=3 with 2 delegations must be rejected (unsatisfiable)")
	}
	result.Delegations[0].Quorum = 2
	result.Delegations[1].Quorum = 2
	if err := validateAgentResult(result); err != nil {
		t.Fatalf("quorum K=2 with 2 delegations (== len) must be valid: %v", err)
	}
}

type fakeImplementationFinalizer struct {
	payload JobPayload
	err     error
}

func (f fakeImplementationFinalizer) FinalizeImplementation(context.Context, db.Job, JobPayload) (JobPayload, error) {
	if f.err != nil {
		return JobPayload{}, f.err
	}
	return f.payload, nil
}

type fakeMergeGate struct {
	decision   MergeDecision
	onEvaluate func(MergeRequest)
	requests   []MergeRequest
}

func (f *fakeMergeGate) Evaluate(_ context.Context, request MergeRequest) (MergeDecision, error) {
	f.requests = append(f.requests, request)
	if f.onEvaluate != nil {
		f.onEvaluate(request)
	}
	return f.decision, nil
}

// TestCanonicalDelegationSetHashStableUnderReorder pins the order-independence and
// change-sensitivity contract the loop detector relies on: reordering the same
// delegation set yields the same hash, while changing any field (prompt, agent,
// or a dep) yields a different one.
func TestCanonicalDelegationSetHashStableUnderReorder(t *testing.T) {
	base := []Delegation{
		{ID: "a", Agent: "wa", Action: "review", Prompt: "step a", Deps: []string{"x", "y"}},
		{ID: "b", Agent: "wb", Action: "review", Prompt: "step b"},
	}
	reordered := []Delegation{
		{ID: "b", Agent: "wb", Action: "review", Prompt: "step b"},
		{ID: "a", Agent: "wa", Action: "review", Prompt: "step a", Deps: []string{"y", "x"}},
	}
	if canonicalDelegationSetHash(base) != canonicalDelegationSetHash(reordered) {
		t.Fatal("hash must be stable under delegation and dep reordering")
	}
	// Whitespace-only prompt differences are normalized away.
	trimmed := []Delegation{
		{ID: "a", Agent: "wa", Action: "review", Prompt: "  step a  ", Deps: []string{"x", "y"}},
		{ID: "b", Agent: "wb", Action: "review", Prompt: "step b"},
	}
	if canonicalDelegationSetHash(base) != canonicalDelegationSetHash(trimmed) {
		t.Fatal("hash must ignore surrounding prompt whitespace")
	}

	promptChanged := []Delegation{
		{ID: "a", Agent: "wa", Action: "review", Prompt: "DIFFERENT", Deps: []string{"x", "y"}},
		{ID: "b", Agent: "wb", Action: "review", Prompt: "step b"},
	}
	agentChanged := []Delegation{
		{ID: "a", Agent: "OTHER", Action: "review", Prompt: "step a", Deps: []string{"x", "y"}},
		{ID: "b", Agent: "wb", Action: "review", Prompt: "step b"},
	}
	depChanged := []Delegation{
		{ID: "a", Agent: "wa", Action: "review", Prompt: "step a", Deps: []string{"x", "z"}},
		{ID: "b", Agent: "wb", Action: "review", Prompt: "step b"},
	}
	baseHash := canonicalDelegationSetHash(base)
	for name, changed := range map[string][]Delegation{
		"prompt": promptChanged,
		"agent":  agentChanged,
		"dep":    depChanged,
	} {
		if canonicalDelegationSetHash(changed) == baseHash {
			t.Fatalf("hash must change when the %s changes", name)
		}
	}
}

// TestCanonicalDelegationSetHashIgnoresPhase pins that phase is metadata for
// loop detection: two delegation sets differing ONLY in phase hash identically,
// exactly like model.
func TestCanonicalDelegationSetHashIgnoresPhase(t *testing.T) {
	base := []Delegation{
		{ID: "a", Agent: "wa", Action: "review", Prompt: "step a"},
		{ID: "b", Agent: "wb", Action: "review", Prompt: "step b"},
	}
	phased := []Delegation{
		{ID: "a", Agent: "wa", Action: "review", Prompt: "step a", Phase: "design"},
		{ID: "b", Agent: "wb", Action: "review", Prompt: "step b", Phase: "implement"},
	}
	if canonicalDelegationSetHash(base) != canonicalDelegationSetHash(phased) {
		t.Fatal("hash must ignore phase (metadata, excluded from loop detection)")
	}
}

// TestAppendDelegationHashWindowKeepsLastThree pins the sliding-window bound: only
// the most recent delegationHashWindowSize hashes are retained.
func TestAppendDelegationHashWindowKeepsLastThree(t *testing.T) {
	var window []string
	for _, h := range []string{"h1", "h2", "h3", "h4"} {
		window = appendDelegationHashWindow(window, h)
	}
	if len(window) != delegationHashWindowSize {
		t.Fatalf("window length = %d, want %d", len(window), delegationHashWindowSize)
	}
	want := []string{"h2", "h3", "h4"}
	if !equalStrings(window, want) {
		t.Fatalf("window = %v, want %v", window, want)
	}
}

// driveStaticGeneration simulates one coordinator continuation generation: it
// stamps the already-enqueued continuation job with a delegation result (the
// same set every time, modeling a static coordinator) and advances it, which is
// exactly what the daemon does when the continuation runs and re-delegates.
func driveStaticGeneration(t *testing.T, store *db.Store, engine Engine, continuationID string, dels []Delegation) error {
	t.Helper()
	completeDelegationChild(t, store, continuationID, JobSucceeded, AgentResult{
		Decision:    "approved",
		Summary:     "still working",
		Delegations: dels,
	})
	return engine.AdvanceJob(context.Background(), continuationID)
}

// TestEngineStaticCoordinatorHaltedByLoopDetection is the core regression: a
// coordinator whose continuation re-issues the SAME delegation set every round is
// stopped by windowed non-progress detection (a warning + corrective nudge, then
// a delegation_loop_detected halt) well before MaxDelegationDepth — not left to
// the blunt depth cap.
func TestEngineStaticCoordinatorHaltedByLoopDetection(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	dels := []Delegation{{ID: "w1", Agent: "w", Action: "review", Prompt: "do work"}}

	// Generation 0: the originating coordinator dispatches the set for real.
	insertCompletedJob(t, store, db.Job{ID: "root-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result:    &AgentResult{Decision: "approved", Summary: "round 0", Delegations: dels},
	})
	if err := engine.AdvanceJob(ctx, "root-job"); err != nil {
		t.Fatalf("AdvanceJob(root) returned error: %v", err)
	}
	if !jobExists(t, store, "root-job/delegation/w1") {
		t.Fatal("generation 0 must dispatch the delegation for real")
	}

	// Child finishes -> continuation (generation 1) is enqueued carrying the
	// window with generation 0's hash.
	completeDelegationChild(t, store, "root-job/delegation/w1", JobSucceeded, AgentResult{Decision: "approved", Summary: "w1 ok"})
	if err := engine.AdvanceJob(ctx, "root-job/delegation/w1"); err != nil {
		t.Fatalf("AdvanceJob(child) returned error: %v", err)
	}
	continuationID := delegationContinuationID("root-job")
	if !jobExists(t, store, continuationID) {
		t.Fatal("a continuation must be enqueued after the child finishes")
	}

	// Generation 1: the continuation re-issues the SAME set. This is the first
	// repeat -> a warning fires and a corrective continuation is enqueued INSTEAD
	// of dispatching. The corrective continuation reuses the continuation id.
	if err := driveStaticGeneration(t, store, engine, continuationID, dels); err != nil {
		t.Fatalf("driveStaticGeneration(gen1) returned error: %v", err)
	}
	if countJobEvents(t, store, continuationID, "delegation_loop_warning") != 1 {
		t.Fatal("first repeat must record exactly one delegation_loop_warning")
	}
	correctiveID := delegationContinuationID(continuationID)
	if !jobExists(t, store, correctiveID) {
		t.Fatal("first repeat must enqueue a corrective continuation instead of dispatching")
	}
	correctivePayload, err := unmarshalPayload(mustJob(t, store, correctiveID).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(corrective) returned error: %v", err)
	}
	if correctivePayload.DelegationRepeatCount != 1 {
		t.Fatalf("corrective DelegationRepeatCount = %d, want 1", correctivePayload.DelegationRepeatCount)
	}
	if !strings.Contains(correctivePayload.Instructions, "EMPTY delegations list") {
		t.Fatalf("corrective prompt missing the change-or-finish nudge: %q", correctivePayload.Instructions)
	}

	// Generation 2: the coordinator repeats AGAIN after the corrective nudge ->
	// delegation_loop_detected halts it with no further dispatch.
	if err := driveStaticGeneration(t, store, engine, correctiveID, dels); err != nil {
		t.Fatalf("driveStaticGeneration(gen2) returned error: %v", err)
	}
	if countJobEvents(t, store, correctiveID, "delegation_loop_detected") != 1 {
		t.Fatal("second repeat after a nudge must record delegation_loop_detected")
	}
	// Graceful finalize (#305 "Later"): loop_detected enqueues ONE terminal finalize
	// continuation (best-effort synthesis) rather than stopping silently. No more
	// delegations are dispatched off the halted generation.
	if jobExists(t, store, correctiveID+"/delegation/"+dels[0].ID) {
		t.Fatal("delegation_loop_detected must not dispatch the repeated delegations")
	}
	finalizeID := delegationContinuationID(correctiveID)
	finalize, err := unmarshalPayload(mustJob(t, store, finalizeID).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(finalize) returned error: %v", err)
	}
	if !finalize.DelegationFinalize {
		t.Fatalf("loop_detected continuation must be a terminal finalize: %+v", finalize)
	}
	// The finalize continuation is itself terminal: even if it returns delegations,
	// they are ignored, so it spawns no children and no further continuation.
	if err := driveStaticGeneration(t, store, engine, finalizeID, dels); err != nil {
		t.Fatalf("driveStaticGeneration(finalize) returned error: %v", err)
	}
	if jobExists(t, store, finalizeID+"/delegation/"+dels[0].ID) {
		t.Fatal("a finalize continuation must not dispatch its delegations")
	}
	if jobExists(t, store, delegationContinuationID(finalizeID)) {
		t.Fatal("a finalize continuation must be terminal (no further continuation)")
	}

	// It stopped well before the blunt depth cap: the deepest job created carries
	// a DelegationDepth far below MaxDelegationDepth.
	if correctivePayload.DelegationDepth >= MaxDelegationDepth {
		t.Fatalf("loop detection must halt well before MaxDelegationDepth=%d (depth=%d)", MaxDelegationDepth, correctivePayload.DelegationDepth)
	}
}

// TestEngineProgressingCoordinatorNotFalselyFlagged pins the false-positive guard:
// a coordinator that issues a DIFFERENT delegation set each continuation AND whose
// children land a genuinely new durable side effect each round keeps dispatching
// for real and is never flagged as a loop — by either the structural fast-path or
// the result-aware non-progress streak (#339).
func TestEngineProgressingCoordinatorNotFalselyFlagged(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// Generation 0: dispatch the first distinct set for real.
	insertCompletedJob(t, store, db.Job{ID: "root-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision:    "approved",
			Summary:     "round 0",
			Delegations: []Delegation{{ID: "w1", Agent: "w", Action: "review", Prompt: "round 0 work"}},
		},
	})
	if err := engine.AdvanceJob(ctx, "root-job"); err != nil {
		t.Fatalf("AdvanceJob(root) returned error: %v", err)
	}

	completeDelegationChild(t, store, "root-job/delegation/w1", JobSucceeded, AgentResult{Decision: "approved", Summary: "w1 ok"})
	if err := engine.AdvanceJob(ctx, "root-job/delegation/w1"); err != nil {
		t.Fatalf("AdvanceJob(child) returned error: %v", err)
	}

	// Walk several generations, each issuing a DISTINCT delegation set. Every
	// generation must dispatch a real child and never warn or halt.
	parentID := "root-job"
	for round := 1; round <= 4; round++ {
		continuationID := delegationContinuationID(parentID)
		if !jobExists(t, store, continuationID) {
			t.Fatalf("round %d: continuation must be enqueued", round)
		}
		delID := fmt.Sprintf("w%d", round+1)
		dels := []Delegation{{ID: delID, Agent: "w", Action: "review", Prompt: fmt.Sprintf("round %d work", round)}}
		if err := driveStaticGeneration(t, store, engine, continuationID, dels); err != nil {
			t.Fatalf("round %d: generation returned error: %v", round, err)
		}
		if countJobEvents(t, store, continuationID, "delegation_loop_warning") != 0 {
			t.Fatalf("round %d: a progressing coordinator must not warn", round)
		}
		if countJobEvents(t, store, continuationID, "delegation_loop_detected") != 0 {
			t.Fatalf("round %d: a progressing coordinator must not be halted", round)
		}
		childID := continuationID + "/delegation/" + delID
		if !jobExists(t, store, childID) {
			t.Fatalf("round %d: a distinct set must dispatch a real child %s", round, childID)
		}
		// Finish the round's child with a genuinely NEW durable side effect (a
		// distinct ChangesMade) so the result-aware progressDigest differs every
		// round and the non-progress streak stays reset — real progress, never
		// flagged.
		completeDelegationChild(t, store, childID, JobSucceeded, AgentResult{
			Decision:    "approved",
			Summary:     "ok",
			ChangesMade: []string{fmt.Sprintf("change %d", round)},
		})
		if err := engine.AdvanceJob(ctx, childID); err != nil {
			t.Fatalf("round %d: AdvanceJob(child) returned error: %v", round, err)
		}
		parentID = continuationID
	}
}

// driveResultAwareGeneration runs one full coordinator continuation generation
// for the result-aware (#339) tests: it stamps the already-enqueued continuation
// job `parentID` with the delegation set `dels`, advances it (dispatching the
// children), then completes each child with the matching result from
// `childResults` (keyed by delegation id) and advances one child so
// advanceDelegations -> maybeEnqueueContinuation runs the non-progress check. It
// returns the id of the next continuation (delegationContinuationID(parentID)),
// which is the same id whether the generation continued normally, tripped a
// corrective continuation, or tripped a finalize continuation.
func driveResultAwareGeneration(t *testing.T, store *db.Store, engine Engine, parentID string, dels []Delegation, childResults map[string]AgentResult) string {
	t.Helper()
	ctx := context.Background()
	// Stamp the continuation job with this generation's delegation set and advance
	// it so the children are dispatched (modeling the coordinator re-delegating).
	completeDelegationChild(t, store, parentID, JobSucceeded, AgentResult{
		Decision:    "approved",
		Summary:     "coordinating",
		Delegations: dels,
	})
	if err := engine.AdvanceJob(ctx, parentID); err != nil {
		t.Fatalf("driveResultAwareGeneration: AdvanceJob(parent %s) returned error: %v", parentID, err)
	}
	// Complete every dispatched child with its chosen result, then advance one so
	// the parent's continuation decision runs once all children are terminal.
	var lastChildID string
	for _, d := range dels {
		childID := parentID + "/delegation/" + d.ID
		if !jobExists(t, store, childID) {
			// The generation was halted before dispatch (e.g. a finalize trip from a
			// prior corrective nudge): no children to complete.
			continue
		}
		result := childResults[d.ID]
		completeDelegationChild(t, store, childID, JobSucceeded, result)
		lastChildID = childID
	}
	if lastChildID != "" {
		if err := engine.AdvanceJob(ctx, lastChildID); err != nil {
			t.Fatalf("driveResultAwareGeneration: AdvanceJob(child %s) returned error: %v", lastChildID, err)
		}
	}
	return delegationContinuationID(parentID)
}

// TestProgressDigestIgnoresDelegationIdentityAndText pins the two properties the
// result-aware loop detector relies on: the digest is independent of the
// delegation labels (so a perturbed set with the same empty results matches) and
// independent of self-reported Summary/Findings text, while ANY new durable side
// effect changes it.
func TestProgressDigestIgnoresDelegationIdentityAndText(t *testing.T) {
	noEffect := func(summary string) *AgentResult {
		return &AgentResult{Decision: "approved", Summary: summary}
	}
	genA := []Delegation{{ID: "w1", Agent: "wa", Action: "review"}}
	payA := map[string]JobPayload{"w1": {Result: noEffect("first wording")}}
	// Perturbed set (different id/agent) + different summary text, same empty side
	// effects: must hash identically.
	genB := []Delegation{{ID: "x9", Agent: "wb", Action: "review"}}
	payB := map[string]JobPayload{"x9": {Result: noEffect("totally different wording")}}
	if progressDigest(genA, payA) != progressDigest(genB, payB) {
		t.Fatal("digest must ignore delegation identity and self-reported text when side effects are unchanged")
	}

	// A new durable side effect (ChangesMade) must change the digest.
	payChanged := map[string]JobPayload{"w1": {Result: &AgentResult{Decision: "approved", ChangesMade: []string{"edited file"}}}}
	if progressDigest(genA, payA) == progressDigest(genA, payChanged) {
		t.Fatal("a new ChangesMade must change the digest")
	}
	// A new PR/HeadSHA must change the digest.
	payPR := map[string]JobPayload{"w1": {PullRequest: 7, HeadSHA: "abc123", Result: noEffect("first wording")}}
	if progressDigest(genA, payA) == progressDigest(genA, payPR) {
		t.Fatal("a new PR/HeadSHA must change the digest")
	}
}

// TestEngineResultAwareNonProgressStreakTripsLadder is the core #339 regression:
// a coordinator that PERTURBS the delegation set every round (evading the
// structural set-hash fast-path) but whose children keep returning NO new durable
// side effect is caught by the result-aware non-progress streak. It trips the SAME
// ladder as the structural check: delegation_loop_warning + a corrective
// continuation, then delegation_loop_detected + a graceful finalize continuation.
func TestEngineResultAwareNonProgressStreakTripsLadder(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// Each round the child returns the same trivial approval with NO durable side
	// effect, regardless of the (perturbed) delegation id.
	noProgress := AgentResult{Decision: "approved", Summary: "still investigating"}
	perturbed := func(round int) ([]Delegation, map[string]AgentResult) {
		id := fmt.Sprintf("w%d", round)
		return []Delegation{{ID: id, Agent: "w", Action: "review", Prompt: fmt.Sprintf("attempt %d", round)}},
			map[string]AgentResult{id: noProgress}
	}

	// Generation 0: the originating coordinator dispatches a real set; its child
	// returns no durable effect, establishing the baseline digest.
	insertCompletedJob(t, store, db.Job{ID: "root-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result:    &AgentResult{Decision: "approved", Summary: "round 0", Delegations: []Delegation{{ID: "w0", Agent: "w", Action: "review", Prompt: "attempt 0"}}},
	})
	if err := engine.AdvanceJob(ctx, "root-job"); err != nil {
		t.Fatalf("AdvanceJob(root) returned error: %v", err)
	}
	completeDelegationChild(t, store, "root-job/delegation/w0", JobSucceeded, noProgress)
	if err := engine.AdvanceJob(ctx, "root-job/delegation/w0"); err != nil {
		t.Fatalf("AdvanceJob(root child) returned error: %v", err)
	}

	// Generation 1 runs the continuation root-job/continuation: its (perturbed-set)
	// children again make no progress, so the digest repeats and the streak climbs
	// to 1. Still a normal continuation, no warning. The set is perturbed each round
	// so the STRUCTURAL fast-path never fires — only the RESULT-aware path can.
	gen1Cont := delegationContinuationID("root-job")
	dels1, res1 := perturbed(1)
	gen2Cont := driveResultAwareGeneration(t, store, engine, gen1Cont, dels1, res1)
	if countJobEvents(t, store, gen1Cont, "delegation_loop_warning") != 0 {
		t.Fatal("generation 1 must not warn: streak only reached 1")
	}
	if countJobEvents(t, store, gen1Cont, "delegation_loop_detected") != 0 {
		t.Fatal("generation 1 must not be halted")
	}
	gen2Payload, err := unmarshalPayload(mustJob(t, store, gen2Cont).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(gen2) returned error: %v", err)
	}
	if gen2Payload.NonProgressStreak != 1 {
		t.Fatalf("gen2 NonProgressStreak = %d, want 1", gen2Payload.NonProgressStreak)
	}

	// Generation 2: streak reaches the threshold (2) -> delegation_loop_warning and a
	// corrective continuation INSTEAD of a normal one. Structural detection never
	// fired (the set hashes differ each round), proving the RESULT-aware path tripped.
	dels2, res2 := perturbed(2)
	gen3Cont := driveResultAwareGeneration(t, store, engine, gen2Cont, dels2, res2)
	if countJobEvents(t, store, gen2Cont, "delegation_loop_warning") != 1 {
		t.Fatal("generation 2 must record exactly one delegation_loop_warning (result-aware trip)")
	}
	// Attribute the trip to the RESULT-aware path, not the structural set-hash path:
	// both emit delegation_loop_warning, distinguished by message.
	if msg := jobEventMessage(t, store, gen2Cont, "delegation_loop_warning"); !strings.Contains(msg, "no new durable side effect") {
		t.Fatalf("warning must come from the result-aware path, got message: %q", msg)
	}
	correctivePayload, err := unmarshalPayload(mustJob(t, store, gen3Cont).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(corrective) returned error: %v", err)
	}
	if correctivePayload.DelegationRepeatCount != 1 {
		t.Fatalf("corrective DelegationRepeatCount = %d, want 1", correctivePayload.DelegationRepeatCount)
	}
	if correctivePayload.NonProgressStreak < 2 {
		t.Fatalf("corrective NonProgressStreak = %d, want >= 2 (carried forward)", correctivePayload.NonProgressStreak)
	}
	if !strings.Contains(correctivePayload.Instructions, "EMPTY delegations list") {
		t.Fatalf("corrective prompt missing the change-or-finish nudge: %q", correctivePayload.Instructions)
	}

	// Generation 3: the coordinator STILL makes no progress after the corrective
	// nudge -> delegation_loop_detected + a terminal finalize continuation.
	dels3, res3 := perturbed(3)
	gen4Cont := driveResultAwareGeneration(t, store, engine, gen3Cont, dels3, res3)
	if countJobEvents(t, store, gen3Cont, "delegation_loop_detected") != 1 {
		t.Fatal("generation 3 must record delegation_loop_detected after the corrective nudge")
	}
	if countJobEvents(t, store, gen3Cont, "delegation_finalize_enqueued") != 1 {
		t.Fatal("delegation_loop_detected must enqueue a graceful finalize continuation")
	}
	finalizePayload, err := unmarshalPayload(mustJob(t, store, gen4Cont).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(finalize) returned error: %v", err)
	}
	if !finalizePayload.DelegationFinalize {
		t.Fatalf("loop_detected continuation must be a terminal finalize: %+v", finalizePayload)
	}
	// It halted well before the blunt depth cap.
	if finalizePayload.DelegationDepth >= MaxDelegationDepth {
		t.Fatalf("result-aware loop detection must halt well before MaxDelegationDepth=%d (depth=%d)", MaxDelegationDepth, finalizePayload.DelegationDepth)
	}
}

// TestEngineResultAwareProgressResetsStreak pins the progress boundary from the
// other side: a coordinator whose children land a genuinely new durable side
// effect each round (a fresh ChangesMade, then a fresh TestsRun, …) is NEVER
// flagged by the result-aware streak, because each new side effect resets the
// streak even though the self-reported summary text repeats verbatim. The
// delegation set is perturbed each round so the STRUCTURAL fast-path stays silent
// and the test isolates the result-aware decision: the only reason it does not
// trip is that the digest changes every round.
func TestEngineResultAwareProgressResetsStreak(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// Each round perturbs the delegation id (silencing the structural set-hash path)
	// and lands a NEW durable side effect, while the Summary text repeats verbatim.
	progressing := func(round int) ([]Delegation, map[string]AgentResult) {
		id := fmt.Sprintf("w%d", round)
		r := AgentResult{Decision: "approved", Summary: "still working"}
		if round%2 == 1 {
			r.ChangesMade = []string{fmt.Sprintf("edit-%d", round)}
		} else {
			r.TestsRun = []string{fmt.Sprintf("go test ./pkg%d", round)}
		}
		return []Delegation{{ID: id, Agent: "w", Action: "review", Prompt: fmt.Sprintf("round %d", round)}},
			map[string]AgentResult{id: r}
	}

	// Generation 0: dispatch the first set for real with a first durable effect.
	insertCompletedJob(t, store, db.Job{ID: "root-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result:    &AgentResult{Decision: "approved", Summary: "still working", Delegations: []Delegation{{ID: "w0", Agent: "w", Action: "review", Prompt: "round 0"}}},
	})
	if err := engine.AdvanceJob(ctx, "root-job"); err != nil {
		t.Fatalf("AdvanceJob(root) returned error: %v", err)
	}
	completeDelegationChild(t, store, "root-job/delegation/w0", JobSucceeded, AgentResult{Decision: "approved", Summary: "still working", ChangesMade: []string{"edit-0"}})
	if err := engine.AdvanceJob(ctx, "root-job/delegation/w0"); err != nil {
		t.Fatalf("AdvanceJob(root child) returned error: %v", err)
	}

	parentID := delegationContinuationID("root-job")
	for round := 1; round <= 4; round++ {
		dels, res := progressing(round)
		next := driveResultAwareGeneration(t, store, engine, parentID, dels, res)
		if countJobEvents(t, store, parentID, "delegation_loop_warning") != 0 {
			t.Fatalf("round %d: a progressing coordinator must not warn", round)
		}
		if countJobEvents(t, store, parentID, "delegation_loop_detected") != 0 {
			t.Fatalf("round %d: a progressing coordinator must not be halted", round)
		}
		nextPayload, err := unmarshalPayload(mustJob(t, store, next).Payload)
		if err != nil {
			t.Fatalf("round %d: unmarshalPayload(next) returned error: %v", round, err)
		}
		if nextPayload.NonProgressStreak != 0 {
			t.Fatalf("round %d: a progressing coordinator must keep NonProgressStreak at 0, got %d", round, nextPayload.NonProgressStreak)
		}
		parentID = next
	}
}

// TestEngineResultAwareStreakIdempotentOnReadvance pins that re-advancing a
// coordinator whose result-aware streak already tripped does not double-fire: the
// once-guards (continuationEnqueued + the finalize guard) make a second
// advanceDelegations a no-op, so the warning/finalize events and the corrective
// continuation are emitted exactly once.
func TestEngineResultAwareStreakIdempotentOnReadvance(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	noProgress := AgentResult{Decision: "approved", Summary: "stuck"}
	perturbed := func(round int) ([]Delegation, map[string]AgentResult) {
		id := fmt.Sprintf("w%d", round)
		return []Delegation{{ID: id, Agent: "w", Action: "review", Prompt: fmt.Sprintf("attempt %d", round)}},
			map[string]AgentResult{id: noProgress}
	}

	insertCompletedJob(t, store, db.Job{ID: "root-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result:    &AgentResult{Decision: "approved", Summary: "round 0", Delegations: []Delegation{{ID: "w0", Agent: "w", Action: "review", Prompt: "attempt 0"}}},
	})
	if err := engine.AdvanceJob(ctx, "root-job"); err != nil {
		t.Fatalf("AdvanceJob(root) returned error: %v", err)
	}
	completeDelegationChild(t, store, "root-job/delegation/w0", JobSucceeded, noProgress)
	if err := engine.AdvanceJob(ctx, "root-job/delegation/w0"); err != nil {
		t.Fatalf("AdvanceJob(root child) returned error: %v", err)
	}

	// Drive to the first trip: generation 1 climbs the streak to 1, generation 2
	// reaches the threshold and the warning + corrective continuation fire on the
	// generation-2 continuation job (gen2Cont).
	gen1Cont := delegationContinuationID("root-job")
	dels1, res1 := perturbed(1)
	gen2Cont := driveResultAwareGeneration(t, store, engine, gen1Cont, dels1, res1)
	dels2, res2 := perturbed(2)
	_ = driveResultAwareGeneration(t, store, engine, gen2Cont, dels2, res2)
	if countJobEvents(t, store, gen2Cont, "delegation_loop_warning") != 1 {
		t.Fatal("warning must fire exactly once on the first result-aware trip")
	}

	// Re-advance the tripped coordinator and its already-completed child several
	// times: the once-guards must keep every count at exactly one.
	for i := 0; i < 3; i++ {
		if err := engine.AdvanceJob(ctx, gen2Cont); err != nil {
			t.Fatalf("re-advance %d AdvanceJob(parent) returned error: %v", i, err)
		}
		if err := engine.AdvanceJob(ctx, gen2Cont+"/delegation/w2"); err != nil {
			t.Fatalf("re-advance %d AdvanceJob(child) returned error: %v", i, err)
		}
	}
	if got := countJobEvents(t, store, gen2Cont, "delegation_loop_warning"); got != 1 {
		t.Fatalf("delegation_loop_warning fired %d times under re-advance, want 1", got)
	}
	if got := countJobEvents(t, store, gen2Cont, "delegation_continuation_enqueued"); got != 1 {
		t.Fatalf("delegation_continuation_enqueued fired %d times under re-advance, want 1", got)
	}
}

// --- #451: actionable delegation preflight errors + corrective continuation ---

// preflightFailureReason returns the message of the single delegation_preflight_failed
// event on jobID, failing the test if absent.
func preflightFailureReason(t *testing.T, store *db.Store, jobID string) string {
	t.Helper()
	msg := jobEventMessage(t, store, jobID, "delegation_preflight_failed")
	if msg == "" {
		t.Fatalf("expected a delegation_preflight_failed event on %s", jobID)
	}
	return msg
}

func TestEngineDelegationPreflightRuntimeNameMixup(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "shipper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// The coordinator names the runtime "claude" instead of a registered agent.
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coordinator", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "coordinator",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "impl", Agent: "claude", Action: "review", Prompt: "do it"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error (want nil, not BlockedError): %v", err)
	}
	if jobExists(t, store, "parent-job/delegation/impl") {
		t.Fatalf("no child must be dispatched for an unroutable delegation set")
	}
	reason := preflightFailureReason(t, store, "parent-job")
	for _, want := range []string{"is a runtime, not a registered agent", "shipper", "ephemeral"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("preflight reason %q missing %q", reason, want)
		}
	}
	// The coordinator is re-invoked via a corrective continuation, not blocked.
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatalf("expected a corrective continuation for the coordinator")
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_continuation_enqueued"); got != 1 {
		t.Fatalf("delegation_continuation_enqueued = %d, want 1", got)
	}
}

func TestEngineDelegationPreflightUnknownAgentNoRuntimeLine(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "shipper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// A typo'd, non-runtime agent name: list available agents, no runtime line.
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coordinator", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "coordinator",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "impl", Agent: "shippr", Action: "review", Prompt: "do it"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	reason := preflightFailureReason(t, store, "parent-job")
	if !strings.Contains(reason, "is not subscribed") || !strings.Contains(reason, "shipper") {
		t.Fatalf("preflight reason %q missing 'is not subscribed' or available agents", reason)
	}
	if strings.Contains(reason, "is a runtime, not a registered agent") {
		t.Fatalf("non-runtime typo must NOT get the runtime-mixup line: %q", reason)
	}
}

func TestEngineDelegationPreflightAgentNamedClaudeNoFalseTrigger(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask"}, "jerryfane/gitmoot")
	// A legitimately-registered, capable agent literally named "claude".
	seedAgent(t, store, "claude", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coordinator", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "coordinator",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "impl", Agent: "claude", Action: "review", Prompt: "do it"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	child := mustJob(t, store, "parent-job/delegation/impl")
	if child.Agent != "claude" || child.State != string(JobQueued) {
		t.Fatalf("a registered agent named 'claude' must dispatch normally: %+v", child)
	}
	if countJobEvents(t, store, "parent-job", "delegation_preflight_failed") != 0 {
		t.Fatalf("a routable agent must not emit a preflight failure")
	}
}

func TestEngineDelegationPreflightNotAllowedOnRepo(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	// shipper exists and is capable but is scoped to a DIFFERENT repo.
	seedAgent(t, store, "shipper", []string{"review"}, "other/repo")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coordinator", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "coordinator",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "impl", Agent: "shipper", Action: "review", Prompt: "do it"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	reason := preflightFailureReason(t, store, "parent-job")
	if !strings.Contains(reason, "is not allowed on") || !strings.Contains(reason, "helper") {
		t.Fatalf("not-allowed reason %q missing 'is not allowed on' or available agents", reason)
	}
	if strings.Contains(reason, "is a runtime, not a registered agent") {
		t.Fatalf("an existing agent must NOT get the runtime-mixup line: %q", reason)
	}
}

func TestEngineDelegationPreflightLacksCapability(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask"}, "jerryfane/gitmoot")
	// shipper is on the repo but cannot do "review".
	seedAgent(t, store, "shipper", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coordinator", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "coordinator",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "impl", Agent: "shipper", Action: "review", Prompt: "do it"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	reason := preflightFailureReason(t, store, "parent-job")
	if !strings.Contains(reason, "lacks") || !strings.Contains(reason, "capability") {
		t.Fatalf("lacks-capability reason %q missing the capability phrasing", reason)
	}
	if !strings.Contains(reason, "ephemeral") {
		t.Fatalf("lacks-capability reason %q must mention the ephemeral option", reason)
	}
}

func TestEngineDelegationPreflightDeferredLeg(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// del-2 is DEFERRED (depends on del-1) and names a runtime; the upfront preflight
	// must catch it and route the whole set through the corrective continuation
	// without dispatching the ready leg del-1.
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coordinator", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "coordinator",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "del-1", Agent: "helper", Action: "review", Prompt: "first"},
				{ID: "del-2", Agent: "claude", Action: "review", Prompt: "second", Deps: []string{"del-1"}},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if jobExists(t, store, "parent-job/delegation/del-1") {
		t.Fatalf("atomic preflight must not dispatch the ready leg when a deferred leg is unroutable")
	}
	if countJobEvents(t, store, "parent-job", "delegation_preflight_failed") != 1 {
		t.Fatalf("the deferred bad-agent leg must trigger exactly one preflight failure")
	}
}

func TestEngineDelegationPreflightIdempotentReadvance(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coordinator", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "coordinator",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "impl", Agent: "claude", Action: "review", Prompt: "do it"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("first AdvanceJob returned error: %v", err)
	}
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("second AdvanceJob returned error: %v", err)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_preflight_failed"); got != 1 {
		t.Fatalf("delegation_preflight_failed fired %d times under re-advance, want 1", got)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_continuation_enqueued"); got != 1 {
		t.Fatalf("delegation_continuation_enqueued fired %d times under re-advance, want 1", got)
	}
}

func TestEngineDelegationPreflightBoundedFinalize(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// A coordinator generation that has already taken a corrective nudge
	// (DelegationRepeatCount >= 1) and is at the streak threshold must finalize
	// gracefully rather than loop, when it again names an unroutable agent.
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coordinator", Type: "ask"}, JobPayload{
		Repo:                  "jerryfane/gitmoot",
		Sender:                "coordinator",
		DelegationRepeatCount: 1,
		NonProgressStreak:     engine.nonProgressStreakThreshold() - 1,
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out again",
			Delegations: []Delegation{
				{ID: "impl", Agent: "claude", Action: "review", Prompt: "do it"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if countJobEvents(t, store, "parent-job", "delegation_finalize_enqueued") != 1 {
		t.Fatalf("a repeated unroutable set after a nudge must finalize gracefully")
	}
	if countJobEvents(t, store, "parent-job", "delegation_preflight_failed") != 1 {
		t.Fatalf("the finalize path must still record the structured preflight failure")
	}
}

// TestEffectiveDelegationBoundsEnvOverride covers #22: the depth/total-jobs bounds
// are env-overridable (so a council coordinator's long continuation chains aren't
// strangled by the default-8 depth), defaulting to the safe consts when unset.
func TestEffectiveDelegationBoundsEnvOverride(t *testing.T) {
	if got := effectiveMaxDelegationDepth(); got != MaxDelegationDepth {
		t.Fatalf("default depth = %d, want %d", got, MaxDelegationDepth)
	}
	if got := effectiveMaxDelegationTotalJobs(); got != MaxDelegationTotalJobs {
		t.Fatalf("default total-jobs = %d, want %d", got, MaxDelegationTotalJobs)
	}
	t.Setenv("GITMOOT_MAX_DELEGATION_DEPTH", "32")
	t.Setenv("GITMOOT_MAX_DELEGATION_TOTAL_JOBS", "128")
	if got := effectiveMaxDelegationDepth(); got != 32 {
		t.Fatalf("override depth = %d, want 32", got)
	}
	if got := effectiveMaxDelegationTotalJobs(); got != 128 {
		t.Fatalf("override total-jobs = %d, want 128", got)
	}
	t.Setenv("GITMOOT_MAX_DELEGATION_DEPTH", "0")
	if got := effectiveMaxDelegationDepth(); got != MaxDelegationDepth {
		t.Fatalf("zero override -> %d, want default %d", got, MaxDelegationDepth)
	}
	t.Setenv("GITMOOT_MAX_DELEGATION_DEPTH", "notanint")
	if got := effectiveMaxDelegationDepth(); got != MaxDelegationDepth {
		t.Fatalf("invalid override -> %d, want default %d", got, MaxDelegationDepth)
	}
}
