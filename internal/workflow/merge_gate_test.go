package workflow

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

func TestPolicyMergeGateMergesPassingPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		WorkflowID:  "release/native-merge",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	if _, err := store.InsertWorkflowNoteWithMeta(ctx,
		db.WorkflowNote{WorkflowID: "release/native-merge", Author: "operator", Body: "ready"},
		db.WorkflowMeta{Status: "ready_to_merge", StatusSet: true}); err != nil {
		t.Fatalf("seed workflow status: %v", err)
	}
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:    9,
			Title:     "Task 9",
			State:     "open",
			URL:       "https://github.com/gitmoot/gitmoot/pull/9",
			HeadRef:   "task-9",
			BaseRef:   "main",
			HeadSHA:   "head123",
			Mergeable: &mergeable,
		},
		status: github.CombinedStatus{
			State: "success",
			Statuses: []github.CommitStatus{
				{Context: gitmootMergeGateContext, State: "failure"},
			},
		},
		checks: []github.PullRequestCheck{
			{Name: gitmootMergeGateContext, Bucket: "fail", State: "FAILURE"},
			{Name: "ci", Bucket: "pass", State: "SUCCESS"},
		},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	git := &fakeMergeGateGit{clean: true}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: git, CheckoutPath: t.TempDir(), DeleteBranch: true}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Reviewer: "audit"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || decision.MergeCommitSHA != "merge123" {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 1 || gh.merges[0].Method != "squash" || gh.merges[0].MatchHeadCommit != "head123" || !gh.merges[0].DeleteBranch {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	// A PR with a passing external check merges through the gate WITHOUT the
	// synthetic gitmoot/ci no-CI stamp (#596: that stamp is only for genuinely
	// CI-less heads, and only after the grace window).
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "success") || hasStatus(gh.statuses, gitmootNoCIContext, "success") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
	if _, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-9"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("branch lock after merge error = %v, want sql.ErrNoRows", err)
	}
	lockEvents, err := store.ListBranchLockEvents(ctx, "gitmoot/gitmoot", "task-9")
	if err != nil {
		t.Fatalf("ListBranchLockEvents returned error: %v", err)
	}
	if len(lockEvents) != 1 || lockEvents[0].Kind != "released" || lockEvents[0].Owner != "lead" {
		t.Fatalf("lock events = %+v", lockEvents)
	}
	pr, err := store.GetPullRequest(ctx, "gitmoot/gitmoot", 9)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.State != "merged" || pr.MergeCommitSHA != "merge123" {
		t.Fatalf("stored pull request = %+v", pr)
	}
	if len(git.updated) != 1 || git.updated[0] != "origin/main" {
		t.Fatalf("updated base calls = %+v", git.updated)
	}
	meta, err := store.GetWorkflowMeta(ctx, "release/native-merge")
	if err != nil || meta.Status != "active" {
		t.Fatalf("workflow meta after native merge = %+v, err=%v", meta, err)
	}
	notes, err := store.ListWorkflowNotes(ctx, "release/native-merge", 0)
	if err != nil {
		t.Fatalf("ListWorkflowNotes: %v", err)
	}
	mergedReceipts := 0
	for _, note := range notes {
		if note.Body == "[auto:pr:9:merged] PR #9 merged" {
			mergedReceipts++
		}
	}
	if mergedReceipts != 1 {
		t.Fatalf("merged receipt count = %d, want 1; notes=%+v", mergedReceipts, notes)
	}
	if inserted, err := RecordPullRequestWorkflowTransition(ctx, store, PullRequestEvent{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 9,
	}, PullRequestJournalMerged); err != nil || inserted {
		t.Fatalf("daemon replay = (inserted=%v, err=%v), want deduplicated no-op", inserted, err)
	}
}

func TestPolicyMergeGateJournalFailureDoesNotChangeMergedDecision(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "native-journal-link", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		WorkflowID:  "release/native-journal-failure",
	})
	if _, err := store.InsertWorkflowNoteWithMeta(ctx,
		db.WorkflowNote{WorkflowID: "release/native-journal-failure", Author: "operator", Body: "ready"},
		db.WorkflowMeta{Status: "ready_to_merge", StatusSet: true}); err != nil {
		t.Fatalf("seed workflow status: %v", err)
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `
CREATE TRIGGER fail_native_merge_workflow_journal
BEFORE INSERT ON workflow_notes
WHEN NEW.author = 'daemon' AND NEW.body LIKE '[auto:pr:%:merged]%'
BEGIN
	SELECT RAISE(ABORT, 'forced workflow journal failure');
END`); err != nil {
		t.Fatalf("create journal failure trigger: %v", err)
	}

	gate := PolicyMergeGate{Store: store}
	decision, err := gate.finishMerged(ctx, MergeRequest{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 9,
	}, github.PullRequest{
		Number: 9, URL: "https://github.com/gitmoot/gitmoot/pull/9",
		HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123",
	}, "merge123")
	if err != nil {
		t.Fatalf("finishMerged returned journal error: %v", err)
	}
	if !decision.Ready || !decision.Merged || decision.MergeCommitSHA != "merge123" || decision.Reason != "merged" {
		t.Fatalf("decision changed by journal failure: %+v", decision)
	}
	pr, err := store.GetPullRequest(ctx, "gitmoot/gitmoot", 9)
	if err != nil || pr.State != "merged" || pr.MergeCommitSHA != "merge123" {
		t.Fatalf("durable merged PR = %+v, err=%v", pr, err)
	}
	meta, err := store.GetWorkflowMeta(ctx, "release/native-journal-failure")
	if err != nil || meta.Status != "ready_to_merge" {
		t.Fatalf("failed journal changed workflow meta = %+v, err=%v", meta, err)
	}
	notes, err := store.ListWorkflowNotes(ctx, "release/native-journal-failure", 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes after forced journal failure = %+v, err=%v", notes, err)
	}
}

func TestPolicyMergeGateCleansTaskWorktreeAfterMerge(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-9", RepoFullName: "gitmoot/gitmoot", GoalID: "goal-1", Title: "Task 9", State: string(TaskReadyToMerge), Branch: "task-9", WorktreePath: "/tmp/gitmoot/worktrees/gitmoot--gitmoot/task-9"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, State: "open", URL: "https://github.com/gitmoot/gitmoot/pull/9", HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	cleaner := &fakeWorktreeCleaner{}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, Worktrees: cleaner}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Reviewer: "audit"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || decision.Reason != "merged" {
		t.Fatalf("decision = %+v", decision)
	}
	if len(cleaner.removed) != 1 || cleaner.removed[0] != "/tmp/gitmoot/worktrees/gitmoot--gitmoot/task-9" {
		t.Fatalf("removed worktrees = %+v", cleaner.removed)
	}
	task, err := store.GetTask(ctx, "task-9")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.WorktreePath != "" {
		t.Fatalf("task worktree path = %q, want cleared", task.WorktreePath)
	}
}

func TestPolicyMergeGateReportsWorktreeCleanupWarning(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-9", RepoFullName: "gitmoot/gitmoot", GoalID: "goal-1", Title: "Task 9", State: string(TaskReadyToMerge), Branch: "task-9", WorktreePath: "/tmp/gitmoot/worktrees/gitmoot--gitmoot/task-9"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, State: "open", URL: "https://github.com/gitmoot/gitmoot/pull/9", HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	cleaner := &fakeWorktreeCleaner{err: errors.New("worktree has uncommitted files")}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, Worktrees: cleaner}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Reviewer: "audit"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || !strings.Contains(decision.Reason, "cleanup task worktree") {
		t.Fatalf("decision = %+v, want cleanup warning", decision)
	}
	task, err := store.GetTask(ctx, "task-9")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.WorktreePath == "" {
		t.Fatal("task worktree path was cleared despite cleanup failure")
	}
}

func TestPolicyMergeGateDoesNotCleanWorktreeForMismatchedTaskBranch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-8", RepoFullName: "gitmoot/gitmoot", GoalID: "goal-1", Title: "Task 8", State: string(TaskImplementing), Branch: "task-8", WorktreePath: "/tmp/gitmoot/worktrees/gitmoot--gitmoot/task-8"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-8",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, State: "open", URL: "https://github.com/gitmoot/gitmoot/pull/9", HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	cleaner := &fakeWorktreeCleaner{}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, Worktrees: cleaner}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 9, TaskID: "task-8", Reviewer: "audit"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || !strings.Contains(decision.Reason, "task task-8 branch is task-8") {
		t.Fatalf("decision = %+v, want branch mismatch cleanup warning", decision)
	}
	if len(cleaner.removed) != 0 {
		t.Fatalf("removed worktrees = %+v, want none", cleaner.removed)
	}
	task, err := store.GetTask(ctx, "task-8")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.WorktreePath == "" {
		t.Fatal("mismatched task worktree path was cleared")
	}
}

func TestPolicyMergeGateLocksCheckoutDuringLocalBaseUpdate(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	checkout := t.TempDir()
	key, err := checkoutMutationLockKey(checkout)
	if err != nil {
		t.Fatalf("checkoutMutationLockKey returned error: %v", err)
	}
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, State: "open", URL: "https://github.com/gitmoot/gitmoot/pull/9", HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	git := &fakeMergeGateGit{clean: true, onUpdate: func() {
		lock, err := store.GetResourceLock(ctx, key)
		if err != nil {
			t.Fatalf("GetResourceLock during UpdateBase returned error: %v", err)
		}
		if lock.OwnerJobID != "merge:gitmoot/gitmoot#9" {
			t.Fatalf("checkout lock owner = %q, want merge:gitmoot/gitmoot#9", lock.OwnerJobID)
		}
	}}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: git, CheckoutPath: checkout}

	if _, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Reviewer: "audit"}); err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if _, err := store.GetResourceLock(ctx, key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("checkout lock after UpdateBase error = %v, want sql.ErrNoRows", err)
	}
}

func TestPolicyMergeGateReturnsRetryableErrorForBusyCheckout(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	checkout := t.TempDir()
	key, err := checkoutMutationLockKey(checkout)
	if err != nil {
		t.Fatalf("checkoutMutationLockKey returned error: %v", err)
	}
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  "task:other",
		OwnerToken:  "other-token",
		ExpiresAt:   "2099-01-01T00:00:00Z",
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, State: "open", URL: "https://github.com/gitmoot/gitmoot/pull/9", HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	git := &fakeMergeGateGit{clean: true}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: git, CheckoutPath: checkout}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Reviewer: "audit"})

	if err == nil {
		t.Fatal("Evaluate returned nil error, want retryable checkout-busy error")
	}
	var blocked BlockedError
	if errors.As(err, &blocked) {
		t.Fatalf("Evaluate error = %v, should not expose checkout contention as policy BlockedError", err)
	}
	if !strings.Contains(err.Error(), checkoutMutationBusyMessage) {
		t.Fatalf("Evaluate error = %v, want checkout busy message", err)
	}
	if decision.Ready || decision.Merged {
		t.Fatalf("decision = %+v, want no merge decision on checkout contention", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge ran despite checkout lock: %+v", gh.merges)
	}
	if len(git.updated) != 0 {
		t.Fatalf("UpdateBase ran despite checkout lock: %+v", git.updated)
	}
	if _, err := store.GetMergeGate(ctx, "gitmoot/gitmoot", 9); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetMergeGate after checkout contention = %v, want sql.ErrNoRows", err)
	}
}

func TestPolicyMergeGateDoesNotRecordPreMergeSyntheticSHA(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:    9,
			State:     "open",
			URL:       "https://github.com/gitmoot/gitmoot/pull/9",
			HeadRef:   "task-9",
			BaseRef:   "main",
			HeadSHA:   "head123",
			MergeSHA:  "synthetic-premerge-sha",
			Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Reviewer: "audit"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || decision.MergeCommitSHA != "" {
		t.Fatalf("decision = %+v", decision)
	}
	pr, err := store.GetPullRequest(ctx, "gitmoot/gitmoot", 9)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.MergeCommitSHA != "" {
		t.Fatalf("stored pull request merge SHA = %q, want empty", pr.MergeCommitSHA)
	}
}

func TestPolicyMergeGateBlocksDirtyWorktree(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	gh := &fakeMergeGateGitHub{pr: github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123"}}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: false}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "worktree") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
}

func TestPolicyMergeGateBlocksFailedExternalCI(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{
			State: "success",
			Statuses: []github.CommitStatus{
				{Context: "gitmoot/review", State: "success"},
			},
		},
		checks: []github.PullRequestCheck{{Name: "ci", Bucket: "fail", State: "COMPLETED"}},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "external CI") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
}

func TestPolicyMergeGateTruncatesLongStatusDescriptions(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:     github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{State: "success"},
		checks: []github.PullRequestCheck{{
			Name:   "ci-" + strings.Repeat("very-long-check-name-", 12),
			Bucket: "fail",
			State:  "COMPLETED",
		}},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.statuses) != 1 {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
	if got := len([]rune(gh.statuses[0].Description)); got > 140 {
		t.Fatalf("status description length = %d, want <= 140: %q", got, gh.statuses[0].Description)
	}
}

func TestPolicyMergeGateAllowsSkippedExternalCI(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success"},
		checks:      []github.PullRequestCheck{{Name: "conditional", Bucket: "skipping", State: "SKIPPED"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestPolicyMergeGateUpdatesStaleBranchAndStaysPending(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:      github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:  github.CombinedStatus{State: "success"},
		compare: github.CompareResult{Status: "behind", BehindBy: 1},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason, "branch update") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if len(gh.updates) != 1 || gh.updates[0].ExpectedHeadSHA != "head123" {
		t.Fatalf("update inputs = %+v", gh.updates)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "pending") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestPolicyMergeGateBlocksStaleBranchUpdateConflict(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:        github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:    github.CombinedStatus{State: "success"},
		compare:   github.CompareResult{Status: "behind", BehindBy: 1},
		updateErr: github.UpdatePullRequestBranchError{Kind: github.UpdatePullRequestBranchErrorConflict, Detail: "conflict"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "conflicts with main") {
		t.Fatalf("decision = %+v", decision)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "failure") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
	if len(gh.comments) != 1 || !strings.Contains(gh.comments[0], "not retryable") ||
		!strings.Contains(gh.comments[0], "task: task-9") ||
		!strings.Contains(gh.comments[0], "Gitmoot applies file changes in the task worktree") ||
		!strings.Contains(gh.comments[0], "rerun review/merge") {
		t.Fatalf("comments = %+v", gh.comments)
	}
}

func TestPolicyMergeGateKeepsStaleHeadRacePending(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:        github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:    github.CombinedStatus{State: "success"},
		compare:   github.CompareResult{Status: "behind", BehindBy: 1},
		updateErr: github.UpdatePullRequestBranchError{Kind: github.UpdatePullRequestBranchErrorStaleHead, Detail: "stale head"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason, "head changed") {
		t.Fatalf("decision = %+v", decision)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "pending") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestPolicyMergeGateKeepsMergeQueueBusyPending(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: mergeQueueLockKey("gitmoot/gitmoot", "main"),
		OwnerJobID:  "merge-queue:gitmoot/gitmoot#8",
		OwnerToken:  "token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:     github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{State: "success"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason, "merge queue") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "pending") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestPolicyMergeGateKeepsPendingCIReadyToRetry(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:     github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{State: "success"},
		checks: []github.PullRequestCheck{{Name: "ci", Bucket: "pending", State: "IN_PROGRESS"}},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason, "pending") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "pending") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestPolicyMergeGateKeepsQueuedMergePending(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Message: "pull request merge is pending"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason, "pending") {
		t.Fatalf("decision = %+v", decision)
	}
	if _, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-9"); err != nil {
		t.Fatalf("branch lock after queued merge error = %v", err)
	}
	if _, err := store.GetPullRequest(ctx, "gitmoot/gitmoot", 9); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetPullRequest after queued merge error = %v, want sql.ErrNoRows", err)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "pending") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestPolicyMergeGateAllowsReviewOptionalPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", ReviewOptional: true})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 1 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
}

func TestPolicyMergeGateRecordsAlreadyMergedPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:   9,
			State:    "closed",
			Merged:   true,
			URL:      "https://github.com/gitmoot/gitmoot/pull/9",
			HeadRef:  "task-9",
			BaseRef:  "main",
			HeadSHA:  "head123",
			MergeSHA: "merge123",
		},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: false}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || decision.MergeCommitSHA != "merge123" {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if _, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-9"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("branch lock after merge error = %v, want sql.ErrNoRows", err)
	}
}

func TestPolicyMergeGateBlocksClosedUnmergedPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:  9,
			State:   "closed",
			Merged:  false,
			HeadRef: "task-9",
			BaseRef: "main",
			HeadSHA: "head123",
		},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "closed") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "failure") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
}

func TestPolicyMergeGateUsesLatestNumericReviewRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-nine", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "old123",
		TaskID:      "task-9",
		ReviewRound: "review-9",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "old change"},
	})
	insertCompletedJob(t, store, db.Job{ID: "review-ten", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-10",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestPolicyMergeGateBlocksReviewForStaleHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "old123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:     github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{State: "success"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "different head SHA") {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestPolicyMergeGateBlocksLegacyReviewWithoutHeadSHA(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "gitmoot/gitmoot",
		Number:       9,
		URL:          "https://github.com/gitmoot/gitmoot/pull/9",
		HeadBranch:   "task-9",
		BaseBranch:   "main",
		HeadSHA:      "head123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "does not record a head SHA") {
		t.Fatalf("decision = %+v", decision)
	}
}

// TestPolicyMergeGateAdvancesIntegrationWorktreeReviewWithoutHeadSHA is the #388
// regression: a gate-required review that ran on a #332 integration worktree has
// its inherited HeadSHA cleared by design (the worktree carries no branch and is
// validated against its own fresh HEAD). The gate must not treat that empty SHA
// as a stale/unverifiable review — otherwise the merge deadlocks because the
// required review can never be satisfied. With the fix the PR advances and merges.
func TestPolicyMergeGateAdvancesIntegrationWorktreeReviewWithoutHeadSHA(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "gitmoot/gitmoot",
		Number:       9,
		URL:          "https://github.com/gitmoot/gitmoot/pull/9",
		HeadBranch:   "task-9",
		BaseBranch:   "main",
		HeadSHA:      "head123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	// An integration-worktree review: a delegation child (DelegationID +
	// WorktreePath set) whose HeadSHA the engine intentionally cleared.
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review", DelegationID: "verify-gate"}, JobPayload{
		Repo:         "gitmoot/gitmoot",
		PullRequest:  9,
		TaskID:       "task-9",
		ReviewRound:  "review-1",
		DelegationID: "verify-gate",
		WorktreePath: "/tmp/gitmoot/integration-verify-gate",
		Result:       &AgentResult{Decision: "approved", Summary: "integration verified"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("integration-worktree review did not advance to merge: decision = %+v", decision)
	}
}

// TestPolicyMergeGateBlocksDelegationReviewForMismatchedHead is the safety guard:
// the #388 exception applies only to an empty HeadSHA. A delegation review that
// DID record a head SHA which does not match the PR head is still a real mismatch
// and must STILL be rejected — the integration-worktree carve-out must not weaken
// the head-match check for any review that carries a concrete (wrong) SHA.
func TestPolicyMergeGateBlocksDelegationReviewForMismatchedHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review", DelegationID: "verify-gate"}, JobPayload{
		Repo:         "gitmoot/gitmoot",
		PullRequest:  9,
		HeadSHA:      "stale999",
		TaskID:       "task-9",
		ReviewRound:  "review-1",
		DelegationID: "verify-gate",
		WorktreePath: "/tmp/gitmoot/integration-verify-gate",
		Result:       &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:     github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{State: "success"},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "different head SHA") {
		t.Fatalf("delegation review with mismatched head was not rejected: decision = %+v", decision)
	}
}

func TestPolicyMergeGateBlocksMissingFinalReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:     github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status: github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "gitmoot/review", State: "success"}}},
	}
	gate := PolicyMergeGate{Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "review") {
		t.Fatalf("decision = %+v", decision)
	}
}

type fakeMergeGateGitHub struct {
	pr          github.PullRequest
	status      github.CombinedStatus
	compare     github.CompareResult
	checks      []github.PullRequestCheck
	mergeResult github.MergeResult
	updateErr   error
	statuses    []github.CommitStatusInput
	merges      []github.MergePullRequestInput
	updates     []github.UpdatePullRequestBranchInput
	comments    []string
}

func (f *fakeMergeGateGitHub) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	return f.pr, nil
}

func (f *fakeMergeGateGitHub) GetCombinedStatus(context.Context, github.Repository, string) (github.CombinedStatus, error) {
	return f.status, nil
}

func (f *fakeMergeGateGitHub) CompareCommits(context.Context, github.Repository, string, string) (github.CompareResult, error) {
	if f.compare.Status == "" && f.compare.AheadBy == 0 && f.compare.BehindBy == 0 {
		return github.CompareResult{Status: "ahead", AheadBy: 1}, nil
	}
	return f.compare, nil
}

func (f *fakeMergeGateGitHub) ListPullRequestChecks(context.Context, github.Repository, int64) ([]github.PullRequestCheck, error) {
	return f.checks, nil
}

func (f *fakeMergeGateGitHub) CreateCommitStatus(_ context.Context, input github.CommitStatusInput) (github.CommitStatus, error) {
	f.statuses = append(f.statuses, input)
	return github.CommitStatus{State: input.State, Context: input.Context}, nil
}

func (f *fakeMergeGateGitHub) PostIssueComment(_ context.Context, _ github.Repository, _ int64, body string) (github.IssueComment, error) {
	f.comments = append(f.comments, body)
	return github.IssueComment{Body: body}, nil
}

func (f *fakeMergeGateGitHub) UpdatePullRequestBranch(_ context.Context, input github.UpdatePullRequestBranchInput) (github.UpdatePullRequestBranchResult, error) {
	f.updates = append(f.updates, input)
	return github.UpdatePullRequestBranchResult{Message: "Updating pull request branch."}, f.updateErr
}

func (f *fakeMergeGateGitHub) MergePullRequest(_ context.Context, input github.MergePullRequestInput) (github.MergeResult, error) {
	f.merges = append(f.merges, input)
	return f.mergeResult, nil
}

type fakeMergeGateGit struct {
	clean    bool
	onUpdate func()
	updated  []string
}

func (f *fakeMergeGateGit) WorktreeClean(context.Context) (bool, error) {
	return f.clean, nil
}

func (f *fakeMergeGateGit) UpdateBase(_ context.Context, remote string, branch string) error {
	if f.onUpdate != nil {
		f.onUpdate()
	}
	f.updated = append(f.updated, remote+"/"+branch)
	return nil
}

type fakeWorktreeCleaner struct {
	removed []string
	err     error
}

func (f *fakeWorktreeCleaner) RemoveWorktree(_ context.Context, path string) error {
	f.removed = append(f.removed, path)
	return f.err
}

func hasStatus(statuses []github.CommitStatusInput, context string, state string) bool {
	for _, status := range statuses {
		if status.Context == context && status.State == state {
			return true
		}
	}
	return false
}
